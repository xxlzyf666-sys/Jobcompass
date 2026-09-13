const h = (value = '') => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const kinds = { diagnosis:'逐项诊断', refine:'简历精修', tailor:'岗位适配', interview:'文字面试' };
const units = { diagnosis:'次', refine:'轮', tailor:'次', interview:'场' };
const statuses = { awaiting_payment:'待付款', submitted:'待人工核款', paid:'已发放', rejected:'需补充核对', cancelled:'已取消', expired:'已过期', refunded:'已退款' };
const eventNames = { welcome_granted:'注册赠送', granted:'到账发放', reserved:'生成中预占', consumed:'生成完成', released:'退回次数', revoked:'退款收回' };
const userActions = {note:'保存管理备注',grant_welcome:'补发四项体验',restrict:'限制 AI 生成',restore:'恢复 AI 生成',revoke_sessions:'退出全部设备'};
const userActionHelp = {grant_welcome:'诊断、精修、岗位适配和面试各赠送一次。每个账号仅可领取一次，已购次数保持不变。',restrict:'停止这个账号后续的 AI 生成请求。已提交的任务继续完成；材料查看、手动编辑、导出和订单售后仍可使用。',restore:'恢复这个账号的 AI 生成权限，继续按现有次数和频率限制使用。',revoke_sessions:'使这个账号的全部登录会话失效。用户需要重新登录，密码、材料和次数保持不变。'};
const money = value => `¥${(Number(value || 0) / 100).toFixed(2)}`;
function cents(value) {
  if (!/^\d{1,4}(\.\d{1,2})?$/.test(value)) throw new Error('金额请填写最多两位小数的人民币数值。');
  const [whole, part = ''] = value.split('.');
  return Number(whole) * 100 + Number(part.padEnd(2, '0'));
}
const included = credits => `<ul class="billing-included">${Object.keys(kinds).map(k => `<li><span>${kinds[k]}</span><strong>${credits?.[k] || 0} ${units[k]}</strong></li>`).join('')}</ul>`;
const rules = `<section class="billing-card billing-rules"><h2>购买前，请先了解这些规则</h2><ul>
  <li>新账号注册即赠诊断 1 次、精修 1 轮、岗位适配 1 次、文字面试 1 场，无需先付款。每个账号仅赠送一次，优先使用赠送次数，用完后再使用已购次数。赠送次数不能兑换现金或申请退款。</li>
  <li>使用个人收款码，付款后提交完整交易单号，由运营者核实到账后发放。点击“我已付款”不会立即获得次数；具体核款时间和联系方式以订单为准。</li>
  <li>诊断：每份完整报告 1 次。精修：生成一轮追问计 1 轮，包含一次基于回答的改写；重新生成追问开启新一轮。岗位适配：每次生成修改建议计 1 次。</li>
  <li>文字面试：每场 3 或 5 题，首题生成后计 1 场，包含后续追问、逐题反馈和复盘。提前结束或放弃已开始的练习，仍计 1 场。</li>
  <li>提交生成时预占次数，首次生成成功后扣次。自动重试不重复扣次；任务最终失败会退回对应次数，再次手动重试时重新预占。手动编辑、核对采纳、导出和固定示例不扣次。</li>
  <li>次数绑定账号，当前不设到期日。材料、报告和求职准备资料最多保留 7 天，到期前请自行导出；删除已生成结果不退回次数，订单和次数记录独立保留。</li>
  <li>如需退款，在订单中申请并联系运营者协商；申请后该订单剩余次数暂停使用。运营者在支付宝或微信完成退款后登记结果，登记退款会收回该订单全部未用次数，并停止该订单已开始练习的后续生成。</li>
  <li>AI 建议需要本人核对，不承诺录用结果。可先查看完整示例，确认功能符合需要后再购买。</li>
</ul></section>`;

export class BillingUI {
  constructor(helpers) {
    Object.assign(this, helpers);
    this.root = document.querySelector('#billing-root');
    this.token = 0; this.active = false; this.busy = false; this.drafts = new Map(); this.checkoutKeys = new Map();
    this.root.addEventListener('submit', event => this.submit(event));
    this.root.addEventListener('click', event => this.click(event));
    this.root.addEventListener('input', event => {
      const form = event.target.closest('form[data-draft]');
      if (form) this.drafts.set(`${this.route}:${form.dataset.draft}`, Object.fromEntries(new FormData(form)));
      if (event.target.closest('#billing-settings')) { this.settingsDirty = true; const note = this.root.querySelector('[data-dirty]'); if (note) note.hidden = false; }
    });
    this.root.addEventListener('change', event => {
      if (event.target.id === 'billing-qr-file') this.uploadQR(event.target.files[0]);
      if (event.target.id === 'admin-user-action') this.updateUserAction();
    });
    this.root.addEventListener('error', event => {
      if (event.target.matches('img.billing-qr')) { event.target.hidden = true; event.target.nextElementSibling.hidden = false; }
    }, true);
    window.addEventListener('beforeunload', event => {
      if (this.settingsDirty || this.recovery) { event.preventDefault(); event.returnValue = ''; }
    });
  }
  stop() { this.captureSettings(); this.active = false; this.token++; clearTimeout(this.timer); }
  go(route) { if (location.hash === `#${route}`) return this.open(route); location.hash = route; }
  message(value) {
    const el = this.root.querySelector('#billing-message');
    if (el) { el.textContent = value; el.hidden = !value; if (value) el.scrollIntoView({block:'nearest'}); }
    else if (value) this.toast(value);
  }
  shell(content, title) {
    document.title = `${title} · 岗位罗盘`;
    this.root.innerHTML = `<div class="billing-view"><p id="billing-message" class="form-error billing-alert" role="alert" hidden></p>${content}</div>`;
  }
  heading(title, description, action = '') {
    return `<div class="billing-heading"><div><p class="eyebrow">岗位罗盘 · ${this.isAdmin ? '管理后台' : '账号与次数'}</p><h1>${title}</h1><p>${description}</p></div>${action ? `<div class="billing-actions">${action}</div>` : ''}</div>`;
  }
  tabs() { return '<nav class="billing-tabs" aria-label="账号页面"><a href="#account" '+(this.route === 'account' ? 'aria-current="page"' : '')+'>我的账号</a><a href="#plans" '+(this.route === 'plans' ? 'aria-current="page"' : '')+'>购买次数</a><a href="#history">我的报告 ↗</a></nav>'; }
  adminTabs() { return `<nav class="billing-tabs" aria-label="管理页面"><a href="#admin" ${this.route === 'admin' || this.route.startsWith('admin/order/') ? 'aria-current="page"' : ''}>订单核对</a><a href="#admin/users" ${this.route.startsWith('admin/users') ? 'aria-current="page"' : ''}>用户管理</a><a href="#admin/settings" ${this.route === 'admin/settings' ? 'aria-current="page"' : ''}>收款与套餐</a><a href="#account">返回账号页 ↗</a></nav>`; }
  draft(name) { return this.drafts.get(`${this.route}:${name}`) || {}; }
  async request(path, body, admin = false, raw = false) {
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 20000);
    try {
      const headers = {Accept:'application/json'};
      if (body !== undefined) {
        headers['Content-Type'] = raw ? body.type || 'application/octet-stream' : 'application/json';
        headers['X-CSRF-Token'] = admin ? this.admin?.csrf || '' : this.config()?.csrf || '';
      }
      const response = await fetch(path, {method:body === undefined ? 'GET' : 'POST', body:body === undefined ? undefined : raw ? body : JSON.stringify(body), headers, credentials:'same-origin', cache:'no-store', signal:controller.signal});
      const result = await response.json().catch(() => ({}));
      if (!response.ok) { const error = new Error(result.error || '请求未完成，请稍后重试。'); error.status = response.status; error.code = result.code; throw error; }
      return result;
    } catch (error) {
      if (error.name === 'AbortError' || error instanceof TypeError) throw new Error('连接中断或请求超时。请刷新页面核对结果后再重试。');
      throw error;
    } finally { clearTimeout(timer); }
  }
  async recoverSession(error, token) {
    if (error.status !== 401 || this.isAdmin || !this.active || token !== this.token) return false;
    this.recovery = null; this.overview = null; this.order = null;
    this.drafts.clear(); this.checkoutKeys.clear();
    await this.onAuth('logout');
    if (this.active && token === this.token) {
      this.renderAuth('login');
      this.message('登录已失效，请重新登录。账号、次数和订单仍保留。');
    }
    return true;
  }
  async open(route) {
    this.stop(); this.active = true;
    const token = this.token;
    [this.route, this.query = ''] = route.split('?');
    this.params = new URLSearchParams(this.query);
    this.page = Math.max(0, Math.min(1000, Number(this.params.get('page')) || 0));
    this.isAdmin = this.route === 'admin' || this.route.startsWith('admin/');
    this.shell('<div class="billing-empty" role="status">正在读取…</div>', this.isAdmin ? '管理后台' : '账号与次数');
    try {
      if (this.isAdmin) {
        const session = await this.request('/api/admin/session');
        if (!this.active || token !== this.token) return;
        this.admin = session;
        if (!session.authenticated) { this.renderAdminLogin(); return; }
        if (this.route === 'admin/users') {
          const data = await this.request(`/api/admin/users?${this.params}`, undefined, true);
          if (this.active && token === this.token) { this.userListReturn = route; this.renderAdminUsers(data); }
          return;
        }
        if (/^admin\/users\/[a-f0-9]{32}$/.test(this.route)) { await this.loadUser(token); return; }
        if (this.route === 'admin/settings') {
          if (!this.settingsDraft) this.settingsDraft = await this.request('/api/admin/billing', undefined, true);
          if (this.active && token === this.token) this.renderSettings();
          return;
        }
        if (this.route === 'admin') {
          const data = await this.request(`/api/admin/orders?${this.params}`, undefined, true);
          if (this.active && token === this.token) this.renderAdminOrders(data.orders);
          return;
        }
      } else {
        const overview = await this.request('/api/billing');
        if (!this.active || token !== this.token) return;
        this.overview = overview; this.onOverview(overview);
        if (this.route.startsWith('account/')) { this.renderAuth(); return; }
        if (this.route === 'plans') { this.renderPlans(); return; }
        if (!overview.account) { this.renderAuth('login'); return; }
        if (this.route === 'account') {
          const {orders} = await this.request(`/api/billing/orders?page=${this.page}`);
          if (this.active && token === this.token) this.renderAccount(orders);
          return;
        }
      }
      if (/^(admin\/)?order\/[a-f0-9]{32}$/.test(this.route)) { await this.loadOrder(token); return; }
      throw new Error('没有找到这个账号页面。');
    } catch (error) {
      if (!this.active || token !== this.token) return;
      if (await this.recoverSession(error, token)) return;
      this.shell(`${this.heading('暂时无法打开', h(error.message))}<div class="billing-actions"><button class="button secondary" data-billing="reload">重新读取</button><a class="text-link" href="#account">我的账号</a><a class="text-link" href="#admin">管理后台</a></div>`, '访问提示');
    }
  }
  renderAuth(mode = this.route.split('/')[1] || 'login') {
    if (!['login','register','reset'].includes(mode)) mode = 'login';
    const creating = mode === 'register', resetting = mode === 'reset';
    const title = creating ? '创建你的账号' : resetting ? '用恢复码重设密码' : '登录你的账号';
    const next = this.params.get('next') || (this.route.startsWith('order/') || this.route === 'plans' ? this.route : 'account');
    this.authNext = /^(account|plans|start|order\/[a-f0-9]{32}|prepare\/[a-f0-9]{32}\/(refine|versions|interview))$/.test(next) ? next : 'account';
    const suffix = `?next=${encodeURIComponent(this.authNext)}`;
    this.shell(`<div class="billing-auth">${this.heading(title, resetting ? '账号恢复码以 JCA- 开头，与报告恢复码不同。重设后旧密码、旧恢复码和其他登录会话都会失效。' : creating ? '注册即赠诊断 1 次、简历精修 1 轮、岗位适配 1 次、文字面试 1 场，无需付款即可体验。' : '体验次数、购买次数与订单保存在账号里，换浏览器后登录即可继续使用。')}
      <section class="billing-card"><form class="billing-form" data-form="auth"><fieldset><input type="hidden" name="mode" value="${mode}">
      <label class="billing-field">账号名<input name="username" required pattern="[A-Za-z0-9][A-Za-z0-9_]{3,31}" minlength="4" maxlength="32" autocomplete="username" autocapitalize="none" spellcheck="false"><small>4—32 位字母、数字或下划线，不区分大小写。</small></label>
      ${resetting ? '<label class="billing-field">账号恢复码<input name="recovery_code" required minlength="52" maxlength="60" placeholder="JCA-…" autocomplete="off" spellcheck="false"></label>' : ''}
      <label class="billing-field">${resetting ? '新密码' : '密码'}<input name="password" type="password" required minlength="10" maxlength="128" autocomplete="${creating || resetting ? 'new-password' : 'current-password'}"><small>至少 10 个字符，请使用独立的长密码。</small></label>
      ${creating || resetting ? '<label class="billing-field">再次输入密码<input name="repeat" type="password" required minlength="10" maxlength="128" autocomplete="new-password"></label>' : ''}
      ${!resetting && !this.overview?.account ? '<label class="billing-check"><input type="checkbox" name="claim_reports" checked><span>将当前匿名浏览器可访问的报告加入这个账号。</span></label>' : ''}
      ${creating ? '<p class="billing-caption">每个账号仅赠送一次；精修包含追问和一次改写，面试包含最多 5 题及复盘。创建后请保存账号恢复码；忘记密码时需要它。</p>' : ''}
      <button type="submit" class="button primary full-width">${title} ↗</button></fieldset></form>
      <div class="billing-actions">${!creating ? `<a class="text-link" href="#account/register${suffix}">创建账号</a>` : `<a class="text-link" href="#account/login${suffix}">已有账号，登录</a>`}${!resetting ? `<a class="text-link muted" href="#account/reset${suffix}">忘记密码</a>` : `<a class="text-link" href="#account/login${suffix}">返回登录</a>`}</div></section><p class="billing-help">账号恢复码只能由本人保管。报告恢复码仅用于恢复报告，不授予账号或购买次数。</p></div>`, title);
  }
  recoveryCard() {
    if (!this.recovery) return '';
    return `<aside class="billing-note billing-recovery"><strong>请保存这份账号恢复码</strong><p>只显示这一次，重设密码后会更换。请勿公开或发给他人。</p><code>${h(this.recovery.code)}</code><div class="billing-actions"><button class="text-link" data-billing="copy-recovery">复制恢复码</button><button class="text-link" data-billing="download-recovery">下载恢复凭据</button><button class="text-link muted" data-billing="saved-recovery">我已保存，隐藏</button></div></aside>`;
  }
  renderAccount(orders) {
    const o = this.overview;
    this.shell(`${this.heading(h(o.account.username), '次数绑定当前账号。材料到期不会删除购买记录；换设备后使用账号密码登录。', '<button class="text-link muted" data-billing="logout">退出账号</button>')}${this.tabs()}${this.recoveryCard()}
      ${o.account.ai_restricted ? '<p class="billing-note pending" role="status">这个账号暂时限制 AI 生成。已有材料和次数保留，仍可查看、编辑、导出和处理订单售后。如需恢复，请联系运营者。</p>' : ''}
      ${!o.enabled ? '<p class="billing-note">目前暂未开放购买，生成沿用当前体验规则。已有订单仍可查询、核款及申请退款。</p>' : ''}
      ${o.account.welcome_granted ? '<aside class="billing-note"><strong>新用户体验已赠送</strong><p>诊断、精修、岗位适配、文字面试各一次，优先使用赠送次数。每个账号仅领取一次，重新登录不会重置；最终生成失败会退回对应次数。</p></aside>' : ''}
      <div class="billing-credit-grid">${Object.keys(kinds).map(k => { const b = o.wallet[k], trial = b.trial_available || 0; return `<article class="billing-credit"><h2>${kinds[k]}</h2><strong>${b.available}</strong><small>${units[k]}可用</small><p class="billing-credit-sources"><span>免费体验 ${trial}</span><span>已购 ${b.available - trial}</span></p><p>预占 ${b.reserved} · 已用 ${b.used}${b.on_hold ? ` · 退款暂停 ${b.on_hold}` : ''}</p></article>`; }).join('')}</div>
      <div class="billing-actions"><a class="button primary" href="#plans">查看套餐与规则 ↗</a><a class="button secondary" href="#${h(this.authNext && this.authNext !== 'account' ? this.authNext : 'start')}">继续使用</a></div>
      <section class="billing-card billing-details"><h2>我的订单</h2>${this.orderTable(orders)}${this.pager(orders, 'account')}</section>
      <details class="billing-card billing-details"><summary>最近的次数记录 · ${o.events.length} 条</summary>${o.events.length ? `<div class="billing-table-wrap"><table class="billing-table"><thead><tr><th>时间</th><th>功能</th><th>变动</th><th>来源</th></tr></thead><tbody>${o.events.map(e => `<tr><td>${h(this.dateText(e.created_at, true))}</td><td>${kinds[e.kind]}</td><td>${eventNames[e.event] || h(e.event)} · ${e.units}</td><td>${e.order_id ? `<a href="#order/${h(e.order_id)}">${h(e.order_id.slice(0,8))}</a>` : '新用户体验'}</td></tr>`).join('')}</tbody></table></div>` : '<p class="billing-empty">还没有次数变动。</p>'}</details>
      ${o.support ? `<p class="billing-help">订单咨询：${h(o.support)}</p>` : ''}`, '我的账号');
  }
  renderPlans() {
    const o = this.overview;
    this.shell(`${this.heading('为下一次机会做好准备', '按需要购买次数。先查看示例，再决定是否适合自己的求职阶段。', '<a class="text-link" href="#prepare/example/refine">体验完整示例 ↗</a>')}${this.tabs()}
      ${!o.account ? '<p class="billing-note">新账号注册可免费体验诊断、精修、岗位适配和文字面试各一次。<a class="text-link" href="#account/register?next=start">注册并领取 ↗</a></p>' : ''}
      ${!o.enabled ? '<section class="billing-card billing-disabled-copy"><h2>购买暂未开放</h2><p class="billing-caption">收款码和套餐准备好后，会在这里开放购买。现在可以使用现有体验功能和固定示例。</p><div class="billing-actions"><a class="text-link" href="#start">开始诊断 ↗</a></div></section>' : `<div class="billing-plans">${o.plans.map(plan => `<article class="billing-plan"><p class="eyebrow">按次使用 · 人工确认到账</p><h2>${h(plan.name)}</h2><div class="billing-price">${money(plan.price_cents)}<small>一次购买，按功能使用</small></div>${included(plan.credits)}${o.account ? `<form class="billing-form" data-form="checkout"><input type="hidden" name="plan_id" value="${h(plan.id)}"><fieldset><label class="billing-check"><input type="checkbox" name="confirmed" required><span>我已阅读下方规则，接受人工核款、次数扣除和退款方式。</span></label><button class="button primary full-width" ${o.purchasing_available ? '' : 'disabled'}>创建订单，查看收款码 ↗</button></fieldset></form>` : '<a class="button primary" href="#account/login?next=plans">登录后购买 ↗</a>'}</article>`).join('')}</div>${!o.purchasing_available ? '<p class="billing-note">生成服务暂不可用，请稍后再购买。</p>' : ''}`}
      ${rules}${o.support ? `<p class="billing-help">付款或退款咨询：${h(o.support)}</p>` : ''}`, '购买次数');
  }
  orderTable(orders, admin = false) {
    if (!orders.length) return '<p class="billing-empty">这里还没有订单。</p>';
    return `<div class="billing-table-wrap"><table class="billing-table"><thead><tr><th>订单 / 套餐${admin ? ' / 账号' : ''}</th><th>金额</th><th>状态</th><th></th></tr></thead><tbody>${orders.map(o => `<tr><td>${h(o.plan_name)}<small>${h(o.id.slice(0,12))} · ${h(this.dateText(o.created_at, true))}</small>${admin ? `<small>${o.account_id ? `<a href="#admin/users/${h(o.account_id)}">${h(o.username)}</a>` : h(o.username)}</small>` : ''}</td><td>${money(o.amount_cents)}${o.refunded_cents ? `<small>退 ${money(o.refunded_cents)}</small>` : ''}</td><td>${this.tag(o)}</td><td><a href="#${admin ? 'admin/' : ''}order/${h(o.id)}">${admin ? '核对' : '查看'} ↗</a></td></tr>`).join('')}</tbody></table></div>`;
  }
  pager(orders, route) {
    const link = page => { const p = new URLSearchParams(this.params); p.set('page', page); return `#${route}?${p}`; };
    return `<nav class="billing-pager" aria-label="订单分页">${this.page ? `<a href="${h(link(this.page-1))}">上一页</a>` : ''}<span>第 ${this.page+1} 页</span>${orders.length === 30 && this.page < 1000 ? `<a href="${h(link(this.page+1))}">下一页</a>` : ''}</nav>`;
  }
  orderStatus(order) { return order.status === 'awaiting_payment' && order.expires_at * 1000 <= Date.now() ? 'expired' : order.status; }
  tag(o) { const status = this.orderStatus(o); return `<span class="billing-tag ${status}">${o.refund_requested_at ? (o.refund_reviewing ? '退款处理中' : '已申请退款') : statuses[status]}</span>`; }
  async loadOrder(token = this.token, poll = false) {
    if (poll && this.busy) { this.timer = setTimeout(() => this.loadOrder(token, true), 3000); return; }
    const id = this.route.split('/').at(-1), admin = this.isAdmin;
    try {
      const order = await this.request(`/api/${admin ? 'admin' : 'billing'}/orders/${id}`, undefined, admin);
      if (!this.active || token !== this.token) return;
      const changed = !this.order || this.order.id !== order.id || this.order.revision !== order.revision || this.order.active_tasks !== order.active_tasks || this.orderStatus(this.order) !== this.displayedStatus;
      this.order = order;
      if (!poll || changed) this.renderOrder();
      clearTimeout(this.timer);
      if (order.status === 'submitted' || order.status === 'awaiting_payment' || order.refund_requested_at) this.timer = setTimeout(() => this.loadOrder(token, true), 15000);
    } catch (error) {
      if (!this.active || token !== this.token) return;
      if (!poll) throw error;
      this.message(`${error.message} 可用“刷新状态”重新核对。`);
    }
  }
  qr(src) { return `<img class="billing-qr" src="${h(src)}" alt="收款二维码"><p class="billing-qr-placeholder" hidden>图片暂时无法加载，请刷新订单重试。</p>`; }
  renderOrder() {
    const o = this.order, s = o.snapshot, status = this.orderStatus(o), admin = this.isAdmin;
    this.displayedStatus = status;
    const paid = ['paid','refunded'].includes(status);
    const heading = admin ? '核对这笔订单' : paid ? '你的购买记录' : '核对金额，再完成付款';
    this.shell(`${this.heading(heading, admin ? '请在收款平台核实交易、金额和付款人后登记。用户提交的单号仅作为核对线索。' : '每笔订单只付一次。付款后提交完整交易单号，等待人工核实到账。', '<button class="text-link" data-billing="reload-order">刷新状态 ↻</button>')}${admin ? this.adminTabs() : this.tabs()}
      <article class="billing-receipt ${status === 'awaiting_payment' && !admin ? 'payment-open' : ''}"><div class="billing-receipt-main"><p class="billing-order-number">ORDER / ${h(o.id)}</p><h2>${h(o.plan_name)}</h2>${this.tag(o)}<div class="billing-price">${money(o.amount_cents)}</div>
      <ol class="billing-order-steps"><li class="done"><span>01</span>创建订单</li><li class="${o.claim ? 'done' : ''}"><span>02</span>提交交易单号</li><li class="${paid ? 'done' : ''}"><span>03</span>核实并发放</li></ol>${included(s.plan.credits)}
      <dl class="billing-description-list"><div><dt>账号</dt><dd>${admin && o.account_id ? `<a class="text-link" href="#admin/users/${h(o.account_id)}">${h(o.username)} ↗</a>` : h(o.username || this.overview?.account?.username)}</dd></div><div><dt>创建时间</dt><dd>${h(this.dateText(o.created_at, true))}</dd></div><div><dt>收款方式</dt><dd>${s.channel === 'alipay' ? '支付宝' : '微信'}</dd></div><div><dt>收款名称</dt><dd>${h(s.payee)}</dd></div>${o.paid_at ? `<div><dt>发放时间</dt><dd>${h(this.dateText(o.paid_at, true))}</dd></div>` : ''}${o.refunded_cents ? `<div><dt>累计已退款</dt><dd>${money(o.refunded_cents)}</dd></div>` : ''}</dl>
      ${o.claim ? `<div class="billing-note"><p><strong>已提交的交易单号</strong><br><span class="billing-order-number">${h(o.claim.receipt)}</span></p>${o.claim.note ? `<p>${h(o.claim.note)}</p>` : ''}</div>` : ''}
      ${o.review ? `<p class="billing-note"><strong>核对结果</strong><br>${h(o.review.note || '已核实到账，次数已发放。')}${admin && o.review.receipt ? `<br>登记收款单号：${h(o.review.receipt)}` : ''}</p>` : ''}
      ${o.refunds?.length ? `<details class="billing-details"><summary>已登记退款 · ${o.refunds.length} 笔</summary>${o.refunds.map(r => `<p class="billing-note">${money(r.amount_cents)} · ${h(this.dateText(r.created_at, true))}<br>退款单号：${h(r.receipt)}<br>${h(r.note)}</p>`).join('')}</details>` : ''}
      ${admin ? this.adminReview(o) : this.customerActions(o)}
      <p class="billing-help">联系运营者：${h(s.support)}<br>请提供订单编号和交易单号，勿发送账号密码或恢复码。</p></div>
      <aside class="billing-receipt-side">${status === 'awaiting_payment' && !admin ? `<p class="eyebrow">使用${s.channel === 'alipay' ? '支付宝' : '微信'}扫码</p><p class="billing-qr-summary"><strong>${money(o.amount_cents)}</strong><br>${h(s.payee)}</p>${this.qr(`/api/billing/orders/${o.id}/qr`)}<p class="billing-caption">扫码后请核对收款名称和金额，支付 <strong>${money(o.amount_cents)}</strong>。付款备注可填写订单前 8 位：${h(o.id.slice(0,8))}。</p><p class="billing-caption">请在 ${h(this.dateText(o.expires_at, true))} 前付款，过期后不要继续使用此订单付款。</p>` : `<div class="billing-qr-placeholder">${paid ? '到账记录已保留<br>请勿重复付款' : status === 'submitted' ? '已提交，等待人工核款<br>请勿重复付款' : admin ? '以收款平台实际到账记录为准' : '订单已停止收款<br>已经付款仍可提交核对'}</div>`}
      <div class="billing-note"><strong>人工核款说明</strong><p>${h(s.notice)}</p></div>${paid ? `<h3 class="billing-help">本订单剩余次数${o.refund_requested_at ? '（退款期间暂停使用）' : ''}</h3>${included(o.remaining)}` : ''}<p class="billing-caption">订单保留创建时的套餐与收款资料，之后调整套餐不会改变本订单。</p></aside></article>${rules}`, admin ? '订单核对' : '订单详情');
  }
  customerActions(o) {
    const status = this.orderStatus(o), draft = this.draft('claim');
    if (status === 'submitted') return '<p class="billing-note pending">交易单号已提交，正在等待人工核实。核款完成后，本页会更新状态，次数发放到当前账号。</p>';
    if (['awaiting_payment','expired','cancelled','rejected'].includes(status)) {
      const form = `<form class="billing-form billing-review-form" data-form="claim" data-draft="claim"><fieldset><label class="billing-field">支付平台完整交易单号<input name="receipt" required minlength="8" maxlength="80" pattern="[A-Za-z0-9_-]{8,80}" value="${h(draft.receipt ?? o.claim?.receipt ?? '')}" spellcheck="false" autocomplete="off"><small>在支付宝或微信账单的交易详情中复制完整交易单号。</small></label><label class="billing-field">付款说明（选填）<textarea name="note" rows="2" maxlength="300" placeholder="例如付款时间、付款人名称，便于核对。">${h(draft.note ?? o.claim?.note ?? '')}</textarea></label><label class="billing-check"><input type="checkbox" name="confirmed" required><span>我已实际付款，确认金额和交易单号准确，理解需要人工核实到账。</span></label><button class="button primary">我已付款，提交核对 ↗</button></fieldset></form>`;
      return `${status === 'awaiting_payment' ? form : `<details class="billing-details"><summary>已经付款？提交或补充交易单号</summary>${form}</details>`}${['awaiting_payment','expired','rejected'].includes(status) ? '<div class="billing-actions billing-details"><button class="billing-quiet-button" data-billing="cancel">未付款，取消此订单</button></div>' : ''}`;
    }
    if (status === 'paid') return `<div class="billing-actions billing-details"><a class="button primary" href="#account">查看全部次数 ↗</a>${o.refund_requested_at ? (o.refund_reviewing ? '<p class="billing-caption">运营者已开始处理退款，暂不能撤回。如需变更，请联系运营者。</p>' : '<button class="text-link muted" data-billing="withdraw_refund">撤回退款申请</button>') : '<button class="text-link muted" data-billing="request_refund">申请退款</button>'}</div>${o.refund_requested_at ? '<p class="billing-note pending">本订单剩余次数已暂停使用。请联系运营者确认退款金额与处理进度。</p>' : ''}`;
    return '<p class="billing-note">本订单已退款，未用次数已收回。已有订单和已生成材料仍按各自保留规则保存。</p>';
  }
  renderAdminLogin() {
    this.shell(`<div class="billing-auth">${this.heading('管理后台', '查看用户、处理使用权限，核对到账与退款记录。')}<section class="billing-card">${this.admin.configured ? '<form class="billing-form" data-form="admin-login"><fieldset><label class="billing-field">管理密钥<input name="key" type="password" minlength="32" maxlength="128" required autocomplete="current-password"></label><button class="button primary full-width">进入管理后台 ↗</button></fieldset></form>' : '<p class="billing-note">管理入口尚未配置。请在服务器设置 BILLING_ADMIN_KEY 后重启服务。</p>'}</section></div>`, '管理后台');
  }
  userTag(user) { return `<span class="admin-user-status ${user.ai_restricted ? 'restricted' : ''}">${user.ai_restricted ? '限制生成' : '正常使用'}</span>`; }
  renderAdminUsers(data) {
    const filtered = Boolean(this.params.get('q') || this.params.get('status'));
    const pageLink = page => { const p = new URLSearchParams(this.params); p.set('page', page); return `#admin/users?${p}`; };
    const rows = data.users.map(u => `<tr>
      <td class="admin-user-identity"><a class="admin-user-name" href="#admin/users/${h(u.id)}">${h(u.username)}</a>${this.userTag(u)}<small>${h(u.id.slice(0,12))}</small></td>
      <td class="admin-user-balance-cell"><div class="admin-user-balances">${Object.keys(kinds).map(k => `<span>${kinds[k]} <strong>${u.wallet[k].available}</strong></span>`).join('')}</div></td>
      <td data-label="订单">${u.order_count} 笔</td>
      <td class="admin-user-dates"><span>注册 ${h(this.dateText(u.created_at))}</span><small>最近登录 ${u.last_login_at ? h(this.dateText(u.last_login_at, true)) : '暂无记录'}</small></td>
      <td class="admin-user-detail-link"><a href="#admin/users/${h(u.id)}" aria-label="查看 ${h(u.username)} 的用户详情">查看详情 ↗</a></td></tr>`).join('');
    this.shell(`${this.heading('用户管理', '核对每个账号的次数、订单和使用状态。', '<button class="text-link muted" data-billing="admin-logout">退出管理</button>')}${this.adminTabs()}
      <div class="admin-user-stats"><span>全部用户 <strong>${data.stats.total}</strong></span><span>近 7 日新增 <strong>${data.stats.new_week}</strong></span><span>限制生成 <strong>${data.stats.restricted}</strong></span></div>
      <form class="billing-filter billing-form admin-user-filter" data-form="user-filter"><label class="billing-field">搜索用户<input name="q" maxlength="64" value="${h(this.params.get('q') || '')}" placeholder="输入账号名或用户编号" autocomplete="off"></label><label class="billing-field">使用状态<select name="status">${[['','全部状态'],['active','正常使用'],['restricted','限制生成']].map(([value,label]) => `<option value="${value}" ${this.params.get('status') === value ? 'selected' : ''}>${label}</option>`).join('')}</select></label><button class="button secondary">搜索用户</button>${filtered ? '<a class="text-link" href="#admin/users">清除筛选</a>' : ''}</form>
      <section class="billing-card admin-user-list"><div class="admin-user-section-heading"><h2>${filtered ? '筛选结果' : '全部用户'}</h2><span>${data.total} 个账号 · 按注册时间倒序</span></div>
      ${data.users.length ? `<table class="billing-table admin-user-table"><thead><tr><th>账号 / 状态</th><th>各项可用次数</th><th>订单</th><th>注册 / 登录</th><th><span class="admin-user-sr-only">操作</span></th></tr></thead><tbody>${rows}</tbody></table>` : `<p class="billing-empty">${filtered ? '没有匹配的用户。请调整账号名或状态筛选。' : '还没有注册用户。用户注册后会显示在这里。'}</p>`}
      <nav class="billing-pager" aria-label="用户分页">${data.page > 0 ? `<a href="${h(pageLink(data.page-1))}">上一页</a>` : '<span></span>'}<span>第 ${data.page+1} / ${Math.max(1,Math.ceil(data.total/data.page_size))} 页</span>${(data.page+1)*data.page_size < data.total ? `<a href="${h(pageLink(data.page+1))}">下一页</a>` : '<span></span>'}</nav></section>`, '用户管理');
  }
  async loadUser(token = this.token) {
    const detail = await this.request(`/api/admin/users/${this.route.split('/').at(-1)}`, undefined, true);
    if (!this.active || token !== this.token) return;
    this.userDetail = detail; this.renderAdminUser();
  }
  updateUserAction() {
    const action = this.root.querySelector('#admin-user-action')?.value;
    const help = this.root.querySelector('#admin-user-action-help'), button = this.root.querySelector('#admin-user-action-submit');
    if (help) help.textContent = userActionHelp[action] || '选择要执行的操作，填写原因后确认。';
    if (button) { button.textContent = userActions[action] || '选择一项操作'; button.disabled = !action; }
  }
  renderAdminUser() {
    const d = this.userDetail, u = d.user, note = this.draft('user-note'), actionDraft = this.draft('user-action');
    const options = [...(!u.welcome_granted ? ['grant_welcome'] : []), u.ai_restricted ? 'restore' : 'restrict', 'revoke_sessions'];
    const selected = options.includes(actionDraft.action) ? actionDraft.action : '';
    const back = this.userListReturn || 'admin/users';
    this.shell(`${this.heading(h(u.username), '查看账号权益，处理使用权限与服务记录。', `<a class="text-link" href="#${h(back)}">← 返回用户列表</a><button class="text-link" data-billing="reload-user">刷新状态 ↻</button>`)}${this.adminTabs()}
      <div class="admin-user-summary">${this.userTag(u)}<span class="admin-user-id">用户编号 ${h(u.id)}</span><span>${u.welcome_granted ? '已领取新用户体验' : '尚未领取新用户体验'}</span></div>
      <dl class="admin-user-facts"><div><dt>注册时间</dt><dd>${h(this.dateText(u.created_at,true))}</dd></div><div><dt>最近登录</dt><dd>${u.last_login_at ? h(this.dateText(u.last_login_at,true)) : '暂无记录'}</dd></div><div><dt>有效登录</dt><dd>${d.active_sessions} 个会话</dd></div><div><dt>仍在保留期的报告</dt><dd>${d.report_count} 份</dd></div></dl>
      <div class="admin-user-layout"><div class="admin-user-main">
      <section class="billing-card"><div class="admin-user-section-heading"><h2>次数与使用情况</h2><span>生成时优先使用免费体验</span></div><p class="admin-user-mobile-hint">表格可左右滑动查看。</p><div class="billing-table-wrap"><table class="billing-table admin-user-wallet"><thead><tr><th>功能</th><th>可用</th><th>免费体验</th><th>已购可用</th><th>生成中</th><th>已使用</th><th>退款暂停</th></tr></thead><tbody>${Object.keys(kinds).map(k => { const b=u.wallet[k]; return `<tr><td>${kinds[k]}<small>按${units[k]}计次</small></td><td><strong>${b.available}</strong></td><td>${b.trial_available}</td><td>${b.available-b.trial_available}</td><td>${b.reserved}</td><td>${b.used}</td><td>${b.on_hold}</td></tr>`; }).join('')}</tbody></table></div></section>
      <section class="billing-card"><div class="admin-user-section-heading"><h2>关联订单 <small>${u.order_count} 笔</small></h2><a class="text-link" href="#admin?owner=${h(u.id)}">查看全部 ↗</a></div><p class="billing-caption">累计实收 ${money(d.paid_cents)}，已扣除登记退款。${d.orders.length > 5 ? '下方显示最近 5 笔。' : ''}</p>${this.orderTable(d.orders.slice(0,5),true)}</section>
      <section class="billing-card"><h2>管理操作记录</h2>${d.actions.length ? `<ol class="admin-user-audit">${d.actions.map(a => `<li><div><strong>${h(userActions[a.action] || a.action)}</strong><time>${h(this.dateText(a.created_at,true))}</time></div><p>${h(a.note || '已清空管理备注。')}${a.action === 'revoke_sessions' ? ` · 已退出 ${a.revoked_sessions} 个会话` : ''}</p></li>`).join('')}</ol><p class="billing-caption">显示最近 30 条记录，完整记录持续保留。</p>` : '<p class="billing-empty">还没有管理操作。保存备注或调整账号后，记录会显示在这里。</p>'}</section>
      <details class="billing-card billing-details"><summary>最近的次数记录 · ${d.events.length} 条</summary>${d.events.length ? `<div class="billing-table-wrap"><table class="billing-table"><thead><tr><th>时间</th><th>功能</th><th>变动</th><th>来源</th></tr></thead><tbody>${d.events.map(e => `<tr><td>${h(this.dateText(e.created_at,true))}</td><td>${kinds[e.kind] || h(e.kind)}</td><td>${h(eventNames[e.event] || e.event)} · ${e.units}</td><td>${e.order_id ? `<a href="#admin/order/${h(e.order_id)}">${h(e.order_id.slice(0,8))}</a>` : '免费体验'}</td></tr>`).join('')}</tbody></table></div>` : '<p class="billing-empty">还没有次数变动。</p>'}</details></div>
      <aside class="admin-user-aside"><section class="billing-card"><h2>管理备注</h2><form class="billing-form" data-form="user-note" data-draft="user-note"><fieldset><label class="billing-field">跟进信息<textarea name="note" rows="4" maxlength="600" placeholder="记录需跟进的事项，避免填写密码或恢复码。">${h(note.note ?? d.note)}</textarea><small>仅管理后台可见，最多 600 个字符。</small></label><button class="button secondary full-width">保存管理备注</button></fieldset></form></section>
      <section class="billing-card"><h2>账号操作</h2><form class="billing-form" data-form="user-action" data-draft="user-action"><fieldset><label class="billing-field">选择操作<select id="admin-user-action" name="action" required><option value="">请选择</option>${options.map(action => `<option value="${action}" ${action===selected ? 'selected' : ''}>${userActions[action]}</option>`).join('')}</select></label><p id="admin-user-action-help" class="billing-note">${h(userActionHelp[selected] || '选择要执行的操作，填写原因后确认。')}</p><label class="billing-field">操作原因<textarea name="note" rows="3" minlength="4" maxlength="600" required placeholder="例如：用户反馈未领取注册体验，经核对后补发。">${h(actionDraft.note || '')}</textarea><small>写明原因，供之后查询；用户端不会显示。</small></label><button id="admin-user-action-submit" class="button secondary full-width" ${selected ? '' : 'disabled'}>${userActions[selected] || '选择一项操作'}</button></fieldset></form>${u.welcome_granted ? '<p class="billing-help">此账号已领取四项体验，不能重复补发。</p>' : ''}</section></aside></div>`, '用户详情');
  }
  renderAdminOrders(orders) {
    this.shell(`${this.heading('到账核对，逐笔有记录', '只在确认实际收款后发放次数。退款需先在支付平台完成，再回到订单登记。', '<button class="text-link muted" data-billing="admin-logout">退出管理</button>')}${this.adminTabs()}
      ${this.params.get('owner') ? `<p class="billing-note">正在查看这位用户的订单。<a class="text-link" href="#admin/users/${h(this.params.get('owner'))}">返回用户详情</a> · <a class="text-link" href="#admin">查看全部订单</a></p>` : ''}
      <form class="billing-filter billing-form" data-form="filter"><label class="billing-field">订单状态<select name="status">${[['','全部'],['submitted','待人工核款'],['refund_requested','待处理退款'],['paid','已发放'],['rejected','需补充核对'],['awaiting_payment','待付款'],['refunded','已退款'],['cancelled','已取消'],['expired','已过期']].map(([v,label]) => `<option value="${v}" ${this.params.get('status') === v ? 'selected' : ''}>${label}</option>`).join('')}</select></label><label class="billing-field">搜索账号或订单编号<input name="q" maxlength="64" value="${h(this.params.get('q') || '')}" placeholder="输入账号名或订单编号"></label><button class="button secondary">筛选订单</button></form><section class="billing-card">${this.orderTable(orders, true)}${this.pager(orders, 'admin')}</section>`, '订单核对');
  }
  captureSettings() {
    const form = this.root.querySelector('#billing-settings');
    if (!form || !this.settingsDraft) return;
    const value = name => form.elements.namedItem(name)?.value, checked = name => Boolean(form.elements.namedItem(name)?.checked), draft = this.settingsDraft;
    for (const key of ['payee','channel','notice','support']) draft[key] = value(key) || '';
    draft.enabled = checked('enabled');
    for (const [i, plan] of draft.plans.entries()) {
      plan.name = value(`name-${i}`) || ''; plan.priceText = value(`price-${i}`) ?? ''; plan.active = checked(`active-${i}`);
      for (const k of Object.keys(kinds)) plan.credits[k] = value(`${k}-${i}`) ?? '';
    }
  }
  renderSettings() {
    const s = this.settingsDraft;
    this.shell(`${this.heading('收款方式与套餐', '先上传真实收款码，填写价格、包含次数、核款时间和联系方式。确认无误后再开放购买。', '<button class="text-link muted" data-billing="admin-logout">退出管理</button>')}${this.adminTabs()}
      <form id="billing-settings" class="billing-form" data-form="settings"><fieldset><div class="billing-grid"><section class="billing-card"><h2>收款资料</h2><label class="billing-field">收款方式<select name="channel"><option value="alipay" ${s.channel === 'alipay' ? 'selected' : ''}>支付宝</option><option value="wechat" ${s.channel === 'wechat' ? 'selected' : ''}>微信</option></select></label><label class="billing-field">收款名称<input name="payee" maxlength="80" value="${h(s.payee)}" placeholder="扫码后能核对的收款名称"></label>
      <label class="billing-field">核款说明<textarea name="notice" rows="4" maxlength="600" placeholder="说明通常在多久内核款、如何联系你。">${h(s.notice)}</textarea></label><label class="billing-field">咨询与退款联系方式<textarea name="support" rows="2" maxlength="300" placeholder="例如你的联系微信、邮箱或其他可用方式。">${h(s.support)}</textarea></label></section>
      <section class="billing-card"><h2>个人收款码</h2>${s.qr_code_id ? this.qr(`/api/admin/billing/qr/${s.qr_code_id}`) : '<div class="billing-qr-placeholder">尚未上传收款码</div>'}<label class="billing-field">上传或更换图片<input id="billing-qr-file" type="file" accept="image/png,image/jpeg"><small>PNG / JPEG，1 MB 以内，宽高 96—1,600 像素。</small></label><p class="billing-caption">上传后点击“保存设置”生效。更换图片不改变已有订单保存的收款码。</p></section></div>
      <section class="billing-card billing-details"><h2>可购买的套餐</h2>${s.plans.map((p,i) => `<article class="billing-plan-editor"><div class="billing-fields-row"><label class="billing-field">套餐名称<input name="name-${i}" minlength="2" maxlength="60" required value="${h(p.name)}"></label><label class="billing-field">价格（元）<input name="price-${i}" inputmode="decimal" type="number" step="0.01" min="0" max="1000" required value="${h(p.priceText ?? (p.price_cents/100).toFixed(2))}"></label></div><div class="billing-credit-inputs">${Object.keys(kinds).map(k => `<label class="billing-field">${kinds[k]}（${units[k]}）<input name="${k}-${i}" type="number" min="0" max="100" step="1" required value="${h(p.credits[k] || 0)}"></label>`).join('')}</div><div class="billing-actions"><label class="billing-check"><input name="active-${i}" type="checkbox" ${p.active ? 'checked' : ''}><span>上架此套餐（至少 ¥1，至少包含 1 次服务）</span></label><button type="button" class="text-link danger" data-billing="remove-plan" data-index="${i}">移除此套餐</button></div><p class="billing-caption">套餐编号：${h(p.id)}</p></article>`).join('')}<button type="button" class="text-link" data-billing="add-plan" ${s.plans.length >= 5 ? 'disabled' : ''}>新增套餐 ＋</button>
      <div class="billing-settings-footer"><label class="billing-check"><input name="enabled" type="checkbox" ${s.enabled ? 'checked' : ''}><span><strong>开放购买并按次数使用 AI 功能</strong><br>关闭时，停止新订单，AI 沿用免费体验规则；已有订单仍能查询、核款和退款。</span></label><p class="billing-note">开启前请自行扫码核对名称，确认价格、核款时间与联系方式准确。系统只保存收款码，不会自动查询支付宝或微信账单。</p><p class="billing-dirty" data-dirty ${this.settingsDirty ? '' : 'hidden'}>有尚未保存的设置，离开页面后草稿会保留在当前标签页。</p><div class="billing-actions"><button class="button primary" type="submit">保存设置</button><button class="text-link muted" type="button" data-billing="discard-settings">放弃草稿，读取已保存设置</button></div></div></section></fieldset></form>${rules}`, '收款与套餐');
  }
  adminReview(o) {
    const draft = this.draft('review');
    if (o.status === 'submitted') return `<form class="billing-form billing-review-form" data-form="review" data-draft="review"><fieldset><input type="hidden" name="action" value="confirm"><h3>核实实际到账后发放</h3><label class="billing-field">实际到账金额（元）<input name="amount" type="number" min="0.01" max="1000" step="0.01" required value="${h(draft.amount || '')}" placeholder="请从收款账单核对后输入"></label><label class="billing-field">收款账单中的完整交易单号<input name="receipt" required minlength="8" maxlength="80" pattern="[A-Za-z0-9_-]{8,80}" value="${h(draft.receipt || '')}" autocomplete="off" spellcheck="false"></label><label class="billing-field">核款说明（用户可见）<textarea name="note" rows="3" maxlength="600">${h(draft.note || '')}</textarea></label><label class="billing-check"><input type="checkbox" name="confirmed" required><span>我已在支付宝或微信核实这笔真实到账，金额、单号及付款人与本订单对应。</span></label><div class="billing-actions"><button class="button primary">确认到账并发放次数</button><button type="button" class="text-link danger" data-billing="reject">未核对成功，退回补充</button></div><p class="billing-caption">退回补充需填写至少 4 个字符的说明。</p></fieldset></form>`;
    if (o.status !== 'paid') return '<p class="billing-caption">当前状态无需发放次数。用户提交交易单号后才可核款。</p>';
    if (!o.refund_reviewing) return `<div class="billing-review-form"><p class="billing-caption">${o.refund_requested_at ? '用户已申请退款，未用次数已暂停。' : '需要退款时，先开始退款处理，暂停本订单剩余次数。'}</p><button class="button secondary" data-billing="hold_refund">开始退款处理</button></div>`;
    return `<form class="billing-form billing-review-form" data-form="review" data-draft="review"><fieldset><input type="hidden" name="action" value="refund"><h3>登记已完成的线下退款</h3><p class="billing-note">先与用户确认金额，等待本订单生成任务结束，再在支付平台退款。此按钮仅登记结果，不会转账；登记后收回全部未用次数并停止本订单练习的后续生成。</p><p class="billing-caption">尚可退金额 ${money(o.amount_cents-o.refunded_cents)} · 正在生成 ${o.active_tasks || 0} 个任务${o.active_tasks ? '，请等待完成后再进行实际退款。' : ''}</p><label class="billing-field">本次实际退款金额（元）<input name="amount" type="number" min="0.01" max="${((o.amount_cents-o.refunded_cents)/100).toFixed(2)}" step="0.01" required value="${h(draft.amount || '')}"></label><label class="billing-field">退款平台完整交易单号<input name="receipt" required minlength="8" maxlength="80" pattern="[A-Za-z0-9_-]{8,80}" value="${h(draft.receipt || '')}" autocomplete="off"></label><label class="billing-field">退款说明（用户可见）<textarea name="note" rows="3" minlength="4" maxlength="600" required>${h(draft.note || '')}</textarea></label><label class="billing-check"><input name="confirmed" type="checkbox" required><span>我已在支付平台实际退还上述金额，确认该订单所有未用次数将被收回。</span></label><div class="billing-actions"><button class="button primary" ${o.active_tasks ? 'disabled' : ''}>登记退款并收回未用次数</button><button type="button" class="text-link muted" data-billing="reject_refund">结束退款处理，恢复使用</button></div><p class="billing-caption">结束退款处理需填写说明，用户可在订单里查看。</p></fieldset></form>`;
  }
  async uploadQR(file) {
    if (!file || this.busy) return;
    if (file.size > 1024*1024) { this.message('收款码图片不能超过 1 MB。'); return; }
    this.captureSettings(); this.busy = true; const token = this.token;
    try {
      const result = await this.request('/api/admin/billing/qr', file, true, true);
      this.settingsDraft.qr_code_id = result.id; this.settingsDirty = true;
      if (this.active && token === this.token) { this.renderSettings(); this.toast('收款码已上传，请保存设置使其生效。'); }
    } catch (error) { if (this.active && token === this.token) this.message(error.message); }
    finally { this.busy = false; }
  }
  async submit(event) {
    const form = event.target.closest('form[data-form]');
    if (!form) return;
    event.preventDefault();
    if (this.busy) return;
    const values = Object.fromEntries(new FormData(form)), type = form.dataset.form, token = this.token;
    this.captureSettings(); this.busy = true; this.message('');
    for (const fieldset of form.querySelectorAll('fieldset')) fieldset.disabled = true;
    try {
      if (type === 'auth') {
        if (values.mode !== 'login' && values.password !== values.repeat) throw new Error('两次输入的密码不一致。');
        const result = await this.request(`/api/account/${values.mode}`, {username:values.username, password:values.password, recovery_code:values.recovery_code || '', claim_reports:Boolean(values.claim_reports)});
        if (result.recovery_code) this.recovery = {code:result.recovery_code, username:values.username.toLowerCase()};
        this.drafts.clear(); await this.onAuth('login');
        if (this.active && token === this.token) await this.go(this.recovery ? 'account' : this.authNext);
      } else if (type === 'checkout') {
        const keyName = `jc-checkout:${this.overview.account.id}:${values.plan_id}`;
        let key = this.checkoutKeys.get(keyName);
        try { key = sessionStorage.getItem(keyName) || key; } catch {}
        if (!/^[a-f0-9]{32}$/.test(key || '')) key = crypto.randomUUID().replaceAll('-', '');
        this.checkoutKeys.set(keyName, key);
        try { sessionStorage.setItem(keyName, key); } catch {}
        const result = await this.request('/api/billing/orders', {plan_id:values.plan_id, request_key:key, revision:this.overview.revision, policy_version:this.overview.policy_version, confirmed:Boolean(values.confirmed)});
        try { sessionStorage.removeItem(keyName); } catch {}
        this.checkoutKeys.delete(keyName);
        if (this.active && token === this.token) await this.go(`order/${result.id}`);
      } else if (type === 'claim') {
        const result = await this.request(`/api/billing/orders/${this.order.id}`, {action:'claim', revision:this.order.revision, receipt:values.receipt, note:values.note});
        this.drafts.delete(`${this.route}:claim`);
        if (this.active && token === this.token) { this.order = result; await this.loadOrder(token); this.toast('已提交核款，请勿重复付款。'); }
      } else if (type === 'admin-login') {
        this.admin = await this.request('/api/admin/login', {key:values.key});
        form.reset(); if (this.active && token === this.token) await this.open(`${this.route}${this.query ? `?${this.query}` : ''}`);
      } else if (type === 'filter') {
        const params = new URLSearchParams({status:values.status, q:values.q});
        if (this.params.get('owner')) params.set('owner', this.params.get('owner'));
        await this.go(`admin?${params}`);
      } else if (type === 'user-filter') {
        await this.go(`admin/users?${new URLSearchParams({q:values.q.trim(),status:values.status})}`);
      } else if (type === 'user-note' || type === 'user-action') {
        const action = type === 'user-note' ? 'note' : values.action;
        const u = this.userDetail.user;
        if (action !== 'note' && !window.confirm(`确认对账号「${u.username}」执行“${userActions[action]}”？\n\n${userActionHelp[action]}`)) return;
        const detail = await this.request(`/api/admin/users/${u.id}`, {action,revision:u.revision,note:values.note,confirmed:true}, true);
        this.drafts.delete(`${this.route}:${type}`);
        if (this.active && token === this.token) { this.userDetail = detail; this.renderAdminUser(); this.toast(`${userActions[action]}：已完成。`); }
      } else if (type === 'settings') {
        const s = structuredClone(this.settingsDraft);
        for (const p of s.plans) { p.price_cents = cents(String(p.priceText ?? p.price_cents / 100)); delete p.priceText; for (const k of Object.keys(kinds)) p.credits[k] = Number(p.credits[k]); }
        this.settingsDraft = await this.request('/api/admin/billing', s, true); this.settingsDirty = false;
        await this.refreshConfig();
        if (this.active && token === this.token) { this.renderSettings(); this.toast('收款与套餐设置已保存。'); }
      } else if (type === 'review') {
        const result = await this.request(`/api/admin/orders/${this.order.id}`, {action:values.action, revision:this.order.revision, amount_cents:cents(values.amount), receipt:values.receipt, note:values.note, confirmed:Boolean(values.confirmed)}, true);
        this.drafts.delete(`${this.route}:review`);
        if (this.active && token === this.token) { this.order = result; this.renderOrder(); this.toast(values.action === 'confirm' ? '到账已登记，次数已发放。' : '退款已登记，未用次数已收回。'); }
      }
    } catch (error) {
      if (this.active && token === this.token) {
        if (type !== 'auth' && await this.recoverSession(error, token)) return;
        if (error.status === 409 && ['review','claim'].includes(type)) { await this.loadOrder(token).catch(() => {}); }
        if (error.status === 409 && ['user-note','user-action'].includes(type)) { await this.loadUser(token).catch(() => {}); }
        this.message(error.message);
      }
    } finally { this.busy = false; for (const fieldset of form.querySelectorAll('fieldset')) fieldset.disabled = false; }
  }
  async click(event) {
    const button = event.target.closest('[data-billing]');
    if (!button || button.disabled || this.busy) return;
    const action = button.dataset.billing, token = this.token;
    this.busy = true;
    try {
      if (action === 'reload') { await this.open(`${this.route}${this.query ? `?${this.query}` : ''}`); return; }
      if (action === 'reload-order') { await this.loadOrder(token); return; }
      if (action === 'reload-user') { await this.loadUser(token); return; }
      if (action === 'copy-recovery') { try { await navigator.clipboard.writeText(this.recovery.code); this.toast('账号恢复码已复制，请妥善保存。'); } catch { this.toast('请手动选中恢复码并复制。'); } return; }
      if (action === 'download-recovery') { this.downloadBlob(`岗位罗盘账号恢复凭据\n网站：${location.origin}\n账号：${this.recovery.username}\n账号恢复码：${this.recovery.code}\n\n此码可以重设账号密码并获得购买权益，请勿公开。每次重设后会更换，旧码失效。它与报告恢复码不同。`, '岗位罗盘-账号恢复凭据.txt'); return; }
      if (action === 'saved-recovery') { this.recovery = null; this.root.querySelector('.billing-recovery')?.remove(); return; }
      if (action === 'logout') { await this.request('/api/account/logout', {}); this.recovery = null; this.drafts.clear(); this.overview = null; await this.onAuth('logout'); if (this.active && token === this.token) await this.go('account'); return; }
      if (action === 'admin-logout') {
        if (this.settingsDirty && !window.confirm('退出管理会清除未保存的设置草稿，继续吗？')) return;
        await this.request('/api/admin/logout', {}, true); this.admin = null; this.settingsDraft = null; this.settingsDirty = false; this.drafts.clear(); await this.go('admin'); return;
      }
      if (action === 'add-plan' || action === 'remove-plan') {
        this.captureSettings();
        if (action === 'add-plan' && this.settingsDraft.plans.length < 5) this.settingsDraft.plans.push({id:`plan-${crypto.randomUUID().slice(0,8)}`, name:'新的求职套餐', price_cents:0, credits:{diagnosis:0,refine:0,tailor:0,interview:0}, active:false});
        if (action === 'remove-plan') this.settingsDraft.plans.splice(Number(button.dataset.index),1);
        this.settingsDirty = true; this.renderSettings(); return;
      }
      if (action === 'discard-settings') {
        if (this.settingsDirty && !window.confirm('放弃尚未保存的设置，重新读取服务器上的配置？')) return;
        this.settingsDraft = await this.request('/api/admin/billing', undefined, true); this.settingsDirty = false; this.renderSettings(); return;
      }
      const prompts = { cancel:'确认这笔订单尚未付款，并取消订单？已付款请提交交易单号。', request_refund:'申请后，本订单剩余次数会暂停使用。请联系运营者协商退款金额，是否继续？', withdraw_refund:'撤回退款申请，恢复本订单剩余次数的使用？', hold_refund:'开始处理退款后，本订单剩余次数暂停，用户不能自行撤回。是否继续？' };
      if (prompts[action] && !window.confirm(prompts[action])) return;
      const fields = {action, revision:this.order.revision};
      if (this.isAdmin) {
        fields.confirmed = true;
        if (['reject','reject_refund'].includes(action)) {
          fields.note = this.root.querySelector('[name=note]')?.value.trim() || '';
          if ([...fields.note].length < 4) throw new Error('请先填写至少 4 个字符的核对或退款说明。');
        }
      }
      const result = await this.request(`/api/${this.isAdmin ? 'admin' : 'billing'}/orders/${this.order.id}`, fields, this.isAdmin);
      this.drafts.delete(`${this.route}:review`);
      if (this.active && token === this.token) { this.order = result; this.renderOrder(); }
    } catch (error) { if (this.active && token === this.token) { if (await this.recoverSession(error, token)) return; if (error.status === 409) await this.loadOrder(token).catch(() => {}); this.message(error.message); } }
    finally { this.busy = false; }
  }
}
