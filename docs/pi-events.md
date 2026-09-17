# Pi JSONL event stream

What `pi -p --mode json` actually emits, mapped from a real run rather than from
the bundled `docs/json.md`. Every claim below has a line of captured JSON under
it as evidence.

Captured with Pi 0.85.1:

```bash
pi -p --mode json --provider moonshotai --model kimi-k2.7-code \
   --tools read,grep,find,ls \
   "List the Go files here and summarise what each does" > sample.jsonl
```

356 lines, 3 turns, 4 tool calls, one deliberate error run alongside it.

## Answers

| Question | Answer |
|---|---|
| Final assistant text lives at | `message_end` where `.message.role == "assistant"` and `.message.stopReason == "stop"`, in the content block with `.type == "text"` → `.text`. Also available streaming as the `text_end` delta's `.content`. |
| Token counts live at | `.message.usage` on `message_end`, and `.usage` (top level) on `message_update`. Keys: `input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`. **Per message, not per run** — see below. |
| Cost lives at | `.usage.cost` — an object, not a scalar: `{input, output, cacheRead, cacheWrite, total}`, in dollars. Pi computes it from its built-in model catalog, so no per-model bookkeeping is needed downstream. |
| A tool call starts / ends with | `tool_execution_start` (carries `toolCallId`, `toolName`, `args`) and `tool_execution_end` (carries `toolCallId`, `toolName`, `result`, `isError`). Only the end carries the result. |
| Fields absent on some events | `usage.reasoning` (missing on the zero-usage first update), `message.stopReason` (`null` on user and toolResult messages), `result.details` (only on error results), and every `assistantMessageEvent` payload key, which varies by delta type. |

## Event inventory

One run, counted:

```
 322 message_update
   8 message_start
   8 message_end
   4 tool_execution_start
   4 tool_execution_end
   3 turn_start
   3 turn_end
   1 session
   1 agent_start
   1 agent_settled
   1 agent_end
```

Top-level keys per type:

```
agent_end            type,messages,willRetry
agent_settled        type
agent_start          type
message_end          type,message
message_start        type,message
message_update       type,usage,assistantMessageEvent
session              type,version,id,timestamp,cwd
tool_execution_end   type,toolCallId,toolName,result,isError
tool_execution_start type,toolCallId,toolName,args
turn_end             type,message,toolResults
turn_start           type
```

Note `agent_settled` and `turn_start` carry nothing but `type`, and `agent_start`
likewise — they are pure markers.

## Session header

First line, as the docs claim:

```json
{"type":"session","version":3,"id":"01a0acb9-720b-71fa-9525-a4c05f4268ab","timestamp":"2026-09-17T00:17:09.644Z","cwd":"/Users/tyrel/Projects/lathe"}
```

`timestamp` here is an ISO-8601 string. **Every other timestamp in the stream is
a Unix milliseconds integer** (`"timestamp":1789604229690` on messages). Two
different time encodings in one stream; the parser has to know which is which.

## Messages

`message_start` carries the whole message, already complete for user messages:

```json
{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"List the Go files here and summarise what each does"}],"timestamp":1789604229690}}
```

Roles seen: `user`, `assistant`, `toolResult`. Each gets its own
`message_start`/`message_end` pair, so 8 `message_end` events for a 3-turn run.

`message_end` for the final assistant message, content stripped:

```json
{"role":"assistant","api":"openai-completions","provider":"moonshotai","model":"kimi-k2.7-code","usage":{"input":1676,"output":225,"cacheRead":1024,"cacheWrite":0,"reasoning":19,"totalTokens":2925,"cost":{"input":0.00159,"output":0.0009,"cacheRead":0.000195,"cacheWrite":0,"total":0.00268676}},"stopReason":"stop","timestamp":1789604237147,"responseId":"chatcmpl-6aab318d5b056459530683d3","rawStopReason":"stop"}
```

Content blocks and their keys:

| Block type | Keys |
|---|---|
| `text` | `type`, `text` |
| `thinking` | `type`, `thinking`, `thinkingSignature` |
| `toolCall` | `type`, `id`, `name`, `arguments` |

Stop reasons observed: `toolUse` while the agent is working, `stop` on the final
message, `null` on user and toolResult messages.

## Usage is per message, not cumulative

This contradicts the hypothesis in the plan. `totalTokens` across the stream's
`message_update` events, consecutive duplicates collapsed:

```
0 1094 0 1188 0 2925
```

It resets to zero at each assistant message and climbs within that message. Per
`message_end`:

```
assistant  1094  $0.00080573
assistant  1188  $0.00054861
assistant  2925  $0.00268676
```

**Consequence for `internal/trace`:** a run total is the *sum* over assistant
`message_end` events, not the last value seen. Summing `cost.total` is correct
(each is a real billed call). Summing `input` double-counts resent context —
that is what was actually billed, so it is right for cost but must not be read
as "distinct tokens of context".

Usage also becomes non-zero *before* the message ends — it first appears on a
`toolcall_delta` — so a mid-message read is a partial, and only the value on
`message_end` is authoritative.

## Deltas (`message_update`)

`assistantMessageEvent` is the delta payload. Types seen, with counts:

```
 204 text_delta        1 text_start        1 text_end
  68 thinking_delta    3 thinking_start    3 thinking_end
  34 toolcall_delta    4 toolcall_start    4 toolcall_end
```

Every delta carries `contentIndex`. The shapes differ per type:

```json
{"type":"thinking_delta","contentIndex":0,"delta":"The"}
{"type":"toolcall_start","contentIndex":1,"id":"find_0_567d3680","toolName":"find"}
{"type":"toolcall_end","contentIndex":1,"toolCall":{"type":"toolCall","id":"find_0_567d3680","name":"find","arguments":{"pattern":"**/*.go"}}}
```

`text_end` carries the whole accumulated `content`, so streaming text does not
have to be reassembled from deltas by hand.

**For Go:** decode `assistantMessageEvent` as `{Type string; ContentIndex int}`
plus a `json.RawMessage` for the rest, and only decode further for the delta
types being acted on. There is no single struct that fits all nine.

## Tool calls

```json
{"type":"tool_execution_start","toolCallId":"find_0_567d3680","toolName":"find","args":{"pattern":"**/*.go"}}
{"type":"tool_execution_end","toolCallId":"find_0_567d3680","toolName":"find","result":{"content":[{"type":"text","text":"internal/install/install.go\ninternal/install/install_test.go\nmain.go"}]},"isError":false}
```

`toolCallId` is `<tool>_<index>_<hash>` and brackets the pair. `args` is
tool-specific and arbitrary — keep it `json.RawMessage` and store it as text.

**Tool calls run in parallel.** The captured order was three `read` starts
before any of their ends:

```
start read_1_7c3584a8
start read_2_ab5ecbbd
start read_3_90e6668b
end   read_1_7c3584a8
end   read_2_ab5ecbbd
end   read_3_90e6668b
```

Nothing may assume start/end nest or alternate. Match on `toolCallId`; a map of
in-flight calls, not a stack.

`isError` is always present as a real boolean (not omitted when false), so a
plain `bool` is safe here. On an error the result grows an extra `details` key:

```json
{"toolName":"read","isError":true,"result":{"content":[{"type":"text","text":"ENOENT: no such file or directory, access '/Users/tyrel/Projects/lathe/definitely-missing-file.txt'"}],"details":{}}}
```

Note the failure is reported *in-band* — the tool result carries the error text
back to the model, Pi exits 0, and the agent recovers on its own. An exit code
of 0 does not mean nothing went wrong.

## Turns

`turn_end` carries the assistant message that closed the turn plus the results
that came back:

```json
{"type":"turn_end","toolResults":[{"role":"toolResult","toolCallId":"...","toolName":"find","content":[...],"isError":false,"timestamp":...}]}
```

`toolResults` is `[]` on the final turn. The sequence is
`agent_start` → (`turn_start` … `turn_end`) × N → `agent_settled` → `agent_end`,
and `agent_end` carries `willRetry` (`false` here) plus the full message list.

## Pointers vs values, for Phase 3

| Field | Go type |
|---|---|
| `message.stopReason` | `*string` — `null` on non-assistant messages |
| `usage.reasoning` | `*int` — absent on the zero-usage first update |
| `usage.cost` | struct, never a scalar |
| `assistantMessageEvent` | `json.RawMessage` past `type`/`contentIndex` |
| `tool_execution_start.args` | `json.RawMessage` |
| `tool_execution_end.result` | `json.RawMessage`; `details` appears only on errors |
| `isError` | plain `bool`, always present |
| `session.timestamp` | ISO-8601 string; every other `timestamp` is Unix ms `int64` |
