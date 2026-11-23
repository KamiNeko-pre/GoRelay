import test from 'node:test';
import assert from 'node:assert/strict';
import {deliveryMarkdown,recordReferences,deliveryLink,deliveryCSV} from './delivery_export.mjs';

test('export keeps local snapshot, source time, unknown values and editable actions',()=>{
 globalThis.location={origin:'http://127.0.0.1:18109'};
 const record={fields:{name:'工具 A',version:'2.0',price:null},provenance:{url:'https://example.org/item',fetched_at:'2026-11-10T12:00:00Z'},evidence_documents:[{snapshot_url:'/evidence#id=doc&revision=rev',url:'https://example.org/item',fetched_at:'2026-11-10T12:00:00Z',origin:'source_response'}]};
 const result=deliveryMarkdown({run:{goal:'比较版本',status:'completed'},result:{records:[record],gaps:['兼容性待确认']}},{note:'核对兼容性'});
 assert.match(result,/状态：完成/);assert.ok(result.includes('http://127.0.0.1:18109/evidence#id=doc&revision=rev'));assert.match(result,/2026-11-10T12:00:00Z/);assert.match(result,/未记录/);assert.match(result,/- \[ \] 核对兼容性/);assert.match(result,/兼容性待确认/);
 assert.equal(recordReferences(record).length,2);
 assert.equal(deliveryLink('javascript:alert(1)'),null);
 const csv=deliveryCSV([{...record,table_title:'版本',observation_ids:['fact'],fields:{name:'工具 A',price:null},evidence_documents:[{doc_id:'doc',revision:'rev',url:'https://example.org/item',snapshot_url:'/evidence#id=doc&revision=rev',fetched_at:'2026-11-10T12:00:00Z'}]}],['name','price'],{gaps:['兼容性待确认'],diagnostics:[{field:'price',observation_ids:['fact'],message:'价格尚未核实'}]});
 assert.ok(csv.includes('http://127.0.0.1:18109/evidence#id=doc&revision=rev'));assert.match(csv,/"rev"/);assert.match(csv,/价格尚未核实/);assert.match(csv,/兼容性待确认/);assert.ok(csv.includes('"工具 A",""'));
 delete globalThis.location;
});

test('export keeps incomplete delivery and changes explicit',()=>{
 const result=deliveryMarkdown({run:{status:'partial'},result:{records:[],report:'已找到候选，仍需核对条款。'},comparison:{notice:'上次获取未完整成功'},changes:[{kind:'field_changed',field:'version',before:'1.0',after:'2.0'}]});
 assert.match(result,/部分完成/);assert.match(result,/已找到候选/);assert.match(result,/上次获取未完整成功/);assert.match(result,/1\.0 → 2\.0/);assert.match(result,/已导出 0 条/);
 globalThis.location={origin:'http://127.0.0.1:18109'};
 const saved=deliveryMarkdown({run:{goal:'版本核对',status:'completed'},result:{delivery_id:'saved',report:'官方版本已核对。',records:[],actions:['检查升级兼容性'],evidence_documents:[{available:true,title:'官方发布记录',snapshot_url:'/evidence#id=original&revision=v1',fetched_at:'2026-11-12T01:00:00Z'},{available:false,unavailable_reason:'来源已撤销'}]}});
 assert.match(saved,/官方版本已核对/);assert.match(saved,/original&revision=v1/);assert.match(saved,/检查升级兼容性/);assert.match(saved,/来源已撤销/);
 delete globalThis.location;
});
