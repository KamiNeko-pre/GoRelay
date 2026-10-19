export function monitorOperators(type){return ({string:['eq'],integer:['eq','lt','lte','gt','gte'],number:['eq','lt','lte','gt','gte'],boolean:['eq'],string_array:['contains','eq']})[type]||[]}
export function parseMonitorValue(raw,type,operator){
 if(!monitorOperators(type).includes(operator))throw new Error('不支持的字段运算');
 const value=String(raw).trim();if(!value)throw new Error('填写条件值');
 if(type==='string'||operator==='contains')return value;
 if(type==='boolean'){if(value==='true')return true;if(value==='false')return false;throw new Error('布尔值填写 true 或 false')}
 if(type==='number'||type==='integer'){const number=Number(value);if(!Number.isFinite(number)||(type==='integer'&&!Number.isSafeInteger(number)))throw new Error('填写有效数值，整数不可带小数');return number}
 const members=JSON.parse(value);if(!Array.isArray(members)||members.some(x=>typeof x!=='string'||!x.trim()))throw new Error('填写字符串数组 JSON');return members;
}
export function addMonitorCondition(config,condition,kind){
 const next=structuredClone(config);next.conditions??=[];next.condition_groups??=[];
 if(kind==='mandatory')next.conditions.push(condition);else if(kind==='alternative')next.condition_groups.push([condition]);else throw new Error('未知条件分组');return next;
}
export function inboxState(item,now=new Date()){if(!item.verified||(item.valid_until&&new Date(item.valid_until)<=now))return '未确认当前有效';return item.active?'当前满足条件':'已不满足条件'}
// Cap the view, not acquisition or persisted evidence. An empty entity list
// remains open to new identities on every acquisition.
export function visibleMonitorItems(items,monitorID,limit=10){return items.filter(x=>x.monitor_id===monitorID).slice(0,limit)}
