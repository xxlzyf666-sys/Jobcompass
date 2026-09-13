const { PreparationUI } = await import(document.querySelector('#preparation-module').href);
const { BillingUI } = await import(document.querySelector('#billing-module').href);
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const escapeHTML = (value = '') => String(value).replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char]));
const labels = { supported: '已有依据', needs_detail: '待补充', not_found: '未体现', pending: '排队中', running: '分析中', done: '已完成', failed: '未完成' };
const state = { config: null, example: null, diagnosis: null, recovery: null, filter: 'all', epoch: 0, pollTimer: null, toastTimer: null, pdfEpoch: 0, pdfTask: null, submitting: false, deletingID: null };
const views = ['editor', 'progress', 'report', 'history', 'error', 'preparation', 'billing'];

function toast(message) {
  clearTimeout(state.toastTimer);
  $('#toast').textContent = message;
  $('#toast').hidden = false;
  state.toastTimer = setTimeout(() => { $('#toast').hidden = true; }, 4600);
}

function showError(element, message = '') { element.textContent = message; element.hidden = !message; }
function dateText(seconds, includeTime = false) {
  const options = { year: 'numeric', month: '2-digit', day: '2-digit' };
  if (includeTime) Object.assign(options, { hour: '2-digit', minute: '2-digit' });
  return new Intl.DateTimeFormat('zh-CN', options).format(new Date(seconds * 1000));
}

async function api(path, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 18000);
  try {
    const headers = { Accept: 'application/json', ...options.headers };
    if (options.body) headers['Content-Type'] = 'application/json';
    if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = state.config?.csrf || '';
    const response = await fetch(path, { ...options, headers, credentials: 'same-origin', cache: 'no-store', signal: controller.signal });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) { const error = new Error(data.error || '请求未完成，请稍后再试。'); error.status = response.status; error.code = data.code; throw error; }
    return data;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('请求超时，请检查网络后重试。');
    if (error instanceof TypeError) throw new Error('暂时无法连接，请检查网络后重试。');
    throw error;
  } finally { clearTimeout(timer); }
}

async function bootstrap() {
  try {
    state.config = await api('/api/bootstrap');
    const { ready, provider, region, retention_hours: hours } = state.config;
    $('#service-notice').textContent = ready ? '' : '目前为体验预览：可以查看完整示例，或体验 PDF 文字提取。真实诊断暂未开放。';
    $('#service-notice').hidden = ready;
    $('#submit-diagnosis').disabled = !ready;
    $('#submit-diagnosis span:first-child').textContent = ready ? '开始逐项诊断' : '真实诊断暂未开放';
    const retention = hours % 24 === 0 ? `${hours / 24} 天` : `${hours} 小时`;
    $('#retention-note').textContent = `材料、报告及求职准备资料保留 ${retention}，可随时删除。引用会核对来源，模型解读仍需你确认。`;
    $('#privacy-provider').textContent = provider;
    $('#privacy-region').textContent = region;
    $('#privacy-retention').textContent = retention;
    updateBillingHints();
  } catch (error) {
    $('#service-notice').textContent = `${error.message} 刷新页面可重新连接；示例报告仍可尝试打开。`;
    $('#service-notice').hidden = false;
    $('#submit-diagnosis').disabled = true;
  }
}

function updateBillingHints() {
  if (!state.config) return;
  $('#account-nav').textContent = state.config.account ? '账号与次数' : '登录 / 次数';
  $('#diagnosis-cost').hidden = !state.config.billing_enabled;
  $('#diagnosis-cost').innerHTML = '<span>本次诊断预占 1 次，生成成功后扣除；最终失败自动退回。</span><a href="'+(state.config.account ? '#account' : '#account/login?next=start')+'">'+(state.config.account ? '查看次数 / 购买' : '登录后使用')+' ↗</a>';
}

async function accountChanged(mode) {
  preparationUI.stop();
  if (mode === 'logout' || state.config?.account) preparationUI.drafts.clear();
  preparationUI.data = null;
  preparationUI.root.innerHTML = ''; state.diagnosis = null; state.recovery = null;
  $('#report-root').innerHTML = ''; $('#history-list').innerHTML = ''; $('#progress-recovery').innerHTML = '';
  if (mode === 'logout') { $('#resume').value = ''; $('#jd').value = ''; $('#pdf-extracted').value = ''; $('#pdf-review').hidden = true; $('#consent').checked = false; updateCounts(); }
  await bootstrap();
}

function showView(name, nav = '') {
  for (const view of views) $(`#${view}-view`).hidden = view !== name;
  for (const link of $$('[data-nav]')) {
    if (link.dataset.nav === nav) link.setAttribute('aria-current', 'page'); else link.removeAttribute('aria-current');
  }
  document.body.dataset.view = name;
}

async function getExample() { if (!state.example) state.example = await api('/api/example'); return state.example; }

async function route() {
  preparationUI.stop();
  billingUI.stop();
  const epoch = ++state.epoch;
  clearTimeout(state.pollTimer);
  showError($('#poll-error'));
  await bootReady;
  if (epoch !== state.epoch) return;
  const routeName = location.hash.slice(1) || 'start';
  try {
    if (/^(account|plans|order|admin)(\/|\?|$)/.test(routeName)) { showView('billing', 'account'); await billingUI.open(routeName); return; }
    if (routeName === 'start') { showView('editor', 'start'); document.title = '岗位罗盘 · 让经历与机会对齐'; return; }
    if (routeName === 'example') {
      const example = await getExample(); if (epoch !== state.epoch) return;
      state.diagnosis = { id: 'example', report: example.report, status: 'done', created_at: 0 };
      state.filter = 'all'; renderReport(); showView('report', 'example');
      document.title = '示例报告 · 岗位罗盘';
      return;
    }
    if (routeName === 'history') { showView('history', 'history'); document.title = '我的报告 · 岗位罗盘'; await loadHistory(epoch); return; }
    const preparationMatch = /^prepare\/(example|[a-f0-9]{32})\/(refine|versions|interview)$/.exec(routeName);
    if (preparationMatch) {
      state.diagnosis = { id: preparationMatch[1] };
      showView('preparation', preparationMatch[1] === 'example' ? 'example' : 'history');
      document.title = '求职准备 · 岗位罗盘';
      await preparationUI.open(preparationMatch[1], preparationMatch[2]);
      return;
    }
    const match = /^report\/([a-f0-9]{32})$/.exec(routeName);
    if (match) {
      state.diagnosis = { id: match[1], status: 'pending' };
      $('#progress-title').textContent = '正在读取报告';
      $('#progress-description').textContent = '正在检查报告的最新状态。';
      $('#progress-recovery').innerHTML = recoveryCard(match[1]);
      showView('progress', 'history');
      await loadDiagnosis(match[1], epoch);
      return;
    }
    throw new Error('没有找到这个页面，请返回开始诊断。');
  } catch (error) { if (epoch === state.epoch) pageError(error.message); }
}

function pageError(message) { $('#page-error').textContent = message; showView('error'); document.title = '报告访问提示 · 岗位罗盘'; }

async function loadDiagnosis(id, epoch) {
  try {
    const diagnosis = await api(`/api/diagnoses/${id}`);
    if (epoch !== state.epoch) return;
    state.diagnosis = diagnosis;
    showError($('#poll-error'));
    if (diagnosis.status === 'done') {
      state.filter = 'all'; renderReport(); showView('report', 'history');
      document.title = `${diagnosis.report.title} · 岗位罗盘`;
    } else if (diagnosis.status === 'failed') {
      $('#report-root').innerHTML = `<div class="report-toolbar"><a href="#history" class="text-link muted">← 我的报告</a></div><div class="empty-state"><span class="empty-symbol" aria-hidden="true">↗</span><h1>这次分析没有完成</h1><p>${escapeHTML(diagnosis.error)}</p><div class="empty-actions"><a class="button primary" href="#start">检查材料后重新提交</a><button class="button secondary" data-delete-current>删除这份材料</button></div></div>${recoveryCard(id)}`;
      showView('report', 'history'); document.title = '分析未完成 · 岗位罗盘';
    } else {
      $('#progress-title').textContent = diagnosis.status === 'running' ? '正在逐项核对经历' : '材料已进入队列';
      $('#progress-description').textContent = diagnosis.status === 'running' ? '正在提取岗位要求、寻找简历依据，并检查引用。通常需要一至两分钟。' : (diagnosis.attempts > 0 ? '上次连接暂时中断，系统会在稍后自动重试。' : '前面的任务完成后，就会开始你的分析。');
      $('#stage-analyzing').textContent = diagnosis.status === 'running' ? '正在对照' : '正在排队';
      $('#progress-recovery').innerHTML = recoveryCard(id);
      showView('progress', 'history'); document.title = '分析进行中 · 岗位罗盘';
      state.pollTimer = setTimeout(() => loadDiagnosis(id, epoch), document.hidden ? 6000 : 2200);
    }
  } catch (error) {
    if (epoch !== state.epoch) return;
    if (error.status === 404 || error.status === 401) { pageError(error.message); return; }
    showError($('#poll-error'), `${error.message} 页面会继续尝试查询，也可以稍后从“我的报告”进入。`);
    state.pollTimer = setTimeout(() => loadDiagnosis(id, epoch), 6000);
  }
}

function recoveryCard(id) {
  if (!state.recovery || state.recovery.id !== id) return '';
  return `<aside class="recovery-card"><h3>保存这份报告的恢复码</h3><p>换浏览器或清理 Cookie 后，可以用它找回报告。</p><code class="recovery-code">${escapeHTML(state.recovery.code)}</code><div class="recovery-actions"><button class="text-link" type="button" data-copy-recovery>复制恢复码</button><button class="text-link" type="button" data-download-recovery>下载恢复凭据 ↗</button></div><p class="recovery-warning">恢复码仅在创建时显示，请勿公开。报告最迟保留至 ${dateText(state.recovery.expires)}，删除后立即失效。</p></aside>`;
}

function badge(status) {
  const color = status === 'needs_detail' ? 'detail' : status === 'not_found' ? 'missing' : 'supported';
  return `<span class="status-badge ${escapeHTML(status)}"><span class="status-dot ${color}" aria-hidden="true"></span>${escapeHTML(labels[status] || status)}</span>`;
}

function renderReport() {
  const diagnosis = state.diagnosis, report = diagnosis.report, example = report.example;
  const total = report.requirements.length;
  $('#report-root').innerHTML = `
    <div class="report-toolbar"><a class="text-link muted" href="${example ? '#start' : '#history'}">← ${example ? '返回开始诊断' : '我的报告'}</a><div class="toolbar-actions"><button type="button" class="button secondary small" data-export-md>导出 Markdown ↓</button><button type="button" class="button primary small" data-export-pdf>保存 PDF ↗</button></div></div>
    ${example ? '<div class="example-banner"><span>这是一份固定的虚构示例，用于展示报告结构。</span><a href="#start" class="text-link">诊断我的简历 ↗</a></div>' : ''}
    <div class="report-heading"><div><p class="eyebrow">岗位与经历 · 逐项对照</p><h1>${escapeHTML(report.title)}</h1><p class="report-summary">${escapeHTML(report.summary)}</p></div><div class="report-stamp">${example ? 'EXAMPLE / 固定示例' : dateText(diagnosis.created_at, true)}<br>${total} 项岗位要求<br>${escapeHTML(report.rubric_version)}</div></div>
    <section class="preparation-cta"><div><h2>把诊断变成下一步行动</h2><p>回答追问、核对简历改写，保存岗位版本，再用你的项目练一轮面试。</p></div><a class="button primary" href="#prepare/${diagnosis.id}/refine">${example ? '查看求职准备示例' : '进入求职准备'} ↗</a></section>
    <div class="counts-bar" aria-label="证据分类统计"><div class="count-item"><span class="count-number">${report.counts.supported}</span><div class="count-text"><strong>已有依据</strong><p>有直接相关的经历</p></div></div><div class="count-item detail"><span class="count-number">${report.counts.needs_detail}</span><div class="count-text"><strong>待补充</strong><p>相关细节还需说明</p></div></div><div class="count-item missing"><span class="count-number">${report.counts.not_found}</span><div class="count-text"><strong>未体现</strong><p>原文未找到明确表述</p></div></div></div>
    <p class="evidence-note">这些数量描述本次材料的证据分布。未写出的经历，不代表你不具备这项能力。</p>
    <section aria-labelledby="evidence-title"><div class="evidence-section-heading"><h2 id="evidence-title">每项要求，找到它的依据</h2><div class="filter-bar" role="group" aria-label="按证据状态筛选"><button type="button" data-filter="all" aria-pressed="true">全部 ${total}</button><button type="button" data-filter="supported" aria-pressed="false">已有依据 ${report.counts.supported}</button><button type="button" data-filter="needs_detail" aria-pressed="false">待补充 ${report.counts.needs_detail}</button><button type="button" data-filter="not_found" aria-pressed="false">未体现 ${report.counts.not_found}</button></div></div><div id="evidence-cards"></div></section>
    <section class="actions-section" aria-labelledby="actions-title"><h2 id="actions-title">先从这 ${report.actions.length} 件事开始改</h2><p>根据原文补充真实细节；不确定的信息，先确认再写进简历。</p><div class="action-grid">${report.actions.map((action, index) => `<article class="action-card"><span class="action-order">优先 ${String(index + 1).padStart(2, '0')}</span><h3>${escapeHTML(action.title)}</h3><p>${escapeHTML(action.suggestion)}</p><div class="action-question"><strong>需要你确认</strong><p>${escapeHTML(action.question)}</p></div><button class="text-link" type="button" data-jump="${escapeHTML(action.requirement_id)}">查看对应要求 ↗</button></article>`).join('')}</div></section>
    ${recoveryCard(diagnosis.id)}
    <div class="report-footnote"><p>岗位罗盘 · ${example ? '固定虚构示例，未调用模型。' : `报告保留至 ${dateText(diagnosis.expires_at, true)}。`} 引文经过来源核对，语义判断仍需本人确认。报告不预测录用结果，请仅补充真实经历。</p>${example ? '' : '<button type="button" class="text-link danger report-delete" data-delete-current>删除材料与报告</button>'}</div>
    <div class="report-end-cta"><a class="button secondary" href="#start">${example ? '准备我的简历与岗位' : '开始一份新的诊断'} <span aria-hidden="true">↗</span></a></div>`;
  renderEvidence();
}

function renderEvidence() {
  const report = state.diagnosis?.report;
  if (!report || !$('#evidence-cards')) return;
  const filtered = report.requirements.filter((item) => state.filter === 'all' || item.status === state.filter);
  $('#evidence-cards').innerHTML = filtered.map((item) => `<article class="evidence-card" id="requirement-${escapeHTML(item.id)}" tabindex="-1"><div class="evidence-card-header"><div class="requirement-title"><span class="requirement-id">${escapeHTML(item.id.toUpperCase())}</span><h3>${escapeHTML(item.title)}</h3><span class="priority-tag">${item.priority === 'preferred' ? '加分项' : '岗位要求'}</span></div>${badge(item.status)}</div><div class="evidence-columns"><div class="evidence-source"><p class="source-heading"><span>B</span> JD 原文</p><blockquote>${escapeHTML(item.jd_quote)}</blockquote></div><div class="evidence-source"><p class="source-heading"><span>A</span> 简历中的依据</p>${item.resume_quote ? `<blockquote>${escapeHTML(item.resume_quote)}</blockquote>` : '<p class="no-evidence">未找到明确表述，可由你补充确认。</p>'}</div></div><p class="evidence-explanation"><strong>判断说明</strong>${escapeHTML(item.explanation)}</p></article>`).join('') || '<p class="filter-empty">这份报告中没有此类要求。</p>';
  for (const button of $$('[data-filter]')) button.setAttribute('aria-pressed', String(button.dataset.filter === state.filter));
}

async function loadHistory(epoch = state.epoch) {
  $('#history-list').innerHTML = '<div class="empty-state"><p>正在读取你的报告…</p></div>';
  try {
    const { diagnoses } = await api('/api/diagnoses'); if (epoch !== state.epoch) return;
    if (!diagnoses.length) {
      $('#history-list').innerHTML = '<div class="empty-state"><span class="empty-symbol" aria-hidden="true">↗</span><h2>下一次对照，从这里开始</h2><p>当前浏览器还没有报告。准备好简历和岗位要求后，就可以开始。已有恢复码，也可以找回报告。</p><div class="empty-actions"><a class="button primary" href="#start">开始诊断 ↗</a><a class="button secondary" href="#example">先看完整示例</a></div></div>';
      return;
    }
    $('#history-list').innerHTML = diagnoses.map((d) => `<article class="history-item"><div><h2>后端开发岗位诊断 <span class="requirement-id">${escapeHTML(d.id.slice(0, 6).toUpperCase())}</span></h2><p>${dateText(d.created_at, true)} 创建 · 保留至 ${dateText(d.expires_at)}</p></div><div class="history-item-actions"><span class="status-badge ${d.status === 'failed' ? 'needs_detail' : ''}">${escapeHTML(labels[d.status])}</span>${d.status === 'done' ? `<a class="text-link" href="#prepare/${escapeHTML(d.id)}/refine">求职准备 ↗</a>` : ''}<a class="text-link" href="#report/${escapeHTML(d.id)}">${d.status === 'done' ? '查看报告' : '查看状态'} ↗</a><button type="button" class="text-link danger" data-delete-id="${escapeHTML(d.id)}">删除</button></div></article>`).join('');
  } catch (error) { if (epoch === state.epoch) $('#history-list').innerHTML = `<div class="empty-state"><h2>报告列表暂时无法打开</h2><p>${escapeHTML(error.message)}</p><div class="empty-actions"><button class="button secondary" data-reload-history>重新加载</button></div></div>`; }
}

function updateCounts() {
  $('#resume-count').textContent = `${[...$('#resume').value].length.toLocaleString('zh-CN')} / 20,000`;
  $('#jd-count').textContent = `${[...$('#jd').value].length.toLocaleString('zh-CN')} / 12,000`;
  showError($('#form-error'));
}

function setInputMode(mode) {
  $('#text-tab').setAttribute('aria-pressed', String(mode === 'text'));
  $('#pdf-tab').setAttribute('aria-pressed', String(mode === 'pdf'));
  $('#pdf-panel').hidden = mode !== 'pdf';
}

async function fillExample() {
  const { input } = await getExample();
  $('#resume').value = input.resume; $('#jd').value = input.jd;
  $('#consent').checked = false; updateCounts();
  if (location.hash && location.hash !== '#start') location.hash = 'start';
  $('#workspace-title').scrollIntoView({ behavior: motion() });
  toast('已填入虚构示例。提交前请替换为你的真实材料，并确认处理说明。');
}

async function extractPDF(file) {
  const token = ++state.pdfEpoch;
  if (state.pdfTask) { await state.pdfTask.destroy().catch(() => {}); state.pdfTask = null; }
  setInputMode('pdf'); $('#pdf-review').hidden = true;
  $('#pdf-status').classList.remove('error');
  const fail = (message) => { $('#pdf-status').textContent = message; $('#pdf-status').classList.add('error'); };
  if (!file) return;
  if (file.size > 5 * 1024 * 1024) { fail('PDF 超过 5 MB，请压缩文件或直接粘贴文字。'); return; }
  if (!/\.pdf$/i.test(file.name) && file.type !== 'application/pdf') { fail('请选择 PDF 文件。扫描图片暂不支持，请改为粘贴文字。'); return; }
  $('#pdf-status').textContent = `正在当前设备读取 ${file.name}…`;
  let task;
  try {
    const pdfjs = await import('/assets/vendor/pdf.mjs');
    if (token !== state.pdfEpoch) return;
    pdfjs.GlobalWorkerOptions.workerSrc = '/assets/vendor/pdf.worker.mjs';
    task = pdfjs.getDocument({ data: new Uint8Array(await file.arrayBuffer()), cMapUrl: '/assets/vendor/cmaps/', cMapPacked: true, standardFontDataUrl: '/assets/vendor/standard_fonts/', wasmUrl: '/assets/vendor/wasm/', isEvalSupported: false });
    state.pdfTask = task;
    const pdf = await task.promise;
    if (token !== state.pdfEpoch) return;
    if (pdf.numPages > 6) throw new Error('PDF 超过 6 页，请只保留与求职相关的页面或粘贴文字。');
    const pages = [];
    for (let pageNumber = 1; pageNumber <= pdf.numPages; pageNumber++) {
      const page = await pdf.getPage(pageNumber);
      const content = await page.getTextContent();
      if (token !== state.pdfEpoch) return;
      pages.push(content.items.map((item) => typeof item.str === 'string' ? item.str + (item.hasEOL ? '\n' : ' ') : '').join(''));
      page.cleanup();
      $('#pdf-status').textContent = `正在本地提取第 ${pageNumber} / ${pdf.numPages} 页…`;
    }
    const text = pages.join('\n\n')
      .replace(/[\u2e80-\u2fff\uf900-\ufaff]/gu, (character) => character.normalize('NFKC'))
      .replace(/([\p{Script=Han}])[ \t]+(?=[\p{Script=Han}])/gu, '$1')
      .replace(/[ \t]+\n/g, '\n').replace(/\n{4,}/g, '\n\n').trim();
    if ([...text].length < 100) throw new Error('没有提取到足够的文字。文件可能是扫描件，请改为粘贴文字。');
    if ([...text].length > 20000) throw new Error('提取文字超过 20,000 字符，请节选相关经历后粘贴。');
    $('#pdf-extracted').value = text;
    $('#pdf-review').hidden = false;
    $('#pdf-status').textContent = `已在本地提取 ${pdf.numPages} 页。请检查顺序与内容，再使用这些文字。`;
  } catch (error) {
    if (token !== state.pdfEpoch) return;
    if (error.name === 'PasswordException') fail('这是加密 PDF，请先移除密码，或直接粘贴文字。');
    else if (/^(PDF 超过|没有提取|提取文字超过)/.test(error.message)) fail(error.message);
    else fail('暂时无法读取这份 PDF。请确认文件完整，或直接粘贴简历文字。');
  } finally {
    if (task) await task.destroy().catch(() => {});
    if (token === state.pdfEpoch) state.pdfTask = null;
  }
}

function downloadBlob(content, name, type = 'text/plain;charset=utf-8') {
  const blob = content instanceof Blob ? content : new Blob([content], { type });
  const url = URL.createObjectURL(blob), link = document.createElement('a');
  link.href = url; link.download = name; document.body.append(link); link.click(); link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

async function exportMarkdown() {
  const d = state.diagnosis;
  if (!d?.report) return;
  if (d.id === 'example') {
    const r = d.report;
    const lines = ['> 固定虚构示例，未调用模型。', '', `# ${r.title}`, '', r.summary, '', `已有依据 ${r.counts.supported} 项 · 待补充 ${r.counts.needs_detail} 项 · 未体现 ${r.counts.not_found} 项`, ''];
    for (const item of r.requirements) lines.push(`## ${item.title} [${labels[item.status]}]`, '', `JD 原文：${item.jd_quote}`, '', `简历原文：${item.resume_quote || '未找到明确表述'}`, '', item.explanation, '');
    lines.push('## 优先修改建议', '');
    for (const action of r.actions) lines.push(`### ${action.title}`, '', action.suggestion, '', `需要你确认：${action.question}`, '');
    lines.push('岗位罗盘 · AI 辅助分析。引文经过来源核对，语义判断仍需本人确认。');
    downloadBlob(lines.join('\n'), '岗位罗盘-示例报告.md', 'text/markdown;charset=utf-8');
  } else {
    const response = await fetch(`/api/diagnoses/${d.id}/export`, { credentials: 'same-origin', cache: 'no-store' });
    if (!response.ok) { const body = await response.json().catch(() => ({})); throw new Error(body.error || '导出未完成，请重试。'); }
    downloadBlob(await response.blob(), `岗位罗盘-${d.id.slice(0, 8)}.md`);
  }
  toast('报告文件已准备下载。');
}

function exportPDF() {
  if (!state.diagnosis?.report) return;
  const filter = state.filter;
  state.filter = 'all'; renderEvidence();
  window.addEventListener('afterprint', () => { if (state.diagnosis?.report) { state.filter = filter; renderEvidence(); } }, { once: true });
  window.print();
}

function motion() { return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'instant' : 'smooth'; }
function openDelete(id) { if (!id || id === 'example') return; state.deletingID = id; showError($('#delete-error')); $('#delete-dialog').showModal(); }

document.addEventListener('click', async (event) => {
  const button = event.target.closest('button, [data-jump]');
  if (!button) return;
  try {
    if (button.dataset.openDialog) { $(`#${button.dataset.openDialog}`).showModal(); return; }
    if (button.hasAttribute('data-close-dialog')) { button.closest('dialog').close(); return; }
    if (button.dataset.filter) { state.filter = button.dataset.filter; renderEvidence(); return; }
    if (button.dataset.jump) { state.filter = 'all'; renderEvidence(); const target = $(`#requirement-${button.dataset.jump}`); target?.scrollIntoView({ behavior: motion(), block: 'start' }); target?.focus({ preventScroll: true }); return; }
    if (button.hasAttribute('data-export-md')) { await exportMarkdown(); return; }
    if (button.hasAttribute('data-export-pdf')) { exportPDF(); return; }
    if (button.hasAttribute('data-delete-current')) { openDelete(state.diagnosis?.id); return; }
    if (button.dataset.deleteId) { openDelete(button.dataset.deleteId); return; }
    if (button.hasAttribute('data-reload-history')) { await loadHistory(); return; }
    if (button.hasAttribute('data-copy-recovery') && state.recovery) {
      try { await navigator.clipboard.writeText(state.recovery.code); toast('恢复码已复制，请保存到安全的位置。'); } catch { toast('复制未完成，请手动选中恢复码并复制。'); }
      return;
    }
    if (button.hasAttribute('data-download-recovery') && state.recovery) {
      downloadBlob(`岗位罗盘报告恢复凭据\n\n恢复码：${state.recovery.code}\n报告编号：${state.recovery.id}\n网站：${location.origin}\n保留至：${dateText(state.recovery.expires, true)}\n\n此恢复码可以授予报告查看及删除权限，请勿公开。报告到期或删除后，恢复码失效。`, `岗位罗盘-恢复凭据-${state.recovery.id.slice(0, 8)}.txt`);
      toast('恢复凭据已准备下载，请妥善保存。');
    }
  } catch (error) { toast(error.message); }
});

$('#diagnosis-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  if (state.submitting) return;
  if (!state.config?.ready) { showError($('#form-error'), '真实诊断暂未开放，可以先查看完整示例报告。'); return; }
  const resume = $('#resume').value.trim(), jd = $('#jd').value.trim();
  if ([...resume].length < 100 || [...resume].length > 20000) { showError($('#form-error'), '简历需为 100—20,000 字符，请检查文字内容。'); $('#resume').focus(); return; }
  if ([...jd].length < 80 || [...jd].length > 12000) { showError($('#form-error'), '岗位要求需为 80—12,000 字符，请粘贴完整 JD。'); $('#jd').focus(); return; }
  if (!$('#consent').checked) { showError($('#form-error'), '请阅读材料处理说明，并确认同意后提交。'); return; }
  state.submitting = true; $('#submit-diagnosis').disabled = true; showError($('#form-error'));
  $('#submit-diagnosis span:first-child').textContent = '正在提交材料…';
  try {
    const result = await api('/api/diagnoses', { method: 'POST', body: JSON.stringify({ resume, jd, role: 'backend', consent: true, consent_version: state.config.consent_version }) });
    if (result.recovery_code) state.recovery = { id: result.id, code: result.recovery_code, expires: result.expires_at };
    if (result.duplicate) toast('这份材料已有对应任务，已为你打开。');
    location.hash = `report/${result.id}`;
    window.scrollTo({ top: 0, behavior: motion() });
  } catch (error) { showError($('#form-error'), error.message); if (error.status === 402) { $('#form-error').innerHTML = `${escapeHTML(error.message)} <a class="text-link" href="${error.code === 'account_required' ? '#account/login?next=start' : '#account'}">前往账号页 ↗</a>（填写的材料会保留在当前页面）`; } }
  finally { state.submitting = false; $('#submit-diagnosis').disabled = !state.config?.ready; $('#submit-diagnosis span:first-child').textContent = '开始逐项诊断'; }
});

$('#recover-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  $('#recover-submit').disabled = true; showError($('#recovery-error'));
  try {
    const result = await api('/api/recover', { method: 'POST', body: JSON.stringify({ code: $('#recovery-input').value }) });
    $('#recover-dialog').close(); $('#recovery-input').value = '';
    if (location.hash === `#report/${result.id}`) await route();
    else location.hash = `report/${result.id}`;
    toast('已为当前浏览器恢复访问权限。');
  } catch (error) { showError($('#recovery-error'), error.message); }
  finally { $('#recover-submit').disabled = false; }
});

$('#confirm-delete').addEventListener('click', async () => {
  if (!state.deletingID) return;
  $('#confirm-delete').disabled = true; showError($('#delete-error'));
  try {
    await api(`/api/diagnoses/${state.deletingID}`, { method: 'DELETE' });
    preparationUI.forget(state.deletingID);
    if (state.recovery?.id === state.deletingID) state.recovery = null;
    $('#delete-dialog').close(); state.deletingID = null;
    clearTimeout(state.pollTimer); toast('材料与报告已删除，恢复码已失效。');
    if (location.hash === '#history') await loadHistory(); else location.hash = 'history';
  } catch (error) { showError($('#delete-error'), error.message); }
  finally { $('#confirm-delete').disabled = false; }
});

$('#resume').addEventListener('input', updateCounts); $('#jd').addEventListener('input', updateCounts);
$('#text-tab').addEventListener('click', () => setInputMode('text'));
$('#pdf-tab').addEventListener('click', () => setInputMode('pdf'));
$('#choose-pdf').addEventListener('click', () => $('#pdf-file').click());
$('#pdf-file').addEventListener('change', () => { extractPDF($('#pdf-file').files[0]); $('#pdf-file').value = ''; });
$('#apply-pdf').addEventListener('click', () => { $('#resume').value = $('#pdf-extracted').value; $('#pdf-review').hidden = true; updateCounts(); $('#resume').focus(); toast('提取文字已放入简历区，确认内容后再提交。'); });
$('#fill-example').addEventListener('click', () => fillExample().catch((error) => toast(error.message)));
for (const name of ['dragenter', 'dragover']) $('#drop-zone').addEventListener(name, (event) => { event.preventDefault(); $('#drop-zone').classList.add('drag-over'); });
for (const name of ['dragleave', 'drop']) $('#drop-zone').addEventListener(name, (event) => { event.preventDefault(); $('#drop-zone').classList.remove('drag-over'); });
$('#drop-zone').addEventListener('drop', (event) => extractPDF(event.dataTransfer.files[0]));
for (const dialog of $$('dialog')) dialog.addEventListener('click', (event) => {
  if (event.target !== dialog) return;
  const rect = dialog.getBoundingClientRect();
  if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
});
window.addEventListener('hashchange', () => { route(); window.scrollTo({ top: 0, behavior: 'instant' }); });
const preparationUI = new PreparationUI({ api, toast, downloadBlob, dateText, config: () => state.config, onError: pageError });
const billingUI = new BillingUI({ toast, downloadBlob, dateText, config: () => state.config, refreshConfig:bootstrap, onAuth:accountChanged, onOverview: overview => { if (state.config) { state.config.account = overview.account; state.config.billing_enabled = overview.enabled; updateBillingHints(); } } });
const bootReady = bootstrap();
updateCounts();
route();
