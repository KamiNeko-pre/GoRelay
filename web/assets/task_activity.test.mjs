import test from 'node:test';
import assert from 'node:assert/strict';
import {taskActivity} from './task_activity.mjs';

test('saved receipt failure is visible even if the transport operation succeeded',()=>{
 const out={run:{status:'failed',created_at:'2026-10-21T01:00:00Z',updated_at:'2026-10-21T03:00:00Z'},operations:[
 {id:1,kind:'read_data',stage:'succeeded',created_at:'2026-10-21T01:00:00Z',updated_at:'2026-10-21T01:00:01Z',result_json:JSON.stringify({error:'selector matched no rows'})}
 ]};
 const a=taskActivity(out);
 assert.equal(a.events[0].state,'failed');
 assert.equal(a.events[0].failure,'selector matched no rows');
 assert.equal(a.lastTime,'2026-10-21T01:00:01Z');
 assert.equal(a.active,null);
 assert.match(a.headline,/停止/);
});

test('unknown/queued receipts never claim active execution; reasoning is not public activity',()=>{
 const op={id:1,kind:'model',stage:'reserved',created_at:'2026-10-21T01:00:00Z',result_json:'{}'};
 assert.equal(taskActivity({run:{status:'queued'},operations:[op]}).active,null);
 assert.equal(taskActivity({run:{status:'failed'},operations:[op]}).events[0].state,'unknown');
 assert.equal(taskActivity({run:{status:'running'},operations:[op]}).active.id,'crawler:1');
 const a=taskActivity({run:{status:'completed'},operations:[{...op,stage:'succeeded',result_json:JSON.stringify({message:{reasoning_content:'private reasoning',tool_calls:[{id:'a',function:{name:'inspect_source',arguments:'{"url":"https://example.org"}'}}]}})}]});
 assert.equal(JSON.stringify(a).includes('private reasoning'),false);
 assert.match(a.events[0].detail,/检查页面结构/);
});

 test('framework completion closes matching start events without hiding a new action',()=>{
 const first={id:'information:1',kind:'model_started',state:'pending',time:'2026-10-25T01:00:00Z',title:'正在规划'};
 const second={id:'information:2',kind:'assistant',state:'done',time:'2026-10-25T01:00:01Z',title:'方案已生成'};
 const out={run:{status:'running'},activity:[first,second],routing_steps:[{id:10,kind:'agent_event',stage:'done'}]};
 const a=taskActivity(out);assert.equal(a.active,null);assert.equal(a.events.length,1);
 out.activity.push({id:'information:3',kind:'tool_started',tool:'query_observations',state:'pending',time:'2026-10-25T01:00:02Z',title:'正在查资料'});
 const b=taskActivity(out);assert.equal(b.active.id,'information:3');
 out.run.status='blocked_provider';const c=taskActivity(out);assert.equal(c.active,null);assert.equal(c.events[0].state,'unknown');
 });
