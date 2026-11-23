import {displayField} from './task_activity.mjs';
import {safeLink,statusText} from './agent_helpers.mjs';
import {marked} from './vendor/marked.mjs';
import DOMPurify from './vendor/dompurify.mjs';

const valueText=value=>value===null||value===undefined?'未记录':typeof value==='object'?JSON.stringify(value):String(value);
const markdown=value=>valueText(value).replace(/[\r\n]+/g,' ').replace(/[\[\]<>]/g,'').replace(/\|/g,'\\|');

export function deliveryLink(value){
 if(typeof value==='string'&&value.startsWith('/')&&!value.startsWith('//')){
  try{return safeLink(new URL(value,globalThis.location.origin).href)}catch{return null}
 }
 return safeLink(value);
}

export function recordDocuments(record,field=''){
 const refs=(record.references||[]).filter(ref=>!field||!ref.field||ref.field===field);
 const selected=new Set(refs.map(ref=>ref.doc_id+'@'+ref.revision));
 return (record.evidence_documents||[]).filter(doc=>doc.available!==false&&(!field||!(record.references||[]).length||selected.has(doc.doc_id+'@'+doc.revision)));
}

export function recordReferences(record){
 const refs=recordDocuments(record).map(doc=>({url:deliveryLink(doc.snapshot_url)||safeLink(doc.url),label:'保存的原文',time:doc.fetched_at,original:doc.url})).filter(ref=>ref.url);
 const p=record.provenance;if(p&&safeLink(p.url))refs.push({url:p.url,label:'来源页面',time:p.fetched_at});
 return [...new Map(refs.map(ref=>[ref.url,ref])).values()];
}

export function deliveryCSV(records,columns,{gaps=[],diagnostics=[]}={}){
 const quoted=value=>{let text=value===null||value===undefined?'':typeof value==='object'?JSON.stringify(value):String(value);if(typeof value==='string'&&/^[=+@-]/.test(text))text="'"+text;return '"'+text.replaceAll('"','""')+'"'};
 const rows=[['结果表',...columns.map(displayField),'来源链接','保存原文链接','原文版本','获取时间','数据说明','未完成事项']];
 for(const record of records){
  const documents=recordDocuments(record),ids=new Set(record.observation_ids||[]);
  const notes=diagnostics.filter(d=>!(d.observation_ids||[]).length||(d.observation_ids||[]).some(id=>ids.has(id))).map(d=>(d.field?displayField(d.field)+'：':'')+d.message);
  for(const doc of record.evidence_documents||[])if(doc.available===false)notes.push(doc.unavailable_reason||'原文当前不可访问');
  rows.push([record.table_title,...columns.map(key=>record.fields?.[key]),documents.map(d=>d.url).filter(Boolean).join(' | '),documents.map(d=>deliveryLink(d.snapshot_url)).filter(Boolean).join(' | '),(record.evidence_documents||[]).map(d=>d.revision).filter(Boolean).join(' | '),documents.map(d=>d.fetched_at).filter(Boolean).join(' | '),notes.join('；'),gaps.join('；')]);
 }
 return rows.map(row=>row.map(quoted).join(',')).join('\r\n');
}

function answerReferences(out){
 return (out.result?.evidence_documents||[]).filter(doc=>doc.available!==false).map(doc=>({url:deliveryLink(doc.snapshot_url),label:doc.title||'保存依据',time:doc.fetched_at})).filter(ref=>ref.url);
}

export function deliveryOverview(out){
 const records=out.result?.records||[],result=out.result||{};
 const answer=result.report||'';
 const refs=records.flatMap(recordReferences);
 const sources=new Set([...refs.map(ref=>ref.original||ref.url),...(result.evidence_documents||[]).filter(doc=>doc.available!==false).map(doc=>doc.url||doc.snapshot_url)]);
 const limitations=(result.gaps||[]).map(valueText);
 const changes=result.changes||out.changes||[];
 return {records,answer,sourceCount:sources.size,savedCount:records.filter(r=>(r.evidence_documents||[]).some(d=>d.snapshot_url)).length,limitations,changes,comparison:result.comparison||out.comparison};
}

export function changeText(change){
 const kind={record_added:'新增记录',record_removed:'移除记录',field_changed:'字段变化',field_added:'新增字段',field_removed:'移除字段'}[change.kind]||'变化';
 let identity='';try{identity=JSON.parse(change.key).map(valueText).join(' · ')}catch{}
 return (change.table?change.table+' · ':'')+(identity?identity+' · ':'')+kind+(change.field?' · '+displayField(change.field):'')+'：'+valueText(change.before)+' → '+valueText(change.after);
}

export function deliveryMarkdown(out,{records=out.result?.records||[],note=''}={}){
 const overview=deliveryOverview({...out,result:{...out.result,records}}),run=out.run||{};
 const lines=['# '+markdown(run.goal||'GoRelay 信息整理结果'),'','状态：'+markdown(run.status?statusText(run.status):'已保存数据，任务状态见原获取记录'),''];
 if(overview.answer)lines.push('## 本次结论','',String(overview.answer),'');
 const answerRefs=answerReferences(out);if(answerRefs.length)lines.push('## 保存的引用依据','',...answerRefs.map(ref=>'- ['+ref.label+'](<'+ref.url+'>)'+(ref.time?'；获取于 '+ref.time:'')),'');
 const baseline=(out.result?.baseline_evidence_documents||[]).filter(doc=>doc.available!==false&&deliveryLink(doc.snapshot_url));if(baseline.length)lines.push('## 上次检查的原文依据','',...baseline.map(doc=>'- ['+markdown(doc.title||'上次保存的原文')+'](<'+deliveryLink(doc.snapshot_url)+'>)'+(doc.fetched_at?'；获取于 '+doc.fetched_at:'')),'');
 lines.push('## 本次成果','',`已导出 ${records.length} 条记录；以下时间来自实际保存的来源。`,'');
 if(note.trim())lines.push('## 我的行动清单','',...note.split(/\r?\n/).filter(Boolean).map(line=>'- [ ] '+line),'');
 if(out.result?.actions?.length)lines.push('## 后续检查事项','',...out.result.actions.map(line=>'- [ ] '+markdown(line)),'');
 const unavailable=[...(out.result?.evidence_documents||[]),...(out.result?.baseline_evidence_documents||[])].filter(doc=>doc.available===false);if(unavailable.length)lines.push('## 引用访问状态','',...unavailable.map(doc=>'- '+markdown(doc.unavailable_reason||'该依据当前不可访问')),'');
 if(overview.limitations.length)lines.push('## 需要确认','',...overview.limitations.map(text=>'- '+text),'');
 if(overview.comparison?.notice)lines.push('## 变化比较','',overview.comparison.notice,'');
 if(overview.comparison&&!overview.comparison.notice&&!overview.changes.length)lines.push('## 变化比较','','与上次获取相比，保存的字段没有变化。','');
 if(overview.changes.length)lines.push('## 变化与检查事项','',...overview.changes.map(change=>'- [ ] '+markdown(changeText(change))+'；核对这项变化对当前方案的影响。'),'');
 records.forEach((record,index)=>{
  const fields=record.fields||{};
  const title=fields.name||fields.title||fields.component||fields.plan_name||fields.provider||`结果 ${index+1}`;
  lines.push('### '+markdown(title),'',...Object.entries(fields).map(([key,value])=>'- '+markdown(displayField(key))+'：'+markdown(value)));
  const refs=recordReferences(record);
  if(refs.length)lines.push(...refs.map(ref=>'- '+ref.label+'：['+markdown(ref.original||ref.url)+'](<'+ref.url+'>)'+(ref.time?'；获取于 '+markdown(ref.time):'')));
  else lines.push('- 此条记录尚无可打开的来源链接。');
  lines.push('');
 });
 return lines.join('\n');
}

export function downloadDelivery(out,options={}){
 const blob=new Blob([deliveryMarkdown(out,options)],{type:'text/markdown;charset=utf-8'});
 const href=URL.createObjectURL(blob),a=document.createElement('a');
 a.href=href;a.download='GoRelay-结果与行动清单.md';a.click();setTimeout(()=>URL.revokeObjectURL(href),1000);
}

export function renderDeliveryOverview(out){
 const info=deliveryOverview(out),root=document.createElement('section');root.className='delivery-overview';root.setAttribute('aria-label','结果摘要');
 const card=(title,text)=>{const el=document.createElement('article'),h=document.createElement('h3'),p=document.createElement('p');h.textContent=title;p.textContent=text;el.append(h,p);return el;};
 const resultCard=card('本次结果',info.records.length?`已整理 ${info.records.length} 条结果，保留 ${info.sourceCount} 个来源入口。`:info.answer?`已生成查询报告，保留 ${info.sourceCount} 份原文依据。`:'正在整理本次结果。');
 const highlights=document.createElement('ul');
 for(const record of info.records.slice(0,3)){
  const fields=Object.entries(record.fields||{}).filter(([,value])=>value!==null&&typeof value!=='object'&&valueText(value).length<120&&!safeLink(value));
  const item=document.createElement('li');item.textContent=fields.slice(0,3).map(([key,value])=>displayField(key)+'：'+valueText(value)).join(' · ');if(item.textContent)highlights.append(item);
 }
 resultCard.append(highlights);root.append(resultCard);
 root.append(card('变化摘要',info.comparison?.notice|| (info.comparison?info.changes.length?`与上次获取相比有 ${info.changes.length} 项变化，具体内容和两次依据见下方。`:'与上次获取相比，保存的字段没有变化。':'首次获取后，可在「来源与资料」中更新并比较前后变化。')));
 if(info.comparison){const details=document.createElement('details'),summary=document.createElement('summary'),list=document.createElement('ul');summary.textContent='查看前后变化与上次依据';details.append(summary);for(const change of info.changes){const item=document.createElement('li');item.textContent=changeText(change);list.append(item)}details.append(list);for(const doc of out.result?.baseline_evidence_documents||[]){const row=document.createElement('p'),href=deliveryLink(doc.snapshot_url);if(doc.available!==false&&href){const link=document.createElement('a');link.href=href;link.textContent=doc.title||'上次检查的原文';row.append(link,document.createTextNode(doc.fetched_at?' · '+doc.fetched_at:''))}else row.textContent=doc.unavailable_reason||'上次依据当前不可访问';details.append(row)}root.append(details)}
 const pending=card('需要确认',info.limitations.length?`有 ${info.limitations.length} 项说明，请在下方查看具体内容。`:'尚未记录额外待确认事项。可查看原始依据，并在结果备注中补充你的下一步行动。');
 if(info.limitations.length){const details=document.createElement('details'),summary=document.createElement('summary'),list=document.createElement('ul');summary.textContent='查看待确认事项';for(const text of info.limitations){const item=document.createElement('li');item.textContent=text;list.append(item)}details.append(summary,list);pending.append(details)}root.append(pending);
 if(info.answer){
  const details=document.createElement('details'),summary=document.createElement('summary'),body=document.createElement('div');details.className='delivery-analysis';details.open=true;summary.textContent='查看完整结论与来源说明';body.className='delivery-markdown';body.innerHTML=DOMPurify.sanitize(marked.parse(info.answer),{USE_PROFILES:{html:true}});const refs=document.createElement('p');for(const ref of answerReferences(out)){const link=document.createElement('a');link.href=ref.url;link.textContent=ref.label;refs.append(link,document.createTextNode('　'))}body.append(refs);details.append(summary,body);root.append(details);
 }
 return root;
}
