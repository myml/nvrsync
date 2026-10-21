/* ==========================================================================
   NVR Sync — 前端应用（原生 JS，无框架、无构建、无外部请求）

   页面：同步 / 录像 / 设置，全部由 hash 路由驱动：
     #/sync                         同步（默认）
     #/recordings                   天卡片墙
     #/recordings/<ch>/<YYYY-MM-DD> 当天播放器
     #/settings                     设置

   代码使用一个 IIFE 包住，不向 window 写入任何私有变量。
   后端接口约定见仓库根目录 ARCH.md，响应统一为 {ok,data,error}。
   ========================================================================== */
(function () {
  'use strict';

  /* ========================================================================
     0. 基础工具：数字、时间、字节、DOM
     ======================================================================== */

  var FALLBACK_CHANNELS = [1, 2, 3, 4, 5, 6, 7, 8];
  var PAGE_SIZE = 50;
  var LOG_LIMIT = 200;
  var WEEKDAYS = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  var ROUTE_TITLES = { sync: '同步', recordings: '录像', settings: '设置' };

  // 段状态 → 中文 + 徽章配色（与 style.css 的 .badge.ok/.wait/.run/.bad/.skip 对应）
  var STATUS_META = {
    done: { label: '已同步', cls: 'ok' },
    pending: { label: '待同步', cls: 'wait' },
    syncing: { label: '同步中', cls: 'run' },
    failed: { label: '失败', cls: 'bad' },
    skipped: { label: '已跳过', cls: 'skip' },
    local_only: { label: '仅本地', cls: 'local' }
  };

  function $(selector, root) {
    return (root || document).querySelector(selector);
  }

  function $$(selector, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(selector));
  }

  function pad2(n) {
    return (n < 10 ? '0' : '') + n;
  }

  function clamp(value, min, max) {
    return Math.min(max, Math.max(min, value));
  }

  function toInt(value, fallback) {
    var n = parseInt(String(value == null ? '' : value), 10);
    return isFinite(n) ? n : fallback || 0;
  }

  var numberFormatter = (typeof Intl !== 'undefined' && Intl.NumberFormat)
    ? new Intl.NumberFormat('zh-CN')
    : null;

  /** 千分位数字，用于统计卡与分页信息。 */
  function num(n) {
    n = Number(n) || 0;
    return numberFormatter ? numberFormatter.format(n) : String(n);
  }

  /** 字节 → B / KB / MB / GB / TB。 */
  function formatBytes(value) {
    var n = Number(value) || 0;
    if (n < 1024) return Math.round(n) + ' B';
    var units = ['KB', 'MB', 'GB', 'TB'];
    var v = n / 1024;
    var i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    var digits = v >= 100 ? 0 : v >= 10 ? 1 : 2;
    return v.toFixed(digits) + ' ' + units[i];
  }

  /** 秒 → 「3 小时 12 分」/「12 分 30 秒」/「30 秒」。 */
  function formatDuration(value) {
    var total = Math.max(0, Math.round(Number(value) || 0));
    if (total === 0) return '0 秒';
    var h = Math.floor(total / 3600);
    var m = Math.floor((total % 3600) / 60);
    var s = total % 60;
    if (h > 0) return h + ' 小时' + (m > 0 ? ' ' + m + ' 分' : '');
    if (m > 0) return m + ' 分' + (s > 0 ? ' ' + s + ' 秒' : '');
    return s + ' 秒';
  }

  /** unix 秒 → 本地 YYYY-MM-DD HH:mm:ss。 */
  function formatDateTime(unix) {
    if (!unix) return '—';
    var d = new Date(Number(unix) * 1000);
    if (isNaN(d.getTime())) return '—';
    return (
      d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
      ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds())
    );
  }

  /** unix 秒 → 本地 HH:mm:ss。 */
  function formatTime(unix) {
    var d = new Date(Number(unix) * 1000);
    if (isNaN(d.getTime())) return '--:--:--';
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }

  /** Date → 本地 YYYY-MM-DD。 */
  function isoDate(date) {
    return date.getFullYear() + '-' + pad2(date.getMonth() + 1) + '-' + pad2(date.getDate());
  }

  /** unix 秒 → 本地 YYYY-MM-DD。 */
  function isoDateFromUnix(unix) {
    return isoDate(new Date(Number(unix) * 1000));
  }

  /** 严格解析 YYYY-MM-DD（拒绝 2026-02-30 这种输入），返回本地 0 点 Date。 */
  function parseDay(value) {
    var m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(value || ''));
    if (!m) return null;
    var y = Number(m[1]);
    var mo = Number(m[2]);
    var d = Number(m[3]);
    var date = new Date(y, mo - 1, d);
    if (date.getFullYear() !== y || date.getMonth() !== mo - 1 || date.getDate() !== d) return null;
    return date;
  }

  /** 某天的 [0 点, 次日 0 点) 区间（unix 秒）与跨度。 */
  function dayBounds(value) {
    var start = parseDay(value);
    if (!start) return null;
    var next = new Date(start.getTime());
    next.setDate(next.getDate() + 1);
    var from = Math.floor(start.getTime() / 1000);
    var to = Math.floor(next.getTime() / 1000);
    return { from: from, to: to, span: to - from, date: isoDate(start) };
  }

  function weekdayName(value) {
    var d = parseDay(value);
    return d ? WEEKDAYS[d.getDay()] : '';
  }

  /** 平均码率，用于天卡片。 */
  function formatBitrate(bytes, seconds) {
    if (!bytes || !seconds || seconds <= 0) return '—';
    var mbps = (Number(bytes) * 8) / Number(seconds) / 1e6;
    return (mbps >= 10 ? mbps.toFixed(1) : mbps.toFixed(2)) + ' Mbps';
  }

  /** 只保留有值的查询参数，避免把空字符串发给后端。 */
  function queryString(params) {
    var sp = new URLSearchParams();
    Object.keys(params).forEach(function (key) {
      var value = params[key];
      if (value === undefined || value === null || value === '' || value === false) return;
      sp.set(key, String(value));
    });
    return sp.toString();
  }

  /* ---- DOM 构造 ---------------------------------------------------------- */

  /**
   * 创建元素。props 支持：
   *   class/text/html/style/dataset/value/checked/disabled/hidden/selected/for
   *   其它属性 → setAttribute；onXxx 函数 → addEventListener。
   * children 支持字符串、Node、数组和 null（自动忽略）。
   */
  function el(tag, props, children) {
    var node = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (key) {
        var value = props[key];
        if (value === undefined || value === null || value === false) return;
        if (key === 'class') { node.className = value; return; }
        if (key === 'text') { node.textContent = value; return; }
        if (key === 'html') { node.innerHTML = value; return; }
        if (key === 'style' && typeof value === 'object') { Object.assign(node.style, value); return; }
        if (key === 'dataset' && typeof value === 'object') { Object.assign(node.dataset, value); return; }
        if (key === 'value') { node.value = value; return; }
        if (key === 'checked') { node.checked = !!value; return; }
        if (key === 'disabled') { node.disabled = !!value; return; }
        if (key === 'hidden') { node.hidden = !!value; return; }
        if (key === 'selected') { node.selected = !!value; return; }
        if (key === 'for') { node.htmlFor = value; return; }
        if (key.slice(0, 2) === 'on' && typeof value === 'function') {
          node.addEventListener(key.slice(2).toLowerCase(), value);
          return;
        }
        node.setAttribute(key, value === true ? '' : String(value));
      });
    }
    appendAll(node, children);
    return node;
  }

  function appendAll(node, children) {
    if (children === undefined || children === null || children === false) return;
    if (Array.isArray(children)) {
      children.forEach(function (child) { appendAll(node, child); });
      return;
    }
    if (children instanceof Node) {
      node.appendChild(children);
      return;
    }
    node.appendChild(document.createTextNode(String(children)));
  }

  function clear(node) {
    if (!node) return;
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  /* ---- 图标（内联 SVG，全部是静态字符串） ------------------------------- */

  var ICON_PATHS = {
    refresh: '<path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/>',
    play: '<path d="M8 5.5v13l11-6.5z"/>',
    stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
    chevron: '<path d="m6 9 6 6 6-6"/>',
    arrowLeft: '<path d="M19 12H5"/><path d="m11 18-6-6 6-6"/>',
    arrowRight: '<path d="M5 12h14"/><path d="m13 6 6 6-6 6"/>',
    database: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.66 3.58 3 8 3s8-1.34 8-3V5"/><path d="M4 11v6c0 1.66 3.58 3 8 3s8-1.34 8-3v-6"/>',
    check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
    clock: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
    alert: '<path d="M12 4.5 21 20H3z"/><path d="M12 10v4"/><path d="M12 17h.01"/>',
    info: '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><path d="M12 8h.01"/>',
    eye: '<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z"/><circle cx="12" cy="12" r="2.8"/>',
    eyeOff: '<path d="m3 3 18 18"/><path d="M10.6 6.1A9.8 9.8 0 0 1 12 6c6 0 9.5 6 9.5 6a17 17 0 0 1-2.4 3.2"/><path d="M6.2 7.4A17.3 17.3 0 0 0 2.5 12S6 18 12 18a9.8 9.8 0 0 0 4-.8"/><path d="M9.9 9.9a3 3 0 0 0 4.2 4.2"/>',
    server: '<rect x="3.5" y="4" width="17" height="7" rx="2"/><rect x="3.5" y="13" width="17" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>',
    folder: '<path d="M3.5 7.5A2 2 0 0 1 5.5 5.5h3.2l1.8 2H19a2 2 0 0 1 2 2v7.5a2 2 0 0 1-2 2H5.5a2 2 0 0 1-2-2z"/>',
    video: '<rect x="3" y="6" width="12" height="12" rx="2.5"/><path d="M15 10.5 21 7v10l-6-3.5"/>',
    retry: '<path d="M20 11a8 8 0 1 0-.7 4.5"/><path d="M20 4v7h-7"/>',
    bolt: '<path d="M13 2 4.5 13.5H11l-1 8.5 8.5-11H12z"/>'
  };

  function icon(name, className) {
    var tpl = document.createElement('template');
    tpl.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" ' +
      'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"' +
      (className ? ' class="' + className + '"' : '') + '>' +
      (ICON_PATHS[name] || ICON_PATHS.info) + '</svg>';
    return tpl.content.firstElementChild;
  }

  /** 统一按钮：图标 + .btn-label 文本，方便运行中改文案。 */
  function button(label, opts) {
    opts = opts || {};
    var className = 'btn ' + (opts.variant || 'btn-ghost') + (opts.sm ? ' btn-sm' : '');
    var node = el('button', {
      class: className,
      type: 'button',
      id: opts.id,
      title: opts.title,
      disabled: opts.disabled,
      hidden: opts.hidden
    }, [
      opts.icon ? icon(opts.icon) : null,
      el('span', { class: 'btn-label', text: label })
    ]);
    if (opts.onClick) node.addEventListener('click', opts.onClick);
    return node;
  }

  /** 表单字段：label + 控件 + 可选提示。 */
  function field(labelText, control, opts) {
    opts = opts || {};
    return el('div', {
      class: 'field' + (opts.compact ? ' compact' : '') + (opts.span ? ' span-2' : '')
    }, [
      el('label', { for: opts.id || null, text: labelText }),
      control,
      opts.hint ? el('div', { class: 'hint', text: opts.hint }) : null
    ]);
  }

  /* ========================================================================
     1. API 封装：fetch → {ok,data,error}，失败统一弹 toast
     ======================================================================== */

  // 登录保护启用后，任何 API 401 都表示会话已失效：整页跳转到登录页。
  // 只跳一次，且登录页自身（不加载 app.js）不会触发，避免循环。
  var authRedirecting = false;

  function handleUnauthorized() {
    if (authRedirecting) return;
    if (String(location.pathname).replace(/\/+$/, '') === '/login') return;
    authRedirecting = true;
    location.replace('/login');
  }

  async function api(path, opts) {
    opts = opts || {};
    var init = {
      method: opts.method || 'GET',
      credentials: 'same-origin',
      headers: Object.assign({ Accept: 'application/json' }, opts.headers || {})
    };
    if (opts.body !== undefined && opts.body !== null) {
      init.headers['Content-Type'] = 'application/json';
      init.body = typeof opts.body === 'string' ? opts.body : JSON.stringify(opts.body);
    }

    var response;
    try {
      response = await fetch(path, init);
    } catch (err) {
      var netError = '网络错误：' + (err && err.message ? err.message : String(err));
      if (!opts.quiet) toast(netError, 'error');
      return { ok: false, error: netError, status: 0 };
    }

    // 401 统一交给登录页处理，不在这里弹 toast，避免每个轮询 / 请求各弹一条。
    if (response.status === 401) {
      handleUnauthorized();
      return { ok: false, error: 'unauthorized', status: 401, unauthorized: true };
    }

    var raw = '';
    try {
      raw = await response.text();
    } catch (err) {
      var readError = '读取响应失败（HTTP ' + response.status + '）';
      if (!opts.quiet) toast(readError, 'error');
      return { ok: false, error: readError, status: response.status };
    }

    var payload = null;
    if (raw) {
      try {
        payload = JSON.parse(raw);
      } catch (err) {
        var parseError = '服务端返回了无法解析的响应（HTTP ' + response.status + '）';
        if (!opts.quiet) toast(parseError, 'error');
        return { ok: false, error: parseError, status: response.status };
      }
    }

    var ok = response.ok && payload && payload.ok !== false;
    if (!ok) {
      var message =
        (payload && (payload.error || payload.message)) ||
        ('请求失败（HTTP ' + response.status + (response.statusText ? ' ' + response.statusText : '') + '）');
      if (!opts.quiet) toast(message, 'error');
      return { ok: false, error: message, status: response.status, data: payload && payload.data };
    }

    // 兼容两种信封：{ok:true,data:...} 与直接返回对象。
    return {
      ok: true,
      status: response.status,
      data: payload ? (payload.data !== undefined ? payload.data : payload) : null
    };
  }

  /** 分页拉取某个筛选条件下的全部段（播放器/时间轴用）。 */
  async function fetchAllSegments(query) {
    var pageSize = 1000;
    var offset = 0;
    var items = [];
    var total = 0;

    for (var i = 0; i < 100; i++) {
      var res = await api('/api/segments?' + queryString(Object.assign({}, query, {
        limit: pageSize,
        offset: offset
      })));
      if (!res.ok) return res;

      var data = res.data || {};
      var batch = Array.isArray(data.items) ? data.items : [];
      total = Number(data.total) || 0;
      items = items.concat(batch);

      if (!batch.length || items.length >= total) break;
      offset += batch.length;
    }

    return { ok: true, data: { items: items, total: total } };
  }

  /* ========================================================================
     2. Toast 通知
     ======================================================================== */

  var toastSeen = new Map();

  function toast(message, type, ttl) {
    if (!message) return;
    type = type || 'info';

    // 同一条错误在 3 秒内只弹一次，避免并发请求刷屏。
    var key = type + '|' + message;
    var now = Date.now();
    if (toastSeen.get(key) && now - toastSeen.get(key) < 3000) return;
    toastSeen.set(key, now);
    if (toastSeen.size > 60) {
      toastSeen.forEach(function (ts, k) {
        if (now - ts > 15000) toastSeen.delete(k);
      });
    }

    var wrap = document.getElementById('toasts');
    if (!wrap) return;

    // 同时最多 4 条。
    while (wrap.children.length >= 4) wrap.removeChild(wrap.firstElementChild);

    var iconName = type === 'success' ? 'check' : type === 'error' ? 'alert' : 'info';
    var box = el('div', { class: 'toast toast-' + type }, [
      icon(iconName, 'toast-icon'),
      el('div', { class: 'toast-msg', text: message })
    ]);
    wrap.appendChild(box);

    // 下一帧再加 .in，触发 CSS 过渡。
    requestAnimationFrame(function () { box.classList.add('in'); });

    var timer = setTimeout(function () {
      box.classList.remove('in');
      setTimeout(function () { box.remove(); }, 220);
    }, ttl || (type === 'error' ? 6200 : 4200));

    box.addEventListener('click', function () {
      clearTimeout(timer);
      box.remove();
    });
  }

  /* ========================================================================
     3. 通用 UI 片段：加载中 / 空状态 / 错误状态 / 按钮忙碌态
     ======================================================================== */

  function loadingPanel(message) {
    return el('div', { class: 'state-panel' }, [
      el('div', { class: 'spinner' }),
      el('p', { class: 'state-hint', text: message || '加载中…' })
    ]);
  }

  function statePanel(opts) {
    opts = opts || {};
    return el('div', { class: 'state-panel' + (opts.error ? ' is-error' : '') }, [
      el('div', { class: 'state-icon' }, [icon(opts.icon || (opts.error ? 'alert' : 'info'))]),
      el('h3', { class: 'state-title', text: opts.title || '' }),
      opts.hint ? el('p', { class: 'state-hint', text: opts.hint }) : null,
      opts.actions && opts.actions.length
        ? el('div', { class: 'state-actions' }, opts.actions)
        : null
    ]);
  }

  /** 给按钮加转圈并禁用，结束后恢复（捕获异常避免未处理 rejection）。 */
  async function withBusy(node, fn) {
    if (!node || node.dataset.busy === '1' || node.disabled) return;
    node.dataset.busy = '1';
    node.disabled = true;
    node.classList.add('is-loading');
    try {
      return await fn();
    } catch (err) {
      console.error(err);
      toast('操作失败：' + (err && err.message ? err.message : String(err)), 'error');
    } finally {
      node.dataset.busy = '';
      node.disabled = false;
      node.classList.remove('is-loading');
    }
  }

  function paramValue(select, value) {
    if (select && Array.prototype.some.call(select.options, function (o) { return o.value === value; })) {
      select.value = value;
    }
  }

  /** 页面标题区。leading 为可选的返回按钮等。 */
  function pageHeader(title, sub, actions, leading) {
    return el('div', { class: 'page-head' }, [
      el('div', { class: 'head-left' }, [
        leading || null,
        el('div', {}, [
          el('h1', {}, Array.isArray(title) ? title : [String(title)]),
          sub ? el('p', { class: 'page-sub', text: sub }) : null
        ])
      ]),
      actions && actions.length ? el('div', { class: 'head-actions' }, actions) : null
    ]);
  }

  function metaCell(label, value) {
    return el('div', {}, [
      el('div', { class: 'meta-label', text: label }),
      el('div', { class: 'meta-value', text: value })
    ]);
  }

  function chip(label, value, accent) {
    return el('span', { class: 'chip' + (accent ? ' chip-accent' : '') }, [
      document.createTextNode(label + ' '),
      el('b', { text: String(value) })
    ]);
  }

  function statusLabel(status) {
    return (STATUS_META[status] && STATUS_META[status].label) || status || '未知';
  }

  function statusBadge(seg) {
    var meta = STATUS_META[seg.status] || { label: statusLabel(seg.status), cls: '' };
    var node = el('span', { class: 'badge ' + meta.cls, text: meta.label });
    if (seg.err) node.title = seg.err;
    return node;
  }

  /* ========================================================================
     4. 全局状态与通道
     ======================================================================== */

  var state = {
    settings: null,
    channels: FALLBACK_CHANNELS.slice(),
    filters: null,
    // 最近一次 /api/sync/status 的结果（字段与 store.SyncState 对齐）
    sync: {
      running: false, cur: '', done: 0, total: 0,
      bytes: 0, speed: 0, started_at: 0, last_error: ''
    },
    statusError: '',
    statusHooks: {},
    recordings: { ch: null },
    logs: { open: true }
  };

  /** 设置里的 channels → 去重、排序后的正整数数组；为空时回退 1..8。 */
  function channelsFromSettings(settings) {
    var raw = settings && Array.isArray(settings.channels) ? settings.channels : [];
    var seen = {};
    raw.forEach(function (value) {
      var n = Number(value);
      if (isFinite(n) && n > 0 && Math.floor(n) === n) seen[n] = true;
    });
    var out = Object.keys(seen).map(Number).sort(function (a, b) { return a - b; });
    return out.length ? out : FALLBACK_CHANNELS.slice();
  }

  function availableChannels() {
    return (state.channels && state.channels.length ? state.channels : FALLBACK_CHANNELS)
      .slice()
      .sort(function (a, b) { return a - b; });
  }

  function defaultFilters() {
    var now = new Date();
    var from = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    from.setDate(from.getDate() - 6);
    return {
      ch: 0,
      from: isoDate(from),
      to: isoDate(now),
      status: '',
      page: 1,
      limit: PAGE_SIZE
    };
  }

  function normalizeSettings(raw) {
    raw = raw && typeof raw === 'object' ? raw : {};
    var channels = Array.isArray(raw.channels) ? raw.channels.map(Number) : [];
    channels = channels.filter(function (n) {
      return isFinite(n) && n > 0 && Math.floor(n) === n;
    }).sort(function (a, b) { return a - b; });

    return {
      nvr_host: typeof raw.nvr_host === 'string' ? raw.nvr_host : '',
      nvr_user: typeof raw.nvr_user === 'string' ? raw.nvr_user : '',
      nvr_pass: typeof raw.nvr_pass === 'string' ? raw.nvr_pass : '',
      admin_port: toInt(raw.admin_port, 80),
      storage_dir: typeof raw.storage_dir === 'string' ? raw.storage_dir : '',
      channels: channels,
      auto_minutes: toInt(raw.auto_minutes, 0),
      retention_days: toInt(raw.retention_days, 0),
      transcode: !!raw.transcode,
      ffmpeg: typeof raw.ffmpeg === 'string' ? raw.ffmpeg : '',
      cutoff: raw.cutoff === 'yesterday' || raw.cutoff === 'days' ? raw.cutoff : 'latest',
      cutoff_days: toInt(raw.cutoff_days, 0),
      catalog_days: toInt(raw.catalog_days, 0),
      catalog_minutes: toInt(raw.catalog_minutes, 0),
      catalog_channels: Array.isArray(raw.catalog_channels)
        ? raw.catalog_channels.map(function (n) { return toInt(n, 0); })
            .filter(function (n) { return isFinite(n) && n > 0; })
            .sort(function (a, b) { return a - b; })
        : [],
      auth_enabled: !!raw.auth_enabled,
      auth_user: typeof raw.auth_user === 'string' && raw.auth_user ? raw.auth_user : 'admin'
    };
  }

  /* ========================================================================
     5. 同步状态轮询 + 顶部指示器
     ======================================================================== */

  var pollTimer = null;
  var pollBusy = false;

  function updateIndicator() {
    var indicator = document.getElementById('sync-indicator');
    var label = document.getElementById('sync-status-label');
    if (!indicator || !label) return;

    var st = state.sync || {};
    var name = 'idle';
    var text = '空闲';
    var title = '同步状态：空闲';

    if (state.statusError) {
      name = 'error';
      text = '离线';
      title = '无法获取同步状态：' + state.statusError;
    } else if (st.running) {
      name = 'running';
      text = st.total > 0 ? '同步中 ' + num(st.done) + '/' + num(st.total) : '同步中';
      title = '同步状态：同步中';
      if (st.total > 0) title += '（' + st.done + '/' + st.total + ' 段）';
      if (st.speed > 0) title += '，' + st.speed.toFixed(1) + ' MB/s';
      if (st.cur) title += '，当前：' + st.cur;
    } else if (st.last_error) {
      name = 'error';
      text = '异常';
      title = '上次同步出错：' + st.last_error;
    }

    indicator.setAttribute('data-state', name);
    indicator.title = title;
    if (label.textContent !== text) label.textContent = text;
  }

  function applyStatus(next) {
    var previous = state.sync || {};
    var wasRunning = !!previous.running;
    state.sync = Object.assign({}, previous, next || {});
    updateIndicator();

    var hooks = state.statusHooks || {};
    if (typeof hooks.onStatus === 'function') hooks.onStatus(state.sync, wasRunning);

    if (wasRunning && !state.sync.running) {
      if (typeof hooks.onFinished === 'function') hooks.onFinished(state.sync);
      var done = Number(state.sync.done) || 0;
      var total = Number(state.sync.total) || 0;
      var summary = total > 0 ? '完成 ' + done + '/' + total + ' 段' : '本次没有需要下载的段';
      if (state.sync.last_error) {
        toast('同步结束（有错误）：' + summary, 'error', 6500);
      } else {
        toast('同步结束：' + summary, 'success', 5000);
      }
    } else if (!wasRunning && state.sync.running) {
      // 自动同步或其它客户端触发的同步，这里补一条提示。
      toast('同步已开始', 'info', 3000);
    }
  }

  async function pollStatus() {
    if (pollBusy) return;
    if (document.visibilityState === 'hidden') return;
    pollBusy = true;
    try {
      var res = await api('/api/sync/status', { quiet: true });
      if (res.ok) {
        var recovered = !!state.statusError;
        state.statusError = '';
        if (recovered) toast('同步状态已恢复', 'success', 2500);
        applyStatus(res.data || {});
      } else {
        var first = !state.statusError;
        state.statusError = res.error || '无法连接服务端';
        updateIndicator();
        if (first) toast('无法获取同步状态：' + state.statusError, 'error', 6000);
      }
    } finally {
      pollBusy = false;
    }
  }

  function startStatusPolling() {
    if (!pollTimer) pollTimer = setInterval(pollStatus, 2000);
    pollStatus();
  }

  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') pollStatus();
  });

  /* ========================================================================
     6. 同步页
     ======================================================================== */

  function statCard(def) {
    var value = el('div', { class: 'stat-value is-loading', text: '—' });
    var sub = el('div', { class: 'stat-sub', text: '加载中…' });
    var node = el('div', { class: 'stat-card' }, [
      el('div', { class: 'stat-head' }, [
        el('span', { class: 'stat-label', text: def.label }),
        el('span', { class: 'stat-icon ' + (def.tone || '') }, [icon(def.icon)])
      ]),
      value,
      sub
    ]);
    return { node: node, value: value, sub: sub };
  }

  function setStat(card, value, sub) {
    card.value.classList.remove('is-loading');
    card.value.textContent = value;
    card.sub.textContent = sub;
  }

  function statLoading(card, message) {
    card.value.classList.add('is-loading');
    card.value.textContent = '—';
    card.sub.textContent = message || '加载中…';
  }

  var syncView = {
    mount: function (root) {
      var view = {
        destroyed: false,
        logTimer: null,
        segSeq: 0,
        statsSeq: 0,
        logsSeq: 0,
        starting: false,
        hooks: null
      };

      if (!state.filters) state.filters = defaultFilters();
      var f = state.filters;
      if (!f.limit) f.limit = PAGE_SIZE;
      // 设置里的通道列表可能在本次会话中被修改过，丢弃已不存在的筛选通道。
      if (f.ch && availableChannels().indexOf(f.ch) < 0) f.ch = 0;

      /* ---- 统计卡 ---- */
      var statDefs = [
        { key: 'all', label: 'NVR 段数', icon: 'database', tone: '' },
        { key: 'done', label: '已同步', icon: 'check', tone: 'tone-ok' },
        { key: 'pending', label: '待同步', icon: 'clock', tone: 'tone-warn' },
        { key: 'failed', label: '失败', icon: 'alert', tone: 'tone-bad' },
        { key: 'local', label: '仅本地', icon: 'database', tone: '' },
        { key: 'earliest', label: '最早录像', icon: 'clock', tone: '' },
        { key: 'capacity', label: '本地容量', icon: 'folder', tone: '' },
        { key: 'speed', label: '同步速率', icon: 'bolt', tone: '' }
      ];
      view.statCards = {};
      var cards = statDefs.map(function (def) {
        var card = statCard(def);
        view.statCards[def.key] = card;
        return card.node;
      });
      var stats = el('div', { class: 'stats' }, cards);

      /* ---- 工具栏 ---- */
      var chSelect = el('select', { class: 'control', id: 'sync-ch' },
        [el('option', { value: '0', text: '全部通道' })].concat(
          availableChannels().map(function (ch) {
            return el('option', { value: String(ch), text: 'CAM ' + ch });
          })
        )
      );
      paramValue(chSelect, String(f.ch || 0));

      var fromInput = el('input', { class: 'control', type: 'date', id: 'sync-from', value: f.from || '' });
      var toInput = el('input', { class: 'control', type: 'date', id: 'sync-to', value: f.to || '' });

      var statusSelect = el('select', { class: 'control', id: 'sync-status' }, [
        el('option', { value: '', text: '全部状态' }),
        el('option', { value: 'pending', text: '待同步' }),
        el('option', { value: 'syncing', text: '同步中' }),
        el('option', { value: 'done', text: '已同步' }),
        el('option', { value: 'failed', text: '失败' }),
        el('option', { value: 'skipped', text: '已跳过' }),
        el('option', { value: 'local_only', text: '仅本地' })
      ]);
      statusSelect.value = f.status || '';

      var refreshBtn = button('刷新', { icon: 'refresh', id: 'sync-refresh' });
      var stopBtn = button('停止', { icon: 'stop', id: 'sync-stop', hidden: true });
      var startBtn = button('开始同步', { icon: 'play', variant: 'btn-primary', id: 'sync-start' });

      var toolbar = el('section', { class: 'card toolbar' }, [
        field('通道', chSelect, { id: 'sync-ch' }),
        field('从', fromInput, { id: 'sync-from' }),
        field('到', toInput, { id: 'sync-to' }),
        field('状态', statusSelect, { id: 'sync-status' }),
        el('div', { class: 'toolbar-actions' }, [refreshBtn, stopBtn, startBtn])
      ]);

      /* ---- 进度条（同步中显示） ---- */
      var progressLabel = el('span', { text: '同步中' });
      var progressFill = el('i');
      var progressMeta = el('span', { class: 'sp-meta', text: '' });
      var progress = el('div', { class: 'sync-progress', hidden: true }, [
        el('span', { class: 'sp-text' }, [el('span', { class: 'sp-dot' }), progressLabel]),
        el('div', { class: 'sp-bar' }, [progressFill]),
        progressMeta
      ]);

      /* ---- 表格 ---- */
      var tableCard = el('section', { class: 'card table-card' });

      /* ---- 日志 ---- */
      var logCount = el('span', { class: 'log-count', text: '0' });
      logCount.title = '最多显示最近 ' + LOG_LIMIT + ' 条';
      var logScroll = el('div', { class: 'log-scroll' }, [
        el('div', { class: 'log-empty', text: '正在加载日志…' })
      ]);
      var logsToggle = el('button', {
        class: 'logs-toggle',
        type: 'button',
        'aria-expanded': String(state.logs.open)
      }, [
        el('h2', {}, ['同步日志', logCount]),
        el('span', { class: 'chevron' }, [icon('chevron')])
      ]);
      var logsCard = el('section', { class: 'card logs-card' }, [
        logsToggle,
        el('div', { class: 'logs-body' }, [logScroll])
      ]);

      var page = el('div', { class: 'page' }, [
        pageHeader('同步', '从 NVR 拉取缺失的录像段；进度与历史日志会自动刷新。'),
        progress,
        stats,
        toolbar,
        tableCard,
        logsCard
      ]);
      root.appendChild(page);

      view.progress = progress;
      view.progressLabel = progressLabel;
      view.progressFill = progressFill;
      view.progressMeta = progressMeta;
      view.tableCard = tableCard;
      view.logScroll = logScroll;
      view.logCount = logCount;
      view.logsCard = logsCard;
      view.logsToggle = logsToggle;
      view.startBtn = startBtn;
      view.stopBtn = stopBtn;
      view.refreshBtn = refreshBtn;

      syncLogsOpen(view);

      /* ---- 事件 ---- */
      chSelect.addEventListener('change', function () {
        f.ch = toInt(chSelect.value, 0);
        f.page = 1;
        loadSegments(view);
        loadStats(view);
      });

      fromInput.addEventListener('change', function () {
        f.from = fromInput.value;
        f.page = 1;
        loadSegments(view);
      });

      toInput.addEventListener('change', function () {
        f.to = toInput.value;
        f.page = 1;
        loadSegments(view);
      });

      statusSelect.addEventListener('change', function () {
        f.status = statusSelect.value;
        f.page = 1;
        loadSegments(view);
      });

      refreshBtn.addEventListener('click', function () {
        withBusy(refreshBtn, function () {
          return refreshCatalog(view, { force: true, loud: true, quiet: true }).then(function () {
            return Promise.all([loadStats(view), loadSegments(view), loadLogs(view)]);
          });
        });
      });

      startBtn.addEventListener('click', function () { startSync(view); });
      stopBtn.addEventListener('click', function () { stopSync(view); });

      logsToggle.addEventListener('click', function () {
        state.logs.open = !state.logs.open;
        syncLogsOpen(view);
        if (state.logs.open) loadLogs(view, { quiet: true });
      });

      /* ---- 生命周期 ---- */
      view.hooks = {
        onStatus: function (st) {
          if (view.destroyed) return;
          updateProgress(view, st);
          updateRunButtons(view, st);
        },
        onFinished: function () {
          if (view.destroyed) return;
          loadStats(view);
          loadSegments(view);
          loadLogs(view, { quiet: true });
        }
      };
      state.statusHooks = view.hooks;

      view.logTimer = setInterval(function () {
        if (view.destroyed) return;
        if (document.visibilityState === 'hidden') return;
        if (!state.logs.open) return;
        loadLogs(view, { quiet: true });
      }, 7000);

      // 首次渲染
      resetStats(view);
      showTableLoading(view);
      updateProgress(view, state.sync);
      updateRunButtons(view, state.sync);
      loadStats(view);
      loadSegments(view);
      loadLogs(view);

      return function cleanup() {
        view.destroyed = true;
        if (view.logTimer) {
          clearInterval(view.logTimer);
          view.logTimer = null;
        }
        if (state.statusHooks === view.hooks) state.statusHooks = {};
      };
    }
  };

  /** 后台对账：把 NVR 当前持有的录像同步进本地索引（只列不下载）。 */
  async function refreshCatalog(view, opts) {
    var force = !!(opts && opts.force);
    var res = await api('/api/catalog/refresh' + (force ? '?force=1' : ''), { method: 'POST', quiet: true });
    if (res.ok && res.data && res.data.skipped) {
      if (opts && opts.loud) toast('对账正在进行，本次已跳过', 'info');
      return res;
    }
    if (res.ok && !view.destroyed) {
      if (opts && opts.loud) {
        toast('对账完成：新增 ' + res.data.added + '，仅本地 ' + res.data.became_local + '，丢失 ' + res.data.marked_gone, 'ok');
      }
      loadStats(view);
      loadSegments(view);
    } else if (!res.ok && opts && opts.loud) {
      toast(res.error || '对账失败', 'error');
    }
    return res;
  }

  function syncLogsOpen(view) {
    view.logsCard.classList.toggle('is-collapsed', !state.logs.open);
    view.logsToggle.setAttribute('aria-expanded', String(state.logs.open));
  }

  function resetStats(view) {
    Object.keys(view.statCards).forEach(function (key) {
      statLoading(view.statCards[key], '加载中…');
    });
  }

  async function loadStats(view) {
    var seq = ++view.statsSeq;
    var f = state.filters;
    var ch = f.ch > 0 ? f.ch : 0;

    resetStats(view);

    var res = await api('/api/stats?' + queryString({ ch: ch }), { quiet: true });
    if (view.destroyed || seq !== view.statsSeq) return;
    if (!res.ok || !res.data) {
      Object.keys(view.statCards).forEach(function (key) {
        statLoading(view.statCards[key], '加载失败');
      });
      toast('统计信息加载失败：' + (res.error || '未知错误'), 'error');
      return;
    }

    var d = res.data;
    var chLabel = f.ch > 0 ? 'CAM ' + f.ch : '全部通道';
    var donePct = d.total > 0 ? Math.round((d.done / d.total) * 100) + '%' : '0%';

    setStat(view.statCards.all, num(d.total), chLabel + ' · 全部状态');
    setStat(view.statCards.done, num(d.done), donePct + ' 已完成');
    setStat(
      view.statCards.pending,
      num((d.pending || 0) + (d.syncing || 0)),
      d.syncing > 0 ? num(d.syncing) + ' 段正在下载' : '排队等待下载'
    );
    setStat(view.statCards.failed, num(d.failed), d.failed > 0 ? '可在下方列表重试' : '没有失败记录');
    setStat(view.statCards.local, num(d.local_only), 'NVR 已覆盖，仅本地保留');

    setStat(
      view.statCards.earliest,
      d.earliest ? formatDateTime(d.earliest).slice(0, 16) : '—',
      d.days > 0 ? '覆盖 ' + num(d.days) + ' 天' : '暂无本地录像'
    );

    var free = Number(d.storage_free) || 0;
    var totalDisk = Number(d.storage_total) || 0;
    var low = totalDisk > 0 && free / totalDisk < 0.1;
    setStat(
      view.statCards.capacity,
      formatBytes(d.bytes),
      totalDisk > 0
        ? (low ? '空间不足！剩余 ' : '剩余 ') + formatBytes(free) + ' / 共 ' + formatBytes(totalDisk)
        : '存储目录未配置'
    );
    view.statCards.capacity.node.classList.toggle('is-warn', low);

    var speed = Number(d.speed) || 0;
    setStat(
      view.statCards.speed,
      speed > 0 ? speed.toFixed(1) + ' MB/s' : '—',
      d.running ? '正在同步' : '上次同步平均'
    );
  }

  function showTableLoading(view) {
    clear(view.tableCard);
    view.tableCard.appendChild(loadingPanel('正在加载录像段…'));
  }

  function showTableError(view, message) {
    clear(view.tableCard);
    view.tableCard.appendChild(statePanel({
      error: true,
      title: '录像段加载失败',
      hint: message,
      actions: [button('重试', { icon: 'refresh', onClick: function () { loadSegments(view); } })]
    }));
  }

  async function loadSegments(view) {
    var seq = ++view.segSeq;
    var f = state.filters;
    var from = f.from ? dayBounds(f.from) : null;
    var to = f.to ? dayBounds(f.to) : null;

    showTableLoading(view);

    var params = {
      ch: f.ch > 0 ? f.ch : '',
      from: from ? from.from : '',
      to: to ? to.to : '',
      status: f.status || '',
      limit: f.limit || PAGE_SIZE,
      offset: Math.max(0, (f.page - 1) * (f.limit || PAGE_SIZE))
    };

    var res = await api('/api/segments?' + queryString(params));
    if (view.destroyed || seq !== view.segSeq) return;

    if (!res.ok) {
      showTableError(view, res.error);
      return;
    }

    var data = res.data || { items: [], total: 0 };
    var total = Number(data.total) || 0;
    var lastPage = Math.max(1, Math.ceil(total / (f.limit || PAGE_SIZE)));
    if (f.page > lastPage) {
      f.page = lastPage;
      loadSegments(view);
      return;
    }

    renderTable(view, data);
  }

  function renderTable(view, data) {
    clear(view.tableCard);
    var items = Array.isArray(data.items) ? data.items : [];
    var total = Number(data.total) || 0;

    if (!items.length) {
      view.tableCard.appendChild(statePanel({
        title: '没有符合条件的录像段',
        hint: '调整通道、日期或状态筛选；也可以按当前筛选范围从 NVR 同步一次。',
        actions: [button('开始同步', {
          icon: 'play',
          variant: 'btn-primary',
          onClick: function () { startSync(view); }
        })]
      }));
      return;
    }

    var table = el('table', { class: 'data-table' }, [
      el('thead', {}, [
        el('tr', {}, [
          el('th', { text: '通道' }),
          el('th', { text: '时间段' }),
          el('th', { class: 'num', text: '大小' }),
          el('th', { text: '状态' }),
          el('th', { text: '同步时间' }),
          el('th', { class: 'actions', text: '操作' })
        ])
      ])
    ]);

    var tbody = el('tbody');
    items.forEach(function (seg) {
      var start = Number(seg.start) || 0;
      var end = Number(seg.end) || 0;

      var endText;
      if (!end) {
        endText = '—';
      } else if (isoDateFromUnix(end) === isoDateFromUnix(start)) {
        endText = formatTime(end);
      } else {
        endText = formatDateTime(end);
      }

      var timeCell = el('td', { class: 'time-cell' }, [
        el('div', { class: 'mono', text: formatDateTime(start) }),
        el('div', { class: 'end', text: '→ ' + endText })
      ]);

      var size = Number(seg.bytes) > 0 ? Number(seg.bytes) : Number(seg.size) || 0;
      var sizeCell = el('td', { class: 'num', text: size ? formatBytes(size) : '—' });
      sizeCell.title = 'NVR 大小：' + (seg.size ? formatBytes(seg.size) : '—') +
        '；本地：' + (seg.bytes ? formatBytes(seg.bytes) : '—');

      var syncedCell = el('td', {
        class: 'mono small' + (seg.synced_at ? '' : ' muted'),
        text: seg.synced_at ? formatDateTime(seg.synced_at) : '—'
      });

      var actionCell = el('td', { class: 'actions' });
      if (seg.status === 'failed' || seg.status === 'pending') {
        var retry = button(seg.status === 'failed' ? '重试' : '同步', {
          icon: 'retry',
          sm: true,
          title: seg.status === 'failed' ? '重新同步这一段录像' : '立即同步这一段录像'
        });
        retry.addEventListener('click', function () { retrySegment(view, seg, retry); });
        actionCell.appendChild(retry);
      } else if (seg.status === 'syncing') {
        actionCell.appendChild(button('同步中', { sm: true, disabled: true }));
      } else if (seg.status === 'done') {
        actionCell.appendChild(el('a', {
          class: 'btn btn-sm btn-ghost',
          href: '#/recordings/' + seg.ch + '/' + isoDateFromUnix(start),
          title: '打开当天回看'
        }, [icon('video'), el('span', { class: 'btn-label', text: '回看' })]));
      } else {
        actionCell.appendChild(el('span', { class: 'muted small', text: '—' }));
      }

      tbody.appendChild(el('tr', {}, [
        el('td', {}, [el('span', { class: 'ch-badge', text: 'CAM ' + seg.ch })]),
        timeCell,
        sizeCell,
        el('td', {}, [statusBadge(seg)]),
        syncedCell,
        actionCell
      ]));
    });

    table.appendChild(tbody);

    var pages = Math.max(1, Math.ceil(total / (state.filters.limit || PAGE_SIZE)));
    var f = state.filters;
    var first = total === 0 ? 0 : (f.page - 1) * (f.limit || PAGE_SIZE) + 1;
    var last = Math.min(total, f.page * (f.limit || PAGE_SIZE));

    var prev = button('上一页', { sm: true, disabled: f.page <= 1 });
    var next = button('下一页', { sm: true, disabled: f.page >= pages });
    prev.addEventListener('click', function () {
      if (f.page > 1) { f.page--; loadSegments(view); }
    });
    next.addEventListener('click', function () {
      if (f.page < pages) { f.page++; loadSegments(view); }
    });

    var pager = el('div', { class: 'pager' }, [
      el('span', {
        class: 'pager-info',
        text: '第 ' + first + '–' + last + ' 条，共 ' + num(total) + ' 条'
      }),
      el('div', { class: 'pager-actions' }, [
        prev,
        el('span', { class: 'page-num', text: f.page + ' / ' + pages }),
        next
      ])
    ]);

    view.tableCard.appendChild(el('div', { class: 'table-wrap' }, [table]));
    view.tableCard.appendChild(pager);
  }

  async function retrySegment(view, seg, btn) {
    await withBusy(btn, async function () {
      var res = await api('/api/sync/start', {
        method: 'POST',
        body: { channels: [seg.ch], from: seg.start, to: seg.end }
      });
      if (res.ok) {
        toast('已提交：CAM ' + seg.ch + ' ' + formatDateTime(seg.start) + '（仅该段）', 'success');
        state.sync.running = true;
        updateIndicator();
        updateRunButtons(view, state.sync);
        pollStatus();
      } else {
        pollStatus();
      }
    });
    // 让按钮恢复后与最新状态保持一致（行可能已被刷新替换）。
    if (!view.destroyed) updateRunButtons(view, state.sync);
  }

  async function startSync(view) {
    var f = state.filters;
    var channels = f.ch > 0 ? [f.ch] : availableChannels();
    if (!channels.length) {
      toast('没有可同步的通道，请先在设置页选择通道', 'error');
      return;
    }

    var from = 0;
    var to = 0;
    if (f.from) {
      var fromBounds = dayBounds(f.from);
      if (fromBounds) from = fromBounds.from;
    }
    if (f.to) {
      var toBounds = dayBounds(f.to);
      if (toBounds) to = toBounds.to;
    }
    if (from && to && from >= to) {
      toast('起始日期不能晚于结束日期', 'error');
      return;
    }
    if (view.starting) return;

    view.starting = true;
    view.startBtn.classList.add('is-loading');
    view.startBtn.disabled = true;
    try {
      var res = await api('/api/sync/start', {
        method: 'POST',
        body: { channels: channels, from: from, to: to }
      });
      if (res.ok) {
        toast('同步任务已启动（' + channels.length + ' 个通道）', 'success');
        state.sync.running = true;
        updateIndicator();
        pollStatus();
      } else {
        pollStatus();
      }
    } finally {
      view.starting = false;
      view.startBtn.classList.remove('is-loading');
      updateRunButtons(view, state.sync);
    }
  }

  async function stopSync(view) {
    if (view.stopBtn.disabled) return;
    view.stopBtn.disabled = true;
    view.stopBtn.classList.add('is-loading');
    try {
      var res = await api('/api/sync/stop', { method: 'POST' });
      if (res.ok) {
        toast('已请求停止同步', 'info');
        pollStatus();
      }
    } finally {
      view.stopBtn.classList.remove('is-loading');
      view.stopBtn.disabled = false;
      pollStatus();
    }
  }

  function updateProgress(view, st) {
    var running = !!(st && st.running);
    view.progress.hidden = !running;
    if (!running) {
      view.progressFill.style.width = '0%';
      return;
    }

    var done = Number(st.done) || 0;
    var total = Number(st.total) || 0;
    var pct = total > 0 ? Math.min(100, (done / total) * 100) : 0;

    view.progressFill.style.width = pct + '%';
    view.progressLabel.textContent = total > 0
      ? '同步中 ' + num(done) + '/' + num(total)
      : '同步中（准备队列…）';

    var parts = [];
    if (total > 0) parts.push(done + '/' + total + ' 段');
    if (st.bytes) parts.push(formatBytes(st.bytes));
    if (st.speed > 0) parts.push(Number(st.speed).toFixed(1) + ' MB/s');
    view.progressMeta.textContent = parts.length ? parts.join(' · ') : '等待进度…';
    view.progress.title = st.cur ? '当前：' + st.cur : '同步进行中';
  }

  function updateRunButtons(view, st) {
    var running = !!(st && st.running);
    view.stopBtn.hidden = !running;
    view.startBtn.disabled = running || !!view.starting;
    var label = view.startBtn.querySelector('.btn-label');
    if (label) label.textContent = running ? '同步中…' : '开始同步';
    view.startBtn.title = running ? '同步正在进行中' : '按当前筛选范围开始同步';
  }

  async function loadLogs(view, opts) {
    opts = opts || {};
    var seq = ++view.logsSeq;
    var res = await api('/api/logs?limit=' + LOG_LIMIT, { quiet: !!opts.quiet });
    if (view.destroyed || seq !== view.logsSeq) return;

    if (!res.ok) {
      if (!opts.quiet) {
        clear(view.logScroll);
        view.logScroll.appendChild(el('div', { class: 'log-empty', text: '日志加载失败：' + res.error }));
      }
      return;
    }
    renderLogs(view, Array.isArray(res.data) ? res.data : []);
  }

  function renderLogs(view, logs) {
    var items = logs.slice().sort(function (a, b) {
      return (Number(b.ts) || 0) - (Number(a.ts) || 0);
    });
    view.logCount.textContent = String(items.length);

    var keepScroll = view.logScroll.scrollTop;
    clear(view.logScroll);

    if (!items.length) {
      view.logScroll.appendChild(el('div', { class: 'log-empty', text: '暂无同步日志' }));
      return;
    }

    items.forEach(function (log) {
      var level = ['info', 'warn', 'error'].indexOf(log.level) >= 0 ? log.level : 'info';
      view.logScroll.appendChild(el('div', { class: 'log-row level-' + level }, [
        el('span', { class: 'log-ts', text: formatDateTime(log.ts) }),
        el('span', { class: 'log-level', text: String(log.level || 'info') }),
        el('span', { class: 'log-kind', text: String(log.kind || '') }),
        el('span', { class: 'log-msg', text: String(log.msg || '') })
      ]));
    });

    // 自动刷新时保留用户滚动位置。
    view.logScroll.scrollTop = keepScroll;
  }

  /* ========================================================================
     7. 录像页：天卡片墙 + 播放器 + 24h 时间轴
     ======================================================================== */

  var recView = {
    /* ---- 天卡片墙 -------------------------------------------------------- */
    mountDays: function (root) {
      var view = { destroyed: false, seq: 0 };
      var channels = availableChannels();

      var ch = state.recordings.ch;
      if (!ch || channels.indexOf(ch) < 0) ch = channels[0];
      state.recordings.ch = ch;

      var chSelect = el('select', { class: 'control', id: 'rec-ch' },
        channels.map(function (value) {
          return el('option', { value: String(value), text: 'CAM ' + value });
        })
      );
      chSelect.value = String(ch);

      var refreshBtn = button('刷新', { icon: 'refresh', id: 'rec-refresh' });
      var gridHost = el('div');

      var page = el('div', { class: 'page' }, [
        pageHeader('录像', '已同步完成的录像按天归档；点击卡片即可在线回看。', [
          el('div', { class: 'field compact' }, [
            el('label', { for: 'rec-ch', text: '通道' }),
            chSelect
          ]),
          refreshBtn
        ]),
        gridHost
      ]);
      root.appendChild(page);

      chSelect.addEventListener('change', function () {
        ch = toInt(chSelect.value, channels[0]);
        state.recordings.ch = ch;
        load();
      });

      refreshBtn.addEventListener('click', function () {
        withBusy(refreshBtn, function () { return load(); });
      });

      function renderSkeleton() {
        clear(gridHost);
        var grid = el('div', { class: 'day-grid' });
        for (var i = 0; i < 6; i++) {
          grid.appendChild(el('div', { class: 'day-skeleton' }, [
            el('div', { class: 'sk', style: { width: '52%', height: '20px' } }),
            el('div', { class: 'sk', style: { width: '30%', height: '12px', marginTop: '9px' } }),
            el('div', { class: 'sk', style: { width: '100%', height: '62px', marginTop: '20px' } })
          ]));
        }
        gridHost.appendChild(grid);
      }

      async function load() {
        var seq = ++view.seq;
        renderSkeleton();
        var res = await api('/api/days?' + queryString({ ch: ch }));
        if (view.destroyed || seq !== view.seq) return;

        if (!res.ok) {
          clear(gridHost);
          gridHost.appendChild(statePanel({
            error: true,
            title: '录像列表加载失败',
            hint: res.error,
            actions: [button('重试', { icon: 'refresh', onClick: load })]
          }));
          return;
        }

        var days = Array.isArray(res.data) ? res.data.slice() : [];
        days.sort(function (a, b) {
          return String(b.date).localeCompare(String(a.date));
        });

        clear(gridHost);
        if (!days.length) {
          gridHost.appendChild(statePanel({
            title: '还没有已同步的录像',
            hint: '在同步页启动一次同步后，已完成的录像会按天汇总到这里。',
            actions: [el('a', {
              class: 'btn btn-primary',
              href: '#/sync'
            }, [icon('play'), el('span', { class: 'btn-label', text: '去同步' })])]
          }));
          return;
        }

        var grid = el('div', { class: 'day-grid' });
        days.forEach(function (day) { grid.appendChild(dayCard(day, ch)); });
        gridHost.appendChild(grid);
      }

      load();

      return function cleanup() {
        view.destroyed = true;
        view.seq++;
      };
    },

    /* ---- 播放器 ---------------------------------------------------------- */
    mountPlayer: function (root, chRaw, dateRaw) {
      var ch = toInt(chRaw, 0);
      var date = String(dateRaw || '');
      var bounds = dayBounds(date);

      if (ch <= 0 || !bounds) {
        var invalid = el('div', { class: 'page' }, [
          pageHeader('录像', null, null),
          statePanel({
            error: true,
            title: '无效的播放链接',
            hint: '请从录像页选择一天，或检查地址里的通道与日期。',
            actions: [button('返回录像', {
              icon: 'arrowLeft',
              onClick: function () { location.hash = '#/recordings'; }
            })]
          })
        ]);
        root.appendChild(invalid);
        return function () {};
      }

      state.recordings.ch = ch;

      var view = {
        destroyed: false,
        loadSeq: 0,
        hls: null,
        ctx: null,
        nowChipValue: null,
        nativeCleanup: null
      };

      var channels = availableChannels();
      if (channels.indexOf(ch) < 0) channels.push(ch);
      channels.sort(function (a, b) { return a - b; });

      var chSelect = el('select', { class: 'control', id: 'pl-ch' },
        channels.map(function (value) {
          return el('option', { value: String(value), text: 'CAM ' + value });
        })
      );
      chSelect.value = String(ch);
      chSelect.addEventListener('change', function () {
        location.hash = '#/recordings/' + chSelect.value + '/' + date;
      });

      var backBtn = el('button', {
        class: 'btn btn-icon btn-ghost',
        type: 'button',
        title: '返回天列表',
        'aria-label': '返回天列表'
      }, [icon('arrowLeft')]);
      backBtn.addEventListener('click', function () { location.hash = '#/recordings'; });

      var refreshBtn = button('重新加载', { icon: 'refresh', id: 'pl-refresh' });

      var subEl = el('p', { class: 'page-sub', text: '正在读取当天录像…' });
      var head = el('div', { class: 'page-head' }, [
        el('div', { class: 'head-left' }, [
          backBtn,
          el('div', {}, [
            el('h1', {}, [date, el('span', { class: 'weekday', text: weekdayName(date) })]),
            subEl
          ])
        ]),
        el('div', { class: 'head-actions' }, [
          el('div', { class: 'field compact' }, [
            el('label', { for: 'pl-ch', text: '通道' }),
            chSelect
          ]),
          refreshBtn
        ])
      ]);

      var video = el('video', {
        id: 'pl-video',
        controls: true,
        playsinline: true,
        preload: 'metadata'
      });
      var overlay = el('div', { class: 'video-overlay', id: 'pl-overlay' });
      var metaRow = el('div', { class: 'player-meta' });

      var playerCard = el('section', { class: 'card player-card' }, [
        el('div', { class: 'video-wrap' }, [video, overlay]),
        metaRow
      ]);

      var track = el('div', { class: 'timeline-track', id: 'pl-track' });
      var playhead = el('div', { class: 'timeline-playhead', hidden: true });
      var tip = el('div', { class: 'timeline-tip', hidden: true });
      var timeline = el('div', {
        class: 'timeline',
        id: 'pl-timeline',
        tabindex: '0',
        role: 'slider',
        'aria-label': '当天录像时间轴',
        'aria-valuemin': '0',
        'aria-valuemax': String(bounds.span),
        'aria-valuenow': '0'
      }, [track, playhead, tip]);

      var ticks = el('div', { class: 'timeline-ticks' });
      for (var hour = 0; hour <= 24; hour += 4) {
        ticks.appendChild(el('span', {
          class: 'tick',
          text: pad2(hour) + ':00',
          style: { left: (hour / 24) * 100 + '%' }
        }));
      }

      var legend = el('div', { class: 'timeline-legend' }, [
        legendItem('ok', '已同步'),
        legendItem('run', '同步中'),
        legendItem('wait', '待同步'),
        legendItem('bad', '失败'),
        el('span', { class: 'muted small', text: '点击时间轴可跳转播放位置' })
      ]);

      var timelineCard = el('section', { class: 'card timeline-card' }, [
        el('div', { class: 'card-head' }, [
          el('h2', {}, ['24 小时时间轴']),
          el('span', { class: 'muted small', text: date })
        ]),
        el('div', { class: 'timeline-wrap' }, [timeline, ticks]),
        legend,
        el('div', { class: 'player-help' }, [
          icon('alert'),
          el('span', {
            text: '浏览器可能不支持 HEVC/H.265 录像的解码。若画面黑屏或一直转圈，请改用 VLC 等播放器打开播放列表地址，或在设置页开启转码（需 ffmpeg）。'
          })
        ])
      ]);

      var page = el('div', { class: 'page' }, [head, el('div', { class: 'player-layout' }, [
        playerCard,
        timelineCard
      ])]);
      root.appendChild(page);

      view.video = video;
      view.overlay = overlay;
      view.track = track;
      view.playhead = playhead;
      view.tip = tip;
      view.timeline = timeline;
      view.metaRow = metaRow;

      /* ---- 播放器内部工具 ---- */

      function showOverlay(node) {
        clear(overlay);
        overlay.hidden = false;
        overlay.appendChild(node);
      }

      function hideOverlay() {
        overlay.hidden = true;
        clear(overlay);
      }

      function showLoadingOverlay() {
        showOverlay(el('div', { class: 'video-error' }, [
          el('div', { class: 'spinner' }),
          el('p', { text: '正在加载播放列表…' })
        ]));
      }

      function destroyHls() {
        if (view.nativeCleanup) {
          try { view.nativeCleanup(); } catch (err) { console.error(err); }
          view.nativeCleanup = null;
        }
        if (view.hls) {
          try { view.hls.destroy(); } catch (err) { console.error(err); }
          view.hls = null;
        }
        try { video.pause(); } catch (err) { /* ignore */ }
        if (video.getAttribute('src')) {
          video.removeAttribute('src');
          try { video.load(); } catch (err) { /* ignore */ }
        }
      }

      function playlistURL() {
        return '/api/playlist?ch=' + encodeURIComponent(String(ch)) + '&date=' + encodeURIComponent(date);
      }

      // 展示给 VLC 的绝对地址（hls.js 内部仍用相对路径）。
      function absolutePlaylistURL() {
        return location.origin + playlistURL();
      }

      function showPlaybackError(title, hint, detail) {
        var actions = [
          button('重新加载', { icon: 'refresh', onClick: function () { load(); } }),
          button('返回天列表', {
            icon: 'arrowLeft',
            onClick: function () { location.hash = '#/recordings'; }
          })
        ];
        showOverlay(el('div', { class: 'video-error' }, [
          el('div', { class: 've-icon' }, [icon('alert')]),
          el('h3', { text: title }),
          el('p', { text: hint }),
          el('p', {}, [el('code', { text: absolutePlaylistURL() })]),
          detail ? el('p', { class: 'small', text: '详细信息：' + detail }) : null,
          el('div', { class: 've-actions' }, actions)
        ]));
      }

      function showEmptyOverlay() {
        showOverlay(statePanel({
          title: '当天没有可播放的录像',
          hint: 'CAM ' + ch + ' 在 ' + date + ' 还没有已同步完成的录像段。' +
            '请先在同步页同步这一天，或切换到其它通道。',
          actions: [
            el('a', { class: 'btn btn-primary', href: '#/sync' },
              [icon('play'), el('span', { class: 'btn-label', text: '去同步' })]),
            button('返回天列表', {
              icon: 'arrowLeft',
              onClick: function () { location.hash = '#/recordings'; }
            })
          ]
        }));
      }

      function setupPlayer() {
        var src = playlistURL();
        var HlsCtor = window.Hls;

        if (HlsCtor && typeof HlsCtor.isSupported === 'function' && HlsCtor.isSupported()) {
          try {
            var hls = new HlsCtor({
              enableWorker: true,
              lowLatencyMode: false,
              backBufferLength: 90
            });
            view.hls = hls;
            var netRetries = 0;
            var mediaRetries = 0;

            hls.on(HlsCtor.Events.MEDIA_ATTACHED, function () {
              if (view.destroyed) return;
              hls.loadSource(src);
            });

            hls.on(HlsCtor.Events.MANIFEST_PARSED, function () {
              if (view.destroyed) return;
              hideOverlay();
              playhead.hidden = false;
            });

            hls.on(HlsCtor.Events.ERROR, function (event, data) {
              if (view.destroyed || !data) return;
              if (!data.fatal) return;

              // 先给一次自愈机会，再展示错误与 HEVC 提示。
              if (data.type === HlsCtor.ErrorTypes.NETWORK_ERROR && netRetries < 1) {
                netRetries++;
                hls.startLoad();
                return;
              }
              if (data.type === HlsCtor.ErrorTypes.MEDIA_ERROR && mediaRetries < 1) {
                mediaRetries++;
                hls.recoverMediaError();
                return;
              }

              var detail = data.details || data.reason || '';
              showPlaybackError(
                '无法播放这段录像',
                '浏览器可能不支持 HEVC/H.265 编码，或者该段录像文件不完整。' +
                '建议用 VLC 等支持 HEVC 的播放器打开下方播放列表地址，或在设置页开启转码（需 ffmpeg）。',
                String(detail)
              );
              destroyHls();
            });

            hls.attachMedia(video);
          } catch (err) {
            console.error(err);
            destroyHls();
            showPlaybackError(
              '播放器初始化失败',
              'hls.js 无法初始化。请刷新页面重试，或直接用 VLC 打开下方播放列表地址。',
              err && err.message ? String(err.message) : ''
            );
          }
          return;
        }

        // Safari 原生 HLS。
        if (video.canPlayType('application/vnd.apple.mpegurl')) {
          video.src = src;
          var onMeta = function () {
            if (view.destroyed) return;
            hideOverlay();
            playhead.hidden = false;
          };
          var onError = function () {
            if (view.destroyed) return;
            showPlaybackError(
              '无法播放这段录像',
              '浏览器无法解码这段录像。建议用 VLC 等支持 HEVC 的播放器打开下方播放列表地址。',
              ''
            );
          };
          video.addEventListener('loadedmetadata', onMeta);
          video.addEventListener('error', onError);
          view.nativeCleanup = function () {
            video.removeEventListener('loadedmetadata', onMeta);
            video.removeEventListener('error', onError);
          };
          return;
        }

        showPlaybackError(
          '浏览器不支持 HLS 播放',
          '请使用新版 Chrome / Edge / Safari，或者直接用 VLC 打开下方播放列表地址。',
          ''
        );
      }

      function renderTimeline(segments) {
        clear(track);
        segments.forEach(function (seg) {
          var start = Number(seg.start) || 0;
          var end = Number(seg.end) || 0;
          var left = clamp(((Math.max(start, bounds.from) - bounds.from) / bounds.span) * 100, 0, 100);
          var right = clamp(((Math.min(end, bounds.to) - bounds.from) / bounds.span) * 100, 0, 100);
          if (right <= left) return;

          var size = Number(seg.bytes) || Number(seg.size) || 0;
          var node = el('div', {
            class: 'tl-seg st-' + (seg.status || 'pending'),
            title: 'CAM ' + ch + ' ' + formatTime(start) + '–' + formatTime(end) +
              '\n状态：' + statusLabel(seg.status) +
              (size ? '\n大小：' + formatBytes(size) : '')
          });
          node.style.left = left + '%';
          node.style.width = Math.max(right - left, 0.08) + '%';
          track.appendChild(node);
        });
      }

      function renderMeta(segments, done) {
        clear(metaRow);
        var seconds = done.reduce(function (sum, seg) {
          return sum + Math.max(0, (Number(seg.end) || 0) - (Number(seg.start) || 0));
        }, 0);
        var bytes = done.reduce(function (sum, seg) {
          return sum + (Number(seg.bytes) || Number(seg.size) || 0);
        }, 0);

        var nowChip = chip('当前', '--:--:--', true);
        view.nowChipValue = nowChip.querySelector('b');

        metaRow.appendChild(chip('通道', 'CAM ' + ch));
        metaRow.appendChild(chip('已同步', done.length + ' / ' + segments.length + ' 段'));
        metaRow.appendChild(chip('时长', formatDuration(seconds)));
        metaRow.appendChild(chip('大小', formatBytes(bytes)));
        metaRow.appendChild(nowChip);
      }

      function mediaAt(media, time) {
        var items = media.items;
        if (!items.length) return null;
        if (time <= 0) return items[0];
        for (var i = 0; i < items.length; i++) {
          if (time < items[i].cum + items[i].dur) return items[i];
        }
        return items[items.length - 1];
      }

      function onTimeUpdate() {
        var ctx = view.ctx;
        if (!ctx) return;
        var time = Number(video.currentTime) || 0;
        var item = mediaAt(ctx.media, time);
        if (!item) {
          playhead.hidden = true;
          return;
        }
        var wall = item.seg.start + clamp(time - item.cum, 0, item.dur);
        var frac = clamp((wall - bounds.from) / bounds.span, 0, 1);
        playhead.hidden = false;
        playhead.style.left = frac * 100 + '%';
        timeline.setAttribute('aria-valuenow', String(Math.round(wall - bounds.from)));
        if (view.nowChipValue) view.nowChipValue.textContent = formatTime(wall);
      }

      function seekToWall(wall) {
        var ctx = view.ctx;
        if (!ctx) return null;
        var item = null;
        for (var i = 0; i < ctx.media.items.length; i++) {
          var candidate = ctx.media.items[i];
          if (wall >= candidate.seg.start && wall < candidate.seg.end) {
            item = candidate;
            break;
          }
        }
        if (item) {
          var target = item.cum + (wall - item.seg.start);
          target = clamp(target, 0, Math.max(0, ctx.media.total - 0.1));
          if (video.readyState >= 1) {
            try { video.currentTime = target; } catch (err) { /* ignore */ }
          } else {
            toast('播放器还在加载，请稍候再跳转', 'info');
          }
          return item;
        }

        var seg = ctx.segments.filter(function (s) {
          return wall >= s.start && wall < s.end;
        })[0];
        if (seg) {
          toast('该时段录像状态为「' + statusLabel(seg.status) + '」，暂时无法播放', 'info');
        } else {
          toast('该时段没有录像', 'info');
        }
        return null;
      }

      function wallFromEvent(event) {
        var rect = timeline.getBoundingClientRect();
        if (!rect.width) return bounds.from;
        var frac = clamp((event.clientX - rect.left) / rect.width, 0, 1);
        return bounds.from + frac * bounds.span;
      }

      /* ---- 时间轴交互 ---- */

      timeline.addEventListener('click', function (event) {
        if (!view.ctx) return;
        seekToWall(wallFromEvent(event));
      });

      timeline.addEventListener('pointermove', function (event) {
        if (!view.ctx) return;
        var rect = timeline.getBoundingClientRect();
        if (!rect.width) return;
        var frac = clamp((event.clientX - rect.left) / rect.width, 0, 1);
        var wall = bounds.from + frac * bounds.span;
        var seg = view.ctx.segments.filter(function (s) {
          return wall >= s.start && wall < s.end;
        })[0];

        clear(tip);
        tip.appendChild(el('b', { text: formatTime(wall) }));
        tip.appendChild(document.createTextNode(' CAM ' + ch + ' · ' + (seg ? statusLabel(seg.status) : '无录像')));
        tip.style.left = clamp(frac * 100, 5, 95) + '%';
        tip.hidden = false;
      });

      timeline.addEventListener('pointerleave', function () {
        tip.hidden = true;
      });

      timeline.addEventListener('keydown', function (event) {
        if (!view.ctx) return;
        var key = event.key;
        if (key === 'ArrowLeft' || key === 'ArrowRight') {
          event.preventDefault();
          var delta = key === 'ArrowLeft' ? -60 : 60;
          video.currentTime = Math.max(0, Math.min(
            (Number(video.currentTime) || 0) + delta,
            Math.max(0, view.ctx.media.total - 0.1)
          ));
        } else if (key === 'Home') {
          event.preventDefault();
          video.currentTime = 0;
        } else if (key === 'End') {
          event.preventDefault();
          video.currentTime = Math.max(0, view.ctx.media.total - 0.1);
        }
      });

      video.addEventListener('timeupdate', onTimeUpdate);

      /* ---- 加载 ---- */

      function buildMediaMap(done) {
        var items = [];
        var cum = 0;
        done.forEach(function (seg) {
          var dur = (Number(seg.end) || 0) - (Number(seg.start) || 0);
          if (dur <= 0) return;
          items.push({ seg: seg, cum: cum, dur: dur });
          cum += dur;
        });
        return { items: items, total: cum };
      }

      async function load() {
        var seq = ++view.loadSeq;
        view.ctx = null;
        destroyHls();
        clear(track);
        clear(metaRow);
        clear(tip);
        playhead.hidden = true;
        subEl.textContent = '正在读取当天录像…';
        showLoadingOverlay();

        var res = await fetchAllSegments({
          ch: ch,
          from: bounds.from,
          to: bounds.to
        });
        if (view.destroyed || seq !== view.loadSeq) return;

        if (!res.ok) {
          subEl.textContent = '录像信息加载失败';
          showPlaybackError('录像列表加载失败', res.error, '');
          return;
        }

        var all = (res.data.items || []).filter(function (seg) {
          return Number(seg.ch) === ch && Number(seg.start) >= bounds.from && Number(seg.start) < bounds.to;
        });
        all.sort(function (a, b) {
          return (Number(a.start) - Number(b.start)) || (Number(a.end) - Number(b.end));
        });

        var done = all.filter(function (seg) {
          return seg.status === 'done' && Number(seg.end) > Number(seg.start);
        });

        renderTimeline(all);
        renderMeta(all, done);
        subEl.textContent = 'CAM ' + ch + ' · 已同步 ' + done.length + ' / ' + all.length + ' 段 · ' +
          formatDuration(done.reduce(function (sum, seg) {
            return sum + (Number(seg.end) - Number(seg.start));
          }, 0));

        if (!done.length) {
          showEmptyOverlay();
          return;
        }

        var media = buildMediaMap(done);
        if (!media.items.length) {
          showEmptyOverlay();
          return;
        }

        view.ctx = { segments: all, done: done, media: media };
        setupPlayer();
      }

      refreshBtn.addEventListener('click', function () {
        withBusy(refreshBtn, function () { return load(); });
      });

      load();

      return function cleanup() {
        view.destroyed = true;
        view.loadSeq++;
        destroyHls();
        view.ctx = null;
      };
    }
  };

  function legendItem(tone, label) {
    return el('span', { class: 'lg' }, [
      el('span', { class: 'legend-dot ' + tone }),
      document.createTextNode(label)
    ]);
  }

  function dayCard(day, ch) {
    var date = String(day.date || '');
    var card = el('button', { class: 'day-card', type: 'button' }, [
      el('div', { class: 'day-top' }, [
        el('span', { class: 'day-date', text: date }),
        el('span', { class: 'tag accent', text: weekdayName(date) })
      ]),
      el('div', { class: 'day-weekday', text: 'CAM ' + ch }),
      el('div', { class: 'day-meta' }, [
        metaCell('段数', num(day.segments)),
        metaCell('时长', formatDuration(day.seconds)),
        metaCell('大小', formatBytes(day.bytes)),
        metaCell('码率', formatBitrate(day.bytes, day.seconds))
      ]),
      el('span', { class: 'day-arrow' }, [icon('arrowRight')])
    ]);
    card.addEventListener('click', function () {
      state.recordings.ch = ch;
      location.hash = '#/recordings/' + ch + '/' + date;
    });
    return card;
  }

  /* ========================================================================
     8. 设置页
     ======================================================================== */

  var settingsView = {
    mount: function (root) {
      var view = { destroyed: false };
      var host = el('div');
      var page = el('div', { class: 'page' }, [
        pageHeader('设置', '保存后立即生效；NVR 地址、存储目录和同步通道是同步工作的前提。'),
        host
      ]);
      root.appendChild(page);

      function load() {
        clear(host);
        host.appendChild(loadingPanel('正在读取设置…'));
        api('/api/settings').then(function (res) {
          if (view.destroyed) return;
          if (!res.ok) {
            // 401 由全局 api() 统一跳转登录页，不再渲染错误面板。
            if (res.unauthorized) return;
            clear(host);
            host.appendChild(statePanel({
              error: true,
              title: '设置读取失败',
              hint: res.error,
              actions: [button('重试', { icon: 'refresh', onClick: load })]
            }));
            return;
          }
          var settings = normalizeSettings(res.data);
          state.settings = settings;
          state.channels = channelsFromSettings(settings);
          clear(host);
          host.appendChild(buildSettingsForm(settings));
        });
      }

      load();

      return function cleanup() { view.destroyed = true; };
    }
  };

  /** 生成设置表单并绑定保存 / 测试连接 / 密码可见性。 */
  function buildSettingsForm(saved) {
    var form = el('form', { class: 'settings-form', id: 'settings-form', novalidate: true });

    /* ---- 基础输入 ---- */
    var hostInput = el('input', {
      class: 'control', id: 'set-host', type: 'text', value: saved.nvr_host,
      placeholder: '192.168.0.49:8000', autocomplete: 'off', spellcheck: 'false'
    });

    var portInput = el('input', {
      class: 'control', id: 'set-port', type: 'number', value: String(saved.admin_port),
      min: '0', max: '65535', step: '1'
    });

    var userInput = el('input', {
      class: 'control', id: 'set-user', type: 'text', value: saved.nvr_user,
      autocomplete: 'username', spellcheck: 'false'
    });

    var passInput = el('input', {
      class: 'control', id: 'set-pass', type: 'password', value: saved.nvr_pass,
      autocomplete: 'current-password'
    });

    var eyeBtn = el('button', {
      class: 'eye-btn', type: 'button', 'aria-label': '显示密码', 'aria-pressed': 'false'
    }, [icon('eye')]);
    eyeBtn.addEventListener('click', function () {
      var show = passInput.type === 'password';
      passInput.type = show ? 'text' : 'password';
      eyeBtn.setAttribute('aria-label', show ? '隐藏密码' : '显示密码');
      eyeBtn.setAttribute('aria-pressed', String(show));
      clear(eyeBtn);
      eyeBtn.appendChild(icon(show ? 'eyeOff' : 'eye'));
    });

    var storageInput = el('input', {
      class: 'control', id: 'set-storage', type: 'text', value: saved.storage_dir,
      placeholder: '/mnt/nvr/records', autocomplete: 'off', spellcheck: 'false'
    });

    var autoInput = el('input', {
      class: 'control', id: 'set-auto', type: 'number', value: String(saved.auto_minutes),
      min: '0', step: '1'
    });

    var catalogInput = el('input', {
      class: 'control', id: 'set-catalog', type: 'number', value: String(saved.catalog_days),
      min: '0', step: '1'
    });

    var catalogIntervalInput = el('input', {
      class: 'control', id: 'set-catalog-interval', type: 'number',
      value: String(saved.catalog_minutes), min: '0', step: '1'
    });

    var cutoffSelect = el('select', { class: 'control', id: 'set-cutoff' }, [
      el('option', { value: 'latest' }, ['最新（约 2 分钟前）']),
      el('option', { value: 'yesterday' }, ['昨天 23:59']),
      el('option', { value: 'days' }, ['N 天前'])
    ]);
    cutoffSelect.value = saved.cutoff || 'latest';

    var cutoffDaysInput = el('input', {
      class: 'control', id: 'set-cutoff-days', type: 'number',
      value: String(saved.cutoff_days > 0 ? saved.cutoff_days : 1), min: '1', step: '1'
    });
    var cutoffDaysField = field('截止天数', cutoffDaysInput, {
      id: 'set-cutoff-days',
      hint: '同步到 N 天前的 00:00，只处理已完整的录像。'
    });

    function syncCutoffFields() {
      var byDays = cutoffSelect.value === 'days';
      cutoffDaysInput.disabled = !byDays;
      cutoffDaysField.classList.toggle('is-disabled', !byDays);
    }

    var retentionInput = el('input', {
      class: 'control', id: 'set-retention', type: 'number', value: String(saved.retention_days),
      min: '0', step: '1'
    });

    var ffmpegInput = el('input', {
      class: 'control', id: 'set-ffmpeg', type: 'text', value: saved.ffmpeg,
      placeholder: 'ffmpeg', autocomplete: 'off', spellcheck: 'false'
    });
    var ffmpegField = field('FFmpeg 路径', ffmpegInput, {
      id: 'set-ffmpeg',
      hint: '转码时使用的可执行文件；留空则按 PATH 查找 ffmpeg。'
    });
    ffmpegField.classList.add('ffmpeg-field');

    var switchInput = el('input', {
      type: 'checkbox', id: 'set-transcode', checked: saved.transcode
    });
    var switchStateLabel = el('span', { text: saved.transcode ? '已启用' : '已关闭' });
    var switchRow = el('div', { class: 'span-2 switch-row' }, [
      el('div', { class: 'switch-copy' }, [
        el('div', { class: 'switch-title', text: '转码为 H.264 代理' }),
        el('div', {
          class: 'switch-hint',
          text: '浏览器无法直接播放 HEVC 时，用 ffmpeg 生成 H.264 代理（需要服务端支持与可用的 ffmpeg）。'
        })
      ]),
      el('label', { class: 'switch', for: 'set-transcode' }, [
        switchInput,
        el('span', { class: 'track' }),
        switchStateLabel
      ])
    ]);

    function syncSwitch() {
      var enabled = switchInput.checked;
      switchStateLabel.textContent = enabled ? '已启用' : '已关闭';
      ffmpegField.classList.toggle('is-disabled', !enabled);
      ffmpegInput.disabled = !enabled;
    }

    /* ---- 通道多选 ---- */
    var savedChannels = saved.channels.slice();
    var highest = 8;
    savedChannels.concat(state.channels).forEach(function (ch) {
      if (ch > highest) highest = ch;
    });

    var chGrid = el('div', { class: 'ch-grid' });
    for (var i = 1; i <= highest; i++) {
      var checkbox = el('input', {
        type: 'checkbox',
        value: String(i),
        id: 'set-ch-' + i,
        checked: savedChannels.indexOf(i) >= 0
      });
      chGrid.appendChild(el('label', { class: 'ch-option', for: 'set-ch-' + i }, [
        checkbox,
        el('span', { text: String(i) })
      ]));
    }

    var channelsField = el('div', { class: 'field span-2' }, [
      el('label', { text: '同步通道' }),
      chGrid,
      el('div', {
        class: 'hint',
        text: '勾选要同步 / 回看的通道。手动同步与自动同步都会使用这组通道。'
      })
    ]);

    var savedCatalogChannels = (saved.catalog_channels || []).slice();
    var catalogHighest = highest;
    savedCatalogChannels.forEach(function (ch) {
      if (ch > catalogHighest) catalogHighest = ch;
    });
    var catalogGrid = el('div', { class: 'ch-grid', id: 'set-cat-grid' });
    for (var c = 1; c <= catalogHighest; c++) {
      var catalogBox = el('input', {
        type: 'checkbox',
        value: String(c),
        id: 'set-catch-' + c,
        checked: savedCatalogChannels.indexOf(c) >= 0
      });
      catalogGrid.appendChild(el('label', { class: 'ch-option', for: 'set-catch-' + c }, [
        catalogBox,
        el('span', { text: String(c) })
      ]));
    }
    var catalogChannelsField = el('div', { class: 'field span-2' }, [
      el('label', { text: '对账通道' }),
      catalogGrid,
      el('div', {
        class: 'hint',
        text: '不勾选表示与「同步通道」一致；只影响后台对账，不影响下载。'
      })
    ]);

    /* ---- 卡片 ---- */
    var connectionCard = el('section', { class: 'card form-card' }, [
      el('div', { class: 'card-head' }, [
        el('h2', {}, ['NVR 连接']),
        el('span', { class: 'muted', text: '登录并读取录像列表' })
      ]),
      el('div', { class: 'form-grid' }, [
        field('NVR 地址', hostInput, {
          id: 'set-host',
          hint: 'IP:端口，例如 192.168.0.49:8000（录像下载端口）。'
        }),
        field('管理端口', portInput, {
          id: 'set-port',
          hint: '管理 API 端口，通常为 80；0 表示自动尝试 80 / 8081 / 8080。'
        }),
        field('用户名', userInput, { id: 'set-user' }),
        field('密码', el('div', { class: 'input-group' }, [passInput, eyeBtn]), { id: 'set-pass' }),
        field('存储目录', storageInput, {
          id: 'set-storage',
          span: true,
          hint: '录像落盘的根目录，需要可写；修改后只影响后续同步与查找。'
        })
      ])
    ]);

    var policyCard = el('section', { class: 'card form-card' }, [
      el('div', { class: 'card-head' }, [
        el('h2', {}, ['同步与存储']),
        el('span', { class: 'muted', text: '频率、保留与转码策略' })
      ]),
      el('div', { class: 'form-grid' }, [
        field('自动同步间隔（分钟）', autoInput, {
          id: 'set-auto',
          hint: '0 表示关闭自动同步。'
        }),
        field('同步截止', cutoffSelect, {
          id: 'set-cutoff',
          hint: '只同步到此时间之前。「昨天」表示今天的录像等明天再同步，永远不会碰到正在写入的段。'
        }),
        cutoffDaysField,
        field('保留天数', retentionInput, {
          id: 'set-retention',
          hint: '0 表示不自动删除旧录像。'
        }),
        ffmpegField,
        switchRow,
        channelsField
      ])
    ]);

    var catalogCard = el('section', { class: 'card form-card' }, [
      el('div', { class: 'card-head' }, [
        el('h2', {}, ['索引（对账）']),
        el('span', { class: 'muted', text: '把 NVR 上仍存在的录像合并进列表' })
      ]),
      el('div', { class: 'form-grid' }, [
        field('对账回看（天）', catalogInput, {
          id: 'set-catalog',
          hint: '每次都往回查这么多天的 NVR 列表；0 表示关闭自动对账。'
        }),
        field('对账间隔（分钟）', catalogIntervalInput, {
          id: 'set-catalog-interval',
          hint: '后台多久对账一次，0 表示使用默认 5 分钟。'
        }),
        catalogChannelsField
      ])
    ]);

    /* ---- 账户安全 ---- */
    var authSwitchInput = el('input', {
      type: 'checkbox', id: 'set-auth-enabled', checked: saved.auth_enabled
    });
    var authStateLabel = el('span', { text: saved.auth_enabled ? '已启用' : '已关闭' });
    var authSwitchRow = el('div', { class: 'span-2 switch-row' }, [
      el('div', { class: 'switch-copy' }, [
        el('div', { class: 'switch-title', text: '启用登录保护' }),
        el('div', {
          class: 'switch-hint',
          text: '开启后访问页面与 API 需要先登录；关闭时行为与之前完全一致。'
        })
      ]),
      el('label', { class: 'switch', for: 'set-auth-enabled' }, [
        authSwitchInput,
        el('span', { class: 'track' }),
        authStateLabel
      ])
    ]);

    var authUserInput = el('input', {
      class: 'control', id: 'set-auth-user', type: 'text',
      value: saved.auth_user || 'admin', autocomplete: 'username', spellcheck: 'false'
    });
    var authOldInput = el('input', {
      class: 'control', id: 'set-auth-old', type: 'password', value: '',
      autocomplete: 'current-password'
    });
    var authNewInput = el('input', {
      class: 'control', id: 'set-auth-new', type: 'password', value: '',
      autocomplete: 'new-password'
    });
    var authConfirmInput = el('input', {
      class: 'control', id: 'set-auth-confirm', type: 'password', value: '',
      autocomplete: 'new-password'
    });
    var passwordBtn = button('设置新密码', {
      icon: 'check',
      id: 'set-auth-save',
      title: '设置或修改登录密码'
    });

    var authFields = [
      authSwitchRow,
      field('用户名', authUserInput, {
        id: 'set-auth-user',
        hint: '默认 admin；修改后需要用新用户名登录。'
      })
    ];
    if (saved.auth_enabled) {
      authFields.push(field('当前密码', authOldInput, {
        id: 'set-auth-old',
        hint: '修改密码前需要验证当前密码。'
      }));
    }
    authFields.push(
      field('新密码', authNewInput, {
        id: 'set-auth-new',
        hint: '最长 72 字节；建议使用 8 位以上的独立密码。'
      }),
      field('确认新密码', authConfirmInput, { id: 'set-auth-confirm' }),
      el('div', { class: 'field span-2' }, [
        passwordBtn,
        el('div', {
          class: 'hint',
          text: '启用登录保护后需要重新登录。忘记密码时可以删除 config.json 里的 auth_hash，' +
            '或以 -reset-auth 参数启动 nvrsync。'
        })
      ])
    );

    var authCard = el('section', { class: 'card form-card' }, [
      el('div', { class: 'card-head' }, [
        el('h2', {}, ['账户安全']),
        el('span', { class: 'muted', text: '登录保护与密码' })
      ]),
      el('div', { class: 'form-grid' }, authFields)
    ]);

    function syncAuthSwitch() {
      authStateLabel.textContent = authSwitchInput.checked ? '已启用' : '已关闭';
    }

    var hint = el('span', { class: 'save-hint', text: '所有修改已保存' });
    var testBtn = button('测试连接', {
      icon: 'server',
      id: 'set-test',
      title: '使用已保存的设置登录 NVR 并读取一段录像'
    });
    var saveBtn = button('保存', {
      icon: 'check',
      variant: 'btn-primary',
      id: 'set-save',
      title: '保存设置（立即生效）'
    });

    var actions = el('div', { class: 'form-actions' }, [
      hint,
      el('div', { class: 'actions' }, [testBtn, saveBtn])
    ]);

    form.appendChild(connectionCard);
    form.appendChild(policyCard);
    form.appendChild(catalogCard);
    form.appendChild(authCard);
    form.appendChild(actions);

    var baseline = JSON.stringify(collectSettings(form));

    function refreshDirty() {
      var dirty = JSON.stringify(collectSettings(form)) !== baseline;
      hint.classList.toggle('dirty', dirty);
      hint.textContent = dirty ? '有未保存的修改' : '所有修改已保存';
    }

    function markSaved() {
      baseline = JSON.stringify(collectSettings(form));
      hint.classList.remove('dirty');
      hint.textContent = '所有修改已保存';
    }

    async function persist() {
      var settings = collectSettings(form);
      var invalid = validateSettings(settings);
      if (invalid) {
        toast(invalid, 'error');
        return { ok: false, error: invalid };
      }
      var authWasEnabled = !!(state.settings && state.settings.auth_enabled);
      var res = await api('/api/settings', { method: 'PUT', body: settings });
      if (res.ok) {
        var normalized = normalizeSettings(res.data || settings);
        state.settings = normalized;
        state.channels = channelsFromSettings(normalized);
        markSaved();
        if (normalized.auth_enabled && !authWasEnabled) {
          // 登录保护刚开启：当前会话不属于登录状态，下一步请求会 401。
          res.authJustEnabled = true;
          toast('已启用登录保护，请重新登录', 'success');
          setTimeout(function () { location.replace('/login'); }, 700);
        }
      }
      return res;
    }

    // 当前服务端是否已启用登录保护（保存后会更新）。
    function authEnabledNow() {
      return !!(state.settings && state.settings.auth_enabled);
    }

    form.addEventListener('input', refreshDirty);
    form.addEventListener('change', function (event) {
      if (event.target === switchInput) syncSwitch();
      if (event.target === cutoffSelect) syncCutoffFields();
      if (event.target === authSwitchInput) syncAuthSwitch();
      refreshDirty();
    });

    switchInput.addEventListener('change', syncSwitch);
    syncCutoffFields();
    syncAuthSwitch();

    saveBtn.addEventListener('click', function () {
      withBusy(saveBtn, async function () {
        var res = await persist();
        if (res.ok && !res.authJustEnabled) toast('设置已保存', 'success');
      });
    });

    passwordBtn.addEventListener('click', function () {
      withBusy(passwordBtn, async function () {
        var newPassword = authNewInput.value;
        if (!newPassword) {
          toast('请输入新密码', 'error');
          return;
        }
        if (newPassword !== authConfirmInput.value) {
          toast('两次输入的新密码不一致', 'error');
          return;
        }
        var enabled = authEnabledNow();
        var body = {
          user: String(authUserInput.value || '').trim() || 'admin',
          new_password: newPassword
        };
        if (enabled) body.old_password = authOldInput.value;
        var res = await api('/api/auth/password', { method: 'POST', body: body });
        if (!res.ok) return;
        authOldInput.value = '';
        authNewInput.value = '';
        authConfirmInput.value = '';
        refreshDirty();
        if (enabled) {
          // 服务端已重置全部会话，当前页面必须重新登录。
          toast('密码已更新，请重新登录', 'success');
          setTimeout(function () { location.replace('/login'); }, 700);
        } else {
          toast('密码已设置，打开「启用登录保护」并保存后生效', 'success', 6500);
        }
      });
    });

    testBtn.addEventListener('click', function () {
      withBusy(testBtn, async function () {
        // /api/nvr/test 使用服务端已保存的设置，所以先保存未提交的修改。
        var dirty = JSON.stringify(collectSettings(form)) !== baseline;
        if (dirty) {
          var saved = await persist();
          if (!saved.ok) return;
          if (saved.authJustEnabled) return;
          toast('设置已保存，正在测试连接…', 'info');
        }
        var res = await api('/api/nvr/test', { method: 'POST' });
        if (res.ok) {
          var data = res.data || {};
          toast(
            '连接成功：CAM ' + (data.ch || '?') +
            '，最近 10 分钟 ' + (data.segments == null ? '?' : num(data.segments)) + ' 段',
            'success',
            6000
          );
        }
      });
    });

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      if (!saveBtn.disabled) saveBtn.click();
    });

    syncSwitch();
    return form;
  }

  function collectSettings(form) {
    function value(id) {
      var node = form.querySelector('#' + id);
      return node ? node.value : '';
    }

    var channels = $$('.ch-option input:checked', form)
      .map(function (input) { return toInt(input.value, 0); })
      .filter(function (n) { return n > 0; })
      .sort(function (a, b) { return a - b; });

    return {
      nvr_host: String(value('set-host')).trim(),
      nvr_user: String(value('set-user')).trim(),
      nvr_pass: String(value('set-pass')),
      admin_port: toInt(value('set-port'), 0),
      storage_dir: String(value('set-storage')).trim(),
      channels: channels,
      auto_minutes: toInt(value('set-auto'), 0),
      retention_days: toInt(value('set-retention'), 0),
      transcode: !!(form.querySelector('#set-transcode') || {}).checked,
      ffmpeg: String(value('set-ffmpeg')).trim(),
      cutoff: String(value('set-cutoff')).trim() || 'latest',
      cutoff_days: toInt(value('set-cutoff-days'), 1),
      catalog_days: toInt(value('set-catalog'), 0),
      catalog_minutes: toInt(value('set-catalog-interval'), 0),
      catalog_channels: $$('#set-cat-grid input:checked', form)
        .map(function (input) { return toInt(input.value, 0); })
        .filter(function (n) { return n > 0; })
        .sort(function (a, b) { return a - b; }),
      auth_enabled: !!(form.querySelector('#set-auth-enabled') || {}).checked,
      // 绝不包含 auth_hash：服务端只接受通过 /api/auth/password 设置密码。
      auth_user: String(value('set-auth-user')).trim() || 'admin'
    };
  }

  function validateSettings(settings) {
    if (settings.admin_port < 0 || settings.admin_port > 65535) {
      return '管理端口需在 0–65535 之间';
    }
    if (settings.auto_minutes < 0) return '自动同步间隔不能为负数';
    if (settings.retention_days < 0) return '保留天数不能为负数';
    if (settings.cutoff === 'days' && settings.cutoff_days < 1) return '截止天数至少为 1';
    if (settings.catalog_days < 0) return '对账范围不能为负数';
    if (settings.catalog_minutes < 0) return '对账间隔不能为负数';
    if (settings.auth_user.length > 64) return '用户名不能超过 64 个字符';
    return '';
  }

  /* ========================================================================
     9. Hash 路由
     ======================================================================== */

  var activeView = null;
  var booted = false;

  function parseRoute() {
    var raw = String(location.hash || '').replace(/^#/, '');
    if (raw.charAt(0) === '/') raw = raw.slice(1);

    var path = raw.split('?')[0];
    var parts = [];
    try {
      parts = path.split('/').filter(Boolean).map(decodeURIComponent);
    } catch (err) {
      parts = path.split('/').filter(Boolean);
    }

    var name = parts[0] || 'sync';
    if (['sync', 'recordings', 'settings'].indexOf(name) < 0) {
      name = 'sync';
      parts = ['sync'];
    }
    return { name: name, parts: parts };
  }

  function route() {
    var r = parseRoute();

    // 顶部 tab 高亮
    $$('.tab').forEach(function (tab) {
      var active = tab.getAttribute('data-route') === r.name;
      tab.classList.toggle('active', active);
      if (active) tab.setAttribute('aria-current', 'page');
      else tab.removeAttribute('aria-current');
    });

    // 标题：播放器额外带上日期
    var player = r.name === 'recordings' && r.parts.length >= 3;
    if (player) {
      document.title = r.parts[2] + ' · 录像 · NVR Sync';
    } else {
      document.title = (ROUTE_TITLES[r.name] || 'NVR Sync') + ' · NVR Sync';
    }

    if (activeView && typeof activeView.cleanup === 'function') {
      try { activeView.cleanup(); } catch (err) { console.error(err); }
    }

    var app = document.getElementById('app');
    clear(app);
    var cleanup = null;

    if (r.name === 'recordings') {
      cleanup = player
        ? recView.mountPlayer(app, r.parts[1], r.parts[2])
        : recView.mountDays(app);
    } else if (r.name === 'settings') {
      cleanup = settingsView.mount(app);
    } else {
      cleanup = syncView.mount(app);
    }

    activeView = { route: r, cleanup: cleanup };

    try { window.scrollTo(0, 0); } catch (err) { /* ignore */ }
  }

  window.addEventListener('hashchange', function () {
    if (booted) route();
  });

  // 跳转链接 (#app) 不能交给 hash 路由，否则会被当成一次页面切换。
  var skipLink = document.querySelector('.skip-link');
  if (skipLink) {
    skipLink.addEventListener('click', function (event) {
      var app = document.getElementById('app');
      if (!app) return;
      event.preventDefault();
      app.focus();
      try { app.scrollIntoView({ block: 'start' }); } catch (err) { /* ignore */ }
    });
  }

  /* ========================================================================
     10. 启动
     ======================================================================== */

  async function boot() {
    state.filters = defaultFilters();

    var app = document.getElementById('app');
    clear(app);
    app.appendChild(loadingPanel('正在加载…'));

    // 通道列表来自设置；失败时使用 1..8 兜底，页面仍然可用。
    var res = await api('/api/settings', { quiet: true });
    if (res.unauthorized) {
      // 已开始跳转登录页，不再渲染骨架或弹错误 toast。
      return;
    }
    if (res.ok && res.data) {
      state.settings = normalizeSettings(res.data);
      state.channels = channelsFromSettings(res.data);
    } else {
      state.settings = null;
      state.channels = FALLBACK_CHANNELS.slice();
      if (res.error) toast('读取设置失败：' + res.error + '（通道暂时使用 1–8）', 'error', 6500);
    }

    if (!location.hash) {
      try {
        history.replaceState(null, '', '#/sync');
      } catch (err) {
        location.hash = '#/sync';
      }
    }

    booted = true;
    route();
    startStatusPolling();
  }

  boot();
})();
