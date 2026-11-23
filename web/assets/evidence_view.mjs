import { requestJSON } from './http.mjs';
import { displayField } from './task_activity.mjs';

const $ = id => document.getElementById(id);
let pending;
const date = value => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '';

function readableRecord(text) {
  try {
    const record = JSON.parse(text);
    if (record && typeof record === 'object' && !Array.isArray(record)) {
      return Object.entries(record).map(([key, value]) => displayField(key) + '：\n' + (typeof value === 'string' ? value : JSON.stringify(value, null, 2))).join('\n\n');
    }
  } catch { /* Partial quotations retain their exact original text. */ }
  return text;
}

function render({ evidence: entry, body }) {
	$('evidence-origin').textContent = entry.origin === 'extracted_record'
	  ? '这是一份整理后的数据记录；核对事实时，还应检查对应的原始网页内容与字段出处。'
	  : entry.origin === 'source_response' ? '以下内容来自实际获取并保存的来源响应。' : '以下内容来自已保存的资料。';
  $('title').textContent = entry.title || '保存的原文';
  $('passage-heading').textContent = entry.origin === 'extracted_record' ? '保存的数据记录' : '引用片段';
  $('passage').textContent = entry.origin === 'extracted_record' ? readableRecord(body) : entry.text;
  $('captured').textContent = entry.fetched_at ? '采集于 ' + date(entry.fetched_at) : '采集时间未记录';
  $('published').textContent = entry.published_at ? '原文发布于 ' + date(entry.published_at) : '';
  let original;
  try { original = new URL(entry.url); } catch { /* An unavailable URL is shown as missing, never guessed. */ }
  $('original').hidden = !original || !['http:', 'https:'].includes(original.protocol);
  if (!$('original').hidden) $('original').href = original.href;
  $('version-note').textContent = !entry.source_active
    ? '该来源已停用。这份历史快照仍可核对原回答，不代表当前情况。'
    : entry.current
      ? '这是目前保存的最新版本；请结合采集时间判断是否适用于当前问题。'
      : '来源已有更新。这份历史快照用于核对原回答；查询现状时请使用最新资料。';
  const chars = Array.from(body);
  const context = $('document');
  context.replaceChildren();
  if (Number.isInteger(entry.start) && Number.isInteger(entry.end) && entry.start >= 0 && entry.end <= chars.length && entry.end >= entry.start && chars.slice(entry.start, entry.end).join('') === entry.text) {
    const mark = document.createElement('mark');
    mark.textContent = entry.text;
    context.append(document.createTextNode(chars.slice(0, entry.start).join('')), mark, document.createTextNode(chars.slice(entry.end).join('')));
  } else {
    context.textContent = body;
  }
  $('identity').replaceChildren();
  for (const [name, value] of [['来源', entry.source_id], ['文档', entry.doc_id], ['内容版本', entry.revision], ['证据编号', entry.id], ['原文位置', entry.start + '–' + entry.end]]) {
    const term = document.createElement('dt');
    const detail = document.createElement('dd');
    term.textContent = name;
    detail.textContent = value || '未记录';
    $('identity').append(term, detail);
  }
  $('evidence').hidden = false;
  $('access-form').hidden = true;
  $('notice').hidden = true;
}

async function load(token) {
  pending?.abort();
  const request = new AbortController();
  pending = request;
  $('evidence').hidden = true;
  $('notice').hidden = false;
  $('notice').textContent = '正在读取保存的原文…';
  const params = new URLSearchParams(location.hash.slice(1));
  const id = params.get('id');
  const revision = params.get('revision');
  if (!id) {
    $('notice').textContent = '链接中缺少证据编号。请从回答中的引用链接重新打开。';
    return;
  }
  try {
    const options = { signal: request.signal };
    if (token !== undefined) options.token = token;
    const data = await requestJSON('/information/evidence/' + encodeURIComponent(id) + (revision ? '?revision=' + encodeURIComponent(revision) : ''), options);
    if (pending === request) render(data);
  } catch (error) {
    if (request.signal.aborted) return;
    if (error.status === 401) {
      $('notice').textContent = '这份资料需要服务访问权限。验证后，同一浏览器可继续打开其他引用。';
      $('access-form').hidden = false;
      $('access-token').focus();
    } else {
      $('notice').textContent = '无法读取原文：' + error.message;
    }
  }
}

$('access-form').addEventListener('submit', async event => {
  event.preventDefault();
  const button = event.currentTarget.querySelector('button');
  button.disabled = true;
  try { await load($('access-token').value); }
  finally { button.disabled = false; $('access-token').value = ''; }
});
window.addEventListener('hashchange', () => load());
load();
