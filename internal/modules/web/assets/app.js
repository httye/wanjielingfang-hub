/* hub 控制台共用脚本：API 封装、格式化、表格/抽屉/提示、hash 路由 */
(function () {
  'use strict';

  // ---------------------------------------------------------------- API
  async function call(method, path, body) {
    var opt = { method: method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    var res = await fetch(path, opt);
    var text = await res.text();
    var data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = text; }
    if (res.status === 401) {
      var back = encodeURIComponent(location.pathname + location.hash);
      location.href = '/auth/login?redirect=' + back;
      throw new Error('请先登录');
    }
    if (!res.ok) {
      var msg = (data && (data.message || data.error)) || ('HTTP ' + res.status);
      var err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  var API = {
    get: function (p) { return call('GET', p); },
    post: function (p, b) { return call('POST', p, b === undefined ? {} : b); },
    put: function (p, b) { return call('PUT', p, b); },
    del: function (p) { return call('DELETE', p); }
  };

  // ---------------------------------------------------------------- DOM
  function $(sel, root) { return (root || document).querySelector(sel); }
  function $$(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // ---------------------------------------------------------------- 格式化
  function pad(n) { return String(n).padStart(2, '0'); }

  function fmtTime(unix) {
    if (!unix) return '—';
    var d = new Date(unix * 1000);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  function ago(unix) {
    if (!unix) return '—';
    var s = Math.floor(Date.now() / 1000) - unix;
    if (s < 0) return '刚刚';
    if (s < 60) return s + ' 秒前';
    if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
    if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
    return Math.floor(s / 86400) + ' 天前';
  }

  function fmtNum(n) {
    if (n == null) return '—';
    return Number(n).toLocaleString('zh-CN');
  }

  // ---------------------------------------------------------------- 徽章
  var TASK_STATE = {
    pending: ['wait', '排队中'],
    running: ['run', '构建中'],
    done: ['ok', '已完成'],
    failed: ['err', '失败']
  };

  function taskBadge(st) {
    var s = TASK_STATE[st] || ['gray', st || '—'];
    return '<span class="badge ' + s[0] + '">' + esc(s[1]) + '</span>';
  }

  function statusText(st) {
    return ({ pending: '排队中', running: '构建中', done: '已完成', failed: '失败' })[st] || st || '—';
  }

  function channelText(ch) {
    return ({ usdt: 'USDT', plan: '套餐', cdk: 'CDK', alipay: '支付宝' })[ch] || ch || '—';
  }

  var PAY_STATE = {
    pending: ['wait', '待确认'],
    confirmed: ['ok', '已入账'],
    cancelled: ['gray', '已取消'],
    refunded: ['err', '已退款']
  };

  function payBadge(st) {
    var s = PAY_STATE[st] || ['gray', st || '—'];
    return '<span class="badge ' + s[0] + '">' + esc(s[1]) + '</span>';
  }

  var SUB_STATE = {
    active: ['ok', '生效中'],
    paused: ['warn', '已暂停'],
    cancelled: ['gray', '已取消']
  };

  function subBadge(st) {
    var s = SUB_STATE[st] || ['gray', st || '—'];
    return '<span class="badge ' + s[0] + '">' + esc(s[1]) + '</span>';
  }

  function kindLabel(k) {
    return { generate: '生成', iterate: '迭代', adapt: '适配', retry: '重试' }[k] || (k || '—');
  }

  // ---------------------------------------------------------------- 提示
  function toast(msg, kind) {
    var box = $('.toast');
    if (!box) {
      box = document.createElement('div');
      box.className = 'toast';
      document.body.appendChild(box);
    }
    var item = document.createElement('div');
    item.className = 'item ' + (kind || '');
    item.textContent = msg;
    box.appendChild(item);
    setTimeout(function () { item.remove(); }, 3400);
  }

  // ---------------------------------------------------------------- 抽屉
  function openDrawer(title, html) {
    var mask = $('.drawer-mask');
    if (!mask) {
      mask = document.createElement('div');
      mask.className = 'drawer-mask';
      mask.innerHTML = '<div class="drawer"><h3></h3><div class="drawer-body"></div></div>';
      mask.addEventListener('click', function (e) {
        if (e.target === mask) mask.classList.remove('open');
      });
      document.body.appendChild(mask);
    }
    $('h3', mask).textContent = title;
    $('.drawer-body', mask).innerHTML = html;
    mask.classList.add('open');
  }

  function closeDrawer() {
    var m = $('.drawer-mask');
    if (m) m.classList.remove('open');
  }

  // ---------------------------------------------------------------- 表格
  function table(cols, rows, opts) {
    opts = opts || {};
    var head = cols.map(function (c) {
      return '<th' + (c.style ? ' style="' + c.style + '"' : '') + '>' + esc(c.title) + '</th>';
    }).join('');
    var body;
    if (!rows || !rows.length) {
      body = '<tr><td class="empty" colspan="' + cols.length + '">' + esc(opts.empty || '暂无数据') + '</td></tr>';
    } else {
      body = rows.map(function (row) {
        return '<tr' + (opts.rowAttr ? ' ' + opts.rowAttr(row) : '') + '>' + cols.map(function (c) {
          var v = c.render ? c.render(row) : esc(row[c.key]);
          return '<td' + (c.cls ? ' class="' + c.cls + '"' : '') + '>' + v + '</td>';
        }).join('') + '</tr>';
      }).join('');
    }
    return '<div class="table-wrap"><table><thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody></table></div>';
  }

  // ---------------------------------------------------------------- 路由
  var timers = [];

  function every(ms, fn) {
    var id = setInterval(fn, ms);
    timers.push(id);
    return id;
  }

  function clearTimers() {
    timers.forEach(function (t) { clearInterval(t); });
    timers = [];
  }

  function router(routes, fallback) {
    function render() {
      clearTimers(); // 切走时停掉上一个页面的轮询，避免叠加
      var raw = (location.hash || '').replace(/^#\/?/, '');
      var name = (raw.split('/')[0] || fallback);
      var fn = routes[name] || routes[fallback];
      $$('.nav-item').forEach(function (n) {
        n.classList.toggle('active', n.dataset.view === name);
      });
      if (fn) {
        Promise.resolve(fn()).catch(function (e) {
          toast(e.message || '加载失败', 'err');
        });
      }
    }

    // 侧边导航必须自己绑点击：<div> 不会因为 data-view 就跳转，
    // 之前只监听 hashchange，导致点标签毫无反应。
    $$('.nav-item[data-view]').forEach(function (n) {
      n.setAttribute('role', 'button');
      n.setAttribute('tabindex', '0');
      function activate() {
        var target = '#/' + n.dataset.view;
        if (location.hash === target) {
          render(); // 同一个 tab 再点一次 = 刷新当前视图
        } else {
          location.hash = target;
        }
      }
      n.addEventListener('click', activate);
      n.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          activate();
        }
      });
    });

    window.addEventListener('hashchange', render);
    render();
    return render;
  }

  // ---------------------------------------------------------------- 其他
  function copy(text) {
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).then(function () { toast('已复制', 'ok'); });
    } else {
      toast('当前浏览器不支持自动复制', 'err');
    }
  }

  function logout() {
    API.post('/api/logout').finally(function () { location.href = '/auth/login'; });
  }

  window.Hub = {
    API: API, $: $, $$: $$, esc: esc,
    fmtTime: fmtTime, ago: ago, fmtNum: fmtNum,
    taskBadge: taskBadge, statusText: statusText, channelText: channelText,
    payBadge: payBadge, subBadge: subBadge, kindLabel: kindLabel,
    toast: toast, openDrawer: openDrawer, closeDrawer: closeDrawer,
    table: table, router: router, copy: copy, logout: logout,
    every: every, clearTimers: clearTimers
  };
})();
