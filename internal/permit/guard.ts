// guard.ts — the write boundary, in front of the tool rather than behind it.
//
// Pi's tool_call hook fires before a tool runs and can refuse it, which is the
// only place a rule about writing can be true rather than merely checked
// afterwards. lathe writes this file into the run directory and loads it with
// `--no-extensions -e`, so the target repo's own .pi/extensions never load.
//
// LATHE_PERMIT names a JSON file: { allow, deny, bashDeny }. allow is exact
// repo-relative paths — the plan's file list, already cleaned by permit.Write
// so both halves of the boundary compare the same spelling. deny and bashDeny
// are patterns. The three are rewritten before every spawn, because each
// agent's scope differs; the file is read per call so a rewrite takes effect
// without a reload.
//
// denied() is a port of internal/permit.Denied and the two must not drift: the
// Go tests and this file's tests each pass on their own while a divergence
// between them leaks. Change one, change both. guard_test.ts is what holds the
// two to the same table.

import { readFileSync, realpathSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export interface Scope {
	allow: string[];
	deny: string[];
	bashDeny: string[];
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

		const rel = relative(realCwd(cwd), abs).split(sep).join("/");
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

	if (toolName === "bash" || toolName === "powershell") {
		const command = String(input?.command ?? "");
		const hit = scope.bashDeny.find((rule) => new RegExp(rule).test(command));
		if (hit) return { block: true, reason: `the command matches the deny rule ${hit} and will not be run` };
	}

	return undefined;
}

// realCwd keeps both sides of the relative() comparison in the same world:
// resolveReal has already followed symlinks, and on macOS cwd is routinely
// /var/... where the realpath is /private/var/....
function realCwd(cwd: string): string {
	try {
		return realpathSync(cwd);
	} catch {
		return cwd;
	}
}

export default function (pi: any) {
	const permitFile = process.env.LATHE_PERMIT;

	pi.on("tool_call", (event: any, ctx: any) => {
		// read, grep, find and ls are the agent's whole working day and none of
		// them is this boundary's business, so they never touch the disk here.
		const governed = ["write", "edit", "bash", "powershell"].includes(event.toolName);
		if (!governed) return undefined;

		// No scope file is a lathe bug, and the safe reading of a missing
		// boundary is that nothing is permitted.
		if (!permitFile) return { block: true, reason: "lathe: LATHE_PERMIT is not set" };
		let scope: Scope;
		try {
			scope = JSON.parse(readFileSync(permitFile, "utf8"));
		} catch (err) {
			return { block: true, reason: `lathe: cannot read the permit file: ${err}` };
		}

		return decide(event.toolName, event.input, ctx.cwd, scope);
	});
}
