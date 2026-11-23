import {test} from 'node:test';
import assert from 'node:assert/strict';
import {workspaceView} from './studio.mjs';
test('saved delivery columns and diagnostics are displayed without requalifying its rows',()=>{
 for(const field of ['release_date','annual_price']){
  const out={result:{tables:[{title:'候选',columns:['name',field],diagnostics:[{code:'FIELD_CONFLICT',field:'name'}]}],records:[{table_title:'候选',fields:{name:'A'}},{table_title:'候选',fields:{name:'B'}}],gaps:['尚待核实']}};
  const view=workspaceView(out);
  assert.deepEqual(view.columns,['name',field]);
  assert.deepEqual(view.missing,[field]);
  assert.deepEqual(view.records,out.result.records);
  assert.equal(view.conflicts,1);
  assert.deepEqual(view.limitations,['尚待核实']);
 }
 const preview=workspaceView({result:{records:[{fields:{name:'A',version:'1'}}],preview:true}});
 assert.deepEqual(preview.columns,['name','version']);
 assert.deepEqual(preview.missing,[]);
 assert.equal(preview.conflicts,0);
 const multiple={result:{tables:[{title:'版本',columns:['version']},{title:'升级事项',columns:['note']}],records:[{table_title:'版本',fields:{version:'1'}},{table_title:'升级事项',fields:{note:'升级前备份'}}]}};
 assert.deepEqual(workspaceView(multiple).columns,['version']);
 assert.deepEqual(workspaceView(multiple,'升级事项').records,[multiple.result.records[1]]);
});
