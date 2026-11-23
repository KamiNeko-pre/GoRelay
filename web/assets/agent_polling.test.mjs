import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('./agent.js', import.meta.url), 'utf8');
const loadCode = source.slice(source.indexOf('async function loadRun()'), source.indexOf('let diagnosticTask='));
const followCode = source.match(/^function follow\(id\).*$/m)[0];

function taskView(id) { return { run: { id, status: 'running' }, children: [], result: {} }; }

test('slow task reads settle once and a previous selection cannot overwrite the current task', async () => {
 const pending=[], ticks=[], rendered=[];
 const nodes=new Map();
 const context=vm.createContext({
  $: id => { if(!nodes.has(id)) nodes.set(id,{open:false,replaceChildren(){},append(){},prepend(){},classList:{add(){},remove(){}},querySelector:()=>null});return nodes.get(id); },
  taskApi: path => new Promise(resolve=>pending.push({path,resolve})),
  guard: fn=>fn(), studio:{renderRun:out=>rendered.push(out.run.id)}, show(){}, statusText:status=>status,isTerminal:()=>false,
  setInterval:fn=>{ticks.push(fn);return ticks.length},clearInterval(){},
 });
 vm.runInContext(`let selected='',timer=null,viewVersion=0,pendingContract=null,runFlight=null,lastTaskSnapshot=null,diagnosticTask='';
${loadCode}
${followCode}
globalThis.followTask=follow;globalThis.readTask=loadRun;`,context);
 const first=context.followTask('A');
 ticks[0]();ticks[0]();
 assert.equal(pending.length,1,'slow reads must be coalesced instead of repeatedly invalidating their response');
 pending[0].resolve(taskView('A'));await first;
 assert.deepEqual(rendered,['A']);
 const stale=context.readTask();
 const next=context.followTask('B');
 assert.equal(pending.length,3);
 pending[2].resolve(taskView('B'));await next;
 pending[1].resolve(taskView('A'));await stale;
 assert.deepEqual(rendered,['A','B'],'stale selection overwrote current task');
});

test('a paused task keeps updating until a later terminal status', async () => {
 for (const initial of ['blocked_provider','needs_input']) {
  let active=false, tick, status=initial; const rendered=[], nodes=new Map();
  const context=vm.createContext({
   $:id=>{if(!nodes.has(id))nodes.set(id,{open:false,replaceChildren(){},append(){},prepend(){},classList:{add(){},remove(){}},querySelector:()=>null});return nodes.get(id)},
   taskApi:async()=>({run:{id:'A',status},children:[],result:{}}),
   guard:fn=>fn(),studio:{renderRun:out=>rendered.push(out.run.status)},show(){},statusText:x=>x,isTerminal:x=>x==='expired',
   setInterval:fn=>{active=true;tick=fn;return 1},clearInterval:()=>{active=false},
  });
  vm.runInContext(`let selected='',timer=null,viewVersion=0,pendingContract=null,runFlight=null,lastTaskSnapshot=null,diagnosticTask='';\n${loadCode}\n${followCode}\nglobalThis.followTask=follow;`,context);
  await context.followTask('A');
  assert.equal(active,true,initial+' must remain observable while it can later expire');
  status='expired';await tick();
  assert.deepEqual(rendered,[initial,'expired']);
  assert.equal(active,false,'terminal tasks stop polling');
 }
});
