import {requestJSON} from './http.mjs';
import {initStudio} from './studio.mjs';
import {initSourceWorkspace} from './source_workspace.mjs';
import{jsonText,safeLink,isTerminal,statusText,loopRoundTitle}from './agent_helpers.mjs';
import{initMonitorUI}from './monitor_ui.mjs';
const $=id=>document.getElementById(id);let selected='',timer=null,viewVersion=0,runFlight=null,creatingKey=null;
$('token').value=sessionStorage.getItem('gorelay-admin-token')||'';
$('token').addEventListener('change',()=>{sessionStorage.setItem('gorelay-admin-token',$('token').value);guard(async()=>{await connect();if($('token').value)await monitorUI.refresh()})});
const show=(id,value)=>{$(id).textContent=jsonText(value)};
function notice(message,error=false){$('notice').textContent=message;$('notice').dataset.error=String(error)}
function api(path,{method='GET',data}={}){return requestJSON(path,{method,token:$('token').value,body:data===undefined?undefined:JSON.stringify(data)})}
const taskKinds=new Map();
async function taskApi(path,options){if(!/^\/agent\/runs(?:\/|$)/.test(path))return api(path,options);const id=path.split('/')[3];if(id&&taskKinds.get(id)==='crawler')return api(path,options);try{const out=await api(path.replace('/agent/runs','/information/studio/runs'),options);if(id)taskKinds.set(id,'information');return out}catch(e){if(id&&e.httpStatus===404){taskKinds.set(id,'crawler');return api(path,options)}throw e}}
async function guard(task,button){if(button)button.disabled=true;try{await task()}catch(error){notice(error.message,true)}finally{if(button)button.disabled=false}}
async function listRuns(){const rows=await taskApi('/agent/runs');$('runs').replaceChildren(new Option('选择任务',''));for(const run of rows)$('runs').add(new Option(`${statusText(run.status)} · ${run.goal.slice(0,45)}`,run.id));$('runs').value=selected;studio.renderRuns(rows)}
function renderLoop(loop){$('loop-rounds').replaceChildren();if(!loop||!loop.rounds.length){$('loop-summary').textContent='尚无 Agent Loop 推进记录。旧任务不补造历史。';return}const summary=loop.summary;$('loop-summary').textContent=`当前展示 ${loop.rounds.length} 次推进：模型调用步骤 ${summary.model_advances}，工具执行步骤 ${summary.tool_advances}，未结束 ${summary.incomplete_advances}，完成请求被拒绝 ${summary.completion_rejections}。推进步骤与实际模型请求分别计数。${loop.history_truncated?'历史窗口已截断，以上不是全任务统计。':''}`;for(const[ordinal,round]of loop.rounds.entries()){const card=document.createElement('article');card.className='loop-round';const title=document.createElement('strong');title.textContent=`${ordinal+1}. ${loopRoundTitle(round)} · ${statusText(round.status_before)} → ${round.status_after?statusText(round.status_after):'未知'}`;card.append(title);const reason=document.createElement('p');reason.textContent=round.reason||round.error||'本次没有记录动作理由';card.append(reason);for(const proposal of round.proposed_tools||[]){const row=document.createElement('div');row.textContent=`模型提议 ${proposal.name} · ${proposal.call_id}`;card.append(row)}for(const observation of round.tools){const row=document.createElement('div');row.textContent=`${observation.name} · ${observation.call_id} · ${observation.error?'失败：'+observation.error:'已返回观察'}`;card.append(row)}const details=document.createElement('details'),label=document.createElement('summary'),facts=document.createElement('pre');label.textContent='需求缺口、预算与状态事实';facts.textContent=jsonText({coverage_before:round.coverage_before,coverage_after:round.coverage_after,reserved_budget_before:round.reserved_budget_before,reserved_budget_after:round.reserved_budget_after,duration_ms:round.duration_ms,completion_rejected:round.completion_rejected,error:round.error});details.append(label,facts);card.append(details);$('loop-rounds').append(card)}}
async function loadRun(){
 if(!selected)return;const id=selected,version=viewVersion;
 if(runFlight?.id===id&&runFlight.version===version)return runFlight.promise;
 const promise=(async()=>{const out=await taskApi('/agent/runs/'+encodeURIComponent(id));if(version!==viewVersion||selected!==id)return;
 lastTaskSnapshot={id,out};studio.renderRun(out); const run=out.run;$('run-status').textContent=`${statusText(run.status)} · ${run.id}${run.last_error?' · '+run.last_error:''}`;show('coverage',out.requirements||'尚无明确的输出要求');show('plan',{plan:out.plan||'正在理解任务',progress:out.progress||null});if($('task-diagnostics').open&&diagnosticTask!==id)await loadDiagnostics(true);
 $('children').replaceChildren();for(const child of out.children){const div=document.createElement('div');div.className='child';div.textContent=`#${child.task.id} ${child.task.status} · ${child.task.url}${child.task.last_error?' · '+child.task.last_error:''}`;$('children').append(div)}
 $('active-task').classList.remove('is-disconnected');$('active-task').querySelector('[data-connection-lost]')?.remove();
 if(isTerminal(run.status)){clearInterval(timer);timer=null}
 })();
 runFlight={id,version,promise};
 try{return await promise}catch(error){if(selected===id){$('active-caption').textContent='连接中断 · 下方为上次进展';$('active-task').classList.add('is-disconnected');$('active-task').querySelector('[data-connection-lost]')?.remove();const p=document.createElement('p');p.dataset.connectionLost='true';p.className='status-alert';p.textContent='暂时无法读取最新进展：'+error.message+'。连接恢复后将继续查询原任务。';$('active-task').prepend(p)}throw error}finally{if(runFlight?.promise===promise)runFlight=null}
}
let diagnosticTask='',diagnosticCursor=null,diagnosticData=null,lastTaskSnapshot=null,diagnosticFlight=null;
async function loadDiagnostics(reset=false){
 if(!selected)return;const id=selected;
 if(diagnosticFlight?.id===id)return diagnosticFlight.promise;
 const cursor=reset||diagnosticTask!==id?null:diagnosticCursor;
 const data=reset||diagnosticTask!==id?{routing_steps:[],operations:[],traces:[]}:diagnosticData;
 const promise=(async()=>{
  const query=new URLSearchParams({limit:'5',...(cursor||{})});
  const information=taskKinds.get(id)==='information';
  const page=information?await api('/information/studio/runs/'+encodeURIComponent(id)+'/details?'+query):await api('/agent/runs/'+encodeURIComponent(id));
  if(selected!==id)return;
  diagnosticTask=id;diagnosticData=data;diagnosticCursor=page.next||null;
  for(const key of ['routing_steps','operations','traces'])data[key].push(...(page[key]||[]));
  show('metrics',{routing_steps:data.routing_steps,operations:data.operations});show('traces',data.traces);renderLoop(page.loop);
  if(lastTaskSnapshot?.id===id)show('result',lastTaskSnapshot.out.result||'尚无已保存的结果');
  $('diagnostics-more').disabled=!page.has_more;$('diagnostics-status').textContent='已载入 '+data.routing_steps.length+' 条调度记录、'+data.operations.length+' 条工具与模型记录；仅按需读取。';
 })();
 diagnosticFlight={id,promise};
 try{return await promise}finally{if(diagnosticFlight?.promise===promise)diagnosticFlight=null;}
}
$('task-diagnostics').addEventListener('toggle',()=>{if($('task-diagnostics').open&&diagnosticTask!==selected)guard(()=>loadDiagnostics(true));});
$('diagnostics-refresh').onclick=()=>guard(()=>loadDiagnostics(true),$('diagnostics-refresh'));
$('diagnostics-more').onclick=()=>guard(()=>loadDiagnostics(false),$('diagnostics-more'));
function follow(id){viewVersion++;selected=id;clearInterval(timer);timer=setInterval(()=>guard(loadRun),2500);return loadRun()}
$('goal').addEventListener('input',()=>{creatingKey=null});$('scope').addEventListener('input',()=>{creatingKey=null});
$('goal-form').addEventListener('submit',e=>{e.preventDefault();guard(async()=>{creatingKey??=crypto.randomUUID();const request={request_key:creatingKey,goal:$('goal').value.trim()};const domains=$('scope').value.split(',').map(x=>x.trim()).filter(Boolean);if(domains.length)request.source_scope=domains;const run=await taskApi('/agent/runs',{method:'POST',data:request});await follow(run.id);await listRuns();notice('任务已受理。正在理解你的目标，来源和进展会在这里更新。')},$('create'))});
$('refresh').onclick=()=>guard(async()=>{await listRuns();await loadRun()},$('refresh'));
$('runs').onchange=()=>guard(()=>follow($('runs').value));
for(const action of ['cancel','advance','resume'])$(action).onclick=()=>guard(async()=>{if(!selected)throw new Error('先选择任务');await taskApi(`/agent/runs/${encodeURIComponent(selected)}/${action}`,{method:'POST',data:{}});await follow(selected);notice('状态已更新')},$(action));
$('sources-refresh').onclick=()=>guard(async()=>show('sources',{registered:await api('/agent/sources'),discovered:await api('/information/discovery/sources')}),$('sources-refresh'));
$('quarantine-form').onsubmit=e=>{e.preventDefault();guard(async()=>{await api(`/agent/sources/${encodeURIComponent($('source-id').value)}/quarantine`,{method:'POST',data:{version:$('source-version').value,reason:$('source-reason').value}});show('sources',await api('/agent/sources'));notice('源已隔离，相关当前知识已停用')})};
async function refreshKnowledge(){const[docs,work]=await Promise.all([api('/knowledge/documents'),api('/knowledge/index-work')]);show('documents',docs);const id=$('doc-id').value.trim();const attempts=id?await api('/knowledge/index-attempts?doc_id='+encodeURIComponent(id)):'填写 Document ID 后查看逐次用量和阶段耗时';show('index-work',{work,attempts})}
$('knowledge-refresh').onclick=()=>guard(refreshKnowledge,$('knowledge-refresh'));
for(const[action,id]of [['retry','index-retry'],['invalidate','invalidate']])$(id).onclick=()=>guard(async()=>{if(!$('doc-id').value)throw new Error('填写 Document ID');await api(`/knowledge/documents/${encodeURIComponent($('doc-id').value)}/${action}`,{method:'POST',data:{}});await refreshKnowledge();notice('索引状态已更新')},$(id));
$('import-form').onsubmit=e=>{e.preventDefault();guard(async()=>{await api('/knowledge/documents',{method:'POST',data:JSON.parse($('import-json').value)});await refreshKnowledge();notice('正文已保存，索引后台处理')})};
$('question-form').onsubmit=e=>{e.preventDefault();guard(async()=>{const out=await api('/knowledge/query',{method:'POST',data:{question:$('question').value.trim()}});show('answer',out);$('evidence').replaceChildren();for(const ev of out.result.evidence.entries){const card=document.createElement('article');card.className='evidence';const title=document.createElement('strong');title.textContent=`${ev.id} · ${ev.title}`;card.append(title);const href=safeLink(ev.url);if(href){const link=document.createElement('a');link.href=href;link.target='_blank';link.rel='noopener noreferrer';link.textContent=' 查看原文';card.append(link)}const p=document.createElement('p');p.textContent=ev.text;card.append(p);$('evidence').append(card)}notice('查询完成；引用快照已保存')},$('ask'))};
window.addEventListener('pagehide',()=>clearInterval(timer));
async function connect(){if(!$('token').value){$('connection').textContent='尚未连接';notice('首次使用：点击右上角「本地空间」，填写管理令牌后连接服务。');return}$('connection').textContent='正在连接本地空间';try{await api('/information/sources?page_size=1');$('connection').textContent='本地空间已连接';sourceWorkspace.route();try{await listRuns()}catch(error){notice('已连接来源与资料；任务列表暂不可用：'+error.message,true);return}notice('已连接。可以创建任务，或打开来源与资料查看保存的内容。')}catch(error){$('connection').textContent='连接中断';throw error}}
const studio=initStudio({api:taskApi,guard,follow,notice});
const sourceWorkspace=initSourceWorkspace({api,guard,notice});
const monitorUI=initMonitorUI({api,guard,notice,follow:id=>{taskKinds.set(id,'information');location.hash='tasks';return follow(id)},openResult:async(id,title)=>{taskKinds.set(id,'information');await follow(id);await studio.openWorkspace(id,title)}});
guard(async()=>{await connect();if($('token').value)await monitorUI.refresh()});
