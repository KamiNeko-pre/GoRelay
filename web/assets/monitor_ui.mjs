import {statusText} from './agent_helpers.mjs';
import {changeText} from './delivery_export.mjs';

export function initMonitorUI({api,guard,notice,follow,openResult}){
 const $=id=>document.getElementById(id);
 const node=(tag,text)=>{const element=document.createElement(tag);if(text!==undefined)element.textContent=text;return element};
 const date=value=>value?new Date(value).toLocaleString('zh-CN'):'尚未安排';
 let selected=null,baseline='',creatingKey=null,revision=0;
 const refreshKeys=new Map();
 const action=(label,task)=>{const button=node('button',label);button.type='button';button.onclick=()=>guard(task,button);return button};
 async function inbox(offset=0){
  const root=$('monitor-inbox');if(!selected){root.replaceChildren(node('p','选择持续任务后，查看每轮检查结果。'));return}
  const id=selected.id,page=await api('/agent/inbox?'+new URLSearchParams({monitor_id:id,offset,limit:20}));
  if(selected?.id!==id)return;
  if(!offset)root.replaceChildren();root.querySelector('[data-more]')?.remove();
  if(!page.items.length&&!offset)root.append(node('p','尚无检查记录。点击「现在检查一次」开始，完成后会保存结果和变化。'));
  for(const item of page.items){
   const card=node('article');card.className='evidence';card.append(node('strong',statusText(item.outcome)+' · '+date(item.created_at)),node('p',item.summary||'本轮未能完成，已有资料和执行原因已保留。'));
   if(item.comparison?.notice)card.append(node('p',item.comparison.notice));
   const changes=item.comparison?.changes||[];
   if(changes.length){const details=node('details'),list=node('ul');details.append(node('summary','查看 '+changes.length+' 项变化'));for(const change of changes)list.append(node('li',changeText(change)));details.append(list);card.append(details)}
   card.append(action(item.delivery_id?'打开本次结果':'查看执行原因',()=>item.delivery_id?openResult(item.request_id,selected.goal):follow(item.request_id)));
   if(item.baseline_delivery_id)card.append(action('查看上次依据',async()=>{const before=await api('/information/deliveries/'+encodeURIComponent(item.baseline_delivery_id));await openResult(before.request_id,before.goal)}));
   card.append(action(item.status==='read'?'标为未读':'标为已读',async()=>{await api('/agent/inbox/'+item.id,{method:'PATCH',data:{status:item.status==='read'?'unread':'read'}});await inbox()}));
   root.append(card);
  }
  if(page.next_offset!==undefined){const more=action('查看更多检查记录',()=>inbox(page.next_offset));more.dataset.more='true';root.append(more)}
 }
 async function load(id){
  const current=++revision,view=await api('/agent/monitors/'+id);if(current!==revision)return;
  selected=view;$('monitor-details').hidden=false;$('monitors').value=String(view.id);$('monitor-heading').textContent=view.goal;
  $('monitor-state').textContent=(view.enabled?'周期检查已开启，下次 '+date(view.next_run_at):'周期检查已暂停，可随时手动检查')+(view.active_request_id?' · 当前检查进行中':'')+(view.last_error?' · 上轮未完成：'+view.last_error:'');
  $('monitor-toggle').textContent=view.enabled?'暂停周期检查':'开启周期检查';$('monitor-refresh').disabled=Boolean(view.active_request_id);
  $('monitor-focus-edit').value=view.config.focus||'';$('monitor-interval-edit').value=view.config.interval_seconds/3600;
  const checks=$('monitor-checks');checks.replaceChildren();
  for(const check of view.checks||[])checks.append(action(date(check.created_at)+' · '+statusText(check.outcome||'running'),()=>check.delivery_id?openResult(check.request_id,view.goal):follow(check.request_id)));
  await inbox();
 }
 async function refresh(){
  const rows=await api('/agent/monitors');$('monitors').replaceChildren(new Option('选择持续任务',''));
  for(const row of rows)$('monitors').add(new Option((row.enabled?'运行':'暂停')+' · '+row.goal,String(row.id)));
  const id=selected?.id||rows[0]?.id;if(id&&rows.some(row=>row.id===id))await load(id);
 }
 for(const id of ['monitor-goal','monitor-scope','monitor-focus','monitor-interval'])$(id).addEventListener('input',()=>{creatingKey=null});
 $('monitor-form').onsubmit=event=>{event.preventDefault();guard(async()=>{
  creatingKey??=crypto.randomUUID();const input={request_key:creatingKey,goal:$('monitor-goal').value.trim(),focus:$('monitor-focus').value.trim(),interval_seconds:Math.round(Number($('monitor-interval').value)*3600),source_scope:$('monitor-scope').value.split(',').map(value=>value.trim()).filter(Boolean)};
  if(baseline){input.delivery_id=baseline;if(!input.source_scope.length)delete input.source_scope}
  const monitor=await api('/agent/monitors',{method:'POST',data:input});await refresh();await load(monitor.id);notice('持续任务已保存。可先手动检查，确认后开启周期检查。');
 },$('monitor-create'))};
 $('monitor-clear-baseline').onclick=()=>{baseline='';creatingKey=null;$('monitor-baseline').textContent='首次检查会保存一份新结果，作为后续对比依据。'};
 window.addEventListener('gorelay:watch-delivery',event=>{baseline=event.detail.delivery_id;creatingKey=null;$('monitor-goal').value=event.detail.goal||'';$('monitor-baseline').textContent='已选择当前保存结果作为对比基线。';location.hash='monitors';$('monitor-focus').focus()});
 $('monitors-refresh').onclick=()=>guard(refresh,$('monitors-refresh'));
 $('monitors').onchange=()=>{if($('monitors').value)guard(()=>load($('monitors').value))};
 $('monitor-toggle').onclick=()=>guard(async()=>{await api('/agent/monitors/'+selected.id,{method:'PATCH',data:{enabled:!selected.enabled}});await load(selected.id)},$('monitor-toggle'));
 $('monitor-settings').onsubmit=event=>{event.preventDefault();guard(async()=>{const config={...selected.config,focus:$('monitor-focus-edit').value.trim(),interval_seconds:Math.round(Number($('monitor-interval-edit').value)*3600)};await api('/agent/monitors/'+selected.id,{method:'PATCH',data:{config}});await load(selected.id);notice('关注重点和检查频率已保存，后续检查生效。')},$('monitor-save'))};
 $('monitor-refresh').onclick=()=>guard(async()=>{
  const id=selected.id;let key=refreshKeys.get(id);if(!key){key=crypto.randomUUID();refreshKeys.set(id,key)}
  const task=await api('/agent/monitors/'+id+'/refresh',{method:'POST',data:{request_key:key}});refreshKeys.delete(id);await load(id);await follow(task.id);notice('本轮检查已开始，沿用任务中心的规划、获取和交付流程。');
 },$('monitor-refresh'));
 window.addEventListener('hashchange',()=>{if(location.hash==='#monitors')guard(refresh)});
 return {refresh};
}
