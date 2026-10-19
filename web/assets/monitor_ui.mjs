import {jsonText,safeLink} from './agent_helpers.mjs';
import {monitorOperators,parseMonitorValue,addMonitorCondition,inboxState,visibleMonitorItems} from './monitor_helpers.mjs';

export function initMonitorUI({api,guard,notice,follow}){
 const $=id=>document.getElementById(id);
 const node=(tag,text)=>{const n=document.createElement(tag);n.textContent=text;return n};
 let selected=null,creatingKey=null,revision=0,definition=null;
 const retryKeys=new Map();
 function action(label,task,disabled=false){const b=node('button',label);b.type='button';b.disabled=disabled;b.onclick=()=>guard(task,b);return b}
 async function refreshInbox(){
  const items=await api('/agent/inbox');const out=$('monitor-inbox');out.replaceChildren();
  if(!selected){out.append(node('p','先选择持续任务'));return}
  const all=items.filter(x=>x.monitor_id===selected.id),visible=visibleMonitorItems(items,selected.id);
  out.append(node('p',`展示最近 ${visible.length} 条，最多 10 条。采集证据与历史不受展示数量限制。`));
  if(!all.length)out.append(node('p','尚无符合条件的机会。首次刷新完成后在这里查看。'));
  for(const item of visible){
   const card=document.createElement('article');card.className='evidence';
   card.append(node('strong',`${inboxState(item)} · ${({unread:'未读',read:'已看',ignored:'已忽略'})[item.status]||item.status}`));
   const facts=document.createElement('dl');
   for(const [key,value] of Object.entries(item.fields||{}))facts.append(node('dt',key),node('dd',typeof value==='object'?jsonText(value):String(value)));
   card.append(facts);const evidence=document.createElement('details');evidence.append(node('summary','来源证据与变化'));
   for(const source of item.sources||[]){
    const line=node('p',`${source.source_id} · ${source.fetched_at}`),href=safeLink(source.url);
    if(href){const link=node('a',' 查看来源');link.href=href;link.target='_blank';link.rel='noopener noreferrer';line.append(link)}evidence.append(line);
   }
   evidence.append(node('pre',jsonText(item.changes)));card.append(evidence);
   for(const [status,label] of [['read','已看'],['ignored','忽略']])card.append(action(label,async()=>{await api(`/agent/inbox/${item.id}`,{method:'PATCH',data:{status,observed_run_id:item.run_id}});await refreshInbox()},item.status===status));
   out.append(card);
  }
 }
 function fillOperators(){const type=selected?.config.spec?.required_fields.find(x=>x.name===$('condition-field').value)?.type;
  $('condition-operator').replaceChildren();for(const op of monitorOperators(type))$('condition-operator').add(new Option(({eq:'等于',lt:'小于',lte:'小于等于',gt:'大于',gte:'大于等于',contains:'包含'})[op],op));
  $('condition-value').placeholder=type==='boolean'?'true / false':type==='string_array'?'contains 填成员；eq 填 JSON 数组':'按字段单位填值；0 有效';
 }
 async function saveConfig(config){await api(`/agent/monitors/${selected.id}`,{method:'PATCH',data:{config}});await load(selected.id);notice('条件已保存，下次刷新生效')}
 function renderConditions(){const out=$('monitor-conditions');out.replaceChildren();const c=selected.config;
  out.append(node('p','必需条件全部满足；如果配置了“或者”，至少满足其中一组。字段单位以任务合同为准。'));
  const render=(label,conditions,remove)=>{const row=document.createElement('div');row.append(node('span',`${label}：${conditions.map(x=>`${x.field} ${x.operator} ${JSON.stringify(x.value)}`).join(' 且 ')}`));row.append(action('删除',remove,Boolean(selected.active_run_id)));out.append(row)};
  (c.conditions||[]).forEach((condition,i)=>render('必需',[condition],()=>{const next=structuredClone(c);next.conditions.splice(i,1);return saveConfig(next)}));
  (c.condition_groups||[]).forEach((group,i)=>render(`或者 ${i+1}`,group,()=>{const next=structuredClone(c);next.condition_groups.splice(i,1);return saveConfig(next)}));
  const fields=c.spec?.required_fields||[];$('condition-field').replaceChildren();for(const field of fields)$('condition-field').add(new Option(`${field.name} (${field.type})`,field.name));fillOperators();
  $('condition-add').disabled=!fields.length||Boolean(selected.active_run_id);
  if(!fields.length)out.append(node('p','首次任务完成并冻结字段合同后，才能配置类型明确的条件。'));
 }
 async function load(id){const current=++revision;const view=await api(`/agent/monitors/${id}`);if(current!==revision)return;selected=view;
  $('monitor-details').hidden=false;$('monitors').value=String(view.id);$('monitor-heading').textContent=view.goal;
  $('monitor-state').textContent=`${view.enabled?'周期调度已启用':'周期调度暂停'} · ${view.execution_plan_ready?'已保存可复用采集程序':'下次刷新由 Agent 调查'}${view.active_run_id?' · 正在执行 '+view.active_run_id:''}${view.last_error?' · '+view.last_error:''}`;
  $('monitor-toggle').textContent=view.enabled?'暂停周期任务':'启用周期任务';$('monitor-refresh').disabled=Boolean(view.active_run_id);
  $('monitor-contract').textContent=jsonText(view.config.spec||'尚未冻结字段合同');renderConditions();
  $('monitor-checks').replaceChildren();for(const check of view.checks)$('monitor-checks').append(action(`${check.mode==='plan'?'复用采集':'Agent 调查'} · ${check.run_id}${check.fallback_reason?' · '+check.fallback_reason:''}`,()=>follow(check.run_id)));
  await refreshInbox();
 }
 async function refresh(){const views=await api('/agent/monitors');$('monitors').replaceChildren(new Option('选择持续任务',''));for(const view of views)$('monitors').add(new Option(`${view.enabled?'运行':'暂停'} · ${view.goal.slice(0,48)}`,String(view.id)));
  const id=selected?.id||views[0]?.id;if(id&&views.some(x=>x.id===id))await load(id);
 }
 for(const id of ['monitor-goal','monitor-scope','monitor-interval'])$(id).addEventListener('input',()=>{creatingKey=null;if(definition){definition=null;notice('已改为手填目标，导入的字段与条件已清除；需要时重新导入配置')}});
 $('monitor-definition').onchange=()=>guard(async()=>{const file=$('monitor-definition').files[0];if(!file)return;const parsed=JSON.parse(await file.text());if(typeof parsed.goal!=='string'||!parsed.goal.trim())throw new Error('配置必须包含 goal');definition=parsed;creatingKey=null;$('monitor-goal').value=parsed.goal;$('monitor-scope').value=(parsed.source_scope||[]).join(',');$('monitor-interval').value=(parsed.interval_seconds||86400)/3600;notice('配置已加载，创建时一并提交字段合同与条件；尚未执行任务')});
 $('monitor-form').onsubmit=e=>{e.preventDefault();guard(async()=>{
  creatingKey??=crypto.randomUUID();const request=definition?{...definition,request_key:creatingKey}:{request_key:creatingKey,goal:$('monitor-goal').value.trim(),source_scope:$('monitor-scope').value.split(',').map(x=>x.trim()).filter(Boolean),interval_seconds:Number($('monitor-interval').value)*3600};
  const m=await api('/agent/monitors',{method:'POST',data:request});await refresh();await load(m.id);notice('持续任务已创建，周期调度暂停。手动刷新才会开始执行。');
 },$('monitor-create'))};
 $('monitors-refresh').onclick=()=>guard(refresh,$('monitors-refresh'));
 $('monitors').onchange=()=>{if($('monitors').value)guard(()=>load($('monitors').value))};
 $('monitor-toggle').onclick=()=>guard(async()=>{await api(`/agent/monitors/${selected.id}`,{method:'PATCH',data:{enabled:!selected.enabled}});await load(selected.id);notice('周期调度状态已更新')},$('monitor-toggle'));
 $('monitor-refresh').onclick=()=>guard(async()=>{
  const id=selected.id;let key=retryKeys.get(id);if(!key){key=crypto.randomUUID();retryKeys.set(id,key)}
  const run=await api(`/agent/monitors/${id}/refresh`,{method:'POST',data:{request_key:key}});retryKeys.delete(id);await load(id);await follow(run.id);notice('已提交刷新，执行记录在任务区查看；完成后刷新待办列表');
 },$('monitor-refresh')).finally(()=>{$('monitor-refresh').disabled=Boolean(selected?.active_run_id)});
 $('condition-field').onchange=fillOperators;
 $('condition-form').onsubmit=e=>{e.preventDefault();guard(async()=>{const field=selected.config.spec.required_fields.find(x=>x.name===$('condition-field').value);const condition={field:field.name,operator:$('condition-operator').value,value:parseMonitorValue($('condition-value').value,field.type,$('condition-operator').value)};await saveConfig(addMonitorCondition(selected.config,condition,$('condition-kind').value))})};
 return {refresh};
}
