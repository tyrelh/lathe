// guard_test.ts — the other half of the boundary, held to the same table as
// permit_test.go. Run by TestGuardMatchesGo, which is what makes it part of
// `go test ./...` rather than a file someone has to remember.
//
// Node runs the TypeScript directly, the way jiti does inside Pi, so there is
// no build step and nothing that can be stale against the file Pi loads.

import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import guard, { decide, denied, normalize, unreadable, withhold } from "./guard.ts";

const shipped = [".git/", ".env*", "*.pem", "*.key"];

// The table permit_test.go's TestDenied runs. A divergence between the two
// halves is a hole neither half's own tests can see, so this is the same list
// of cases, not a similar one.
for (
	const [path, want] of [
		[".git/config", true],
		["vendor/x/.git/HEAD", true],
		[".env", true],
		[".env.production", true],
		["config/.env.local", true],
		["certs/server.pem", true],
		["deploy/id.key", true],
		["internal/run/run.go", false],
		["gitignore.md", false],
		["env.go", false],
		["keys/README.md", false],
	] as [string, boolean][]
) {
	assert.equal(denied(path, shipped) !== undefined, want, `denied(${path})`);
}
assert.equal(denied(".env", shipped), ".env*", "the matching rule is what the agent is told");

// Pi strips a leading @ and folds Unicode spaces before it writes, so a check
// against the untransformed string checks a path nothing will touch.
assert.equal(normalize("@.env"), ".env");
assert.equal(normalize("a b.go"), "a b.go");
assert.equal(normalize("plain.go"), "plain.go");

const repo = mkdtempSync(join(tmpdir(), "lathe-guard-"));
const scope = { allow: ["allowed.go"], deny: shipped, bashDeny: ["\\bcurl\\b"], readRoots: [] as string[] };
const block = (path: string) => decide("write", { path }, repo, scope);

writeFileSync(join(repo, "allowed.go"), "package main\n");
assert.equal(block("allowed.go"), undefined, "an in-scope path is writable");

// "@allowed.go" is not in the allow list, but Pi writes "allowed.go", which is.
// The interesting direction is the other one: a plan entry Pi would rewrite
// into a protected path must not clear the boundary on its spelling.
assert.match(block("@.env")!.reason, /protected/, "@.env is .env by the time Pi writes it");
assert.equal(block("@allowed.go"), undefined, "and @allowed.go is allowed.go");

// An allowed name is still an allowed name when the file under it is a symlink
// pointing somewhere git can never revert.
mkdirSync(join(repo, "outside"));
writeFileSync(join(repo, "outside", "secret.pem"), "key\n");
symlinkSync(join(repo, "outside", "secret.pem"), join(repo, "sneaky.go"));
scope.allow.push("sneaky.go");
assert.match(block("sneaky.go")!.reason, /protected/, "a symlink to key material is that key material");

symlinkSync("/etc/hosts", join(repo, "escape.go"));
scope.allow.push("escape.go");
assert.match(block("escape.go")!.reason, /outside the repository/, "a symlink out of the repo leaves the repo");

// permit.Write cleans the allow list, so the guard never has to: this proves
// the spelling it receives is the spelling it compares.
assert.match(block("missing.go")!.reason, /not in the plan/);
assert.match(block("/etc/passwd")!.reason, /outside the repository/);
assert.match(decide("bash", { command: "curl https://x" }, repo, scope)!.reason, /deny rule/);
assert.equal(decide("bash", { command: "go test ./..." }, repo, scope), undefined);
assert.equal(decide("read", { path: "/etc/passwd" }, repo, scope), undefined, "with no read roots, reads are unchecked");

// Read roots, for agents with web_read: the repo and the Go module cache. A path is judged where it resolves, and protected names are
// unreadable even inside a root.
const sandbox = mkdtempSync(join(tmpdir(), "lathe-roots-"));
const [inRepo, gomod, outside] = ["repo", "gomod", "outside"].map((d) => join(sandbox, d));
for (const d of [inRepo, gomod, outside, join(sandbox, "other-project")]) mkdirSync(d);
writeFileSync(join(outside, "id_ed25519"), "key\n");
symlinkSync(outside, join(inRepo, "link-to-ssh"));
symlinkSync(join(outside, "id_ed25519"), join(inRepo, "notes\u2019txt"));
const roots = [inRepo, gomod];
for (
	const [tool, path, want] of [
		["read", "internal/run/run.go", "allowed"],
		["read", join(gomod, "github.com/x/y/a.go"), "allowed"],
		["read", join(outside, "id_ed25519"), "outside"],
		["read", "../other-project/config.yaml", "outside"],
		["read", ".env", "protected"],
		["read", "config/.env.local", "protected"],
		["read", ".git/config", "protected"],
		["read", "link-to-ssh/id_ed25519", "outside"],
		["read", "notes'txt", "outside"], // Pi falls back to the curly-quote spelling, a symlink out
		["read", "plain'name", "allowed"], // no variant exists, so the path as given is judged
		["read", "~/.aws/credentials", "outside"],
		["ls", "/", "outside"],
		["find", undefined, "allowed"],
		["grep", "internal", "allowed"],
		["grep", "link-to-ssh", "outside"],
	] as [string, string | undefined, string][]
) {
	const got = decide(tool, { path }, inRepo, { ...scope, readRoots: roots });
	const kind = got === undefined ? "allowed" : /protected/.test(got.reason) ? "protected" : /outside the folders/.test(got.reason) ? "outside" : got.reason;
	assert.equal(kind, want, `${tool}(${path})`);
}
assert.match(unreadable(join(outside, "id_ed25519"), inRepo, roots, shipped)!, /outside the folders this agent may read: .*repo/, "a refusal names the roots");
assert.equal(decide("write", { path: "allowed.go" }, repo, { ...scope, readRoots: roots }), undefined, "read roots leave writes to the allow list");

// grep searches hidden files and takes the model's own glob, so a protected
// file inside a root reaches its output; the guard withholds those lines.
const grepped = [
	"main.go:3: token := os.Getenv(\"TOKEN\")",
	".git/config:7: url = https://x-access-token:secret@github.com/o/r",
	"deploy/id.key:1: -----BEGIN PRIVATE KEY-----",
	".env-1- SECRET=1",
	"a:b.go:2: colons in a path",
	"",
	"[100 matches limit reached. Use limit=200 for more, or refine pattern]",
].join("\n");
assert.equal(withhold(grepped, shipped), [
	"main.go:3: token := os.Getenv(\"TOKEN\")",
	"a:b.go:2: colons in a path",
	"",
	"[100 matches limit reached. Use limit=200 for more, or refine pattern]",
	"[3 lines from protected paths withheld]",
].join("\n"));
assert.equal(withhold("main.go:1: ok", shipped), "main.go:1: ok", "nothing withheld, nothing said");

// The extension writes a snapshot at agent settlement, not for each tool or
// model response. A post-compaction unknown stays null, including zero being
// distinct from unknown.
const snapshot = join(repo, "context.json");
process.env.LATHE_CONTEXT_SNAPSHOT = snapshot;
const hooks = new Map<string, Function>();
guard({ on: (name: string, fn: Function) => hooks.set(name, fn) });
assert(hooks.has("agent_end"));
const settled = hooks.get("agent_end")!;
settled({}, { getContextUsage: () => ({ tokens: 1200, contextWindow: 8000, percent: 15 }) });
assert.deepEqual(JSON.parse(readFileSync(snapshot, "utf8")), { tokens: 1200, contextWindow: 8000, percent: 15 });
settled({}, { getContextUsage: () => ({ tokens: null, contextWindow: 8000, percent: null }) });
assert.deepEqual(JSON.parse(readFileSync(snapshot, "utf8")), { tokens: null, contextWindow: 8000, percent: null });
settled({}, { getContextUsage: () => ({ tokens: 0, contextWindow: 8000, percent: 0 }) });
assert.deepEqual(JSON.parse(readFileSync(snapshot, "utf8")), { tokens: 0, contextWindow: 8000, percent: 0 });
settled({}, { getContextUsage: () => undefined });
assert.deepEqual(JSON.parse(readFileSync(snapshot, "utf8")), { tokens: 0, contextWindow: 8000, percent: 0 },
	"unavailable usage does not fabricate a new estimate");
process.env.LATHE_CONTEXT_SNAPSHOT = join(repo, "missing", "context.json");
const unwritable = new Map<string, Function>();
guard({ on: (name: string, fn: Function) => unwritable.set(name, fn) });
unwritable.get("agent_end")!({}, { getContextUsage: () => ({ tokens: 1, contextWindow: 8000, percent: 0 }) });
delete process.env.LATHE_CONTEXT_SNAPSHOT;

// The result hook filters only grep, and only for an agent with read roots.
const permitPath = join(repo, "permit.json");
process.env.LATHE_PERMIT = permitPath;
const resultHooks = new Map<string, Function>();
guard({ on: (name: string, fn: Function) => resultHooks.set(name, fn) });
const onResult = resultHooks.get("tool_result")!;
const leak = { toolName: "grep", isError: false, content: [{ type: "text", text: ".env:1: SECRET=1" }] };
writeFileSync(permitPath, JSON.stringify({ ...scope, readRoots: [] }));
assert.equal(onResult(leak), undefined, "no read roots, no filtering");
writeFileSync(permitPath, JSON.stringify({ ...scope, readRoots: [repo] }));
assert.match(onResult(leak).content[0].text, /^\[1 lines from protected paths withheld\]$/);
assert.equal(onResult({ ...leak, toolName: "find" }), undefined, "find lists names, not contents");
const truncated = { ...leak, details: { truncation: { truncated: true, content: ".env:1: SECRET=1\nmain.go:1: ok" } } };
assert.equal(onResult(truncated).details.truncation.content, "main.go:1: ok\n[1 lines from protected paths withheld]",
	"a truncated search's details are filtered too");
assert.equal(onResult(leak).details, undefined, "a search with no truncation leaves details alone");
delete process.env.LATHE_PERMIT;

console.log("guard.ts: ok");
