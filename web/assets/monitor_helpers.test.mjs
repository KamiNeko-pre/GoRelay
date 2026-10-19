import test from 'node:test';
import assert from 'node:assert/strict';
import {parseMonitorValue,addMonitorCondition,inboxState,visibleMonitorItems} from './monitor_helpers.mjs';

test('typed conditions preserve free and false, reject missing or coerced values',()=>{
 assert.equal(parseMonitorValue('0','integer','lte'),0);
 assert.equal(parseMonitorValue('false','boolean','eq'),false);
 assert.equal(parseMonitorValue('zh','string_array','contains'),'zh');
 for(const value of ['',' ','30abc','NaN']) assert.throws(()=>parseMonitorValue(value,'number','lte'));
 assert.throws(()=>parseMonitorValue('2.5','integer','eq'));
 assert.throws(()=>parseMonitorValue('yes','boolean','eq'));
});
test('display cap does not freeze entity identities or mutate acquired evidence',()=>{
 const items=Array.from({length:14},(_,i)=>({monitor_id:2,id:i}));items.unshift({monitor_id:1,id:99});
 assert.equal(visibleMonitorItems(items,2).length,10);assert.equal(items.length,15);
 const changed=[{monitor_id:2,id:'new-object'},...items];assert.equal(visibleMonitorItems(changed,2)[0].id,'new-object');
 assert.equal(visibleMonitorItems(items,1).length,1);
});
test('OR alternatives retain mandatory currency and do not mutate saved configuration',()=>{
 const original={conditions:[{field:'unit',operator:'eq',value:'CNY'}],condition_groups:[[{field:'discount',operator:'gte',value:20}]]};
 const next=addMonitorCondition(original,{field:'amount',operator:'lte',value:3000},'alternative');
 assert.equal(next.conditions[0].value,'CNY');assert.equal(next.condition_groups.length,2);assert.equal(original.condition_groups.length,1);
 assert.throws(()=>addMonitorCondition(original,{field:'amount',operator:'lte',value:3000},'unknown'));
});
test('failed/expired evidence is unconfirmed and withdrawal remains distinct',()=>{
 const now=new Date('2026-10-10T08:00:00Z');
 assert.equal(inboxState({active:true,verified:true,valid_until:'2026-10-10T07:00:00Z'},now),'未确认当前有效');
 assert.equal(inboxState({active:true,verified:false},now),'未确认当前有效');
 assert.equal(inboxState({active:false,verified:true},now),'已不满足条件');
 assert.equal(inboxState({active:true,verified:true},now),'当前满足条件');
});
