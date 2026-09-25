// Run the embedded client against a small DOM surface and paginated API.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = new Map();
class Element {
  constructor() {
    this.innerHTML = ''; this.children = []; this.attrs = new Map();
    this.textContent = ''; this.className = ''; this.style = {}; this.dataset = {};
    this.scrollTop = 0; this.clientHeight = 100; this.scrollHeight = 1000;
  }
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
                  'nav-overview', 'nav-runs', 'nav-projects', 'follow-box', 'follow-label',
                  'project-head', 'project-runs']) nodes.set(id, new Element());
// Parse only the scaffold IDs when app is replaced. In particular, do not
// precreate #log: that would skip the first-render branch in detail().
const scaffoldIDs = ['project-title', 'head', 'run-cancel', 'phases', 'phase', 'costs', 'summary',
                     'iterations', 'revise', 'revise-request', 'revise-submit', 'revise-status', 'log'];
const appNode = nodes.get('app');
let appHTML = '', scaffoldCount = 0;
Object.defineProperty(appNode, 'innerHTML', {
  get: () => appHTML,
  set: html => {
    appHTML = html;
    for (const id of scaffoldIDs) nodes.delete(id);
    const ids = [...html.matchAll(/id="([^"]+)"/g)].map(match => match[1]);
    for (const id of ids) {
      assert(scaffoldIDs.includes(id), `unregistered app ID ${id}`);
      nodes.set(id, new Element());
    }
    if (ids.includes('log')) {
      scaffoldCount++;
      assert.deepEqual(ids, scaffoldIDs, 'all run regions come from the initial scaffold');
    }
  },
});
let response, resolveFetch, failNext = null, frames = [];
let routeResponses = null;
const observers = [];
const context = vm.createContext({
  document: {
    getElementById: id => nodes.get(id) ?? null, createElement: () => new Element(),
    body: {scrollHeight: 1000}, activeElement: null,
  },
  location: {hash: '#run'}, addEventListener() {}, setInterval() {}, Date, URLSearchParams,
  innerHeight: 100, scrollY: 0, scrollTo() { throw Error('must not scroll a reader away'); },
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
const script = fs.readFileSync('index.html', 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const bootstrap = script.lastIndexOf('navigate();');
assert(bootstrap > 0, 'bootstrap not found');
vm.runInContext(script.slice(0, bootstrap), context);
const evaluate = code => vm.runInContext(code, context);
const call = (fn, ...args) => { context.args = args; return evaluate(`${fn}(...args)`); };

// Routing: the agreed default, the explicit routes, the legacy deep link, and
// an unknown hash that must not be mistaken for a run id.
for (const [hash, want] of [
  ['', 'overview'], ['#/', 'overview'], ['#/overview', 'overview'], ['#/runs', 'runs'],
  ['#/projects', 'projects'], ['#/projects/detail?repo=%2Frepos%2Falpha', 'project'],
  ['#/runs/20260920T120000Z_build_1', 'run'], ['#20260920T120000Z_build_1', 'run'],
  ['#/nope', 'missing'],
]) {
  context.location.hash = hash;
  const r = evaluate('route()');
  assert.equal(r.page, want, `${hash} -> ${r.page}`);
  if (want === 'run') assert.equal(r.id, '20260920T120000Z_build_1', hash);
  if (want === 'project') assert.equal(r.repo, '/repos/alpha', hash);
}

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
assert(html.includes('<span class="piece" data-line="0">✅</span>'), 'the status mark leads the label');
assert(!html.includes('data-line="0">plan<'), 'the row names the phase, so the block does not');
assert(!html.includes('data-line="1">success'), 'the status word is only in the accessible name');
assert(html.includes('data-lines="1"'), 'a label with nothing under the status sits on one centred line');
assert(html.includes('class="block ok" style="--hue:var(--dim);'), 'a phase with no model fills dim, border the status');
assert.equal(call('modelColour', 'claude-fable-5.1'), 'color-mix(in srgb, #D57355 100%, var(--card))', 'dotted names match');
assert.equal(call('modelColour', 'kimi-k2.6'), 'color-mix(in srgb, #1C81FA 75%, var(--card))', 'models draw at their tier');
assert.equal(call('modelColour', 'claude-haiku-4-5'), 'color-mix(in srgb, #D57355 50%, var(--card))');
assert.equal(call('modelColour', 'kimi-k9'), call('modelColour', 'kimi-k3'), 'unlisted models take the full provider hue');
assert.equal(call('modelColour', 'mystery'), '#7A9EFB', 'unknown providers take the default hue');
assert.equal(call('providerColour', 'anthropic'), '#D57355', 'providers draw at their full hue');
assert.equal(call('providerColour', 'openai-codex'), '#439F7C', 'provider variants share a hue');
assert.equal(call('providerColour', 'unknown'), '#7A9EFB', 'unknown providers take the default hue');
assert(call('gantt', settled, [ph(1,'x',T(0),T(5),{owner:'"><b>'})], 0).includes('data-owner="&quot;>&lt;b>"'), 'owner is escaped');

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
assert(w(ms(10)).includes('data-line="0">⏳</span>'), 'running is marked');
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
assert(html.includes('⋯ 58m20s') && html.includes('title="idle 58m20s · 2026-09-20T12:01:40.000Z'), 'the break keeps its real span');
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

// arrange: each piece inside or beside its block, cut or hidden when there is
// no room, and sublanes for overlaps. x is from the block's left edge.
const arrange = (blocks, width) => JSON.parse(JSON.stringify(call('arrange', blocks, width)));
const lines = (blocks, width) => arrange(blocks, width).map(b => b.lines);
const at = (x, sep = false) => ({x, sep});
// Everything fits: one piece, then two side by side under it.
assert.deepEqual(lines([{left:0, width:200, lines:[[40], [50, 50]]}], 400),
  [[[at(4)], [at(4), at(54, true)]]]);
// Pieces choose one by one, in reading order: what fits stays inside, the
// rest of that line starts again beside the block.
assert.deepEqual(lines([{left:0, width:60, lines:[[30], [40, 50, 60]]}], 400),
  [[[at(4)], [at(4), at(64), at(114, true)]]]);
assert.deepEqual(arrange([{left:0, width:100, lines:[[40], [50, 50]]}], 400)[0],
  {lane:0, side:'right', lines:[[at(4)], [at(4), at(104)]]});
// No room on the right: the left, with the line ending at the gap.
assert.deepEqual(arrange([{left:300, width:20, lines:[[40], []]}], 330)[0],
  {lane:0, side:'left', lines:[[at(-44)], []]});
// Nowhere fits whole: cut to the wider side, and the next block's label then
// sees what that took.
assert.deepEqual(lines([{left:0, width:10, lines:[[100], []]}, {left:60, width:10, lines:[[5], []]}], 200),
  [[[{x:14, sep:false, cut:42}], []], [[at(14)], []]]);
// A long name is cut inside a block with more room than either side.
assert.deepEqual(lines([{left:0, width:100, lines:[[300], [20]]}], 110),
  [[[{x:4, sep:false, cut:92}], [at(4)]]]);
// A spilled label is an obstacle for its neighbour, which then has no room.
assert.deepEqual(lines([{left:0, width:10, lines:[[40], []]}, {left:60, width:10, lines:[[40], []]}], 70),
  [[[at(14)], []], [[{hidden:true}], []]]);
// Nowhere wide enough to read: hidden rather than a stray glyph at the edge.
assert.deepEqual(lines([{left:0, width:3, lines:[[80], []]}, {left:20, width:3, lines:[[80], []]}], 30),
  [[[{hidden:true}], []], [[{hidden:true}], []]]);
// The first piece past the room is cut and the rest of its line hidden.
assert.deepEqual(lines([{left:0, width:10, lines:[[20], [30, 40, 50]]}], 100),
  [[[at(14)], [at(14), {x:44, sep:true, cut:52}, {hidden:true}]]]);
assert.deepEqual(arrange([{left:50, width:2, lines:[[5]]}, {left:51, width:2, lines:[[5]]}, {left:60, width:2, lines:[[5]]}], 200)
  .map(s => s.lane), [0, 1, 0], 'overlapping minimum-width blocks take sublanes');

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

  const events = [event('input',{prompt:'first'}), event('usage',{attempt:0,seq:1,tokens:10,cost:0.1,model:'model'}), event('permit',{kept:['a.go'],reverted:['<bad>']})];
  while (events.length < 500) events.push(event('log','padding'));
  assert.equal(nodes.has('log'), false, 'the run scaffold must not be precreated');
  serve(events, phases, {...run, repo:'/repo/MiXeD <&"\'>', request:'fix <prompt>&'});
  await evaluate('tick()');
  assert.equal(scaffoldCount, 1, 'the first poll creates the whole run scaffold');
  const scaffold = nodes.get('app').innerHTML;
  const sections = [...scaffold.matchAll(/<section class="card[^>]*>([\s\S]*?)<\/section>/g)].map(m => m[1]);
  assert.equal(sections.length, 6, 'only the six requested top-level sections');
  assert.deepEqual(sections.map(s => s.match(/<h2[^>]*>(.*?)<\/h2>/)[1]),
    ['', 'phases', 'costs', 'details', 'iterations', 'events']);
  assert(sections[1].includes('<div id="phases"></div>') && sections[1].includes('id="phase"'),
    'the selected-phase inspector stays nested within PHASES');
  assert(sections[2].includes('id="costs"') && sections[3].includes('id="summary"'));
  assert.equal(nodes.get('project-title').innerHTML, '/repo/MiXeD &lt;&amp;&quot;&#39;>');
  assert.equal(nodes.get('head').innerHTML, 'fix &lt;prompt>&amp;');
  assert.equal(evaluate('done'), false, 'full page must keep polling a settled run');
  assert(costs().includes('$0.30000') && costs().includes('unknown') && costs().includes('model') && costs().includes('$0.10000'), costs());
  assert(costs().includes('Loading usage…') && !costs().includes('No usage recorded.'), 'partial rows still load');
  assert(costs().indexOf('$0.30000') < costs().indexOf('providers') && costs().indexOf('providers') < costs().indexOf('models'),
    'run total precedes providers, which precede models');
  assert(!costs().includes('unknown/model'), 'model names are bare');
  assert.deepEqual(JSON.parse(JSON.stringify(evaluate('providerCosts()'))), [{provider:'unknown', cost:0.1}],
    'missing provider becomes unknown in providerCosts');
  assert(chart().includes('fix · model · success · $0.10000'), chart());
  assert(chart().includes('data-line="0">✅</span><span class="piece tight" data-line="0">model</span><span class="piece" data-line="1">$0.10000</span>'),
    'status and model share the first line, cost the second');
  assert(chart().includes('aria-label="#2 fix · success"'), 'no usage means no model or cost');
  assert.equal(chart().split('class="node"').length - 1, 1, 'both entries share a row');
  assert(panel().includes('Select a phase.'));
  assert.equal(nodes.get('app').scrollTop, 0, 'must not scroll a reader away');
  assert.equal(observers[0].watching[0], nodes.get('phases'), 'the chart is watched for width changes');
  assert.equal(nodes.get('follow-label').style.display, 'inline-flex', 'follow toggle is shown on a run page');
  assert.equal(nodes.get('follow-box').checked, undefined, 'follow defaults off');

  // Selection renders from cache; missing usage is still loading while the
  // backlog drains, not "not recorded".
  evaluate(`pick('p2')`);
  assert(panel().includes('Loading usage…') && panel().includes('Input context not recorded.'), panel());
  assert(chart().includes('data-phase="p2" data-focus="block-p2" aria-pressed="true"'));
  evaluate(`pick('p1')`);
  assert(panel().includes('first') && panel().includes('#1'), panel());
  evaluate(`panels.set('p1-inputs', true)`);

  // Keyboard focus comes back to the same control, without scrolling, and
  // only when it was inside the replaced region.
  const focused = new Element(), twin = new Element();
  focused.dataset.focus = twin.dataset.focus = 'block-p1';
  nodes.get('phases').contains = el => el === focused;
  nodes.get('phases').querySelectorAll = sel => sel === '[data-focus]' ? [twin] : [];
  context.document.activeElement = focused;

  // With follow disabled, being at the bottom must not pull the page down.
  nodes.get('follow-box').checked = false;
  nodes.get('app').scrollTop = nodes.get('app').scrollHeight - nodes.get('app').clientHeight;
  serve([event('input',{prompt:'correction'}),event('usage',{attempt:1,seq:1,provider:'pi',tokens:20,cost:0.2}),
         event('log','test command','p2','command'),event('log','  <b>spaced</b>\n  out','p2','output'),
         event('log','','p2','output')]);
  await evaluate('tick()');
  assert.equal(evaluate('done'), true);
  assert.equal(nodes.get('app').scrollTop, nodes.get('app').scrollHeight - nodes.get('app').clientHeight, 'follow off preserves scroll position');
  assert.deepEqual({...twin.focused}, {preventScroll: true}, 'focus restored without scrolling');
  twin.focused = null; context.document.activeElement = new Element();
  evaluate('draw()');
  assert.equal(twin.focused, null, 'focus elsewhere is left alone');
  nodes.get('phases').contains = () => false; nodes.get('phases').querySelectorAll = () => [];

  // The selection and its open disclosures survived the poll; spend summed
  // once across pages, with one row per recorded response.
  for (const text of ['first','correction','30 tok','$0.30000','unknown/model','pi/unknown',
                      'data-panel="p1-inputs" open']) assert(panel().includes(text), text);
  assert.equal(panel().split('attempt ').length - 1, 2, 'one usage row per response');
  assert(chart().includes('$0.30000'));

  // Selection still works after polling stops, and disclosure state is per phase.
  evaluate(`pick('p2')`);
  for (const text of ['test command','<pre>  &lt;b>spaced&lt;/b>\n  out</pre>','The command produced no output.',
                      'Usage not recorded.']) assert(panel().includes(text), text);
  assert(!panel().includes('Structured output not recorded'), 'a command-only phase has no report to miss');
  assert(!panel().includes('data-panel="p2-inputs" open'));
  evaluate(`pick('p1')`);
  assert(panel().includes('data-panel="p1-inputs" open'), 'switching back keeps what was open');
  evaluate(`pick('p1')`);
  assert(panel().includes('Select a phase.'), 'clicking the selected block deselects it');
  assert.equal(nodes.get('log').children.length, 505);
  const permitRows = nodes.get('log').children.filter(row => row.innerHTML.includes('>permit<'));
  assert.equal(permitRows.length, 1, 'permit event appears once across paginated polls');
  assert(permitRows[0].innerHTML.includes('&lt;bad>') && !permitRows[0].innerHTML.includes('<bad>'),
    'permit payload is escaped in the events log');
  // The bottom bar carries the run's own context, and a settled run reads run complete.
  const bar = nodes.get('status').innerHTML;
  // Queue time and execution time are separate: 10s waiting, 1m05s working.
  for (const text of ['build','ok','$0.30000','10s queued','1m05s','repo','run']) assert(bar.includes(text),`status: ${text}`);
  const summary = nodes.get('summary').innerHTML;
  for (const text of ['<dt>workflow</dt><dd>build</dd>', '<dt>status</dt><dd class="ok">ok</dd>',
                      '<dt>queued</dt><dd>10s</dd>', '<dt>duration</dt><dd>1m05s</dd>',
                      '<dt>repo</dt><dd>repo</dd>', '<dt>run id</dt><dd>run</dd>'])
    assert(summary.includes(text), `details card: ${text}`);
  assert(!summary.includes('<dt>cost</dt>'), 'cost is only in COSTS, not DETAILS');
  assert(!costs().includes('Loading usage…') && costs().includes('pi') && costs().includes('model'),
    'finished pages have final rows');
  assert(!costs().includes('pi/unknown') && !costs().includes('unknown/model'), 'model names are bare');
  assert.equal(scaffoldCount, 1, 'a redraw does not rebuild the append-only event stream');
  assert.equal(nodes.get('note').textContent, 'run complete');
  assert.equal(nodes.get('follow-label').style.display, 'none', 'follow toggle is hidden once the run is complete');

  // Label placement reads the rendered pixels, resets what it set last time,
  // and runs again when the chart's width changes.
  const piece = (line, scrollWidth) => Object.assign(new Element(), {dataset: {line: String(line)}, scrollWidth});
  const blocks = [[piece(0, 20), piece(1, 30)], [piece(0, 40)]]
    .map((children, i) => Object.assign(new Element(), {offsetLeft: i * 100, offsetWidth: 10, children}));
  const pieces = blocks.flatMap(b => b.children);
  const track = Object.assign(new Element(), {clientWidth: 400, querySelectorAll: () => blocks});
  nodes.get('phases').querySelectorAll = sel => sel === '.track' ? [track] : [];
  pieces[2].style.display = 'none';   // left over from an earlier placement
  evaluate('layout()');
  assert.deepEqual(pieces.map(c => c.style.left), ['14px', '14px', '14px']);
  assert.equal(pieces[2].style.display, '', 'placement starts from a clean slate');
  track.clientWidth = 150;   // the rail expanded: the last label no longer fits on the right
  observers[0].callback([]); observers[0].callback([]);
  assert.equal(frames.length, 1, 'resize callbacks coalesce into one frame');
  frames.shift()();
  assert.deepEqual(pieces.map(c => c.style.left), ['14px', '14px', '-44px']);
  blocks[1].offsetLeft = 5;   // overlapping minimum-width blocks
  evaluate('layout()');
  assert.deepEqual(blocks.map(b => b.style.top), ['1px', '41px']);
  assert.equal(track.style.height, '80px');
  nodes.get('phases').querySelectorAll = () => [];

  // With follow enabled, the page snaps to the tail when the reader is already at the bottom.
  evaluate('done = false');
  nodes.get('follow-box').checked = true;
  nodes.get('app').scrollTop = nodes.get('app').scrollHeight - nodes.get('app').clientHeight;
  serve([event('log','tail event')]);
  await evaluate('tick()');
  assert.equal(nodes.get('app').scrollTop, nodes.get('app').scrollHeight, 'follow on scrolls to the tail');
  assert.equal(nodes.get('follow-label').style.display, 'none', 'follow toggle is hidden once the run is complete');

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

  // Navigation while a request is in flight cannot contaminate the next run.
  evaluate('done = false'); response = null;
  const pending = evaluate('tick()');
  await evaluate('tick()'); // overlapping interval must not make another request
  evaluate('navigate()');
  assert.equal(observers[0].watching.length, 0, 'navigation stops watching the old chart');
  for (const name of ['usage', 'tails', 'panels', 'outputs', 'responses', 'spend']) assert.equal(evaluate(name + '.size'), 0, name);
  assert.equal(evaluate('selected'), '');
  resolveFetch(json({run,phases,events:[event('usage',{tokens:999}),event('log','late','p1','output'),
                                        event('output',{report:{summary:'late'}})]}));
  await pending;
  assert.equal(evaluate('after'),0);
  for (const name of ['usage', 'tails', 'outputs', 'spend']) assert.equal(evaluate(name + '.size'), 0, `stale ${name}`);
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
  assert(costs().includes('$0.90000') && costs().includes('Loading usage…'), costs());
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
  assert.equal(costsHtml.indexOf('$0.90000') < costsHtml.indexOf('class="model-costs"'), true);

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
  assert(costs().includes('$0.50000') && costs().includes('No usage recorded.'), costs());
  assert(!costs().includes('unknown/') && !costs().includes('Loading usage…'), costs());
  assert.equal(evaluate('usage.size'), 0, 'navigating to another run resets the event cache');
  const liveEmpty = call('renderCosts', {...run, status:'running', cost:0});
  assert(liveEmpty.includes('$0.00000') && liveEmpty.includes('No usage recorded yet.'), liveEmpty);

  // Runs: the summary says displayed versus total, and says whose numbers those are.
  context.location.hash = '#/runs';
  evaluate('view = route(); generation++; done = false');
  response = json([{...run, run_id:'r1'}], {'X-Total-Runs': '1284'});
  await evaluate('tick()');
  const runsBar = nodes.get('status').innerHTML;
  assert(runsBar.includes('showing 1 of 1,284 runs'), runsBar);
  assert(runsBar.includes('displayed:'), runsBar);
  assert(nodes.get('app').innerHTML.includes("location.hash='/runs/r1'"));
  assert(nodes.get('app').innerHTML.includes('>1m05s</td>'), 'the list shows how long a run took');
  assert(!nodes.get('app').innerHTML.includes('10s queued'), 'the list leaves out the wait');

  // A queued run is a live run, not a failed one.
  evaluate('generation++');
  response = json([{...run, run_id:'r2', status:'queued', started_at:'', ended_at:''}], {'X-Total-Runs': '1'});
  await evaluate('tick()');
  const queuedRow = nodes.get('app').innerHTML;
  assert(queuedRow.includes('class="running">queued'), queuedRow);
  assert(!queuedRow.includes('s queued'), 'no wait in the list');

  // Projects list is complete, sorted by the API, and uses full paths for links.
  context.location.hash = '#/projects';
  evaluate('view = route(); generation++');
  response = json([{repo:'/one/alpha',runs:3,tokens:300,cost:3,share:0.75},
                   {repo:'/two/alpha',runs:1,tokens:100,cost:1,share:0.25}]);
  await evaluate('tick()');
  const projects = nodes.get('app').innerHTML;
  assert(projects.includes('4 runs') === false, projects);
  assert(projects.includes('2</b><span>projects recorded'), projects);
  assert(projects.includes('#/projects/detail?repo=%2Fone%2Falpha'), projects);
  assert(projects.includes('#/projects/detail?repo=%2Ftwo%2Falpha'), projects);
  assert(projects.indexOf('$3.00000') < projects.indexOf('$1.00000'), projects);

  // One project shows lifetime totals and pages its own runs into run links.
  context.location.hash = '#/projects/detail?repo=%2Fone%2Falpha';
  evaluate('view = route(); generation++; projectRows = []; projectLoaded = false; projectMore = false');
  const first = {...run, run_id:'r3', repo:'/one/alpha'};
  const second = {...run, run_id:'r2', repo:'/one/alpha'};
  const third = {...run, run_id:'r1', repo:'/one/alpha'};
  routeResponses = new Map([
    ['/api/projects/detail?repo=%2Fone%2Falpha', json({repo:'/one/alpha',runs:3,tokens:300,cost:3,share:0.75})],
    ['/api/projects/runs?repo=%2Fone%2Falpha&n=50', json({runs:[first,second],more:true})],
    ['/api/projects/runs?repo=%2Fone%2Falpha&n=50&before_at=2026-09-20T11%3A59%3A50Z&before_id=r2',
      json({runs:[third],more:false})],
  ]);
  await evaluate('tick()');
  const head = nodes.get('project-head').innerHTML;
  assert(head.includes('300</b><span>tokens') && head.includes('$3.00000'), head);
  let project = nodes.get('project-runs').innerHTML;
  assert(project.includes("location.hash='/runs/r3'") && project.includes('Load more runs'), project);
  assert(nodes.get('status').innerHTML.includes('showing 2 of 3 runs'));
  await evaluate('moreProjectRuns()');
  project = nodes.get('project-runs').innerHTML;
  assert(project.includes("location.hash='/runs/r1'") && !project.includes('Load more runs'), project);
  assert(nodes.get('status').innerHTML.includes('showing 3 of 3 runs'));
  routeResponses = null;

  // Overview: lifetime totals, all four rankings, and the attribution caveats.
  context.location.hash = '#/overview';
  evaluate('view = route(); generation++');
  response = json({
    runs: 1284, tokens: 9_000_000, cost: 42.18374,
    top_projects: [{repo:'/repos/alpha', runs:900, cost:38, share:0.9},
                   {repo:'', runs:100, cost:4, share:0.1}],
    top_runs: [{...run, run_id:'top', cost: 9.5}, {...run, run_id:'long', request:'x'.repeat(149) + '😀tail'}],
    top_models: [{provider:'moonshotai', model:'kimi', cost:30, share:0.7113, phases:2700},
                 {provider:'', model:'', cost:2, share:0.0474, phases:12}],
    top_providers: [{provider:'moonshotai', cost:32, share:0.7586, phases:2712},
                    {provider:'', cost:2, share:0.0474, phases:12}],
    at: '2026-09-20T12:00:00Z',
  });
  await evaluate('tick()');
  const over = nodes.get('app').innerHTML;
  for (const text of ['1,284','$42.18374','$9.50000','alpha','$38.00000','90.0%','900 runs',
                      'moonshotai/kimi','71.1%','unknown/unknown',
                      '2,700 phases','counted per phase',"location.hash='/runs/top'"]) {
    assert(over.includes(text), `overview: ${text}`);
  }
  assert(over.includes('>' + 'x'.repeat(149) + '😀…</td>'), 'a long request is cut to 150 characters');
  assert(over.includes('>fix</td>'), 'a short request is not cut');
  assert(nodes.get('status').innerHTML.includes('all repositories · all time'));
  const projectsH2 = over.indexOf('top projects by spend');
  const modelsH2 = over.indexOf('top models by spend');
  const providersH2 = over.indexOf('top providers by spend');
  const runsH2 = over.indexOf('top runs by spend');
  assert(projectsH2 >= 0 && modelsH2 >= 0 && providersH2 >= 0 && runsH2 >= 0, 'all four ranking headings present');
  assert(projectsH2 < runsH2 && runsH2 < modelsH2 && modelsH2 < providersH2,
    'projects and runs stack in the first column, models and providers in the second');
  const provCard = over.slice(providersH2);
  for (const text of ['$32.00000', '75.9%', '2,712 phases']) assert(provCard.includes(text), `provider card: ${text}`);
  assert(provCard.includes('>unknown<'), 'empty provider falls back to unknown');
  assert(provCard.indexOf('moonshotai') < provCard.indexOf('>unknown<'), 'providers are sorted by spend descending');
  assert(over.includes('#/projects/detail?repo=%2Frepos%2Falpha'), 'overview project links to its detail');

  // Empty database: zero totals and empty tables, never a blank page.
  evaluate('generation++');
  response = json({runs:0, tokens:0, cost:0, top_projects:[], top_runs:[], top_models:[], top_providers:[], at:''});
  await evaluate('tick()');
  const empty = nodes.get('app').innerHTML;
  assert(empty.includes('$0.00000') && empty.includes('No runs recorded yet.')
         && empty.includes('No model spend recorded yet.')
         && empty.includes('No provider spend recorded yet.')
         && empty.includes('No project spend recorded yet.'), empty);

  // A failed refresh keeps the last good values and says they are stale.
  failNext = 'database is locked';
  await evaluate('tick()');
  assert(nodes.get('app').innerHTML.includes('$0.00000'), 'values must survive a failed poll');
  assert.equal(nodes.get('note').className, 'stale');
  assert(nodes.get('note').textContent.includes('database is locked'));

  // An unknown route is a page, not a run id.
  context.location.hash = '#/nope';
  evaluate('view = route(); generation++');
  await evaluate('tick()');
  assert(nodes.get('app').innerHTML.includes('not found'));

  console.log('dashboard client checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
