'use strict';

const $ = (id) => document.getElementById(id);
const cv = $('cv');
const ctx = cv.getContext('2d', { alpha: false });
const wrap = $('wrap');
const hint = $('hint');
const cursorEl = $('cursor');
const dot = $('dot');
const stat = $('stat');
const infoEl = $('info');
const sheet = $('sheet');
const elemsBox = $('elems');
const elCount = $('elCount');
const elUrl = $('elUrl');

// 平板上点按钮给一点触感反馈
function haptic() {
  if (navigator.vibrate) navigator.vibrate(8);
}
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('.big, .scene, .icon')) haptic();
}, { passive: true });

$('btnCfg').onclick = () => sheet.classList.toggle('open');
$('btnCfgClose').onclick = () => sheet.classList.remove('open');

/* ---------------- 分段切换 ---------------- */

function switchTab(name) {
  document.querySelectorAll('.tab').forEach((x) => x.classList.toggle('on', x.dataset.pane === name));
  $('paneScenes').classList.toggle('hidden', name !== 'scenes');
  $('paneElems').classList.toggle('hidden', name !== 'elems');
  if (name === 'elems' && !elCount.textContent) send({ type: 'page.scan' });
}

document.querySelectorAll('.tab').forEach((t) => {
  t.onclick = () => switchTab(t.dataset.pane);
});

const dpr = Math.min(window.devicePixelRatio || 1, 2);

const S = {
  ws: null,
  online: false,
  bmp: null,
  decoding: false,
  dropped: 0,
  got: 0,
  fps: 0,
  winStart: 0,
  winCount: 0,
  lastBytes: 0,
  lastFrameAt: 0,
  dragging: false,
  lastMoveSent: 0,
  fullscreen: false,
  chromeReady: false,
  scenes: [],
};

// 当前帧的显示几何：CSS 尺度下的缩放与留边偏移
const view = { s: 1, ox: 0, oy: 0, w: 0, h: 0 };

/* ---------------- 画面 ---------------- */

function layout() {
  const r = wrap.getBoundingClientRect();
  const w = Math.max(1, Math.round(r.width * dpr));
  const h = Math.max(1, Math.round(r.height * dpr));
  if (cv.width !== w || cv.height !== h) {
    cv.width = w;
    cv.height = h;
    draw();
    reportPanel();
  }
}

// 面板尺寸变了就告诉服务端：按实际能显示的像素宽下发，别再传整张原图。
let panelTimer = 0;
function reportPanel() {
  clearTimeout(panelTimer);
  panelTimer = setTimeout(() => send({ type: 'panel', w: cv.width }), 250);
}

function draw() {
  const W = cv.width;
  const H = cv.height;
  ctx.fillStyle = '#05070a';
  ctx.fillRect(0, 0, W, H);
  if (!S.bmp || !view.w) return;

  const s = Math.min(W / view.w, H / view.h);
  const ox = (W - view.w * s) / 2;
  const oy = (H - view.h * s) / 2;
  ctx.drawImage(S.bmp, ox, oy, view.w * s, view.h * s);

  view.s = s / dpr;
  view.ox = ox / dpr;
  view.oy = oy / dpr;
}

new ResizeObserver(layout).observe(wrap);
window.addEventListener('resize', layout);

/* ---------------- WebSocket ---------------- */

// 每次 connect 递增。旧 socket 的 onopen/onclose 拿到过期的 gen 就自行退出，
// 免得手动重连和 1.5s 自动重试打架，叠出两条连接。
let wsGen = 0;
let retryTimer = 0;

function connect() {
  const gen = ++wsGen;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  ws.binaryType = 'arraybuffer';
  S.ws = ws;

  ws.onopen = () => {
    if (gen !== wsGen) return;
    S.online = true;
    dot.className = 'dot on';
    retune();
  };

  ws.onmessage = (ev) => {
    if (gen !== wsGen) return;
    if (typeof ev.data === 'string') {
      try { onJson(JSON.parse(ev.data)); } catch (_) {}
    } else {
      onFrame(ev.data);
    }
  };

  ws.onclose = () => {
    if (gen !== wsGen) return;
    S.online = false;
    dot.className = 'dot off';
    stat.textContent = '已断开，重连中…';
    clearTimeout(retryTimer);
    retryTimer = setTimeout(async () => {
      if (gen !== wsGen) return;
      // 先分清是「服务端没起」还是「会话失效」：前者重试有意义，后者重试到天亮也没用
      if (await sessionLost()) {
        if (gen !== wsGen) return;
        showLogin();
        return;
      }
      if (gen === wsGen) connect();
    }, 1500);
  };

  ws.onerror = () => {};
}

// 探一次 /api/state 判断 cookie 是否已经失效。只有 401 算失效；
// fetch 直接抛错说明服务端根本没起来，那时继续退避重连。
async function sessionLost() {
  try {
    const res = await fetch('/api/state', { headers: { 'Accept': 'application/json' } });
    return res.status === 401;
  } catch (_) {
    return false;
  }
}

// 会话失效（服务端换了 cookie 规则、或密码被改）时直接退回密码框，不再空转重连。
// 登录成功后 onsubmit 会 location.reload()，重新走 boot()，不需要在这里续连。
function showLogin() {
  clearTimeout(retryTimer);
  S.online = false;
  dot.className = 'dot off';
  stat.textContent = '会话已失效，请重新登录';
  $('login').classList.remove('hidden');
}

// 手动重连：丢掉旧连接和画面缓存，立刻建一条新的，不等退避。
function reconnectNow() {
  clearTimeout(retryTimer);
  if (S.bmp) { S.bmp.close(); S.bmp = null; }
  view.w = 0;
  view.h = 0;
  S.got = 0;
  S.dropped = 0;
  S.fps = 0;
  S.winStart = 0;
  S.winCount = 0;
  S.lastBytes = 0;
  S.lastFrameAt = 0;
  S.decoding = false;
  S.online = false;
  ctx.fillStyle = '#05070a';
  ctx.fillRect(0, 0, cv.width, cv.height);
  hint.textContent = '重连中…';
  hint.classList.remove('hide');
  dot.className = 'dot off';
  stat.textContent = '重连中…';
  const old = S.ws;
  if (old) { try { old.close(); } catch (_) {} }
  connect();
}

function send(obj) {
  if (S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify(obj));
}

function retune() {
  send({
    type: 'stream.start',
    quality: Number($('q').value),
    w: cv.width,
  });
}

function onJson(msg) {
  switch (msg.type) {
    case 'hello':
      if (msg.chrome) applyChrome(msg.chrome);
      if (msg.scenes) renderScenes(msg.scenes);
      break;

    case 'scenes.state':
      markScene(msg.id, msg.phase, msg.msg);
      if (msg.phase === 'error') toast('场景失败: ' + (msg.msg || ''));
      // 场景打开完直接切到按钮页，下一步就是点页面里的东西
      if (msg.phase === 'ready') {
        S.chromeReady = true;
        switchTab('elems');
        // 静默启动时 Chrome 是后起的：连上那一刻取帧失败会让服务端停流，这里重新拉一次
        retune();
      }
      break;

    case 'chrome.state':
      if (typeof msg.fullscreen === 'boolean') applyFullscreen(msg.fullscreen);
      if (msg.error) toast('操作失败: ' + msg.error);
      break;

    case 'page.buttons':
      renderElems(msg.scan);
      break;

    case 'page.pressed':
      if (msg.error) toast('点击失败: ' + msg.error);
      break;

    case 'error':
      // 服务端静默启动时 Chrome 还没拉起，取帧失败属正常状态，不弹错、不标红
      if (msg.code === 'E_SHOT' && !S.chromeReady) {
        stat.textContent = 'Chrome 未启动';
        hint.textContent = '还没有打开页面';
        hint.classList.remove('hide');
        break;
      }
      toast(msg.code + ': ' + msg.msg);
      if (msg.code === 'E_SHOT') stat.textContent = '截图失败';
      break;
  }
}

function applyChrome(st) {
  if (typeof st.fullscreen === 'boolean') applyFullscreen(st.fullscreen);
  S.chromeReady = !!st.ready;
  if (!st.ready) {
    if (st.err) stat.textContent = 'Chrome 未启动';
    hint.textContent = '还没有打开页面';
    return;
  }
  if (!S.got) hint.textContent = '等待画面…';
}

function applyFullscreen(on) {
  S.fullscreen = on;
  const b = $('btnFull');
  b.classList.toggle('on', on);
  b.title = on ? '退出全屏' : '全屏';
  b.setAttribute('aria-label', b.title);
  $('fsBadge').classList.toggle('hidden', !on);
}

/* ---------------- 帧 ---------------- */

function onFrame(buf) {
  if (buf.byteLength < 5) return;
  const dv = new DataView(buf);
  const hlen = dv.getUint32(0);
  if (hlen === 0 || 4 + hlen > buf.byteLength) return;

  let head;
  try {
    head = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 4, hlen)));
  } catch (_) {
    return;
  }
  const jpeg = new Uint8Array(buf, 4 + hlen);

  S.got++;
  S.lastFrameAt = performance.now();
  S.lastBytes = head.bytes || jpeg.length;

  // 解码跟不上就丢帧，宁可少画也不要排队
  if (S.decoding) {
    S.dropped++;
    updateInfo();
    return;
  }
  S.decoding = true;

  createImageBitmap(new Blob([jpeg], { type: 'image/jpeg' }))
    .then((b) => {
      if (S.bmp) S.bmp.close();
      S.bmp = b;
      view.w = head.iw || b.width;
      view.h = head.ih || b.height;
      draw();
      hint.classList.add('hide');
      S.decoding = false;
      tickFps();
      updateInfo();
    })
    .catch(() => {
      S.decoding = false;
    });
}

function tickFps() {
  const now = performance.now();
  if (!S.winStart) S.winStart = now;
  S.winCount++;
  const dt = now - S.winStart;
  if (dt >= 1000) {
    S.fps = (S.winCount * 1000) / dt;
    S.winStart = now;
    S.winCount = 0;
  }
}

function updateInfo() {
  const kb = (S.lastBytes / 1024).toFixed(0);
  infoEl.textContent =
    `帧率  ${S.fps ? S.fps.toFixed(1) : '—'} fps\n` +
    `单帧  ${kb} KB\n` +
    `丢帧  ${S.dropped}`;
  if (!S.online) return;
  // 画面不变时 Chrome 不推帧，没有帧不等于断了
  const still = S.lastFrameAt && performance.now() - S.lastFrameAt > 2500;
  stat.textContent = still
    ? `静止 · ${view.w}×${view.h}`
    : `${S.fps ? S.fps.toFixed(1) : '—'} fps · ${view.w}×${view.h}`;
  dot.className = 'dot on';
}

// 静止时也要把状态从「x fps」切成「静止」，不能等下一帧
setInterval(() => { if (S.online) updateInfo(); }, 1000);

/* ---------------- 坐标与鼠标 ---------------- */

function toNorm(e) {
  if (!view.w) return null;
  const r = cv.getBoundingClientRect();
  const px = e.clientX - r.left;
  const py = e.clientY - r.top;
  const nx = (px - view.ox) / (view.w * view.s);
  const ny = (py - view.oy) / (view.h * view.s);
  if (nx < 0 || nx > 1 || ny < 0 || ny > 1) return null;
  return { x: nx, y: ny };
}

function moveCursor(e) {
  const r = wrap.getBoundingClientRect();
  cursorEl.style.left = e.clientX - r.left + 'px';
  cursorEl.style.top = e.clientY - r.top + 'px';
  cursorEl.classList.remove('hidden');
}

let cursorTimer = 0;
function fadeCursor() {
  clearTimeout(cursorTimer);
  cursorTimer = setTimeout(() => cursorEl.classList.add('hidden'), 900);
}

cv.addEventListener('pointerdown', (e) => {
  const p = toNorm(e);
  moveCursor(e);
  if (!p) return;
  cv.setPointerCapture(e.pointerId);
  S.dragging = true;
  S.lastMoveSent = 0;
  send({ type: 'mouse', action: 'down', x: p.x, y: p.y, buttons: 1 });
});

cv.addEventListener('pointermove', (e) => {
  moveCursor(e);
  if (!S.dragging) return;
  const now = performance.now();
  if (now - S.lastMoveSent < 16) return;
  S.lastMoveSent = now;
  const p = toNorm(e);
  if (p) send({ type: 'mouse', action: 'move', x: p.x, y: p.y, buttons: 1 });
});

function endDrag(e) {
  fadeCursor();
  if (!S.dragging) return;
  S.dragging = false;
  const p = toNorm(e);
  if (p) {
    send({ type: 'mouse', action: 'move', x: p.x, y: p.y, buttons: 1 });
    send({ type: 'mouse', action: 'up', x: p.x, y: p.y, buttons: 0 });
  } else {
    send({ type: 'mouse', action: 'up', x: 0.5, y: 0.5, buttons: 0 });
  }
}

cv.addEventListener('pointerup', endDrag);
cv.addEventListener('pointercancel', endDrag);

cv.addEventListener('wheel', (e) => {
  e.preventDefault();
  const p = toNorm(e);
  if (!p) return;
  send({ type: 'wheel', x: p.x, y: p.y, dx: e.deltaX, dy: e.deltaY });
}, { passive: false });

// 页面失焦时补一次 up，避免会话里残留按下状态
window.addEventListener('blur', () => {
  if (S.dragging) {
    S.dragging = false;
    send({ type: 'mouse', action: 'up', x: 0.5, y: 0.5, buttons: 0 });
  }
});

/* ---------------- 场景与操作 ---------------- */

function renderScenes(list) {
  S.scenes = list;
  const box = $('scenes');
  box.innerHTML = '';
  list.forEach((s) => {
    const b = document.createElement('button');
    b.className = 'scene';
    b.dataset.id = s.id;
    b.innerHTML = `<span class="n">${escapeHtml(s.name)}</span><span class="u">${escapeHtml(s.url)}</span>`;
    b.onclick = () => {
      send({ type: 'scenes.run', id: s.id });
      markScene(s.id, 'running');
    };
    box.appendChild(b);
  });
  if (!list.length) box.innerHTML = '<div class="info">配置里还没有场景</div>';
}

function markScene(id, phase, msg) {
  document.querySelectorAll('.scene').forEach((el) => {
    if (el.dataset.id !== id) return;
    el.classList.toggle('running', phase === 'running');
    el.classList.toggle('err', phase === 'error');
    const u = el.querySelector('.u');
    if (u) {
      if (phase === 'running') u.textContent = '打开中…';
      else if (phase === 'error') u.textContent = msg || '失败';
      else {
        const s = S.scenes.find((x) => x.id === id);
        if (s) u.textContent = s.url;
      }
    }
  });
}

$('btnFull').onclick = () => send({ type: 'chrome.fullscreen', on: !S.fullscreen });
$('btnFocus').onclick = () => send({ type: 'chrome.focus' });

$('btnRestart').onclick = async () => {
  const b = $('btnRestart');
  if (b.disabled) return;
  b.disabled = true;
  b.classList.add('spin');
  try {
    await fetch('/api/chrome/restart', { method: 'POST' });
  } catch (_) {}
  b.classList.remove('spin');
  b.disabled = false;
  retune(); // 新实例起来后重新开流，否则画面停在最后一帧
};

$('btnReconnect').onclick = () => {
  const b = $('btnReconnect');
  b.classList.add('spin');
  reconnectNow();
  setTimeout(() => b.classList.remove('spin'), 1000);
};

document.querySelectorAll('[data-swipe]').forEach((b) => {
  b.onclick = () => send({ type: 'gesture.swipe', dir: b.dataset.swipe, distance: 0.35 });
});

// 上一页 / 下一页：按页面高度滚一屏（-1 向上，1 向下）
document.querySelectorAll('[data-scroll]').forEach((b) => {
  b.onclick = () => {
    if (!view.h) return;
    send({ type: 'wheel', x: 0.5, y: 0.5, dx: 0, dy: Number(b.dataset.scroll) * view.h * 0.85 });
  };
});

['q'].forEach((id) => {
  const el = $(id);
  const label = $(id + 'v');
  el.oninput = () => { label.textContent = el.value; };
  el.onchange = () => retune();
});

/* ---------------- 页面按钮 ---------------- */

const KIND_LABEL = {
  link: '链接', button: '按钮', input: '输入', menuitem: '菜单', tab: '标签',
  summary: '折叠', checkbox: '勾选', radio: '单选', switch: '开关', option: '选项',
};

let elemsSig = '';

function renderElems(scan) {
  const items = (scan && scan.items) || [];
  elCount.textContent = items.length ? String(items.length) : '';
  elUrl.textContent = scan && scan.url ? String(scan.url).replace(/^https?:\/\//, '') : '等待页面…';

  if (!items.length) {
    elemsSig = '';
    elemsBox.innerHTML = '<div class="empty">这个页面没扫到可点的按钮</div>';
    return;
  }

  // 内容没变就不重建 DOM，否则用户滚到一半会被弹回顶部
  const sig = (scan.url || '') + '#' + items.map((i) => i.text).join('|');
  if (sig === elemsSig) return;
  elemsSig = sig;

  const keep = $('paneElems').scrollTop;
  const frag = document.createDocumentFragment();
  items.forEach((it) => {
    const b = document.createElement('button');
    b.className = 'el' + (it.inView ? '' : ' away');
    b.innerHTML = '<span class="n"></span><span class="t"></span><span class="k"></span>';
    b.querySelector('.n').textContent = String(it.id + 1);
    b.querySelector('.t').textContent = it.text;          // 页面文本不可信，一律走 textContent
    b.querySelector('.k').textContent = KIND_LABEL[it.kind] || it.kind || '按钮';
    b.onclick = () => {
      send({ type: 'page.press', index: it.id });
      b.classList.add('hit');
      setTimeout(() => b.classList.remove('hit'), 900);
    };
    frag.appendChild(b);
  });
  elemsBox.innerHTML = '';
  elemsBox.appendChild(frag);
  $('paneElems').scrollTop = keep;
}

$('btnScan').onclick = () => {
  elemsSig = '';
  send({ type: 'page.scan' });
  elUrl.textContent = '扫描中…';
};

/* ---------------- 杂项 ---------------- */

function toast(msg) {
  stat.textContent = msg;
  setTimeout(() => { if (S.online) updateInfo(); }, 3200);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[c]);
}

setInterval(() => send({ type: 'ping', t: Date.now() }), 20000);

/* ---------------- 启动 ---------------- */

async function boot() {
  layout();
  try {
    const res = await fetch('/api/state', { headers: { 'Accept': 'application/json' } });
    if (res.status === 401) {
      $('login').classList.remove('hidden');
      return;
    }
    const data = await res.json();
    if (data.scenes) renderScenes(data.scenes);
    if (data.chrome) applyChrome(data.chrome);
    connect();
  } catch (e) {
    $('login').classList.remove('hidden');
  }
}

$('loginForm').onsubmit = async (e) => {
  e.preventDefault();
  const err = $('loginErr');
  err.textContent = '';
  const res = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: $('pwd').value }),
  });
  if (res.ok) {
    $('login').classList.add('hidden');
    location.reload();
  } else {
    const d = await res.json().catch(() => ({}));
    err.textContent = d.error || '登录失败';
  }
};

boot();
