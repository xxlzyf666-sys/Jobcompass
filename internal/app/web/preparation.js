const h = (value = '') => String(value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const taskNames = { questions: '正在整理需要你补充的问题', rewrite: '正在根据确认的事实生成修改建议', tailor: '正在对照目标岗位调整简历重点', interview_start: '正在准备第一道面试题', interview_answer: '正在分析回答并准备下一步', interview_review: '正在整理本轮面试复盘' };
const focusNames = { project: '项目深挖', backend: '后端技术', behavioral: '协作表达' };
const list = items => `<ul>${(items || []).map(item => `<li>${h(item)}</li>`).join('')}</ul>`;
const pendingEdits = v => (v?.edits || []).filter(e => e.status === 'pending').length;

export class PreparationUI {
  constructor(helpers) {
    Object.assign(this, helpers);
    this.root = document.querySelector('#preparation-root');
    this.token = 0; this.drafts = new Map(); this.active = false;
    this.root.addEventListener('click', event => this.click(event));
    this.root.addEventListener('submit', event => this.submit(event));
    this.root.addEventListener('change', event => {
      if (event.target.id === 'prep-version-select') { this.versionID = event.target.value; this.render(); }
      if (event.target.id === 'prep-interview-select') { this.interviewID = event.target.value; this.render(); }
    });
    this.root.addEventListener('input', event => {
      const form = event.target.closest('form[data-draft]');
      if (!form) return;
      this.drafts.set(this.draftKey(form.dataset.draft), Object.fromEntries(new FormData(form)));
      const note = form.querySelector('[data-unsaved]');
      if (note) { note.textContent = '有尚未保存的修改'; note.hidden = false; }
    });
    this.root.addEventListener('invalid', event => { const details = event.target.closest('details'); if (details) details.open = true; }, true);
    window.addEventListener('beforeunload', event => { if (this.drafts.size) { event.preventDefault(); event.returnValue = ''; } });
  }
  stop() { this.active = false; this.token++; clearTimeout(this.timer); }
  forget(id) { for (const key of this.drafts.keys()) if (key.startsWith(`${id}:`)) this.drafts.delete(key); try { sessionStorage.removeItem(`jc-preparation:${id}`); } catch {} if (this.id === id) { this.stop(); this.data = null; this.root.innerHTML = ''; } }
  remember() { try { sessionStorage.setItem(`jc-preparation:${this.id}`, JSON.stringify({ version:this.versionID, interview:this.interviewID })); } catch {} }
  draftKey(kind) { return `${this.id}:${kind}:${kind === 'answer' ? this.interviewID : this.versionID}`; }
  draft(kind) { return this.drafts.get(this.draftKey(kind)) || {}; }
  get work() { return this.data?.preparation; }
  get version() { return this.work?.versions.find(v => v.id === this.versionID) || this.work?.versions[0]; }
  get interview() { return this.work?.interviews.find(i => i.id === this.interviewID); }
  get busy() { return this.sending || ['pending', 'running'].includes(this.data?.task?.status); }
  get demo() { return this.id === 'example'; }
  get blocked() { return this.busy || this.demo; }
  get aiBlocked() { return this.blocked || !this.data?.ready; }
  get paid() { return !this.demo && (this.data?.billing_enabled ?? this.config()?.billing_enabled); }
  cost(kind) {
    if (!this.paid) return '';
    const text = {refine:'生成追问使用 1 轮精修，包含一次回答后的改写。重新生成追问开启新一轮。',tailor:'每次按岗位生成建议使用 1 次岗位适配；手动编辑、采纳和导出不扣次。',interview:'首题生成后使用 1 场面试，包含最多 5 题、反馈及复盘。提前结束仍计 1 场。'}[kind];
    return `<div class="billing-cost prep-billing-cost"><span>${text} 最终生成失败退回次数，手动重试会重新预占。</span><a href="${this.config()?.account ? '#account' : `#account/login?next=${encodeURIComponent(`prepare/${this.id}/${this.tab}`)}`}">查看次数 / 登录 ↗</a></div>`;
  }
  url(tab) { return `#prepare/${this.id}/${tab}`; }
  async open(id, tab) {
    this.stop(); this.active = true;
    if (this.id !== id) {
      this.versionID = 'base'; this.interviewID = null; this.data = null;
      try { const selected = JSON.parse(sessionStorage.getItem(`jc-preparation:${id}`) || '{}'); this.versionID = selected.version || 'base'; this.interviewID = selected.interview || null; } catch {}
    }
    this.id = id; this.tab = tab;
    this.root.innerHTML = '<div class="prep-loading" role="status">正在读取求职准备资料…</div>';
    await this.load(this.token);
  }
  async load(token = this.token, polling = false) {
    try {
      const data = await this.api(this.demo ? '/assets/preparation-example.json' : `/api/diagnoses/${this.id}/preparation`);
      if (!this.active || token !== this.token) return;
      const changed = !this.data || this.data.preparation.revision !== data.preparation.revision || this.data.task?.status !== data.task?.status;
      const completed = polling && this.data?.task && ['pending','running'].includes(this.data.task.status) && data.task?.status === 'done';
      this.data = data;
      if (!this.work.versions.some(v => v.id === this.versionID)) this.versionID = this.work.versions[0].id;
      if (!this.work.interviews.some(i => i.id === this.interviewID)) this.interviewID = this.work.interviews.at(-1)?.id;
      if (!polling || changed) this.render();
      if (completed) {
        const target = this.tab === 'interview' ? this.root.querySelector(this.interview?.review ? '.prep-review' : '#interview-answer') : this.root.querySelector('.prep-edit');
        target?.scrollIntoView({ behavior:'instant', block:'center' });
      }
      this.schedule(token);
    } catch (error) {
      if (!this.active || token !== this.token) return;
      if (error.status === 401 || error.status === 404) { this.stop(); this.onError(error.message); return; }
      if (!this.data) this.root.innerHTML = `<div class="empty-state"><h1>暂时无法打开工作台</h1><p>${h(error.message)}</p><div class="empty-actions"><button class="button secondary" data-prep="reload">重新加载</button><a class="text-link" href="#history">返回我的报告</a></div></div>`;
      else this.showMessage(`${error.message} 已提交的任务可稍后继续查看。`);
      if (polling) this.timer = setTimeout(() => this.load(token, true), 6000);
    }
  }
  schedule(token) {
    clearTimeout(this.timer);
    if (this.active && !this.demo && ['pending','running'].includes(this.data?.task?.status)) this.timer = setTimeout(() => this.load(token, true), document.hidden ? 6000 : 1800);
  }
  showMessage(message) { const el = this.root.querySelector('#prep-message'); if (el) { el.textContent = message; el.hidden = false; } else this.toast(message); }
  async change(action, fields = {}, clearDraft = '') {
    if (this.demo) { this.toast('这里是固定虚构示例。准备自己的简历与岗位后，即可使用。'); return; }
    if (this.busy) return;
    const token = this.token, id = this.id, draftKey = clearDraft ? this.draftKey(clearDraft) : '';
    this.sending = true;
    for (const fieldset of this.root.querySelectorAll('fieldset')) fieldset.disabled = true;
    const oldVersions = new Set(this.work.versions.map(v => v.id));
    const oldInterviews = new Set(this.work.interviews.map(i => i.id));
    try {
      const data = await this.api(`/api/diagnoses/${id}/preparation`, { method: 'POST', body: JSON.stringify({ action, revision: this.work.revision, version_id: this.versionID, consent: true, consent_version: this.config()?.consent_version, ...fields }) });
      if (draftKey) this.drafts.delete(draftKey);
      if (action === 'save_version') this.drafts.delete(`${id}:facts:${fields.version_id || this.versionID}`);
      if (!this.active || token !== this.token) return;
      this.data = data;
      const addedVersion = data.preparation.versions.find(v => !oldVersions.has(v.id));
      const addedInterview = data.preparation.interviews.find(i => !oldInterviews.has(i.id));
      if (addedVersion) this.versionID = addedVersion.id;
      if (addedInterview) this.interviewID = addedInterview.id;
      if (!data.preparation.versions.some(v => v.id === this.versionID)) this.versionID = 'base';
      if (!data.preparation.interviews.some(i => i.id === this.interviewID)) this.interviewID = data.preparation.interviews.at(-1)?.id;
      if (action === 'tailor') location.hash = this.url('refine');
      if (action === 'accept_edit') this.toast('已采纳，简历正文已更新。');
      if (action === 'save_version') this.toast('岗位版本已保存。');
      if (action === 'new_version') this.toast('新岗位版本已创建，可继续调整正文或生成岗位修改建议。');
      this.sending = false; this.render(); this.schedule(token);
    } catch (error) {
      if (!this.active || token !== this.token) return;
      this.sending = false;
      // Reload after uncertain delivery; saved revision and question identity prevent duplicates.
      await this.load(token);
      this.showMessage(error.message);
      if (error.status === 402) { const el = this.root.querySelector('#prep-message'); if (el) el.innerHTML = `${h(error.message)} <a class="text-link" href="${error.code === 'account_required' ? `#account/login?next=${encodeURIComponent(`prepare/${id}/${this.tab}`)}` : '#account'}">前往账号页 ↗</a>`; }
    } finally { this.sending = false; if (this.active && token !== this.token && this.data) this.render(); }
  }
  status() {
    const task = this.data.task;
    if (!task) return '';
    if (['pending','running'].includes(task.status)) return `<div class="prep-task" role="status"><span class="prep-pulse" aria-hidden="true"></span><div><strong>${h(taskNames[task.kind])}</strong><p>${task.status === 'pending' ? '任务已保存，正在排队。' : '可以离开此页，稍后回来继续。'} 当前已保存的简历仍可导出。</p></div></div>`;
    if (task.status === 'failed') return `<div class="prep-task prep-failure" role="alert"><div><strong>这次生成没有完成</strong><p>${h(task.error)}</p></div><button class="button secondary small" data-prep="retry" ${this.aiBlocked ? 'disabled' : ''}>重试本次生成</button></div>`;
    return '';
  }
  render() {
    if (!this.data || !this.active) return;
    this.remember();
    const p = this.work, v = this.version;
    const tabs = [['refine','简历精修'], ['versions','岗位版本'], ['interview','面试练习']];
    this.root.innerHTML = `
      <div class="report-toolbar prep-toolbar"><a class="text-link muted" href="${this.demo ? '#example' : `#report/${this.id}`}">← 诊断报告</a><button class="text-link muted" data-prep="reload">刷新资料</button></div>
      ${this.demo ? '<div class="example-banner"><span>固定虚构示例 · 展示精修、岗位版本和面试复盘，未调用 AI。</span><a class="text-link" href="#start">准备我的材料 ↗</a></div>' : ''}
      <header class="prep-heading"><div><p class="eyebrow">从诊断，到下一次机会</p><h1>你的求职准备</h1><p>补全真实经历，保存岗位简历，用自己的项目练习面试。</p></div><span class="prep-saved">${p.versions.length} 个版本 · ${p.interviews.length} 场练习</span></header>
      <nav class="prep-tabs" aria-label="求职准备步骤">${tabs.map(([key,label]) => `<a href="${this.url(key)}" ${this.tab === key ? 'aria-current="page"' : ''}>${label}${key === 'refine' && pendingEdits(v) ? `<span>${pendingEdits(v)} 待核对</span>` : ''}</a>`).join('')}</nav>
      <div class="prep-context"><label for="prep-version-select">当前简历版本</label><select id="prep-version-select">${p.versions.map(item => `<option value="${h(item.id)}" ${item.id === v.id ? 'selected' : ''}>${h(item.name)}</option>`).join('')}</select><span>修改只影响选定版本</span></div>
      <p id="prep-message" class="form-error" role="alert" hidden></p>
      ${!this.data.ready && !this.demo ? '<p class="service-notice">AI 准备服务暂未开放，已有版本仍可编辑、保存和导出。</p>' : ''}
      ${this.status()}
      ${this.tab === 'refine' ? this.refinement(v) : this.tab === 'versions' ? this.versions(v) : this.interviews(v)}
      <p class="prep-retention">${this.demo ? '示例为虚构资料，仅用于展示。' : `本工作台与报告一同保留至 ${this.dateText(this.data.expires_at, true)}。恢复码可找回全部资料；删除报告会同时删除版本、补充事实和面试记录。`} ${this.demo ? '' : `生成时，所选简历、岗位、补充事实及相关回答会交给 ${h(this.config()?.provider || '配置的 AI 服务')} 处理。`}</p>`;
  }
  source(v) {
    return `<aside class="prep-source"><div class="prep-source-heading"><span class="document-number">A</span><h2>已保存的简历</h2></div><p class="prep-caption">${h(v.name)}</p><div class="prep-source-text">${h(v.text)}</div><a class="text-link" href="${this.url('versions')}">编辑正文与导出 ↗</a><details class="prep-details"><summary>查看目标岗位 JD</summary><div class="prep-preserve">${h(v.jd)}</div></details>${v.facts?.length ? `<details class="prep-details"><summary>已确认的补充事实 · ${v.facts.length}</summary>${list(v.facts)}</details>` : ''}</aside>`;
  }
  refinement(v) {
    const answers = this.draft('facts');
    const questions = v.questions || [], edits = v.edits || [], completed = this.paid && v.refinement_complete;
    return `<div class="prep-grid">${this.source(v)}<div class="prep-main">
      <section class="prep-panel"><div class="prep-section-heading"><div><span class="prep-step">第一步 · 补充事实</span><h2>先把经历问清楚</h2></div>${questions.length ? `<button class="text-link" data-prep="questions" ${this.aiBlocked ? 'disabled' : ''}>重新生成追问</button>` : ''}</div>
      ${this.cost('refine')}<p class="prep-description">针对当前简历和岗位，追问职责、技术取舍和可验证结果。没有的经历和数据，请直接说明。</p>
      ${questions.length ? `<form data-form="facts" data-draft="facts"><fieldset ${this.blocked || completed ? 'disabled' : ''}>${questions.map((q,index) => `<article class="prep-question"><div class="prep-question-label"><span>追问 ${index+1}</span><span>${h(q.why)}</span></div><blockquote>${h(q.quote)}</blockquote><label for="fact-${h(q.id)}">${h(q.question)}</label><textarea id="fact-${h(q.id)}" name="${h(q.id)}" rows="3" minlength="2" maxlength="1600" required placeholder="补充你实际负责的工作；不确定的信息请如实说明。">${h(answers[q.id] ?? q.answer)}</textarea></article>`).join('')}<label class="prep-check"><input type="checkbox" name="confirmed" required ${answers.confirmed ? 'checked' : ''}><span>我确认补充内容真实，同意用于本次简历精修。</span></label><p class="prep-caption" data-unsaved ${this.drafts.has(this.draftKey('facts')) ? '' : 'hidden'}>回答尚未提交，生成时会一起保存。</p><button type="submit" class="button primary" ${this.aiBlocked ? 'disabled' : ''}>${completed ? '本轮精修已完成' : '确认补充，生成修改建议 ↗'}</button></fieldset>${completed ? '<p class="prep-caption">本轮追问与改写已完成。可继续核对采纳；需要再次精修时，请生成新一轮追问。</p>' : ''}</form>` : `<div class="prep-invitation"><p>让问题具体到你的经历。回答后，再一起整理表达。</p><button class="button primary" data-prep="questions" ${this.aiBlocked ? 'disabled' : ''}>生成针对我的追问 ↗</button></div>`}</section>
      <section class="prep-panel"><div class="prep-section-heading"><div><span class="prep-step">第二步 · 核对改写</span><h2>逐段看清，逐条采纳</h2></div><span class="prep-caption">${pendingEdits(v)} 条待核对</span></div><p class="prep-description">核对含义与事实后再采纳。已保存的简历只包含你接受的修改，建议不会自动写入。</p>
      ${edits.length ? edits.map(e => `<article class="prep-edit"><div class="prep-edit-title"><strong>${h(e.reason)}</strong><span class="status-badge ${e.status === 'pending' ? 'needs_detail' : ''}">${({pending:'待核对',accepted:'已采纳',rejected:'已保留原文'})[e.status]}</span></div><div class="prep-diff"><div><span class="prep-diff-label">原文</span><p>${h(e.before)}</p></div><div><span class="prep-diff-label">修改建议</span><p>${h(e.after)}</p></div></div><details class="prep-details"><summary>使用了哪些事实</summary>${e.evidence.map(item => `<p><strong>${item.source === 'fact' ? '你的补充' : '简历原文'}</strong> · ${h(item.quote)}</p>`).join('')}</details>${e.status === 'pending' ? `<div class="prep-edit-actions"><button class="button secondary small" data-prep="reject_edit" data-edit="${h(e.id)}" ${this.blocked ? 'disabled' : ''}>保留原文</button><button class="button primary small" data-prep="accept_edit" data-edit="${h(e.id)}" ${this.blocked ? 'disabled' : ''}>核对并采纳</button></div>` : ''}</article>`).join('') : '<div class="prep-quiet-empty">补充回答后，修改前后的对比会出现在这里。也可以在“岗位版本”中生成针对 JD 的修改建议。</div>'}
      ${v.notes?.length ? `<div class="prep-notes"><strong>还需要你留意</strong>${list(v.notes)}</div>` : ''}
      ${edits.some(e => e.status === 'accepted') ? `<a class="button secondary" href="${this.url('versions')}">查看简历成稿与导出 ↗</a>` : ''}</section></div></div>`;
  }
  versions(v) {
    const draft = this.draft('editor'), create = this.draft('new');
    return `<div class="prep-grid prep-version-grid"><aside><section class="prep-panel"><h2>为不同岗位留一份简历</h2><p class="prep-description">从当前版本复制正文和已确认事实，再填写新的岗位要求。每份版本独立编辑。</p><div class="prep-version-list">${this.work.versions.map(item => `<button class="prep-version-card" data-prep="select_version" data-version="${h(item.id)}" aria-pressed="${item.id === v.id}"><strong>${h(item.name)}</strong><span>${this.dateText(item.updated_at)} 更新${pendingEdits(item) ? ` · ${pendingEdits(item)} 条待核对` : ''}</span></button>`).join('')}</div>
      <form data-form="new" data-draft="new"><fieldset ${this.blocked ? 'disabled' : ''}><label class="prep-field" for="new-version-name">新版本名称<input id="new-version-name" name="name" minlength="2" maxlength="80" required placeholder="例如：某公司 · Go 后端" value="${h(create.name || '')}"></label><label class="prep-field" for="new-version-jd">新岗位 JD<textarea id="new-version-jd" name="jd" rows="6" minlength="80" maxlength="12000" required placeholder="粘贴完整岗位要求，至少 80 字符。">${h(create.jd || '')}</textarea></label><button class="button secondary full-width" type="submit">复制为新的岗位版本 ＋</button></fieldset></form></section></aside>
      <section class="prep-panel prep-editor"><div class="prep-section-heading"><div><span class="prep-step">当前版本 · ${h(v.name)}</span><h2>把简历整理成稿</h2></div>${v.id !== 'base' ? `<button class="text-link danger" data-prep="delete_version" ${this.blocked ? 'disabled' : ''}>删除此版本</button>` : ''}</div>
      ${this.cost('tailor')}<form data-form="editor" data-draft="editor"><fieldset ${this.blocked ? 'disabled' : ''}><label class="prep-field" for="version-name">版本名称<input id="version-name" name="name" required minlength="2" maxlength="80" value="${h(draft.name ?? v.name)}"></label><details class="prep-details"><summary>查看或调整目标岗位 JD</summary><label class="prep-field" for="version-jd">目标岗位要求<textarea id="version-jd" name="jd" rows="7" minlength="80" maxlength="12000" required>${h(draft.jd ?? v.jd)}</textarea></label></details><label class="prep-field" for="version-text">简历正文<textarea class="prep-resume-editor" id="version-text" name="text" rows="20" minlength="100" maxlength="20000" required spellcheck="false">${h(draft.text ?? v.text)}</textarea></label><details class="prep-details"><summary>核对或修改补充事实 · ${v.facts?.length || 0} 条</summary><label class="prep-field" for="version-facts">已确认的补充事实<textarea id="version-facts" name="facts" rows="7" maxlength="14000" placeholder="每条事实之间空一行。可以删除不再准确的内容。">${h(draft.facts ?? (v.facts || []).join('\n\n'))}</textarea></label><p class="prep-caption">每条事实之间空一行。它们会用于后续改写和面试提问，请及时纠正过时信息。</p></details><p class="prep-caption">保存前核对经历和联系方式。正文、JD 或补充事实改变后，旧追问与修改建议会清除，已有面试快照保持原样。</p><p class="prep-unsaved" data-unsaved ${this.drafts.has(this.draftKey('editor')) ? '' : 'hidden'}>有尚未保存的修改</p><div class="prep-inline-actions"><button class="button primary" type="submit">核对并保存版本</button><button class="button secondary" data-prep="tailor" type="button" ${this.aiBlocked ? 'disabled' : ''}>按此岗位生成修改建议</button></div></fieldset></form>
      <div class="prep-export"><div><h3>导出已保存的简历</h3><p>清晰的 A4 文字版，自动分页。文件仅包含简历正文。</p></div><div class="prep-inline-actions">${this.demo ? '<button class="button secondary small" disabled>纯文字 ↓</button><button class="button primary small" disabled>预览 / 保存 PDF ↗</button>' : `<a class="button secondary small" href="/api/diagnoses/${this.id}/versions/${h(v.id)}/export" download>纯文字 ↓</a><a class="button primary small" target="_blank" rel="noopener" href="/api/diagnoses/${this.id}/versions/${h(v.id)}/export?format=html">预览 / 保存 PDF ↗</a>`}</div></div>
      <a class="text-link" href="${this.url('interview')}">用这个版本练习面试 ↗</a></section></div>`;
  }
  interviews(v) {
    const current = this.interview, draft = this.draft('answer');
    const newForm = `<section class="prep-panel"><h2>开始一场针对你的练习</h2><p class="prep-description">使用“${h(v.name)}”的当前成稿和目标岗位。面试开始后，题目与复盘始终基于这份快照。</p>${this.cost('interview')}<form data-form="interview"><fieldset ${this.aiBlocked ? 'disabled' : ''}><label class="prep-field" for="interview-focus">练习方向<select id="interview-focus" name="focus"><option value="project">项目深挖</option><option value="backend">后端技术</option><option value="behavioral">协作表达</option></select></label><label class="prep-field" for="interview-rounds">本轮题数<select id="interview-rounds" name="rounds"><option value="3">3 题 · 快速练习</option><option value="5" selected>5 题 · 完整一轮</option>${this.paid ? '' : '<option value="8">8 题 · 深入练习</option>'}</select></label><button class="button primary full-width" type="submit">开始文字模拟面试 ↗</button></fieldset></form></section>`;
    let transcript = '<section class="prep-panel prep-interview-empty"><span class="empty-symbol" aria-hidden="true">↗</span><h2>从你实际做过的项目问起</h2><p>每次回答都会影响下一次追问。练习结束后，查看逐题反馈和接下来的训练建议。</p><div class="prep-example-question"><span>一次项目深挖可以这样展开</span><p>你负责什么？ → 为什么这样设计？ → 遇到问题如何处理？</p></div></section>';
    if (current) {
      const last = current.turns.at(-1), answered = current.turns.filter(t => t.answer).length;
      transcript = `<section class="prep-panel prep-conversation"><div class="prep-section-heading"><div><span class="prep-step">${h(focusNames[current.focus])} · 已回答 ${answered} / ${current.rounds} 题</span><h2>${current.status === 'done' ? '这一轮，带走什么' : '沿着你的回答，继续追问'}</h2></div><button class="text-link danger" data-prep="delete_interview" ${this.blocked ? 'disabled' : ''}>删除此练习</button></div><p class="prep-description">${h(current.name)} · ${this.dateText(current.created_at, true)} 开始</p><details class="prep-details"><summary>查看本轮使用的简历快照</summary><div class="prep-preserve">${h(current.resume)}</div></details>
        ${current.review ? `<section class="prep-review"><p class="prep-step">本轮复盘</p><h3>${h(current.review.summary)}</h3><div class="prep-review-grid"><div><h4>已经体现的长处</h4>${current.review.strengths.length ? list(current.review.strengths) : '<p>本轮回答尚不足以确认稳定长处。</p>'}</div><div><h4>下一轮重点补齐</h4>${list(current.review.gaps)}</div></div><div class="prep-practice"><h4>接下来这样练</h4><ol>${current.review.practice_plan.map(s => `<li>${h(s)}</li>`).join('')}</ol></div><div class="prep-inline-actions"><button class="button primary small" data-prep="practice_again" ${this.aiBlocked ? 'disabled' : ''}>针对薄弱点再练一轮</button><button class="button secondary small" data-prep="export_interview">导出本轮复盘 ↓</button></div><p class="prep-caption">反馈仅针对本轮回答，不预测录用结果。</p></section>` : ''}
        <div class="prep-turns">${current.turns.map((turn,index) => `<article class="prep-turn"><div class="prep-turn-heading"><span>面试官 · 第 ${index+1} 题</span><span>${h(turn.question.focus)}</span></div><h3>${h(turn.question.question)}</h3><details class="prep-details"><summary>提问依据</summary><blockquote>${h(turn.question.resume_quote)}</blockquote></details>${turn.answer ? `<div class="prep-answer"><span>你的回答</span><p>${h(turn.answer)}</p></div>` : ''}${turn.feedback ? `<details class="prep-feedback" ${current.status === 'done' ? 'open' : ''}><summary>本题反馈与回答提纲</summary><blockquote>“${h(turn.feedback.answer_quote)}”</blockquote><p>${h(turn.feedback.assessment)}</p><p><strong>下一次可以这样改：</strong>${h(turn.feedback.improvement)}</p>${list(turn.feedback.outline)}</details>` : ''}</article>`).join('')}</div>
        ${current.status === 'active' && last && !last.answer ? `<form class="prep-answer-form" data-form="answer" data-draft="answer"><fieldset ${this.aiBlocked ? 'disabled' : ''}><label class="prep-field" for="interview-answer">你的回答<textarea id="interview-answer" name="answer" rows="6" minlength="2" maxlength="3000" required placeholder="像真实面试一样回答。不清楚的地方可以直接说明。">${h(draft.answer || '')}</textarea></label><p class="prep-caption" data-unsaved ${draft.answer ? '' : 'hidden'}>回答尚未提交</p><div class="prep-inline-actions"><button type="submit" class="button primary">${answered + 1 === current.rounds ? '提交回答，生成完整复盘' : '提交回答，继续追问'} ↗</button>${answered ? '<button type="button" class="text-link muted" data-prep="finish_interview">结束并复盘已答部分</button>' : ''}</div></fieldset></form>` : ''}
        ${['starting','thinking','reviewing'].includes(current.status) ? '<p class="prep-quiet-empty">已提交的材料与回答已保存。生成完成后会显示在这里；若生成失败，可使用上方的重试按钮。</p>' : ''}</section>`;
    }
    return `<div class="prep-grid prep-interview-grid"><aside>${this.work.interviews.length ? `<section class="prep-panel"><label class="prep-field" for="prep-interview-select">继续或回看练习<select id="prep-interview-select">${this.work.interviews.map((i,index) => `<option value="${h(i.id)}" ${i.id === this.interviewID ? 'selected' : ''}>第 ${index+1} 场 · ${h(i.name)} · ${i.status === 'done' ? '已复盘' : '进行中'}</option>`).join('')}</select></label></section>` : ''}${newForm}</aside>${transcript}</div>`;
  }
  async click(event) {
    const button = event.target.closest('[data-prep]');
    if (!button || button.disabled) return;
    const action = button.dataset.prep;
    try {
      if (action === 'reload') { await this.load(); return; }
      if (action === 'select_version') { this.versionID = button.dataset.version; this.render(); return; }
      if (action === 'export_interview') { this.exportInterview(); return; }
      if (['questions','tailor','accept_edit','practice_again'].includes(action) && this.drafts.has(this.draftKey('editor'))) { this.showMessage('请先在“岗位版本”保存正文、岗位和事实修改，再继续。'); return; }
      if (action === 'questions' && this.version.questions?.length && !window.confirm('重新生成会替换当前追问和修改建议，已采纳的正文及已确认事实会保留。' + (this.paid ? '本次使用 1 轮精修。' : '') + '继续吗？')) return;
      if (action === 'delete_version' && !window.confirm('删除当前岗位版本？已开始的面试仍保留原有快照。')) return;
      if (action === 'delete_interview' && !window.confirm('删除这场面试的回答和复盘？此操作无法撤回。')) return;
      if (action === 'finish_interview' && this.draft('answer').answer && !window.confirm('当前输入还没有提交。结束后仅复盘之前已提交的回答，继续吗？')) return;
      if (action === 'practice_again') { await this.change('start_interview', { interview_id: this.interview.id, rounds: this.paid ? Math.min(5, this.interview.rounds) : this.interview.rounds, focus: this.interview.focus }); return; }
      await this.change(action, { edit_id: button.dataset.edit || '', confirmed: action === 'accept_edit', interview_id: this.interviewID || '', task_id: this.data.task?.id || '' }, action === 'questions' ? 'facts' : '');
    } catch (error) { this.showMessage(error.message); }
  }
  async submit(event) {
    const form = event.target.closest('form[data-form]');
    if (!form) return;
    event.preventDefault();
    const values = Object.fromEntries(new FormData(form));
    if (['facts','new','interview'].includes(form.dataset.form) && this.drafts.has(this.draftKey('editor'))) { this.showMessage('请先在“岗位版本”保存正文、岗位和事实修改，再继续。'); return; }
    switch (form.dataset.form) {
      case 'facts': await this.change('rewrite', { confirmed: Boolean(values.confirmed), answers: this.version.questions.map(q => ({ id:q.id, answer:values[q.id] || '' })) }, 'facts'); break;
      case 'new': await this.change('new_version', { name:values.name, jd:values.jd }, 'new'); break;
      case 'editor': await this.change('save_version', { name:values.name, jd:values.jd, text:values.text, facts:values.facts.split(/\n\s*\n/u).map(s => s.trim()).filter(Boolean), confirmed:true }, 'editor'); break;
      case 'interview':
        if (this.drafts.has(this.draftKey('editor'))) { this.showMessage('请先保存简历正文，再用最新成稿开始面试。'); return; }
        await this.change('start_interview', { rounds:Number(values.rounds), focus:values.focus }); break;
      case 'answer': await this.change('answer', { interview_id:this.interviewID, question_id:this.interview.turns.at(-1).question.id, answer:values.answer }, 'answer'); break;
    }
  }
  exportInterview() {
    const i = this.interview;
    if (!i?.review) return;
    const lines = [`# ${i.name} · ${focusNames[i.focus]}复盘`, this.demo ? '> 固定虚构示例，未调用 AI。' : '', '', i.review.summary, '', '## 已体现的长处', ...i.review.strengths.map(s => `- ${s}`), '', '## 下一轮重点', ...i.review.gaps.map(s => `- ${s}`), '', '## 练习计划', ...i.review.practice_plan.map((s,n) => `${n+1}. ${s}`)];
    for (const [n,t] of i.turns.entries()) { lines.push('',`## 第 ${n+1} 题`,t.question.question,'','### 我的回答',t.answer || '未作答'); if (t.feedback) lines.push('','### 反馈',t.feedback.assessment,t.feedback.improvement,...t.feedback.outline.map(s => `- ${s}`)); }
    this.downloadBlob(lines.join('\n'), `岗位罗盘-面试复盘-${i.id.slice(0,8)}.md`); this.toast('面试复盘已准备下载。');
  }
}
