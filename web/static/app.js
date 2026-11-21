// ZeroOS desktop UI: core helpers, sign-in, setup, desktop and windows.
'use strict';
const $ = (s, p) => (p || document).querySelector(s);
const $$ = (s, p) => [...(p || document).querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const el = (html) => { const t = document.createElement('template'); t.innerHTML = html.trim(); return t.content.firstElementChild; };
const S = { user: null, apps: [], attention: [], system: null, sharedCount: 0, hostname: '', version: '' };
const can = (p) => !!S.user && S.user.perms.includes(p);
const isPhone = () => window.innerWidth <= 760;

async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  else if (method !== 'GET') { opt.headers['Content-Type'] = 'application/json'; opt.body = '{}'; }
  let res;
  try { res = await fetch(path, opt); } catch (e) { throw new Error("Can't reach ZeroOS. Check that the mini PC is on."); }
  let data = null;
  try { data = await res.json(); } catch (e) { /* no body */ }
  if (res.status === 401 && S.user) { S.user = null; boot(); }
  if (!res.ok) { const err = new Error((data && data.error) || 'Something went wrong.'); err.data = data; err.status = res.status; throw err; }
  return data;
}
const GET = (p) => api('GET', p), POST = (p, b) => api('POST', p, b), PUT = (p, b) => api('PUT', p, b), DEL = (p) => api('DELETE', p);

function toast(msg, bad) {
  const t = el(`<div class="toast ${bad ? 'bad' : ''}">${esc(msg)}</div>`);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), bad ? 6000 : 3500);
}
const oops = (e) => toast(e.message, true);

// busy(button, promiseFn): disables the button while the work runs.
async function busy(btn, fn) {
  if (btn.disabled) return;
  const old = btn.innerHTML;
  btn.disabled = true; btn.innerHTML = '<span class="spinner dark"></span>';
  try { return await fn(); } catch (e) { oops(e); } finally { btn.disabled = false; btn.innerHTML = old; }
}

const SVG = {
  logo: '<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/></svg>',
  store: '<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 2 3 6v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V6l-3-4zM3 6h18M16 10a4 4 0 0 1-8 0"/></svg>',
  gear: '<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1 7 17M17 7l2.1-2.1"/></svg>',
  search: '<svg aria-hidden="true" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>',
  bell: '<svg aria-hidden="true" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9M10.3 21a1.9 1.9 0 0 0 3.4 0"/></svg>',
  min: '<svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 12h14"/></svg>',
  max: '<svg aria-hidden="true" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="4" y="4" width="16" height="16" rx="2"/></svg>',
  x: '<svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 6l12 12M18 6 6 18"/></svg>',
  back: '<svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6"/></svg>',
  fwd: '<svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6l6 6-6 6"/></svg>',
  home: '<svg aria-hidden="true" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 10.5 12 3l9 7.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z"/></svg>',
  tab: '<svg aria-hidden="true" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>',
  folder: '<svg aria-hidden="true" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>',
};
const COLORS = ['#0E6E8C', '#6D28D9', '#1D4ED8', '#0F766E', '#0369A1', '#A16207', '#BE185D', '#B91C1C', '#7C3AED', '#C2410C', '#15803D', '#475569'];
function colorOf(s) { let h = 0; for (const c of String(s)) h = (h * 31 + c.charCodeAt(0)) >>> 0; return COLORS[h % COLORS.length]; }
// App icon: the catalog's logo, with a coloured letter underneath if it has none.
function icon(storeId, name, size) {
  const st = `--c:${colorOf(name)}${size ? `;--s:${size}` : ''}`;
  const img = storeId ? `<img src="/api/icons/${encodeURIComponent(storeId)}" alt="" loading="lazy" onerror="this.remove()">` : '';
  return `<span class="ic" style="${st}"><b>${esc((name || '?')[0].toUpperCase())}</b>${img}</span>`;
}
const sysIcon = (svg, color, size) => `<span class="ic" style="--c:${color}${size ? `;--s:${size}` : ''}">${svg}</span>`;
const fmtBytes = (n) => { if (!n) return '0 GB'; const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1000 && i < 4) { n /= 1000; i++; } return (n >= 100 || i < 3 ? Math.round(n) : n.toFixed(1)) + ' ' + u[i]; };
const fmtTime = (ts) => ts ? new Date(ts * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : 'never';
const pct = (a, b) => b ? Math.min(100, Math.round(100 * a / b)) : 0;

// --- overlays: dialogs, menus --------------------------------------------------
function dialog(html, opt = {}) {
  const scrim = el(`<div class="scrim"><div class="dlg ${opt.wide ? 'wide' : ''}" role="dialog" aria-modal="true">${html}</div></div>`);
  const close = () => { scrim.remove(); document.removeEventListener('keydown', onKey, true); if (opt.onClose) opt.onClose(); };
  const onKey = (e) => { if (e.key === 'Escape' && !opt.sticky && $('#layer').lastElementChild === scrim) { e.stopPropagation(); close(); } };
  scrim.addEventListener('mousedown', (e) => { if (e.target === scrim && !opt.sticky) close(); });
  document.addEventListener('keydown', onKey, true);
  $('#layer').append(scrim);
  const first = $('input:not([type=hidden]),select,textarea,button.pri', scrim); if (first) setTimeout(() => first.focus(), 30);
  $$('[data-close]', scrim).forEach((b) => b.onclick = close);
  return { el: $('.dlg', scrim), close };
}
function confirmDlg(title, text, okLabel = 'OK', danger = false) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(`<h2>${esc(title)}</h2><p style="color:var(--ink2)">${text}</p><div class="foot"><button class="btn" data-no>Cancel</button><button class="btn ${danger ? 'danger' : 'pri'}" data-yes>${esc(okLabel)}</button></div>`, { onClose: () => { if (!done) resolve(false); } });
    $('[data-no]', d.el).onclick = () => d.close();
    $('[data-yes]', d.el).onclick = () => { done = true; d.close(); resolve(true); };
  });
}
function menu(x, y, items) {
  closeMenu();
  const m = el(`<div class="menu" role="menu"></div>`);
  for (const it of items) {
    if (!it) continue;
    if (it === '-') { m.append(el('<hr>')); continue; }
    const b = el(`<button role="menuitem" ${it.danger ? 'style="color:var(--bad)"' : ''}>${it.bold ? `<strong>${esc(it.label)}</strong>` : esc(it.label)}${it.right ? `<span class="r">${esc(it.right)}</span>` : ''}</button>`);
    b.onclick = () => { closeMenu(); it.run(); };
    m.append(b);
  }
  $('#layer').append(m);
  const r = m.getBoundingClientRect();
  m.style.left = Math.max(6, Math.min(x, innerWidth - r.width - 6)) + 'px';
  m.style.top = Math.max(6, Math.min(y, innerHeight - r.height - 6)) + 'px';
  setTimeout(() => { document.addEventListener('mousedown', outside, true); document.addEventListener('keydown', escMenu, true); }, 0);
  const f = $('button', m); if (f) f.focus();
}
function outside(e) { if (!e.target.closest('.menu')) closeMenu(); }
function escMenu(e) { if (e.key === 'Escape') closeMenu(); }
function closeMenu() { $$('#layer .menu').forEach((m) => m.remove()); document.removeEventListener('mousedown', outside, true); document.removeEventListener('keydown', escMenu, true); }

// --- boot: setup, sign-in or desktop -------------------------------------------
async function boot() {
  clearInterval(boot.timer);
  $('#layer').innerHTML = '';
  let st;
  try { st = await GET('/api/state'); } catch (e) { $('#root').innerHTML = `<div class="gate"><div class="inner"><div class="card"><h2>Can't reach ZeroOS</h2><p class="muted">${esc(e.message)}</p><button class="btn pri" onclick="boot()">Try again</button></div></div></div>`; return; }
  S.hostname = st.hostname; S.version = st.version;
  if (!st.setupDone) return showSetup(1);
  if (!st.user) return showLogin();
  S.user = st.user;
  if (localStorage.getItem('zeroos-setup') === 'more') return showSetup(3);
  showDesktop();
}
const gateLogo = () => `<div class="logo">${sysIcon(SVG.logo, 'var(--accent)')}<h1>ZeroOS</h1><span style="color:#D5DBE2;font-size:.9em">${esc(S.hostname)}</span></div>`;

function showLogin() {
  $('#root').innerHTML = `<div class="gate"><div class="inner">${gateLogo()}
    <form class="card" autocomplete="on">
      <label class="f">Username<input type="text" name="u" autocomplete="username" required></label>
      <label class="f">Password<input type="password" name="p" autocomplete="current-password" required></label>
      <label class="f hide" data-code>6-digit code from your authenticator app<input type="text" name="c" inputmode="numeric" autocomplete="one-time-code" maxlength="7"></label>
      <label class="check"><input type="checkbox" name="r">Keep me signed in on this device</label>
      <div class="note bad hide" role="alert"></div>
      <button class="btn pri" type="submit">Sign in</button>
      <p class="muted" style="text-align:center">Forgot your password? Ask the person who runs this server.</p>
    </form></div></div>`;
  const f = $('form'); f.u.focus();
  f.onsubmit = async (e) => {
    e.preventDefault();
    const note = $('.note', f); note.classList.add('hide');
    await busy($('button[type=submit]', f), async () => {
      try {
        const r = await POST('/api/login', { username: f.u.value, password: f.p.value, code: f.c.value, remember: f.r.checked });
        S.user = r.user; showDesktop();
      } catch (err) {
        if (err.data && err.data.needCode) { $('[data-code]', f).classList.remove('hide'); f.c.focus(); }
        note.textContent = err.message; note.classList.remove('hide');
      }
    });
  };
}

// First-run setup: code → account → NAS → sharing → done.
function showSetup(step, ctx = {}) {
  const steps = ['Setup code', 'Your account', 'Add your NAS', 'Sharing online', 'Ready'];
  const nav = steps.map((s, i) => `<div class="step ${i + 1 < step ? 'done' : i + 1 === step ? 'now' : ''}"><span class="n">${i + 1 < step ? '✓' : i + 1}</span>${s}</div>`).join('');
  $('#root').innerHTML = `<div class="gate"><section class="setup"><nav aria-label="Setup steps"><strong style="margin-bottom:.7em">Set up ZeroOS</strong>${nav}</nav><div class="main"></div></section></div>`;
  const main = $('.setup .main');
  const head = (title, text, optional) => `<div class="col" style="gap:.35em"><span class="muted">Step ${step} of ${steps.length}${optional ? ' · you can skip this' : ''}</span><h1>${title}</h1><p style="color:var(--ink2)">${text}</p></div>`;
  if (step === 1) {
    main.innerHTML = `${head('Enter the setup code', 'The code was shown in the terminal when ZeroOS was installed. It makes sure nobody else on your network can claim this server.')}
      <form class="col"><label class="f">Setup code<input type="text" name="code" autocomplete="off" autocapitalize="characters" style="font-size:1.3rem;letter-spacing:.2em;max-width:11em" required></label>
      <p class="muted">Lost it? Run <code>sudo zeroos setup-code</code> on the mini PC.</p><div class="note bad hide" role="alert"></div>
      <div class="row" style="justify-content:flex-end"><button class="btn pri" type="submit">Continue</button></div></form>`;
    const f = $('form', main); f.code.focus();
    f.onsubmit = async (e) => { e.preventDefault(); const n = $('.note', f); n.classList.add('hide');
      await busy($('button', f), async () => { try { const r = await POST('/api/setup/code', { code: f.code.value }); showSetup(2, { ticket: r.ticket }); } catch (err) { n.textContent = err.message; n.classList.remove('hide'); } }); };
  } else if (step === 2) {
    main.innerHTML = `${head('Create your account', "You'll be the owner: the one account that can always do everything.")}
      <form class="col"><label class="f">Username<input type="text" name="u" autocomplete="username" required></label>
      <label class="f">Password<input type="password" name="p" autocomplete="new-password" minlength="8" required></label>
      <label class="f">Password again<input type="password" name="p2" autocomplete="new-password" required></label>
      <div class="note bad hide" role="alert"></div><div class="row" style="justify-content:flex-end"><button class="btn pri" type="submit">Continue</button></div></form>`;
    const f = $('form', main); f.u.focus();
    f.onsubmit = async (e) => { e.preventDefault(); const n = $('.note', f); n.classList.add('hide');
      if (f.p.value !== f.p2.value) { n.textContent = "The two passwords don't match."; n.classList.remove('hide'); return; }
      await busy($('button', f), async () => { try { const r = await POST('/api/setup/owner', { ticket: ctx.ticket, username: f.u.value, password: f.p.value }); S.user = r.user; localStorage.setItem('zeroos-setup', 'more'); showSetup(3); } catch (err) { n.textContent = err.message; n.classList.remove('hide'); } }); };
  } else if (step === 3) {
    main.innerHTML = `${head('Add your NAS', 'Apps keep their own data on this mini PC. Your NAS gives them room for big things like films, photos and backups.', true)}<div data-nas></div>
      <div class="between" style="border-top:1px solid #F0F1F3;padding-top:1em"><span></span><div class="row"><button class="btn" data-skip>Skip</button><button class="btn pri" data-next>Continue</button></div></div>`;
    nasFlow($('[data-nas]', main));
    $('[data-skip]', main).onclick = $('[data-next]', main).onclick = () => showSetup(4);
  } else if (step === 4) {
    main.innerHTML = `${head('Sharing online', 'Link a free playit.gg account to reach apps and game servers from outside your home, with no router setup.', true)}<div data-pl></div>
      <div class="between" style="border-top:1px solid #F0F1F3;padding-top:1em"><button class="btn" data-back>Back</button><div class="row"><button class="btn" data-skip>Skip</button><button class="btn pri" data-next>Continue</button></div></div>`;
    playitLinkBox($('[data-pl]', main));
    $('[data-back]', main).onclick = () => showSetup(3);
    $('[data-skip]', main).onclick = $('[data-next]', main).onclick = () => showSetup(5);
  } else {
    main.innerHTML = `${head("You're ready", 'ZeroOS is set up. Open the Store to get your first apps.')}
      <div class="row" style="justify-content:flex-end"><button class="btn pri" data-go>Open my desktop</button></div>`;
    $('[data-go]', main).onclick = () => { localStorage.removeItem('zeroos-setup'); showDesktop(); openStore(); };
  }
}

// --- desktop ---------------------------------------------------------------------
const WINS = new Map(); let zTop = 10;
// Keeps the browser's Back (and the Back button on app windows) from leaving ZeroOS.
function guardHistory() {
  if (!history.state || !history.state.zeroos) history.pushState({ zeroos: 1 }, '');
  if (guardHistory.on) return; guardHistory.on = true;
  window.addEventListener('popstate', (e) => { if (S.user && (!e.state || !e.state.zeroos)) history.pushState({ zeroos: 1 }, ''); });
}
function showDesktop() {
  WINS.clear(); guardHistory();
  $('#root').innerHTML = `
    <header class="bar">
      <button class="brand" data-home aria-label="Show desktop">${sysIcon(SVG.logo, 'var(--accent)')}<span>ZeroOS</span></button>
      <button class="search" data-search aria-label="Search apps, settings and actions">${SVG.search}<span>Search apps, settings and actions</span><kbd>Ctrl K</kbd></button>
      <div class="row" style="gap:.2em;flex-wrap:nowrap">
        <button class="ibtn" data-bell aria-label="Notifications">${SVG.bell}<span class="dotn hide"></span></button>
        <button class="ibtn" data-me aria-label="Account menu"><span class="av">${esc(S.user.username[0].toUpperCase())}</span><span class="uname">${esc(S.user.username)}</span></button>
      </div>
    </header>
    <main class="desk"><div class="icons" role="list" aria-label="Apps"></div><aside class="widgets" aria-label="Widgets"></aside><div class="wins" style="position:absolute;inset:0;pointer-events:none"></div></main>
    <div class="dockwrap"><nav class="dock" aria-label="Dock"></nav></div>`;
  $('[data-home]').onclick = () => WINS.forEach((w) => w.el.classList.add('min')) || renderDock();
  $('[data-search]').onclick = openPalette;
  $('[data-me]').onclick = (e) => { const r = e.currentTarget.getBoundingClientRect(); menu(r.right - 220, r.bottom + 4, [
    { label: 'Profile and password', run: () => openSettings('profile') },
    { label: 'Settings', run: () => openSettings() }, '-',
    { label: 'Sign out', run: async () => { await POST('/api/logout').catch(() => 0); S.user = null; boot(); } }]); };
  $('[data-bell]').onclick = (e) => { const r = e.currentTarget.getBoundingClientRect();
    if (!S.attention.length) return menu(r.right - 240, r.bottom + 4, [{ label: 'Nothing needs your attention', run() {} }]);
    menu(r.right - 340, r.bottom + 4, S.attention.map((a) => ({ label: a.text.length > 60 ? a.text.slice(0, 58) + '…' : a.text, run: () => a.appId ? openAppSettings(a.appId) : openSettings('space') }))); };
  $('.desk').addEventListener('contextmenu', (e) => { if (e.target.closest('.dicon,.win,.widget')) return; e.preventDefault();
    menu(e.clientX, e.clientY, [{ label: 'Open the Store', run: openStore }, { label: 'Settings', run: () => openSettings() }, { label: 'Refresh', run: refresh }]); });
  refresh();
  boot.timer = setInterval(refresh, 5000);
}

async function refresh() {
  if (!S.user || !$('.desk')) return;
  try { const d = await GET('/api/desktop'); S.apps = d.apps; S.attention = d.attention; S.system = d.system || null; S.sharedCount = d.sharedCount; } catch (e) { return; }
  newVersionCheck(S.system && S.system.version);
  renderIcons(); renderWidgets(); renderDock(); liveTick();
  const n = $('.bar .dotn'); if (n) { n.textContent = S.attention.length; n.classList.toggle('hide', !S.attention.length); }
}

// After "zeroos update", a page that was already open still runs the old version.
// With nothing open it reloads itself; otherwise it offers to, so nothing you have open is lost.
function newVersionCheck(v) {
  if (!v) return;
  if (!S.version) { S.version = v; return; }
  if (v === S.version || $('#newver')) return;
  if (!WINS.size && !$('#layer').children.length) { location.reload(); return; }
  const t = el(`<div class="toast" id="newver" role="status" style="pointer-events:auto">ZeroOS was updated to ${esc(v)}. <button class="link" style="color:#fff;text-decoration:underline">Reload to use it</button></div>`);
  $('button', t).onclick = () => location.reload();
  $('#toasts').append(t);
}

// --- keeping open windows current -------------------------------------------------------
// Every window says what it shows (w.watch: addresses to ask, or functions), and is drawn
// again when an answer changes. Never while you are busy in it: typing, a half-filled form,
// or an open dialog or menu.
const appsSig = () => JSON.stringify(S.apps.map((a) => [a.id, a.storeId, a.state, a.name, a.shared, a.update]));
function winBusy(w) {
  if ($('#layer').children.length) return true;
  const ae = document.activeElement;
  if (ae && w.el.contains(ae) && !ae.hasAttribute('data-keep') && // data-keep: lives through a redraw (the Store's search box)
     (ae.tagName === 'TEXTAREA' || ae.tagName === 'SELECT' || (ae.tagName === 'INPUT' && !['checkbox', 'radio', 'button', 'submit'].includes(ae.type)))) return true;
  // a form you started filling in; switches and tick boxes outside a form save at once
  for (const i of $$('form input,form textarea,form select', w.body)) {
    if (i.hasAttribute('data-keep')) continue;
    if (i.type === 'checkbox' || i.type === 'radio') { if (i.checked !== i.defaultChecked) return true; }
    else if (i.tagName === 'SELECT') { if ([...i.options].some((o) => o.selected !== o.defaultSelected)) return true; }
    else if (i.value !== i.defaultValue) return true;
  }
  return false;
}
async function liveCheck(w) {
  if (!w.watch || w.liveRun) return;
  w.liveRun = true;
  try {
    const deps = w.watch(); if (!deps) return;
    const key = deps.map((d) => typeof d === 'string' ? d : d.url || 'fn').join('|');
    const parts = await Promise.all(deps.map((d) => typeof d === 'function' ? d() : GET(d.url || d).then((v) => JSON.stringify(d.pick ? d.pick(v) : v), () => 'gone')));
    if (!WINS.has(w.key)) return;
    const now = parts.join('\n');
    if (w.liveKey !== key) { w.liveKey = key; w.liveSig = now; return; } // a new page: this is what it shows
    if (now === w.liveSig) return;
    if (!winBusy(w)) return await liveApply(w, now);
    // You are busy in it: draw it the moment you are done, not at the next round.
    w.livePending = now;
    if (!w.liveWait) w.liveWait = setInterval(() => {
      if (!WINS.has(w.key)) { clearInterval(w.liveWait); w.liveWait = null; return; }
      if (winBusy(w) || w.liveRun) return;
      clearInterval(w.liveWait); w.liveWait = null;
      if (w.livePending !== w.liveSig) liveApply(w, w.livePending);
    }, 300);
  } catch (e) { /* next time */ } finally { w.liveRun = false; }
}
async function liveApply(w, now) {
  w.liveSig = now;
  const scroller = $('.pane', w.body), top = scroller ? scroller.scrollTop : 0, open = $$('details', w.body).map((d) => d.open);
  try { await w.redrawQuiet(); } catch (e) { w.liveSig = null; return; } // didn't work: try again next round
  const sc = $('.pane', w.body); if (sc) sc.scrollTop = top;
  $$('details', w.body).forEach((d, i) => { if (open[i]) d.open = true; });
}
// liveKick: a window just drew a page; note what it shows right away.
function liveKick(w) { w.liveKey = null; liveCheck(w); }
function liveTick() {
  for (const w of [...WINS.values()]) {
    const id = w.key.startsWith('appset:') ? w.key.slice(7) : w.key.startsWith('app:') ? w.key.slice(4) : null;
    if (id && !S.apps.some((a) => a.id === id)) { if (!$('#layer').children.length) closeWin(w.key); continue; } // the app was removed
    if (w.tickFn && !winBusy(w)) w.tickFn();
    liveCheck(w);
  }
}

const BAD = ['waiting', 'crashed', 'failed'];
function renderIcons() {
  const box = $('.icons'); if (!box) return;
  const sig = JSON.stringify(S.apps.map((a) => [a.id, a.state, a.name, a.update]));
  if (box.dataset.sig === sig) return; box.dataset.sig = sig;
  box.innerHTML = '';
  const add = (html, label, onOpen, onMenu, cls = '') => {
    const b = el(`<button class="dicon ${cls}" role="listitem" title="${esc(label)}">${html}<span class="nm">${esc(label)}</span></button>`);
    b.onclick = onOpen;
    if (onMenu) b.addEventListener('contextmenu', (e) => { e.preventDefault(); $$('.dicon.sel').forEach((x) => x.classList.remove('sel')); onMenu(e.clientX, e.clientY); });
    box.append(b);
  };
  for (const a of S.apps) {
    const busyState = ['installing', 'starting', 'removing', 'updating'].includes(a.state);
    const extra = BAD.includes(a.state) ? '<span class="bang" aria-label="Needs attention">!</span>' : busyState ? '<span class="spin"><span class="spinner"></span></span>' : a.update ? '<span class="bang upd" aria-label="Update available">↑</span>' : '';
    const ic = icon(a.storeId, a.name).replace('</span>', extra + '</span>');
    add(ic, a.name, () => openApp(a.id), (x, y) => appMenu(a.id, x, y), a.state === 'stopped' ? 'off' : '');
  }
  add(sysIcon(SVG.store, '#111827'), 'Store', openStore);
  add(sysIcon(SVG.gear, '#475569'), 'Settings', () => openSettings());
  $('.empty')?.remove();
  if (!S.apps.length && !isPhone() && can('apps.install')) {
    const e = el(`<div class="empty"><h1>Welcome to ZeroOS</h1><p>Your apps will show up here. Get your first one from the Store.</p><button class="btn pri">Open the Store</button></div>`);
    $('button', e).onclick = openStore; $('.desk').append(e);
  }
}

function renderWidgets() {
  const box = $('.widgets'); if (!box) return;
  let h = '';
  if (S.attention.length) {
    h += `<section class="widget"><h3>Needs attention</h3>${S.attention.map((a, i) => `<div class="item">${a.appId ? icon(a.storeId, a.name, '1.9em') : sysIcon(SVG.folder, '#A16207', '1.9em')}<div class="col grow" style="gap:.35em"><span>${esc(a.text)}</span><button class="link" data-att="${i}" style="align-self:flex-start">${a.appId ? 'Look at it' : 'Check Space'}</button></div></div>`).join('')}</section>`;
  }
  const shared = S.apps.filter((a) => a.shared);
  if (S.sharedCount) h += `<section class="widget shared"><div class="between"><h3>Shared online</h3><button class="link" data-sharing>Manage</button></div><div class="row" style="gap:.35em">${shared.map((a) => icon(a.storeId, a.name, '1.9em')).join('')}</div><span class="muted">${S.sharedCount} ${S.sharedCount === 1 ? 'thing is' : 'things are'} reachable from outside your home.</span></section>`;
  if (S.system) {
    const s = S.system;
    h += `<section class="widget sys"><h3>This mini PC</h3><div class="wstat"><span class="muted">CPU</span><div class="meter"><span style="width:${Math.round(s.cpu)}%"></span></div><span class="muted">Memory</span><div class="meter"><span style="width:${pct(s.memUsed, s.memTotal)}%"></span></div><span class="muted">SSD</span><div class="meter"><span style="width:${pct(s.ssd.used, s.ssd.total)}%"></span></div></div></section>`;
  }
  if (box.dataset.h === h) return; box.dataset.h = h; box.innerHTML = h;
  $$('[data-att]', box).forEach((b) => b.onclick = () => { const a = S.attention[+b.dataset.att]; a.appId ? openAppSettings(a.appId) : openSettings('space'); });
  const sh = $('[data-sharing]', box); if (sh) sh.onclick = () => openSettings('sharing');
}

function renderDock() {
  const d = $('.dock'); if (!d) return;
  d.innerHTML = '';
  const add = (html, label, key, run) => { const b = el(`<button class="${WINS.has(key) ? 'open' : ''}" aria-label="${esc(label)}" title="${esc(label)}">${html}<i></i></button>`); b.onclick = run; d.append(b); };
  add(sysIcon(SVG.store, '#111827'), 'Store', 'store', openStore);
  add(sysIcon(SVG.gear, '#475569'), 'Settings', 'settings', () => openSettings());
  const open = [...WINS.values()].filter((w) => w.dock);
  if (open.length) d.append(el('<span class="sep"></span>'));
  for (const w of open) add(w.iconHTML, w.title, w.key, () => focusWin(w.key));
}

function appMenu(id, x, y) {
  const a = S.apps.find((v) => v.id === id); if (!a) return;
  const has = (p) => a.perms.includes(p);
  const running = !['stopped', 'failed'].includes(a.state);
  menu(x, y, [
    has('app.open') && !a.noGui && { label: 'Open', bold: true, run: () => openApp(id) },
    has('app.open') && !a.noGui && { label: 'Open in a new tab', run: () => openApp(id, true) },
    '-',
    has('app.share') && { label: 'Share online', right: a.shared ? 'On' : 'Off', run: () => openAppSettings(id, 'sharing') },
    { label: 'Settings', run: () => openAppSettings(id) },
    a.update && has('app.update') && { label: 'Update to ' + a.update, run: () => updateApp(id) },
    has('app.power') && '-',
    has('app.power') && running && { label: 'Restart', run: () => appAction(id, 'restart', 'Restarting ' + a.name + '…') },
    has('app.power') && running && { label: 'Stop', run: () => appAction(id, 'stop', 'Stopping ' + a.name + '…') },
    has('app.power') && !running && { label: 'Start', run: () => appAction(id, 'start', 'Starting ' + a.name + '…') },
    has('app.remove') && '-',
    has('app.remove') && { label: 'Remove', danger: true, run: () => removeApp(id) },
  ]);
}
async function appAction(id, action, msg) {
  toast(msg);
  try { await POST(`/api/apps/${id}/${action}`); } catch (e) { oops(e); }
  refresh();
}
async function removeApp(id) {
  const a = S.apps.find((v) => v.id === id); if (!a) return;
  const d = dialog(`<h2>Remove ${esc(a.name)}?</h2><p style="color:var(--ink2)">The app is taken off this mini PC. Folders on your NAS are never touched.</p>
    <label class="check"><input type="checkbox" data-data checked>Also delete the app's own data (settings, database)</label>
    <div class="foot"><button class="btn" data-close>Cancel</button><button class="btn danger" data-go>Remove</button></div>`);
  $('[data-go]', d.el).onclick = (e) => busy(e.currentTarget, async () => {
    await DEL(`/api/apps/${id}${$('[data-data]', d.el).checked ? '?data=1' : ''}`);
    d.close(); closeWin('app:' + id); closeWin('appset:' + id); toast(a.name + ' was removed.'); refresh();
  });
}

// Open an app: in a window when its page allows that, otherwise in a new tab.
async function openApp(id, newTab) {
  const a = S.apps.find((v) => v.id === id); if (!a) return;
  if (a.state === 'installing') return toast(a.name + ' is still installing.');
  if (a.state === 'updating') return toast(a.name + ' is being updated.');
  if (BAD.includes(a.state) || a.state === 'stopped' || a.noGui || !a.perms.includes('app.open')) return openAppSettings(id);
  if (WINS.has('app:' + id) && !newTab) return focusWin('app:' + id);
  let info; try { info = await GET(`/api/apps/${id}/open`); } catch (e) { return oops(e); }
  if (newTab || isPhone() || info.remote) { window.open(info.url, '_blank', 'noopener'); return; }
  openWin({ key: 'app:' + id, title: a.name, iconHTML: icon(a.storeId, a.name), dock: true, w: 1100, h: 720, nav: { url: info.url, frame: info.frameable }, render(body) {
    if (info.frameable) { body.innerHTML = `<iframe src="${esc(info.url)}" title="${esc(a.name)}" allow="fullscreen; autoplay; clipboard-write"></iframe>`; }
    else { body.innerHTML = `<div class="center">${icon(a.storeId, a.name, '4.5em')}<h2>${esc(a.name)} opens in its own tab</h2><p class="muted" style="max-width:26em">This app doesn't allow being shown inside a window.</p><a class="btn pri" href="${esc(info.url)}" target="_blank" rel="noopener">Open ${esc(a.name)}</a></div>`; }
  } });
}

// --- window manager ----------------------------------------------------------------
function openWin({ key, title, iconHTML, render, w = 900, h = 640, dock = false, nav = null }) {
  if (WINS.has(key)) { focusWin(key); return WINS.get(key); }
  const host = $('.wins'); const area = $('.desk').getBoundingClientRect();
  const saved = JSON.parse(localStorage.getItem('zw:' + key) || 'null');
  const maxH = area.height - (parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--dock')) || 76) - 16;
  w = Math.min(saved?.w || w, area.width - 24); h = Math.min(saved?.h || h, maxH);
  const n = WINS.size;
  const x = Math.max(8, Math.min(saved?.x ?? (area.width - w) / 2 + n * 22, area.width - w - 8)), y = Math.max(8, Math.min(saved?.y ?? Math.max(8, (maxH - h) / 2) + n * 22, maxH - h + 8));
  const node = el(`<section class="win ${saved?.max ? 'max' : ''}" style="left:${x}px;top:${y}px;width:${w}px;height:${h}px;pointer-events:auto" aria-label="${esc(title)}">
    <div class="tbar">${iconHTML}${nav && nav.frame ? `<span class="wnav"><button class="wbtn" data-nav="back" aria-label="Back" title="Back">${SVG.back}</button><button class="wbtn" data-nav="fwd" aria-label="Forward" title="Forward">${SVG.fwd}</button><button class="wbtn" data-nav="home" aria-label="Go to the app's start page" title="Start page">${SVG.home}</button></span>` : ''}<span class="ttl">${esc(title)}</span>
      ${nav ? `<button class="wbtn" data-nav="tab" aria-label="Open in a new tab" title="Open in a new tab">${SVG.tab}</button>` : ''}<button class="wbtn wmin" aria-label="Minimize">${SVG.min}</button><button class="wbtn wmax" aria-label="Maximize">${SVG.max}</button><button class="wbtn wclose" aria-label="Close">${SVG.x}</button></div>
    <div class="wbody"></div><div class="grip"></div></section>`);
  const win = { key, title, iconHTML, dock, el: node, body: $('.wbody', node) };
  WINS.set(key, win); host.append(node);
  const save = () => { const r = node.getBoundingClientRect(), a = $('.desk').getBoundingClientRect(); if (node.classList.contains('max')) { const o = JSON.parse(localStorage.getItem('zw:' + key) || '{}'); o.max = true; localStorage.setItem('zw:' + key, JSON.stringify(o)); } else localStorage.setItem('zw:' + key, JSON.stringify({ x: r.left - a.left, y: r.top - a.top, w: r.width, h: r.height })); };
  $('.wclose', node).onclick = () => closeWin(key);
  // Back and forward use the browser's own history, which is where pages inside app windows are recorded.
  $$('[data-nav]', node).forEach((b) => b.onclick = () => { const k = b.dataset.nav;
    if (k === 'back') history.back(); else if (k === 'fwd') history.forward();
    else if (k === 'tab') window.open(nav.url, '_blank', 'noopener');
    else { const f = $('iframe', node); if (f) f.src = nav.url; } });
  $('.wmin', node).onclick = () => { node.classList.add('min'); };
  const toggleMax = () => { node.classList.toggle('max'); save(); };
  $('.wmax', node).onclick = toggleMax;
  node.addEventListener('pointerdown', () => focusWin(key), true);
  // drag by the title bar; drop on an edge to snap
  const tb = $('.tbar', node);
  tb.addEventListener('dblclick', (e) => { if (!e.target.closest('.wbtn')) toggleMax(); });
  tb.addEventListener('pointerdown', (e) => {
    if (e.target.closest('.wbtn') || isPhone() || e.button !== 0) return;
    const a = $('.desk').getBoundingClientRect(); let r = node.getBoundingClientRect();
    const wasMax = node.classList.contains('max'); let ox = e.clientX - r.left, oy = e.clientY - r.top, moved = false, snap = null, hint = null;
    tb.setPointerCapture(e.pointerId); document.body.classList.add('dragging');
    const move = (ev) => {
      if (!moved && Math.abs(ev.clientX - e.clientX) + Math.abs(ev.clientY - e.clientY) < 5) return;
      if (!moved && wasMax) { node.classList.remove('max'); r = node.getBoundingClientRect(); ox = r.width / 2; }
      moved = true;
      node.style.left = Math.max(-r.width + 80, Math.min(ev.clientX - a.left - ox, a.width - 80)) + 'px';
      node.style.top = Math.max(0, Math.min(ev.clientY - a.top - oy, a.height - 60)) + 'px';
      snap = ev.clientY - a.top < 6 ? 'max' : ev.clientX - a.left < 6 ? 'left' : a.right - ev.clientX < 6 ? 'right' : null;
      hint?.remove(); hint = null;
      if (snap) { const dk = a.height - maxH; hint = el(`<div class="snaphint" style="top:6px;height:${a.height - dk - 4}px;${snap === 'max' ? 'left:6px;right:6px' : snap === 'left' ? 'left:6px;width:calc(50% - 9px)' : 'right:6px;width:calc(50% - 9px)'}"></div>`); host.append(hint); }
    };
    const up = () => { tb.removeEventListener('pointermove', move); tb.removeEventListener('pointerup', up); document.body.classList.remove('dragging'); hint?.remove();
      if (snap === 'max') node.classList.add('max');
      else if (snap) { node.style.top = '0px'; node.style.height = maxH + 8 + 'px'; node.style.width = a.width / 2 + 'px'; node.style.left = (snap === 'left' ? 0 : a.width / 2) + 'px'; }
      if (moved) save(); };
    tb.addEventListener('pointermove', move); tb.addEventListener('pointerup', up);
  });
  const grip = $('.grip', node);
  grip.addEventListener('pointerdown', (e) => { e.preventDefault(); const r = node.getBoundingClientRect(); grip.setPointerCapture(e.pointerId); document.body.classList.add('dragging');
    const move = (ev) => { node.style.width = Math.max(320, r.width + ev.clientX - e.clientX) + 'px'; node.style.height = Math.max(240, r.height + ev.clientY - e.clientY) + 'px'; };
    const up = () => { grip.removeEventListener('pointermove', move); grip.removeEventListener('pointerup', up); document.body.classList.remove('dragging'); save(); };
    grip.addEventListener('pointermove', move); grip.addEventListener('pointerup', up); });
  render(win.body, win);
  focusWin(key); renderDock();
  return win;
}
function focusWin(key) { const w = WINS.get(key); if (!w) return; w.el.classList.remove('min'); w.el.style.zIndex = ++zTop; }
function closeWin(key) { const w = WINS.get(key); if (!w) return; if (w.onClose) w.onClose(); w.el.remove(); WINS.delete(key); renderDock(); }

// --- search everything (Ctrl+K) ------------------------------------------------------
function openPalette() {
  if ($('.palette')) return;
  const scrim = el(`<div class="scrim" style="align-items:flex-start;background:rgba(17,24,39,.25)"><div class="palette" role="dialog" aria-label="Search"><input type="search" placeholder="Search apps, settings and actions" aria-label="Search"><div class="res"></div></div></div>`);
  $('#layer').append(scrim);
  const inp = $('input', scrim), res = $('.res', scrim); let items = [], sel = 0, storeTimer;
  const close = () => scrim.remove();
  scrim.addEventListener('mousedown', (e) => { if (e.target === scrim) close(); });
  const base = () => {
    const out = [];
    for (const a of S.apps) {
      out.push({ t: a.name, s: 'Open', i: icon(a.storeId, a.name), run: () => openApp(a.id) });
      out.push({ t: a.name + ' settings', s: 'App settings', i: icon(a.storeId, a.name), run: () => openAppSettings(a.id) });
      if (a.perms.includes('app.power')) out.push({ t: 'Restart ' + a.name, s: 'Action', i: icon(a.storeId, a.name), run: () => appAction(a.id, 'restart', 'Restarting ' + a.name + '…') });
      if (a.perms.includes('app.share')) out.push({ t: 'Share ' + a.name + ' online', s: 'Action', i: icon(a.storeId, a.name), run: () => openAppSettings(a.id, 'sharing') });
    }
    out.push({ t: 'Store', s: 'Get apps', i: sysIcon(SVG.store, '#111827'), run: openStore });
    for (const [k, label, show] of settingsSections()) if (show) out.push({ t: label, s: 'Settings', i: sysIcon(SVG.gear, '#475569'), run: () => openSettings(k) });
    out.push({ t: 'Sign out', s: 'Action', i: sysIcon(SVG.gear, '#475569'), run: async () => { await POST('/api/logout').catch(() => 0); S.user = null; boot(); } });
    return out;
  };
  const draw = () => { res.innerHTML = items.length ? '' : '<p class="muted" style="padding:.8em">Nothing found.</p>';
    items.forEach((it, i) => { const b = el(`<button class="${i === sel ? 'on' : ''}">${it.i}<span class="grow">${esc(it.t)}</span><span class="muted">${esc(it.s)}</span></button>`); b.onclick = () => { close(); it.run(); }; b.onmousemove = () => { if (sel !== i) { sel = i; $$('button', res).forEach((x, j) => x.classList.toggle('on', j === i)); } }; res.append(b); }); };
  const filter = () => { const q = inp.value.trim().toLowerCase(); sel = 0;
    items = base().filter((it) => !q || q.split(/\s+/).every((w) => it.t.toLowerCase().includes(w))).slice(0, 12); draw();
    clearTimeout(storeTimer);
    if (q.length > 1 && can('apps.install')) storeTimer = setTimeout(async () => { try { const r = await GET('/api/store?q=' + encodeURIComponent(q)); if (inp.value.trim().toLowerCase() !== q) return;
      for (const a of r.apps.filter((x) => !x.installed).slice(0, 6)) items.push({ t: a.name, s: 'In the Store', i: icon(a.id, a.name), run: () => openStore(a.id) }); draw(); } catch (e) { /* ignore */ } }, 200); };
  inp.oninput = filter;
  inp.onkeydown = (e) => { if (e.key === 'Escape') close(); else if (e.key === 'ArrowDown') { sel = Math.min(items.length - 1, sel + 1); draw(); e.preventDefault(); } else if (e.key === 'ArrowUp') { sel = Math.max(0, sel - 1); draw(); e.preventDefault(); } else if (e.key === 'Enter' && items[sel]) { close(); items[sel].run(); } };
  filter(); inp.focus();
}
document.addEventListener('keydown', (e) => {
  if (!S.user || !$('.desk')) return;
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); openPalette(); return; }
  // start typing anywhere on the desktop to search
  if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey && /\S/.test(e.key) && !e.target.closest('input,textarea,select,[contenteditable]') && !$('#layer').children.length && ![...WINS.values()].some((w) => !w.el.classList.contains('min'))) openPalette();
});
window.addEventListener('DOMContentLoaded', boot);
