// ZeroOS desktop UI: Store, app settings, Settings, NAS and playit flows, folder picker.
'use strict';

// --- adding a NAS share (used in setup and in Settings › Space) ----------------------
function nasFlow(box, onAdded) {
  box.innerHTML = `<div class="col">
    <div class="col" data-found><div class="row"><span class="spinner dark"></span><span class="muted">Looking for a NAS on your network…</span></div></div>
    <form class="col" data-login>
      <label class="f">NAS address<input type="text" name="host" placeholder="For example 192.168.1.20" required></label>
      <div class="row" style="align-items:flex-end"><label class="f grow">NAS username<input type="text" name="user" autocomplete="off"></label><label class="f grow">NAS password<input type="password" name="pass" autocomplete="off"></label></div>
      <p class="muted">Use an account from your NAS that can read and write the shares.</p>
      <div class="note bad hide" role="alert"></div>
      <div><button class="btn" type="submit">Show its shares</button></div>
    </form>
    <div class="col hide" data-shares></div><div class="col" data-added></div></div>`;
  const f = $('[data-login]', box), found = $('[data-found]', box), sharesBox = $('[data-shares]', box), added = $('[data-added]', box);
  POST('/api/storage/discover').then((r) => {
    if (!r.found.length) { found.innerHTML = `<p class="muted">No NAS found automatically. Type its address below.</p>`; return; }
    const row = (n) => `<button type="button" class="opt" data-host="${esc(n.host)}"><span class="grow"><strong>${esc(n.name)}</strong><br><span class="muted">${esc(n.host)}</span></span></button>`;
    const nas = r.found.filter((n) => n.nas), pcs = r.found.filter((n) => !n.nas);
    found.innerHTML = (nas.length ? `<h3>Found on your network</h3>` + nas.map(row).join('') : `<p class="muted">No NAS found automatically. Type its address below.</p>`)
      + (pcs.length ? `<details ${nas.length ? '' : 'open'}><summary class="muted" style="cursor:pointer">Other computers that share folders (${pcs.length})</summary><div class="col" style="margin-top:.5em">${pcs.map(row).join('')}</div></details>` : '');
    $$('[data-host]', found).forEach((b) => b.onclick = () => { $$('.opt', found).forEach((x) => x.classList.remove('sel')); b.classList.add('sel'); f.host.value = b.dataset.host; f.user.focus(); });
    if (nas.length === 1) $('[data-host]', found).click();
  }).catch(() => { found.innerHTML = ''; });
  f.onsubmit = async (e) => {
    e.preventDefault(); const note = $('.note', f); note.classList.add('hide');
    await busy($('button', f), async () => {
      try {
        const r = await POST('/api/storage/list', { host: f.host.value, username: f.user.value, password: f.pass.value });
        if (!r.shares.length) throw new Error("This NAS has no shares this account can see.");
        sharesBox.classList.remove('hide');
        sharesBox.innerHTML = `<h3>Which shares can apps use?</h3><div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(10em,1fr))">${r.shares.map((s) => `<label class="opt"><input type="checkbox" value="${esc(s)}"><strong>${esc(s)}</strong></label>`).join('')}</div><div class="note bad hide" role="alert"></div><div><button class="btn pri" type="button">Add selected</button></div>`;
        $('button', sharesBox).onclick = (ev) => busy(ev.currentTarget, async () => {
          const n2 = $('.note', sharesBox); n2.classList.add('hide');
          const picks = $$('input:checked', sharesBox).map((i) => i.value);
          if (!picks.length) { n2.textContent = 'Tick at least one share.'; n2.classList.remove('hide'); return; }
          for (const share of picks) {
            try { await POST('/api/storage/shares', { name: share, host: f.host.value, share, username: f.user.value, password: f.pass.value });
              added.append(el(`<div class="note ok">✓ ${esc(share)} is connected and can be read and written.</div>`));
              $(`input[value="${CSS.escape(share)}"]`, sharesBox).closest('label').remove();
            } catch (err) { n2.textContent = share + ': ' + err.message; n2.classList.remove('hide'); }
          }
          if (!$$('input[type=checkbox]', sharesBox).length) { sharesBox.classList.add('hide'); sharesBox.innerHTML = ''; }
          if (onAdded) onAdded();
        });
      } catch (err) { note.textContent = err.message; note.classList.remove('hide'); }
    });
  };
}

// --- linking playit (used in setup and in Settings › Sharing online) ------------------
function playitLinkBox(box, onLinked) {
  let timer;
  const stop = () => clearInterval(timer);
  const draw = async () => {
    let info; try { info = (await GET('/api/sharing')).playit; } catch (e) { box.innerHTML = `<div class="note bad">${esc(e.message)}</div>`; return; }
    if (info.linked) { stop(); box.innerHTML = `<div class="note ok">✓ playit is linked${info.premium ? ' (Premium)' : ''}. You can share apps online from each app's settings.</div>`; if (onLinked) onLinked(); return; }
    box.innerHTML = `<div class="col"><div><button class="btn pri" data-link>Link playit account</button></div><div data-wait class="col hide"></div>
      <details><summary class="muted" style="cursor:pointer">I already have an agent secret key</summary><form class="row" style="margin-top:.6em;flex-wrap:nowrap"><input type="password" name="k" placeholder="Secret key" aria-label="Secret key" required><button class="btn">Use key</button></form></details></div>`;
    $('[data-link]', box).onclick = (e) => busy(e.currentTarget, async () => {
      const r = await POST('/api/sharing/claim');
      const w = $('[data-wait]', box); w.classList.remove('hide');
      w.innerHTML = `<p>1. Open this link and sign in to playit.gg (or make a free account):</p><a class="btn" href="${esc(r.url)}" target="_blank" rel="noopener">Open playit.gg</a><p>2. Approve this computer there. This page continues by itself.</p><div class="row"><span class="spinner dark"></span><span class="muted" data-st>Waiting for you to open the link…</span></div>`;
      stop(); timer = setInterval(async () => {
        if (!document.body.contains(box)) return stop();
        try { const s = await GET('/api/sharing/claim'); const st = $('[data-st]', box);
          if (s.state === 'linked') { stop(); draw(); } else if (s.state === 'rejected') { stop(); w.innerHTML = '<div class="note bad">The link was declined on playit.gg. Try again.</div>'; } else if (st) st.textContent = s.state === 'approve' ? 'Waiting for you to approve it on playit.gg…' : 'Waiting for you to open the link…';
        } catch (err) { /* keep waiting */ }
      }, 3000);
    });
    $('form', box).onsubmit = (e) => { e.preventDefault(); busy($('button', e.target), async () => { await POST('/api/sharing/key', { secret: e.target.k.value }); draw(); }); };
  };
  draw();
}

// --- secure (https) addresses -------------------------------------------------------------
// playit only lets a person make these, on playit.gg. ZeroOS shows what to fill in there,
// then finds the tunnel by itself.
function secureBox(o) { // o: { secure, premium, port, what, canSetUp }
  const x = o.secure;
  if (x) return `<div class="box"><div class="between"><h3>Secure address</h3><span class="badge ${x.cert && !x.problem ? 'ok' : 'warn'}">${x.problem ? 'Switched off on playit.gg' : x.cert ? 'Encrypted' : 'Needs a change'}</span></div>
    <div class="row" style="flex-wrap:nowrap"><code class="addr grow">${esc(x.address)}</code><button class="btn sm" data-copy="${esc(x.address)}">Copy</button></div>
    ${x.problem ? `<div class="note warn">playit.gg has this tunnel switched off: ${esc(x.problem)}</div>` : x.cert ? `<span class="muted">Browsers and phone and TV apps trust this address, with no warning. Right after it is made, playit needs about a minute to fetch its certificate.</span>`
      : `<div class="note warn">This tunnel doesn't use playit's certificate yet, so the address won't open. On playit.gg, open the tunnel "${esc(x.name)}" and set <strong>TLS Termination</strong> to <strong>letsencrypt</strong>.</div>`}</div>`;
  if (!o.canSetUp) return '';
  if (!o.premium) return `<div class="box"><h3>Secure address</h3><span class="muted">An address that starts with https:// and opens without a warning needs playit Premium.</span></div>`;
  return `<div class="box"><div class="between"><h3>Secure address</h3><span class="badge prem">Premium</span></div>
    <span class="muted">An address that starts with https://, which browsers and phone and TV apps trust. playit only lets you make it on their website, once for ${esc(o.what)}. ZeroOS finds it by itself afterwards.</span>
    <details><summary class="link" style="cursor:pointer">Show me how</summary>
      <ol class="steps-list">
        <li>Open playit.gg and start a new tunnel.<div><a class="btn sm" href="https://playit.gg/account/setup/new-tunnel" target="_blank" rel="noopener">Open playit.gg</a></div></li>
        <li>As what to share, choose <strong>Website (HTTPS)</strong>.</li>
        <li>As the computer, choose <strong>ZeroOS</strong>.</li>
        <li>Fill in <strong>Http Port</strong> <code class="addr">${o.port}</code> <button class="btn sm" data-copy="${o.port}">Copy</button> and set <strong>TLS Termination</strong> to <strong>letsencrypt</strong>.</li>
        <li>As the address, pick a name. Premium includes three names that end in .playit.plus, and a name under one you already have (like jellyfin.yourname.playit.plus) doesn't use up another.</li>
      </ol>
      <div class="row"><button class="btn sm" data-seccheck>I've made it, check now</button><span class="muted" data-secmsg></span></div>
    </details></div>`;
}
function secureWire(root, redraw, found) { // found(): asks ZeroOS again and tells whether the tunnel is there now
  $$('[data-copy]', root).forEach((b) => { if (!b.onclick) b.onclick = () => copyText(b.dataset.copy); });
  const c = $('[data-seccheck]', root);
  if (c) c.onclick = () => busy(c, async () => { await POST('/api/sharing/refresh');
    if (await found()) { toast('Found it.'); redraw(); } else $('[data-secmsg]', root).textContent = 'Not found yet. Check the Http Port on playit.gg, then try again.'; });
}

// --- folder picker ---------------------------------------------------------------------
function pickFolder(title, hint) {
  return new Promise((resolve) => {
    let done = false, root = null, path = [];
    const d = dialog(`<h2>${esc(title)}</h2>${hint ? `<p class="muted">${esc(hint)}</p>` : ''}<nav class="row" data-crumbs style="gap:.3em;font-size:.9em" aria-label="Folder path"></nav><div class="list" data-list style="max-height:42vh;overflow:auto"></div>
      <div class="between"><button class="btn sm hide" data-new>New folder</button><span></span></div><div class="foot"><button class="btn" data-close>Cancel</button><button class="btn pri" data-ok disabled>Choose this folder</button></div>`, { wide: true, onClose: () => { if (!done) resolve(null); } });
    const list = $('[data-list]', d.el), crumbs = $('[data-crumbs]', d.el), okb = $('[data-ok]', d.el), newb = $('[data-new]', d.el);
    const draw = async () => {
      okb.disabled = !root; newb.classList.toggle('hide', !root);
      crumbs.innerHTML = `<button class="link" data-i="-1">All places</button>` + (root ? ` / <button class="link" data-i="0">${esc(root.label)}</button>` : '') + path.map((p, i) => ` / <button class="link" data-i="${i + 1}">${esc(p)}</button>`).join('');
      $$('button', crumbs).forEach((b) => b.onclick = () => { const i = +b.dataset.i; if (i < 0) { root = null; path = []; } else path = path.slice(0, i); draw(); });
      list.innerHTML = '<div class="row" style="padding:1em"><span class="spinner dark"></span></div>';
      try {
        if (!root) {
          const roots = await GET('/api/folders/roots');
          list.innerHTML = '';
          for (const r of roots) { const b = el(`<button class="frow" ${r.connected ? '' : 'disabled'}>${SVG.folder}<span class="grow"><strong>${esc(r.label)}</strong></span>${r.connected ? '' : '<span class="badge warn">Not connected</span>'}</button>`); b.onclick = () => { root = r; path = []; draw(); }; list.append(b); }
          if (roots.length === 1) list.append(el(`<p class="muted" style="padding:.8em 1em">No NAS share added yet. Add one under Settings › Space to keep big files on your NAS.</p>`));
          return;
        }
        const items = await GET(`/api/folders?root=${encodeURIComponent(root.id)}&path=${encodeURIComponent('/' + path.join('/'))}`);
        list.innerHTML = items.length ? '' : '<p class="muted" style="padding:.8em 1em">No folders in here. You can choose this folder, or make a new one.</p>';
        for (const it of items) { const b = el(`<button class="frow">${SVG.folder}<span class="grow">${esc(it.name)}</span><span class="muted">${it.items} ${it.items === 1 ? 'item' : 'items'}</span></button>`); b.onclick = () => { path.push(it.name); draw(); }; list.append(b); }
      } catch (e) { list.innerHTML = `<div class="note bad" style="margin:.8em">${esc(e.message)}</div>`; }
    };
    newb.onclick = () => { const n = dialog(`<h2>New folder</h2><form class="col"><label class="f">Name<input type="text" name="n" required></label><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Create</button></div></form>`);
      $('form', n.el).onsubmit = (e) => { e.preventDefault(); busy($('button.pri', n.el), async () => { await POST('/api/folders', { root: root.id, path: '/' + path.join('/'), name: e.target.n.value }); path.push(e.target.n.value.trim()); n.close(); draw(); }); }; };
    okb.onclick = () => { done = true; d.close(); resolve({ root: root.id, path: '/' + path.join('/'), label: root.label + (path.length ? ' / ' + path.join(' / ') : '') }); };
    draw();
  });
}

// --- Store ---------------------------------------------------------------------------------
const catName = (c) => ({ ai: 'AI', iot: 'Smart home' }[c] || c.charAt(0).toUpperCase() + c.slice(1));
function openStore(showApp) {
  const win = openWin({ key: 'store', title: 'Store', iconHTML: sysIcon(SVG.store, '#111827'), w: 1060, h: 700, render(body, w) {
    body.innerHTML = `<nav class="side" aria-label="Categories"></nav><div class="pane"></div>`;
    w.state = { q: '', cat: '', verified: false, installedOnly: false };
    w.seq = 0; w.list = () => storeList(w, ++w.seq); w.show = (id) => { clearTimeout(w.qt); return storeApp(w, id, ++w.seq); }; // seq: a slow answer never draws over a newer page
    w.list();
  } });
  if (typeof showApp === 'string') win.show(showApp);
}
async function storeList(w, seq) {
  const pane = $('.pane', w.body), side = $('.side', w.body), st = w.state;
  let r; try { r = await GET(`/api/store?q=${encodeURIComponent(st.q)}&cat=${encodeURIComponent(st.cat)}${st.verified ? '&verified=1' : ''}`); } catch (e) { if (seq === w.seq) pane.innerHTML = `<div class="note bad">${esc(e.message)}</div>`; return; }
  if (seq !== w.seq) return;
  side.innerHTML = `<button data-c="" class="${!st.cat && !st.installedOnly ? 'on' : ''}">All apps</button>${r.categories.map((c) => `<button data-c="${esc(c)}" class="${st.cat === c ? 'on' : ''}">${esc(catName(c))}</button>`).join('')}<hr><button data-inst class="${st.installedOnly ? 'on' : ''}">Installed</button>`;
  $$('[data-c]', side).forEach((b) => b.onclick = () => { st.cat = b.dataset.c; st.installedOnly = false; w.list(); });
  $('[data-inst]', side).onclick = () => { st.installedOnly = true; st.cat = ''; w.list(); };
  const apps = st.installedOnly ? r.apps.filter((a) => a.installed) : r.apps;
  if (!$('[data-q]', pane)) {
    pane.innerHTML = `<div class="row"><div class="grow" style="position:relative;flex:1 1 18em"><input type="search" data-q placeholder="Search, for example photos, passwords or Minecraft" aria-label="Search the store" style="padding-left:2.4em"><span style="position:absolute;left:.8em;top:.85em;color:var(--mute)">${SVG.search}</span></div>
      <label class="check"><input type="checkbox" data-v>Verified only</label></div><p class="muted" data-info></p><div class="cards" data-cards></div>`;
    $('[data-q]', pane).oninput = (e) => { clearTimeout(w.qt); w.qt = setTimeout(() => { st.q = e.target.value; w.list(); }, 200); };
    $('[data-v]', pane).onchange = (e) => { st.verified = e.target.checked; w.list(); };
  }
  $('[data-info]', pane).innerHTML = (r.loading ? `The store is still loading the rest of its catalog. ${can('store.repos') ? '<button class="link" data-sync>Load it now</button>' : 'Try again in a moment.'} ` : '') + (r.total ? `${apps.length} of ${r.total} apps. <span class="badge ok">✓ Verified</span> passed ZeroOS's install test. <span class="badge mute">Community</span> comes straight from the catalog.` : '');
  const sy = $('[data-sync]', pane); if (sy) sy.onclick = (e) => busy(e.currentTarget, async () => { await POST('/api/store/sync'); w.list(); });
  const cards = $('[data-cards]', pane); cards.innerHTML = '';
  if (r.total && !apps.length) cards.innerHTML = '<p class="muted">No apps match.</p>';
  for (const a of apps) {
    const c = el(`<button class="scard"><div class="row" style="flex-wrap:nowrap">${icon(a.id, a.name)}<div class="col grow" style="gap:.25em"><strong>${esc(a.name)}</strong><span class="badge ${a.verified ? 'ok' : 'mute'}" style="align-self:flex-start">${a.verified ? '✓ Verified' : 'Community'}</span></div></div><p>${esc(a.shortDesc)}</p><div class="between"><span class="muted">${a.installed ? 'Installed' : ''}</span><span class="btn sm ${a.installed ? '' : 'pri'}">${a.installed ? 'View' : 'Get'}</span></div></button>`);
    c.onclick = () => w.show(a.id); cards.append(c);
  }
}
async function storeApp(w, id, seq) {
  const pane = $('.pane', w.body);
  let r; try { r = await GET('/api/store/' + encodeURIComponent(id)); } catch (e) { return oops(e); }
  if (seq !== w.seq) return;
  const a = r.app, inst = S.apps.find((x) => x.storeId === id);
  const about = (a.description || '').split(/\n#+\s*Folder Info/i)[0].replace(/^#+\s*/gm, '').replace(/\[([^\]]+)\]\([^)]+\)/g, '$1').replace(/[*_`]{1,2}/g, '').trim();
  pane.innerHTML = `<div><button class="link" data-back>← Store</button></div>
    <div class="between"><div class="row" style="flex-wrap:nowrap">${icon(a.id, a.name, '4.4em')}<div class="col" style="gap:.3em"><h1>${esc(a.name)}</h1><div class="row" style="gap:.4em"><span class="badge ${a.verified ? 'ok' : 'mute'}">${a.verified ? '✓ Verified' : 'Community'}</span>${a.categories.map((c) => `<span class="badge info">${esc(catName(c))}</span>`).join('')}</div></div></div>
      <div class="row">${inst ? `<button class="btn" data-open>Open</button>` : ''}${can('apps.install') ? `<button class="btn pri" data-get>${inst ? 'Get another copy' : 'Get'}</button>` : ''}</div></div>
    <p style="font-size:1.05em">${esc(a.shortDesc)}</p>
    ${a.usesDocker ? '<div class="note warn">This app controls Docker on the whole mini PC (it needs that to do its job), so only the owner can install it.</div>' : ''}
    <div class="desc">${esc(about)}</div>
    ${a.website ? `<p class="muted">Website: <a href="${esc(a.website)}" target="_blank" rel="noopener noreferrer">${esc(a.website)}</a></p>` : ''}`;
  pane.scrollTop = 0;
  $('[data-back]', pane).onclick = () => { pane.innerHTML = ''; w.list(); };
  const ob = $('[data-open]', pane); if (ob) ob.onclick = () => openApp(inst.id);
  const gb = $('[data-get]', pane); if (gb) gb.onclick = () => installWizard(a, r.gpuAvailable);
}

// The install wizard asks only what has no good default.
function installWizard(a, gpuAvailable) {
  const fields = a.form.filter((f) => f.type !== 'random');
  const folders = {};
  const fieldHTML = (f) => {
    const id = 'ff-' + f.env_variable, def = f.default ?? '';
    if (f.type === 'boolean') return `<label class="check"><input type="checkbox" data-env="${esc(f.env_variable)}" ${def === true || def === 'true' ? 'checked' : ''}>${esc(f.label)}</label>`;
    if (f.options && f.options.length) return `<label class="f">${esc(f.label)}<select data-env="${esc(f.env_variable)}">${f.options.map((o) => `<option value="${esc(o.value)}" ${String(o.value) === String(def) ? 'selected' : ''}>${esc(o.label)}</option>`).join('')}</select>${f.hint ? `<span class="muted" style="font-weight:400">${esc(f.hint)}</span>` : ''}</label>`;
    const type = f.type === 'password' ? 'password' : f.type === 'number' ? 'number' : f.type === 'email' ? 'email' : 'text';
    return `<label class="f" for="${id}">${esc(f.label)}${f.required ? '' : ' (optional)'}<input id="${id}" type="${type}" data-env="${esc(f.env_variable)}" value="${esc(def)}" placeholder="${esc(f.placeholder || '')}" ${f.required ? 'required' : ''} autocomplete="off">${f.hint ? `<span class="muted" style="font-weight:400">${esc(f.hint)}</span>` : ''}</label>`;
  };
  const d = dialog(`<div class="row" style="flex-wrap:nowrap">${icon(a.id, a.name, '3.2em')}<div class="col" style="gap:.2em"><h2>Get ${esc(a.name)}</h2><span class="muted">${a.folders.length + fields.length ? 'A few quick questions, then it installs.' : 'Nothing to set up. It just installs.'}</span></div></div>
    <form class="col">
      ${a.folders.map((n, i) => `<div class="box"><strong>${esc(n.label)}</strong><span class="muted">${esc(n.hint)}</span><div class="row"><code class="addr grow" data-fl="${i}">On this mini PC</code><button type="button" class="btn sm" data-pick="${i}">Choose folder</button></div></div>`).join('')}
      ${fields.map(fieldHTML).join('')}
      ${gpuAvailable ? `<label class="check"><input type="checkbox" data-gpu>Let it use this mini PC's graphics chip (faster video)</label>` : ''}
      <p class="muted">Its own data goes on this mini PC's SSD. You get an Open button; no ports to think about.</p>
      <div class="note bad hide" role="alert"></div>
      <div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri" type="submit">Install</button></div>
    </form>`, { wide: true });
  $$('[data-pick]', d.el).forEach((b) => b.onclick = async () => { const n = a.folders[+b.dataset.pick]; const p = await pickFolder(n.label, n.hint); if (p) { folders[n.key] = { root: p.root, path: p.path }; $(`[data-fl="${b.dataset.pick}"]`, d.el).textContent = p.label; } });
  $('form', d.el).onsubmit = async (e) => {
    e.preventDefault(); const note = $('.note', d.el); note.classList.add('hide');
    const env = {}; $$('[data-env]', d.el).forEach((i) => env[i.dataset.env] = i.type === 'checkbox' ? String(i.checked) : i.value);
    await busy($('button[type=submit]', d.el), async () => {
      try { const r = await POST('/api/apps', { storeId: a.id, env, folders, gpu: !!$('[data-gpu]', d.el)?.checked }); d.close(); refresh(); installProgress(a, r.job, r.appId); }
      catch (err) { note.textContent = err.message; note.classList.remove('hide'); }
    });
  };
}
function installProgress(a, jobId, appId) {
  const d = dialog(`<div class="row" style="flex-wrap:nowrap">${icon(a.id, a.name, '3.2em')}<div class="col grow" style="gap:.2em"><h2>Installing ${esc(a.name)}</h2><div class="row"><span class="spinner dark" data-sp></span><span data-step class="muted">Starting…</span></div></div></div>
    <details><summary class="muted" style="cursor:pointer">Details</summary><pre class="log" data-log style="max-height:30vh;margin-top:.6em"></pre></details>
    <div class="foot"><button class="btn" data-close>Hide (it keeps installing)</button></div>`, { wide: true });
  const t = setInterval(async () => {
    let j; try { j = await GET('/api/jobs/' + jobId); } catch (e) { return; }
    if (!document.body.contains(d.el)) { if (j.done) { clearInterval(t); refresh(); toast(j.error ? a.name + " didn't install." : a.name + ' is ready.', !!j.error); } return; }
    $('[data-step]', d.el).textContent = j.step; const lg = $('[data-log]', d.el); lg.textContent = j.lines.join('\n'); lg.scrollTop = lg.scrollHeight;
    if (!j.done) return;
    clearInterval(t); refresh(); $('[data-sp]', d.el).remove();
    const foot = $('.foot', d.el);
    if (j.error) { $('[data-step]', d.el).innerHTML = `<span style="color:var(--bad)">${esc(j.error)}</span>`; $('details', d.el).open = true; foot.innerHTML = `<button class="btn" data-x>Close</button><button class="btn danger" data-rm>Remove it</button>`; $('[data-rm]', d.el).onclick = async () => { await DEL(`/api/apps/${appId}?data=1`).catch(oops); d.close(); refresh(); }; }
    else { $('[data-step]', d.el).textContent = a.name + ' is ready. It is on your desktop.'; foot.innerHTML = `<button class="btn" data-x>Close</button>${a.noGui ? '' : '<button class="btn pri" data-o>Open</button>'}`; const o = $('[data-o]', d.el); if (o) o.onclick = async () => { d.close(); await refresh(); openApp(appId); }; }
    $('[data-x]', d.el).onclick = d.close;
  }, 1200);
}

// --- one app's settings window ---------------------------------------------------------------
const STATE_TEXT = { running: 'Working normally', starting: 'Starting up…', stopped: 'Stopped', waiting: 'Waiting for your NAS', crashed: 'Stopped unexpectedly', failed: "Didn't install", installing: 'Installing…', removing: 'Removing…' };
function openAppSettings(id, tab) {
  const a = S.apps.find((v) => v.id === id); if (!a) return;
  const win = openWin({ key: 'appset:' + id, title: a.name + ' settings', iconHTML: icon(a.storeId, a.name), w: 860, h: 620, render(body, w) {
    body.innerHTML = `<nav class="side" aria-label="${esc(a.name)} settings"></nav><div class="pane"></div>`;
    w.go = (t) => { w.tab = t; appSettingsDraw(w, id); };
    w.tab = 'overview';
  } });
  win.go(tab || win.tab || 'overview');
}
async function appSettingsDraw(w, id) {
  const side = $('.side', w.body), pane = $('.pane', w.body);
  let d; try { d = await GET('/api/apps/' + id); } catch (e) { pane.innerHTML = `<div class="note bad">${esc(e.message)}</div>`; return; }
  const a = d.app, has = (p) => d.perms.includes(p);
  const tabs = [['overview', 'Overview', true], ['folders', 'Folders', has('app.settings')], ['sharing', 'Sharing', has('app.share')], ['access', 'Who can use it', has('app.access')], ['activity', 'Activity', true], ['log', 'Log', has('app.settings')]].filter((t) => t[2]);
  if (!tabs.some((t) => t[0] === w.tab)) w.tab = 'overview';
  side.innerHTML = tabs.map(([k, l]) => `<button data-t="${k}" class="${w.tab === k ? 'on' : ''}">${l}</button>`).join('');
  $$('[data-t]', side).forEach((b) => b.onclick = () => w.go(b.dataset.t));
  const redraw = () => { refresh(); appSettingsDraw(w, id); };
  const running = !['stopped', 'failed', 'installing'].includes(a.state);
  if (w.tab === 'overview') {
    const bad = ['waiting', 'crashed', 'failed'].includes(a.state);
    pane.innerHTML = `<div class="between"><div class="row" style="flex-wrap:nowrap">${icon(a.storeId, a.name, '3.8em')}<div class="col" style="gap:.25em"><h1>${esc(a.name)}</h1><span class="${bad ? '' : 'muted'}" ${bad ? 'style="color:var(--warn);font-weight:600"' : ''}>${esc(STATE_TEXT[a.state] || a.state)}</span></div></div>
        <div class="row">${has('app.open') && !a.noGui && running ? `<button class="btn" data-tab>Open in a new tab</button><button class="btn pri" data-open>Open</button>` : ''}</div></div>
      ${a.problem ? `<div class="note warn">${esc(a.problem)}</div>` : ''}
      ${a.noGui ? '<div class="note info">This app has no page of its own to open. It works in the background.</div>' : ''}
      <div class="cards">
        ${d.folders.length ? `<div class="box"><div class="between"><h3>Folders</h3>${has('app.settings') ? '<button class="link" data-go="folders">Change</button>' : ''}</div>${d.folders.map((f) => `<span>${esc(f.label)}: <strong>${esc(f.rootLabel)}${f.path && f.path !== '/' ? esc(f.path.replace(/\//g, ' / ')) : ''}</strong>${f.connected ? '' : ' <span class="badge warn">Not connected</span>'}</span>`).join('')}<span class="muted">Its own data stays on this mini PC's SSD.</span></div>` : ''}
        ${has('app.share') && !a.noGui ? `<div class="box"><div class="between"><h3>Share online</h3><button class="link" data-go="sharing">${d.tunnel || d.secure ? 'Manage' : 'Set up'}</button></div>${d.secure ? `<code class="addr">${esc(d.secure.address)}</code>` : ''}${d.tunnel ? (d.tunnel.address ? `<code class="addr">${esc(d.tunnel.address)}</code>` : '<span class="muted">Getting its address…</span>') : (d.secure ? '' : '<span class="muted">Only reachable at home.</span>')}</div>` : ''}
        <div class="box"><div class="between"><h3>Who can use it</h3>${has('app.access') ? '<button class="link" data-go="access">Change</button>' : ''}</div><div class="row" style="gap:.3em">${d.people.map((p) => `<span class="av" style="background:${colorOf(p)};color:#fff" title="${esc(p)}">${esc(p[0].toUpperCase())}</span>`).join('')}</div><span class="muted">${esc(d.people.join(', '))}</span></div>
        ${d.gpuAvailable && has('app.settings') ? `<div class="box"><div class="between"><h3>Graphics chip</h3><input type="checkbox" class="switch" data-gpu ${a.settings.gpu ? 'checked' : ''} aria-label="Let this app use the graphics chip"></div><span class="muted">Lets the app use this mini PC's graphics chip, for faster video.</span></div>` : ''}
      </div>
      <div class="row">${has('app.power') ? (running ? `<button class="btn" data-act="restart">Restart</button><button class="btn" data-act="stop">Stop</button>` : a.state !== 'installing' ? `<button class="btn pri" data-act="start">Start</button>` : '') : ''}${has('app.remove') ? `<button class="btn danger" data-rm>Remove</button>` : ''}</div>
      ${d.store ? `<p class="muted">From the Store${d.store.verified ? ' · Verified' : ' · Community'}${d.store.website ? ` · <a href="${esc(d.store.website)}" target="_blank" rel="noopener noreferrer">Website</a>` : ''}</p>` : ''}`;
    const o = $('[data-open]', pane); if (o) { o.onclick = () => openApp(id); $('[data-tab]', pane).onclick = () => openApp(id, true); }
    $$('[data-go]', pane).forEach((b) => b.onclick = () => w.go(b.dataset.go));
    $$('[data-act]', pane).forEach((b) => b.onclick = () => busy(b, async () => { await POST(`/api/apps/${id}/${b.dataset.act}`); redraw(); }));
    const rm = $('[data-rm]', pane); if (rm) rm.onclick = () => removeApp(id);
    const g = $('[data-gpu]', pane); if (g) g.onchange = async () => { try { await PUT(`/api/apps/${id}/gpu`, { on: g.checked }); toast('Saved. The app restarts to use it.'); } catch (e) { g.checked = !g.checked; oops(e); } };
  } else if (w.tab === 'folders') {
    pane.innerHTML = `<h1>Folders</h1><p class="muted">Big files live in the folders below. ${esc(a.name)}'s own data always stays on this mini PC's SSD.</p>${d.folders.length ? '' : `<p>${esc(a.name)} doesn't use any big-file folders.</p>`}
      ${d.folders.map((f, i) => `<div class="box"><strong>${esc(f.label)}</strong><div class="row"><code class="addr grow">${esc(f.rootLabel)}${f.path && f.path !== '/' ? esc(f.path.replace(/\//g, ' / ')) : ''}</code>${f.connected ? '' : '<span class="badge warn">Not connected</span>'}<button class="btn sm" data-ch="${i}">Change</button></div></div>`).join('')}`;
    $$('[data-ch]', pane).forEach((b) => b.onclick = async () => { const f = d.folders[+b.dataset.ch]; const p = await pickFolder(f.label); if (!p) return;
      busy(b, async () => { await PUT(`/api/apps/${id}/folders`, { key: f.key, root: p.root, path: p.path }); toast('Folder changed. The app restarts to use it.'); redraw(); }); });
  } else if (w.tab === 'sharing') {
    pane.innerHTML = `<h1>Share online</h1><p class="muted">Reach ${esc(a.name)} from outside your home through playit.gg. No router setup needed.</p>
      ${!d.playitLinked ? `<div class="note info">Link a playit account first.</div><div><button class="btn pri" data-pl>Set up sharing</button></div>` : `
      <div class="box"><div class="between"><strong>Share ${esc(a.name)} online</strong><input type="checkbox" class="switch" data-sw ${d.tunnel ? 'checked' : ''} aria-label="Share ${esc(a.name)} online"></div>
      ${d.tunnel ? (d.tunnel.address ? `<div class="row" style="flex-wrap:nowrap"><code class="addr grow">${esc(d.tunnel.address)}</code><button class="btn sm" data-copy1>Copy</button></div><span class="muted">Anyone with this address reaches ${esc(a.name)}'s own sign-in page. Use a strong password there.</span>` : '<div class="row"><span class="spinner dark"></span><span class="muted">Getting its address from playit…</span></div>') : '<span class="muted">Off: only reachable at home.</span>'}</div>`}`;
    if (d.playitLinked) { pane.insertAdjacentHTML('beforeend', secureBox({ secure: d.secure, premium: d.playitPremium, port: d.port, what: 'each app', canSetUp: true })); secureWire(pane, redraw, async () => !!(await GET(`/api/apps/${id}`)).secure); }
    const pl = $('[data-pl]', pane); if (pl) pl.onclick = () => openSettings('sharing');
    const sw = $('[data-sw]', pane); if (sw) sw.onchange = async () => { sw.disabled = true; try { await PUT(`/api/apps/${id}/share`, { on: sw.checked }); } catch (e) { oops(e); } redraw(); };
    const cp = $('[data-copy1]', pane); if (cp) cp.onclick = () => copyText(d.tunnel.address);
    if (d.tunnel && !d.tunnel.address) setTimeout(() => { if (w.tab === 'sharing' && document.body.contains(pane)) appSettingsDraw(w, id); }, 5000);
  } else if (w.tab === 'access') {
    let acc; try { acc = await GET(`/api/apps/${id}/access`); } catch (e) { pane.innerHTML = `<div class="note bad">${esc(e.message)}</div>`; return; }
    const cols = acc.perms.filter((p) => p.name !== 'app.view' && d.perms.includes(p.name));
    const short = { 'app.open': 'Open', 'app.power': 'Start/stop', 'app.settings': 'Settings', 'app.share': 'Share online', 'app.backups': 'Backups', 'app.update': 'Update', 'app.access': 'Give access', 'app.remove': 'Remove' };
    pane.innerHTML = `<h1>Who can use it</h1><p class="muted">Tick what each person may do with ${esc(a.name)}. People with nothing ticked don't see it at all.</p>
      <div class="tablebox"><table><thead><tr><th scope="col">Person</th>${cols.map((p) => `<th scope="col" title="${esc(p.label)}">${esc(short[p.name] || p.label)}</th>`).join('')}</tr></thead><tbody>
      ${acc.users.map((u) => `<tr><td><strong>${esc(u.username)}</strong><br><span class="muted">${esc(u.role || 'No role')}</span></td>${cols.map((p) => `<td><input type="checkbox" data-u="${u.id}" data-p="${p.name}" ${u.perms.includes(p.name) ? 'checked' : ''} ${u.locked ? 'disabled' : ''} aria-label="${esc(u.username)}: ${esc(p.label)}"></td>`).join('')}</tr>`).join('')}
      </tbody></table></div><p class="muted">Owners and admins can always do everything.</p>`;
    $$('input[data-u]', pane).forEach((c) => c.onchange = async () => { const uid = +c.dataset.u; const perms = $$(`input[data-u="${uid}"]:checked`, pane).map((x) => x.dataset.p);
      try { await PUT(`/api/apps/${id}/access`, { userId: uid, perms }); } catch (e) { c.checked = !c.checked; oops(e); } });
  } else if (w.tab === 'activity') {
    pane.innerHTML = `<h1>Activity</h1>${await activityHTML('?app=' + encodeURIComponent(id))}`;
  } else if (w.tab === 'log') {
    pane.innerHTML = `<div class="between"><h1>Log</h1><button class="btn sm" data-r>Refresh</button></div><p class="muted">What the app itself has printed. Handy when something isn't working.</p><pre class="log" data-l>Loading…</pre>`;
    const load = async () => { try { const r = await GET(`/api/apps/${id}/logs`); const l = $('[data-l]', pane); l.textContent = r.log || 'Nothing yet.'; l.scrollTop = l.scrollHeight; } catch (e) { $('[data-l]', pane).textContent = e.message; } };
    $('[data-r]', pane).onclick = load; load();
  }
}
async function copyText(t) { try { await navigator.clipboard.writeText(t); toast('Copied.'); } catch (e) { const d = dialog(`<h2>Copy this address</h2><input type="text" readonly value="${esc(t)}"><div class="foot"><button class="btn pri" data-close>Done</button></div>`); $('input', d.el).select(); } }
async function activityHTML(query) {
  let rows; try { rows = await GET('/api/activity' + (query || '')); } catch (e) { return `<div class="note bad">${esc(e.message)}</div>`; }
  if (!rows.length) return '<p class="muted">Nothing has happened yet.</p>';
  return `<div class="list">${rows.map((r) => `<div class="lrow"><span class="grow"><strong>${esc(r.user)}</strong> ${esc(r.action)} ${r.target ? `<strong>${esc(r.target)}</strong>` : ''}${r.ok ? '' : ` <span class="badge bad">Didn't work</span>`}${!r.ok && r.detail ? `<br><span class="muted">${esc(r.detail)}</span>` : ''}</span><span class="muted">${fmtTime(r.ts)}</span></div>`).join('')}</div>`;
}

// --- Settings window -----------------------------------------------------------------------------
const settingsSections = () => [['users', 'Users and roles', can('users.manage')], ['space', 'Space', true], ['sharing', 'Sharing online', true], ['system', 'System', can('system.view')], ['activity', 'Activity', true], ['profile', 'Profile', true]];
function openSettings(section) {
  const win = openWin({ key: 'settings', title: 'Settings', iconHTML: sysIcon(SVG.gear, '#475569'), w: 980, h: 680, render(body, w) {
    body.innerHTML = `<nav class="side" aria-label="Settings sections"></nav><div class="pane"></div>`;
    w.go = (s) => { w.sec = s; settingsDraw(w); };
  } });
  win.go(section || win.sec || settingsSections().find((s) => s[2])[0]);
}
function settingsDraw(w) {
  const side = $('.side', w.body), pane = $('.pane', w.body);
  const secs = settingsSections().filter((s) => s[2]);
  if (!secs.some((s) => s[0] === w.sec)) w.sec = secs[0][0];
  side.innerHTML = secs.map(([k, l]) => `<button data-s="${k}" class="${w.sec === k ? 'on' : ''}">${l}</button>`).join('');
  $$('[data-s]', side).forEach((b) => b.onclick = () => w.go(b.dataset.s));
  pane.innerHTML = '<div class="row"><span class="spinner dark"></span></div>';
  ({ users: setUsers, space: setSpace, sharing: setSharing, system: setSystem, activity: setActivity, profile: setProfile })[w.sec](w, pane).catch((e) => { pane.innerHTML = `<div class="note bad">${esc(e.message)}</div>`; });
}

async function setSpace(w, pane) {
  const d = await GET('/api/storage');
  pane.innerHTML = `<div class="between"><div class="col" style="gap:.2em"><h1>Space</h1><span class="muted">Apps keep their own data on the SSD. Big files live on your NAS.</span></div>${d.canManage ? '<button class="btn pri" data-add>Add NAS share</button>' : ''}</div>
    <div class="box"><div class="row" style="flex-wrap:nowrap"><span class="ic" style="--c:var(--accent-l);--s:2.6em;color:var(--accent-d)">${SVG.folder}</span><div class="col grow" style="gap:.1em"><h3>This mini PC's SSD</h3><span class="muted">App data and databases</span></div></div><div class="meter"><span style="width:${pct(d.ssd.used, d.ssd.total)}%"></span></div><div class="between"><span class="muted">${fmtBytes(d.ssd.used)} used</span><span class="muted">${fmtBytes(d.ssd.free)} free</span></div></div>
    <h2>On your NAS</h2>${d.shares.length ? '' : '<p class="muted">No NAS share added yet.</p>'}
    <div class="cards">${d.shares.map((s) => `<div class="box" ${s.connected ? '' : 'style="border-color:#E8C79A;background:#FFFBF4"'}><div class="between" style="flex-wrap:nowrap"><div class="col" style="gap:.1em"><h3>${esc(s.name)}</h3><span class="muted">${esc(s.host)}</span></div><span class="badge ${s.connected ? 'ok' : 'warn'}">${s.connected ? 'Connected' : 'Reconnecting'}</span></div>
      ${s.connected && s.usage ? `<div class="meter"><span style="width:${pct(s.usage.used, s.usage.total)}%"></span></div><div class="between"><span class="muted">${fmtBytes(s.usage.used)} used</span><span class="muted">${fmtBytes(s.usage.free)} free</span></div>` : ''}
      ${s.connected ? '' : `<span>${esc(s.problem || "Can't reach this share right now.")} ZeroOS keeps trying every 30 seconds, and apps that use it wait and start by themselves.</span>`}
      <span class="muted">${s.usedBy.length ? 'Used by ' + esc(s.usedBy.join(', ')) : 'Not used by any app yet'}</span>
      ${d.canManage ? `<div class="row">${s.connected ? '' : `<button class="btn sm" data-re="${s.id}">Try now</button>`}<button class="btn sm" data-login="${s.id}">Change login</button><button class="btn sm danger" data-rm="${s.id}">Remove</button></div>` : ''}</div>`).join('')}</div>`;
  const again = () => settingsDraw(w);
  const add = $('[data-add]', pane); if (add) add.onclick = () => { const dl = dialog(`<h2>Add a NAS share</h2><div data-n></div><div class="foot"><button class="btn pri" data-close>Done</button></div>`, { wide: true, onClose: again }); nasFlow($('[data-n]', dl.el)); };
  $$('[data-re]', pane).forEach((b) => b.onclick = () => busy(b, async () => { await POST(`/api/storage/shares/${b.dataset.re}/reconnect`); toast('Connected.'); again(); }));
  $$('[data-rm]', pane).forEach((b) => b.onclick = async () => { if (await confirmDlg('Remove this NAS share?', 'ZeroOS forgets the share. Nothing on the NAS is deleted.', 'Remove', true)) busy(b, async () => { await DEL(`/api/storage/shares/${b.dataset.rm}`); again(); }); });
  $$('[data-login]', pane).forEach((b) => b.onclick = () => { const s = d.shares.find((x) => x.id === +b.dataset.login);
    const dl = dialog(`<h2>Change login for ${esc(s.name)}</h2><form class="col"><label class="f">NAS username<input type="text" name="u" value="${esc(s.username)}" autocomplete="off"></label><label class="f">NAS password<input type="password" name="p" autocomplete="off"></label><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Save and connect</button></div></form>`);
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); busy($('button.pri', dl.el), async () => { await PUT(`/api/storage/shares/${s.id}/login`, { username: e.target.u.value, password: e.target.p.value }); dl.close(); toast('Connected.'); again(); }); }; });
}

async function setSharing(w, pane) {
  const d = await GET('/api/sharing'), p = d.playit;
  pane.innerHTML = `<div class="between"><div class="col" style="gap:.2em"><h1>Sharing online</h1><span class="muted">Reach your apps and game servers from outside your home, through playit.gg.</span></div>${p.linked && d.canPorts ? '<button class="btn pri" data-port>Share a port</button>' : ''}</div>
    ${p.linked ? `<div class="box"><div class="row"><span class="badge ok">playit linked</span>${p.premium ? '<span class="badge prem">Premium</span>' : ''}<span class="badge ${p.agentRunning ? 'ok' : 'warn'}">${p.agentRunning ? 'Agent running' : 'Agent not running'}</span><span class="grow"></span><a href="https://playit.gg/account" target="_blank" rel="noopener">Open playit.gg</a>${d.canAccount ? '<button class="btn sm danger" data-unlink>Unlink</button>' : ''}</div>${p.problem ? `<div class="note warn">${esc(p.problem)}</div>` : ''}</div>`
      : d.canAccount ? `<div class="box"><h3>Link playit</h3><span class="muted">playit.gg is a free service that gives your apps an address on the internet, without changing anything on your router.</span><div data-link></div></div>` : '<div class="note info">Sharing online isn\'t set up on this server yet. Ask the owner.</div>'}
    ${p.linked && d.canRemote ? `<div class="box"><div class="between"><div class="col" style="gap:.1em"><h3>Reach ZeroOS from outside</h3><span class="muted">Open this desktop when you are away from home.</span></div><input type="checkbox" class="switch" data-remote ${d.remote.on ? 'checked' : ''} aria-label="Reach ZeroOS from outside"></div>
      ${d.remote.on ? (d.remote.address ? `<div class="row" style="flex-wrap:nowrap"><code class="addr grow">${esc(d.remote.address)}</code><button class="btn sm" data-copy="${esc(d.remote.address)}">Copy</button></div><span class="muted">The first time, your browser warns that it doesn't know this site's certificate. That is expected: ZeroOS makes its own. Choose "Advanced" and continue. The connection is encrypted. Away from home, apps open in a new tab and need Share online turned on.</span>` : '<div class="row"><span class="spinner dark"></span><span class="muted">Getting its address from playit…</span></div>') : (d.twoStep ? '' : '<div class="note info">Turn on two-step sign-in for your account first. It protects ZeroOS once it can be reached from the internet. <button class="link" data-2fa>Go to Profile</button></div>')}</div>${d.remote.on || d.remote.secure ? secureBox({ secure: d.remote.secure, premium: p.premium, port: d.remote.webPort, what: 'ZeroOS itself', canSetUp: true }) : ''}${d.remote.secure && !d.remote.on ? '<div class="note info">This secure address is closed while "Reach ZeroOS from outside" is off.</div>' : ''}` : ''}
    ${p.linked ? `<h2>Apps</h2>${tunnelList(d.tunnels.filter((t) => t.kind === 'app'), 'No app is shared online. Turn it on in an app\'s settings (right-click the app › Share online).')}
    <h2>Shared ports</h2>${tunnelList(d.tunnels.filter((t) => t.kind === 'port'), 'For things without their own switch, like a game server made inside Pterodactyl.')}` : ''}`;
  const again = () => { settingsDraw(w); refresh(); };
  const lk = $('[data-link]', pane); if (lk) playitLinkBox(lk, again);
  const un = $('[data-unlink]', pane); if (un) un.onclick = async () => { if (await confirmDlg('Unlink playit?', 'Everything stops being reachable from outside your home. Your tunnels stay on your playit account.', 'Unlink', true)) busy(un, async () => { await DEL('/api/sharing'); again(); }); };
  const rs = $('[data-remote]', pane); if (rs) rs.onchange = async () => { rs.disabled = true; try { await PUT('/api/sharing/remote', { on: rs.checked }); } catch (e) { oops(e); } again(); };
  const tf = $('[data-2fa]', pane); if (tf) tf.onclick = () => w.go('profile');
  $$('[data-copy]', pane).forEach((b) => b.onclick = () => copyText(b.dataset.copy));
  secureWire(pane, again, async () => !!(await GET('/api/sharing')).remote.secure);
  $$('[data-stop]', pane).forEach((b) => b.onclick = () => busy(b, async () => { await DEL('/api/sharing/tunnels/' + b.dataset.stop); again(); }));
  const pb = $('[data-port]', pane); if (pb) pb.onclick = () => {
    const dl = dialog(`<h2>Share a port</h2><p class="muted">For things without their own switch, like a game server made in Pterodactyl. You'll find the port in that app.</p>
      <form class="col"><label class="f">Name<input type="text" name="n" placeholder="For example Minecraft · Survival" required></label><label class="f">Port on this mini PC<input type="number" name="p" min="1" max="65535" inputmode="numeric" required></label>
      <fieldset style="border:0;margin:0;padding:0" class="col"><legend class="muted" style="font-weight:600;margin-bottom:.4em">Type</legend><div class="row"><label class="check"><input type="radio" name="t" value="tcp">TCP</label><label class="check"><input type="radio" name="t" value="udp">UDP</label><label class="check"><input type="radio" name="t" value="both" checked>Both</label></div><span class="muted">Not sure? Keep Both.</span></fieldset>
      <div class="note bad hide" role="alert"></div><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Share</button></div></form>`);
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); const f = e.target, note = $('.note', dl.el); note.classList.add('hide');
      busy($('button.pri', dl.el), async () => { try { await POST('/api/sharing/ports', { name: f.n.value, port: +f.p.value, proto: f.t.value }); dl.close(); again(); } catch (err) { note.textContent = err.message; note.classList.remove('hide'); } }); }; };
  if (d.tunnels.some((t) => !t.address) || (d.remote.on && !d.remote.address)) setTimeout(() => { if (w.sec === 'sharing' && document.body.contains(pane) && !$('#layer').children.length) settingsDraw(w); }, 6000);
}
function tunnelList(list, emptyText) {
  if (!list.length) return `<p class="muted">${emptyText}</p>`;
  return `<div class="list">${list.map((t) => `<div class="lrow">${t.kind === 'app' ? icon(t.storeId, t.name) : ''}<strong style="flex:1 1 9em">${esc(t.name)}</strong>${t.kind === 'port' ? `<span class="muted">Port ${t.localPort} · ${esc(t.proto === 'both' ? 'TCP + UDP' : t.proto.toUpperCase())}</span>` : ''}
    ${t.secure ? `<code class="addr" style="flex:1 1 22em">${esc(t.secure)}</code><button class="btn sm" data-copy="${esc(t.secure)}">Copy</button>` : ''}
    ${t.id ? (t.address ? `<code class="addr" style="flex:1 1 22em">${esc(t.address)}</code><button class="btn sm" data-copy="${esc(t.address)}">Copy</button>` : '<span class="muted" style="flex:1 1 12em">Getting its address…</span>') : ''}${t.canRemove ? `<button class="btn sm" data-stop="${t.id}">Stop sharing</button>` : ''}</div>`).join('')}</div>`;
}

async function setSystem(w, pane) {
  const s = await GET('/api/system');
  const up = s.uptime > 86400 ? Math.floor(s.uptime / 86400) + ' days' : Math.floor(s.uptime / 3600) + ' hours';
  pane.innerHTML = `<h1>System</h1>
    <div class="cards"><div class="box"><div class="between"><span class="muted">CPU</span><strong>${Math.round(s.cpu)}%</strong></div><div class="meter"><span style="width:${Math.round(s.cpu)}%"></span></div><span class="muted">${esc(s.cpuModel)} · ${s.cores} threads</span></div>
    <div class="box"><div class="between"><span class="muted">Memory</span><strong>${fmtBytes(s.memUsed)} of ${fmtBytes(s.memTotal)}</strong></div><div class="meter"><span style="width:${pct(s.memUsed, s.memTotal)}%"></span></div></div>
    <div class="box"><div class="between"><span class="muted">SSD</span><strong>${fmtBytes(s.ssd.free)} free</strong></div><div class="meter"><span style="width:${pct(s.ssd.used, s.ssd.total)}%"></span></div></div></div>
    <div class="box"><h3>About</h3><span>ZeroOS ${esc(s.version)} on <strong>${esc(s.hostname)}</strong>, running for ${up}.</span><span>Web address at home: ${s.addresses.map((a) => `<a href="${esc(a)}">${esc(a)}</a>`).join(' or ')}</span><span>Graphics chip for apps: ${s.gpu ? 'available' : 'not found'}</span><span class="muted">To update ZeroOS, run <code>sudo zeroos update</code> on the mini PC. Apps keep running while it updates.</span></div>
    ${can('store.repos') ? `<div class="box"><div class="between"><div class="col" style="gap:.1em"><h3>Store catalog</h3><span class="muted">Refreshed by itself every day.</span></div><button class="btn sm" data-sync>Refresh now</button></div></div>` : ''}`;
  const sy = $('[data-sync]', pane); if (sy) sy.onclick = () => busy(sy, async () => { const r = await POST('/api/store/sync'); toast(`The store has ${r.total} apps.`); });
}
async function setActivity(w, pane) { pane.innerHTML = `<h1>Activity</h1><p class="muted">${can('system.view') ? 'Everything that was done on this server.' : 'What you have done on this server.'}</p>${await activityHTML()}`; }

async function setProfile(w, pane) {
  const [sessions] = await Promise.all([GET('/api/me/sessions')]);
  const u = S.user;
  pane.innerHTML = `<div class="row" style="flex-wrap:nowrap"><span class="av" style="font-size:1.3em">${esc(u.username[0].toUpperCase())}</span><div class="col" style="gap:.1em"><h1>${esc(u.username)}</h1><span class="muted">${esc(u.role || 'No role')}</span></div></div>
    <form class="box" data-pw><h3>Change password</h3><label class="f">Current password<input type="password" name="o" autocomplete="current-password" required></label><label class="f">New password<input type="password" name="n" autocomplete="new-password" minlength="8" required></label><div class="note bad hide" role="alert"></div><div><button class="btn">Change password</button></div></form>
    <div class="box"><div class="between"><div class="col" style="gap:.1em"><h3>Two-step sign-in</h3><span class="muted">Asks for a code from an authenticator app on your phone when you sign in.</span></div>${u.totp ? '<button class="btn sm danger" data-toff>Turn off</button>' : '<button class="btn sm" data-ton>Turn on</button>'}</div>${u.totp ? '<span class="badge ok" style="align-self:flex-start">On</span>' : ''}</div>
    <div class="box"><h3>Where you're signed in</h3>${sessions.map((s) => `<div class="between"><span>${esc(deviceName(s.agent))} <span class="muted">· since ${fmtTime(s.created)}</span></span>${s.current ? '<span class="badge info">This device</span>' : `<button class="btn sm" data-end="${esc(s.id)}">Sign out</button>`}</div>`).join('')}</div>`;
  const f = $('[data-pw]', pane);
  f.onsubmit = (e) => { e.preventDefault(); const note = $('.note', f); note.classList.add('hide'); busy($('button', f), async () => { try { await PUT('/api/me/password', { old: f.o.value, new: f.n.value }); f.reset(); toast('Password changed. Other devices were signed out.'); settingsDraw(w); } catch (err) { note.textContent = err.message; note.classList.remove('hide'); } }); };
  $$('[data-end]', pane).forEach((b) => b.onclick = () => busy(b, async () => { await DEL('/api/me/sessions/' + b.dataset.end); settingsDraw(w); }));
  const reload = async () => { S.user = await GET('/api/me'); settingsDraw(w); };
  const off = $('[data-toff]', pane); if (off) off.onclick = async () => { if (await confirmDlg('Turn off two-step sign-in?', 'You will only need your password to sign in.', 'Turn off', true)) { await DEL('/api/me/totp').catch(oops); reload(); } };
  const on = $('[data-ton]', pane); if (on) on.onclick = () => busy(on, async () => { const t = await POST('/api/me/totp');
    const dl = dialog(`<h2>Turn on two-step sign-in</h2><p>1. In your authenticator app (Google Authenticator, Aegis, 1Password…), add an account and choose "enter a setup key". Type this key:</p><code class="addr" style="white-space:normal;word-break:break-all;font-size:1em">${esc(t.secret.replace(/(.{4})/g, '$1 '))}</code><p class="muted">On this phone? <a href="${esc(t.link)}">Open it in your authenticator app</a>.</p>
      <form class="col"><label class="f">2. Type the 6-digit code the app shows<input type="text" name="c" inputmode="numeric" autocomplete="one-time-code" maxlength="7" required></label><div class="note bad hide" role="alert"></div><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Turn on</button></div></form>`);
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); const note = $('.note', dl.el); note.classList.add('hide'); busy($('button.pri', dl.el), async () => { try { await PUT('/api/me/totp', { secret: t.secret, code: e.target.c.value }); dl.close(); toast('Two-step sign-in is on.'); reload(); } catch (err) { note.textContent = err.message; note.classList.remove('hide'); } }); }; });
}
function deviceName(ua) { const os = /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iPhone or iPad' : /Windows/.test(ua) ? 'Windows' : /Mac OS/.test(ua) ? 'Mac' : /Linux/.test(ua) ? 'Linux' : 'Unknown device'; const br = /Edg\//.test(ua) ? 'Edge' : /Chrome\//.test(ua) ? 'Chrome' : /Firefox\//.test(ua) ? 'Firefox' : /Safari\//.test(ua) ? 'Safari' : 'browser'; return `${br} on ${os}`; }

// Users and roles
async function setUsers(w, pane) {
  const [users, roles, perms] = await Promise.all([GET('/api/users'), GET('/api/roles'), GET('/api/perms')]);
  if (w.view === 'roles') return rolesView(w, pane, roles, perms);
  const cur = users.find((u) => u.id === w.uid) || users[0]; w.uid = cur.id;
  pane.style.padding = '0'; pane.style.flexDirection = 'row'; pane.style.gap = '0'; pane.classList.add('split');
  const restore = () => { pane.style.padding = pane.style.flexDirection = pane.style.gap = ''; pane.classList.remove('split'); };
  const oldGo = w.go; w.go = (s) => { restore(); w.go = oldGo; w.view = null; oldGo(s); };
  pane.innerHTML = `<div style="flex:1 1 13em;max-width:100%;border-right:1px solid var(--line);padding:.8em .6em;overflow-y:auto" class="col" data-ul></div><div class="col" style="flex:999 1 24em;min-width:0;padding:var(--pad);overflow-y:auto;gap:1.1em" data-ud></div>`;
  const ul = $('[data-ul]', pane), ud = $('[data-ud]', pane);
  ul.innerHTML = `<div class="between" style="padding:0 .4em"><h3>Users</h3><button class="btn sm" data-add>Add</button></div>${users.map((u) => `<button class="urow ${u.id === cur.id ? 'on' : ''}" data-u="${u.id}"><span class="av" style="background:${colorOf(u.username)};color:#fff">${esc(u.username[0].toUpperCase())}</span><span class="col" style="gap:0"><strong>${esc(u.username)}</strong><span class="muted">${esc(u.role || 'No role')}</span></span></button>`).join('')}${can('roles.manage') ? '<button class="link" data-roles style="padding:.6em .4em;text-align:left">Edit roles</button>' : ''}`;
  const again = () => { restore(); w.go = oldGo; settingsDraw(w); };
  $$('[data-u]', ul).forEach((b) => b.onclick = () => { w.uid = +b.dataset.u; again(); });
  const rb = $('[data-roles]', ul); if (rb) rb.onclick = () => { w.view = 'roles'; again(); };
  $('[data-add]', ul).onclick = () => {
    const dl = dialog(`<h2>Add a user</h2><form class="col"><label class="f">Username<input type="text" name="u" autocomplete="off" required></label><label class="f">Password<input type="password" name="p" autocomplete="new-password" minlength="8" required><span class="muted" style="font-weight:400">Tell them this password. They can change it under Settings › Profile.</span></label>
      <label class="f">Role<select name="r">${roles.map((r) => `<option value="${r.id}" ${r.name === 'Member' ? 'selected' : ''}>${esc(r.name)}</option>`).join('')}</select></label><div class="note bad hide" role="alert"></div><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Add user</button></div></form>`);
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); const f = e.target, note = $('.note', dl.el); note.classList.add('hide');
      busy($('button.pri', dl.el), async () => { try { const r = await POST('/api/users', { username: f.u.value.trim(), password: f.p.value, roleId: +f.r.value }); w.uid = r.id; dl.close(); again(); } catch (err) { note.textContent = err.message; note.classList.remove('hide'); } }); }; };
  // detail
  const ua = await GET(`/api/users/${cur.id}/apps`);
  const role = roles.find((r) => r.id === cur.roleId);
  const cols = perms.app.filter((p) => ['app.open', 'app.power', 'app.settings', 'app.share'].includes(p.name));
  const short = { 'app.open': 'Open', 'app.power': 'Start/stop', 'app.settings': 'Settings', 'app.share': 'Share online' };
  const mine = ua.apps.filter((a) => a.perms.length), others = ua.apps.filter((a) => !a.perms.length);
  ud.innerHTML = `<div class="between"><div class="row" style="flex-wrap:nowrap"><span class="av" style="font-size:1.3em;background:${colorOf(cur.username)};color:#fff">${esc(cur.username[0].toUpperCase())}</span><div class="col" style="gap:.1em"><h1>${esc(cur.username)}</h1><span class="muted">${esc(cur.role || 'No role')} · last seen ${fmtTime(cur.lastSeen)}</span></div></div>
      <div class="row"><button class="btn sm" data-pw>Reset password</button>${cur.isOwner || cur.id === S.user.id ? '' : '<button class="btn sm danger" data-del>Delete</button>'}</div></div>
    ${cur.isOwner ? '<div class="note info">The owner can always do everything, and sees every app.</div>' : `
    <label class="f" style="max-width:16em">Role<select data-role>${roles.map((r) => `<option value="${r.id}" ${r.id === cur.roleId ? 'selected' : ''}>${esc(r.name)}</option>`).join('')}</select></label>
    <div class="col"><div class="between"><h2>Apps on ${esc(cur.username)}'s desktop</h2>${!ua.locked && others.length ? '<button class="btn sm" data-addapp>Add an app</button>' : ''}</div>
      ${ua.locked ? '<p class="muted">This role sees and manages every app.</p>' : mine.length ? `<div class="tablebox"><table><thead><tr><th scope="col">App</th>${cols.map((p) => `<th scope="col">${short[p.name]}</th>`).join('')}<th scope="col"><span class="muted">Remove</span></th></tr></thead><tbody>
      ${mine.map((a) => `<tr><td><div class="row" style="flex-wrap:nowrap;gap:.5em">${icon(a.storeId, a.name, '1.8em')}<strong>${esc(a.name)}</strong></div></td>${cols.map((p) => `<td><input type="checkbox" data-a="${esc(a.id)}" data-p="${p.name}" ${a.perms.includes(p.name) ? 'checked' : ''} aria-label="${esc(a.name)}: ${short[p.name]}"></td>`).join('')}<td><button class="link" data-take="${esc(a.id)}" aria-label="Take ${esc(a.name)} off their desktop">✕</button></td></tr>`).join('')}</tbody></table></div>` : `<p class="muted">${esc(cur.username)} has no apps yet. ${others.length ? 'Add one to put it on their desktop.' : 'Install an app first.'}</p>`}</div>
    <div class="row" style="align-items:flex-start;gap:1.5em"><div class="col" style="flex:1 1 15em;gap:.1em"><h2 style="margin-bottom:.3em">Whole server</h2>${perms.global.map((p) => `<label class="check"><input type="checkbox" data-g="${p.name}" ${cur.perms.includes(p.name) ? 'checked' : ''} ${S.user.perms.includes(p.name) ? '' : 'disabled'}>${esc(p.label)}</label>`).join('')}</div>
      <div class="col" style="flex:1 1 13em"><h2>Limits</h2><label class="f">Apps they can install<input type="number" min="0" data-qa value="${cur.quotaApps}"></label><label class="f">Online shares they can make<input type="number" min="0" data-qt value="${cur.quotaTunnels}"></label><span class="muted">0 means no limit for installing, and no sharing of ports unless ticked on the left.</span></div></div>
    <div class="row" style="justify-content:flex-end;border-top:1px solid #F0F1F3;padding-top:1em"><button class="btn pri" data-save>Save</button></div>`}`;
  $('[data-pw]', ud).onclick = () => { const dl = dialog(`<h2>Reset password for ${esc(cur.username)}</h2><form class="col"><label class="f">New password<input type="password" name="p" autocomplete="new-password" minlength="8" required></label><p class="muted">They are signed out everywhere and use this password next time.</p><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Set password</button></div></form>`);
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); busy($('button.pri', dl.el), async () => { await PUT(`/api/users/${cur.id}/password`, { password: e.target.p.value }); dl.close(); toast('Password set.'); }); }; };
  const del = $('[data-del]', ud); if (del) del.onclick = async () => { if (await confirmDlg(`Delete ${cur.username}?`, 'They can no longer sign in. Apps they installed stay.', 'Delete', true)) busy(del, async () => { await DEL('/api/users/' + cur.id); w.uid = null; again(); }); };
  if (cur.isOwner) return;
  const saveApps = async () => { const map = {}; mine.forEach((a) => map[a.id] = ['app.view', ...$$(`input[data-a="${CSS.escape(a.id)}"]:checked`, ud).map((x) => x.dataset.p)]); await PUT(`/api/users/${cur.id}/apps`, map); };
  $$('input[data-a]', ud).forEach((c) => c.onchange = () => saveApps().catch((e) => { c.checked = !c.checked; oops(e); }));
  $$('[data-take]', ud).forEach((b) => b.onclick = async () => { try { await PUT(`/api/users/${cur.id}/apps`, { [b.dataset.take]: [] }); again(); } catch (e) { oops(e); } });
  const aa = $('[data-addapp]', ud); if (aa) aa.onclick = (e) => { const r = e.currentTarget.getBoundingClientRect(); menu(r.left, r.bottom + 4, others.map((a) => ({ label: a.name, run: async () => { try { await PUT(`/api/users/${cur.id}/apps`, { [a.id]: ['app.view', 'app.open'] }); again(); } catch (err) { oops(err); } } }))); };
  $('[data-save]', ud).onclick = (e) => busy(e.currentTarget, async () => {
    const roleId = +$('[data-role]', ud).value, r = roles.find((x) => x.id === roleId), rp = r ? r.perms : [];
    const checked = $$('input[data-g]:checked', ud).map((x) => x.dataset.g);
    // when the role changes, start from the new role's own permissions
    const body = roleId !== cur.roleId ? { extraPerms: [], removedPerms: [] } : { extraPerms: checked.filter((p) => !rp.includes(p)), removedPerms: rp.filter((p) => !checked.includes(p)) };
    await PUT('/api/users/' + cur.id, { username: cur.username, roleId, ...body, quotaApps: +$('[data-qa]', ud).value || 0, quotaTunnels: +$('[data-qt]', ud).value || 0 });
    toast('Saved.'); again();
  });
}
function rolesView(w, pane, roles, perms) {
  const back = () => { w.view = null; settingsDraw(w); };
  pane.innerHTML = `<div><button class="link" data-back>← Users</button></div><div class="between"><div class="col" style="gap:.2em"><h1>Roles</h1><span class="muted">A role is a saved set of permissions. Changing a role changes everyone who has it.</span></div><button class="btn pri" data-new>New role</button></div>
    <div class="list">${roles.map((r) => `<div class="lrow"><span class="grow"><strong>${esc(r.name)}</strong><br><span class="muted">${r.users} ${r.users === 1 ? 'user' : 'users'} · ${r.perms.length ? r.perms.length + ' whole-server permissions' : 'only the apps shared with them'}</span></span><button class="btn sm" data-ed="${r.id}">Edit</button>${r.users ? '' : `<button class="btn sm danger" data-rm="${r.id}">Delete</button>`}</div>`).join('')}</div>`;
  $('[data-back]', pane).onclick = back;
  const edit = (r) => { const dl = dialog(`<h2>${r.id ? 'Edit role' : 'New role'}</h2><form class="col"><label class="f">Name<input type="text" name="n" value="${esc(r.name)}" required></label><h3>Whole server</h3><div class="col" style="gap:0">${perms.global.map((p) => `<label class="check"><input type="checkbox" data-g="${p.name}" ${r.perms.includes(p.name) ? 'checked' : ''} ${S.user.perms.includes(p.name) ? '' : 'disabled'}>${esc(p.label)}</label>`).join('')}</div><div class="note bad hide" role="alert"></div><div class="foot"><button class="btn" type="button" data-close>Cancel</button><button class="btn pri">Save</button></div></form>`, { wide: true });
    $('form', dl.el).onsubmit = (e) => { e.preventDefault(); const note = $('.note', dl.el); note.classList.add('hide'); busy($('button.pri', dl.el), async () => { try { const body = { name: e.target.n.value, perms: $$('input[data-g]:checked', dl.el).map((x) => x.dataset.g), appPerms: r.appPerms }; r.id ? await PUT('/api/roles/' + r.id, body) : await POST('/api/roles', body); dl.close(); settingsDraw(w); } catch (err) { note.textContent = err.message; note.classList.remove('hide'); } }); }; };
  $('[data-new]', pane).onclick = () => edit({ id: 0, name: '', perms: [], appPerms: ['app.view', 'app.open'] });
  $$('[data-ed]', pane).forEach((b) => b.onclick = () => edit(roles.find((r) => r.id === +b.dataset.ed)));
  $$('[data-rm]', pane).forEach((b) => b.onclick = async () => { if (await confirmDlg('Delete this role?', 'Nobody has this role, so nothing else changes.', 'Delete', true)) busy(b, async () => { await DEL('/api/roles/' + b.dataset.rm); settingsDraw(w); }); });
}
