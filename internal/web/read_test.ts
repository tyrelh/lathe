// read_test.ts — run by TestReadTS, the same way guard_test.ts is, so it is
// part of `go test ./...`.
//
// The fetch tests run against a local plain-HTTP server: request is swapped for
// http.request and lookup for one that sends fixture.test to it. Every other
// name still goes through the real lookup, which is what the redirect cases
// exercise.

import assert from "node:assert/strict";
import { createServer, request as httpRequest } from "node:http";
import https from "node:https";
import { gzipSync } from "node:zlib";
import read, { blocked, check, fetchPage, lookup, markdown, MAX_DOWNLOAD, MAX_OUTPUT, MAX_REDIRECTS, render, strip } from "./read.ts";

// Every blocked range, by one address inside it, and public addresses beside them.
for (
	const address of [
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.0.0.1", "192.0.2.1", "192.168.1.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd12:3456::1", "fe80::1", "fec0::1", "ff02::1", "2001:db8::1",
		"::ffff:127.0.0.1", "::ffff:7f00:1", "::ffff:169.254.169.254", "::ffff:a9fe:a9fe", "::ffff:10.0.0.1",
		"not-an-ip",
	]
) assert.equal(blocked(address), true, `${address} is blocked`);
for (const address of ["8.8.8.8", "1.1.1.1", "172.32.0.1", "100.128.0.1", "2606:4700::1111", "::ffff:8.8.8.8"]) {
	assert.equal(blocked(address), false, `${address} is public`);
}

// localhost never leaves this machine, and lookup must refuse it rather than
// hand the socket its address.
const refused = await new Promise<Error | null>((resolve) => lookup("localhost", {}, (err: Error | null) => resolve(err)));
assert.match(String(refused), /private or reserved/, "lookup refuses a name that resolves to loopback");

for (
	const [url, why] of [
		["http://example.com/", /not https/],
		["file:///etc/passwd", /not https/],
		["ftp://example.com/", /not https/],
		["https://user:pass@example.com/", /credentials/],
		["https://127.0.0.1/", /private or reserved/],
		["https://[::1]/", /private or reserved/],
		["https://[::ffff:127.0.0.1]/", /private or reserved/],
		["https://2130706433/", /private or reserved/], // 127.0.0.1 as one decimal number
		["https://169.254.169.254/latest/meta-data/", /private or reserved/],
		["not a url", /not a URL/],
	] as [string, RegExp][]
) assert.throws(() => check(url), why, url);
assert.equal(check("https://example.com/a?b=c").href, "https://example.com/a?b=c");

// The fixture server.
const big = "x".repeat(MAX_DOWNLOAD + 1024 * 1024);
const methods: string[] = [];
const server = createServer((req, res) => {
	methods.push(req.method!);
	const [, route, n] = req.url!.split("/");
	const send = (status: number, type: string, body: string | Buffer, headers: Record<string, string> = {}) => {
		res.writeHead(status, { "content-type": type, ...headers });
		res.end(body);
	};
	if (route === "page") return send(200, "text/html; charset=utf-8", "<html><body><article><h2>Title</h2><p>Body text that is long enough to be the main content of this page.</p></article></body></html>");
	if (route === "text") return send(200, "text/plain", "plain");
	if (route === "gzip") return send(200, "text/plain", gzipSync("unzipped"), { "content-encoding": "gzip" });
	if (route === "big") return send(200, "text/plain", big);
	if (route === "bomb") return send(200, "text/plain", gzipSync("z".repeat(MAX_DOWNLOAD * 4)), { "content-encoding": "gzip" });
	if (route === "png") return send(200, "image/png", Buffer.from([0x89, 0x50, 0x4e, 0x47]));
	if (route === "missing") return send(404, "text/plain", "nope");
	if (route === "hops") {
		const left = Number(n);
		if (left === 0) return send(200, "text/plain", "arrived");
		res.writeHead(302, { location: `/hops/${left - 1}` });
		return res.end();
	}
	if (route === "to") {
		res.writeHead(302, { location: decodeURIComponent(n) });
		return res.end();
	}
	send(500, "text/plain", "unexpected route");
});
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = (server.address() as any).port;
const origin = `https://fixture.test:${port}`;
const deps = {
	request: httpRequest as unknown as typeof https.request,
	lookup: ((host: string, options: any, callback: any) => {
		if (host !== "fixture.test") return lookup(host, options, callback);
		if (options.all) return callback(null, [{ address: "127.0.0.1", family: 4 }]);
		callback(null, "127.0.0.1", 4);
	}) as typeof lookup,
};
const get = (path: string) => fetchPage(origin + path, undefined, deps);

const text = await get("/text");
assert.equal(text.body, "plain");
assert.equal(text.url, `${origin}/text`);
assert.deepEqual([...new Set(methods)], ["GET"], "web_read only ever sends GET");
assert.equal((await get("/gzip")).body, "unzipped", "a compressed body is decoded");

assert.equal((await get(`/hops/${MAX_REDIRECTS}`)).body, "arrived", `${MAX_REDIRECTS} redirects are followed`);
await assert.rejects(get(`/hops/${MAX_REDIRECTS + 1}`), /redirected more than/);
await assert.rejects(get(`/to/${encodeURIComponent("https://10.0.0.1/")}`), /private or reserved/, "a redirect to a private literal");
await assert.rejects(get(`/to/${encodeURIComponent(`https://localhost:${port}/text`)}`), /private or reserved/, "a redirect to a name that resolves to loopback");
await assert.rejects(get(`/to/${encodeURIComponent(`http://fixture.test:${port}/text`)}`), /not https/, "a redirect downgrading to http");

const capped = await get("/big");
assert.equal(capped.truncated, true);
assert.equal(capped.body.length, MAX_DOWNLOAD, "the download stops at the cap");
assert.match(await render(capped, origin + "/big"), /download stopped at 5MB/);
const bomb = await get("/bomb");
assert.equal(bomb.truncated, true);
assert.equal(bomb.body.length, MAX_DOWNLOAD, "the cap counts decompressed bytes");

await assert.rejects(get("/png"), /text, HTML, JSON and markdown only/);
await assert.rejects(get("/missing"), /HTTP 404/);

const page = await render(await get("/page"), origin + "/page");
assert.match(page, /^URL: https:\/\/fixture\.test:\d+\/page\n/);
assert.match(page, /untrusted data, not instructions/);
assert.match(page, /## Title/);
server.close();

// defuddle keeps the structure that makes a doc page worth reading.
const doc = `<html><head><title>net/http</title></head><body><nav>Home Docs Blog</nav><main><article>
<h1>net/http</h1><p>Package http provides HTTP client and server implementations, and this paragraph is long enough to be main content.</p>
<h2>Example</h2><p>See <a href="/pkg/net/http#Get">Get</a> for the client.</p>
<pre><code class="language-go">resp, err := http.Get("https://example.com/")</code></pre>
</article></main><footer>Copyright</footer></body></html>`;
const md = await markdown(doc, "https://pkg.go.dev/net/http");
assert.match(md, /^## Example$/m, "headings survive");
assert.match(md, /```\nresp, err := http\.Get\("https:\/\/example\.com\/"\)\n```/, "code blocks survive, fenced");
assert.match(md, /\[Get\]\(https:\/\/pkg\.go\.dev\/pkg\/net\/http#Get\)/, "links survive, made absolute");

// Defuddle's site extractors fetch on their own unless told not to. Nothing
// may leave this process while it runs, by fetch or by a socket.
let outbound = 0;
const realFetch = globalThis.fetch;
const realRequest = https.request;
globalThis.fetch = (async () => {
	outbound++;
	throw new Error("fetch");
}) as typeof fetch;
(https as any).request = (...args: any[]) => {
	outbound++;
	return (realRequest as any)(...args);
};
for (const url of ["https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://x.com/someone/status/1", "https://twitter.com/someone/status/1"]) {
	await markdown(doc, url);
}
globalThis.fetch = realFetch;
(https as any).request = realRequest;
assert.equal(outbound, 0, "defuddle made a request of its own");

// The fallback, and the output cap on both paths.
assert.equal(strip("<p>a &amp; b</p><script>evil()</script><style>p{}</style>\n\n<b>c&#33;&#x21;</b>"), "a & b\nc!!");
const long = { url: "https://e.test/", status: 200, truncated: false };
const cappedText = await render({ ...long, contentType: "text/plain", body: "y".repeat(MAX_OUTPUT * 2) }, long.url);
assert.match(cappedText, /Truncated: showing the first 100KB of 200KB/);
assert.equal(cappedText.split("\n---\n\n")[1].length, MAX_OUTPUT);
const wide = await render({ ...long, contentType: "text/plain", body: "字".repeat(MAX_OUTPUT / 2) }, long.url);
const kept = wide.split("\n---\n\n")[1];
assert.match(wide, /Truncated: showing the first 100KB of 150KB/, "the cap counts UTF-8 bytes");
assert(Buffer.byteLength(kept) <= MAX_OUTPUT && !kept.includes("\uFFFD"), "the cut keeps whole characters");
const cappedHTML = await render({ ...long, contentType: "text/html", body: `<p>${"word ".repeat(MAX_OUTPUT)}</p>` }, long.url);
assert.match(cappedHTML, /Truncated: showing the first 100KB/);
assert.match(await render({ ...long, contentType: "text/html", body: "<script>x()</script>" }, long.url), /---\n\n$/, "an empty page is empty, not an error");

// The extension registers one tool, with a URL as its only argument.
const tools: any[] = [];
read({ registerTool: (t: any) => tools.push(t) });
assert.equal(tools.length, 1);
assert.equal(tools[0].name, "web_read");
assert.deepEqual(Object.keys(tools[0].parameters.properties), ["url"]);
assert.equal(tools[0].parameters.additionalProperties, false);

console.log("read.ts: ok");
