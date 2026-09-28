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
import guard, { decide, denied, normalize } from "./guard.ts";

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
const scope = { allow: ["allowed.go"], deny: shipped, bashDeny: ["\\bcurl\\b"] };
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
assert.equal(decide("read", { path: "/etc/passwd" }, repo, scope), undefined, "reading is not this boundary's business");

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

console.log("guard.ts: ok");
