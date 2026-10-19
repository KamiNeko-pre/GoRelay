export const jsonText=value=>typeof value==='string'?prettyJSON(value):JSON.stringify(value,null,2);
export function prettyJSON(value){try{return JSON.stringify(JSON.parse(value),null,2)}catch{return String(value??'')}}
export function safeLink(value){try{const url=new URL(value);return ['http:','https:'].includes(url.protocol)?url.href:null}catch{return null}}
export function isTerminal(status){return ['completed','partial','failed','cancelled','expired'].includes(status)}
export function statusText(status){return ({planning:'等待规划',reviewing:'审查观察与下一步',waiting:'等待子任务',blocked_core:'等待核心函数实现',blocked_configuration:'等待服务配置',needs_input:'需要补充输入',completed:'完成',partial:'部分完成',failed:'失败',cancelled:'已取消',expired:'已超时'})[status]||status}
export function loopRoundTitle(round){if(round.stage!=='finished')return '推进未结束，结果未知';if(round.status_after==='blocked_core')return '等待核心函数实现';if(round.completion_rejected)return '完成请求被拒绝，仍有缺口';return ({model:'模型调用与动作提议',tools:'工具执行与反馈',wait:'等待结果',finish:'完成条件检查',monitor_submit:'复用程序提交采集',monitor_finish:'新事实完成条件检查',monitor_replan:'复用失效，返回 Agent 调查'})[round.action]||'推进停止'}
