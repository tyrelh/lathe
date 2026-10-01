// guard.ts — the write boundary, and for web_read agents the read boundary,
// in front of the tool rather than behind it.
//
// Pi's tool_call hook fires before a tool runs and can refuse it, which is the
// only place a rule about writing can be true rather than merely checked
// afterwards. Its tool_result hook is where a grep's output is filtered. lathe
// writes this file into the run directory and loads it with
// `--no-extensions -e`, so the target repo's own .pi/extensions never load.
//
// LATHE_PERMIT names a JSON file: { allow, deny, bashDeny, readRoots }. allow
// is exact repo-relative paths — the plan's file list, already cleaned by
// permit.Write so both halves of the boundary compare the same spelling. deny
// and bashDeny are patterns. readRoots, when not empty, are the only folders
// read, grep, find and ls may touch, and deny applies to reads inside them. The
// four are rewritten before every spawn, because each agent's scope differs;
// the file is read per call so a rewrite takes effect without a reload.
//
// LATHE_CONTEXT_SNAPSHOT, when set, names a file this extension overwrites at
// agent_end with Pi's context estimate: { tokens, contextWindow, percent }.
// run.go reads it after the process exits. It is telemetry riding along with
// the boundary, not part of it; a failed write is dropped.
//
// denied() is a port of internal/permit.Denied and the two must not drift: the
// Go tests and this file's tests each pass on their own while a divergence
// between them leaks. Change one, change both. guard_test.ts is what holds the
// two to the same table.

import { existsSync, readFileSync, realpathSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export interface Scope {
	allow: string[];
	deny: string[];
	bashDeny: string[];
	readRoots: string[];
}

export interface Block {
	block: true;
	reason: string;
}

// globMatch is Go's filepath.Match over a basename, which never contains a
// separator. A malformed pattern matches nothing, the same as Go returning an
// error that permit.Denied discards.
function globMatch(pattern: string, name: string): boolean {
	let re = "";
	for (let i = 0; i < pattern.length; i++) {
		const c = pattern[i];
		if (c === "*") re += "[^/]*";
		else if (c === "?") re += "[^/]";
		else if (c === "\\") re += escapeLiteral(pattern[++i] ?? "\\");
		else if (c === "[") {
			const end = pattern.indexOf("]", i + 1);
			if (end < 0) return false;
			const body = pattern.slice(i + 1, end);
			re += `[${body.startsWith("^") || body.startsWith("!") ? `^${body.slice(1)}` : body}]`;
			i = end;
		} else re += escapeLiteral(c);
	}
	return new RegExp(`^${re}$`).test(name);
}

function escapeLiteral(c: string): string {
	return c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
}

// denied reports the first protected pattern path matches. An entry ending in
// "/" is a directory name compared against every segment of the path — segment
// and not prefix, which is what covers vendor/x/.git/config and an absolute
// /Users/you/.ssh/id_rsa with one list. Anything else is a glob on the basename.
export function denied(path: string, deny: string[]): string | undefined {
	const segments = path.split(/[\\/]+/);
	for (const pattern of deny) {
		if (pattern.endsWith("/")) {
			if (segments.includes(pattern.slice(0, -1))) return pattern;
			continue;
		}
		if (globMatch(pattern, segments[segments.length - 1])) return pattern;
	}
	return undefined;
}

const UNICODE_SPACES = /[  -   　]/g;

// normalize is Pi's own normalizePath, with the options its write and edit
// tools pass (normalizeUnicodeSpaces and stripAtPrefix, expandTilde by
// default). Every transform Pi makes has to happen here too, because Pi writes
// the path it derives and not the one the model typed: a check against the
// untyped-through string is a check on a path nothing will touch. "@.env" is
// the sharp case — it clears the Go plan gate and this allow list spelled
// "@.env", and Pi then writes ".env".
export function normalize(input: string): string {
	let p = input.replace(UNICODE_SPACES, " ");
	if (p.startsWith("@")) p = p.slice(1);
	const home = homedir();
	if (p === "~") return home;
	if (p.startsWith("~/")) return join(home, p.slice(2));
	if (/^file:\/\//.test(p)) return fileURLToPath(p);
	return p;
}

// resolveReal is the path the write actually lands on. The whole path is
// realpath'd first, because the last component can itself be a symlink out of
// the repo: an allowed "config.ts" is still an allowed name when it resolves to
// /etc/passwd, and git can never revert that. Only a path that does not exist
// yet — the ordinary case for a new file — falls back to the nearest existing
// ancestor, which settles a symlinked directory and not every case.
export function resolveReal(input: string, cwd: string): string {
	const norm = normalize(input);
	const p = isAbsolute(norm) ? resolve(norm) : resolve(cwd, norm);
	try {
		return realpathSync(p);
	} catch {
		// Not there yet, so there is nothing to follow; settle the ancestors.
	}
	for (let dir = p; ; ) {
		const parent = resolve(dir, "..");
		if (parent === dir) return p; // reached the root with nothing existing
		try {
			return join(realpathSync(parent), relative(parent, p));
		} catch {
			dir = parent;
		}
	}
}

// decide is the whole rule, separated from reading the scope file so a test can
// hold it to the same table internal/permit's Go tests use.
export function decide(toolName: string, input: any, cwd: string, scope: Scope): Block | undefined {
	if (toolName === "write" || toolName === "edit") {
		const abs = resolveReal(String(input?.path ?? ""), cwd);
		const hit = denied(abs, scope.deny);
		if (hit) return { block: true, reason: `${abs} is protected (it matches ${hit}) and may never be written` };

		const rel = relative(real(cwd), abs).split(sep).join("/");
		// An absolute path or a climb out of the repo arrives here as
		// "../../..", which is true and unreadable; the agent gets told what it
		// actually did instead.
		if (rel.startsWith("../")) {
			return { block: true, reason: `${abs} is outside the repository and may never be written` };
		}
		if (!scope.allow.includes(rel)) {
			return {
				block: true,
				reason: `${rel} is not in the plan, so it cannot be written. Do not try another path. ` +
					`Finish the work you can do and list this path in "needed".`,
			};
		}
	}

	if (READS.includes(toolName) && scope.readRoots?.length) {
		const path = String(input?.path ?? ".");
		const reason = unreadable(toolName === "read" ? readTarget(path, cwd) : path, cwd, scope.readRoots, scope.deny);
		if (reason) return { block: true, reason };
	}

	if (toolName === "bash" || toolName === "powershell") {
		const command = String(input?.command ?? "");
		const hit = scope.bashDeny.find((rule) => new RegExp(rule).test(command));
		if (hit) return { block: true, reason: `the command matches the deny rule ${hit} and will not be run` };
	}

	return undefined;
}

const READS = ["read", "grep", "find", "ls"];

// readTarget is the file Pi's read tool opens for input: input itself when it
// exists, else the first existing spelling of Pi's resolveReadPath fallbacks.
export function readTarget(input: string, cwd: string): string {
	const norm = normalize(input);
	const p = isAbsolute(norm) ? resolve(norm) : resolve(cwd, norm);
	const nfd = p.normalize("NFD");
	const curly = (s: string) => s.replace(/'/g, "\u2019");
	return [p, p.replace(/ (AM|PM)\./gi, "\u202F$1."), nfd, curly(p), curly(nfd)].find((v) => existsSync(v)) ?? p;
}

// unreadable is why an agent held to roots may not read path, or undefined if
// it may. deny is matched against path relative to the root that holds it.
export function unreadable(path: string, cwd: string, roots: string[], deny: string[]): string | undefined {
	const abs = resolveReal(path, cwd);
	for (const root of roots) {
		const rel = relative(real(root), abs);
		if (rel === ".." || rel.startsWith(`..${sep}`) || isAbsolute(rel)) continue;
		const hit = rel ? denied(rel, deny) : undefined;
		return hit ? `${abs} is protected (it matches ${hit}) and may never be read` : undefined;
	}
	return `${abs} is outside the folders this agent may read: ${roots.join(", ")}`;
}

// withhold drops grep output lines whose path matches deny and appends how
// many it dropped. A line is "path:N: text" or "path-N- text"; every prefix
// ending at such a marker is tried, so a path with a colon cannot hide.
export function withhold(output: string, deny: string[]): string {
	let withheld = 0;
	const kept = output.split("\n").filter((line) => {
		for (const marker of line.matchAll(/[:-]\d+[:-] /g)) {
			if (denied(line.slice(0, marker.index), deny)) {
				withheld++;
				return false;
			}
		}
		return true;
	});
	if (withheld === 0) return output;
	return [...kept, `[${withheld} lines from protected paths withheld]`].join("\n");
}

// real keeps both sides of a relative() comparison in the same world:
// resolveReal has already followed symlinks, and on macOS cwd is routinely
// /var/... where the realpath is /private/var/....
function real(dir: string): string {
	try {
		return realpathSync(dir);
	} catch {
		return dir;
	}
}

export default function (pi: any) {
	const permitFile = process.env.LATHE_PERMIT;
	const contextFile = process.env.LATHE_CONTEXT_SNAPSHOT;

	// agent_end is after the session's last turn (including corrections). Pi's
	// estimate is not billed usage: after compaction tokens and percent can be
	// unknown. Preserve that uncertainty rather than converting it to zero.
	pi.on("agent_end", (_event: any, ctx: any) => {
		if (!contextFile) return;
		const usage = ctx.getContextUsage?.();
		if (usage == null) return;
		// Written aside and renamed, so a Pi killed mid-write leaves no torn
		// file. An estimate is never worth failing the agent's turn over.
		try {
			writeFileSync(contextFile + ".tmp", JSON.stringify({
				tokens: usage.tokens ?? null,
				contextWindow: usage.contextWindow ?? null,
				percent: usage.percent ?? null,
			}));
			renameSync(contextFile + ".tmp", contextFile);
		} catch {}
	});

	// No scope file is a lathe bug, and the safe reading of a missing boundary
	// is that nothing is permitted.
	const readScope = (): Scope | Block => {
		if (!permitFile) return { block: true, reason: "lathe: LATHE_PERMIT is not set" };
		try {
			return JSON.parse(readFileSync(permitFile, "utf8"));
		} catch (err) {
			return { block: true, reason: `lathe: cannot read the permit file: ${err}` };
		}
	};

	pi.on("tool_call", (event: any, ctx: any) => {
		// Reads are governed because an agent with web_read can carry anything
		// it reads out in a URL; the scope's readRoots decide whether they are
		// checked at all.
		const governed = ["write", "edit", "bash", "powershell", ...READS].includes(event.toolName);
		if (!governed) return undefined;
		const scope = readScope();
		if ("block" in scope) return scope;
		return decide(event.toolName, event.input, ctx.cwd, scope);
	});

	pi.on("tool_result", (event: any) => {
		if (event.toolName !== "grep" || event.isError) return undefined;
		// ripgrep searches hidden files, .git/ included, and a model's own glob
		// outranks any exclude, so a search is held to deny only here.
		const scope = readScope();
		// A missing scope already blocked the call, so there is no result here
		// to withhold from.
		if ("block" in scope || !scope.readRoots?.length) return undefined;
		// Pi keeps the unfiltered text of a truncated search in details, which
		// reaches the trace even though the model never sees it.
		const truncation = event.details?.truncation;
		return {
			content: event.content.map((b: any) => b.type === "text" ? { ...b, text: withhold(b.text, scope.deny) } : b),
			...(typeof truncation?.content === "string" && {
				details: { ...event.details, truncation: { ...truncation, content: withhold(truncation.content, scope.deny) } },
			}),
		};
	});
}
