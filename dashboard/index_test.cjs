// Run the embedded client against a small DOM surface and paginated API.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = new Map();
class Element {
  constructor() {
    this.innerHTML = ''; this.children = []; this.attrs = new Map();
    this.textContent = ''; this.className = ''; this.style = {}; this.dataset = {};
    this.clientLeft = 0; this.scrollLeft = 0; this.clientWidth = 400; this.scrollTop = 0; this.clientHeight = 100; this.scrollHeight = 1000;
  }
  appendChild(el) { this.children.push(el); }
  getBoundingClientRect() { return {top: 0, bottom: 0}; }
  insertRow() { const row = new Element(); this.children.push(row); return row; }
  setAttribute(k, v) { this.attrs.set(k, v); }
  removeAttribute(k) { this.attrs.delete(k); }
  // Markup is never parsed; a test that needs measurement or focus hands the
  // element its children by overriding these.
  querySelectorAll() { return []; }
  contains() { return false; }
  focus(options) { this.focused = options; }
}
for (const id of ['app', 'status', 'note', 'version',
                  'tooltip', 'nav-overview', 'nav-runs', 'nav-projects', 'nav-agents', 'follow-box', 'follow-label',
                  'project-head', 'project-runs', 'follow-home', 'follow-state', 'reader']) nodes.set(id, new Element());
// Parse only the scaffold IDs when app is replaced. In particular, do not
// precreate #log: that would skip the first-render branch in detail().
const scaffoldIDs = ['project-title', 'head', 'run-cancel', 'phases', 'phase', 'costs', 'summary',
  'iterations', 'revise', 'revise-request', 'revise-submit', 'revise-status', 'log', 'run-total',
  'tab-phases', 'tab-events', 'phases-panel', 'events-panel', 'event-controls', 'events-body', 'run-side'];
const pageIDs = ['page-summary', 'average-spend', 'rank-projects', 'rank-runs', 'rank-models', 'rank-providers', 'list-body',
  'project-head', 'project-runs', 'new-run', 'run-prompt', 'run-issue', 'run-type', 'run-submit', 'run-status',
  'agent-head', 'agent-prompts'];
// The agent page redraws each prompt box's editor on its own, so those regions
// register the IDs inside them, and each editor registers its own.
const parsedIDs = new Map();
const forget = el => {
  for (const id of parsedIDs.get(el) || []) { forget(nodes.get(id)); nodes.delete(id); }
  parsedIDs.delete(el);
};
function parses(el) {
  let html = '';
  Object.defineProperty(el, 'innerHTML', {
    get: () => html,
    set: value => {
      html = value;
      forget(el);
      const ids = [...value.matchAll(/id="([^"]+)"/g)].map(match => match[1]);
      parsedIDs.set(el, ids);
      for (const id of ids) {
        assert(!nodes.has(id), `duplicate ID ${id}`);
        const child = new Element();
        if (id.endsWith('-edit')) parses(child);
        nodes.set(id, child);
      }
    },
  });
}
// deepHTML is el's markup with every registered region's contents filled in.
const deepHTML = el => el.innerHTML.replace(/(<[^>]+id="([^"]+)"[^>]*>)/g, (tag, _, id) => tag + (nodes.has(id) ? deepHTML(nodes.get(id)) : ''));
const appNode = nodes.get('app');
let appHTML = '', scaffoldCount = 0;
Object.defineProperty(appNode, 'innerHTML', {
  get: () => appHTML,
  set: html => {
    appHTML = html;
    if (nodes.has('agent-prompts')) forget(nodes.get('agent-prompts'));
    for (const id of [...scaffoldIDs, ...pageIDs]) nodes.delete(id);
    const ids = [...html.matchAll(/id="([^"]+)"/g)].map(match => match[1]);
    for (const id of ids) {
      assert([...scaffoldIDs, ...pageIDs].includes(id), `unregistered app ID ${id}`);
      nodes.set(id, new Element());
      if (id === 'agent-prompts') parses(nodes.get(id));
    }
    if (ids.includes('log')) {
      scaffoldCount++;
      for (const id of scaffoldIDs) assert(ids.includes(id), `stable run region: ${id}`);
    }
  },
});
const pageHTML = () => appHTML.replace(/(<[^>]+id="([^"]+)"[^>]*>)/g,
  (tag, _, id) => tag + (nodes.get(id)?.innerHTML || ''));
let response, resolveFetch, failNext = null, frames = [];
let routeResponses = null;
const observers = [];
const listeners = new Map();
const intervals = [];
// confirms records every confirm() the page asks; confirmAnswer is the reply.
const confirms = [];
let confirmAnswer = true;
const context = vm.createContext({
  document: {
    getElementById: id => nodes.get(id) ?? null, createElement: () => new Element(),
    body: {scrollHeight: 1000}, activeElement: null,
  },
  confirm: message => { confirms.push(message); return confirmAnswer; },
  // went records the last history.go().
  history: {state: null, replaceState(state, title, url) { this.state = state; if (url !== undefined) context.location.hash = url; }, go(n) { this.went = n; }},
  location: {hash: '#run'}, addEventListener(type, handler) { listeners.set(type, handler); }, setInterval(f) { intervals.push(f); }, setTimeout, clearTimeout, Date, URLSearchParams,
  innerWidth: 800, innerHeight: 600, scrollY: 0, scrollTo() { throw Error('must not scroll a reader away'); },
  ResizeObserver: class {
    constructor(callback) { this.callback = callback; this.watching = []; observers.push(this); }
    observe(el) { this.watching.push(el); }
    disconnect() { this.watching = []; }
  },
  requestAnimationFrame: f => frames.push(f),
  fetch: async url => {
    if (url === '/api/meta') return json({version: 'v0.2.0-test'});
    if (failNext) { const e = failNext; failNext = null; return {ok: false, text: async () => e}; }
    if (routeResponses?.has(url)) return routeResponses.get(url);
    return response || await new Promise(resolve => { resolveFetch = resolve; });
  },
});
const json = (body, headers = {}) =>
  ({ok: true, headers: {get: k => headers[k] ?? null}, json: async () => body});

// Everything up to the bootstrap; the harness drives tick() itself. Cut at one
// anchor rather than matching the call verbatim, so reformatting it does not
// silently leave the page self-ticking against an unresolved fetch.
const source = fs.readFileSync('index.html', 'utf8');
const script = source.match(/<script>([\s\S]*?)<\/script>/)[1];
const bootstrap = script.lastIndexOf('navigate();');
assert(bootstrap > 0, 'bootstrap not found');
vm.runInContext(script.slice(0, bootstrap), context);
const evaluate = code => vm.runInContext(code, context);
const call = (fn, ...args) => { context.args = args; return evaluate(`${fn}(...args)`); };
assert.equal(call('averageMoney', 1, 4), '$0.25');
assert.equal(call('averageMoney', 9, 0), '$0.00');
assert.equal(call('averageMoney', 0, 3), '$0.00');
for (const [n, text] of [[0, '$0.00'], [null, '$0.00'], [0.00004, '$0.00004'], [0.00123, '$0.0012'],
                         [0.45482, '$0.45'], [2.99474, '$2.99'], [22.73561, '$22.74'],
                         [121.64971, '$121.65'], [1234.5, '$1,234.50'], [0.1, '$0.10'], [0.5, '$0.50'],
                         [0.9, '$0.90'], [0.0333, '$0.03'], [0.01, '$0.01']])
  assert.equal(call('money', n), text, `money(${n})`);
// Formatting must not depend on roundingPriority, which older browsers ignore.
assert(!script.includes('roundingPriority'), 'money must not rely on roundingPriority');

// Immediate text-only content, viewport clamping, keyboard dismissal, and
// rebinding when polling replaces the hovered chart block.
const tip = nodes.get('tooltip');
tip.getBoundingClientRect = () => ({width: 300, height: 120});
const trigger = new Element();
trigger.dataset = {tooltip: '<script>unsafe</script>\nStatus: success', tooltipKey: 'phase-c1'};
trigger.getBoundingClientRect = () => ({left: 780, top: 550, bottom: 570});
trigger.closest = () => trigger;
listeners.get('pointerover')({target: trigger, pointerType: 'mouse'});
assert.equal(tip.hidden, false);
assert.equal(tip.textContent, trigger.dataset.tooltip, 'tooltip content stays literal text');
assert.equal(tip.style.left, '492px', 'tooltip fits the right edge');
assert.equal(tip.style.top, '424px', 'tooltip flips above a low trigger');
assert.equal(trigger.attrs.get('aria-describedby'), 'tooltip');
listeners.get('keydown')({key: 'Escape'});
assert.equal(tip.hidden, true);
assert(!trigger.attrs.has('aria-describedby'));
listeners.get('focusin')({target: trigger});
assert.equal(tip.hidden, false, 'keyboard focus opens the tooltip');
const refreshed = new Element();
refreshed.dataset = {...trigger.dataset, tooltip: 'Status: running\nDuration: 2s'};
refreshed.getBoundingClientRect = () => ({left: -10, top: 0, bottom: 20});
const region = new Element();
region.contains = el => el === trigger;
region.querySelectorAll = selector => selector === '[data-tooltip-key]' ? [refreshed] : [];
call('swap', region, '<button>refreshed</button>');
assert.equal(tip.textContent, refreshed.dataset.tooltip, 'polling updates an open tooltip');
assert.equal(tip.style.left, '8px');
assert.equal(tip.style.top, '26px');
assert(!trigger.attrs.has('aria-describedby'));
assert.equal(refreshed.attrs.get('aria-describedby'), 'tooltip');
region.contains = el => el === refreshed;
region.querySelectorAll = () => [];
call('swap', region, '');
assert.equal(tip.hidden, true, 'removing the trigger clears its tooltip');
listeners.get('pointerover')({target: trigger, pointerType: 'touch'});
assert.equal(tip.hidden, true, 'touch does not create a hover tooltip');

// Routing: the agreed default, the explicit routes, the legacy deep link, and
// an unknown hash that must not be mistaken for a run id.
for (const [hash, want] of [
  ['', 'overview'], ['#/', 'overview'], ['#/overview', 'overview'], ['#/runs', 'runs'],
  ['#/projects', 'projects'], ['#/projects/detail?repo=%2Frepos%2Falpha', 'project'],
  ['#/runs/20260920T120000Z_build_1', 'run'], ['#20260920T120000Z_build_1', 'run'],
  ['#/nope', 'missing'], ['#/agents', 'agents'], ['#/agents/route-plan', 'agent'], ['#/agents/', 'missing'],
  ['#/agents/%E0%A4%A', 'missing'],
]) {
  context.location.hash = hash;
  const r = evaluate('route()');
  assert.equal(r.page, want, `${hash} -> ${r.page}`);
  if (want === 'run') assert.equal(r.id, '20260920T120000Z_build_1', hash);
  if (want === 'project') assert.equal(r.repo, '/repos/alpha', hash);
  if (want === 'agent') assert.equal(r.name, 'route-plan', hash);
}
context.location.hash = '#/agents/' + encodeURIComponent("o'<b>");
assert.equal(evaluate('route()').name, "o'<b>", 'agent names are decoded');

const long = 'x'.repeat(400) + '<script>tail</script>';
context.long = long;
const preview = evaluate(`eventPayload({type:'envelope', payload:{text:long}})`);
assert(preview.split('</summary>')[0].includes('…'));
assert(preview.includes('&lt;script>tail&lt;/script>'));
assert(!preview.includes('<script>'));
for (const e of [
  {type:'tool_call', name:'bash', payload:{args:{command:long}}},
  {type:'tool_call', name:'write', payload:{isError:true, result:{reason:long}}},
  {type:'log', name:'command', payload:long},
]) {
  context.event = e;
  assert(evaluate('eventPayload(event)').includes('tail'));
}
assert(evaluate(`eventPayload({type:'tool_call',payload:{isError:true}})`).includes('class="fail"'));

// The chart, against a controlled clock: a run from 12:00:00 to 12:01:40 is
// 100s, so every second is exactly one percent of the track.
const T = s => new Date(Date.UTC(2026, 8, 20, 12, 0, s)).toISOString().replace('.000', '');
const ms = s => Date.parse(T(s));
const settled = {status:'ok', started_at:T(0), ended_at:T(100)};
const ph = (seq, name, from, to, extra = {}) =>
  ({phase_id:'c'+seq, seq, name, owner:'builder', status:'success', error:'', started_at:from, ended_at:to, ...extra});
const noNaN = html => assert(!/NaN|Infinity|-\d/.test(html.match(/style="[^"]*"/g)?.join('') || ''), html);

let html = call('gantt', settled, [ph(1,'plan',T(10),T(35))], ms(200));
assert(html.includes('left:min(10.000%, calc(100% - 2px));width:max(2px, 25.000%)'), html);
assert(!html.includes(' title='), 'chart uses custom tooltips only');
assert(html.includes('Duration: 25s'), 'phase tooltip includes duration');
assert(html.includes('12:00:00 UTC') && html.includes('12:01:40 UTC'), 'axis labels start and end in UTC');
// Clamped to the axis, and a zero-length block at the very end stays in the track.
html = call('gantt', settled, [ph(1,'early',T(-20),T(10)), ph(2,'edge',T(100),T(100))], ms(200));
assert(html.includes('left:min(0.000%, calc(100% - 2px));width:max(2px, 10.000%)'), html);
assert(html.includes('left:min(100.000%, calc(100% - 2px));width:max(2px, 0.000%)'), html);
noNaN(html);

// Repeated names share a row; each entry keeps its own block, in seq order.
html = call('gantt', settled, [ph(3,'plan',T(50),T(60)), ph(1,'plan',T(0),T(20)), ph(2,'review',T(20),T(50))], ms(200));
assert.equal(html.split('class="node"').length - 1, 2, 'one row per node');
const planRow = html.split('class="node"')[1];
assert(planRow.indexOf('data-phase="c1"') < planRow.indexOf('data-phase="c3"'), 'blocks follow seq');
assert(!planRow.split('class="track"')[1].split('</div>')[0].includes('c2'), 'review is its own row');
assert(html.includes('aria-label="#3 plan · success"'), 'the accessible name carries the sequence');
assert(html.includes('<span class="piece" aria-hidden="true">✅</span>'), 'the block shows its status mark');
assert(!html.includes('data-line='), 'all chart rows use a single line');
assert(html.includes('class="block ok" style="--hue:var(--dim);'), 'a phase with no model fills dim, border the status');
assert.equal(call('modelColour', 'claude-fable-5.1'), 'color-mix(in srgb, #D57355 100%, var(--card))', 'dotted names match');
assert.equal(call('modelColour', 'kimi-k2.6'), 'color-mix(in srgb, #1C81FA 75%, var(--card))', 'models draw at their tier');
assert.equal(call('modelColour', 'claude-haiku-4-5'), 'color-mix(in srgb, #D57355 50%, var(--card))');
assert.equal(call('modelColour', 'kimi-k9'), call('modelColour', 'kimi-k3'), 'unlisted models take the full provider hue');
assert.equal(call('modelColour', 'mystery'), '#7A9EFB', 'unknown providers take the default hue');
assert.equal(call('providerColour', 'anthropic'), '#D57355', 'providers draw at their full hue');
assert.equal(call('providerColour', 'openai-codex'), '#439F7C', 'provider variants share a hue');
assert.equal(call('providerColour', 'unknown'), '#7A9EFB', 'unknown providers take the default hue');
assert.equal(call('modelColour', 'jev-1.13.0'), 'color-mix(in srgb, #F386A1 100%, var(--card))', 'jev draws in the TypeSafe hue');
assert.equal(call('providerColour', 'typesafe'), '#F386A1', 'TypeSafe has its own hue');
{
  const levels = [
    {when: 'Mechanical', provider: 'openai-codex', model: 'gpt-6-luna', probability: 0.7},
    {when: 'Behaviour', provider: 'openai-codex', model: 'gpt-6-sol', default: true, probability: 0.2},
    {when: 'Cross-cutting <b>', provider: 'openai-codex', model: 'gpt-6-astra', thinking: 'high', probability: 0.1},
  ];
  const routed = call('routeResult', {agent: 'builder', tier: 1, reason: 'low-confidence', floor: 0.5,
    confidence: 0.31, score: 0.4, jev: 'jev-1.13.0', levels});
  for (const want of ['<b>openai-codex/gpt-6-sol</b>', 'tier 2 of 3', 'low-confidence', 'under the 0.5 floor',
                      '0.31', '70.0%', 'jev-1.13.0', '· default', 'Cross-cutting &lt;b>', 'thinking high']) {
    assert(routed.includes(want), `route result missing ${want}: ${routed}`);
  }
  const failed = call('routeResult', {agent: 'builder', tier: 1, reason: 'error', floor: 0.5,
    error: 'TYPESAFE_API_KEY is not set', levels: levels.map(({probability, ...l}) => l)});
  for (const want of ['class="fail">error', 'default tier was used', 'No answer', 'TYPESAFE_API_KEY is not set', '—']) {
    assert(failed.includes(want), `failed route result missing ${want}: ${failed}`);
  }
  assert.equal(call('prettyJSON', '{"a":1}'), '{\n  "a": 1\n}');
  assert.equal(call('prettyJSON', 'not json'), 'not json');
}
assert(call('gantt', settled, [ph(1,'x',T(0),T(5),{owner:'"><b>'})], 0).includes('data-owner="&quot;>&lt;b>"'), 'owner is escaped');

// Execution arrows follow send-backs, but never bridge iterations, concurrent
// work, or an invalid timestamp entry (which would invent a missing handoff).
const links = phases => JSON.parse(JSON.stringify(call('phaseLinks', phases, settled, ms(200))));
assert.deepEqual(links([ph(3,'plan',T(50),T(60)), ph(1,'plan',T(0),T(20)), ph(2,'review',T(20),T(50))]), [['c1','c2'],['c2','c3']]);
assert.deepEqual(links([ph(1,'plan',T(0),T(20)), ph(2,'plan',T(30),T(40),{iteration:1})]), []);
assert.deepEqual(links([ph(1,'test',T(0),T(40)), ph(2,'review',T(10),T(30))]), []);
assert.deepEqual(links([ph(1,'plan',T(0),T(20)), ph(2,'broken','invalid',T(30)), ph(3,'review',T(30),T(40))]), []);
assert.deepEqual(links([ph(1,'plan',T(0),''), ph(2,'review',T(30),T(40))]), []);
for (const target of [{left:20,right:40,top:28,bottom:46}, {left:20,right:40,top:0,bottom:18}, {left:25,right:40,top:56,bottom:74}]) {
  const path = call('flowPath', {left:0,right:20,top:56,bottom:74}, target);
  assert(!/NaN|Infinity/.test(path));
  assert(path.startsWith('M 20 65'), 'arrow starts at the source end');
}

// Before the run starts, with no phases, with no length, and with bad times.
assert(call('gantt', {status:'queued', started_at:'', ended_at:''}, [], ms(0)).includes('Waiting for run to start.'));
assert(call('gantt', {status:'running', started_at:T(0), ended_at:''}, [], ms(5)).includes('No phases recorded yet.'));
html = call('gantt', {status:'ok', started_at:T(0), ended_at:T(0)}, [ph(1,'blink',T(0),T(0))], ms(0));
assert(html.includes('left:min(0.000%, calc(100% - 2px));width:max(2px, 0.000%)'), html);
noNaN(html);
html = call('gantt', settled, [ph(1,'garbled','yesterday',T(5)), ph(2,'reversed',T(50),T(40)), ph(3,'fine',T(0),T(10))], ms(200));
const untimed = html.split('Untimed')[1];
assert(untimed.includes('data-phase="c1"') && untimed.includes('data-phase="c2"'), 'bad times stay selectable');
assert(!untimed.includes('data-phase="c3"'));
noNaN(html);
assert(call('gantt', {status:'fail', started_at:T(0), ended_at:''}, [ph(1,'x',T(0),T(5))], ms(200))
  .includes('Timing unavailable'), 'a settled run with no end does not measure against now');

// Unfinished phases: running while live and growing with the clock;
// interrupted once settled, bounded by the run's end whatever the clock says.
const open = ph(1,'implement',T(0),'',{status:'fail'});
const liveRun = {status:'running', started_at:T(0), ended_at:''};
const w = now => call('gantt', liveRun, [ph(1,'implement',T(5),'',{status:'fail'})], now);
assert(w(ms(10)).includes('implement · running') && w(ms(10)).includes('>now<'));
assert(w(ms(10)).includes('aria-hidden="true">⏳</span>'), 'running is marked');
assert(w(ms(10)).includes('left:min(50.000%, calc(100% - 2px));width:max(2px, 50.000%)'), w(ms(10)));
assert(w(ms(20)).includes('left:min(25.000%, calc(100% - 2px));width:max(2px, 75.000%)'), 'a running phase grows with now');
const cut = {status:'fail', started_at:T(0), ended_at:T(40)};
assert.equal(call('gantt', cut, [open], ms(100)), call('gantt', cut, [open], ms(900)), 'interrupted phases stop growing');
assert(call('gantt', cut, [open], ms(100)).includes('class="block interrupted"'));
assert(call('gantt', cut, [open], ms(100)).includes('>⚠️</span>'), 'interrupted is marked');
assert.equal(call('gantt', settled, [ph(1,'t',T(0),T(9),{status:'fail'}), ph(2,'u',T(0),T(9),{status:'odd'})], 0)
  .match(/>(❌|❔)</g).join(''), '>❌<>❔<', 'failures and unknown statuses are marked');
const card = call('details', open, cut, ms(900));
assert(card.includes('interrupted') && card.includes('40s · bounded by the run’s end') && card.includes('Not recorded'), card);
assert(call('gantt', settled, [ph(1,'test',T(0),T(9),{status:'fail'})], 0).includes('test · fail'), 'finished failures keep their status');

// A revised run: two iterations of 100s an hour apart. The idle 3500s is one
// break a twentieth of the 200s of work wide (10s), so the track spans 210s:
// the second plan starts 100s + 10s + 10s in. An iterations row names each
// request over its span, and the break's label says how long it stands for.
const H = 3600;
const revised = {status:'ok', started_at:T(0), ended_at:T(H + 100), iteration:1, iterations:[
  {iteration:0, request:'greet', status:'ok', started_at:T(0), ended_at:T(100)},
  {iteration:1, request:'<louder>', status:'ok', started_at:T(H), ended_at:T(H + 100)}]};
html = call('gantt', revised, [ph(1,'plan',T(10),T(35),{iteration:0}), ph(2,'plan',T(H+10),T(H+35),{iteration:1})], ms(H+200));
assert(html.includes('left:min(4.762%, calc(100% - 2px));width:max(2px, 11.905%)'), html);
assert(html.includes('left:min(57.143%, calc(100% - 2px));width:max(2px, 11.905%)'), html);
assert(html.includes('⋯ 58m20s') && html.includes('data-tooltip="idle 58m20s · 2026-09-20T12:01:40.000Z'), 'the break keeps its real span');
assert(html.includes('#1 &lt;louder>') && html.includes('>iterations</div>'), 'iterations are named over their spans');
noNaN(html);
assert.equal(call('active', revised), '3m20s', 'active time is the iterations, not the idle between them');
// One iteration draws exactly as before: no break, no iterations row.
const single = {...settled, iterations:[{iteration:0, request:'x', status:'ok', started_at:T(0), ended_at:T(100)}]};
assert.equal(call('gantt', single, [ph(1,'plan',T(10),T(35))], ms(200)), call('gantt', settled, [ph(1,'plan',T(10),T(35))], ms(200)));

// Reopening a run does not make an old unfinished phase run again: it is
// bounded by its own iteration's end, and says it is from an earlier request.
const reopened = {status:'running', started_at:T(0), ended_at:'', iteration:1, iterations:[
  {iteration:0, request:'greet', status:'ok', started_at:T(0), ended_at:T(40)},
  {iteration:1, request:'louder', status:'running', started_at:T(H), ended_at:''}]};
const stale = ph(1,'implement',T(5),'',{status:'fail', iteration:0});
assert(call('gantt', reopened, [stale], ms(H+50)).includes('class="block interrupted"'), 'an old phase is interrupted, not running');
const staleCard = call('details', stale, reopened, ms(H+50));
assert(staleCard.includes('35s · bounded by its iteration’s end') && staleCard.includes('iteration ended ' + T(40)), staleCard);
assert(staleCard.includes('From iteration 0, a previous request: greet') && staleCard.includes('which is running'), staleCard);
assert(!call('details', ph(2,'plan',T(H+1),T(H+9),{iteration:1}), reopened, ms(H+50)).includes('From iteration'),
  'the latest iteration is not labelled as previous');

// The history: each request, how it ended, where its commit is and the models.
const history = call('renderIterations', {iterations:[
  {iteration:0, request:'<greet>', status:'ok', commit:'abcdef1234', remote:'', models:{planner:'moonshotai/kimi-k3'}},
  {iteration:1, request:'louder', status:'fail', reason:'not published', commit:'1234567890', remote:'abcdef1234', models:{}}]});
for (const text of ['&lt;greet>', 'abcdef12', 'origin at abcdef12', 'kimi-k3', 'planner: moonshotai/kimi-k3', 'not published'])
  assert(history.includes(text), `history: ${text}`);

// Status marks stay right-aligned inside wide blocks and prefer spilling right.
const arrange = (blocks, width) => JSON.parse(JSON.stringify(call('arrange', blocks, width)));
assert.deepEqual(arrange([{left:0, width:200, label:20}], 400), [{x:177, side:'inside'}]);
assert.deepEqual(arrange([{left:40, width:2, label:16}], 100), [{x:5, side:'right'}]);
assert.deepEqual(arrange([{left:98, width:2, label:16}], 100), [{x:-19, side:'left'}]);
assert.deepEqual(arrange([{left:40, width:2, label:16}, {left:50, width:40, label:16}], 100),
  [{x:-19, side:'left'}, {x:21, side:'inside'}], 'a neighbour can force left overflow');
assert.deepEqual(arrange([{left:0, width:2, label:16}], 10), [{hidden:true}],
  'a status with no room remains available in the tooltip instead of clipping');

// Generic reports are data, escaped to the keys, and nothing falsy vanishes.
const generic = call('renderReport', {'<key>': [{name: '<b>', none: null, off: false, zero: 0, list: [], obj: {}}], text: 'a\n  b'});
for (const text of ['<h4>&lt;key></h4>', '<h4>name</h4><p>&lt;b></p>', '<p>null</p>', '<p>false</p>', '<p>0</p>',
                    'Empty list.', 'Empty object.', '<p>a\n  b</p>', '<ul><li><h4>name']) assert(generic.includes(text), text);
assert(!generic.includes('<b>'));

const phases = [ph(1,'fix',T(0),T(30)), ph(2,'fix',T(30),T(65))].map(p => ({...p, phase_id:'p'+p.seq}));
const run = {run_id:'run', workflow:'build',status:'ok',tokens:30,cost:0.3,repo:'/repo',request:'fix',submitted_at:'2026-09-20T11:59:50Z',activity_at:'2026-09-20T11:59:50Z',started_at:'2026-09-20T12:00:00Z',ended_at:'2026-09-20T12:01:05Z'};
let id = 0;
const event = (type, payload, phase_id='p1', name='fix') => ({event_id:++id,type,payload,phase_id,name});
function serve(events, ps = phases, r = run) { response = json({run:r,phases:ps,events}); }
const chart = () => nodes.get('phases').innerHTML, panel = () => nodes.get('phase').innerHTML;
const costs = () => nodes.get('costs').innerHTML;
(async () => {
  context.location.hash = '#run';
  evaluate('view = route()');

  const events = [event('input',{prompt:'first'}), event('usage',{attempt:0,seq:1,tokens:10,cost:0.1,model:'model'}),
    event('context',{attempt:0,tokens:1200,contextWindow:8000,percent:15.26}), event('permit',{kept:['a.go'],reverted:['<bad>']})];
  while (events.length < 500) events.push(event('log','padding'));
  assert.equal(nodes.has('log'), false, 'the run scaffold must not be precreated');
  serve(events, phases, {...run, repo:'/repo/MiXeD <&"\'>', request:'fix <prompt>&'});
  await evaluate('tick()');
  assert.equal(scaffoldCount, 1, 'the first poll creates the whole run scaffold');
  const scaffold = pageHTML();
  for (const id of ['phases-panel', 'events-panel', 'events-body', 'run-side'])
    assert(scaffold.includes(`id="${id}"`), `bounded run region: ${id}`);
  assert(scaffold.includes('role="tablist"') && scaffold.includes('aria-controls="phases-panel"'));
  assert(scaffold.includes('id="events-panel" role="tabpanel" aria-labelledby="tab-events" hidden'));
  assert.equal(nodes.get('project-title').innerHTML, '/repo/MiXeD &lt;&amp;&quot;&#39;>');
  assert.equal(nodes.get('head').innerHTML, 'fix &lt;prompt>&amp;');
  assert.equal(evaluate('done'), false, 'full page must keep polling a settled run');
  assert(nodes.get('run-total').textContent === '$0.30' && costs().includes('unknown') && costs().includes('model') && costs().includes('$0.10'), costs());
  assert(costs().includes('Loading usage…') && !costs().includes('No usage recorded.'), 'partial rows still load');
  assert(costs().indexOf('providers') < costs().indexOf('models'), 'providers precede models');
  assert(!costs().includes('unknown/model'), 'model names are bare');
  assert.deepEqual(JSON.parse(JSON.stringify(evaluate('providerCosts()'))), [{provider:'unknown', cost:0.1}],
    'missing provider becomes unknown in providerCosts');
  assert(chart().includes('fix · model · success · $0.10'), chart());
  assert(chart().includes('Context (estimated): 1,200 tokens / 8,000 window · 15.3% full'),
    'block tooltip rounds the snapshot to one decimal');
  assert(!chart().match(/data-phase="p2"[^>]*data-tooltip="[^"]*Context \(estimated\)/),
    'a phase without a snapshot has no context in its tooltip');
  assert(chart().includes('<span class="piece" aria-hidden="true">✅</span></button>'),
    'only the status is visible; model and cost remain in the tooltip');
  assert(chart().includes('aria-label="#2 fix · success"'), 'no usage means no model or cost');
  assert.equal(chart().split('class="node"').length - 1, 1, 'both entries share a row');
  assert(panel().includes('Select a phase.'));
  assert.equal(nodes.get('events-body').scrollTop, 0, 'must not scroll a reader away');
  assert(observers[0].watching.includes(nodes.get('phases')), 'the chart is watched for width changes');
  assert.equal(nodes.get('follow-label').style.display, 'inline-flex', 'follow toggle is shown on a run page');
  assert.equal(nodes.get('follow-box').checked, undefined, 'follow defaults off');

  // Selection renders from cache; missing usage is still loading while the
  // backlog drains, not "not recorded".
  evaluate(`pick('p2')`);
  assert(panel().includes('Loading usage…') && panel().includes('Input context not recorded.'), panel());
  assert(chart().includes('data-phase="p2" data-focus="block-p2" aria-pressed="true"'));
  evaluate(`pick('p1')`);
  assert(panel().includes('first') && panel().includes('#1'), panel());
  assert(panel().includes('Context (estimated): 1,200 tokens / 8,000 window · 15.3% full'), panel());
  assert.equal(call('contextEstimate', {tokens:0,contextWindow:100,percent:15}),
    'Context (estimated): 0 tokens / 100 window · 15.0% full', 'whole percentages keep a decimal');
  evaluate(`panels.set('p1-inputs', true)`);

  // Keyboard focus comes back to the same control, without scrolling, and
  // only when it was inside the replaced region.
  const focused = new Element(), twin = new Element();
  focused.dataset.focus = twin.dataset.focus = 'block-p1';
  const hovered = new Element(), polled = new Element();
  hovered.dataset = {tooltipKey:'phase-p1', tooltip:'old context'};
  polled.dataset.tooltipKey = 'phase-p1';
  polled.getBoundingClientRect = hovered.getBoundingClientRect = () => ({left:10, top:10, bottom:30});
  nodes.get('phases').contains = el => el === focused || el === hovered;
  nodes.get('phases').querySelectorAll = sel => sel === '[data-focus]' ? [twin]
    : sel === '[data-tooltip-key]' ? [polled] : [];
  call('tooltip.show', hovered);
  // Mimic the browser parsing the refreshed button's data-tooltip attribute.
  Object.defineProperty(polled.dataset, 'tooltip', {get: () => chart().match(/data-phase="p1"[^>]*data-tooltip="([^"]*)"/)[1]});
  context.document.activeElement = focused;

  // With follow disabled, being at the bottom must not pull the page down.
  nodes.get('follow-box').checked = false;
  nodes.get('events-body').scrollTop = nodes.get('events-body').scrollHeight - nodes.get('events-body').clientHeight;
  const nextEvents = [event('input',{prompt:'correction'}),event('usage',{attempt:1,seq:1,provider:'pi',tokens:20,cost:0.2}),
         event('context',{attempt:1,tokens:null,contextWindow:8000,percent:null}),
         event('log','test command','p2','command'),event('log','  <b>spaced</b>\n  out','p2','output'),
         event('log','','p2','output')];
  serve(nextEvents);
  await evaluate('tick()');
  assert.equal(evaluate('done'), true);
  assert(tip.textContent.includes('Context (estimated): unknown tokens / 8,000 window · unknown full'),
    'polling refreshes the open block tooltip with the latest snapshot');
  assert.equal(tip.textContent, polled.dataset.tooltip);
  assert.equal(nodes.get('events-body').scrollTop, nodes.get('events-body').scrollHeight - nodes.get('events-body').clientHeight, 'follow off preserves scroll position');
  assert.deepEqual({...twin.focused}, {preventScroll: true}, 'focus restored without scrolling');
  twin.focused = null; context.document.activeElement = new Element();
  evaluate('draw()');
  assert.equal(twin.focused, null, 'focus elsewhere is left alone');
  call('tooltip.hide');
  nodes.get('phases').contains = () => false; nodes.get('phases').querySelectorAll = () => [];

  // The selection and its open disclosures survived the poll; spend summed
  // once across pages, with one row per recorded response.
  for (const text of ['first','correction','30 tok','$0.30','unknown/model','pi/unknown',
                      'data-panel="p1-inputs" open']) assert(panel().includes(text), text);
  assert.equal(panel().split('attempt ').length - 1, 2, 'one usage row per response');
  assert(panel().includes('Context (estimated): unknown tokens') && panel().includes('8,000 window · unknown full'),
    'latest snapshot wins, unknown after compaction is not zero');
  assert(!panel().includes('1,200 tokens'), 'earlier snapshot is replaced on polling');
  assert(chart().includes('$0.30'));

  // Selection still works after polling stops, and disclosure state is per phase.
  evaluate(`pick('p2')`);
  for (const text of ['test command','<pre>  &lt;b>spaced&lt;/b>\n  out</pre>','The command produced no output.',
                      'Usage not recorded.']) assert(panel().includes(text), text);
  assert(!panel().includes('Structured output not recorded'), 'a command-only phase has no report to miss');
  assert(!panel().includes('data-panel="p2-inputs" open'));
  assert(!panel().includes('Context (estimated)'), 'no context snapshot on another phase');
  evaluate(`pick('p1')`);
  assert(panel().includes('data-panel="p1-inputs" open'), 'switching back keeps what was open');
  evaluate(`pick('p1')`);
  assert(panel().includes('Select a phase.'), 'clicking the selected block deselects it');
  assert.equal(nodes.get('log').children.length, events.length + nextEvents.length,
    'each event from both polls is appended exactly once, including context snapshots');
  const permitRows = nodes.get('log').children.filter(row => row.innerHTML.includes('>permit<'));
  assert.equal(permitRows.length, 1, 'permit event appears once across paginated polls');
  assert(permitRows[0].innerHTML.includes('&lt;bad>') && !permitRows[0].innerHTML.includes('<bad>'),
    'permit payload is escaped in the events log');
  // The bottom bar carries the run's own context, and a settled run reads run complete.
  const bar = nodes.get('status').innerHTML;
  // Queue time and execution time are separate: 10s waiting, 1m05s working.
  for (const text of ['build','ok','$0.30','10s queued','1m05s','repo','run']) assert(bar.includes(text),`status: ${text}`);
  const summary = nodes.get('summary').innerHTML;
  for (const text of ['<dt>workflow</dt><dd>build</dd>', '<dt>status</dt><dd class="ok">ok</dd>',
                      '<dt>queued</dt><dd>10s</dd>', '<dt>duration</dt><dd>1m05s</dd>', '<dt>run</dt><dd>run</dd>'])
    assert(summary.includes(text), `details card: ${text}`);
  assert(!summary.includes('<dt>cost</dt>'), 'cost is shown once in the run summary');
  assert(!costs().includes('Loading usage…') && costs().includes('pi') && costs().includes('model'),
    'finished pages have final rows');
  assert(!costs().includes('pi/unknown') && !costs().includes('unknown/model'), 'model names are bare');
  assert.equal(scaffoldCount, 1, 'a redraw does not rebuild the append-only event stream');
  assert.equal(nodes.get('note').textContent, 'run complete');
  assert.equal(nodes.get('follow-label').style.display, 'inline-flex', 'follow remains available in event controls after completion');

  // Label placement reads the rendered pixels, resets what it set last time,
  // and runs again when the chart's width changes.
  const piece = scrollWidth => Object.assign(new Element(), {scrollWidth});
  const blocks = [[piece(20)], [piece(40)]]
    .map((children, i) => Object.assign(new Element(), {offsetLeft: i * 100, offsetWidth: 10, children}));
  const pieces = blocks.flatMap(b => b.children);
  const track = Object.assign(new Element(), {clientWidth: 400, querySelectorAll: () => blocks});
  nodes.get('phases').querySelectorAll = sel => sel === '.track' ? [track] : [];
  pieces[1].style.display = 'none';
  evaluate('layout()');
  assert.deepEqual(pieces.map(c => c.style.left), ['13px', '13px']);
  assert.equal(pieces[1].style.display, '', 'placement starts from a clean slate');
  track.clientWidth = 150;
  observers[0].callback([]); observers[0].callback([]);
  assert.equal(frames.length, 1, 'resize callbacks coalesce into one frame');
  frames.shift()();
  assert.deepEqual(pieces.map(c => c.style.left), ['13px', '-43px']);
  blocks[1].offsetLeft = 5;
  evaluate('layout()');
  assert.equal(pieces[0].style.display, 'none');
  assert(blocks.every(b => b.style.top === undefined), 'overlaps never introduce extra lanes');
  assert.equal(track.style.height, undefined);
  nodes.get('phases').querySelectorAll = () => [];

  // With follow enabled, the page snaps to the tail when the reader is already at the bottom.
  evaluate("done = false; activeTab = 'events'");
  nodes.get('follow-box').checked = true;
  nodes.get('events-body').scrollTop = nodes.get('events-body').scrollHeight - nodes.get('events-body').clientHeight;
  serve([event('log','tail event')]);
  await evaluate('tick()');
  assert.equal(nodes.get('events-body').scrollTop, nodes.get('events-body').scrollHeight, 'follow on scrolls to the tail');
  assert.equal(nodes.get('follow-label').style.display, 'inline-flex', 'follow remains available in event controls after completion');

  // Each plan/review keeps its own output across pages. The readable result
  // precedes raw replies and inputs, with nested disclosures retained on polls.
  const reportPhases = [
    {phase_id:'plan', owner:'planner', name:'plan'},
    {phase_id:'review', owner:'plan-reviewer', name:'review'},
    {phase_id:'replan', owner:'planner', name:'plan'},
    {phase_id:'accepted', owner:'plan-reviewer', name:'review'},
    {phase_id:'old', owner:'planner', name:'plan'},
    {phase_id:'bad', owner:'plan-reviewer', name:'review', status:'fail', error:'Invalid report\nsecond line'},
    {phase_id:'build', owner:'builder', name:'implement'},
  ].map((p,i) => ({seq:i+1,status:'success',error:'',started_at:T(i*5),ended_at:T(i*5+5),...p}));
  const report = {summary:'Initial <plan>',steps:['Edit <file>'],files:['a.go'],risks:['Check & verify']};
  evaluate('done = false');
  const page = [
    event('output',{attempt:0,report,text:'RAW PLAN <script>unsafe</script>'},'plan','planner'),
    event('input',{prompt:'PLAN INPUT'},'plan','planner'),
    event('output',{attempt:0,report:{summary:'Missing tests',feedback:['Add <tests>']},text:'RAW REVIEW'},'review','plan-reviewer'),
    event('envelope',{attempt:0,text:'MALFORMED <reply>',violations:['missing feedback']},'bad','plan-reviewer'),
    event('gate',{attempt:1,text:'PROTECTED PATH <reply>',violations:['protected path']},'bad','plan-reviewer'),
    event('output',{attempt:0,report:{summary:'Built <it>',artifacts:[{path:'a.go',note:null}]},text:'BUILT'},'build','builder'),
  ];
  while (page.length < 500) page.push(event('log','padding'));
  serve(page, reportPhases);
  await evaluate('tick()');
  const select = p => { evaluate(`selected = ''`); evaluate(`pick('${p}')`); return panel(); };
  let out = select('plan');
  for (const text of ['Initial &lt;plan>','<ol>','Edit &lt;file>','a.go','Check &amp; verify']) assert(out.includes(text), text);
  assert(out.indexOf('Initial &lt;plan>') < out.indexOf('Raw responses'));
  assert(out.indexOf('RAW PLAN') < out.indexOf('PLAN INPUT'));
  assert(!out.includes('<script>'), 'agent text must be escaped');
  assert(!out.includes('data-panel="plan-raw" open'), 'raw responses start collapsed');
  assert(!out.includes('data-panel="plan-inputs" open'), 'inputs start collapsed');
  assert(select('review').includes('Add &lt;tests>'));
  out = select('bad');
  for (const text of ['MALFORMED &lt;reply>','PROTECTED PATH &lt;reply>','Response 2 · rejected',
                      'Error · Invalid report</summary>','Invalid report\nsecond line']) assert(out.includes(text), text);
  out = select('build');
  for (const text of ['Built &lt;it>','<h4>artifacts</h4>','<h4>path</h4>','<p>null</p>']) assert(out.includes(text), text);
  assert(select('old').includes('Loading phase output…'), 'rows can arrive before their events');

  evaluate(`panels.set('plan-raw', true); panels.set('plan-inputs', true)`);
  serve([
    event('output',{attempt:0,report:{...report,summary:'Revised plan',files:['a.go','a_test.go']},text:'REVISED RAW'},'replan','planner'),
    event('output',{attempt:0,report:{summary:'Ready',feedback:[]},text:'ACCEPTED RAW'},'accepted','plan-reviewer'),
  ], reportPhases);
  evaluate(`selected = 'plan'`);
  await evaluate('tick()');
  out = panel();
  for (const text of ['Initial &lt;plan>','data-panel="plan-raw" open','data-panel="plan-inputs" open']) assert(out.includes(text), text);
  assert.equal(out.split('RAW PLAN').length-1,1,'raw output must not duplicate across pages');
  assert(select('replan').includes('a_test.go'), 'a later page updates the panels');
  assert(select('accepted').includes('Accepted · no objections.'));
  assert(select('old').includes('Structured output not recorded'), 'only drained runs say not recorded');
  assert.equal(evaluate('done'),true);

  // A context snapshot is independent of billed usage, including zero and
  // phases which ended before producing a billed response.
  evaluate(`contexts.set('old', {tokens:0,contextWindow:8000,percent:0})`);
  out = select('old');
  assert(out.includes('Usage not recorded.') && out.includes('Context (estimated): 0 tokens')
    && out.includes('8,000 window · 0.0% full'), out);
  assert(chart().includes('Context (estimated): 0 tokens / 8,000 window · 0.0% full'),
    'a block shows a zero snapshot even without billed usage');
  assert(!call('details', {...reportPhases.find(p => p.phase_id === 'old'), owner:'engineer'}, run, ms(200)).includes('Context (estimated)'),
    'a non-agent phase does not show context');
  assert(!call('block', {...reportPhases.find(p => p.phase_id === 'old'), owner:'engineer'}, {status:'success', start:0, end:1}).includes('Context (estimated)'),
    'a non-agent block tooltip agrees with its details');

  // Navigation while a request is in flight cannot contaminate the next run.
  evaluate('done = false'); response = null;
  const pending = evaluate('tick()');
  await evaluate('tick()'); // overlapping interval must not make another request
  evaluate('navigate()');
  assert.equal(observers[0].watching.length, 0, 'navigation stops watching the old chart');
  for (const name of ['usage', 'tails', 'panels', 'outputs', 'responses', 'spend', 'contexts']) assert.equal(evaluate(name + '.size'), 0, name);
  assert.equal(evaluate('selected'), '');
  // The dropped reply re-ticks for the new page; fail that fetch so the
  // ticks below drive this page by hand.
  failNext = 'superseded';
  resolveFetch(json({run,phases,events:[event('usage',{tokens:999}),event('log','late','p1','output'),
                                        event('output',{report:{summary:'late'}})]}));
  await pending;
  await new Promise(setImmediate);
  assert.equal(failNext, null, 'navigation during a fetch fetches the new page');
  assert.equal(evaluate('after'),0);
  for (const name of ['usage', 'tails', 'outputs', 'spend', 'contexts']) assert.equal(evaluate(name + '.size'), 0, `stale ${name}`);
  assert.equal(nodes.has('costs'), false, 'navigation removes the old cost panel');

  // A different run starts with no usage. Its total is authoritative even
  // before the backlog reaches any model responses.
  const priced = {...run, run_id:'priced', cost:0.9};
  context.location.hash = '#/runs/priced';
  evaluate('view = route()');
  const blankPage = Array.from({length:500}, () => event('log', 'padding'));
  serve(blankPage, phases, priced);
  await evaluate('tick()');
  assert.equal(scaffoldCount, 2, 'navigation makes a new run scaffold');
  assert(nodes.get('run-total').textContent === '$0.90' && costs().includes('Loading usage…'), costs());
  assert(!costs().includes('No usage recorded') && !costs().includes('unknown/model'),
    'neither an empty state nor the previous run leaks into a partial page');

  // Providers aggregate by name and models aggregate by bare name across
  // attempts and phases; zero-cost usage and missing provider/model are kept.
  const priceEvents = [
    event('usage',{attempt:0,seq:1,provider:'a/b',model:'c',cost:0.1},'p1'),
    event('usage',{attempt:0,seq:2,provider:'a',model:'b/c',cost:0.2},'p1'),
    event('usage',{attempt:1,seq:1,provider:'a/b',model:'c',cost:0.15},'p1'),
    event('usage',{attempt:1,seq:1,provider:'b',model:'shared',cost:0.25},'p2'),
    event('usage',{attempt:1,seq:2,provider:'z',model:'free',cost:0},'p2'),
    event('usage',{attempt:1,seq:3,provider:'<pi>',model:'<m>',cost:0},'p2'),
  ];
  while (priceEvents.length < 500) priceEvents.push(event('log','padding'));
  serve(priceEvents, phases, priced);
  await evaluate('tick()');
  const providerGroups = () => JSON.parse(JSON.stringify(evaluate('providerCosts()')));
  const providerCostOf = provider => providerGroups().find(p => p.provider === provider)?.cost;
  assert.equal(providerCostOf('a/b'), 0.25);
  assert.equal(providerCostOf('a'), 0.2);
  assert.equal(providerCostOf('b'), 0.25);
  assert.equal(providerCostOf('z'), 0, 'zero-cost provider usage is retained');
  assert.equal(providerCostOf('<pi>'), 0);
  assert.deepEqual(providerGroups().slice(0, 2).map(p => p.provider), ['a/b', 'b'],
    'equal-cost providers use the provider name as a tie-breaker');

  const costsHtml = costs();
  assert(costsHtml.includes('&lt;pi>') && costsHtml.includes('&lt;m>') && !costsHtml.includes('<pi>') && !costsHtml.includes('<m>'),
    'provider and model labels are escaped');
  assert(costsHtml.includes('Loading usage…'), 'partial rows stay marked as loading');
  const colouredRows = html => {
    const rows = html.match(/<li style="--model-color:[^"]+">[\s\S]*?<\/li>/g) || [];
    return new Map(rows.map((row, i) => [row.match(/<span class="model-name">([^<]*)<\/span>/)?.[1], row.match(/--model-color:([^"]+)"/)?.[1]]));
  };
  const beforeColours = colouredRows(costsHtml);
  assert([...beforeColours.values()].every(Boolean), 'every cost row has a coloured dot');
  assert(costsHtml.includes('class="model-dot" aria-hidden="true"'), 'dots are decorative');
  assert.equal(nodes.get('run-total').textContent, '$0.90');

  serve([
    event('usage',{attempt:2,seq:1,provider:'a',model:'b/c',cost:0.15},'p1'),
    event('usage',{attempt:2,seq:2,provider:'other',model:'shared',cost:0},'p2'),
    event('usage',{attempt:2,seq:3,model:'shared',cost:0},'p2'),
    event('usage',{attempt:2,seq:4,provider:'other',cost:0.03},'p2'),
    event('usage',{attempt:2,seq:5,cost:0.02},'p2'),
  ], phases, priced);
  await evaluate('tick()');
  assert.equal(evaluate('done'), true);

  assert.equal(providerCostOf('a'), 0.35);
  assert.equal(providerGroups()[0].provider, 'a', 'increased provider spend moves it to the top');
  assert.equal(providerCostOf('other'), 0.03);
  assert.equal(providerCostOf('unknown'), 0.02, 'missing provider usage is grouped under unknown');
  assert.equal(providerGroups().length, 7, 'different providers are separate rows');

  const spent = () => JSON.parse(JSON.stringify(evaluate('modelSpend()')));
  assert.equal(spent().find(m => m.model === 'b/c')?.cost, 0.35);
  assert.equal(spent().find(m => m.model === 'shared')?.cost, 0.25, 'shared spending is aggregated across providers');
  assert.equal(spent().find(m => m.model === 'unknown')?.cost, 0.05, 'unknown models are aggregated across providers');
  assert.deepEqual(spent().map(m => m.model), [...spent()].sort((a,b) => b.cost - a.cost ||
    (a.model < b.model ? -1 : a.model > b.model ? 1 : 0)).map(m => m.model));

  const finalCosts = costs();
  assert(!finalCosts.includes('Loading usage…') && !finalCosts.includes('No usage recorded.'), finalCosts);
  assert(finalCosts.indexOf('providers') < finalCosts.indexOf('models'), 'providers section precedes models section');
  assert(finalCosts.indexOf('>a</span>') < finalCosts.indexOf('>a/b</span>'), 'providers are sorted by spend');
  assert(finalCosts.indexOf('>b/c</span>') < finalCosts.indexOf('>unknown</span>', finalCosts.indexOf('models')),
    'models are sorted by spend');
  assert(!finalCosts.includes('a/b/c') && !finalCosts.includes('unknown/unknown'), 'model names are bare');
  for (const [identity, colour] of beforeColours) assert.equal(colouredRows(costs()).get(identity), colour, identity);
  const finished = finalCosts, finishedProviders = providerGroups();
  evaluate('draw()'); evaluate('draw()');
  assert.equal(costs(), finished, 'redraws do not double-count responses');
  assert.deepEqual(providerGroups(), finishedProviders);

  // A third run clears those identities and, once drained, explicitly says
  // no usage was recorded (rather than fabricating a balancing cost row).
  context.location.hash = '#/runs/empty';
  response = json({run:{...run, run_id:'empty', cost:0.5}, phases:[], events:[]});
  evaluate('navigate()');
  await new Promise(setImmediate); // navigate launches an async tick
  assert.equal(scaffoldCount, 3);
  assert(nodes.get('run-total').textContent === '$0.50' && costs().includes('No usage recorded.'), costs());
  assert(!costs().includes('unknown/') && !costs().includes('Loading usage…'), costs());
  assert.equal(evaluate('usage.size'), 0, 'navigating to another run resets the event cache');
  const liveEmpty = call('renderCosts', {...run, status:'running', cost:0});
  assert(liveEmpty.includes('No usage recorded yet.'), liveEmpty);
  for (const count of [3, 4, 5]) {
    const rows = Math.min(count, 4);
    evaluate(`usage.clear(); pagers.clear(); for (let i = 0; i < ${count}; i++) usage.set('p' + i, [{provider: 'p' + i, model: 'm', cost: 0.1}])`);
    let html = call('renderCosts', run);
    assert(html.includes(`data-cost-list="providers" style="--rows:${rows}"`), `${count} providers size to ${rows} rows`);
    // A provider body sized to its --rows of 21px rows, as the CSS lays it out.
    const row = Object.assign(new Element(), {getBoundingClientRect: () => ({height: 21})});
    const pager = Object.assign(new Element(), {hidden: true});
    const body = Object.assign(new Element(), {clientHeight: rows * 21, clientWidth: 400, dataset: {pageBody: 'providers'},
      querySelector: () => row, parentElement: Object.assign(new Element(), {querySelector: () => pager})});
    nodes.get('app').querySelectorAll = sel => sel === '[data-page-body]' ? [body] : [];
    evaluate('fitPages()');
    nodes.get('app').querySelectorAll = () => [];
    html = call('renderCosts', run);
    const providersHtml = html.slice(html.indexOf('data-cost-list="providers"'), html.indexOf('data-cost-list="models"'));
    const shownRows = (providersHtml.match(/<li /g) || []).length;
    const pagerHidden = /data-pager="providers" hidden/.test(providersHtml);
    assert(shownRows === rows && pagerHidden === (count <= rows), `${count} providers show ${rows} rows`);
    if (count > rows) assert(providersHtml.includes(`1–${rows} of ${count}`), `${count} providers page ${rows} at a time`);
  }
  evaluate('usage.clear(); pagers.clear()');

  // Runs: the summary says displayed versus total, and says whose numbers those are.
  context.location.hash = '#/runs';
  evaluate('view = route(); generation++; done = false');
  response = json([{...run, run_id:'r1'}], {'X-Total-Runs': '1284'});
  await evaluate('tick()');
  const runsBar = nodes.get('status').innerHTML;
  assert(runsBar.includes('showing 1 of 1,284 runs'), runsBar);
  assert(runsBar.includes('displayed:'), runsBar);
  assert(pageHTML().includes("location.hash='/runs/r1'"));
  assert(pageHTML().includes('>1m05s</td>'), 'the list shows how long a run took');
  assert(!pageHTML().includes('10s queued'), 'the list leaves out the wait');

  // A queued run is a live run, not a failed one.
  evaluate('generation++');
  response = json([{...run, run_id:'r2', status:'queued', started_at:'', ended_at:''}], {'X-Total-Runs': '1'});
  await evaluate('tick()');
  const queuedRow = pageHTML();
  assert(queuedRow.includes('class="running">queued'), queuedRow);
  assert(!queuedRow.includes('s queued'), 'no wait in the list');

  // Projects list is complete, sorted by the API, and uses full paths for links.
  context.location.hash = '#/projects';
  evaluate('view = route(); generation++');
  response = json([{repo:'/one/alpha',runs:3,tokens:300,cost:3,share:0.75},
                   {repo:'/two/alpha',runs:2,tokens:100,cost:1,share:0.25},
                   {repo:'/free',runs:2,tokens:10,cost:0,share:0},
                   {repo:'/no-runs',runs:0,tokens:0,cost:0,share:0}]);
  await evaluate('tick()');
  const projects = pageHTML();
  assert(projects.includes('4 runs') === false, projects);
  assert(projects.includes('4</b><span>projects recorded'), projects);
  const projectTableHTML = nodes.get('list-body').innerHTML;
  assert(projectTableHTML.includes('<th class="num">Total Spend</th><th class="num">Avg / PR</th>'), projectTableHTML);
  assert.equal((projectTableHTML.match(/<th[\s>]/g) || []).length, 7);
  const projectRowHTML = repo => projectTableHTML.match(new RegExp(`<tr class="project" data-row="${repo}"[^>]*>[\\s\\S]*?<\\/tr>`))?.[0];
  assert(projectRowHTML('/one/alpha').includes('<td class="num cost">$3.00</td>\n    <td class="num cost">$1.00</td>'));
  assert(projectRowHTML('/two/alpha').includes('<td class="num cost">$1.00</td>\n    <td class="num cost">$0.50</td>'));
  assert(projectRowHTML('/free').includes('<td class="num cost">$0.00</td>\n    <td class="num cost">$0.00</td>'), 'free runs remain in the denominator');
  assert(projectRowHTML('/no-runs').includes('<td class="num cost">$0.00</td>\n    <td class="num cost">$0.00</td>'), 'no runs have a zero average');
  assert(projects.includes('#/projects/detail?repo=%2Fone%2Falpha'), projects);
  assert(projects.includes('#/projects/detail?repo=%2Ftwo%2Falpha'), projects);
  assert(projects.indexOf('$3.00') < projects.indexOf('$1.00'), projects);
  response = json([]);
  await evaluate('tick()');
  assert(nodes.get('list-body').innerHTML.includes('<td colspan="7" class="dim">No projects recorded yet.</td>'));

  // One project shows lifetime totals and pages its own runs into run links.
  context.location.hash = '#/projects/detail?repo=%2Fone%2Falpha';
  evaluate('view = route(); generation++; projectRows = []; projectLoaded = false; projectMore = false');
  const first = {...run, run_id:'r3', repo:'/one/alpha'};
  const second = {...run, run_id:'r2', repo:'/one/alpha'};
  const third = {...run, run_id:'r1', repo:'/one/alpha'};
  routeResponses = new Map([
    ['/api/projects/detail?repo=%2Fone%2Falpha', json({repo:'/one/alpha',runs:3,tokens:300,cost:3.5,share:0.75})],
    ['/api/projects/runs?repo=%2Fone%2Falpha&n=50', json({runs:[first,second],more:true})],
    ['/api/projects/runs?repo=%2Fone%2Falpha&n=50&before_at=2026-09-20T11%3A59%3A50Z&before_id=r2',
      json({runs:[third],more:false})],
  ]);
  await evaluate('tick()');
  const head = nodes.get('project-head').innerHTML;
  assert(appHTML.includes('id="project-head" class="card page-summary project-summary"'));
  assert(head.includes('300</b><span>tokens') && head.includes('$3.50'), head);
  assert(head.indexOf('runs recorded') < head.indexOf('tokens') && head.indexOf('tokens') < head.indexOf('avg spend per PR')
    && head.indexOf('avg spend per PR') < head.indexOf('recorded spend'), 'project metrics keep their reading order');
  assert(head.includes('<div class="cost"><b>$1.17</b><span>avg spend per PR</span></div>'), head);
  assert(head.includes('Lifetime metrics for this repository. 75.0% of total recorded spend.'));
  assert(head.includes('Full path') && head.includes('/one/alpha'), 'repository identity stays in the summary');
  let project = nodes.get('project-runs').innerHTML;
  assert(project.includes("location.hash='/runs/r3'") && project.includes('Load more runs'), project);
  assert(nodes.get('status').innerHTML.includes('showing 2 of 3 runs'));
  await evaluate('moreProjectRuns()');
  project = nodes.get('project-runs').innerHTML;
  assert(project.includes("location.hash='/runs/r1'") && !project.includes('Load more runs'), project);
  assert(nodes.get('status').innerHTML.includes('showing 3 of 3 runs'));
  assert(nodes.get('project-head').innerHTML.includes('$1.17'), 'loading more run rows does not change the lifetime average');
  routeResponses.set('/api/projects/detail?repo=%2Fone%2Falpha',
    json({repo:'/one/alpha',runs:4,tokens:330,cost:3.6,share:0.75}));
  await evaluate('tick()');
  assert(nodes.get('project-head').innerHTML.includes('<b>$0.90</b><span>avg spend per PR</span>'),
    'polling updates from summary totals, not loaded runs');
  failNext = 'database is locked';
  await evaluate('tick()');
  assert(nodes.get('project-head').innerHTML.includes('<b>$0.90</b><span>avg spend per PR</span>'),
    'a failed project refresh retains the last average');
  routeResponses = null;

  // Overview: lifetime totals, all four rankings, and the attribution caveats.
  context.location.hash = '#/overview';
  evaluate('view = route(); generation++');
  const overviewData = {
    runs: 1284, tokens: 9_000_000, cost: 42.18374,
    top_projects: [{repo:'/repos/alpha', runs:900, cost:38, share:0.9},
                   {repo:'', runs:100, cost:4, share:0.1}],
    top_runs: [{...run, run_id:'top', cost: 9.5}, {...run, run_id:'long', request:'x'.repeat(149) + '😀tail'}],
    top_models: [{provider:'moonshotai', model:'kimi', cost:30, share:0.7113, phases:2700},
                 {provider:'', model:'', cost:2, share:0.0474, phases:12}],
    top_providers: [{provider:'moonshotai', cost:32, share:0.7586, phases:2712},
                    {provider:'', cost:2, share:0.0474, phases:12}],
    at: '2026-09-20T12:00:00Z',
  };
  response = json(overviewData);
  await evaluate('tick()');
  const over = pageHTML();
  assert.equal(nodes.get('average-spend').textContent, '$0.03', 'global lifetime totals determine the fractional average');
  assert(appHTML.includes('<div class="ranking-column"><section class="card average-card"><h2>average spend per PR</h2>'));
  assert(appHTML.includes('id="average-spend" class="cost"'), 'average uses the purple cost style');
  assert(appHTML.indexOf('id="average-spend"') < appHTML.indexOf('id="rank-projects"')
    && appHTML.indexOf('id="rank-projects"') < appHTML.indexOf('id="rank-runs"')
    && appHTML.indexOf('id="rank-runs"') < appHTML.indexOf('id="rank-models"')
    && appHTML.indexOf('id="rank-models"') < appHTML.indexOf('id="rank-providers"'),
    'overview left and right columns retain ranking order');
  assert(appHTML.includes('</div><div class="ranking-column">'), 'models and providers occupy an explicit second column');
  assert(!nodes.get('rank-projects').innerHTML.includes('Avg / PR'), 'overview rankings do not gain an average column');
  for (const text of ['1,284','$42.18','$9.50','alpha','$38.00','90.0%','900 runs',
                      'moonshotai/kimi','71.1%','unknown/unknown',
                      '2,700 phases','counted per phase',"location.hash='/runs/top'"]) {
    assert(over.includes(text), `overview: ${text}`);
  }
  assert(over.includes('>' + 'x'.repeat(149) + '😀…</span></td>'), 'a long request is cut to 150 characters');
  assert(over.includes('>fix</span></td>'), 'a short request is not cut');
  assert(nodes.get('status').innerHTML.includes('all repositories · all time'));
  const projectsH2 = over.indexOf('top projects by spend');
  const modelsH2 = over.indexOf('top models by spend');
  const providersH2 = over.indexOf('top providers by spend');
  const runsH2 = over.indexOf('top runs by spend');
  assert(projectsH2 >= 0 && modelsH2 >= 0 && providersH2 >= 0 && runsH2 >= 0, 'all four ranking headings present');
  assert(projectsH2 < runsH2 && runsH2 < modelsH2 && modelsH2 < providersH2,
    'projects and runs stack in the first column, models and providers in the second');
  const provCard = over.slice(providersH2);
  for (const text of ['$32.00', '75.9%', '2,712 phases']) assert(provCard.includes(text), `provider card: ${text}`);
  assert(provCard.includes('>unknown<'), 'empty provider falls back to unknown');
  assert(provCard.indexOf('moonshotai') < provCard.indexOf('>unknown<'), 'providers are sorted by spend descending');
  assert(over.includes('#/projects/detail?repo=%2Frepos%2Falpha'), 'overview project links to its detail');
  const dot = c => `<span class="dot-label"><span class="model-dot" aria-hidden="true" style="--model-color:${c}"></span>`;
  const modelsHTML = nodes.get('rank-models').innerHTML, providersHTML = nodes.get('rank-providers').innerHTML;
  assert(modelsHTML.includes(dot(call('modelColour', 'kimi')) + 'moonshotai/kimi</span>'), 'model row has a coloured dot');
  assert(modelsHTML.includes(dot('#7A9EFB') + 'unknown/unknown</span>'), 'unknown model dot falls back');
  assert(providersHTML.includes(dot('#1C81FA') + 'moonshotai</span>'), 'provider row has its full hue dot');
  assert(providersHTML.includes(dot('#7A9EFB') + 'unknown</span>'), 'unknown provider dot falls back');

  response = json({...overviewData, runs: 3, cost: 12.34567});
  await evaluate('tick()');
  assert.equal(nodes.get('average-spend').textContent, '$4.12', 'polling replaces the global average');
  failNext = 'database is locked';
  await evaluate('tick()');
  assert.equal(nodes.get('average-spend').textContent, '$4.12', 'failed polling keeps the last good average');
  assert.equal(nodes.get('note').className, 'stale');
  response = json({...overviewData, runs: 4, cost: 0});
  await evaluate('tick()');
  assert.equal(nodes.get('average-spend').textContent, '$0.00', 'zero-cost runs show a zero average');

  // Empty database: zero totals and empty tables, never a blank page.
  evaluate('generation++');
  response = json({runs:0, tokens:0, cost:0, top_projects:[], top_runs:[], top_models:[], top_providers:[], at:''});
  await evaluate('tick()');
  const empty = pageHTML();
  assert.equal(nodes.get('average-spend').textContent, '$0.00', 'zero runs show a zero average');
  assert(empty.includes('$0.00') && empty.includes('No runs recorded yet.')
         && empty.includes('No model spend recorded yet.')
         && empty.includes('No provider spend recorded yet.')
         && empty.includes('No project spend recorded yet.'), empty);

  // A failed refresh keeps the last good values and says they are stale.
  failNext = 'database is locked';
  await evaluate('tick()');
  assert.equal(nodes.get('average-spend').textContent, '$0.00', 'the zero-run average survives a failed poll');
  assert(pageHTML().includes('$0.00'), 'values must survive a failed poll');
  assert.equal(nodes.get('note').className, 'stale');
  assert(nodes.get('note').textContent.includes('database is locked'));

  // An unknown route is a page, not a run id.
  context.location.hash = '#/nope';
  evaluate('view = route(); generation++');
  await evaluate('tick()');
  assert(pageHTML().includes('not found'));

  // Agents: a list linking every row, read once rather than polled.
  const navOn = id => nodes.get(id).attrs.get('aria-current') === 'page';
  const agentRows = [
    {name:'planner', kind:'agent', repo_instructions:true, workflows:['plan','implement','build','revise'], prompts:[
      {role:'system', key:'prompts/planner/system.md', shared_with:[]}, {role:'user', key:'prompts/planner/user.md', shared_with:[]}]},
    {name:'code-review-general', kind:'agent', repo_instructions:true, workflows:['implement','build','revise'], prompts:[
      {role:'system', key:'prompts/code-review-general/system.md', shared_with:[]},
      {role:'user', key:'prompts/code-review/user.md', shared_with:['code-review-security','code-review-slop']}]},
    {name:'route-plan', kind:'routing', routes:'planner', repo_instructions:false, workflows:['plan','implement','build','revise'], prompts:[
      {role:'instructions', key:'prompts/routing/planner.md', shared_with:[]}]},
    {name:"o'<b>", kind:'agent', repo_instructions:false, workflows:[], prompts:[]},
  ];
  routeResponses = new Map([['/api/agents', json(agentRows)]]);
  context.location.hash = '#/agents';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(navOn('nav-agents') && !navOn('nav-projects'), 'Agents is selected on its list');
  const agentList = nodes.get('list-body').innerHTML;
  assert(agentList.includes('<th>Agent</th><th>Kind</th><th>Workflows</th><th>Prompts</th>'), agentList);
  assert(agentList.includes(`<tr class="agent" data-row="code-review-general" onclick="location.hash='#/agents/code-review-general'">`), agentList);
  assert(agentList.includes('<a data-focus="agent-route-plan" href="#/agents/route-plan">route-plan</a>'), agentList);
  assert(agentList.includes('<td>system · user</td>'), 'an unshared prompt is unmarked');
  assert(agentList.includes('system · user <span class="dim" title="Shared with code-review-security, code-review-slop">shared</span>'), agentList);
  assert(agentList.includes('<td>instructions</td>') && agentList.includes('<td class="dim">routing</td>'), agentList);
  assert(agentList.includes('<td>plan, implement, build, revise</td>'), agentList);
  assert(agentList.includes('href="#/agents/o%27%3Cb%3E"') && agentList.includes('>o&#39;&lt;b></a>') && !agentList.includes('<b>'),
    'agent names are escaped and encoded');
  assert(pageHTML().includes('3</b><span>agents') && pageHTML().includes('1</b><span>routing phases'));
  assert.equal(evaluate('done'), true, 'the list is not polled');
  assert.equal(nodes.get('note').textContent, 'loaded · reload to refresh');

  // An agent page: one read-only box per prompt, bodies as text, never markup.
  routeResponses.set('/api/agents/code-review-general', json({...agentRows[1], prompts:[
    {...agentRows[1].prompts[0], body:'Review </textarea><script>alert(1)</script> & report'},
    {...agentRows[1].prompts[1], body:'{{request}}\n\nline 3'}]}));
  context.location.hash = '#/agents/code-review-general';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(navOn('nav-agents') && !navOn('nav-runs'), 'Agents stays selected on an agent page');
  const agentHead = nodes.get('agent-head').innerHTML;
  assert(agentHead.includes('<h2>code-review-general</h2>') && agentHead.includes('Agent · workflows: implement, build, revise'), agentHead);
  const promptsHTML = () => deepHTML(nodes.get('agent-prompts'));
  let boxes = promptsHTML();
  assert.equal((boxes.match(/<textarea /g) || []).length, 2, boxes);
  assert(!boxes.includes('readonly'), 'prompt boxes are editable');
  assert(boxes.includes('<label for="prompt-system">System prompt</label>') && boxes.includes('<label for="prompt-user">User prompt</label>'), boxes);
  assert(boxes.includes('&lt;/textarea>&lt;script>alert(1)&lt;/script> &amp; report</textarea>') && !boxes.includes('<script>'),
    'a prompt body is escaped');
  assert(/rows="4"[^>]*>\n\{\{request\}\}\n\nline 3<\/textarea>/.test(boxes), 'boxes size to their lines');
  assert(boxes.includes('Built-in <code>prompts/code-review/user.md</code>'), boxes);
  assert(boxes.includes('Shared with <a href="#/agents/code-review-security">code-review-security</a>, <a href="#/agents/code-review-slop">code-review-slop</a>'), boxes);
  assert.equal(boxes.split('Shared with').length - 1, 1, 'only the shared prompt says so');
  assert.equal(boxes.split('is replaced by the request.').length - 1, 1, 'only the user box has the request hint');
  assert(boxes.includes('The repo’s AGENTS.md is prepended on the first turn.'), boxes);
  assert(!call('promptInfo', {role:'user', key:'k', body:'x', shared_with:[]}, false).includes('AGENTS.md'),
    'no AGENTS.md note without repo instructions');
  assert.equal(nodes.get('status').textContent, 'Agents · code-review-general · agent');
  assert.equal(evaluate('done'), true, 'an agent page is not polled');

  // A routing phase has one instructions box and names the agent it routes.
  routeResponses.set('/api/agents/route-plan', json({...agentRows[2], prompts:[{...agentRows[2].prompts[0], body:'Pick a tier.'}]}));
  context.location.hash = '#/agents/route-plan';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(nodes.get('agent-head').innerHTML.includes('Routing phase for <a href="#/agents/planner">planner</a>'));
  boxes = promptsHTML();
  assert(boxes.includes('<label for="prompt-instructions">Instructions</label>') && boxes.includes('Pick a tier.'), boxes);
  assert(!boxes.includes('{{request}}') && !boxes.includes('Shared with'), boxes);

  // An unknown agent is a not-found page; a broken roster is a failure.
  routeResponses.set('/api/agents/nope', {ok:false, status:404, text: async () => 'no such agent\n'});
  context.location.hash = '#/agents/nope';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(appHTML.includes('No agent named <code>nope</code>') && navOn('nav-agents'), appHTML);
  assert(evaluate('done') && nodes.get('note').className === '', 'not found is final, not stale');
  routeResponses.set('/api/agents/broken', {ok:false, status:500, text: async () => 'roster is broken'});
  context.location.hash = '#/agents/broken';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(nodes.get('note').className === 'stale' && nodes.get('note').textContent.includes('roster is broken'));
  assert.equal(nodes.get('status').textContent, '');
  // A failed first load is retried by the shared timer until it succeeds.
  routeResponses.set('/api/agents/broken', json({...agentRows[0], name:'broken', prompts:[]}));
  evaluate('last = 0');
  intervals.forEach(f => f());
  await new Promise(setImmediate);
  assert(nodes.get('agent-head').innerHTML.includes('<h2>broken</h2>'));
  assert.equal(evaluate('done'), true);
  assert.equal(nodes.get('note').textContent, 'loaded · reload to refresh');

  // A reply that lands after navigating away is dropped, and the page it was
  // blocking fetches without a manual tick.
  response = null;
  context.location.hash = '#/agents/slow';
  evaluate('navigate()');
  context.location.hash = '#/agents';
  evaluate('navigate()');
  resolveFetch(json({...agentRows[0], name:'slow', prompts:[]}));
  await new Promise(setImmediate);
  assert(!nodes.has('agent-head') && nodes.has('list-body'), 'a late agent reply is not drawn on the list');
  assert(nodes.get('list-body').innerHTML.includes('route-plan'));
  assert.equal(evaluate('done'), true);

  // Prompt overrides: the list marks edited agents.
  const dataRoot = '/home/me/.local/share/lathe/';
  routeResponses.set('/api/agents', json(agentRows.map((a, i) => ({...a, prompts: a.prompts.map(p => ({...p, overridden: i === 1 && p.role === 'system'}))}))));
  context.location.hash = '#/agents';
  evaluate('navigate()');
  await new Promise(setImmediate);
  const editedList = nodes.get('list-body').innerHTML;
  assert(editedList.includes('>code-review-general</a> <span class="badge" title="A prompt override is active">edited</span></td>'), editedList);
  assert.equal(editedList.split('class="badge"').length - 1, 1, 'only an agent with an override is marked');

  // An agent page edits each prompt in place and shows where its override lives.
  const reviewers = ['code-review-security', 'code-review-slop', 'code-review-adversarial'];
  const prompt = (role, body, more) => ({role, key: `prompts/${role === 'user' ? 'code-review' : 'code-review-general'}/${role}.md`,
    shared_with: role === 'user' ? reviewers : [], body, builtin: body, hash: 'h-' + role, overridden: false,
    override_path: `${dataRoot}prompts/${role === 'user' ? 'code-review' : 'code-review-general'}/${role}.md`, ...more});
  const reviewer = prompts => ({...agentRows[1], prompts});
  routeResponses.set('/api/agents/code-review-general', json(reviewer([
    prompt('system', 'Review </textarea><script>alert(1)</script>', {builtin: 'Review', overridden: true}),
    prompt('user', '{{request}}')])));
  context.location.hash = '#/agents/code-review-general';
  evaluate('navigate()');
  await new Promise(setImmediate);
  assert(nodes.get('agent-head').innerHTML.includes('Edits are saved as prompt overrides on this machine. Each run captures its prompts when it is submitted.'));
  const boxHTML = role => ['edit', 'status', 'info'].map(part => deepHTML(nodes.get(`prompt-${role}-${part}`))).join('');
  const statusOf = role => nodes.get(`prompt-${role}-status`);
  assert(boxHTML('system').includes(`Override: <code>${dataRoot}prompts/code-review-general/system.md</code> (active)`), boxHTML('system'));
  assert(boxHTML('user').includes(`Override: <code>${dataRoot}prompts/code-review/user.md</code> (not set, using built-in <code>prompts/code-review/user.md</code>)`), boxHTML('user'));
  assert(boxHTML('system').includes('&lt;/textarea>&lt;script>alert(1)&lt;/script></textarea>') && !promptsHTML().includes('<script>'),
    'an editable body is escaped');
  assert(boxHTML('system').includes('>Reset to built-in</button>') && !boxHTML('user').includes('Reset to built-in'),
    'only an active override can be reset');
  const box = role => nodes.get('prompt-' + role);
  const disabled = role => box(role + '-save').disabled && box(role + '-discard').disabled;
  assert.equal(box('system').value, 'Review </textarea><script>alert(1)</script>');
  assert(disabled('system') && disabled('user'), 'nothing to save until a box changes');

  // Editing enables Save and Discard; typing the loaded text back disables them.
  const type = (role, value) => { box(role).value = value; call('promptInput', {dataset: {role}, value}); };
  type('system', '  Review harder\n');
  assert(!box('system-save').disabled && !box('system-discard').disabled && disabled('user'));
  type('system', 'Review </textarea><script>alert(1)</script>');
  assert(disabled('system'), 'the loaded text is not an edit');
  type('system', 'Review harder');
  context.document.activeElement = Object.assign(box('system-discard'), {dataset: {role: 'system'}});
  call('discardPrompt', 'system');
  assert(disabled('system') && box('system').value === 'Review </textarea><script>alert(1)</script>', 'Discard restores the loaded text');
  assert.deepEqual({...box('system').focused}, {preventScroll: true}, 'focus moves from the disabled Discard to the box');
  context.document.activeElement = null;

  // Leaving with unsaved edits asks first; declining keeps the page and its draft.
  // Declining a link steps back over its new entry; declining Back goes
  // forward again, so the entry it left is still there.
  type('user', '{{request}} carefully');
  confirmAnswer = false;
  const at = context.history.state.at;
  context.history.state = null;
  context.location.hash = '#/agents';
  listeners.get('hashchange')();
  assert.equal(confirms.pop(), 'Leave this page? Unsaved prompt edits will be lost.');
  assert.equal(context.history.went, -1, 'a declined link steps back');
  context.history.state = {at: at - 1};
  listeners.get('hashchange')();
  confirms.pop();
  assert.equal(context.history.went, 1, 'a declined Back goes forward again');
  // Arriving back on the page's own entry is not a navigation.
  context.history.state = {at};
  context.location.hash = '#/agents/code-review-general';
  listeners.get('hashchange')();
  assert.equal(confirms.length, 0, 'coming back asks nothing');
  assert(nodes.has('prompt-user') && box('user').value === '{{request}} carefully' && evaluate('view.page') === 'agent');
  const unload = {preventDefault() { this.prevented = true; }};
  listeners.get('beforeunload')(unload);
  assert(unload.prevented && unload.returnValue === '', 'closing the tab with a draft asks first');
  confirmAnswer = true;

  // Writes go to the agent's prompt route as same-origin JSON.
  const writes = [];
  let writeReply;
  const readFetch = context.fetch;
  context.fetch = async (url, init) => {
    if (!init) return readFetch(url);
    writes.push({url, method: init.method, headers: {...init.headers}, body: init.body});
    return writeReply;
  };
  const reply = (status, body) => ({ok: status < 300, status, text: async () => JSON.stringify(body)});
  const saved = 'Saved. Applies to runs and revisions submitted from now on.';

  // Save sends the text verbatim with the hash it was loaded with, and redraws
  // from the reply without touching another box's draft. The status line and
  // the other box's textarea are the same elements afterwards, and focus on the
  // disabled Save moves to the box.
  type('system', '  Review harder\n');
  const userBox = box('user'), live = statusOf('system');
  context.document.activeElement = Object.assign(box('system-save'), {dataset: {role: 'system'}});
  writeReply = reply(200, reviewer([prompt('system', '  Review harder\n', {builtin: 'Review', hash: 'h-saved', overridden: true}), prompt('user', '{{request}}')]));
  await call('savePrompt', 'system');
  assert.deepEqual({...box('system').focused}, {preventScroll: true}, 'focus moves from the disabled Save to the box');
  context.document.activeElement = null;
  assert(box('user') === userBox && statusOf('system') === live, 'a save rebuilds neither another draft nor the status line');
  assert.deepEqual(writes.pop(), {url: '/api/agents/code-review-general/prompts/system', method: 'PUT',
    headers: {'Content-Type': 'application/json'}, body: JSON.stringify({body: '  Review harder\n', base_hash: 'h-system'})});
  assert.equal(confirms.length, 0, 'an unshared prompt saves without asking');
  assert(statusOf('system').innerHTML === saved && statusOf('system').className === '' && disabled('system'), boxHTML('system'));
  assert(box('user').value === '{{request}} carefully' && !box('user-save').disabled, 'a save keeps other drafts');

  // A shared prompt names every agent it changes; cancelling sends nothing.
  confirmAnswer = false;
  await call('savePrompt', 'user');
  assert.equal(confirms.pop(), 'Saving changes this prompt for code-review-general, code-review-security, code-review-slop and code-review-adversarial.');
  assert.equal(writes.length, 0, 'a cancelled save sends nothing');
  confirmAnswer = true;

  // A conflict offers the current version and keeps the draft until asked.
  writeReply = reply(409, {error: 'this prompt changed since you loaded it', body: '{{request}} from <elsewhere>', hash: 'h-other', overridden: true});
  await call('savePrompt', 'user');
  confirms.length = 0;
  assert.equal(JSON.parse(writes.pop().body).base_hash, 'h-user');
  assert.equal(statusOf('user').innerHTML, 'this prompt changed since you loaded it <button type="button" data-role="user" data-focus="prompt-user-load" onclick="loadCurrent(this.dataset.role)">Load current version</button>');
  assert.equal(statusOf('user').className, 'fail');
  assert(box('user').value === '{{request}} carefully' && !box('user-save').disabled, 'a conflict keeps the draft');
  call('loadCurrent', 'user');
  assert(box('user').value === '{{request}} from <elsewhere>' && disabled('user'), 'loading the current version replaces the text');
  assert(boxHTML('user').includes('{{request}} from &lt;elsewhere></textarea>') && !boxHTML('user').includes('Load current version'));
  assert(boxHTML('user').includes('(active)'), 'a current version other than the built-in is an override');

  // A refusal shows the server's reason; the next save uses the loaded hash.
  type('user', 'no placeholder');
  writeReply = {ok: false, status: 400, text: async () => JSON.stringify({error: 'a user prompt has to contain {{request}}'})};
  await call('savePrompt', 'user');
  confirms.length = 0;
  assert.equal(JSON.parse(writes.pop().body).base_hash, 'h-other');
  assert(statusOf('user').innerHTML === 'a user prompt has to contain {{request}}' && statusOf('user').className === 'fail', boxHTML('user'));
  writeReply = {ok: false, status: 403, text: async () => 'forbidden <origin>\n'};
  await call('savePrompt', 'user');
  confirms.length = 0;
  writes.pop();
  assert(boxHTML('user').includes('forbidden &lt;origin>'), 'a plain-text refusal is shown as text');

  // Reset asks before dropping a draft in its own box.
  type('system', 'half-written');
  confirmAnswer = false;
  await call('resetPrompt', 'system');
  assert.equal(confirms.pop(), 'Reset to the built-in? Your unsaved edits to this prompt will be lost.');
  assert(writes.length === 0 && box('system').value === 'half-written', 'a declined reset keeps the draft');
  confirmAnswer = true;

  // Reset deletes the override and redraws on the built-in.
  writeReply = reply(200, reviewer([prompt('system', 'Review', {hash: 'h-builtin'}), prompt('user', '{{request}}')]));
  await call('resetPrompt', 'system');
  confirms.length = 0;
  assert.deepEqual(writes.pop(), {url: '/api/agents/code-review-general/prompts/system', method: 'DELETE',
    headers: {'Content-Type': 'application/json'}, body: '{}'});
  assert(boxHTML('system').includes('Reset to the built-in. Applies to runs and revisions submitted from now on.'), boxHTML('system'));
  assert(boxHTML('system').includes('(not set, using built-in') && !boxHTML('system').includes('>Reset to built-in</button>'));
  assert(box('system').value === 'Review' && box('user').value === 'no placeholder', 'a reset keeps other drafts');
  context.fetch = readFetch;

  // Confirming the leave navigates and drops the draft.
  context.location.hash = '#/agents';
  listeners.get('hashchange')();
  await new Promise(setImmediate);
  assert.equal(confirms.pop(), 'Leave this page? Unsaved prompt edits will be lost.');
  assert(evaluate('view.page') === 'agents' && nodes.has('list-body') && !evaluate('unsaved()'));
  const clean = {preventDefault() { this.prevented = true; }};
  listeners.get('beforeunload')(clean);
  assert(!clean.prevented, 'nothing to lose, nothing to ask');
  routeResponses = null;

  console.log('dashboard client checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
