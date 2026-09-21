// Run the embedded client against a small DOM surface and paginated API.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = new Map();
class Element {
  constructor() {
    this.innerHTML = ''; this.children = []; this.attrs = new Map();
    this.textContent = ''; this.className = '';
    this.scrollTop = 0; this.clientHeight = 100; this.scrollHeight = 1000;
  }
  insertRow() { const row = new Element(); this.children.push(row); return row; }
  setAttribute(k, v) { this.attrs.set(k, v); }
  removeAttribute(k) { this.attrs.delete(k); }
}
for (const id of ['app', 'status', 'note', 'version', 'head', 'phases', 'log', 'permits',
                  'nav-overview', 'nav-runs']) nodes.set(id, new Element());
let expanded = [], response, resolveFetch, failNext = null;
const context = vm.createContext({
  document: {
    getElementById: id => nodes.get(id), createElement: () => new Element(),
    querySelectorAll: () => expanded, body: {scrollHeight: 1000},
  },
  location: {hash: '#run'}, addEventListener() {}, setInterval() {}, Date,
  innerHeight: 100, scrollY: 0, scrollTo() { throw Error('must not scroll a reader away'); },
  fetch: async url => {
    if (url === '/api/meta') return json({version: 'v0.2.0-test'});
    if (failNext) { const e = failNext; failNext = null; return {ok: false, text: async () => e}; }
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

// Routing: the agreed default, the explicit routes, the legacy deep link, and
// an unknown hash that must not be mistaken for a run id.
for (const [hash, want] of [
  ['', 'overview'], ['#/', 'overview'], ['#/overview', 'overview'], ['#/runs', 'runs'],
  ['#/runs/20260920T120000Z_build_1', 'run'], ['#20260920T120000Z_build_1', 'run'],
  ['#/nope', 'missing'],
]) {
  context.location.hash = hash;
  const r = evaluate('route()');
  assert.equal(r.page, want, `${hash} -> ${r.page}`);
  if (want === 'run') assert.equal(r.id, '20260920T120000Z_build_1', hash);
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
const phases = [1,2].map(seq => ({phase_id:'p'+seq, seq, name:'fix', owner:'builder', status:'success', started_at:'', ended_at:'done'}));
const run = {run_id:'run', workflow:'build',status:'ok',tokens:30,cost:0.3,repo:'/repo',request:'fix',submitted_at:'2026-09-20T11:59:50Z',started_at:'2026-09-20T12:00:00Z',ended_at:'2026-09-20T12:01:05Z'};
let id = 0;
const event = (type, payload, phase_id='p1', name='fix') => ({event_id:++id,type,payload,phase_id,name});
function serve(events) { response = json({run,phases,events}); }
(async () => {
  context.location.hash = '#run';
  evaluate('view = route()');

  const events = [event('input',{prompt:'first'}), event('usage',{tokens:10,cost:0.1,model:'model'}), event('permit',{kept:['a.go'],reverted:['<bad>']})];
  while (events.length < 500) events.push(event('log','padding'));
  serve(events);
  await evaluate('tick()');
  assert.equal(evaluate('done'), false, 'full page must keep polling a settled run');
  assert(nodes.get('phases').innerHTML.includes('first'));
  assert(nodes.get('phases').innerHTML.includes('Input context not recorded.'));
  assert(nodes.get('permits').innerHTML.includes('&lt;bad>'));
  assert.equal(nodes.get('app').scrollTop, 0, 'must not scroll a reader away');
  expanded = [{dataset:{phase:'p1'}}];
  serve([event('input',{prompt:'correction'}),event('usage',{tokens:20,cost:0.2}),event('log','test command','p2','command')]);
  await evaluate('tick()');
  assert.equal(evaluate('done'), true);
  const html = nodes.get('phases').innerHTML;
  for (const text of ['first','correction','30 tok','$0.30000','model','test command','data-phase="p1" open']) assert(html.includes(text),text);
  // One line per phase: the summary is the phase row, not a second row under it.
  assert.equal(html.split('<details').length - 1, phases.length, 'one details per phase');
  assert.equal(html.split('<summary').length - 1, phases.length, 'one summary per phase');
  assert(!html.includes('<tr'), 'phases must no longer render table rows');
  assert.equal(nodes.get('log').children.length,503);
  assert.equal(nodes.get('permits').innerHTML.split('<section>').length - 1, 1, 'permit rendered once');
  // The bottom bar carries the run's own context, and a settled run reads final.
  const bar = nodes.get('status').innerHTML;
  // Queue time and execution time are separate: 10s waiting, 1m05s working.
  for (const text of ['build','ok','$0.30000','10s queued','1m05s','repo','run']) assert(bar.includes(text),`status: ${text}`);
  assert.equal(nodes.get('note').textContent, 'final');

  // Navigation while a request is in flight cannot contaminate the next run.
  evaluate('done = false'); response = null;
  const pending = evaluate('tick()');
  await evaluate('tick()'); // overlapping interval must not make another request
  evaluate('generation++; after = 0; spend.clear(); inputs.clear()');
  resolveFetch(json({run,phases,events:[event('usage',{tokens:999})]}));
  await pending;
  assert.equal(evaluate('after'),0);
  assert.equal(evaluate('spend.size'),0);

  // Runs: the summary says displayed versus total, and says whose numbers those are.
  context.location.hash = '#/runs';
  evaluate('view = route(); generation++; done = false');
  response = json([{...run, run_id:'r1'}], {'X-Total-Runs': '1284'});
  await evaluate('tick()');
  const runsBar = nodes.get('status').innerHTML;
  assert(runsBar.includes('showing 1 of 1,284 runs'), runsBar);
  assert(runsBar.includes('displayed:'), runsBar);
  assert(nodes.get('app').innerHTML.includes("location.hash='/runs/r1'"));

  // A queued run is a live run, not a failed one, and it shows its wait.
  evaluate('generation++');
  response = json([{...run, run_id:'r2', status:'queued', started_at:'', ended_at:''}], {'X-Total-Runs': '1'});
  await evaluate('tick()');
  const queuedRow = nodes.get('app').innerHTML;
  assert(queuedRow.includes('class="running">queued'), queuedRow);
  assert(queuedRow.includes('queued</td>') || queuedRow.includes(' queued'), queuedRow);

  // Overview: lifetime totals, both rankings, and the attribution caveats.
  context.location.hash = '#/overview';
  evaluate('view = route(); generation++');
  response = json({
    runs: 1284, tokens: 9_000_000, cost: 42.18374,
    top_runs: [{...run, run_id:'top', cost: 9.5}],
    top_models: [{provider:'moonshotai', model:'kimi', cost:30, share:0.7113, phases:2700},
                 {provider:'', model:'', cost:2, share:0.0474, phases:12}],
    at: '2026-09-20T12:00:00Z',
  });
  await evaluate('tick()');
  const over = nodes.get('app').innerHTML;
  for (const text of ['1,284','$42.18374','$9.50000','moonshotai/kimi','71.1%','unknown/unknown',
                      '2,700 phases','counted per phase',"location.hash='/runs/top'"]) {
    assert(over.includes(text), `overview: ${text}`);
  }
  assert(nodes.get('status').innerHTML.includes('all repositories · all time'));

  // Empty database: zero totals and empty tables, never a blank page.
  evaluate('generation++');
  response = json({runs:0, tokens:0, cost:0, top_runs:[], top_models:[], at:''});
  await evaluate('tick()');
  const empty = nodes.get('app').innerHTML;
  assert(empty.includes('$0.00000') && empty.includes('No runs recorded yet.')
         && empty.includes('No model spend recorded yet.'), empty);

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
