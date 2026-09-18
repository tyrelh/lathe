// Run the embedded client against a small DOM surface and paginated API.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = new Map();
class Element {
  constructor() { this.innerHTML = ''; this.children = []; }
  insertRow() { const row = new Element(); this.children.push(row); return row; }
}
for (const id of ['app', 'sub', 'head', 'phases', 'log', 'permits']) nodes.set(id, new Element());
let expanded = [], response, resolveFetch;
const context = vm.createContext({
  document: {
    getElementById: id => nodes.get(id), createElement: () => new Element(),
    querySelectorAll: () => expanded, body: {scrollHeight: 1000},
  },
  location: {hash: '#run'}, addEventListener() {}, setInterval() {},
  innerHeight: 100, scrollY: 0, scrollTo() { throw Error('must not scroll a reader away'); },
  fetch: async () => response || await new Promise(resolve => { resolveFetch = resolve; }),
});
// Everything up to the bootstrap; the harness drives tick() itself. Cut at one
// anchor rather than matching the two lines verbatim, so reformatting them does
// not silently leave the page self-ticking against an unresolved fetch.
const script = fs.readFileSync('index.html', 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const bootstrap = script.lastIndexOf('tick();');
assert(bootstrap > 0, 'bootstrap not found');
vm.runInContext(script.slice(0, bootstrap), context);
const evaluate = code => vm.runInContext(code, context);
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
const run = {workflow:'build',status:'ok',tokens:30,cost:0.3,repo:'/repo',request:'fix'};
let id = 0;
const event = (type, payload, phase_id='p1', name='fix') => ({event_id:++id,type,payload,phase_id,name});
function serve(events) { response = {ok:true,json:async () => ({run,phases,events})}; }
(async () => {
  const events = [event('input',{prompt:'first'}), event('usage',{tokens:10,cost:0.1,model:'model'}), event('permit',{kept:['a.go'],reverted:['<bad>']})];
  while (events.length < 500) events.push(event('log','padding'));
  serve(events);
  await evaluate('tick()');
  assert.equal(evaluate('done'), false, 'full page must keep polling a settled run');
  assert(nodes.get('phases').innerHTML.includes('first'));
  assert(nodes.get('phases').innerHTML.includes('Input context not recorded.'));
  assert(nodes.get('permits').innerHTML.includes('&lt;bad>'));
  expanded = [{dataset:{phase:'p1'}}];
  serve([event('input',{prompt:'correction'}),event('usage',{tokens:20,cost:0.2}),event('log','test command','p2','command')]);
  await evaluate('tick()');
  assert.equal(evaluate('done'), true);
  const html = nodes.get('phases').innerHTML;
  for (const text of ['first','correction','30 tok','$0.30000','model','test command','data-phase="p1" open']) assert(html.includes(text),text);
  assert.equal(nodes.get('log').children.length,503);
  assert.equal(nodes.get('permits').innerHTML.split('<section>').length - 1, 1, 'permit rendered once');
  // Navigation while a request is in flight cannot contaminate the next run.
  evaluate('done = false'); response = null;
  const pending = evaluate('tick()');
  await evaluate('tick()'); // overlapping interval must not make another request
  evaluate('generation++; after = 0; spend.clear(); inputs.clear()');
  resolveFetch({ok:true,json:async()=>({run,phases,events:[event('usage',{tokens:999})]})});
  await pending;
  assert.equal(evaluate('after'),0);
  assert.equal(evaluate('spend.size'),0);
  console.log('dashboard client checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
