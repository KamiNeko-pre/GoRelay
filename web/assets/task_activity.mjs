import {isTerminal,statusText,safeLink} from './agent_helpers.mjs';
const parse=v=>{if(v&&typeof v==='object')return v;try{return JSON.parse(v)||{}}catch{return {}}};
export const displayField=name=>({release_scope:'发布范围',security_fixes:'安全修复',cve_ids:'CVE 编号',component:'组件',observed_version:'核实版本',baseline_record_date:'上次核对日期',change_type:'变化类型',advisory_id:'安全公告编号',affected_versions:'受影响版本',fixed_version:'修复版本',baseline_version:'当前使用版本',baseline_date:'上次核对日期',item_type:'变化类型',security_id:'安全公告编号',affected_range:'受影响版本',check_action:'建议检查事项',priority:'优先级',evidence_quote:'来源摘录',product:'产品',stable_version:'稳定版本',installation_url:'安装入口',source_text:'来源原文',release_date:'发布日期',published_at:'发布时间',install_url:'安装入口',official_url:'官方网站',free_plan_name:'免费方案',free_allowance:'免费额度',major_limits:'主要限制',annual_price:'年付价格',currency:'币种',price:'价格',status:'状态',support:'支持情况',provider:'服务商',plan_name:'套餐',region:'地区',price_annual_cny:'年付价格（元）',memory_gb:'内存（GB）',cpu_cores:'CPU 核数',disk_gb:'硬盘（GB）',bandwidth:'带宽 / 流量（原文）',residential_ip_upgrade:'住宅 IP 升级',claude_ban_reports:'Claude 封号反馈',proxy_use_policy:'代理使用条款',ip_purity:'IP 纯净度',purchase_url:'购买链接',source_url:'来源页面',title:'标题',url:'原文链接',description:'简介',platforms:'支持平台',name:'名称',version:'版本'})[name]||name;
const labels={update_plan:'安排调查计划',query_observations:'查看已取得的数据',query_documents:'查看已保存的资料',build_view:'整理和对比结果',link_materials:'采用已有资料',list_assets:'查找可复用能力',submit_delivery:'检查并保存交付结果',search_saved_materials:'查找本地资料',read_saved_material:'读取保存的资料',acquire_sources:'采集选定来源',discover_sources:'发现并验证来源',lookup_sources:'查找已发现的来源',request_browser:'评估来源访问限制',ask_user:'等待补充信息',inspect_api_candidate:'检查 API 来源',search_api:'搜索候选来源',trial_api:'验证 API 获取方案',trial_browser:'验证浏览器获取方案',inspect_browser:'检查浏览器来源',execute_asset:'运行已有能力',inspect_source:'检查页面结构',read_data:'读取选择器匹配结果',list_sources:'检查可复用能力',validate_program:'试运行采集程序',register_source:'保存已验证能力',submit_acquisition:'提交正式采集',worker_attempt:'执行采集程序',get_task_results:'读取采集结果',evaluate_result:'检查字段与来源证据',finish_task:'提交任务结果',index_embedding:'建立检索索引'};
function toolDetail(args,data){
 const parts=[];
 for(const key of ['query','url','selector','rows_selector']){
  const value=args[key]??data[key];
  if(typeof value==='string'&&value)parts.push(({query:'查询',url:'来源',selector:'选择器',rows_selector:'行选择器'})[key]+'：'+value);
 }
 if(Array.isArray(data.records))parts.push('返回 '+data.records.length+' 条记录');
 if(Array.isArray(data.items))parts.push('返回 '+data.items.length+' 项匹配');
 if(data.coverage&&typeof data.coverage.satisfied==='boolean')parts.push(data.coverage.satisfied?'字段与证据检查通过':'字段或证据仍有缺口');
 return parts.join(' · ');
}
export function taskActivity(out){
 const events=Array.isArray(out.activity)?out.activity.map(e=>({...e,title:labels[e.tool]||({assistant:'已制定下一步方案',tool:'已取得工具结果',interrupt:'进度已保存，等待后续执行',framework:'执行任务'})[e.kind]||e.title,detail:e.detail==='acquisition'?'':e.detail})):[],calls=new Map(),run=out.run||{};
 const hasInformationEvents=events.some(e=>e.id?.startsWith('information:'));
 const all=[...(hasInformationEvents?[]:out.routing_steps||[]).map(x=>({...x,origin:'host'})),...(events.length?[]:out.operations||[]).map(x=>({...x,origin:'crawler'}))];
 for(const op of all){
  const result=parse(op.result_json);
  for(const call of result.message?.tool_calls||[])calls.set(call.id,{name:call.function?.name,args:parse(call.function?.arguments)});
 }
 for(const op of all){
  const result=parse(op.result_json),input=parse(op.input_json);
  if(op.kind==='route_semantic'){
   if(result.repeated)events.push({id:op.origin+':'+op.id,time:op.updated_at,title:'检测到重复决策',detail:'已将重复信号交给纠偏机制，下一步需要改变行为并取得新证据。',state:'warning'});
   continue;
  }
  if(op.kind==='index_embedding')continue;
  let title=labels[op.kind]||'执行工具：'+op.kind,detail='',failure=op.error||result.error||result.failure?.message||'';
  if(op.kind==='model'||op.kind==='eino_model'){
   const proposed=result.message?.tool_calls||[];
   title='分析资料并安排下一步';
   if(proposed.length)detail='已提出：'+proposed.map(c=>labels[c.function?.name]||c.function?.name).join('、');
   else if(op.stage==='reserved')detail='等待模型返回；此时还没有新的执行结果。';
   if(result.finish_reason==='length')failure='模型响应被截断，未执行不完整的动作。';
  }else{
   const call=calls.get(result.tool_call_id)||{};
   detail=toolDetail(call.args||{},parse(result.data));
   if(!detail&&input.url)detail='来源：'+input.url;
  }
  let state=op.stage==='failed'||failure?'failed':op.stage==='reserved'?'pending':'done';
  if(state==='pending'&&(isTerminal(run.status)||run.status==='blocked'||run.status?.startsWith('blocked_')))state='unknown';
  events.push({id:op.origin+':'+op.id,time:op.stage==='reserved'?op.created_at:op.updated_at||op.created_at,title,detail,failure:failure?String(failure):'',state});
 }
 for(const child of out.children||[]){
  const t=child.task||{};
  if(!t.id)continue;
  events.push({id:'task:'+t.id,time:t.updated_at||t.created_at,title:t.status==='succeeded'?'来源采集完成':t.status==='failed'?'来源采集失败':'采集来源数据',detail:safeLink(t.url)||child.link?.source_id||'',failure:t.last_error||'',state:t.status==='succeeded'?'done':t.status==='failed'?'failed':isTerminal(run.status)?'unknown':'pending'});
 }
 events.sort((a,b)=>(Date.parse(b.time)||0)-(Date.parse(a.time)||0)||b.id.localeCompare(a.id));
 const closed=new Set(),retained=[];
 for(const event of events){
  const family=event.kind==='model_started'||event.kind==='assistant'?'model':event.kind==='tool_started'||event.kind==='tool'?'tool:'+event.tool:null;
  const lane=family?event.id.split(':')[0]+':'+family:null;
  if(event.state==='pending'&&lane&&closed.has(lane))continue;
  if(lane&&(event.state==='done'||event.state==='failed'))closed.add(lane);
  if(event.state==='pending'&&(isTerminal(run.status)||run.status==='blocked'||run.status?.startsWith('blocked_')||run.status==='needs_input'))event.state='unknown';
  retained.push(event);
 }
 events.splice(0,events.length,...retained);
 const active=run.status==='running'?(events.find(e=>e.state==='pending')||null):null;
 let headline=active?.title||statusText(run.status);
 if(run.status==='failed')headline='任务未完成，已停止';
 let message='';
 if(run.status==='failed')message='已有结果和错误记录已保留，可展开查看具体原因。';
 else if(run.status==='blocked'||run.status?.startsWith('blocked_'))message='当前没有继续执行。需要先处理阻碍，再恢复任务。';
 else if(run.status==='completed')message='结果已检查，可以打开工作台进行筛选、对比和导出。';
 else if(run.status==='correcting')message='上次动作未通过。纠偏机制将结合错误原因调整方案。';
 else if(active)message=active.detail||'正在执行这一步，等待工具回执。';
 else if(run.status==='waiting')message='等待采集作业返回；下方保留最近的实际动作。';
 else if(run.status==='needs_input')message=out.question||'需要你补充一项会影响候选范围的信息；当前未继续执行。';
 else if(run.status==='queued'||run.status==='planning')message='等待调度；执行开始后会在这里逐步显示动作和结果。';
 return {events,active,headline,message,lastTime:events[0]?.time||run.created_at};
}

const element=(tag,text,cls)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n};
const timestamp=v=>new Date(v).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',second:'2-digit'});
export function renderTaskActivity(out){
 const a=taskActivity(out),root=element('section',undefined,'task-activity');
 root.setAttribute('aria-label','实际执行进展');
 const head=element('div',undefined,'activity-heading');
 head.append(element('span','当前进展','eyebrow'),element('span',statusText(out.run.status),'activity-state '+out.run.status));
 root.append(head,element('h3',a.headline,'activity-title'),element('p',a.message,'activity-message'));
 const plan=out.plan;
 if(plan?.interpretation){const block=element('div',undefined,'activity-plan');block.append(element('strong','这次要完成什么'),element('p',plan.interpretation));if(plan.coverage_rationale)block.append(element('p',plan.coverage_rationale));const actions=plan.actions||[];if(actions.length){const list=element('ol');for(const action of actions)list.append(element('li',action.goal));block.append(list)}root.append(block)}
 const updated=element('p','最近一次反馈 '+timestamp(a.lastTime),'activity-updated');
 if(a.active?.time){
  const seconds=Math.max(0,Math.floor((Date.now()-new Date(a.active.time))/1000));
  updated.append(element('span','本步已等待 '+(seconds<60?seconds+' 秒':Math.floor(seconds/60)+' 分 '+seconds%60+' 秒')));
 }
 root.append(updated);
 const makeEvent=e=>{
  const item=element('li',undefined,'activity-event '+e.state),time=element('time',timestamp(e.time)),body=element('div',undefined,'activity-event-body'),title=element('div',undefined,'activity-event-title');
  time.dateTime=e.time||'';
  title.append(element('strong',e.title),element('span',({done:'已完成',failed:'未通过',pending:out.run.status==='running'?'执行中':'等待回执',unknown:'结果待核对',warning:'已提醒纠偏'})[e.state],'activity-event-state'));
  body.append(title);
  if(e.detail)body.append(element('p',e.detail));
  if(e.failure){const error=element('details',undefined,'activity-error');error.append(element('summary','查看具体错误'),element('pre',e.failure));body.append(error)}
  item.append(time,body);return item;
 };
 const list=element('ol',undefined,'activity-list');
 for(const e of a.events.slice(0,5))list.append(makeEvent(e));
 root.append(list);
 if(a.events.length>5){const history=element('details',undefined,'activity-history'),older=element('ol',undefined,'activity-list');history.dataset.history='true';history.append(element('summary','查看更早的 '+(a.events.length-5)+' 条记录'));for(const e of a.events.slice(5))older.append(makeEvent(e));history.append(older);root.append(history)}
 if(out.run.last_error){
  const error=element('details',undefined,'activity-error task-error');error.dataset.error='true';
  error.append(element('summary',isTerminal(out.run.status)?'任务停止原因与原始错误':'上一步错误与处理建议'),element('pre',out.run.last_error));
  const steps=out.run.operational_status?.recovery||[];
  if(steps.length){const list=element('ol');for(const step of steps)list.append(element('li',step));error.append(list)}
  root.append(error);
 }
 return root;
}
