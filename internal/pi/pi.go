// Package pi spawns the Pi coding agent and turns its JSONL stdout into
// events while the agent is still working. Nothing here knows about SQLite:
// the caller decides what a given event is worth recording.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// maxLine caps a single JSONL record. Scanner's 64KB default truncates a run
// the moment a tool result carries a whole file, and the failure looks like
// the agent stopping early rather than like an error.
const maxLine = 8 * 1024 * 1024

// Event is the shallow view of a Pi event: the fields lathe reads, and raw
// JSON for everything a branch has not yet needed. Per docs/pi-events.md the
// stream is a union of eleven shapes and no single struct fits them all.
type Event struct {
	Type       string          `json:"type"`
	Message    json.RawMessage `json:"message"`
	Usage      *Usage          `json:"usage"` // pointer: absent must be distinguishable from zero
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"` // always present in the stream, so a plain bool is safe
}

// Usage is per message, never per run. Summing over assistant message_end
// events is the only correct way to total a run — see docs/pi-events.md.
type Usage struct {
	Input       int  `json:"input"`
	Output      int  `json:"output"`
	CacheRead   int  `json:"cacheRead"`
	CacheWrite  int  `json:"cacheWrite"`
	Reasoning   *int `json:"reasoning"` // absent on the zero-usage first update
	TotalTokens int  `json:"totalTokens"`
	Cost        Cost `json:"cost"`
}

// Cost is an object in dollars, never a scalar; Pi computes it from its
// built-in model catalog.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Message is the payload of message_start / message_end.
type Message struct {
	Role       string  `json:"role"` // user | assistant | toolResult
	StopReason *string `json:"stopReason"`
	Usage      *Usage  `json:"usage"`
	Content    []Block `json:"content"`
}

// Block is one content block. thinking and toolCall blocks carry other keys;
// nothing here reads them yet.
type Block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text joins every text block, which is where the assistant's reply lives.
// Both captured runs carry one block per message, but returning only the first
// would drop an envelope silently if that ever stopped being true.
func (m Message) Text() string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// DecodeMessage parses the deferred message payload.
func (e Event) DecodeMessage() (Message, error) {
	var m Message
	err := json.Unmarshal(e.Message, &m)
	return m, err
}

// Handler sees every parsed event as it arrives. It must not block for long:
// the agent's stdout is not being read while it runs.
type Handler func(Event)

// Result is what a whole stream added up to.
type Result struct {
	Tokens    int     // sum over assistant message_end
	Cost      float64 // dollars, same sum
	Text      string  // final assistant reply
	Events    int     // events parsed
	Malformed int     // lines that were not JSON, which would mean a Pi change
}

// Options configures one Pi invocation. Empty fields are left off the command
// line so Pi's own defaults apply.
type Options struct {
	Bin          string // resolved on PATH when empty; never hardcode a path
	Dir          string // target root: how a global tool acts locally
	Provider     string
	Model        string
	Thinking     string
	SessionID    string // same id continues the same context window
	SessionDir   string
	SystemPrompt string
	Tools        []string
	Raw          io.Writer     // verbatim JSONL, written before parsing
	OnStart      func(pid int) // child PID, so a hung agent is killable by run id
}

// Run spawns Pi, streams its events to onEvent, and returns the run's totals.
// The context kills the child.
func Run(ctx context.Context, o Options, prompt string, onEvent Handler) (Result, error) {
	bin := o.Bin
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("pi"); err != nil {
			return Result{}, fmt.Errorf("pi not found on PATH: %w", err)
		}
	}

	args := []string{"-p", "--mode", "json"}
	for _, kv := range [][2]string{
		{"--provider", o.Provider},
		{"--model", o.Model},
		{"--thinking", o.Thinking},
		{"--session-id", o.SessionID},
		{"--session-dir", o.SessionDir},
		{"--system-prompt", o.SystemPrompt},
		{"--tools", strings.Join(o.Tools, ",")},
	} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	args = append(args, "--", prompt) // -- so a prompt starting with - stays a prompt

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = o.Dir
	// Explicit: an inherited stdin makes Pi wait forever for input that never
	// comes — no request, no output, 0% CPU. nil gets /dev/null.
	cmd.Stdin = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	if o.OnStart != nil {
		o.OnStart(cmd.Process.Pid)
	}

	res, scanErr := Scan(stdout, o.Raw, onEvent)

	if err := cmd.Wait(); err != nil {
		return res, fmt.Errorf("pi: %w: %s", err, tail(strings.TrimSpace(stderr.String()), 500))
	}
	return res, scanErr
}

// Scan reads a Pi JSONL stream. Every line is written to raw verbatim before
// it is parsed: the JSONL file is the authoritative record and SQLite is a
// derived mirror, so a parser bug must not cost the evidence.
func Scan(r io.Reader, raw io.Writer, onEvent Handler) (Result, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	var res Result
	var rawErr error
	// Tool calls run in parallel and do not nest, so in-flight calls are a map
	// keyed by toolCallId, never a stack. Only the start carries args.
	inflight := map[string]json.RawMessage{}

	for sc.Scan() {
		line := sc.Bytes()
		if raw != nil {
			if _, err := raw.Write(append(line, '\n')); err != nil && rawErr == nil {
				rawErr = err
			}
		}

		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			res.Malformed++
			continue
		}
		res.Events++

		switch ev.Type {
		case "tool_execution_start":
			inflight[ev.ToolCallID] = ev.Args
		case "tool_execution_end":
			if args, ok := inflight[ev.ToolCallID]; ok {
				ev.Args = args // the end event carries the result but not the args
				delete(inflight, ev.ToolCallID)
			}
		case "message_end":
			if m, err := ev.DecodeMessage(); err == nil && m.Role == "assistant" {
				if m.Usage != nil {
					res.Tokens += m.Usage.TotalTokens
					res.Cost += m.Usage.Cost.Total
				}
				if m.StopReason != nil && *m.StopReason == "stop" {
					res.Text = m.Text()
				}
			}
		}

		if onEvent != nil {
			onEvent(ev)
		}
	}
	return res, errors.Join(sc.Err(), rawErr)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
