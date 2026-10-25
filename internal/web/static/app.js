/* OMQ WebUI — 前端逻辑
 *
 * 无框架、无构建、纯 DOM。整个界面约 1200 行。
 *
 * 三条约定，改代码前先看一眼：
 *
 *  1. 所有用户/模型产出（消息正文、记忆摘要、画像、黑话含义）一律走
 *     textContent 或 el() 的文本参数，绝不拼 innerHTML。
 *     这些内容是外部输入，群里任何人发的东西都会进库再回到这里。
 *
 *  2. 视图函数统一签名 render(host, actions)，host 是 <main>，
 *     actions 是 topbar 右侧容器。视图自己负责往两处塞内容。
 *
 *  3. 路由是 hash 路由（#/chats）。选中的会话放在 state.sessionId，
 *     刷新后靠 localStorage 恢复 —— 不然每次刷新都退回空对话区。
 */

(() => {
  'use strict';

  /* ===================== 基础设施 ===================== */

  const $ = (sel, root = document) => root.querySelector(sel);

  // el 造一个元素。文本走 textContent，永远不会被当成 HTML 解析。
  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v == null || v === false) continue;
        if (k === 'class') node.className = v;
        else if (k === 'text') node.textContent = v;
        else if (k === 'html') node.innerHTML = v; // 仅用于内联的静态图标
        else if (k.startsWith('on')) node.addEventListener(k.slice(2).toLowerCase(), v);
        else if (k === 'dataset') Object.assign(node.dataset, v);
        else node.setAttribute(k, v);
      }
    }
    for (const c of children.flat()) {
      if (c == null || c === false) continue;
      node.append(c.nodeType ? c : document.createTextNode(String(c)));
    }
    return node;
  }

  // icon 引用 sprite 里的符号。
  function icon(name, cls = 'ic') {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', cls);
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#i-' + name);
    svg.append(use);
    return svg;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
    return node;
  }

  /* ---------- 格式化 ---------- */

  function fmtNum(n) {
    if (n == null || Number.isNaN(n)) return '0';
    const v = Number(n);
    if (Math.abs(v) >= 1e8) return (v / 1e8).toFixed(1).replace(/\.0$/, '') + '亿';
    if (Math.abs(v) >= 1e4) return (v / 1e4).toFixed(1).replace(/\.0$/, '') + '万';
    return v.toLocaleString('zh-CN');
  }

  function fmtCost(n) {
    const v = Number(n || 0);
    if (!v) return '¥0';
    if (v < 0.01) return '¥' + v.toFixed(4);
    return '¥' + v.toFixed(2);
  }

  // fmtUptime 把秒转成「3 天 4 小时」这种可读形式。
  function fmtUptime(sec) {
    sec = Number(sec || 0);
    if (sec < 60) return sec + ' 秒';
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d > 0) return `${d} 天 ${h} 小时`;
    if (h > 0) return `${h} 小时 ${m} 分`;
    return `${m} 分`;
  }

  const pad2 = (n) => String(n).padStart(2, '0');

  function fmtClock(sec) {
    if (!sec) return '';
    const d = new Date(sec * 1000);
    return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  }

  function fmtClockSec(ms) {
    if (!ms) return '';
    const d = new Date(ms);
    return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
  }

  // fmtAgo 输出「刚刚 / 12 分钟前 / 3 天前」，用于列表里的相对时间。
  function fmtAgo(sec) {
    if (!sec) return '';
    const diff = Math.floor(Date.now() / 1000) - Number(sec);
    if (diff < 60) return '刚刚';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    if (diff < 86400 * 30) return Math.floor(diff / 86400) + ' 天前';
    const d = new Date(sec * 1000);
    return `${d.getMonth() + 1}/${d.getDate()}`;
  }

  function fmtDayLabel(dateStr) {
    const d = new Date(dateStr + 'T00:00:00');
    if (Number.isNaN(d.getTime())) return dateStr;
    const today = new Date();
    today.setHours(0, 0, 0, 0);
    const diff = Math.round((today - d) / 86400000);
    if (diff === 0) return '今天';
    if (diff === 1) return '昨天';
    return `${d.getMonth() + 1}/${d.getDate()}`;
  }

  // dayKey 把毫秒时间戳归到本地日期，用于插入日期分隔线。
  function dayKey(ms) {
    const d = new Date(ms);
    return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
  }

  function fmtDayFull(ms) {
    const d = new Date(ms);
    return `${d.getFullYear()} 年 ${d.getMonth() + 1} 月 ${d.getDate()} 日`;
  }

  /* ---------- 请求 ---------- */

  async function api(path, options = {}) {
    const res = await fetch(path, {
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      ...options,
    });
    if (res.status === 401) {
      showLogin();
      throw new Error('登录已失效');
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw new Error(body.error || `HTTP ${res.status}`);
    }
    // 204 之类没有 body 的情况
    const text = await res.text();
    return text ? JSON.parse(text) : {};
  }

  // del 发一个删除请求。
  //
  // 必须用 DELETE 方法：后端只认 DELETE，防的是「浏览器预取 / 爬虫扫链接 /
  // <img src> 触发删除」这类事故（AGENTS.md 第 64 条）。
  function del(path) {
    return api(path, { method: 'DELETE' });
  }

  // confirmDelete 先确认再删。返回 true 表示用户确认了。
  //
  // 文案里带上「要删的是什么」：只写「确定删除吗」时，
  // 用户在列表里点错一行也看不出来。
  function confirmDelete(what, detail) {
    const first = detail ? `删除${what}？\n${detail}` : `删除${what}？`;
    return window.confirm(first + '\n\n此操作不可撤销。');
  }

  // delBtn 造一个删除按钮。onDelete 返回 Promise，成功后调 reload。
  //
  // 失败时把后端给的错误原样显示：删除失败的原因（不存在 / 库故障）
  // 对排查很关键，统一显示「删除失败」等于把信息丢掉。
  function delBtn(what, detail, onDelete, reload) {
    return el('button', {
      class: 'btn btn-ghost btn-sm btn-danger', type: 'button', text: '删除',
      onclick: async (e) => {
        if (!confirmDelete(what, detail)) return;
        const btn = e.currentTarget;
        btn.disabled = true;
        try {
          const res = await onDelete();
          toast(res && res.deleted > 1 ? `已删除 ${res.deleted} 条` : '已删除');
          reload();
        } catch (err) {
          toast('删除失败：' + err.message, 'error');
          btn.disabled = false;
        }
      },
    });
  }

  /* ---------- Toast ---------- */

  let toastBox = null;

  function toast(msg, kind = 'ok') {
    if (!toastBox) {
      toastBox = el('div', { class: 'toasts' });
      document.body.append(toastBox);
    }
    const node = el('div', { class: 'toast ' + kind },
      icon(kind === 'error' ? 'close' : 'chevron'),
      el('span', { text: msg }),
    );
    toastBox.append(node);
    setTimeout(() => {
      node.style.opacity = '0';
      node.style.transition = 'opacity 160ms ease';
      setTimeout(() => node.remove(), 200);
    }, kind === 'error' ? 4200 : 2400);
  }

  /* ---------- 状态占位 ---------- */

  function skeleton(rows = 3, height = 44) {
    const box = el('div', { class: 'memory-list' });
    for (let i = 0; i < rows; i++) {
      box.append(el('div', { class: 'skeleton', style: `height:${height}px` }));
    }
    return box;
  }

  function emptyState(text) {
    return el('div', { class: 'state' }, icon('empty'), el('p', { text }));
  }

  function errorState(err, retry) {
    return el('div', { class: 'state error' },
      icon('close'),
      el('p', { text: err.message || String(err) }),
      retry ? el('button', { class: 'btn btn-outline btn-sm', onclick: retry, text: '重试' }) : null,
    );
  }

  function notice(text) {
    // 图标只用进来「可能用得上」的那个：原本这里是 reason，
    // 删掉独立推理页后那个图标也一起没了。
    return el('div', { class: 'notice' }, icon('config'), el('span', { text }));
  }

  // confirmAction 用原生 confirm：这个界面里唯一的破坏性操作是删记忆，
  // 为它做一个定制弹窗不划算。
  function confirmAction(msg) {
    return window.confirm(msg);
  }

  /* ===================== 状态 ===================== */

  const state = {
    sessionId: null,
    sessions: [],
    sessionQuery: '',
    logSource: null,
    // 会话页的游标
    oldestMS: 0,
    newestMS: 0,
    hasMore: false,
    // 当前打开的会话对象。抽屉要拿到完整字段（target_id / scope / 消息数），
    // 只存 sessionId 的话 drawer 里又得去数组里反查一遍。
    current: null,
    pendingPoll: null,
    // 消息事件流（/api/events 的 EventSource）。
    // 事件驱动刷新的主通道；轮询只是它断线时的兜底。
    eventSource: null,
    // 概览页「模型调用」的时间范围。放 state 而不是 usageSection
    // 的局部变量：概览页会整段静默重绘，局部变量会被重置回 1 天。
    usageDays: 1,
  };

  // liveHooks 是当前页面注册的实时刷新钩子。
  //
  // 为什么需要它：loadSessions 这类函数是页面渲染函数的闭包内变量，
  // 事件分发器够不着。页面渲染时注册、navigate 切页时清空。
  const liveHooks = {};

  // refreshTimers 把短时间内的多次事件合并成一次刷新。
  // 群聊洪峰时一秒可能落库十几条，逐条拉增量只会白打接口。
  const refreshTimers = {};

  // scheduleRefresh 节流合并：同一个 key 在 delay 毫秒内只触发一次。
  function scheduleRefresh(key, fn, delay = 400) {
    if (refreshTimers[key]) return;
    refreshTimers[key] = setTimeout(() => {
      delete refreshTimers[key];
      fn();
    }, delay);
  }

  const STORE_SESSION = 'omq.session';
  const STORE_THEME = 'omq.theme';

  /* ===================== 主题 ===================== */

  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    const btn = $('#theme-toggle');
    if (btn) {
      clear(btn).append(icon(theme === 'dark' ? 'sun' : 'moon'));
      btn.title = theme === 'dark' ? '切换到浅色' : '切换到深色';
    }
    try { localStorage.setItem(STORE_THEME, theme); } catch { /* 隐私模式忽略 */ }
  }

  function initTheme() {
    let theme = null;
    try { theme = localStorage.getItem(STORE_THEME); } catch { /* ignore */ }
    if (!theme) {
      theme = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    }
    applyTheme(theme);
  }

  /* ===================== 登录 ===================== */

  function showLogin() {
    $('#login').hidden = false;
    $('#app').hidden = true;
    stopLogs();
    stopEvents();
  }

  function showApp() {
    $('#login').hidden = true;
    $('#app').hidden = false;
  }

  $('#login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const errBox = $('#login-err');
    errBox.textContent = '';
    const btn = $('button[type=submit]', e.currentTarget);
    btn.disabled = true;
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ password: $('#password').value }),
      });
      $('#password').value = '';
      showApp();
      startApp();
    } catch (err) {
      errBox.textContent = err.message;
    } finally {
      btn.disabled = false;
    }
  });

  $('#logout').addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => {});
    showLogin();
  });

  $('#theme-toggle').addEventListener('click', () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  });

  /* ===================== 侧栏（移动端） ===================== */

  function setSidebar(open) {
    $('#sidebar').classList.toggle('open', open);
    $('#scrim').classList.toggle('show', open);
  }

  $('#menu-toggle').addEventListener('click', () => setSidebar(true));
  $('#scrim').addEventListener('click', () => setSidebar(false));

  /* ===================== 路由 ===================== */

  // 结构对齐 MaiBot 的 dashboard，但砍掉了它的「工作区」层：
  //
  //   侧栏分组菜单 = 概览 / 配置管理 / 资源管理 / 运行记录
  //   页内页签     = 记忆、学习各自有
  //
  // 原本日志与用量统计归在「日志工作区」（顶栏切过去侧栏整体消失、
  // 换成一组页签）。那样做在手机上尤其别扭：工作区切换器窄屏放不下，
  // 只能靠底部标签栏 + 「管理」页中转，多了两层跳转。
  // 现在日志就是侧栏里的普通两条，任何视口下都一步可达。
  //
  // 侧栏与页签的数据都在 ROUTES 里声明，不画在 HTML 里 ——
  // 加一个页面只要加一条，导航与页签两处同时生效。

  // group 是侧栏分组标题。
  const ROUTES = {
    overview: {
      title: '概览', sub: '运行时状态、数据总量与近期趋势',
      group: '概览', render: viewOverview,
    },
    chats: {
      title: '会话', sub: '按会话查看消息与思考过程',
      group: '概览', render: viewChats, fill: true,
    },
    // 网页对话窗口：以人类身份与机器人私聊。
    // 对应 MaiBot dashboard 的 /chat 聊天页 —— 会话页是观察，对话页是聊天。
    talk: {
      title: '对话', sub: '以人类身份与机器人对话',
      group: '概览', render: viewTalk, fill: true,
    },
    // 配置管理拆成两个入口，与 MaiBot 一致（它侧栏里也是
    // 「麦麦设置 /config/bot」与「模型管理 /config/model」两条）。
    //
    // 这里**没有页签**：配置页内部用左侧竖排二级导航
    // （#/config?section=personality），因为分区多到 14 个时
    // 横向页签在 1180px 里放不下、手机上要横向滚动才找得到。
    // 页签是「平级的少数几个视图」，二级导航才是「一个页面里的目录」。
    config: {
      title: '详细设置', sub: '人格、聊天、回复、记忆、学习、运行参数',
      group: '配置管理', render: viewConfig,
    },
    models: {
      title: '模型', sub: '服务商、模型列表与功能任务分配',
      group: '配置管理', render: viewModels,
      // 与 MaiBot 的 /config/model 两个页签同名同顺序。
      tabs: [{ id: 'providers', label: '模型设置' }, { id: 'tasks', label: '功能分配' }],
    },
    // Prompt 管理（参考实现的 /config/prompts，在它的「高级工具」组）。
    prompts: {
      title: '提示词', sub: '编辑 Planner / Replyer 等提示词模板',
      group: '配置管理', render: viewPrompts,
    },
    // 数据管理（参考实现的 /data-transfer，在它的「高级工具」组）。
    data: {
      title: '数据', sub: '导出与导入数据库',
      group: '配置管理', render: viewData,
    },
    memory: {
      title: '记忆', sub: '自动生成的聊天回想与人物画像',
      group: '资源管理', render: viewMemory,
      tabs: [{ id: 'recall', label: '聊天回想' }, { id: 'profiles', label: '人物画像' }],
    },
    learning: {
      title: '学习', sub: '表达、黑话与表情包',
      group: '资源管理', render: viewLearning,
      tabs: [
        { id: 'expression', label: '表达' },
        { id: 'jargon', label: '黑话' },
        { id: 'emoji', label: '表情包' },
      ],
    },
    // 运行记录只剩日志一条。
    //
    // 用量统计原来也在这里，现在归并进概览页（参考实现也是把
    // 请求数 / Token / 花费三张趋势卡放在首页）：它回答的是同一个问题
    // 「这台机器现在跑得怎么样」，分成两个入口只会让人来回翻。
    logs: {
      title: '终端日志', sub: '实时日志流',
      group: '运行记录', render: viewLogs, fill: true,
    },
  };

  const DEFAULT_TAB = {
    memory: 'recall', learning: 'expression',
    models: 'providers',
  };

  // parseHash 拆出路由名与查询参数（#/memory?tab=profiles）。
  function parseHash() {
    const raw = (location.hash || '').replace(/^#\/?/, '');
    const qIndex = raw.indexOf('?');
    const name = qIndex < 0 ? raw : raw.slice(0, qIndex);
    const params = new URLSearchParams(qIndex < 0 ? '' : raw.slice(qIndex + 1));
    return { name: ROUTES[name] ? name : 'overview', params };
  }

  function currentRoute() {
    return parseHash().name;
  }

  // routeTab 取当前页签：URL 里没写或写了不存在的值就退回默认页签。
  function routeTab(name, params) {
    const route = ROUTES[name];
    if (!route || !route.tabs) return '';
    const wanted = params.get('tab');
    if (wanted && route.tabs.some((t) => t.id === wanted)) return wanted;
    return DEFAULT_TAB[name] || route.tabs[0].id;
  }

  async function navigate() {
    const { name, params } = parseHash();
    const route = ROUTES[name];

    stopLogs();
    // 切页清空实时刷新钩子：旧页面的闭包函数不能再被事件触发
    // （比如还指着已经摘掉的 DOM）。
    for (const k of Object.keys(liveHooks)) delete liveHooks[k];
    for (const k of Object.keys(refreshTimers)) {
      clearTimeout(refreshTimers[k]);
      delete refreshTimers[k];
    }

    document.querySelectorAll('.nav-item').forEach((a) => {
      a.classList.toggle('active', a.dataset.nav === name);
    });

    $('#page-title').textContent = route.title;
    $('#page-sub').textContent = route.sub;

    // 页内页签（记忆 / 学习）
    const tabBar = clear($('#page-tabs'));
    if (route.tabs) {
      const active = routeTab(name, params);
      tabBar.hidden = false;
      for (const t of route.tabs) {
        tabBar.append(el('button', {
          class: 'page-tab' + (t.id === active ? ' active' : ''),
          type: 'button',
          role: 'tab',
          'aria-selected': t.id === active ? 'true' : 'false',
          dataset: { tab: t.id },
          text: t.label,
          onclick: () => { location.hash = `#/${name}?tab=${t.id}`; },
        }));
      }
    } else {
      tabBar.hidden = true;
    }

    const content = $('#content');
    content.className = 'content' + (route.fill ? ' fill' : '');
    clear(content);

    const actions = clear($('#page-actions'));

    try {
      await route.render(content, actions, params);
    } catch (err) {
      clear(content).append(errorState(err, () => navigate()));
    }

    setSidebar(false);
  }

  window.addEventListener('hashchange', navigate);

  /* ===================== 顶部状态 ===================== */

  async function refreshStatus() {
    try {
      const s = await api('/api/status');
      const dot = $('#conn-dot');
      dot.className = 'conn-dot ok';
      $('#conn-text').textContent =
        s.active_chats > 0 ? `${s.active_chats} 个活跃会话` : '在线';
      return s;
    } catch (err) {
      const dot = $('#conn-dot');
      dot.className = 'conn-dot bad';
      $('#conn-text').textContent = err.message === '登录已失效' ? '未登录' : '连接失败';
      return null;
    }
  }

  /* ===================== 概览 ===================== */

  async function viewOverview(host, actions) {
    actions.append(el('button', {
      class: 'btn btn-outline btn-sm',
      onclick: () => navigate(),
    }, icon('refresh'), '刷新'));

    host.append(el('div', { class: 'page' }, skeleton(4, 76)));

    const data = await api('/api/overview');
    const page = el('div', { class: 'page' });
    clear(host).append(page);
    buildOverviewPage(page, data);
  }

  // refreshOverviewSilently 静默刷新概览页。
  //
  // 概览页开着时会由事件流与定时器触发（见启动区的 setInterval），
  // 重拉数据后原位替换 .page —— 不闪骨架屏、不打断正在看的数字。
  // 失败静默跳过：不能把用户正看着的页面突然换成错误状态，
  // 下一次触发自然重试。
  async function refreshOverviewSilently() {
    if (currentRoute() !== 'overview') return;
    const host = $('#content');
    const old = $('.page', host || document);
    if (!old) return; // 首屏还没画完，交给首屏
    try {
      const data = await api('/api/overview');
      const page = el('div', { class: 'page' });
      buildOverviewPage(page, data);
      old.replaceWith(page);
    } catch { /* 静默：保持当前页面 */ }
  }

  // buildOverviewPage 把一份 overview 数据填进容器。
  // 首屏与静默刷新共用这一份 —— 两处渲染才不会长出「简化版」差异。
  function buildOverviewPage(page, data) {

    const c = data.counts || {};
    const rt = data.runtime || {};
    const feat = data.features || {};
    const bot = data.bot || {};

    // 部分区块失败时给出提示，但页面照常显示 —— 后端就是这么设计的。
    if (data.errors && data.errors.length) {
      page.append(notice(
        '部分数据读取失败：' +
        data.errors.map((e) => `${e.section}（${e.message}）`).join('；')
      ));
    }

    /* --- 指标 --- */
    const metrics = el('div', { class: 'metrics' });
    const metric = (k, v, sub, unit) => el('div', { class: 'metric' },
      el('div', { class: 'metric-k', text: k }),
      el('div', { class: 'metric-v' },
        String(v),
        unit ? el('small', { text: unit }) : null,
      ),
      sub ? el('div', { class: 'metric-sub', text: sub }) : null,
    );

    metrics.append(
      metric('消息总量', fmtNum(c.messages), `今日 ${fmtNum(c.messages_today)} 条`),
      metric('会话', fmtNum(c.sessions), `${fmtNum(rt.active_chats)} 个活跃`),
      metric('认识的人', fmtNum(c.persons), '去重后的发言用户'),
      metric('运行时长', fmtUptime(rt.uptime_seconds), bot.nickname || ''),
    );
    page.append(el('div', { class: 'section' }, metrics));

    /* --- 趋势 --- */
    const daily = data.daily || [];
    if (daily.length) {
      const max = Math.max(1, ...daily.map((d) => d.total));
      const rows = el('div', { class: 'trend-rows' });

      for (const d of daily) {
        // 整条宽度 = 当天总量占最高那天的比例；
        // 里面白色部分是「收到」，灰色部分接在后面是「发出」。
        const inRatio = d.total ? d.inbound / d.total : 1;
        const fullPct = (d.total / max) * 100;

        const fill = el('div', {
          class: 'trend-fill',
          style: `width:${(fullPct * inRatio).toFixed(2)}%`,
        });
        const track = el('div', { class: 'trend-track' }, fill);

        if (d.outbound > 0) {
          track.append(el('div', {
            class: 'trend-fill out',
            style: `width:${(fullPct * (1 - inRatio)).toFixed(2)}%`,
          }));
        }

        rows.append(el('div', {
          class: 'trend-row',
          title: `${d.date}：收到 ${d.inbound} 条，发出 ${d.outbound} 条，共 ${d.total} 条`,
        },
          el('span', { class: 'trend-day', text: fmtDayLabel(d.date) }),
          track,
          el('span', { class: 'trend-value' },
            fmtNum(d.total),
            d.outbound ? el('span', { class: 'dim', text: ` / ${fmtNum(d.outbound)}` }) : null,
          ),
        ));
      }

      page.append(el('div', { class: 'section card card-pad' },
        el('div', { class: 'section-head' },
          el('h3', { text: '近 7 天消息' }),
          el('div', { class: 'trend-legend' },
            el('span', {}, el('i'), '收到'),
            el('span', {}, el('i', { class: 'out' }), '发出'),
          ),
        ),
        rows,
      ));
    }

    /* --- 功能与数据量 --- */
    const feats = [
      ['长期记忆', feat.memory, `${fmtNum(c.memory)} 条回想 · ${fmtNum(c.profiles)} 份画像`, 'memory'],
      ['表情包', feat.emoji, `${fmtNum(c.emojis)} 个已收藏`, 'learning'],
      ['表达学习', feat.expression, `${fmtNum(c.expressions)} 条`, 'learning'],
      ['黑话学习', feat.jargon, `${fmtNum(c.jargons)} 条`, 'learning'],
    ];

    const grid = el('div', { class: 'metrics' });
    for (const [name, on, detail, route] of feats) {
      grid.append(el('div', { class: 'metric' },
        el('div', { class: 'row' },
          el('span', { class: 'metric-k', text: name }),
          el('span', { class: 'spacer' }),
          el('span', { class: 'tag ' + (on ? 'tag-ok' : 'tag-off'), text: on ? '启用' : '关闭' }),
        ),
        el('div', { class: 'metric-sub', style: 'margin-top:8px', text: detail }),
        on ? el('a', {
          class: 'btn btn-ghost btn-sm',
          href: '#/' + route,
          style: 'margin-top:6px;margin-left:-9px',
          text: '查看 →',
        }) : null,
      ));
    }
    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '功能与数据' })),
      grid,
    ));

    // 模型调用（原来的独立「用量统计」页面）。
    //
    // 放在功能卡之后、机器人身份之前：它是「跑得怎么样」的数据，
    // 与上面两组指标同类；身份信息属于「配置结果」，放最后。
    page.append(usageSection());

    /* --- 机器人身份 --- */
    page.append(el('div', { class: 'section card' },
      el('div', { class: 'cfg-row' },
        el('span', { class: 'k', text: '昵称' }),
        el('span', { class: 'v', text: bot.nickname || '—' })),
      el('div', { class: 'cfg-row' },
        el('span', { class: 'k', text: '平台' }),
        el('span', { class: 'v mono', text: bot.platform || '—' })),
      el('div', { class: 'cfg-row' },
        el('span', { class: 'k', text: '账号' }),
        el('span', { class: 'v mono', text: bot.account_id || '—' })),
    ));
  }

  /* ---------- 消息渲染 ---------- */

  // 消息渲染的三条约定（改之前先看）：
  //
  //  1. 一条消息 = 头像 + 气泡。以前是没有气泡的纯文本行，
  //     消息一多就糊成一片，分不清谁在说话。
  //  2. 同一个人连续发言时，后几条隐藏头像与名字，只留气泡 ——
  //     这是聊天软件的通行做法，也是 MaiBot 的做法。
  //     隐藏的依据是「上一条同作者、同一分钟内、且都是普通消息」。
  //  3. 只 @ 不说话的消息（at 段 + 没有文本）**不是事件**，
  //     它是这个人说的一句话。以前把它降级成居中小字，
  //     于是「谁 @ 了谁」这个信息就丢了 —— 表现为「单独 @ 没有归属」。

  // msgWho 返回展示用的名字。
  function msgWho(m) {
    return m.user_card || m.user_name || m.user_id || '未知';
  }

  // msgInitial 取头像里的首字。
  //
  // 用 Array.from 而不是 charAt：不这样处理会把 emoji 与部分汉字
  // 的代理对劈成半个字符，界面上是一个乱码方块。
  function msgInitial(name) {
    const chars = Array.from(String(name || '').trim());
    return chars.length ? chars[0].toUpperCase() : '?';
  }

  // avatarTone 按名字算一个稳定的色调下标，让同一个人每次都是同一个颜色。
  function avatarTone(name) {
    let sum = 0;
    for (const ch of String(name || '')) sum = (sum + ch.codePointAt(0)) % 100000;
    return sum % 6;
  }

  // renderAvatar 造头像。isBot 时用空心描边区分。
  function renderAvatar(name, isBot) {
    const cls = 'avatar' + (isBot ? ' avatar-bot' : '') + ' tone-' + avatarTone(name);
    return el('div', { class: cls, 'aria-hidden': 'true', text: msgInitial(name) });
  }

  // renderMessage 把一条消息转成 DOM。
  //
  // 放在模块级而不是会话视图内部：轮询新消息时也要用它。
  // 曾经在视图内定义一份、轮询又写一份极简版，结果同一个消息
  // 两处显示不一致（轮询版没有引用、没有 @、没有媒体占位）。
  //
  // 第二个参数 prev 是上一条消息，用来判断要不要合并（隐藏头像与名字）。
  function renderMessage(m, prev) {
    const isSelf = !!m.is_self;
    const segs = m.segments || [];

    const atSegs = segs.filter((s) => s.type === 'at');
    const mediaSegs = segs.filter((s) => s.has_media);
    const who = msgWho(m);

    // 只有 at 段、没有文字：这仍然是一条正常发言（「@麦麦」就是这么发的），
    // 只是内容比一句话短。绝不能当成系统事件居中显示，否则这句话
    // 的发送者在界面上就消失了。
    const text = (m.text || '').trim();

    // 合并判断：上一条同作者、同属机器人/用户、时间在同一分钟内。
    // 引用消息不参与合并 —— 它自带一块引用，视觉上需要断开。
    const sameAsPrev = !!(
      prev &&
      !m.reply_to &&
      !prev.reply_to &&
      prev.is_self === m.is_self &&
      (prev.user_id || prev.user_name) === (m.user_id || m.user_name) &&
      Math.abs((m.created_ms || 0) - (prev.created_ms || 0)) < 60000
    );

    const row = el('div', {
      class: 'msg' + (isSelf ? ' self' : '') + (sameAsPrev ? ' merged' : ''),
    });

    // 头像：合并时留一个等宽占位，保证气泡左边界仍然对齐。
    row.append(sameAsPrev
      ? el('div', { class: 'avatar-gap', 'aria-hidden': 'true' })
      : renderAvatar(who, isSelf));

    const main = el('div', { class: 'msg-main' });

    // 名字 + 时间：合并时不重复显示名字。
    if (!sameAsPrev) {
      main.append(el('div', { class: 'msg-head' },
        el('span', { class: 'msg-who', text: who }),
        isSelf ? el('span', { class: 'tag tag-off', text: '机器人' }) : null,
        el('span', { class: 'msg-time', text: fmtClockSec(m.created_ms) }),
      ));
    }

    // 引用块放在气泡外面、上方：它引的是别的消息，不属于「我」说的话。
    //
    // 优先级：批量查好的预览（谁: 内容）> 事件自带的 quoted_text 快照 > 截断 ID。
    //
    // 第二条是 QQ 官方通道的特点：引用正文**随事件下发**
    // （msg_elements），所以就算被引的那条已经被保留期清掉、
    // 或压根没在这个群里收到过，引用内容仍然能显示。
    if (m.reply_to) {
      let preview = (m.reply_preview || '').trim();
      if (!preview) {
        const snap = (m.quoted_text || '').trim();
        if (snap) {
          const who = (m.quoted_sender || '').trim();
          preview = who ? who + ': ' + snap : snap;
        }
      }
      main.append(el('div', {
        class: 'quote' + (m.is_quote_target ? ' quote-ref' : '') + (preview ? '' : ' quote-raw'),
        title: preview || m.reply_to,
      },
        el('span', { class: 'quote-mark', 'aria-hidden': 'true', text: '↩' }),
        el('span', { class: 'quote-text', text: preview || shortID(m.reply_to) }),
      ));
    }

    const bubble = el('div', { class: 'bubble' });

    for (const s of atSegs) {
      bubble.append(el('span', { class: 'msg-at', text: '@' + (s.user_name || '某人') }));
    }
    if (text) bubble.append(el('span', { class: 'bubble-text', text }));

    // 媒体：有 src 就直接显示图（表情包采集过的那些），
    // 没有就退回占位字（普通图片、或库里没这张）。
    //
    // 为什么不一律显示占位：表情包是这个群聊里最常出现的内容，
    // 全部变成「[表情]」等于什么都没看到。参考实现也是直接显示图。
    for (const s of mediaSegs) {
      bubble.append(renderMedia(s));
    }

    main.append(bubble);
    row.append(main);
    return row;
  }

  // renderMedia 渲染一个媒体段。
  //
  // src 能加载就显示图片；加载失败（文件被删、库记录还在）时
  // 由 onerror 把 img 换成占位字 —— 浏览器里一个碎图标比
  // 一句「[表情]」更难理解。
  function renderMedia(s) {
    const label = mediaLabel(s.type);
    if (!s.src) {
      return el('span', { class: 'msg-media', text: '[' + label + ']' });
    }

    const isEmoji = s.type === 'emoji';
    const img = el('img', {
      class: 'msg-img' + (isEmoji ? ' msg-img-emoji' : ''),
      src: s.src,
      alt: label,
      loading: 'lazy',
      // 点击在新标签打开原图：表情包很小，看不清细节时有用。
      onclick: () => window.open(s.src, '_blank', 'noopener'),
    });
    img.addEventListener('error', () => {
      const fallback = el('span', { class: 'msg-media', text: '[' + label + ']' });
      img.replaceWith(fallback);
    });
    return img;
  }

  function mediaLabel(type) {
    switch (type) {
      case 'image': return '图片';
      case 'emoji': return '表情';
      case 'voice': return '语音';
      case 'file': return '文件';
      default: return '附件';
    }
  }

  // shortID 截断长 ID：提示里不需要看完整的 message_id
  function shortID(id) {
    const s = String(id || '');
    return s.length > 12 ? s.slice(0, 6) + '…' + s.slice(-4) : s;
  }

  // buildTimeline 把一批时间线条目转成带日期分隔的片段。
  //
  // 时间线条目有四类（对齐参考实现主事件账本的事件分类）：
  // message 消息、reasoning 一轮思考、gate 门控判断、trim 上下文裁切。
  // 后端已经按时间排好序，这里只负责渲染 —— 顺序判断留在后端一处，
  // 免得「什么时候插入思考」这种规则在前后端各写一遍、各偏一次。
  //
  // 上翻历史时要插到现有列表前面，所以不能只往容器 append ——
  // 需要一个能插入任意位置的 DocumentFragment。
  //
  // prev 参数是「这批之前的那条消息」，用于正确判断首条能否合并
  // （不传的话，第一屏顶部总会多出一个头像）。
  function buildTimeline(entries, prev) {
    const frag = document.createDocumentFragment();
    let lastDay = '';
    let lastPrev = prev && !prev.kind ? prev : null;

    for (const entry of entries) {
      // 非消息类事件：思考轮次、门控、上下文裁切。
      if (entry.kind === 'reasoning') {
        frag.append(renderTraceTurn(entry.reasoning));
        continue;
      }
      if (entry.kind === 'gate') {
        frag.append(renderGate(entry.gate || {}));
        continue;
      }
      if (entry.kind === 'trim') {
        frag.append(renderTrim(entry.trim || {}));
        continue;
      }

      const m = entry.message || entry;
      const ms = m.created_ms || (m.created_at || 0) * 1000;
      const k = dayKey(ms);
      if (k !== lastDay) {
        lastDay = k;
        frag.append(el('div', { class: 'day-sep' }, el('span', { text: fmtDayFull(ms) })));
        // 跨天必须断开合并，否则新一天的第一条会没有头像。
        lastPrev = null;
      }
      frag.append(renderMessage(m, lastPrev));
      lastPrev = m;
    }
    return frag;
  }

  // renderTraceTurn 在消息流里渲染一轮思考。
  //
  // 形态严格对齐参考实现的 TraceRound（dashboard/src/components/ui/
  // conversation.tsx:224）与 PlannerFinalizedCard（maisaka-monitor.tsx:1188）：
  //
  //   按钮行：[chevron] [徽标"第 N 轮"] [时间段] [摘要] [右对齐指标]
  //   展开后：Planner 卡片 → 每个工具的步骤
  //
  // 三个刻意的细节：
  // 1. **默认展开**。参考实现的初始状态是 `{ all: true }`
  //    （maisaka-monitor.tsx:2462）。我之前猜成默认折叠，是错的。
  // 2. 折叠时的摘要不是「第 N 轮思考」，而是一串「工具名 + 状态圆点」，
  //    见 CollapsedPlannerToolSummary（maisaka-monitor.tsx:2222）。
  // 3. 指标放右侧、tabular-nums，两行轮次的时间列能对齐。
  function renderTraceTurn(turn) {
    const steps = (turn && turn.steps) || [];
    if (!steps.length) return el('div', { class: 'trace-turn-empty' });

    // 汇总耗时与 token（指标行右侧）。参考实现是 `${duration_ms} · ${tok} tok`。
    let totalMS = 0, totalTok = 0;
    for (const st of steps) {
      totalMS += Number(st.duration_ms) || 0;
      totalTok += Number(st.total_tokens) || 0;
    }

    // 轮次编号取第一个 planner 步骤的 round（工具/回复器步骤没有 round）。
    const planner = steps.find((st) => st.type === 'planner');
    const roundNo = planner ? planner.round : 1;

    const base = el('div', { class: 'trace-round' });
    const bodyID = 'trace-' + String(turn.turn_id || 'x').replace(/[^a-zA-Z0-9]/g, '');

    const open = state.traceOpen !== false;  // 默认展开，用户手动折叠过后记住

    const trigger = el('button', {
      type: 'button',
      class: 'trace-round-trigger',
      'aria-expanded': open ? 'true' : 'false',
      'aria-controls': bodyID,
      onclick: () => {
        const now = trigger.getAttribute('aria-expanded') !== 'true';
        trigger.setAttribute('aria-expanded', now ? 'true' : 'false');
        body.hidden = !now;
      },
    },
      icon('chevron'),
      el('span', { class: 'trace-badge-round', text: '第 ' + roundNo + ' 轮' }),
      el('span', { class: 'trace-round-time' }, timeRange(steps)),
      el('span', { class: 'trace-round-summary' }, collapsedToolSummary(steps, open)),
      el('span', { class: 'trace-round-metrics', text: metricsText(totalMS, totalTok) }),
    );
    base.append(trigger);

    const body = el('div', { class: 'trace-round-body', id: bodyID });
    body.hidden = !open;
    // 竖线串起每一步：Planner 想了什么 → 工具实际做了什么 → 回复器怎么说的。
    const list = el('div', { class: 'trace-steps' });
    for (const st of steps) list.append(renderStep(st));
    body.append(list);
    base.append(body);
    return base;
  }

  // metricsText 拼指标行：`4.93s · 6191 tok`。
  function metricsText(ms, tok) {
    const parts = [];
    if (ms) parts.push(fmtSec(ms));
    if (tok) parts.push(fmtNum(tok) + ' tok');
    return parts.join(' · ');
  }

  // timeRange 给出「开始 → 结束」，参考实现的 EventTimestamp
  // （maisaka-monitor.tsx:113）就是这个形态。
  function timeRange(steps) {
    if (!steps.length) return '';
    const start = steps[0].at || 0;
    const last = steps[steps.length - 1];
    if (!start) return '';
    const end = Math.max((last.at || 0) + (Number(last.duration_ms) || 0), start);
    return fmtClockSec(start) + ' → ' + fmtClockSec(end);
  }

  // collapsedToolSummary 复刻 CollapsedPlannerToolSummary
  // （maisaka-monitor.tsx:2222）：折叠时显示「圆点 + 工具名」列表，
  // 执行成功的绿点、失败的红点、没有结果的灰点。
  //
  // 展开时摘要让位给内容，只显示结束原因 —— 否则顶部一行工具名
  // 会和下面每一条工具步骤重复。
  function collapsedToolSummary(steps, open) {
    if (open) {
      const planner = steps.find((st) => st.type === 'planner');
      return planner && planner.finish_reason ? String(planner.finish_reason) : '';
    }
    const tools = [];
    for (const st of steps) {
      if (st.type === 'planner') for (const c of (st.tool_calls || [])) tools.push(c);
      if (st.type === 'tool') tools.push({ name: st.name, done: true, failed: st.failed });
    }
    if (!tools.length) return '';
    // 去重保序：同一个工具连续调多次只显示一次。
    const seen = new Set();
    const uniq = [];
    for (const t of tools) {
      if (!t.name || seen.has(t.name)) continue;
      seen.add(t.name);
      uniq.push(t);
    }
    return uniq.map((t) => {
      const dot = el('span', { class: 'trace-dot' + (t.failed ? ' fail' : t.done ? '' : ' idle') });
      return el('span', { class: 'trace-tool-inline' }, dot, el('span', { class: 'trace-tool-inline-name', text: t.name }));
    });
  }

  // renderStep 按类型分发到三个渲染器。
  //
  // 这三种步骤对应参考实现的三张卡片：
  //   planner → PlannerFinalizedCard（maisaka-monitor.tsx:1188）
  //   tool    → PlannerToolResultCard（maisaka-monitor.tsx:1580）
  //   replyer → ReplierResponseCard（maisaka-monitor.tsx:2056）
  function renderStep(st) {
    switch (st.type) {
      case 'tool': return renderToolStep(st);
      case 'replyer': return renderReplyerStep(st);
      default: return renderPlannerStep(st);
    }
  }

  // renderPlannerStep 复刻 PlannerFinalizedCard：标题行（Planner + 模型徽标 +
  // 上下文档数/可用工具数），然后是思考与正文。
  function renderPlannerStep(st) {
    const step = el('div', { class: 'trace-step', 'data-tone': st.failed ? 'error' : 'success' });

    const head = el('div', { class: 'trace-step-head' },
      el('span', { class: 'trace-step-title', text: 'Planner' }),
    );
    if (st.model) {
      head.append(el('span', { class: 'trace-model-badge', text: '模型: ' + st.model }));
    }
    if (st.history_count || st.tool_count) {
      head.append(el('span', {
        class: 'trace-step-meta',
        text: '上下文 ' + (st.history_count || 0) + ' 条 / 可用工具 ' + (st.tool_count || 0),
      }));
    }
    if (st.failed) head.append(el('span', { class: 'trace-status fail', text: '失败' }));
    head.append(el('span', { class: 'trace-step-time', text: fmtClockSec(st.at) }));
    step.append(head);

    if (st.thinking) {
      step.append(el('div', { class: 'trace-think' }, el('span', { text: st.thinking })));
    }
    if (st.content) {
      step.append(el('div', { class: 'trace-content' }, el('span', { text: st.content })));
    }

    // 这一步要求调用哪些工具。真正的执行结果在下一条 tool 步骤里。
    for (const call of (st.tool_calls || [])) {
      step.append(el('div', { class: 'trace-tool' },
        el('div', { class: 'trace-tool-head' },
          el('span', { class: 'trace-tool-name', text: call.name }),
          el('span', { class: 'trace-tool-args', text: summarizeArgs(call.args) }),
        )));
    }

    if (st.request || st.response) {
      const raw = el('details', { class: 'trace-raw' });
      raw.append(el('summary', { text: '原始请求与响应' }));
      if (st.request) raw.append(codeBlock('请求', st.request));
      if (st.response) raw.append(codeBlock('响应', st.response));
      step.append(raw);
    }

    return step;
  }

  // renderToolStep 复刻 PlannerToolResultCard：工具名、参数、执行结果摘要。
  //
  // 这是原来完全缺失的一环 —— 界面上只看到「Planner 决定调用 reply」，
  // 看不到 reply 实际返回了什么。
  function renderToolStep(st) {
    const step = el('div', { class: 'trace-step', 'data-tone': st.failed ? 'error' : 'tool' });

    const head = el('div', { class: 'trace-step-head' },
      el('span', { class: 'trace-step-title mono', text: st.name || 'unknown' }),
      el('span', { class: 'trace-step-time', text: fmtClockSec(st.at) }),
    );
    if (st.duration_ms) head.append(el('span', { class: 'trace-step-meta', text: fmtSec(st.duration_ms) }));
    head.append(el('span', {
      class: 'trace-status' + (st.failed ? ' fail' : ''),
      text: st.failed ? '执行失败' : '执行成功',
    }));
    step.append(head);

    // 参数：每个键值对一个小标签，参考实现的 ToolArgumentBlock。
    const args = safeParse(st.args);
    const keys = Object.keys(args);
    if (keys.length) {
      const row = el('div', { class: 'trace-args' });
      for (const k of keys) {
        row.append(el('span', { class: 'trace-arg' },
          el('span', { class: 'trace-arg-key', text: k }),
          el('span', { class: 'trace-arg-eq', text: '=' }),
          el('span', { class: 'trace-arg-val', text: shortValue(args[k]) }),
        ));
      }
      step.append(row);
    }

    // 执行结果摘要
    if (st.result) {
      step.append(el('div', { class: 'trace-result' },
        el('span', { class: 'trace-result-label', text: '执行结果' }),
        el('span', { class: 'trace-result-text', text: st.result }),
      ));
    }

    return step;
  }

  // renderReplyerStep 复刻 ReplierResponseCard：正文、思考、模型与 token。
  function renderReplyerStep(st) {
    const step = el('div', { class: 'trace-step', 'data-tone': st.failed ? 'error' : 'success' });

    const head = el('div', { class: 'trace-step-head' },
      el('span', { class: 'trace-step-title', text: '回复器' }),
    );
    if (st.model) head.append(el('span', { class: 'trace-model-badge', text: '模型: ' + st.model }));
    head.append(el('span', { class: 'trace-step-time', text: fmtClockSec(st.at) }));
    if (st.duration_ms) head.append(el('span', { class: 'trace-step-meta', text: fmtSec(st.duration_ms) }));
    head.append(el('span', {
      class: 'trace-status' + (st.failed ? ' fail' : ''),
      text: st.failed ? '失败' : '成功',
    }));
    step.append(head);

    if (st.content) {
      step.append(el('div', { class: 'trace-content' }, el('span', { text: st.content })));
    }
    if (st.thinking) {
      // 思考收进折叠区：回复器的思考通常比 Planner 短，但也不该占正文位置。
      const det = el('details', { class: 'trace-raw' });
      det.append(el('summary', { text: '思考过程' }));
      det.append(el('div', { class: 'trace-think' }, el('span', { text: st.thinking })));
      step.append(det);
    }

    // token 用量，参考实现是「输入 / 输出 / 总计」三联。
    if (st.prompt_tokens || st.completion_tokens || st.total_tokens) {
      step.append(el('div', { class: 'trace-tokens' },
        el('span', { text: '输入 ' + fmtNum(st.prompt_tokens || 0) }),
        el('span', { text: '输出 ' + fmtNum(st.completion_tokens || 0) }),
        el('span', { text: '总计 ' + fmtNum(st.total_tokens || 0) }),
      ));
    }

    return step;
  }

  // shortValue 把参数值压成一行短文本。
  function shortValue(v) {
    let text = typeof v === 'string' ? v : JSON.stringify(v);
    if (text === undefined) text = String(v);
    if (text.length > 40) text = text.slice(0, 40) + '…';
    return text;
  }

  // renderGate 渲染一次门控判断。
  //
  // 对应参考实现的 TimingGateCard（maisaka-monitor.tsx:1031）：
  // 「反应」标签 + 三种动作徽标（继续执行/等待/不回复）+ 判定说明。
  // 这是回答「机器人刚才为什么没理我」的唯一线索。
  function renderGate(gate) {
    const labels = { continue: '继续执行', wait: '等待', no_action: '不回复' };
    const box = el('div', { class: 'trace-gate', 'data-action': gate.action });
    box.append(el('span', { class: 'trace-gate-label', text: '反应' }));
    box.append(el('span', { class: 'trace-gate-action', text: labels[gate.action] || gate.action }));
    if (gate.action === 'wait' && gate.defer_seconds) {
      box.append(el('span', { class: 'trace-gate-meta', text: '等待 ' + gate.defer_seconds.toFixed(1) + ' 秒' }));
    }
    if (gate.detail) {
      box.append(el('span', { class: 'trace-gate-detail', text: gate.detail }));
    }
    return box;
  }

  // renderTrim 渲染一次上下文裁切。
  //
  // 对应参考实现的 ContextTrimCard（maisaka-monitor.tsx:2166）。
  // 它意味着「模型从这一刻起看不到更早的内容了」。
  function renderTrim(trim) {
    return el('div', { class: 'trace-trim' },
      el('span', { class: 'trace-trim-label', text: '上下文裁切' }),
      el('span', {
        class: 'trace-trim-detail',
        text: '裁掉 ' + trim.removed + ' 条，保留 ' + trim.keep + ' 条',
      }),
    );
  }

  // safeParse 解析 JSON，失败返回空对象。
  function safeParse(raw) {
    if (!raw) return {};
    try { return JSON.parse(raw) || {}; } catch { return {}; }
  }

  // fmtSec 把毫秒格式化成「1.23s」。
  function fmtSec(ms) {
    const v = Number(ms) || 0;
    if (v < 1000) return v + 'ms';
    return (v / 1000).toFixed(2) + 's';
  }

  /* ===================== 消息渲染 ===================== */

  async function viewChats(host, actions) {
    const pane = el('div', { class: 'chats' });

    /* --- 左：会话列表 --- */
    const search = el('input', {
      class: 'input',
      placeholder: '搜索会话…',
      value: state.sessionQuery,
      oninput: (e) => {
        state.sessionQuery = e.target.value;
        renderSessions();
      },
    });

    const list = el('div', { class: 'session-list' });
    const sessionPane = el('div', { class: 'session-pane' },
      el('div', { class: 'session-search' }, search),
      list,
    );

    /* --- 右：对话 --- */
    // 移动端需要从对话退回列表的入口，否则选了会话就出不去。
    const backBtn = el('button', {
      class: 'btn btn-ghost btn-icon chat-back',
      type: 'button',
      title: '返回会话列表',
      'aria-label': '返回会话列表',
      onclick: () => {
        pane.classList.remove('has-selection');
      },
    }, icon('chevron'));

    const head = el('div', { class: 'chat-head' },
      backBtn,
      el('div', { class: 'chat-head-main' },
        el('h3', { text: '未选择会话' }),
        el('p', { text: '' }),
      ),
      el('button', {
        class: 'btn btn-ghost btn-icon chat-more',
        type: 'button',
        title: '会话操作',
        'aria-label': '会话操作',
        onclick: () => {
          if (!state.current) return;
          if (sheet.classList.contains('open')) {
            toggleSheet(false);
          } else {
            renderSheet(state.current);
            toggleSheet(true);
          }
        },
      }, icon('config')),
    );

    // 会话详情抽屉。
    //
    // 为什么从列表行的 hover 按钮搬到这里：删除会话会连带删消息、表达、
    // 画像，误触代价很大，而列表行上那个小垃圾桶离「切换会话」只有几像素。
    // 抽屉里三个动作都有完整文案，用户点之前能看清自己要删什么。
    const sheetBody = el('div', { class: 'sheet-body' });
    const sheet = el('aside', { class: 'sheet', id: 'chat-sheet' }, sheetBody);

    const messages = el('div', { class: 'messages' });
    // 没有发送框：会话页是观察窗口，以人类身份对话去「对话」页。
    // 旧的「以机器人身份发话」入口已移除（后端同名接口一并删掉）。
    const chatPane = el('div', { class: 'chat-pane' }, head, sheet, messages);

    pane.append(sessionPane, chatPane);
    host.append(pane);

    actions.append(el('button', {
      class: 'btn btn-outline btn-sm',
      onclick: () => loadSessions(true),
    }, icon('refresh'), '刷新'));

    /* --- 会话列表渲染 --- */
    function renderSessions() {
      clear(list);

      const q = state.sessionQuery.trim().toLowerCase();
      const items = q
        ? state.sessions.filter((s) =>
            (s.name || '').toLowerCase().includes(q) ||
            (s.target_id || '').toLowerCase().includes(q))
        : state.sessions;

      if (!items.length) {
        list.append(el('p', {
          class: 'state-inline',
          // 空态不提具体昵称：昵称是配置项，改完之后这里的硬码「木木」就成了错字。
          text: state.sessions.length ? '没有匹配的会话' : '还没有会话。在群里 @ 一下机器人试试。',
        }));
        return;
      }

      for (const s of items) {
        const active = s.id === state.sessionId;
        const btn = el('button', {
          class: 'session-item' + (active ? ' active' : ''),
          type: 'button',
          onclick: () => selectSession(s),
        },
          el('div', { class: 'session-top' },
            el('span', { class: 'session-name', text: s.name || s.target_id }),
            el('span', { class: 'session-time', text: fmtAgo(s.last_active) }),
          ),
          el('div', { class: 'session-meta' },
            el('span', { class: 'tag ' + (s.scope === 'group' ? '' : 'tag-off'),
              text: s.scope === 'group' ? '群聊' : '私聊' }),
            el('span', { text: `${fmtNum(s.message_count)} 条` }),
          ),
        );
        // 删除按钮不能嵌在 button 里（HTML 不允许，点击会同时触发外层），
        // 所以会话条目改成一个容器 + 里面的可点区域 + 删除按钮。
        const delB = el('button', {
          class: 'btn btn-ghost btn-icon session-del', type: 'button',
          title: '删除这个会话', 'aria-label': '删除这个会话',
          onclick: async (e) => {
            e.stopPropagation();
            const detail = `会话「${s.name || s.target_id}」的 `
              + `${fmtNum(s.message_count)} 条消息、以及属于它的表达与画像都会一起删掉。`;
            if (!confirmDelete('会话', detail)) return;
            try {
              await del('/api/sessions/delete?id=' + encodeURIComponent(s.id));
              toast('已删除会话');
              if (state.sessionId === s.id) state.sessionId = '';
              await loadSessions();
            } catch (err) {
              toast('删除失败：' + err.message, 'error');
            }
          },
        }, icon('trash'));
        list.append(el('div', { class: 'session-row' }, btn, delB));
      }
    }

    /* --- 加载 --- */
    async function loadSessions(reselect = false) {
      try {
        const data = await api('/api/sessions?limit=200');
        state.sessions = data.sessions || [];
      } catch (err) {
        clear(list).append(errorState(err, () => loadSessions()));
        return;
      }
      renderSessions();

      // 恢复上次选中的会话；会话已不存在就不选
      if (state.sessionId && !state.sessions.some((s) => s.id === state.sessionId)) {
        state.sessionId = null;
      }
      if (reselect || !state.sessionId) {
        const saved = loadSavedSession();
        if (saved && state.sessions.some((s) => s.id === saved)) {
          state.sessionId = saved;
        }
      }
      if (state.sessionId) {
        const s = state.sessions.find((x) => x.id === state.sessionId);
        if (s) await openSession(s, false);
        else renderSessions();
      }
    }

    function loadSavedSession() {
      try { return localStorage.getItem(STORE_SESSION); } catch { return null }
    }
    function saveSession(id) {
      try { localStorage.setItem(STORE_SESSION, id); } catch { /* ignore */ }
    }

    // 注册给事件流：任何会话来了新消息，左栏列表的「最后一条」要更新。
    // loadSessions 是本函数的闭包变量，事件分发器够不着，走注册表。
    liveHooks.sessions = () => loadSessions(false);

    async function selectSession(s) {
      if (pane.classList.contains('has-selection') && state.sessionId === s.id) return;
      await openSession(s, true);
    }

    /* --- 会话详情抽屉 --- */

    // toggleSheet 开关抽屉；传 false 强制关闭。
    // 关掉时顺便清空内容：不留下一个指向已切走会话的陈旧面板。
    function toggleSheet(force) {
      const open = force === undefined ? !sheet.classList.contains('open') : force;
      sheet.classList.toggle('open', open);
      // 抽屉打开时把「会话操作」按钮藏起来。
      //
      // 抽屉是 right:0 的整高浮层，而那个按钮就在右上角 ——
      // 抽屉自己的关闭按钮会正好压在它上面（实测 28x28px 完全重合）。
      // 两个按钮堆在同一处，点哪个都说不清，而且触发器被盖住后
      // 用户会以为「点不回去了」。
      pane.classList.toggle('sheet-open', open);
      if (!open) clear(sheetBody);
    }

    // renderSheet 画当前会话的操作面板。
    //
    // 三个动作全部是「对整会话生效的破坏性/写操作」，
    // 所以都放在抽屉里而不是列表行的 hover 按钮上。
    function renderSheet(s) {
      clear(sheetBody);

      sheetBody.append(el('div', { class: 'sheet-head' },
        el('h3', { text: '会话操作' }),
        el('button', {
          class: 'btn btn-ghost btn-icon', type: 'button',
          'aria-label': '关闭', onclick: () => toggleSheet(false),
        }, icon('close')),
      ));

      // 基本信息：排查「这个会话到底是哪个群」时用得着。
      for (const [k, v] of [
        ['会话 ID', s.id],
        ['目标 ID', s.target_id],
        ['类型', s.scope === 'group' ? '群聊' : '私聊'],
        ['消息数', fmtNum(s.message_count)],
      ]) {
        sheetBody.append(el('div', { class: 'cfg-row' },
          el('span', { class: 'k', text: k }),
          el('span', { class: 'v mono', text: String(v || '—') }),
        ));
      }

      /* --- 重命名 --- */
      const nameInput = el('input', {
        class: 'input',
        value: s.name || '',
        placeholder: s.target_id || '会话名',
      });
      const nameBtn = el('button', {
        class: 'btn btn-outline btn-sm', type: 'button', text: '保存名字',
        onclick: async () => {
          const name = nameInput.value.trim();
          if (!name) {
            toast('名字不能为空', 'error');
            return;
          }
          nameBtn.disabled = true;
          try {
            await api('/api/sessions/rename', {
              method: 'PATCH',
              body: JSON.stringify({ id: s.id, name }),
            });
            s.name = name;
            $('.chat-head h3', head).textContent = name;
            toast('名字已更新');
            await loadSessions();
          } catch (err) {
            toast('保存失败：' + err.message, 'error');
          } finally {
            nameBtn.disabled = false;
          }
        },
      });

      sheetBody.append(el('div', { class: 'sheet-sec' },
        el('h4', { text: '重命名' }),
        el('p', { class: 'hint', text: 'QQ 官方不提供群名，默认显示的是目标 ID。' }),
        nameInput,
        el('div', { class: 'row', style: 'margin-top:8px' }, nameBtn),
      ));

      /* --- 清空消息 --- */
      // 与「删除会话」分开：前者只删聊天记录，画像与学到的表达保留。
      const clearBtn = el('button', {
        class: 'btn btn-outline btn-danger btn-sm', type: 'button', text: '清空消息',
        onclick: async () => {
          const detail = `会删掉本会话的 ${fmtNum(s.message_count)} 条消息，`
            + '人物画像与学到的表达会保留。聊天回想不会自动删。';
          if (!confirmDelete('会话消息', detail)) return;
          clearBtn.disabled = true;
          try {
            const res = await del('/api/messages/delete?session_id=' + encodeURIComponent(s.id));
            toast(`已清空 ${fmtNum(res.deleted || 0)} 条消息`);
            await loadSessions();
          } catch (err) {
            toast('清空失败：' + err.message, 'error');
            clearBtn.disabled = false;
          }
        },
      });

      sheetBody.append(el('div', { class: 'sheet-sec' },
        el('h4', { text: '清空聊天记录' }),
        el('p', { class: 'hint', text: '只删消息，图片文件与画像不受影响。' }),
        clearBtn,
      ));

      /* --- 删除会话 --- */
      const delBtnEl = el('button', {
        class: 'btn btn-outline btn-danger btn-sm', type: 'button', text: '删除会话',
        onclick: async () => {
          const detail = `会话「${s.name || s.target_id}」的 ${fmtNum(s.message_count)} 条消息、`
            + '以及属于它的表达与画像都会一起删掉。';
          if (!confirmDelete('会话', detail)) return;
          delBtnEl.disabled = true;
          try {
            await del('/api/sessions/delete?id=' + encodeURIComponent(s.id));
            toast('已删除会话');
            if (state.sessionId === s.id) {
              state.sessionId = '';
              pane.classList.remove('has-selection');
            }
            toggleSheet(false);
            await loadSessions();
          } catch (err) {
            toast('删除失败：' + err.message, 'error');
            delBtnEl.disabled = false;
          }
        },
      });

      sheetBody.append(el('div', { class: 'sheet-sec' },
        el('h4', { text: '删除会话' }),
        el('p', { class: 'hint', text: '连同该会话的消息、表达、画像、聊天回想一起删，不可恢复。' }),
        delBtnEl,
      ));
    }

    async function openSession(s, scrollToBottom) {
      state.sessionId = s.id;
      state.current = s;
      saveSession(s.id);
      renderSessions();
      pane.classList.add('has-selection');
      toggleSheet(false);

      $('.chat-head h3', head).textContent = s.name || s.target_id;
      $('.chat-head p', head).textContent =
        `${s.scope === 'group' ? '群聊' : '私聊'} · ${fmtNum(s.message_count)} 条 · ${s.target_id}`;

      await loadMessages(scrollToBottom);
    }

    async function loadMessages(scrollToBottom = true) {
      if (!state.sessionId) return;
      clear(messages).append(el('div', { class: 'state-inline', text: '加载中…' }));

      let data;
      try {
        data = await api(`/api/messages?session_id=${encodeURIComponent(state.sessionId)}&limit=100`);
      } catch (err) {
        clear(messages).append(errorState(err, () => loadMessages(scrollToBottom)));
        return;
      }

      const list_ = data.messages || [];
      state.oldestMS = data.oldest_ms || 0;
      state.newestMS = data.newest_ms || 0;
      state.hasMore = !!data.has_more;

      clear(messages);

      if (!list_.length) {
        messages.append(el('div', { class: 'state' },
          icon('chat'),
          el('p', { text: '这个会话还没有消息。' }),
        ));
        return;
      }

      if (state.hasMore) {
        messages.append(el('div', { class: 'load-more' },
          el('button', { class: 'btn btn-ghost btn-sm', onclick: loadMore, text: '加载更早的消息' })));
      }

      // 优先用后端混排好的时间线（消息 + 思考）；
      // 老接口不带 timeline 时退回纯消息列表。
      const entries = data.timeline && data.timeline.length ? data.timeline : list_;
      messages.append(buildTimeline(entries));
      state.lastMsg = list_[list_.length - 1] || null;

      if (scrollToBottom) messages.scrollTop = messages.scrollHeight;
    }

    async function loadMore() {
      if (!state.oldestMS) return;
      const btn = $('.load-more .btn', messages);
      if (btn) { btn.disabled = true; btn.textContent = '加载中…'; }

      let data;
      try {
        data = await api(`/api/messages?session_id=${encodeURIComponent(state.sessionId)}&limit=100&before_ms=${state.oldestMS}`);
      } catch (err) {
        toast(err.message, 'error');
        if (btn) { btn.disabled = false; btn.textContent = '加载更早的消息'; }
        return;
      }

      const older = data.messages || [];
      state.hasMore = !!data.has_more;
      if (data.oldest_ms) state.oldestMS = data.oldest_ms;
      if (!older.length) {
        if (btn) btn.remove();
        return;
      }

      // 插入历史会把内容推下去，不补偿滚动位置的话视觉上会「跳」一大截。
      const beforeH = messages.scrollHeight;
      const beforeTop = messages.scrollTop;

      // 插到「加载更多」按钮之后、最早一条消息之前。
      //
      // 这里不传 prev：older 是「更早的一批」，它内部自己合并就够了。
      // 它与下方已有消息的接缝处可能多出一个头像 —— 这是刻意的取舍：
      // 想要完美接缝就得重渲染已经渲染过的第一条，代价不值得。
      const olderEntries = data.timeline && data.timeline.length ? data.timeline : older;
      const anchor = $('.load-more', messages);
      messages.insertBefore(
        buildTimeline(olderEntries),
        anchor ? anchor.nextSibling : messages.firstChild);

      messages.scrollTop = messages.scrollHeight - beforeH + beforeTop;

      if (!state.hasMore && btn) btn.remove();
      else if (btn) { btn.disabled = false; btn.textContent = '加载更早的消息'; }
    }

    /* --- 会话页不再有发送框（以机器人身份发话已移除，对话去「对话」页） --- */

    await loadSessions();
  }

  /* ===================== 对话（网页对话窗口） ===================== */

  // 对话页：以人类身份与机器人私聊，对应 MaiBot dashboard 的 /chat。
  //
  // 会话页是「看机器人怎么工作」（单列观察流），这一页是「跟机器人说话」
  // （IM 双列：木木在左、网页用户在右）。消息投进 platform=webui 的
  // 虚拟会话，机器人回复落库后由下方全局轮询带回来。
  //
  // talkState 放模块级而不是视图内：全局轮询线程要读游标，
  // 重进页面时 loadTalkHistory 会重拉历史并重置。
  const talkState = {
    sessionId: '',
    botName: '',
    newestMS: 0,
    nick: '网友',
    typingEl: null,
  };

  const STORE_TALK_NICK = 'omq.talk-nick';

  function loadTalkNick() {
    try {
      const v = localStorage.getItem(STORE_TALK_NICK);
      if (v && v.trim()) talkState.nick = v.trim().slice(0, 24);
    } catch { /* 隐私模式忽略 */ }
  }

  function saveTalkNick(v) {
    try { localStorage.setItem(STORE_TALK_NICK, v); } catch { /* ignore */ }
  }

  // 「正在输入」指示：发送后机器人要经历防抖 + 思考 + 打字延迟，
  // 期间什么都不显示的话体验就是「发了没人理」。
  // typingEl 挂在 talkState 上而不是闭包里：轮询线程也要能撤掉它。
  function showTalkTyping() {
    hideTalkTyping();
    const box = $('.talk-msgs');
    if (!box) return;
    const node = el('div', { class: 'talk-typing' },
      renderAvatar(talkState.botName || '木', true),
      el('div', { class: 'talk-typing-text', text: (talkState.botName || '木木') + ' 正在输入…' }),
    );
    box.append(node);
    talkState.typingEl = node;
    box.scrollTop = box.scrollHeight;
  }

  function hideTalkTyping() {
    if (talkState.typingEl) {
      talkState.typingEl.remove();
      talkState.typingEl = null;
    }
  }

  // renderTalkMsg 把一条消息画成对话气泡。
  //
  // 与会话页的 renderMessage 刻意不共用：那是左对齐观察流（含引用块、
  // 推理事件穿插），这是 IM 双列，两边视觉语言不同；共用一份只会
  // 互相迁就出第三个谁都不像的东西。
  function renderTalkMsg(m) {
    const mine = !m.is_self;
    const name = m.is_self ? (talkState.botName || '机器人') : (m.user_name || talkState.nick);
    // 对话窗口里的正文优先取 text（不含占位），空正文退回 plain_text
    // （机器人带媒体的消息会渲染成「正文 [图片]」这样的占位文案）。
    const text = (m.text || '').trim() || (m.plain_text || '').trim() || '…';

    const row = el('div', { class: 'talk-msg' + (mine ? ' mine' : '') });
    const avatar = renderAvatar(name, m.is_self);
    const col = el('div', { class: 'talk-col' },
      el('div', { class: 'talk-name', text: name }),
      el('div', { class: 'talk-bubble', text }),
      el('div', { class: 'talk-time', text: fmtClockSec(m.created_ms) }),
    );
    if (mine) row.append(col, avatar);
    else row.append(avatar, col);
    return row;
  }

  // makeNickBtn 造「身份」按钮，点击就地变输入框改昵称。
  //
  // 昵称随每条消息发给后端（模型看到的发言者名字），存 localStorage。
  // 改名后要重建按钮而不是改 span 文本 —— el() 的构造参数里
  // text 节点是创建时写死的，就地改 textContent 会在下次重建时回退。
  function makeNickBtn() {
    const btn = el('button', {
      class: 'talk-nick', type: 'button',
      title: '点击修改你在对话里的名字',
    },
      el('span', { class: 'talk-nick-k', text: '我' }),
      el('span', { class: 'talk-nick-v', text: talkState.nick }),
    );
    btn.addEventListener('click', () => {
      const input = el('input', {
        class: 'talk-nick-input', value: talkState.nick,
        maxlength: '24', 'aria-label': '我的昵称',
      });
      btn.replaceWith(input);
      input.focus();
      input.select();
      const commit = () => {
        const v = input.value.trim() || '网友';
        talkState.nick = v;
        saveTalkNick(v);
        input.replaceWith(makeNickBtn());
      };
      input.addEventListener('blur', commit);
      input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); input.blur(); }
        if (e.key === 'Escape') { input.value = talkState.nick; input.blur(); }
      });
    });
    return btn;
  }

  async function viewTalk(host) {
    loadTalkNick();
    talkState.newestMS = 0;

    const pane = el('div', { class: 'talk' });

    const headName = el('span', { class: 'talk-head-name', text: talkState.botName || '…' });
    const head = el('div', { class: 'talk-head' },
      renderAvatar('木', true),
      el('div', { class: 'talk-head-main' },
        headName,
        el('p', { text: '网页对话窗口 · 消息只进本地会话，不发往 QQ' }),
      ),
    );

    const msgs = el('div', { class: 'talk-msgs' });

    const ta = el('textarea', {
      placeholder: '以人类身份说点什么…', rows: '1',
      'aria-label': '对话输入框',
    });
    const sendBtn = el('button', { class: 'btn btn-primary', type: 'submit', text: '发送' });
    const composer = el('form', { class: 'composer talk-composer' },
      makeNickBtn(),
      el('div', { class: 'talk-composer-main' }, ta, sendBtn),
    );

    pane.append(head, msgs, composer);
    host.append(pane);

    /* --- 发送 --- */
    composer.addEventListener('submit', async (e) => {
      e.preventDefault();
      const text = ta.value.trim();
      if (!text || !talkState.sessionId) return;

      sendBtn.disabled = true;
      try {
        await api('/api/chat/send', {
          method: 'POST',
          body: JSON.stringify({ text, user_name: talkState.nick }),
        });
        ta.value = '';
        ta.style.height = 'auto';

        // 乐观显示自己这条。游标直接推到「现在」：服务端给这条消息
        // 的时间戳一定早于此刻，轮询不会把它再拉一遍。
        msgs.append(renderTalkMsg({
          is_self: false, user_name: talkState.nick,
          text, created_ms: Date.now(),
        }));
        talkState.newestMS = Date.now();
        showTalkTyping();
        msgs.scrollTop = msgs.scrollHeight;
      } catch (err) {
        toast('发送失败：' + err.message, 'error');
      } finally {
        sendBtn.disabled = false;
        ta.focus();
      }
    });

    // Enter 发送，Shift+Enter 换行（与会话页旧输入框一致的键位习惯）
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        composer.requestSubmit();
      }
    });
    ta.addEventListener('input', () => {
      ta.style.height = 'auto';
      ta.style.height = Math.min(ta.scrollHeight, 132) + 'px';
    });

    /* --- 历史 --- */
    try {
      const info = await api('/api/chat/session');
      talkState.sessionId = info.session_id || '';
      talkState.botName = info.bot_name || '机器人';
      headName.textContent = talkState.botName;

      const data = await api(
        `/api/messages?session_id=${encodeURIComponent(talkState.sessionId)}&limit=50`);
      for (const m of data.messages || []) msgs.append(renderTalkMsg(m));
      talkState.newestMS = data.newest_ms || Date.now();
    } catch (err) {
      msgs.append(el('p', { class: 'state-inline', text: '对话窗口加载失败：' + err.message }));
    }
    msgs.scrollTop = msgs.scrollHeight;
  }

  // summarizeArgs 把工具参数压成一行。
  //
  // 参数是 JSON 字符串，直接铺开会把流程撑得很长；
  // 这里只取键值对拼一行，想细看就展开下面的原始响应。
  function summarizeArgs(rawArgs) {
    if (!rawArgs) return '';
    let obj;
    try { obj = JSON.parse(rawArgs); } catch { return String(rawArgs).slice(0, 80); }
    const parts = [];
    for (const [k, v] of Object.entries(obj)) {
      let text = typeof v === 'string' ? v : JSON.stringify(v);
      if (text === undefined) text = String(v);
      if (text.length > 40) text = text.slice(0, 40) + '…';
      parts.push(k + '=' + text);
    }
    return parts.join('  ');
  }

  function codeBlock(title, raw) {
    const block = el('div', { class: 'reason-block' },
      el('div', { class: 'reason-label', text: title }),
      el('pre', { class: 'code', text: prettyJSON(raw) }),
    );
    return block;
  }

  function prettyJSON(raw) {
    if (!raw) return '(空)';
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return String(raw);
    }
  }

  /* ===================== 记忆 ===================== */

  // viewMemory 按页签只渲染一块。
  //
  // 照抄 MaiBot 的结构：它把「记录 / 画像 / 图片 / 图谱 …」折进长期记忆
  // 一个入口，用 ?tab= 区分。这里只有「聊天回想」和「人物画像」两块，
  // 折进一个入口后侧栏从 3 条记忆相关项变成 1 条。
  async function viewMemory(host, actions, params) {
    const tab = routeTab('memory', params || new URLSearchParams());
    if (tab === 'profiles') {
      await viewProfiles(host, actions);
      return;
    }
    await viewRecall(host, actions);
  }

  async function viewProfiles(host, actions) {
    const page = el('div', { class: 'page' });
    host.append(page);
    void actions;

    /* --- 画像 --- */
    const profileBox = el('div', { class: 'profile-list' });
    const profileMeta = el('span', { class: 'meta' });

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' },
        el('h3', { text: '人物画像' }),
        profileMeta,
      ),
      profileBox,
      el('p', { class: 'hint' },
        '画像由模型按分节合并生成，不是原文摘抄。消息数是「本人发言累计」的条数。'),
    ));

    let data;
    try {
      data = await api('/api/profiles?limit=200');
    } catch (err) {
      clear(profileBox).append(errorState(err, () => navigate()));
      return;
    }

    const items = data.items || [];
    profileMeta.textContent = `${items.length} 份`;
    clear(profileBox);

    if (!items.length) {
      profileBox.append(emptyState('还没有画像。同一个人在同一会话里攒够足够发言后才会生成。'));
      return;
    }

    for (const p of items) {
      // 删除按钮放在标题行：它是这张卡上唯一的写操作，
      // 放进 foot 会与「N 条发言 / 更新于」这种只读信息混在一起。
      const delB = el('button', {
        class: 'btn btn-ghost btn-icon btn-danger btn-sm',
        type: 'button',
        title: '删除这份画像',
        'aria-label': '删除这份画像',
        onclick: async (e) => {
          // 阻止冒泡：卡片本体绑了「展开/折叠」，不挡的话
          // 点删除会同时把卡片展开一下。
          e.stopPropagation();
          if (!confirmDelete('人物画像',
            `${p.display_name || p.user_id} 的画像。删掉后模型对他的了解会重置，`
            + '攒够发言后会重新生成。')) return;
          delB.disabled = true;
          try {
            await del('/api/profiles/delete?' + new URLSearchParams({
              platform: p.platform || '',
              user_id: p.user_id,
              session_id: p.session_id || '',
            }));
            toast('已删除');
            card.remove();
            profileMeta.textContent = `${profileBox.childElementCount} 份`;
          } catch (err) {
            toast('删除失败：' + err.message, 'error');
            delB.disabled = false;
          }
        },
      }, icon('trash'));

      const card = el('div', { class: 'profile-card' },
        el('div', { class: 'profile-head' },
          el('span', { class: 'profile-name', text: p.display_name || '未命名' }),
          el('span', { class: 'profile-id', text: p.user_id }),
          el('span', { class: 'spacer' }),
          delB,
        ),
        el('div', { class: 'profile-body', text: p.profile_text || '(空)' }),
        el('div', { class: 'profile-foot' },
          el('span', { text: `${fmtNum(p.message_count)} 条发言` }),
          el('span', { text: '更新于 ' + fmtAgo(p.updated_at) }),
          p.session_id
            ? el('span', { class: 'dim', text: '会话 ' + p.session_id.slice(0, 8) })
            : el('span', { class: 'dim', text: '跨会话' }),
        ),
      );
      // 点开看全文（卡片里默认 4 行截断）
      card.addEventListener('click', () => {
        card.classList.toggle('expanded');
        const body = $('.profile-body', card);
        body.style.webkitLineClamp = body.style.webkitLineClamp === 'unset' ? '4' : 'unset';
      });
      profileBox.append(card);
    }
  }

  async function viewRecall(host, actions) {
    const page = el('div', { class: 'page' });
    host.append(page);
    void actions;

    /* --- 回想 --- */
    const searchInput = el('input', {
      class: 'input',
      placeholder: '按关键词检索…',
      style: 'flex:1',
    });
    const form = el('form', { class: 'row', style: 'margin-bottom:12px' },
      el('div', { class: 'field', style: 'flex:1' }, icon('search'), searchInput),
      el('button', { class: 'btn btn-outline', type: 'submit', text: '检索' }),
    );

    const summaryBox = el('div', { class: 'memory-list' });
    const summaryMeta = el('span', { class: 'meta' });

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' },
        el('h3', { text: '聊天回想' }),
        summaryMeta,
      ),
      form,
      summaryBox,
      el('p', { class: 'hint' },
        '记忆在上下文被裁切时自动生成。不填关键词时列出最近的回想；',
        '检索走向量语义匹配，向量不可用时退回关键词。'),
    ));

    form.addEventListener('submit', (e) => {
      e.preventDefault();
      loadSummaries(searchInput.value.trim());
    });

    async function loadSummaries(q) {
      clear(summaryBox).append(skeleton(3, 84));
      summaryMeta.textContent = '';

      const params = new URLSearchParams({ limit: '50' });
      if (q) params.set('q', q);
      if (state.sessionId) params.set('session_id', state.sessionId);

      let data;
      try {
        data = await api('/api/memory?' + params);
      } catch (err) {
        clear(summaryBox).append(errorState(err, () => loadSummaries(q)));
        return;
      }

      const items = data.items || [];
      summaryMeta.textContent = `${items.length} 条`;
      clear(summaryBox);

      if (!items.length) {
        summaryBox.append(emptyState(
          q ? '没有匹配的记忆。' : '还没有记忆。聊天记录被裁切够多之后才会自动生成。'));
        return;
      }

      for (const it of items) {
        const head = el('div', { class: 'memory-head' },
          el('span', { text: it.time_range || fmtClock(it.created_at) }),
          it.participants && it.participants.length
            ? el('span', { class: 'dim', text: '· ' + it.participants.join('、') })
            : null,
          it.source_count ? el('span', { class: 'dim', text: `· 压缩 ${it.source_count} 条` }) : null,
          el('span', { class: 'spacer' }),
          it.embed_model
            ? el('span', { class: 'tag tag-off', text: '向量' })
            : el('span', { class: 'tag tag-off', text: '仅关键词' }),
          el('button', {
            class: 'btn btn-ghost btn-icon btn-danger btn-sm',
            title: '删除这条记忆',
            onclick: (e) => removeSummary(it, e.currentTarget),
          }, icon('trash')),
        );

        const item = el('div', { class: 'memory-item' },
          head,
          el('div', { class: 'memory-text', text: it.summary || '(空)' }),
        );

        if (it.recall_cues && it.recall_cues.length) {
          const cues = el('div', { class: 'memory-cues' });
          for (const cue of it.recall_cues) {
            cues.append(el('span', { class: 'tag', text: cue }));
          }
          item.append(cues);
        }

        summaryBox.append(item);
      }
    }

    async function removeSummary(it, btn) {
      if (!confirmAction('删除这条记忆？删除后无法恢复，检索不会再命中它。')) return;
      btn.disabled = true;
      try {
        await api(`/api/memory/delete?id=${encodeURIComponent(it.id)}`, { method: 'DELETE' });
        const card = btn.closest('.memory-item');
        if (card) card.remove();
        toast('已删除');
      } catch (err) {
        btn.disabled = false;
        toast('删除失败：' + err.message, 'error');
      }
    }

    await loadSummaries('');
  }

  /* ===================== 学习 ===================== */

  // 学习页拆成三个页签，对齐 MaiBot 的 /resource/learning：
  // 那边就是把「表达方式 / 黑话 / 表情包」折进一个侧栏入口。
  //
  // 为什么值得拆：三块数据完全独立，各自的检索框互不相干。
  // 挤在同一页时，表情包网格会把表达/黑话表挤到很下面。
  async function viewLearning(host, actions, params) {
    const tab = routeTab('learning', params || new URLSearchParams());
    const views = { expression: viewExpressions, jargon: viewJargons, emoji: viewEmojis };
    const view = views[tab] || viewExpressions;
    await view(host, actions);
  }

  async function viewExpressions(host, actions) {
    const page = el('div', { class: 'page' });
    host.append(page);
    void actions;

    const exprTable = el('table', { class: 'table' },
      el('thead', {}, el('tr', {},
        el('th', { text: '情境' }),
        el('th', { text: '说法' }),
        el('th', { class: 'num', text: '用过' }),
        el('th', { text: '最近' }),
        el('th', { class: 'act', text: '' }),
      )),
      el('tbody', {}),
    );
    const exprMeta = el('span', { class: 'meta' });

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '表达' }), exprMeta),
      el('div', { class: 'table-wrap' }, exprTable),
    ));

    page.append(el('p', { class: 'hint' },
      '学习默认关闭（learning.expression_enabled），因为它要花模型调用。'));

    const tbody = $('tbody', exprTable);
    clear(tbody).append(el('tr', {}, el('td', { colspan: '4', class: 'muted', text: '加载中…' })));

    let data;
    try {
      data = await api('/api/expressions?limit=100');
    } catch (err) {
      clear(tbody).append(el('tr', {}, el('td', { colspan: '4' }, errorState(err, () => navigate()))));
      return;
    }

    const items = data.items || [];
    exprMeta.textContent = `共 ${fmtNum(data.total)} 条`;
    clear(tbody);

    if (!items.length) {
      clear(tbody).append(el('tr', {}, el('td', { colspan: '4' },
        el('div', { class: 'state-inline', text: '还没有学到表达。学习功能默认关闭。' }))));
      return;
    }

    const reload = () => navigate();
    for (const it of items) {
      tbody.append(el('tr', {},
        el('td', { text: it.situation || '—' }),
        el('td', { text: it.style || '—' }),
        el('td', { class: 'num', text: fmtNum(it.used_count) }),
        el('td', { class: 'muted nowrap', text: fmtAgo(it.last_used_at) || '—' }),
        el('td', { class: 'act' }, delBtn('表达的这条说法', it.style, () =>
          del('/api/expressions/delete?id=' + it.id), reload)),
      ));
    }
  }

  async function viewJargons(host, actions) {
    const page = el('div', { class: 'page' });
    host.append(page);
    void actions;

    const jargonTable = el('table', { class: 'table' },
      el('thead', {}, el('tr', {},
        el('th', { text: '词条' }),
        el('th', { text: '含义' }),
        el('th', { class: 'num', text: '出现' }),
        el('th', { class: 'act', text: '' }),
      )),
      el('tbody', {}),
    );
    const jargonMeta = el('span', { class: 'meta' });

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '黑话' }), jargonMeta),
      el('div', { class: 'table-wrap' }, jargonTable),
    ));

    page.append(el('p', { class: 'hint' },
      '同一个词出现 4 次后才会触发含义推断，8 / 25 / 100 次会再推断一次。'));

    const tbody = $('tbody', jargonTable);
    clear(tbody).append(el('tr', {}, el('td', { colspan: '4', class: 'muted', text: '加载中…' })));

    let data;
    try {
      data = await api('/api/jargons?limit=100');
    } catch (err) {
      clear(tbody).append(el('tr', {}, el('td', { colspan: '4' }, errorState(err, () => navigate()))));
      return;
    }

    const items = data.items || [];
    jargonMeta.textContent = `共 ${fmtNum(data.total)} 条`;
    clear(tbody);

    if (!items.length) {
      clear(tbody).append(el('tr', {}, el('td', { colspan: '4' },
        el('div', { class: 'state-inline', text: '还没有学到黑话。同一个词出现 4 次后才会推断含义。' }))));
      return;
    }

    const reload = () => navigate();
    for (const it of items) {
      tbody.append(el('tr', {},
        el('td', { class: 'mono', text: it.content || '—' }),
        el('td', {
          class: it.meaning ? '' : 'muted',
          text: it.meaning || '待推断',
        }),
        el('td', { class: 'num', text: fmtNum(it.count) }),
        el('td', { class: 'act' }, delBtn('这条黑话', it.content, () =>
          del('/api/jargons/delete?id=' + it.id), reload)),
      ));
    }
  }

  async function viewEmojis(host, actions) {
    const page = el('div', { class: 'page' });
    host.append(page);
    void actions;

    const emojiGrid = el('div', { class: 'emoji-grid' });
    const emojiMeta = el('span', { class: 'meta' });
    const emojiSearch = el('input', { class: 'input', placeholder: '按描述检索…' });
    const emojiForm = el('form', { class: 'row', style: 'margin-bottom:12px' },
      el('div', { class: 'field', style: 'flex:1' }, icon('search'), emojiSearch),
      el('button', { class: 'btn btn-outline', type: 'submit', text: '检索' }),
    );

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '表情包' }), emojiMeta),
      emojiForm,
      emojiGrid,
    ));

    page.append(el('p', { class: 'hint' },
      'emoji.collect_enabled 控制自动收藏。标签由视觉模型异步打，',
      '刚收进来时描述是空的，检索不到是正常的。'));

    emojiForm.addEventListener('submit', (e) => {
      e.preventDefault();
      loadEmojis(emojiSearch.value.trim());
    });

    async function loadEmojis(q) {
      clear(emojiGrid).append(skeleton(2, 104));
      emojiMeta.textContent = '';

      let data;
      try {
        const params = new URLSearchParams({ limit: '60' });
        if (q) params.set('q', q);
        data = await api('/api/emojis?' + params);
      } catch (err) {
        clear(emojiGrid).append(errorState(err, () => loadEmojis(q)));
        return;
      }

      const items = data.items || [];
      emojiMeta.textContent = `库内 ${fmtNum(data.total)} 个`;
      clear(emojiGrid);

      if (!items.length) {
        emojiGrid.append(emptyState(
          q ? '没有匹配的表情包。' : '还没有收藏表情包。别人在群里发表情时会自动收进来。'));
        return;
      }

      for (const it of items) {
        const img = el('img', {
          loading: 'lazy',
          alt: it.description || '表情包',
          src: `/api/emoji/image?hash=${encodeURIComponent(it.file_hash)}`,
        });
        const fig = el('figure', { class: 'emoji-cell' }, img,
          el('figcaption', { text: it.description || '未打标签' }),
          el('div', { class: 'emoji-cell-act' },
            delBtn('这个表情包', it.description || it.file_hash.slice(0, 12), () =>
              del('/api/emojis/delete?id=' + it.id), () => loadEmojis(q))));

        // 图片可能已经被删掉或格式不受支持，失败时换成占位而不是碎图标
        img.addEventListener('error', () => {
          const ph = el('div', { class: 'emoji-broken', text: '无法显示' });
          img.replaceWith(ph);
        }, { once: true });

        emojiGrid.append(fig);
      }
    }

    await loadEmojis('');
  }

  /* ===================== Prompt 管理 =====================
   *
   * 对应参考实现的 /config/prompts（routes/config/prompts.tsx）：
   * 左侧列出全部提示词，右侧编辑内容，另有「恢复默认」。
   *
   * 我们只做这一层，不做它的版本历史（versions / activate）：
   * 那边是为了反复调参对比，删掉它用 git 管更合适。
   *
   * 提示词的存储方式（internal/prompt/manager.go）：
   * 磁盘 prompts/zh-CN/*.prompt 优先，没有就用二进制内嵌的默认模板。
   * 所以「恢复默认」= 删掉磁盘上那个文件。
   *
   * 现在暴露的是**全部** 11 份模板，不是早期那 4 份：
   * 聊天回想、人物画像、表达学习、黑话提取、黑话含义推断（两份）、
   * 表情包打标签也都能改。这些以前硬编码在各自包里的 const 里，
   * 用户想调一句措辞只能改代码 —— 而它们同样是「跑模型的一句话」，
   * 凭什么是提示词的一部分却不给改。
   *
   * 高级项（advanced）默认收起来：11 份平铺会让「改哪份」变难判断，
   * 而其中真正日常会调的只有规划 / 回复两份。
   */

  async function viewPrompts(host, actions) {
    void actions;
    host.append(el('div', { class: 'page' }, skeleton(3, 120)));

    let data;
    try {
      data = await api('/api/prompts');
    } catch (err) {
      clear(host).append(errorState(err, () => navigate()));
      return;
    }

    const page = el('div', { class: 'page' });
    clear(host).append(page);

    const items = data.items || [];
    // 默认选中规划提示词。items 是后端按名字字典序给的，
    // 直接取第一个会落在 emoji_content_analysis 上 —— 打开页面看到一张
    // 表情包打标签的模板，会让人以为进错了地方。
    const preferred = items.find((it) => it.name === 'planner') || items[0] || {};
    const state = { name: preferred.name || '', content: '', def: '', customized: false };

    const aside = el('aside', { class: 'mdl-side' },
      el('div', { class: 'mdl-side-head' }, el('h3', { text: '提示词' })),
    );
    const list = el('div', { class: 'mdl-side-list' });

    // 主用一个「显示高级项」开关：默认只列日常会调的那几份。
    // 高级项的定义与参考实现的 .meta.toml advanced 字段一致，
    // 由后端 promptDescriptions 给（不是前端自己判断）。
    const advancedTools = items.filter((it) => it.advanced);
    const showAdvanced = el('label', { class: 'mdl-side-toggle' },
      el('input', {
        type: 'checkbox',
        onchange: (e) => {
          advancedTools.forEach((it) => {
            const btn = list.querySelector('[data-prompt="' + it.name + '"]');
            if (btn) btn.hidden = !e.currentTarget.checked;
          });
        },
      }),
      el('span', { text: '显示高级项（' + advancedTools.length + '）' }),
    );

    for (const it of items) {
      const btn = el('button', {
        class: 'mdl-side-item' + (it.name === state.name ? ' on' : ''),
        type: 'button',
        title: it.description || '',
        onclick: () => load(it.name),
      },
        el('span', { class: 'mdl-side-name', text: it.display_name || it.name }),
        el('span', { class: 'mdl-side-meta' },
          el('span', { text: it.name }),
          it.customized ? el('span', { class: 'tag', text: '已改' }) : null,
        ),
      );
      btn.dataset.prompt = it.name;
      if (it.advanced) btn.hidden = true;
      list.append(btn);
    }
    if (advancedTools.length) aside.append(showAdvanced);
    aside.append(list);

    const main = el('section', { class: 'mdl-main' });
    page.append(el('div', { class: 'mdl-body' }, aside, main));

    /* --- 编辑器 --- */
    const title = el('h3', { text: '' });
    const desc = el('p', { class: 'hint', text: '' });
    const badge = el('span', { class: 'tag', text: '' });
    const editor = el('textarea', { class: 'input cfg-textarea prompt-editor', spellcheck: 'false' });
    const status = el('span', { class: 'cfg-status' });
    // 占位符清单：后端在保存时会校验「不能增删占位符」，
    // 提前写在这里，用户就不用靠保存失败去猜哪几个必须留着。
    const placeholders = el('div', { class: 'prompt-ph' });

    const saveBtn = el('button', {
      class: 'btn btn-primary btn-sm', type: 'button', text: '保存', disabled: 'disabled',
      onclick: async () => {
        saveBtn.disabled = true;
        status.textContent = '保存中…';
        try {
          await api('/api/prompts', {
            method: 'PUT',
            body: JSON.stringify({ name: state.name, content: editor.value }),
          });
          state.content = editor.value;
          state.customized = true;
          badge.textContent = '已改（磁盘覆盖）';
          status.textContent = '';
          toast('提示词已保存，下一轮思考生效');
          refreshList();
        } catch (err) {
          status.textContent = '保存失败：' + err.message;
          saveBtn.disabled = false;
        }
      },
    });

    const resetBtn = el('button', {
      class: 'btn btn-outline btn-sm', type: 'button', text: '恢复默认',
      onclick: async () => {
        if (!window.confirm('恢复成内置默认提示词？\n磁盘上的自定义版本会被删除。')) return;
        try {
          const res = await api('/api/prompts?name=' + encodeURIComponent(state.name), { method: 'DELETE' });
          state.content = res.content || '';
          state.customized = false;
          editor.value = state.content;
          badge.textContent = '内置默认';
          toast('已恢复默认');
          refreshList();
        } catch (err) {
          toast('恢复失败：' + err.message, 'error');
        }
      },
    });

    editor.addEventListener('input', () => {
      saveBtn.disabled = editor.value === state.content;
    });

    const card = el('div', { class: 'mdl-card' },
      el('div', { class: 'mdl-card-head' }, title, badge, desc),
      editor,
      placeholders,
      el('div', { class: 'cfg-save' }, status, el('span', { class: 'spacer' }), resetBtn, saveBtn),
    );

    // 目录未配置时保存会失败，提前说明比让用户改完才发现好。
    if (!data.dir) {
      card.append(el('p', { class: 'hint' },
        '没有配置提示词目录，保存会失败。请在配置文件里指定 prompts 目录后重启。'));
    }
    main.append(card);

    // renderPlaceholders 画占位符清单。
    //
    // 每一项都可以点一下插到光标处 —— 这是唯一能保证「不改坏」的交互：
    // 手打 {bot_alias_notice} 很容易打错一个字母，而那种错表现为
    // 那段内容静默变空（保存时会拦住，但拦在保存时已经白改一遍了）。
    function renderPlaceholders(names) {
      clear(placeholders);
      if (!names.length) return;
      placeholders.append(el('span', { class: 'prompt-ph-label', text: '可用占位符' }));
      for (const ph of names) {
        placeholders.append(el('button', {
          class: 'prompt-ph-chip', type: 'button', text: '{' + ph + '}',
          onclick: () => {
            const start = editor.selectionStart || 0;
            const end = editor.selectionEnd || 0;
            const token = '{' + ph + '}';
            editor.value = editor.value.slice(0, start) + token + editor.value.slice(end);
            editor.focus();
            editor.selectionStart = editor.selectionEnd = start + token.length;
            saveBtn.disabled = editor.value === state.content;
          },
        }));
      }
    }

    async function load(name) {
      state.name = name;
      // 用 dataset.prompt 比对。以前这里比的是显示名，而显示名是
      // 「规划提示词」这类中文标题、不是唯一的 key，所以从来没匹配上过
      // —— 表现是「点左边另一项，高亮还留在原来那条」。
      for (const b of list.children) {
        b.classList.toggle('on', b.dataset.prompt === name);
      }
      refreshList();
      clear(main).append(el('p', { class: 'hint', text: '加载中…' }));
      try {
        const d = await api('/api/prompts?name=' + encodeURIComponent(name));
        state.content = d.content || '';
        state.customized = !!d.customized;
        const meta = items.find((i) => i.name === name) || {};
        title.textContent = meta.display_name || name;
        desc.textContent = meta.description || '';
        badge.textContent = d.customized ? '已改（磁盘覆盖）' : '内置默认';
        editor.value = state.content;
        saveBtn.disabled = true;
        status.textContent = '';
        renderPlaceholders(d.placeholders || []);
        clear(main).append(card);
      } catch (err) {
        clear(main).append(errorState(err, () => load(name)));
      }
    }

    // refreshList 重新拉列表以更新「已改」标记。
    //
    // 按 dataset.prompt 找对应项，不要再从 DOM 文本里反查名字：
    // 之前的写法读 `.mdl-side-meta span` 的第一段文本当 key，
    // 一旦那一段的层级或内容变了（本次就加了占位符提示），
    // 它就会静默对不上 —— 表现是「保存成功了但列表上没出现『已改』」。
    function refreshList() {
      api('/api/prompts').then((d) => {
        const byName = new Map((d.items || []).map((i) => [i.name, i]));
        for (const b of list.children) {
          const it = byName.get(b.dataset.prompt || '');
          if (!it) continue;
          const tag = $('.tag', b);
          if (it.customized && !tag) {
            $('.mdl-side-meta', b).append(el('span', { class: 'tag', text: '已改' }));
          } else if (!it.customized && tag) {
            tag.remove();
          }
        }
      }).catch(() => {});
    }

    if (state.name) await load(state.name);
  }

  /* ===================== 数据管理 =====================
   *
   * 对应参考实现的 /data-transfer（routers/data_transfer.py）：
   * 导出 / 导入。
   *
   * 差异（刻意做的简化，都写在界面上）：
   *   - 导出的是**数据库快照**，不含表情包图片文件（data/emoji 下的二进制）
   *   - 不做异步任务队列：数据量在几十 MB 内，同步几秒就完
   *   - 不做「可选包含 plugins / logs」：我们没有插件，日志另有保留期清理
   *   - 导入后必须重启才生效（连接池还指着旧文件），这一点如实告知
   */

  async function viewData(host, actions) {
    void actions;
    const page = el('div', { class: 'page' });
    clear(host).append(page);

    /* --- 概览：当前有什么 --- */
    let overview = null;
    try {
      overview = await api('/api/overview');
    } catch {
      overview = null;
    }

    const counts = overview && overview.counts ? overview.counts : {};
    // 复用概览页已有的 .metrics / .metric，不新造一套数字卡片样式。
    const stats = el('div', { class: 'metrics' });
    const statRows = [
      ['消息', counts.messages],
      ['会话', counts.sessions],
      ['记忆', counts.memory],
      ['画像', counts.profiles],
      ['表情包', counts.emojis],
      ['表达', counts.expressions],
      ['黑话', counts.jargons],
    ];
    for (const [label, value] of statRows) {
      stats.append(el('div', { class: 'metric' },
        el('div', { class: 'metric-k', text: label }),
        el('div', { class: 'metric-v', text: fmtNum(value) }),
      ));
    }
    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '当前数据' })),
      stats,
    ));

    /* --- 导出 --- */
    const exportBtn = el('a', {
      class: 'btn btn-primary', href: '/api/backup', download: '',
    }, icon('download'), '下载数据库快照');

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '导出' })),
      el('p', { class: 'hint' },
        '导出一个 .db 文件，包含消息、记忆、画像、表情包索引、表达与黑话。'),
      el('p', { class: 'hint' },
        '注意：表情包图片本体（data/emoji 下的文件）不在里面，'
        + '它是一个普通目录，需要单独备份。'),
      el('div', { class: 'row' }, exportBtn),
    ));

    /* --- 导入 --- */
    const fileInput = el('input', { class: 'input', type: 'file', accept: '.db,.sqlite,.sqlite3' });
    const confirmInput = el('input', { class: 'input', placeholder: '输入 REPLACE 确认' });
    const importStatus = el('span', { class: 'cfg-status' });
    const importBtn = el('button', {
      class: 'btn btn-outline btn-danger', type: 'button', text: '导入并覆盖',
      onclick: async () => {
        const f = fileInput.files && fileInput.files[0];
        if (!f) {
          importStatus.textContent = '请先选择文件';
          return;
        }
        if (confirmInput.value.trim() !== 'REPLACE') {
          importStatus.textContent = '请输入 REPLACE 确认';
          return;
        }
        if (!window.confirm('导入会覆盖当前全部数据。\n覆盖前会自动把现有数据备份到数据库旁边。\n\n继续？')) {
          return;
        }
        importBtn.disabled = true;
        importStatus.textContent = '导入中…';
        try {
          const res = await api('/api/backup?confirm=replace', {
            method: 'POST',
            headers: { 'Content-Type': 'application/octet-stream' },
            body: f,
          });
          importStatus.textContent = res.message || '已导入，请重启程序';
          toast('导入完成，请重启程序');
        } catch (err) {
          importStatus.textContent = '导入失败：' + err.message;
        } finally {
          importBtn.disabled = false;
        }
      },
    });

    page.append(el('div', { class: 'section' },
      el('div', { class: 'section-head' }, el('h3', { text: '导入' })),
      el('p', { class: 'hint' },
        '上传一个之前导出的 .db 文件。导入前会自动把当前数据备份成 '
        + 'oh-my-qqbot.db.before-import-<时间>，放在同一个目录。'),
      el('p', { class: 'hint' },
        '导入后必须重启程序才生效：正在使用的数据库连接还指旧文件。'),
      el('div', { class: 'field' }, fileInput),
      el('div', { class: 'field' }, confirmInput),
      el('div', { class: 'row' }, importBtn, importStatus),
    ));
  }

  /* ===================== 用量统计（概览页内嵌） =====================
   *
   * 这里原来是一个独立页面（侧栏「运行记录 → 用量统计」）。
   * 归并进概览的理由：
   *   - 参考实现把请求数 / Token / 花费三张趋势卡直接放在首页
   *     （refs/MaiBot/dashboard/src/routes/home/StatisticsCards.tsx）；
   *   - 它回答的是「这台机器现在跑得怎么样」，和首页其它指标是同一个问题，
   *     分成两个页面只会让人来回翻。
   *
   * 所以现在它是一个自带时间范围切换的区块，由 viewOverview 挂上去。
   * 数据仍走 /api/usage（按天聚合在后端算，前端只负责展示）。
   */

  // usageSection 造一块「模型调用」区块。
  //
  // 自带 days 状态与刷新逻辑：概览页的整体刷新按钮不该连带重置
  // 用户刚选好的时间范围，所以这里的 load() 与页面刷新是独立的。
  function usageSection() {
    // 范围放 state：概览页会整段静默重绘，局部变量会被重置回「今天」，
    // 用户刚选的 30 天下一秒就被打回去了。
    let days = state.usageDays;

    const ranges = [[1, '今天'], [7, '7 天'], [30, '30 天']];
    const metrics = el('div', { class: 'metrics' });
    const tableWrap = el('div', { class: 'table-wrap' });
    const note = el('p', { class: 'hint' },
      '数据来自 llm_usage 表，按调用完成时记录。花费按配置里的每百万 token 单价折算。');

    const rangeBtns = ranges.map(([d, label]) =>
      el('button', {
        class: 'btn btn-sm' + (d === days ? ' btn-outline' : ' btn-ghost'),
        type: 'button',
        onclick: () => { days = d; state.usageDays = d; syncRange(); load(); },
        text: label,
      }));

    const section = el('div', { class: 'section' },
      el('div', { class: 'section-head' },
        el('h3', { text: '模型调用' }),
        el('span', { class: 'spacer' }),
        el('div', { class: 'row' }, ...rangeBtns),
      ),
      metrics,
      el('div', { class: 'subsection-head' }, el('h4', { text: '按任务' })),
      tableWrap,
      note,
    );

    function syncRange() {
      rangeBtns.forEach((b, i) => {
        b.className = 'btn btn-sm ' + (ranges[i][0] === days ? 'btn-outline' : 'btn-ghost');
      });
    }

    async function load() {
      clear(metrics).append(...Array.from({ length: 5 }, () =>
        el('div', { class: 'metric' }, el('div', { class: 'skeleton', style: 'height:34px' }))));

      let data;
      try {
        data = await api('/api/usage?days=' + days);
      } catch (err) {
        clear(metrics);
        clear(tableWrap).append(errorState(err, load));
        return;
      }

      const s = data.summary || {};
      clear(metrics);

      const metric = (k, v, unit) => el('div', { class: 'metric' },
        el('div', { class: 'metric-k', text: k }),
        el('div', { class: 'metric-v' }, String(v), unit ? el('small', { text: unit }) : null),
      );

      const avg = s.calls ? Math.round(s.total_tokens / s.calls) : 0;
      metrics.append(
        metric('调用次数', fmtNum(s.calls)),
        metric('总 Token', fmtNum(s.total_tokens)),
        metric('平均 Token', fmtNum(avg), '/ 次'),
        metric('输入 / 输出', fmtNum(s.prompt_tokens) + ' / ' + fmtNum(s.completion_tokens)),
        metric('花费', fmtCost(s.cost)),
      );

      const byTask = data.by_task || [];
      clear(tableWrap);

      if (!byTask.length) {
        tableWrap.append(emptyState('这段时间没有调用记录。'));
        return;
      }

      const table = el('table', { class: 'table' },
        el('thead', {}, el('tr', {},
          el('th', { text: '任务' }),
          el('th', { class: 'num', text: '调用' }),
          el('th', { class: 'num', text: 'Token' }),
          el('th', { class: 'num', text: '花费' }),
        )),
        el('tbody', {}),
      );
      const tbody = $('tbody', table);

      for (const t of byTask) {
        tbody.append(el('tr', {},
          el('td', { text: t.task || '—' }),
          el('td', { class: 'num', text: fmtNum(t.calls) }),
          el('td', { class: 'num', text: fmtNum(t.total_tokens) }),
          el('td', { class: 'num', text: fmtCost(t.cost) }),
        ));
      }

      tableWrap.append(table);
    }

    load();
    return section;
  }

  /* ===================== 配置 ===================== */
  /* ===================== 模型管理 =====================
   *
   * 结构照抄参考实现的两个页签（refs/MaiBot/dashboard/src/routes/config/model）：
   *
   *   模型设置 = 服务商侧栏（左）+ 选中服务商的模型列表（右）
   *   功能分配 = 任务列表（左）+ 选中任务的配置卡（右）
   *
   * 只借「左右分栏 + 左侧选中项高亮」这个骨架，视觉用我们自己的样式。
   *
   * 数据来自 /api/config/llm，保存走 /api/config/llm/save。
   * 一次只提交一个动作（provider.save / model.save / task.save / *.delete），
   * 不做整节覆盖 —— 整节覆盖会让「前端少发一个字段」等于「把它清空」，
   * 而这种丢失完全没有提示。
   *
   * 密钥永远不出现在响应里（后端只回 has_api_key）；
   * 编辑时密钥输入框留空 = 不修改原值。
   */

  // TASK_META 把任务名映射成中文标题与说明。
  // 与参考实现的 TASK_CONFIGS（model/constants.ts）对齐，去掉我们没有的任务。
  const TASK_META = {
    planner: { title: '规划模型', desc: '决定什么时候回复、调用什么工具' },
    replyer: { title: '回复模型', desc: '把决定好的内容说成人话' },
    utils: { title: '组件模型', desc: '表情包、画像等辅助任务' },
    learner: { title: '学习模型', desc: '表达方式与黑话学习' },
    vlm: { title: '图像识别', desc: '给图片和表情包打标' },
    embedding: { title: '嵌入模型', desc: '长期记忆的向量检索，配错了检索就不可用' },
  };

  // 嵌入模型单选：向量空间必须一致，多选会让检索结果无意义。
  const SINGLE_MODEL_TASKS = { embedding: true };

  function taskTitle(name) {
    return (TASK_META[name] || {}).title || name;
  }

  // ---------------------------------------------------------------------------
  // 模型探活
  //
  // 模型配置是唯一一类「填错了不报错、只表现为机器人不说话」的配置。
  // 以前只能在命令行跑 tools/livecheck 才能验证，而真正会填错的人
  // 恰恰是在网页上改配置的人。
  //
  // 两个入口刻意分开：
  //   - 测试服务商：调 GET /models，只验「地址通 + 鉴权过」。
  //     刚填完 BaseURL 与 key、还没加模型时用这个。
  //   - 测试模型：真的发一次生成请求。它能分辨「通了但模型名不对」
  //     与「模型是推理模型、不适合当回复器」。
  // ---------------------------------------------------------------------------

  // probeChip 是探活结果的一行展示。
  //
  // 用 inline 块而不是 toast：探活结果信息量大（耗时、模型名、错误原文、
  // 排查建议），toast 2 秒就消失，用户根本来不及读完。
  function probeChip(res) {
    const ok = !!res.ok;
    const box = el('div', { class: 'probe' + (ok ? ' ok' : ' bad') });

    const head = el('div', { class: 'probe-head' },
      el('span', { class: 'probe-badge', text: ok ? '通' : '失败' }),
      el('span', { class: 'probe-title', text: (res.model || res.provider || '') }),
      // 标出测的是哪条路径。向量模型与聊天模型走完全不同的端点，
      // 不标的话用户看到「向量模型测试通过」会以为它能当 Replyer。
      res.kind === 'embedding'
        ? el('span', { class: 'probe-tag', text: '向量' })
        : (res.kind === 'chat' ? el('span', { class: 'probe-tag', text: '对话' }) : null),
      res.dimension ? el('span', { class: 'probe-meta', text: res.dimension + ' 维' }) : null,
      res.latency_ms ? el('span', { class: 'probe-meta', text: res.latency_ms + ' ms' }) : null,
      res.http_status ? el('span', { class: 'probe-meta', text: 'HTTP ' + res.http_status }) : null,
      res.tokens ? el('span', { class: 'probe-meta', text: res.tokens + ' tokens' }) : null,
    );
    box.append(head);

    // 正文：只回一句「ok」，但只要它有内容就说明模型真的在生成。
    if (res.reply) {
      box.append(el('div', { class: 'probe-reply', text: '回复：' + res.reply }));
    }
    if (res.error) {
      box.append(el('div', { class: 'probe-error', text: res.error }));
    }
    // hint 单独一块：错误原文要保留（用户可能拿去搜），
    // 而「接下来改哪里」是另一件事，混在一起两边都读不清。
    if (res.hint) {
      box.append(el('div', { class: 'probe-hint', text: res.hint }));
    }
    // 服务商探活会带回模型列表 —— 这是填错模型名时最有用的东西：
    // 用户可以直接照着列表改，不用去服务商控制台翻。
    if (res.models && res.models.length) {
      box.append(el('div', { class: 'probe-models' },
        el('span', { class: 'hint', text: '服务端有 ' + res.models.length + ' 个模型：' }),
        el('span', { class: 'mono', text: res.models.slice(0, 12).join(', ')
          + (res.models.length > 12 ? ' …' : '') }),
      ));
    }
    return box;
  }

  // showProbe 把结果插到按钮下方。重复点会替换上一次的结果。
  function showProbe(btn, res) {
    const host = btn.closest('.mdl-card') || btn.closest('.mdl-main');
    if (!host) return;
    const old = host.querySelector('.probe');
    if (old) old.remove();
    const chip = probeChip(res);
    // 插到卡片头部之后，紧跟标题，不会被长列表推到屏幕外。
    const head = btn.closest('.mdl-card-head');
    if (head) head.insertAdjacentElement('afterend', chip);
    else host.append(chip);
  }

  // busy 期间禁用按钮并改文案，避免连点发出十几个请求。
  async function withBusy(btn, text, fn) {
    const label = btn.textContent;
    btn.disabled = true;
    btn.textContent = text;
    try {
      return await fn();
    } finally {
      btn.disabled = false;
      btn.textContent = label;
    }
  }

  async function probeModel(name, btn) {
    await withBusy(btn, '测试中…', async () => {
      try {
        const res = await api('/api/config/llm/probe', {
          method: 'POST',
          body: JSON.stringify({ model: name }),
        });
        // 后端返回的是裸对象（llm.ProbeResult 本身），没有 data 包裹。
        // 以前这里读 res.data —— 恒为 undefined，于是无论模型通不通
        // 界面都显示「失败：没有返回结果」。
        showProbe(btn, res || { ok: false, error: '没有返回结果' });
      } catch (err) {
        showProbe(btn, { ok: false, error: err.message, hint: '接口调用失败，检查 WebUI 是否还在运行' });
      }
    });
  }

  async function probeProvider(name, btn) {
    await withBusy(btn, '测试中…', async () => {
      try {
        const res = await api('/api/config/llm/probe-provider', {
          method: 'POST',
          body: JSON.stringify({ provider: name }),
        });
        showProbe(btn, res || { ok: false, error: '没有返回结果' });
      } catch (err) {
        showProbe(btn, { ok: false, error: err.message, hint: '接口调用失败，检查 WebUI 是否还在运行' });
      }
    });
  }

  async function viewModels(host) {
    host.append(el('div', { class: 'page' }, skeleton(3, 140)));

    let data;
    try {
      data = await api('/api/config/llm');
    } catch (err) {
      clear(host).append(errorState(err, () => navigate()));
      return;
    }

    const page = el('div', { class: 'page' });
    clear(host).append(page);

    const state = {
      data,
      provider: (data.providers[0] || {}).name || '',
      task: (data.tasks[0] || {}).name || '',
      tab: routeTab('models', parseHash().params),
    };

    const body = el('div', { class: 'mdl-body' });
    page.append(body);

    // send 是唯一的写入口：所有动作都过它，成功后用后端返回的完整数据重建。
    // 用后端返回的数据而不是本地改一份：Validate 可能补齐默认值，
    // 本地猜会和磁盘上的真实状态越差越远。
    async function send(payload, okText) {
      const res = await api('/api/config/llm/save', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      if (res.data) state.data = res.data;
      syncSelection();
      toast(okText || '已保存');
      render();
    }

    // syncSelection 在删除后修正选中项：
    // 选中的服务商被删掉时退回第一个，否则右侧会一直显示空白。
    function syncSelection() {
      const ps = state.data.providers || [];
      if (!ps.some((p) => p.name === state.provider)) {
        state.provider = (ps[0] || {}).name || '';
      }
    }

    function render() {
      clear(body);
      if (state.tab === 'tasks') renderTasks();
      else renderProviders();
    }

    /* ------------------ 页签 1：模型设置 ------------------ */

    function renderProviders() {
      const providers = state.data.providers || [];
      const models = state.data.models || [];
      const current = providers.find((p) => p.name === state.provider);
      const shown = current ? models.filter((m) => m.provider === current.name) : [];

      const aside = el('aside', { class: 'mdl-side' },
        el('div', { class: 'mdl-side-head' },
          el('h3', { text: '服务商' }),
          el('button', {
            class: 'btn btn-outline btn-sm', type: 'button',
            onclick: () => providerForm(null),
          }, '添加'),
        ),
      );

      const list = el('div', { class: 'mdl-side-list' });
      if (!providers.length) {
        list.append(el('p', { class: 'hint', text: '还没有服务商，点「添加」新建一个。' }));
      }
      for (const p of providers) {
        list.append(el('button', {
          class: 'mdl-side-item' + (p.name === state.provider ? ' on' : ''),
          type: 'button',
          onclick: () => { state.provider = p.name; render(); },
        },
          el('span', { class: 'mdl-side-name', text: p.name }),
          el('span', { class: 'mdl-side-meta' },
            el('span', { text: p.model_count + ' 个模型' }),
            p.has_api_key ? null : el('span', { class: 'tag tag-warn', text: '缺密钥' }),
          ),
        ));
      }
      aside.append(list);

      const main = el('section', { class: 'mdl-main' });

      if (!current) {
        main.append(el('p', { class: 'hint', text: '选择左侧的服务商查看详情。' }));
        body.append(aside, main);
        return;
      }

      const card = el('div', { class: 'mdl-card' },
        el('div', { class: 'mdl-card-head' },
          el('h3', { text: current.name }),
          el('div', { class: 'mdl-card-actions' },
            el('button', {
              class: 'btn btn-ghost btn-sm', type: 'button',
              onclick: (ev) => probeProvider(current.name, ev.currentTarget),
            }, '测试'),
            el('button', {
              class: 'btn btn-ghost btn-sm', type: 'button',
              onclick: () => providerForm(current),
            }, '编辑'),
            el('button', {
              class: 'btn btn-ghost btn-sm btn-danger', type: 'button',
              onclick: () => {
                const msg = '删除服务商「' + current.name + '」？\n它下面的 '
                  + current.model_count + ' 个模型也会一起删掉。';
                if (!window.confirm(msg)) return;
                send({ action: 'provider.delete', name: current.name }, '已删除')
                  .catch((e) => toast('删除失败：' + e.message, 'error'));
              },
            }, '删除'),
          ),
        ),
      );

      const infoRows = [
        ['Base URL', current.base_url],
        ['鉴权方式', current.auth_type || 'bearer'],
        ['API Key', current.has_api_key ? '已配置' : '未配置'],
        ['超时', current.timeout + ' 秒'],
        ['重试', current.max_retry + ' 次，间隔 ' + current.retry_interval + ' 秒'],
      ];
      for (const [k, v] of infoRows) {
        card.append(el('div', { class: 'cfg-row' },
          el('span', { class: 'k', text: k }),
          el('span', { class: 'v', text: String(v) }),
        ));
      }
      main.append(card);

      const mcard = el('div', { class: 'mdl-card' },
        el('div', { class: 'mdl-card-head' },
          el('h3', { text: '模型' }),
          el('button', {
            class: 'btn btn-outline btn-sm', type: 'button',
            onclick: () => modelForm(null, current.name),
          }, '添加模型'),
        ),
      );

      if (!shown.length) {
        mcard.append(el('p', { class: 'hint', text: '这个服务商下还没有模型。' }));
      } else {
        const table = el('div', { class: 'mdl-table' });
        table.append(el('div', { class: 'mdl-tr mdl-th' },
          el('span', { text: '名称' }),
          el('span', { text: '模型标识符' }),
          el('span', { text: '多模态' }),
          el('span', { text: '' }),
        ));
        for (const m of shown) {
          table.append(el('div', { class: 'mdl-tr' },
            el('span', { class: 'mono', text: m.name }),
            el('span', { text: m.model_identifier }),
            el('span', { text: m.visual ? '是' : '否' }),
            el('span', { class: 'mdl-row-actions' },
              el('button', {
                class: 'btn btn-ghost btn-sm', type: 'button',
                onclick: (ev) => probeModel(m.name, ev.currentTarget),
              }, '测试'),
              el('button', {
                class: 'btn btn-ghost btn-sm', type: 'button',
                onclick: () => modelForm(m, current.name),
              }, '编辑'),
              el('button', {
                class: 'btn btn-ghost btn-sm btn-danger', type: 'button',
                onclick: () => {
                  const msg = '删除模型「' + m.name + '」？\n引用它的任务会同时移除这一项。';
                  if (!window.confirm(msg)) return;
                  send({ action: 'model.delete', name: m.name }, '已删除')
                    .catch((e) => toast('删除失败：' + e.message, 'error'));
                },
              }, '删除'),
            ),
          ));
        }
        mcard.append(table);
      }
      main.append(mcard);

      body.append(aside, main);
    }

    /* ------------------ 页签 2：功能分配 ------------------ */

    function renderTasks() {
      const tasks = state.data.tasks || [];
      const models = state.data.models || [];
      const current = tasks.find((t) => t.name === state.task) || tasks[0];

      const aside = el('aside', { class: 'mdl-side' },
        el('div', { class: 'mdl-side-head' }, el('h3', { text: '功能任务' })),
      );

      const list = el('div', { class: 'mdl-side-list' });
      for (const t of tasks) {
        const configured = (t.models || []).length > 0;
        list.append(el('button', {
          class: 'mdl-side-item' + (current && t.name === current.name ? ' on' : ''),
          type: 'button',
          onclick: () => { state.task = t.name; render(); },
        },
          el('span', { class: 'mdl-side-name', text: taskTitle(t.name) }),
          el('span', { class: 'mdl-side-meta' },
            el('span', { text: configured ? t.models.join('、') : '未配置' }),
          ),
        ));
      }
      aside.append(list);

      const main = el('section', { class: 'mdl-main' });
      if (current) main.append(taskCard(current, models));
      body.append(aside, main);
    }

    // taskCard 造一个任务的配置卡。
    //
    // 字段与参考实现 TaskConfigCard 一一对应：
    // 模型（多选，嵌入模型单选）、选择策略、温度、最大 Token、时间上限。
    function taskCard(task, models) {
      const meta = TASK_META[task.name] || {};
      const single = !!SINGLE_MODEL_TASKS[task.name];

      const draft = {
        models: (task.models || []).slice(),
        selection_strategy: task.selection_strategy || 'balance',
        temperature: task.temperature,
        max_tokens: task.max_tokens,
        hard_timeout: task.hard_timeout,
      };

      const card = el('div', { class: 'mdl-card' },
        el('div', { class: 'mdl-card-head' },
          el('h3', { text: meta.title || task.name }),
          el('span', { class: 'hint', text: meta.desc || '' }),
        ),
      );

      const form = el('form', { class: 'cfg-form narrow', novalidate: 'novalidate' });
      const status = el('span', { class: 'cfg-status' });

      const chips = el('div', { class: 'mdl-chips' });
      const paintChips = () => {
        clear(chips);
        if (!models.length) {
          chips.append(el('span', { class: 'hint', text: '还没有模型，请先到「模型设置」添加。' }));
          return;
        }
        for (const m of models) {
          const on = draft.models.includes(m.name);
          chips.append(el('button', {
            class: 'chip' + (on ? ' on' : ''),
            type: 'button',
            onclick: () => {
              if (single) {
                draft.models = on ? [] : [m.name];
              } else if (on) {
                draft.models = draft.models.filter((n) => n !== m.name);
              } else {
                draft.models = draft.models.concat([m.name]);
              }
              paintChips();
            },
          }, m.name));
        }
      };
      paintChips();

      form.append(el('div', { class: 'cfg-row block' },
        el('span', { class: 'k', text: single ? '模型（单选）' : '模型（可多选）' }),
        chips,
      ));

      if (!single) {
        const sel = el('select', {
          class: 'input',
          onchange: (e) => { draft.selection_strategy = e.target.value; },
        });
        for (const v of ['balance', 'random', 'sequential']) {
          const o = el('option', { value: v, text: v });
          if (draft.selection_strategy === v) o.selected = true;
          sel.append(o);
        }
        form.append(el('div', { class: 'cfg-row' },
          el('span', { class: 'k', text: '选择策略' }), sel));
      }

      // numRow 造一个数字输入行。
      const numRow = (label, value, step, onInput) => {
        const input = el('input', {
          class: 'input', type: 'number', step: String(step), min: '1',
          oninput: (e) => onInput(e.target.value),
        });
        input.value = value == null ? '' : String(value);
        return el('div', { class: 'cfg-row' },
          el('span', { class: 'k', text: label }), input);
      };

      form.append(numRow('温度', draft.temperature, 0.1, (v) => { draft.temperature = v; }));
      form.append(numRow('最大 Token', draft.max_tokens, 1, (v) => { draft.max_tokens = v; }));
      form.append(numRow('时间上限（秒）', draft.hard_timeout, 1, (v) => { draft.hard_timeout = v; }));

      const saveBtn = el('button', { class: 'btn btn-primary btn-sm', type: 'submit', text: '保存' });
      form.append(el('div', { class: 'cfg-save' },
        status, el('span', { class: 'spacer' }), saveBtn));

      form.addEventListener('submit', (e) => {
        e.preventDefault();
        // 数字框留空时保持原值，不能当 0 提交：
        // 温度 0 是确定性输出、max_tokens 0 是「不限」，都不是用户的本意。
        const num = (raw, fallback) => {
          const s = String(raw == null ? '' : raw).trim();
          if (!s) return fallback;
          const n = Number(s);
          return Number.isFinite(n) ? n : fallback;
        };
        saveBtn.disabled = true;
        status.textContent = '保存中…';
        send({
          action: 'task.save',
          task: {
            name: task.name,
            models: draft.models,
            selection_strategy: draft.selection_strategy,
            temperature: num(draft.temperature, task.temperature),
            max_tokens: num(draft.max_tokens, task.max_tokens),
            hard_timeout: num(draft.hard_timeout, task.hard_timeout),
          },
        }).catch((err) => {
          status.textContent = '保存失败：' + err.message;
          saveBtn.disabled = false;
        });
      });

      card.append(form);
      return card;
    }

    /* ------------------ 服务商 / 模型的编辑表单 ------------------ */

    // providerForm 在内容区顶部就地展开编辑面板（不用弹窗）。
    //
    // 密钥输入框永远留空：后端把「空」理解为「不改」。
    // 这样密钥既不出现在响应里，也不会被浏览器记住。
    function providerForm(p) {
      const isNew = !p;
      const v = p || {
        name: '', base_url: '', auth_type: 'bearer',
        timeout: 120, max_retry: 2, retry_interval: 3, has_api_key: false,
      };

      const row = (label, input, help) => el('div', { class: 'cfg-row block' },
        el('label', { class: 'k' }, label,
          help ? el('span', { class: 'cfg-help', text: help }) : null),
        input,
      );

      const nameIn = el('input', { class: 'input' });
      nameIn.value = v.name;
      const urlIn = el('input', { class: 'input', placeholder: 'https://.../v1' });
      urlIn.value = v.base_url;
      const keyIn = el('input', {
        class: 'input', type: 'password', autocomplete: 'new-password',
        placeholder: v.has_api_key ? '已配置，留空表示不修改' : '填 API Key',
      });
      const authSel = el('select', { class: 'input' });
      for (const k of ['bearer', 'header', 'query', 'none']) {
        const o = el('option', { value: k, text: k });
        if ((v.auth_type || 'bearer') === k) o.selected = true;
        authSel.append(o);
      }
      const timeoutIn = el('input', { class: 'input', type: 'number', min: '1' });
      timeoutIn.value = String(v.timeout || 120);
      const retryIn = el('input', { class: 'input', type: 'number', min: '1' });
      retryIn.value = String(v.max_retry || 2);
      const intervalIn = el('input', { class: 'input', type: 'number', min: '1' });
      intervalIn.value = String(v.retry_interval || 3);

      const form = el('form', { class: 'cfg-form narrow', novalidate: 'novalidate' },
        row('名称', nameIn, '模型引用的是这个名字，改名会自动更新引用'),
        row('Base URL', urlIn, ''),
        row('API Key', keyIn, v.has_api_key ? '留空保持原密钥' : '必填'),
        row('鉴权方式', authSel, ''),
        row('超时（秒）', timeoutIn, ''),
        row('最大重试次数', retryIn, ''),
        row('重试间隔（秒）', intervalIn, ''),
      );

      let clearKey = false;
      if (v.has_api_key) {
        const cb = el('input', {
          type: 'checkbox', onchange: (e) => { clearKey = e.target.checked; },
        });
        form.append(el('label', { class: 'cfg-check' }, cb,
          el('span', { text: '清空已保存的密钥' })));
      }

      const status = el('span', { class: 'cfg-status' });
      const saveBtn = el('button', { class: 'btn btn-primary btn-sm', type: 'submit', text: '保存' });
      const cancelBtn = el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', text: '取消',
        onclick: () => { render(); },
      });
      form.append(el('div', { class: 'cfg-save' },
        status, el('span', { class: 'spacer' }), cancelBtn, saveBtn));

      form.addEventListener('submit', (e) => {
        e.preventDefault();
        saveBtn.disabled = true;
        status.textContent = '保存中…';
        const target = nameIn.value.trim() || state.provider;
        send({
          action: 'provider.save',
          provider: {
            original: isNew ? '' : v.name,
            name: nameIn.value.trim(),
            base_url: urlIn.value.trim(),
            auth_type: authSel.value,
            timeout: Number(timeoutIn.value) || 0,
            max_retry: Number(retryIn.value) || 0,
            retry_interval: Number(intervalIn.value) || 0,
            api_key: keyIn.value,
            clear_api_key: clearKey,
          },
        }).then(() => { state.provider = target; render(); })
          .catch((err) => {
            status.textContent = '保存失败：' + err.message;
            saveBtn.disabled = false;
          });
      });

      body.prepend(el('div', { class: 'mdl-card' },
        el('div', { class: 'mdl-card-head' },
          el('h3', { text: isNew ? '添加服务商' : '编辑服务商' })),
        form,
      ));
    }

    function modelForm(m, providerName) {
      const isNew = !m;
      const v = m || {
        name: '', model_identifier: '', provider: providerName,
        visual: false, force_stream: false,
        temperature: null, max_tokens: null, dimension: null,
      };

      const row = (label, input, help) => el('div', { class: 'cfg-row block' },
        el('label', { class: 'k' }, label,
          help ? el('span', { class: 'cfg-help', text: help }) : null),
        input,
      );

      const nameIn = el('input', { class: 'input' });
      nameIn.value = v.name;
      const identIn = el('input', { class: 'input', placeholder: '服务商侧的模型名' });
      identIn.value = v.model_identifier;
      // 可选覆盖项留空 = 沿用任务级默认。空与 0 是两个意思，不能都当 0。
      const tempIn = el('input', {
        class: 'input', type: 'number', step: '0.1', placeholder: '沿用任务默认',
      });
      tempIn.value = v.temperature == null ? '' : String(v.temperature);
      const maxTokIn = el('input', {
        class: 'input', type: 'number', min: '1', placeholder: '沿用任务默认',
      });
      maxTokIn.value = v.max_tokens == null ? '' : String(v.max_tokens);
      const dimIn = el('input', {
        class: 'input', type: 'number', min: '1', placeholder: '仅嵌入模型填',
      });
      dimIn.value = v.dimension == null ? '' : String(v.dimension);

      const visualCb = el('input', { type: 'checkbox' });
      visualCb.checked = !!v.visual;
      const streamCb = el('input', { type: 'checkbox' });
      streamCb.checked = !!v.force_stream;

      const form = el('form', { class: 'cfg-form narrow', novalidate: 'novalidate' },
        row('名称', nameIn, '任务里引用的是这个名字'),
        row('模型标识符', identIn, '填给服务商的那个字符串'),
        row('温度', tempIn, '留空沿用任务默认'),
        row('最大 Token', maxTokIn, '留空沿用任务默认'),
        row('向量维度', dimIn, '只有嵌入模型需要填'),
        el('label', { class: 'cfg-check' }, visualCb, el('span', { text: '多模态（能看图）' })),
        el('label', { class: 'cfg-check' }, streamCb, el('span', { text: '强制流式输出' })),
      );

      const status = el('span', { class: 'cfg-status' });
      const saveBtn = el('button', { class: 'btn btn-primary btn-sm', type: 'submit', text: '保存' });
      const cancelBtn = el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', text: '取消',
        onclick: () => { render(); },
      });
      form.append(el('div', { class: 'cfg-save' },
        status, el('span', { class: 'spacer' }), cancelBtn, saveBtn));

      form.addEventListener('submit', (e) => {
        e.preventDefault();
        const numOrNull = (input) => {
          const raw = input.value.trim();
          if (!raw) return null;
          const n = Number(raw);
          return Number.isFinite(n) ? n : null;
        };
        saveBtn.disabled = true;
        status.textContent = '保存中…';
        send({
          action: 'model.save',
          original: isNew ? '' : v.name,
          model: {
            name: nameIn.value.trim(),
            model_identifier: identIn.value.trim(),
            provider: v.provider,
            visual: visualCb.checked,
            force_stream: streamCb.checked,
            temperature: numOrNull(tempIn),
            max_tokens: numOrNull(maxTokIn),
            dimension: numOrNull(dimIn),
          },
        }).catch((err) => {
          status.textContent = '保存失败：' + err.message;
          saveBtn.disabled = false;
        });
      });

      body.prepend(el('div', { class: 'mdl-card' },
        el('div', { class: 'mdl-card-head' },
          el('h3', { text: isNew ? '添加模型' : '编辑模型' })),
        form,
      ));
    }

    render();
  }


  // keywordPanel 画「关键词反应规则」表格编辑器。
  //
  // 对应参考实现「麦麦设置 → 关键词」那张表：关键词 / 正则 / 反应，逐行增删。
  //
  // 提交语义是**整表替换**（与 llm 的 action 式不同）：
  // 用户在页面上看到的就是全部规则，表格编辑器天然是「提交整张表」。
  // 后端 ApplyKeywordRules 会逐行校验并在错误里指到第几条。
  function keywordPanel(host) {
    // 内嵌到配置页的「关键词」一节里，所以自己不管页面骨架，
    // 只往传进来的容器里画。先放占位，拿到数据后替换。
    const box = el('div', { class: 'cfg-group' });
    host.append(box);
    box.append(skeleton(2, 96));

    api('/api/config/keywords').then((data) => {
      clear(box);
      paintKeywordPanel(box, data);
    }).catch((err) => {
      clear(box).append(errorState(err, () => navigate()));
    });
  }

  function paintKeywordPanel(page, data) {
    const maxRules = data.max_rules || 30;
    // draft 是「当前编辑中的规则」。
    // 字符串数组在编辑期间存成「一行一条的字符串」，提交时才拆 ——
    // 直接存数组的话，用户敲到一半的多行文本会丢换行。
    const draft = (data.rules || []).map((r) => ({
      keywords: (r.keywords || []).join('\n'),
      regex: (r.regex || []).join('\n'),
      reaction: r.reaction || '',
    }));
    const initial = JSON.stringify(draft);

    const listBox = el('div', { class: 'kw-list' });
    const status = el('span', { class: 'cfg-status' });

    const addBtn = el('button', {
      class: 'btn btn-outline btn-sm', type: 'button', text: '添加一条',
      onclick: () => {
        if (draft.length >= maxRules) return;
        draft.push({ keywords: '', regex: '', reaction: '' });
        paint();
      },
    });

    const saveBtn = el('button', {
      class: 'btn btn-primary btn-sm', type: 'submit', text: '保存规则',
    });

    function sync() {
      const dirty = JSON.stringify(draft) !== initial;
      saveBtn.disabled = !dirty;
      addBtn.disabled = draft.length >= maxRules;
      status.textContent = dirty ? '有未保存的改动' : '';
    }

    // paint 重建整张表。
    // 行数不多（上限 30），整表重画比逐行做增量更新简单且不会漏。
    function paint() {
      clear(listBox);

      if (!draft.length) {
        listBox.append(emptyState(
          '还没有关键词规则。命中规则后不会直接发消息，而是给回复模型追加一句提示。'));
        sync();
        return;
      }

      draft.forEach((rule, i) => {
        const kw = el('textarea', {
          class: 'input cfg-textarea mono',
          rows: '2',
          placeholder: '一行一个',
          oninput: (e) => { rule.keywords = e.target.value; sync(); },
        });
        kw.value = rule.keywords;

        const re = el('textarea', {
          class: 'input cfg-textarea mono',
          rows: '2',
          placeholder: '一行一个，留空则只用关键词',
          oninput: (e) => { rule.regex = e.target.value; sync(); },
        });
        re.value = rule.regex;

        const reaction = el('textarea', {
          class: 'input cfg-textarea',
          rows: '2',
          placeholder: '命中后追加给回复模型的提示',
          oninput: (e) => { rule.reaction = e.target.value; sync(); },
        });
        reaction.value = rule.reaction;

        listBox.append(el('div', { class: 'kw-row' },
          el('div', { class: 'kw-cell' },
            el('span', { text: '关键词 ' + (i + 1) }), kw),
          el('div', { class: 'kw-cell' },
            el('span', { text: '正则（可选）' }), re),
          el('div', { class: 'kw-cell kw-full' },
            el('span', { text: '命中后的反应' }), reaction),
          el('div', { class: 'kw-full row' },
            el('button', {
              class: 'btn btn-ghost btn-sm btn-danger', type: 'button', text: '删除这条',
              onclick: () => {
                draft.splice(i, 1);
                paint();
              },
            })),
        ));
      });

      sync();
    }

    const form = el('form', { class: 'cfg-form', novalidate: 'novalidate' });
    form.append(
      el('div', { class: 'cfg-group' },
        el('h3', { text: '关键词反应' }),
        el('p', { class: 'cfg-help',
          text: '命中任一个关键词或正则时，把「反应」追加给回复模型作为额外提示。'
          + '它不直接发送，只是让模型知道该往哪个方向说。' }),
        el('p', { class: 'cfg-help',
          text: '生效是即时的：每次判断都重新读当前配置，改完下一轮就是新规则。' }),
        listBox,
        el('div', { class: 'kw-foot' }, addBtn,
          el('span', { class: 'hint', text: `最多 ${maxRules} 条，当前 ${draft.length} 条` })),
      ),
      el('div', { class: 'cfg-save' }, status, el('span', { class: 'spacer' }), saveBtn),
    );

    form.addEventListener('submit', async (e) => {
      e.preventDefault();

      // 把「一行一条」的文本拆回数组，顺便丢掉空行。
      // 空行在这里丢掉，后端也会再 trim 一次 —— 两道都留是因为
      // 空串在关键词里会匹配「所有消息」，那是灾难性的（见 convert.go）。
      const rules = draft.map((r) => ({
        keywords: r.keywords.split('\n').map((s) => s.trim()).filter(Boolean),
        regex: r.regex.split('\n').map((s) => s.trim()).filter(Boolean),
        reaction: r.reaction.trim(),
      }));

      saveBtn.disabled = true;
      status.textContent = '保存中…';
      try {
        await api('/api/config/keywords/save', {
          method: 'POST',
          body: JSON.stringify({ rules }),
        });
        toast('关键词规则已保存');
        navigate();
      } catch (err) {
        status.textContent = '保存失败：' + err.message;
        toast('保存失败：' + err.message, 'error');
        saveBtn.disabled = false;
      }
    });

    page.append(form);
    paint();
  }

  // qqCredCard 画「QQ 机器人凭据」卡片。
  //
  // 为什么单独渲染而不进平铺字段清单：
  // AppSecret 是密钥，而平铺清单的 Snapshot() 会把每个字段的值一起
  // 返回给前端 —— 密钥一旦进那个机制，就只能是「靠记得把它排除」
  // 这种容易破的约束。单独一张卡可以保证：返回的 QQView 结构体里
  // 压根没有 AppSecret 字段，想泄露也泄露不了。
  //
  // 规则跟 LLM 密钥一致：输入框留空 = 保留原值，
  // 想清空得点「清空密钥」。不回显密钥就必然要区分「空」与「不改」。
  function qqCredCard() {
    const card = el('div', { class: 'cfg-group' },
      el('h3', {},
        el('span', { text: 'QQ 机器人凭据' }),
        el('span', { class: 'tag tag-off cfg-restart', text: '需重启' }),
      ),
      el('p', { class: 'cfg-help',
        text: '在 QQ 开放平台创建机器人后可以拿到。保存后需重启进程才生效。' }),
    );

    const body = el('div', {});
    card.append(body);

    let draft = { app_id: '', app_secret: '', unified_account_id: '' };
    let clearSecret = false;

    // 类名带 qq- 前缀：页面上已经有主表单的 .cfg-status，
    // 同名会让 document.querySelector('.cfg-status') 命中这里，
    // 于是主表单的错误提示永远读不到（也读不到正确的那条）。
    const status = el('span', { class: 'cfg-status qq-status' });
    const saveBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '保存凭据' });

    function renderQQ() {
      clear(body);

      const appID = el('input', {
        class: 'input', type: 'text',
        placeholder: 'AppID（纯数字）',
        oninput: (e) => { draft.app_id = e.target.value; sync(); },
      });
      appID.value = draft.app_id;
      appID.dataset.key = 'qq.app_id';

      const secret = el('input', {
        class: 'input', type: 'password',
        placeholder: info.secret_set
          ? '已设置（留空则不修改）'
          : '未设置，请填写',
        oninput: (e) => { draft.app_secret = e.target.value; sync(); },
      });
      secret.value = draft.app_secret;
      secret.dataset.key = 'qq.app_secret';
      // 浏览器的密码管理器会把这个框当成登录框，自动填成站点的密码。
      // 关了它，否则用户会看到一串自己没填过的字符。
      secret.autocomplete = 'new-password';

      const unified = el('input', {
        class: 'input', type: 'text',
        placeholder: '留空则按群/私聊分别归属',
        oninput: (e) => { draft.unified_account_id = e.target.value; sync(); },
      });
      unified.value = draft.unified_account_id;
      unified.dataset.key = 'qq.unified_account_id';

      body.append(
        fieldRow('AppID', appID, '开放平台的应用 AppID，纯数字'),
        fieldRow('AppSecret', secret,
          clearSecret ? '保存后将清空' : '留空表示不修改已保存的密钥'),
        el('div', { class: 'cfg-row block' },
          el('span', { class: 'k', text: ' ' }),
          el('label', { class: 'cfg-check' },
            el('input', {
              type: 'checkbox',
              onchange: (e) => { clearSecret = e.target.checked; renderQQ(); },
            }),
            el('span', { text: '清空已保存的密钥' }),
          ),
        ),
        fieldRow('统一账号 ID', unified, '多群共用一个账号身份时填写，一般不填'),
      );

      // 环境变量覆盖提示：不说的话会得到一个几乎无法自查的现象 ——
      // 网页上改了、提示保存成功、重启后还是连不上旧账号。
      if (info.env_app_id || info.env_app_secret) {
        const who = [];
        if (info.env_app_id) who.push('AppID');
        if (info.env_app_secret) who.push('AppSecret');
        body.append(el('p', { class: 'cfg-help warn',
          text: who.join(' 与 ') + ' 当前由环境变量（OMQ_QQ_APP_ID / OMQ_QQ_APP_SECRET）覆盖，' +
            '在这里改不会生效；要改请改环境变量或去掉它。' }));
      }

      // 当前状态：没凭据时明确告诉用户「填完要重启」。
      if (!info.ready) {
        body.append(el('p', { class: 'cfg-help warn',
          text: '缺少 ' + (info.missing || []).join('、') + '，机器人连不上 QQ。' }));
      }

      // 类名不用 .cfg-save：那是页底主表单的保存栏，而这里是卡片内的
      // 次级保存。用同名会让两张卡的按钮互相混在一起（两个「保存」）
      // 既困住用户（不知道点哪个），也困住测试（按 .cfg-save 取到的
      // 可能是被禁用的那个）。
      body.append(el('div', { class: 'cfg-actions' }, saveBtn, status));
      sync();
    }

    function sync() {
      // 没改 AppID 也没填新密钥就不允许保存：否则每点一次
      // 都在无意义地重写文件。
      const changed = draft.app_id !== (info.app_id || '') ||
        draft.app_secret !== '' ||
        draft.unified_account_id !== (info.unified_account_id || '') ||
        clearSecret;
      saveBtn.disabled = !changed;
    }

    saveBtn.addEventListener('click', async () => {
      saveBtn.disabled = true;
      status.textContent = '保存中…';
      try {
        const res = await api('/api/config/qq/save', {
          method: 'POST',
          body: JSON.stringify({
            app_id: draft.app_id.trim(),
            app_secret: clearSecret ? '' : draft.app_secret.trim(),
            clear_app_secret: clearSecret,
            unified_account_id: draft.unified_account_id.trim(),
          }),
        });
        info = res.data || info;
        draft.app_secret = '';
        clearSecret = false;
        status.textContent = '';
        toast('凭据已保存，重启后生效');
        renderQQ();
      } catch (err) {
        status.textContent = '保存失败：' + err.message;
        saveBtn.disabled = false;
      }
    });

    let info = {};
    api('/api/config/qq').then((d) => {
      info = d || {};
      draft = {
        app_id: info.app_id || '',
        app_secret: '',
        unified_account_id: info.unified_account_id || '',
      };
      renderQQ();
    }).catch((err) => {
      clear(body).append(el('p', { class: 'cfg-help warn',
        text: '读取凭据状态失败：' + err.message }));
    });

    return card;
  }

  // fieldRow 是「标签 + 控件 + 说明」的一行，供上面两张手写卡片复用。
  function fieldRow(label, control, help) {
    const row = el('div', { class: 'cfg-row block' },
      el('span', { class: 'k', text: label }),
      control,
    );
    if (help) row.append(el('p', { class: 'cfg-help', text: help }));
    return row;
  }

  // 配置页是可编辑表单，不是只读视图。
  //
  // 表单本身不写死：字段清单、类型、取值范围、是否需要重启，
  // 全部由后端 /api/config/edit 返回（源自 config.EditableFields）。
  // 加一个配置项只要在 Go 里加一行，界面上自动出现 ——
  // 前端硬编码表单的话，两边迟早对不上，而那种「界面上少了一项」
  // 的缺失几乎不可能被发现。
  //
  // 提交方式是「只发改动的字段」的补丁，不是整份回传：
  // 整份回传时，某个字段这次没渲染就会把它清空。
  //
  // ===================== 配置页布局 =====================
  //
  // 结构：左侧竖排二级导航 + 右侧单节编辑区。
  //
  // 为什么从「一个 50 字段的长表单」改成这个形态：
  // 原来的页面要滚 2000+ 像素，分区只靠 <h3> 撑着，保存栏是全局的 ——
  // 改一个阈值要在几十个框里找它，保存时还不知道自己改了几处。
  // 现在每节最多 6 个字段，各带独立保存栏，改完就地存。
  //
  // 三个必需的设计约束：
  //  1. 保存栏必须 append 到 form 里（AGENTS.md 第 140 条）——
  //     type=submit 的按钮不在 form 内就不触发提交，表现为「点了没反应」。
  //  2. 每节独立提交：后端 /api/config/save 收的是「只含改动键」的补丁，
  //     天然支持按节提交，不需要后端做任何事。
  //  3. 切换节不能丢掉未保存的改动：draft 放在整个页面的闭包里，
  //     切节只重画右侧（不 navigate）—— navigate 会从服务端重读，
  //     把其它节里还没保存的编辑静默抹掉。

  // CONFIG_SECTIONS 把后端给的 section 中文名映射成 URL 里用的 slug。
  //
  // 为什么要映射而不是直接用中文：URL 里带中文要 encode，分享出去
  // 经常被聊天软件转义坏掉。映射表还顺带固定了导航顺序 ——
  // 后端返回的顺序是 struct 声明顺序，那不一定是最合理的阅读顺序。
  const CONFIG_SECTIONS = [
    { slug: 'connection', title: '连接', help: 'QQ 机器人凭据' },
    { slug: 'identity', section: '身份', title: '身份', help: '昵称与别名' },
    { slug: 'personality', section: '人格', title: '人格', help: '身份设定与说话风格' },
    { slug: 'chat', section: '聊天', title: '聊天', help: '上下文、防抖、发言频率' },
    { slug: 'quote', section: '引用', title: '引用', help: '什么时候引用别人的消息' },
    { slug: 'response', section: '回复', title: '回复', help: '拆分、长度、打字速度' },
    { slug: 'filter', section: '消息过滤', title: '消息过滤', help: '屏蔽词与白名单' },
    { slug: 'memory', section: '长期记忆', title: '长期记忆', help: '回想摘要与人物画像' },
    { slug: 'emoji', section: '表情包', title: '表情包', help: '收藏与发送' },
    { slug: 'learning', section: '学习', title: '学习', help: '表达与黑话' },
    { slug: 'keywords', title: '关键词', help: '命中后追加提示' },
    { slug: 'command', section: '命令', title: '命令', help: '绑定命令的管理员' },
    { slug: 'runtime', section: '运行', title: '运行', help: '日志、保留期、端口' },
    { slug: 'info', title: '信息', help: '只读信息与文件路径' },
  ];

  async function viewConfig(host, actions) {
    host.append(el('div', { class: 'page' }, skeleton(3, 120)));

    let data;
    try {
      data = await api('/api/config/edit');
    } catch (err) {
      clear(host).append(errorState(err, () => navigate()));
      return;
    }

    const page = el('div', { class: 'page' });
    clear(host).append(page);

    const allFields = data.fields || [];
    const values = data.values || {};
    const ro = data.readonly || {};

    // 按后端给的 section 分组，保持声明顺序（不重排，重排规则只写在
    // CONFIG_SECTIONS 里，两处都排会互相打架）。
    const bySection = new Map();
    for (const f of allFields) {
      if (!bySection.has(f.section)) bySection.set(f.section, []);
      bySection.get(f.section).push(f);
    }

    // 组装导航项。后端出现了映射表里没有的 section 时，兜底生成一项 ——
    // 绝不能因为「忘了更新映射表」就让那批字段在界面上消失。
    const known = new Set(CONFIG_SECTIONS.map((s) => s.section).filter(Boolean));
    const navItems = CONFIG_SECTIONS.map((s) => ({ ...s }));
    for (const secName of bySection.keys()) {
      if (!known.has(secName)) {
        navItems.push({ slug: 'sec-' + navItems.length, section: secName, title: secName });
      }
    }

    // draft/initial 覆盖所有字段（不只当前节），这样切节不丢改动。
    const draft = new Map();
    for (const f of allFields) draft.set(f.key, values[f.key]);
    const initial = new Map(draft);

    // sectionDirty 判断某一节有没有未保存的改动，用于导航上的圆点。
    const sectionDirty = (item) => {
      if (!item.section) return false;
      for (const f of bySection.get(item.section) || []) {
        if (!sameValue(draft.get(f.key), initial.get(f.key))) return true;
      }
      return false;
    };

    /* --- 重启提示（整页一条，不按节拆） --- */
    if (data.restart && data.restart.length) {
      page.append(notice('以下项需要重启进程才生效：' + data.restart.join('、')));
    }

    /* --- 骨架：左导航 + 右内容 --- */
    const navBox = el('nav', { class: 'subnav', 'aria-label': '配置分区' });
    const bodyBox = el('div', { class: 'cfg-body' });
    page.append(el('div', { class: 'cfg-layout' }, navBox, bodyBox));

    // 当前节。URL 里 ?section= 优先，其次第一个。
    let active = parseHash().params.get('section') || '';
    if (!navItems.some((s) => s.slug === active)) active = navItems[0].slug;

    function paintNav() {
      clear(navBox);
      for (const item of navItems) {
        navBox.append(el('button', {
          class: 'subnav-item' + (item.slug === active ? ' on' : ''),
          type: 'button',
          'aria-current': item.slug === active ? 'page' : null,
          dataset: { section: item.slug },
          onclick: () => select(item.slug),
          // dirty 圆点放在右侧：一眼能看出「哪一节还没保存」，
          // 这是长表单改成按节编辑后唯一的多节状态线索。
        },
          el('span', { class: 'subnav-name', text: item.title }),
          sectionDirty(item) ? el('span', { class: 'subnav-dot', 'aria-label': '未保存' }) : null,
        ));
      }
    }

    // select 切节。
    //
    // 用 history.replaceState 而不是 location.hash：
    // 后者会触发 hashchange → navigate() → 从服务端重读配置，
    // 于是其它节里没保存的改动会被静默抹掉。
    function select(slug) {
      active = slug;
      history.replaceState(null, '', '#/config?section=' + encodeURIComponent(slug));
      paintNav();
      paintBody();
    }

    function paintBody() {
      clear(bodyBox);
      const item = navItems.find((s) => s.slug === active) || navItems[0];

      if (active === 'connection') {
        bodyBox.append(qqCredCard());
        return;
      }
      if (active === 'keywords') {
        keywordPanel(bodyBox);
        return;
      }
      if (active === 'info') {
        bodyBox.append(readonlyPanel(data, ro));
        return;
      }

      const fields = bySection.get(item.section) || [];
      bodyBox.append(buildSectionForm(item, fields));

      // 重启卡只出现在「运行」一节：日志级别、保留期、端口都在这里，
      // 而它们恰恰是改完必须重启的那批。放在别处用户会以为一保存就生效。
      if (active === 'runtime') bodyBox.append(restartCard());
    }

    // restartCard 是「重启程序」入口。
    //
    // 存在的理由：日志级别、保留期、端口改完都标了「需重启」，
    // 但以前只能自己去任务管理器结束进程再启动 —— 而这要求用户
    // 知道可执行文件在哪、当时带的是哪个 -config 参数。
    function restartCard() {
      const status = el('span', { class: 'cfg-status' });
      const btn = el('button', {
        class: 'btn btn-danger', type: 'button', text: '立即重启',
      });

      btn.addEventListener('click', async () => {
        const msg = '重启会让机器人短暂掉线（约 3~5 秒），期间收到的消息可能丢失。\n确定重启？';
        if (!window.confirm(msg)) return;

        status.textContent = '正在重启…';
        await withBusy(btn, '重启中…', async () => {
          try {
            const res = await api('/api/restart', {
              method: 'POST',
              body: JSON.stringify({ confirm: 'restart' }),
            });
            // 同样是裸对象，不是 res.data。
            const delay = (res && res.delay_ms) || 1200;
            status.textContent = '已发出重启指令，等待新进程…';
            toast('正在重启，页面会自动恢复');

            // 轮询直到新进程响应。
            //
            // 不能只 sleep 一下就刷新：新进程起得快慢取决于机器，
            // 而且旧进程在退出期间会拒绝连接 —— 直接 reload 大概率
            // 看到「无法访问此网站」，用户会以为重启把程序弄挂了。
            const deadline = Date.now() + 60000;
            const waitBack = async () => {
              await new Promise((r) => setTimeout(r, Math.max(500, delay)));
              while (Date.now() < deadline) {
                try {
                  await api('/api/status');
                  status.textContent = '已恢复，正在刷新页面…';
                  location.reload();
                  return;
                } catch (_) {
                  // 还没起来，继续等。这是预期内的失败，不该提示用户。
                  await new Promise((r) => setTimeout(r, 800));
                }
              }
              status.textContent = '等了 60 秒仍未恢复。请检查进程是否启动失败（看终端日志），或手动启动。';
              toast('重启后 60 秒内没有恢复', 'error');
            };
            waitBack();
          } catch (err) {
            status.textContent = '重启失败：' + err.message;
            toast('重启失败：' + err.message, 'error');
          }
        });
      });

      return el('div', { class: 'cfg-group' },
        el('div', { class: 'cfg-sec-head' },
          el('h3', { text: '重启程序' }),
          el('p', { class: 'hint', text:
            '端口、日志级别、数据保留期改完需要重启才生效。' +
            'QQ 凭据与模型配置不需要。' }),
        ),
        el('div', { class: 'cfg-actions' },
          btn,
          status,
        ),
      );
    }

    // buildSectionForm 造「一个分区的表单 + 它自己的保存栏」。
    //
    // 每次切节都重建：字段最多 6 个，重建比做增量更新简单，
    // 而且能保证控件显示的永远是 draft 里的最新值。
    function buildSectionForm(item, fields) {
      const form = el('form', {
        class: 'cfg-form', autocomplete: 'off', novalidate: 'novalidate',
      });

      const head = el('div', { class: 'cfg-sec-head' },
        el('h3', { text: item.title }),
        item.help ? el('p', { class: 'hint', text: item.help }) : null,
      );
      form.append(head);

      const statusText = el('span', { class: 'cfg-status' });
      const saveBtn = el('button', {
        class: 'btn btn-primary', type: 'submit', text: '保存本节',
      });
      const resetBtn = el('button', {
        class: 'btn btn-ghost', type: 'button', text: '放弃改动',
      });

      const isDirty = () => fields.some(
        (f) => !sameValue(draft.get(f.key), initial.get(f.key)));

      function sync() {
        const d = isDirty();
        saveBtn.disabled = !d;
        resetBtn.disabled = !d;
        statusText.textContent = d ? '有未保存的改动' : '';
        paintNav();
      }

      for (const f of fields) {
        form.append(renderField(f, draft, sync));
      }

      resetBtn.addEventListener('click', () => {
        for (const f of fields) draft.set(f.key, initial.get(f.key));
        paintBody();
      });

      form.addEventListener('submit', async (e) => {
        e.preventDefault();

        let patch;
        try {
          patch = collectPatch(fields, draft, initial);
        } catch (err) {
          // collectPatch 会为「数字框填了非数字」抛错。它必须在 try 里：
          // 否则异常冒到控制台，界面上什么都不显示，用户只看到「点了没反应」。
          statusText.textContent = '保存失败：' + err.message;
          toast('保存失败：' + err.message, 'error');
          return;
        }
        if (!Object.keys(patch).length) {
          statusText.textContent = '没有改动';
          return;
        }

        saveBtn.disabled = true;
        resetBtn.disabled = true;
        statusText.textContent = '保存中…';
        try {
          const res = await api('/api/config/save', {
            method: 'POST',
            body: JSON.stringify({ values: patch }),
          });
          // 用后端返回的值刷新基线：Validate 会补齐缺省值，
          // 前端保留自己的旧值会显示成「没生效」。
          const fresh = res.values || {};
          for (const f of allFields) {
            if (!(f.key in fresh)) continue;
            initial.set(f.key, fresh[f.key]);
            draft.set(f.key, fresh[f.key]);
          }
          if (res.warning) {
            toast('已保存，但' + res.warning);
          } else if (res.restart && res.restart.length) {
            toast('已保存，' + res.restart.join('、') + ' 需重启生效');
          } else {
            toast('已保存');
          }
          // 只重画当前节，不 navigate：其它节里可能有没保存的改动，
          // 整页重读会把它们静默抹掉。
          paintBody();
        } catch (err) {
          // 页内提示 + toast 都写。
          // 只写 statusText 的话，若这一次触发重绘（切节 / 重试），
          // 用户就什么都看不到了。toast 挂在 body 上不受重绘影响。
          statusText.textContent = '保存失败：' + err.message;
          toast('保存失败：' + err.message, 'error');
          saveBtn.disabled = false;
          resetBtn.disabled = false;
        }
      });

      // 保存栏必须在 form 内（AGENTS.md 第 140 条）：
      // 不在 form 里的 type=submit 按钮点了不触发提交，表现是「点了没反应」。
      form.append(el('div', { class: 'cfg-save' },
        statusText, el('span', { class: 'spacer' }), resetBtn, saveBtn));

      // 初始状态同步一次（控件值来自 draft，dirty 通常为 false）。
      queueMicrotask(() => {
        const d = isDirty();
        saveBtn.disabled = !d;
        resetBtn.disabled = !d;
      });

      return form;
    }

    paintNav();
    paintBody();

    actions.append(el('button', {
      class: 'btn btn-outline btn-sm',
      onclick: () => navigate(),
    }, icon('refresh'), '刷新'));
  }

  // readonlyPanel 画「只读信息」：排查问题用，全部不可改。
  //
  // 单独一节而不是塞在表单末尾：这些值有的根本不在可编辑清单里
  // （数据库路径、日志目录），混在可编辑项里会让人以为能点。
  function readonlyPanel(data, ro) {
    const box = el('div', { class: 'cfg-group' });
    box.append(el('div', { class: 'cfg-sec-head' },
      el('h3', { text: '只读信息' }),
      el('p', { class: 'hint', text: '这些值不能在线编辑。日志级别、保留期、端口在「运行」里改。' }),
    ));

    const rows = [
      ['配置文件', data.path || '（未绑定）'],
      ['数据库', ro.database_path || '—'],
      ['日志目录', ro.log_dir || '—'],
      ['WebUI 监听', (ro.web_host || '') + ':' + String(ro.web_port || '')],
      ['QQ 凭据', ro.qq_credentials_ready ? '已配置' : '缺失（机器人连不上 QQ）'],
      ['模型', `${fmtNum(ro.llm_provider_count)} 个服务商 / ${fmtNum(ro.llm_model_count)} 个模型`],
      ['关键词规则', fmtNum(ro.keyword_rule_count) + ' 条'],
    ];
    for (const [k, v] of rows) {
      box.append(el('div', { class: 'cfg-row' },
        el('span', { class: 'k', text: k }),
        el('span', { class: 'v mono', text: String(v) }),
      ));
    }

    box.append(el('p', { class: 'cfg-help',
      text: 'WebUI 登录口令（web.password）不在表单里，请直接编辑 config.toml ' +
        '或设置环境变量后重启。其余项（QQ 凭据、模型、提示词）都可以在本界面改。' }));

    return box;
  }

  // sameValue 比较两个表单值是否等价。
  // 数组要按内容比：=== 比的是引用，重新渲染后永远不等。
  function sameValue(a, b) {
    if (Array.isArray(a) && Array.isArray(b)) {
      return a.length === b.length && a.every((v, i) => v === b[i]);
    }
    return a === b;
  }

  // renderField 按 Field.kind 造一个控件，并把改动写回 draft。
  function renderField(f, draft, onChange) {
    const value = draft.get(f.key);

    const set = (v) => {
      draft.set(f.key, v);
      onChange();
    };

    let input;
    let control;

    switch (f.kind) {
      case 'bool': {
        input = el('input', {
          type: 'checkbox',
          onchange: (e) => set(e.target.checked),
        });
        input.checked = !!value;
        control = el('label', { class: 'cfg-check' }, input, el('span', { text: '启用' }));
        break;
      }
      case 'text': {
        input = el('textarea', {
          class: 'input cfg-textarea',
          rows: '4',
          oninput: (e) => set(e.target.value),
        });
        input.value = value == null ? '' : String(value);
        control = input;
        break;
      }
      case 'enum': {
        input = el('select', {
          class: 'input',
          onchange: (e) => set(e.target.value),
        });
        const opts = f.options || [];
        for (const opt of opts) {
          const o = el('option', { value: opt, text: opt });
          if (opt === value) o.selected = true;
          input.append(o);
        }
        // 配置里的值不在候选里（手改过 toml、或旧版本遗留值）：
        // 补一个标出来的选项，否则下拉框显示成空白、用户以为配置丢了。
        if (value != null && value !== '' && !opts.includes(String(value))) {
          input.append(el('option', { value: String(value), text: String(value) + '（当前值，不在候选里）' }));
        }
        control = input;
        break;
      }
      case 'int':
      case 'float': {
        input = el('input', {
          class: 'input',
          type: 'number',
          step: f.kind === 'int' ? '1' : 'any',
          oninput: (e) => set(e.target.value),
        });
        // 用 != null 而不是真值判断：min=0 是常见且合法的边界
        // （talk_value 的下限就是 0），用 if (f.min) 会把它当成「没设置」。
        if (f.min != null) input.min = String(f.min);
        if (f.max != null) input.max = String(f.max);
        input.value = value == null ? '' : String(value);
        control = input;
        break;
      }
      case 'string_list': {
        // 多行文本框，一行一项。比「一个标签一个输入框 + 加号按钮」
        // 代码少得多，也更适合「屏蔽词」这种批量粘贴的场景。
        input = el('textarea', {
          class: 'input cfg-textarea mono',
          rows: '3',
          placeholder: '一行一个',
          oninput: (e) => set(e.target.value.split('\n')),
        });
        // 值是 undefined 时也要得到空文本框：Array.isArray 比 || 更明确，
        // 避免把字符串误传给 string_list 字段时静默拆成单个字符。
        input.value = (Array.isArray(value) ? value : []).join('\n');
        control = input;
        break;
      }
      default: {
        input = el('input', {
          class: 'input',
          type: 'text',
          oninput: (e) => set(e.target.value),
        });
        input.value = value == null ? '' : String(value);
        control = input;
      }
    }

    // 给控件打上配置键：既方便排查「这个框是哪个配置」，
    // 也让测试能精确定位（按顺序取第一个 input 会取错字段）。
    control.dataset.key = f.key;

    // 行的定位属性刻意不叫 key：控件本身已经用了 data-key，
    // 同名会让 querySelector('[data-key=...]') 命中行而不是控件，
    // 拿到一个没有 .value 的 div（这个坑当场就踩到了）。
    const row = el('div', { class: 'cfg-row block', dataset: { rowKey: f.key } },
      el('span', { class: 'k' },
        el('span', { text: f.label }),
        f.restart ? el('span', { class: 'tag tag-off cfg-restart', text: '需重启' }) : null,
      ),
      control,
    );
    if (f.help) {
      row.append(el('p', { class: 'cfg-help', text: f.help }));
    }
    return row;
  }

  // collectPatch 只收集与基线不同的项，并按 kind 转成后端要的类型。
  //
  // 类型转换放在这里而不是让后端猜：
  // 后端 config.ApplyField 对类型是严格匹配的，
  // 而 <input type=number> 的 value 永远是字符串。
  function collectPatch(fields, draft, initial) {
    const patch = {};
    for (const f of fields) {
      const now = draft.get(f.key);
      if (sameValue(now, initial.get(f.key))) continue;

      switch (f.kind) {
        case 'bool':
          patch[f.key] = !!now;
          break;
        case 'int': {
          const n = parseInt(String(now).trim(), 10);
          if (Number.isNaN(n)) {
            throw new Error(f.label + ' 需要一个整数');
          }
          patch[f.key] = n;
          break;
        }
        case 'float': {
          const n = parseFloat(String(now).trim());
          if (Number.isNaN(n)) {
            throw new Error(f.label + ' 需要一个数字');
          }
          patch[f.key] = n;
          break;
        }
        case 'string_list':
          patch[f.key] = (now || [])
            .map((x) => String(x).trim())
            .filter((x) => x !== '');
          break;
        default:
          patch[f.key] = String(now == null ? '' : now);
      }
    }
    return patch;
  }

  /* ===================== 日志 ===================== */

  async function viewLogs(host, actions) {
    const box = el('pre', { class: 'logs', text: '等待日志…\n' });
    const wrap = el('div', { class: 'logs-page' }, box);

    actions.append(
      el('button', {
        class: 'btn btn-ghost btn-sm',
        onclick: () => { box.textContent = ''; },
        text: '清空',
      }),
    );

    host.append(wrap);
    startLogs();
  }

  function startLogs() {
    if (state.logSource) return;
    const box = $('.logs');
    if (!box) return;

    const src = new EventSource('/api/logs');
    state.logSource = src;

    src.onmessage = (e) => {
      try {
        appendLog(JSON.parse(e.data));
      } catch { /* 忽略坏数据 */ }
    };
    src.onerror = () => { /* 浏览器会自动重连 */ };
  }

  function stopLogs() {
    if (state.logSource) {
      state.logSource.close();
      state.logSource = null;
    }
  }

  /* ---------- 消息事件流（实时刷新的主通道） ---------- */

  // 对齐 MaiBot dashboard 的做法（unified WebSocket 的 chat 域事件）：
  // 消息一落库就通知前端，页面立刻拉增量，不等轮询。
  // 我们用 SSE 平替（后端 /api/events），EventSource 自带断线重连；
  // 轮询 setInterval 保留，作为事件流断线期间的兜底。

  function startEvents() {
    if (state.eventSource) return;
    const src = new EventSource('/api/events');
    state.eventSource = src;

    src.onmessage = (e) => {
      let ev;
      try { ev = JSON.parse(e.data); } catch { return; }
      if (ev && ev.type === 'message.new') onMessageEvent(ev);
    };
    src.onerror = () => { /* 浏览器会自动重连；期间由轮询兜底 */ };
  }

  function stopEvents() {
    if (state.eventSource) {
      state.eventSource.close();
      state.eventSource = null;
    }
  }

  // onMessageEvent 按当前页面分发刷新。事件只是「某会话有新消息」的信号：
  // 当前正看那个会话就拉消息流增量；否则只刷新列表/概览这类汇总视图。
  function onMessageEvent(ev) {
    if ($('#app').hidden || document.hidden) return;
    const route = currentRoute();

    if (route === 'chats') {
      // 事件属于当前打开的会话才拉消息流；
      // 但无论哪个会话有新消息，左栏列表的「最后一条」都该更新。
      if (ev.session_id && ev.session_id === state.sessionId) {
        scheduleRefresh('chats-messages', fetchChatsIncrement);
      }
      scheduleRefresh('chats-sessions', () => liveHooks.sessions && liveHooks.sessions());
      return;
    }

    if (route === 'talk') {
      if (ev.session_id && ev.session_id === talkState.sessionId) {
        scheduleRefresh('talk-messages', fetchTalkIncrement);
      }
      return;
    }

    if (route === 'overview') {
      scheduleRefresh('overview', refreshOverviewSilently, 1000);
    }
  }

  // fetchChatsIncrement 拉会话页当前会话的新消息。
  //
  // 只拉 after_ms 之后的，不做全量重载 —— 全量重载会把用户正在
  // 看的滚动位置拍到底部。事件驱动与轮询兜底共用这一份，
  // 保证两条路径刷出来的消息渲染完全一致。
  async function fetchChatsIncrement() {
    if (!state.sessionId || !state.newestMS) return;

    try {
      const data = await api(
        `/api/messages?session_id=${encodeURIComponent(state.sessionId)}` +
        `&limit=50&after_ms=${state.newestMS}`);
      const fresh = data.messages || [];
      if (!fresh.length) return;

      const box = $('.messages');
      if (!box) return;

      const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
      // 用和首屏、上翻完全相同的渲染器，三处显示才能一致。
      // 要把「上一条」传进去，否则同一个人连发三句会每句都带一次头像。
      const freshEntries = data.timeline && data.timeline.length ? data.timeline : fresh;
      box.append(buildTimeline(freshEntries, state.lastMsg));
      state.lastMsg = fresh[fresh.length - 1];
      state.newestMS = data.newest_ms || state.newestMS;
      if (atBottom) box.scrollTop = box.scrollHeight;
    } catch { /* 拉取失败静默重试：轮询兜底还在 */ }
  }

  // fetchTalkIncrement 拉对话页的新消息（含机器人回复）。
  async function fetchTalkIncrement() {
    if (!talkState.sessionId || !talkState.newestMS) return;

    try {
      const data = await api(
        `/api/messages?session_id=${encodeURIComponent(talkState.sessionId)}` +
        `&limit=50&after_ms=${talkState.newestMS}`);
      const fresh = data.messages || [];
      if (!fresh.length) return;

      const box = $('.talk-msgs');
      if (!box) return;

      for (const m of fresh) box.append(renderTalkMsg(m));
      talkState.newestMS = data.newest_ms || talkState.newestMS;
      // 拉到机器人的话就撤掉「正在输入」提示
      if (fresh.some((m) => m.is_self)) hideTalkTyping();
      box.scrollTop = box.scrollHeight;
    } catch { /* 拉取失败静默重试：轮询兜底还在 */ }
  }

  function appendLog(line) {
    const box = $('.logs');
    if (!box) return;
    if (box.textContent === '等待日志…\n') box.textContent = '';

    // 只在底部时才自动跟随，用户往上翻历史时不打扰
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 60;

    const row = el('span', { class: 'log-line' },
      el('span', { class: 'log-time', text: fmtClockSec(new Date(line.time).getTime()) + ' ' }),
      el('span', { class: 'log-lv ' + (line.level || '').trim(), text: (line.level || '').trim() + ' ' }),
      el('span', { class: 'log-logger', text: '[' + (line.logger || '') + '] ' }),
      el('span', { class: 'log-msg', text: line.message || '' }),
      document.createTextNode('\n'),
    );
    box.append(row);

    if (atBottom) box.scrollTop = box.scrollHeight;

    // 长时间开着日志页会无限增长，限制一下节点数
    while (box.childElementCount > 2000) box.removeChild(box.firstChild);
  }

  /* ===================== 启动 ===================== */

  async function startApp() {
    await refreshStatus();
    // 登录确认后才连事件流：匿名连上也是 401 反复重连。
    startEvents();
    if (!location.hash) location.hash = '#/overview';
    await navigate();
  }

  (async function init() {
    initTheme();

    // 探测登录态：直接请求 status，401 会走 showLogin
    try {
      const res = await fetch('/api/status', { credentials: 'same-origin' });
      if (res.ok) {
        showApp();
        await startApp();
      } else {
        showLogin();
      }
    } catch {
      showLogin();
    }
  })();

  // 定时刷新在线状态。日志页和会话页不重复拉 —— 那些页面自己会更。
  setInterval(() => {
    if ($('#app').hidden) return;
    refreshStatus();
  }, 20000);

  // 会话页轮询新消息：事件流的兜底。
  //
  // 主通道是 /api/events（消息落库即推送），但 SSE 断线、
  // 服务重启的窗口期里事件收不到，轮询保证数据最终会出现。
  // 间隔比事件流慢是刻意的：兜底不需要快，需要的是「永不缺席」。
  setInterval(() => {
    if ($('#app').hidden || currentRoute() !== 'chats') return;
    if (document.hidden) return;
    fetchChatsIncrement();
  }, 8000);

  // 对话页轮询机器人回复：事件流的兜底（同上）。
  // 保留 3.5s 的较快节奏：这是正在进行的对话，用户在等回话；
  // 防抖 + 打字延迟本来就要几秒，兜底再慢一拍体验就是「发了没人理」。
  setInterval(() => {
    if ($('#app').hidden || currentRoute() !== 'talk') return;
    if (document.hidden) return;
    fetchTalkIncrement();
  }, 3500);

  // 概览页定时刷新。参考实现的资源页有 5s 轮询的先例，
  // 概览数据块大，10s 足够「数字是新的」而不至于打爆接口。
  // 静默刷新：不闪骨架屏、不重置滚动位置。
  setInterval(() => {
    if ($('#app').hidden || currentRoute() !== 'overview') return;
    if (document.hidden) return;
    scheduleRefresh('overview-timer', refreshOverviewSilently, 0);
  }, 10000);

  // 切回可见立即补一次数据。后台标签页里事件与轮询都停了
  // （onMessageEvent 与 setInterval 都看 document.hidden），
  // 切回来却要干等下一轮轮询——对齐 React Query 的
  // refetchOnWindowFocus 行为：可见即刷新。
  document.addEventListener('visibilitychange', () => {
    if (document.hidden || $('#app').hidden) return;
    const route = currentRoute();
    if (route === 'chats') fetchChatsIncrement();
    else if (route === 'talk') fetchTalkIncrement();
    else if (route === 'overview') scheduleRefresh('overview', refreshOverviewSilently, 0);
  });
})();
