import {safeLink,statusText} from './agent_helpers.mjs';
import {displayField} from './task_activity.mjs';
import {workspaceCell} from './studio.mjs';
import {downloadDelivery,renderDeliveryOverview,deliveryLink,changeText} from './delivery_export.mjs';

const node=(tag,text,cls)=>{const el=document.createElement(tag);if(text!==undefined)el.textContent=String(text);if(cls)el.className=cls;return el;};
const button=(label,run,cls='text-button')=>{const el=node('button',label,cls);el.type='button';el.onclick=async()=>{el.disabled=true;try{await run()}finally{el.disabled=false}};return el;};
const date=value=>value?new Date(value).toLocaleString('zh-CN'):'尚未获取';
const text=value=>value===null||value===undefined?'—':typeof value==='object'?JSON.stringify(value):String(value);
const finished=job=>job.terminal||['completed','succeeded','partial','failed','cancelled','expired'].includes(job.status);
const link=(label,href)=>{const a=node('a',label,'text-button');a.href=deliveryLink(href);a.target='_blank';a.rel='noopener noreferrer';return a;};

export function sourcePresentation(source){
 let host='';try{host=new URL(source.entry_url).hostname.replace(/^www\./,'')}catch{}
 const fieldNames=Object.keys(source.output_schema?.items?.properties||{}).filter(key=>key!=='source_url');
 const preferred=['title','name','provider','plan_name','free_plan_name','version','price_annual_cny','release_date'];
 fieldNames.sort((a,b)=>{const ai=preferred.indexOf(a),bi=preferred.indexOf(b);return (ai<0?preferred.length:ai)-(bi<0?preferred.length:bi)});
 const fields=fieldNames.map(displayField).filter(label=>/\p{Script=Han}/u.test(label));
 const originalTitle=source.title||'';
 const search=source.role==='source_discovery'||Boolean(source.input_schema?.properties?.query&&fieldNames.includes('title')&&fieldNames.includes('url'));
 const title=search?'网页搜索结果':/\p{Script=Han}/u.test(originalTitle)?originalTitle:fields.length?fields.slice(0,2).join('、')+(fields.length>2?'等资料':'资料'):source.role==='candidate'?'来自 '+(host||'公开网站')+' 的网页':(host||'本地来源')+' 的保存资料';
 const description=search?'保存搜索到的文章标题和链接，可用新关键词继续查找。':fields.length?'可获取：'+fields.slice(0,4).join('、')+(fields.length>4?`等 ${fields.length} 项信息`:''):source.role==='candidate'?'尚未保存内容，可交给任务继续了解。':source.can_execute?'已验证获取方式，可在新任务中再次使用。':'内容已保存在本地，可查看并用于后续任务。';
 return {title,host,description,originalTitle};
}

export function initSourceWorkspace({api,guard,notice}){
 const root=document.getElementById('source-workspace');
 let sources=[],catalogCursor='',query='',catalogView='saved',selectedSource=null,selectedJob=null,poll=null,revision=0;
 let jobs=[],historyCursor='',rows=[],dataCursor='',dataTotal=0,difference=null,pollFlight=false;
 let listRoot,detailRoot,jobRoot,dataRoot,diffRoot,historyRoot,tabsRoot;
 const requestKeys=new Map();
 const notes=new Map();
 const path=(base,params={})=>base+'?'+new URLSearchParams(Object.entries(params).filter(([,v])=>v!==undefined&&v!==''));

 function showError(container,error){container.append(node('p',error.message||String(error),'status-alert'));}
 async function loadSources(append=false){
  if(!root)return;
  if(!append){sources=[];catalogCursor='';root.replaceChildren();const heading=node('div',undefined,'section-heading');const copy=node('div');copy.append(node('h1','来源与资料'),node('p','找回任务保存的内容，查看依据，或沿用已有来源获取新信息。','muted'));heading.append(copy);root.append(heading);
   const form=node('form',undefined,'source-search'),input=node('input');input.type='search';input.placeholder='搜索资料名称或网站';input.setAttribute('aria-label','搜索来源与资料');input.value=query;const submit=node('button','搜索','primary');submit.type='submit';form.append(input,submit);form.onsubmit=e=>{e.preventDefault();query=input.value.trim();guard(()=>loadSources())};root.append(form);tabsRoot=node('div',undefined,'source-view-tabs');tabsRoot.setAttribute('aria-label','资料分类');root.append(tabsRoot);listRoot=node('div',undefined,'source-catalog');detailRoot=node('section',undefined,'source-detail');root.append(listRoot,detailRoot);
  }
  const page=await api(path('/information/sources',{query,view:catalogView,cursor:append?catalogCursor:''}));
  sources.push(...page.sources||[]);catalogCursor=page.next_cursor||'';
  if(!append){for(const [key,label]of [['saved','已保存的资料'],['reusable','可更新的来源'],['discovered','发现的网页']]){const tab=button(label+(page.counts?` ${page.counts[key]||0}`:''),()=>guard(async()=>{catalogView=key;history.replaceState(null,'','#sources');await loadSources()}));tab.setAttribute('aria-pressed',String(catalogView===key));tabsRoot.append(tab);}}
  drawSources(page.total_sources);
  const [sourcePath,jobQuery]=location.hash.replace(/^#(?:sources|library)\/?/,'').split('?'),wanted=decodeURIComponent(sourcePath);
  if(!append&&wanted){
   let source=sources.find(s=>s.source_id===wanted);
   if(!source){const matches=await api(path('/information/sources',{query:wanted}));source=(matches.sources||[]).find(s=>s.source_id===wanted);}
   if(source)await openSource(source,new URLSearchParams(jobQuery).get('job'));
  }
 }

 function drawSources(total){
  listRoot.replaceChildren();listRoot.append(node('p',({saved:'这些内容已保存在本地。打开资料即可查看，不需要重新采集。',reusable:'这些网站已接入获取方式，可以在任务中复用；新的获取记录可更新并比较。',discovered:'这些是任务发现的网页入口，尚未全部验证或保存。'})[catalogView],'source-view-explainer'));
  listRoot.append(node('p',`共 ${total||0} 项${catalogCursor?`，当前显示 ${sources.length} 项`:''}`,'workspace-summary'));
  if(!sources.length){const empty=node('div',undefined,'empty-state');empty.append(node('h2','从第一项调查开始积累'),node('p','完成任务后，可复用的来源与保存的资料会出现在这里。'),link('去创建任务','/agent#tasks'));listRoot.append(empty);return;}
  for(const source of sources){
   const card=node('article',undefined,'source-card'),copy=node('div',undefined,'source-card-copy'),presentation=sourcePresentation(source),title=presentation.title;
   const heading=node('h2',title);heading.title=title;
   if(presentation.host&&presentation.host!==title)copy.append(node('span',presentation.host,'source-host'));
   copy.append(heading,node('p',presentation.description,'source-purpose'));
   const count=source.saved_documents||0;
   copy.append(node('p',count?`${count} 份保存内容 · ${date(source.latest_capture_at)}`:'尚无保存内容','workspace-summary'));
   const controls=node('div',undefined,'source-card-actions');
   if(count)controls.append(button('打开资料',()=>guard(()=>openSource(source)),'button primary'));
   else if(source.can_execute)controls.append(button('查看来源',()=>guard(()=>openSource(source))));
   const reuse=button(source.can_execute?'用此来源创建任务':'交给任务继续了解',()=>{document.getElementById('goal').value=`请根据${source.entry_url||title}整理我需要的信息，保留来源并保存结果。`;document.getElementById('goal').dispatchEvent(new Event('input'));location.hash='tasks';document.getElementById('goal').focus();});controls.append(reuse);
   card.append(copy,controls);listRoot.append(card);
  }
  if(catalogCursor)listRoot.append(button('加载更多来源',()=>guard(()=>loadSources(true))));
 }

 async function openSource(source,wantedJob){
  stopPolling();const current=++revision;selectedSource=source;selectedJob=null;difference=null;jobs=[];rows=[];
  history.replaceState(null,'','#sources/'+encodeURIComponent(source.source_id));
  detailRoot.replaceChildren();detailRoot.append(node('h2',sourcePresentation(source).title));
  const sourceInfo=node('details',undefined,'source-technical');sourceInfo.append(node('summary','网站与接入详情'));if(source.title)sourceInfo.append(node('p','原始名称：'+source.title));if(safeLink(source.entry_url))sourceInfo.append(link('访问原网站 ↗',source.entry_url));sourceInfo.append(node('p',source.entry_url||'未记录原网页地址'),node('pre',source.description||''));detailRoot.append(sourceInfo);
  jobRoot=node('div',undefined,'source-job');historyRoot=node('div',undefined,'source-history');diffRoot=node('section',undefined,'source-differences');dataRoot=node('div',undefined,'source-data');const docsRoot=node('section',undefined,'saved-source-documents');
  detailRoot.append(jobRoot,historyRoot,diffRoot,dataRoot,docsRoot);jobRoot.append(node('p','正在读取保存的获取记录…','muted'));
  const outcomes=await Promise.allSettled([api(path('/information/source-jobs',{source_id:source.source_id})),api(path('/information/source-documents',{source_id:source.source_id}))]);
  if(current!==revision)return;
  if(outcomes[0].status==='fulfilled'){const page=outcomes[0].value;jobs=page.jobs||[];historyCursor=page.next_cursor||'';drawHistory();
   let job=jobs.find(j=>j.job_id===wantedJob);
   if(wantedJob&&!job)job=await api('/information/source-jobs/'+encodeURIComponent(wantedJob));
   if(job||jobs.length)await openJob(job||jobs[0]);else{jobRoot.replaceChildren(node('p','以下展示该来源保存的历史数据与原文，每条保留获取时间。要获取新数据，可在任务页使用该来源。','workspace-summary'));await loadData(false);}
  }else{jobRoot.replaceChildren();showError(jobRoot,outcomes[0].reason);}
  if(current!==revision)return;
  if(outcomes[1].status==='fulfilled')drawDocuments(docsRoot,outcomes[1].value,source.source_id);else showError(docsRoot,outcomes[1].reason);
  detailRoot.scrollIntoView({block:'start',behavior:'smooth'});
 }

 function drawDocuments(container,page,sourceID,append=false){
  if(!append)container.replaceChildren(node('h3','本地保存的资料'));
  container.querySelector('[data-more-docs]')?.remove();
  for(const doc of page.documents||[]){const row=node('article',undefined,'saved-document');row.append(link(doc.title||'查看保存原文',doc.snapshot_url),node('span',date(doc.fetched_at),'muted'));if(safeLink(doc.url))row.append(link('原网站 ↗',doc.url));container.append(row);}
  if(!page.total_documents&&!append)container.append(node('p','当前没有可读取的原文快照。上方已保存的数据不受影响。','muted'));
  if(page.next_cursor){const more=button('加载更多保存原文',()=>guard(async()=>{const next=await api(path('/information/source-documents',{source_id:sourceID,cursor:page.next_cursor}));drawDocuments(container,next,sourceID,true)}));more.dataset.moreDocs='true';container.append(more);}
 }

 function drawHistory(){
  historyRoot.replaceChildren(node('h3','获取记录'));
  for(const job of jobs){const choose=button(`${date(job.created_at)} · ${statusText(job.status)} · ${job.records_saved||0} 条`,()=>guard(()=>openJob(job)));choose.classList.toggle('selected',selectedJob?.job_id===job.job_id);historyRoot.append(choose);}
  if(historyCursor)historyRoot.append(button('更早的记录',()=>guard(async()=>{const page=await api(path('/information/source-jobs',{source_id:selectedSource.source_id,cursor:historyCursor}));jobs.push(...page.jobs||[]);historyCursor=page.next_cursor||'';drawHistory();})));
 }

 async function openJob(job){
  stopPolling();selectedJob=job;rows=[];dataCursor='';dataTotal=0;difference=null;diffRoot.replaceChildren();drawJob();drawHistory();await loadData(false);
  history.replaceState(null,'','#sources/'+encodeURIComponent(selectedSource.source_id)+'?job='+encodeURIComponent(job.job_id));
  if(selectedJob?.job_id!==job.job_id)return;
  if(finished(job))await loadDifference();else poll=setInterval(()=>guard(pollJob),2500);
 }

 async function pollJob(){
  if(pollFlight)return;
  if(!selectedJob||!/^#sources(?:\/|$)/.test(location.hash)){stopPolling();return;}
  pollFlight=true;
  const id=selectedJob.job_id;try{const job=await api('/information/source-jobs/'+encodeURIComponent(id));if(selectedJob?.job_id!==id)return;selectedJob=job;drawJob();if(finished(job)){stopPolling();await loadData(false);await loadDifference();}}catch(error){jobRoot.querySelector('[data-connection]')?.remove();const message=node('p','连接中断：'+error.message+'。已保存结果仍在，连接恢复后会继续查询。','status-alert');message.dataset.connection='true';jobRoot.append(message);}finally{pollFlight=false;}
 }

 function drawJob(){
  const job=selectedJob;if(!job)return;jobRoot.replaceChildren();
  jobs=jobs.map(saved=>saved.job_id===job.job_id?job:saved);drawHistory();
  const heading=node('div',undefined,'section-heading');heading.append(node('h3',statusText(job.status)),node('span',`已保存 ${job.records_saved||0} 条 · 最近进展 ${date(job.updated_at)}`,'muted'));jobRoot.append(heading);
  const action=job.activities?.at(-1);
  const progress=finished(job)?job.status==='succeeded'?'获取结束，结果已保存，可查看原文或再次更新。':'本次获取已停止，已保存的结果仍可查看。':action?.error?'最近一次操作遇到问题，正在等待执行链路处理。':action?'正在执行来源获取；已完成的结果会持续保存。':'已进入获取队列，等待开始执行。';
  jobRoot.append(node('p',progress,'source-current-action'));
  if(job.error)jobRoot.append(node('p',job.error,'status-alert'));
  for(const step of job.failure?.recovery_steps||[])jobRoot.append(node('p',step,'workspace-summary'));
  const controls=node('div',undefined,'source-card-actions');controls.append(button('重新读取进展',()=>guard(pollJob)));
  if(finished(job)&&selectedSource?.can_execute){const update=button('更新一次',()=>guard(()=>refreshJob(job,update)),'button primary');controls.append(update);}
  if(!finished(job))controls.append(button('停止本次获取',()=>guard(async()=>{const result=await api('/information/source-jobs/'+encodeURIComponent(job.job_id)+'/cancel',{method:'POST',data:{}});selectedJob=result;drawJob();})));
  jobRoot.append(controls);
  const details=node('details');details.append(node('summary','技术详情'));details.append(node('pre',JSON.stringify({job_id:job.job_id,source_id:job.source_id,program_version:job.version,refresh_id:job.refresh_id,tasks:job.tasks,activities:job.activities},null,2)));jobRoot.append(details);
 }

 async function refreshJob(job,control){
  control.disabled=true;let key=requestKeys.get(job.job_id);if(!key){key=crypto.randomUUID();requestKeys.set(job.job_id,key);}
  try{const result=await api('/information/source-refreshes',{method:'POST',data:{previous_job_id:job.job_id,request_key:key}});const next=result.job;requestKeys.delete(job.job_id);if(!jobs.some(x=>x.job_id===next.job_id))jobs.unshift(next);await openJob(next);notice('已提交新一轮获取；沿用已有来源与程序，结果保存后可对比变化。');}finally{control.disabled=false;}
 }

 async function loadData(append){
  if(!selectedSource)return;const id=selectedJob?.job_id,sourceID=selectedSource.source_id;
  const page=await api(path('/information/source-data',{job_id:id,source_id:id?undefined:sourceID,cursor:append?dataCursor:''}));if(selectedJob?.job_id!==id||selectedSource?.source_id!==sourceID)return;
  rows=append?[...rows,...page.records||[]]:page.records||[];dataCursor=page.next_cursor||'';dataTotal=page.total_records||0;drawData();
 }

 function drawData(){
  const oldScroll=dataRoot.querySelector('.workspace-scroll'),position={top:oldScroll?.scrollTop||0,left:oldScroll?.scrollLeft||0};dataRoot.replaceChildren();
  const out={run:{goal:sourcePresentation(selectedSource).title+'（本次来源获取）',status:selectedJob?.status},result:{records:rows},changes:difference?.changes||[],comparison:difference};dataRoot.append(renderDeliveryOverview(out));
  const noteLabel=node('label','我的行动清单','source-action-note'),noteInput=node('textarea');noteInput.rows=2;noteInput.placeholder='下一步要核对或处理什么？每行一项，导出时会成为待办清单。';noteInput.setAttribute('aria-label','我的行动清单');
  const noteKey='gorelay-source-note:'+(selectedJob?.job_id||selectedSource.source_id);
  if(!notes.has(noteKey)){try{notes.set(noteKey,localStorage.getItem(noteKey)||'')}catch{notes.set(noteKey,'')}}
  noteInput.value=notes.get(noteKey);noteInput.oninput=()=>{notes.set(noteKey,noteInput.value);try{localStorage.setItem(noteKey,noteInput.value)}catch{notice('当前浏览器无法保存备注；本页仍可编辑并导出。',true)}};noteLabel.append(noteInput,node('small','备注仅保存在当前浏览器，不改变采集结果。','muted'));dataRoot.append(noteLabel);
  const controls=node('div',undefined,'source-card-actions');controls.append(button('导出全部结果与行动清单',()=>guard(async()=>{const id=selectedJob?.job_id,sourceID=selectedSource.source_id,all=[];let cursor='';do{const page=await api(path('/information/source-data',{job_id:id,source_id:id?undefined:sourceID,cursor,page_size:100}));all.push(...page.records||[]);cursor=page.next_cursor||'';}while(cursor);downloadDelivery(out,{records:all,note:notes.get(noteKey)||''});})));
  if(dataCursor)controls.append(button('加载更多结果',()=>guard(()=>loadData(true))));
  controls.append(button('重新载入结果',()=>guard(()=>loadData(false))));dataRoot.append(controls,node('p',`已显示 ${rows.length} / ${dataTotal} 条保存结果${dataCursor?'，继续加载可查看其余结果。':'。'}`,'workspace-summary'));
  if(!rows.length){dataRoot.append(node('p',!selectedJob||finished(selectedJob)?'尚无保存数据。可先查看下方原文，或在任务页使用该来源。':'正在获取，第一批结果保存后会在这里显示。','muted'));return;}
  const scroll=node('div',undefined,'workspace-scroll'),table=node('table'),head=node('thead'),tr=node('tr');const keys=[...new Set(rows.flatMap(r=>Object.keys(r.fields||{})))];
  for(const key of [...keys,'来源与依据','获取时间'])tr.append(node('th',displayField(key)));head.append(tr);table.append(head);const body=node('tbody');
  for(const record of rows){const row=node('tr');for(const key of keys)row.append(workspaceCell(record.fields?.[key],key));const refs=node('td');for(const doc of record.evidence_documents||[]){if(deliveryLink(doc.snapshot_url))refs.append(link(doc.origin==='extracted_record'?'保存的数据记录':'查看保存原文',doc.snapshot_url),node('br'));}if(safeLink(record.provenance?.url))refs.append(link('访问原网页 ↗',record.provenance.url));row.append(refs,node('td',date(record.provenance?.fetched_at)));body.append(row);}
  table.append(body);scroll.append(table);dataRoot.append(scroll);scroll.scrollTop=position.top;scroll.scrollLeft=position.left;
 }

 async function loadDifference(){
  if(!selectedJob)return;const after=selectedJob.job_id,before=selectedJob.previous_job_id||jobs.find(j=>j.job_id!==after&&new Date(j.created_at)<new Date(selectedJob.created_at))?.job_id;
  diffRoot.replaceChildren();if(!before)return;
  difference=await api(path('/information/source-differences',{before,after}));if(selectedJob?.job_id!==after)return;
  diffRoot.append(node('h3','与上次获取相比'));
  if(difference.notice){diffRoot.append(node('p',difference.notice,'workspace-summary'));drawData();return;}
  diffRoot.append(node('p',`${date(difference.before.created_at)} → ${date(difference.after.created_at)}`,'muted'));
  if(!difference.changes.length)diffRoot.append(node('p','本次获取完整成功，保存的字段没有变化。'));
  for(const change of difference.changes){const row=node('article',undefined,'change-entry');row.append(node('p',changeText(change)));diffRoot.append(row);}
  const evidence=node('div',undefined,'source-card-actions');evidence.append(button('查看上次依据',()=>guard(async()=>openJob(await api('/information/source-jobs/'+encodeURIComponent(before))))),button('查看本次依据',()=>dataRoot.scrollIntoView({block:'start',behavior:'smooth'})));diffRoot.append(evidence);
  drawData();
 }

 function stopPolling(){clearInterval(poll);poll=null;}
 function route(){if(/^#(?:sources|library)(?:\/|$)/.test(location.hash))guard(()=>loadSources());else stopPolling();}
 window.addEventListener('hashchange',route);window.addEventListener('pagehide',stopPolling);
 return {refresh:()=>loadSources(),route};
}
