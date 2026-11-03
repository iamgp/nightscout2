/* Tideline web app. Plain JavaScript, no build step. */
'use strict';

(() => {
  // ---------------------------------------------------------------------------
  // Small helpers

  const $ = (sel, el = document) => el.querySelector(sel);
  const $$ = (sel, el = document) => Array.from(el.querySelectorAll(sel));
  const SVGNS = 'http://www.w3.org/2000/svg';
  const MIN = 60 * 1000;

  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const store = {
    get(k, d = null) { try { const v = localStorage.getItem('tl.' + k); return v === null ? d : v; } catch { return d; } },
    set(k, v) { try { localStorage.setItem('tl.' + k, v); } catch { /* private mode */ } },
    del(k) { try { localStorage.removeItem('tl.' + k); } catch { /* ignore */ } },
  };

  function svg(tag, attrs = {}, parent) {
    const el = document.createElementNS(SVGNS, tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v !== undefined && v !== null) el.setAttribute(k, v);
    }
    if (parent) parent.appendChild(el);
    return el;
  }

  const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const dayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short' });
  const dateTimeFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
  const fmtTime = (ms) => timeFmt.format(new Date(ms));
  const fmtDateTime = (ms) => (ms ? dateTimeFmt.format(new Date(ms)) : '–');

  function ago(ms) {
    const m = Math.floor((Date.now() - ms) / MIN);
    if (m < 1) return 'just now';
    if (m < 60) return `${m} min ago`;
    const h = Math.floor(m / 60);
    if (h < 48) return `${h} h ${m % 60} min ago`;
    return `${Math.floor(h / 24)} days ago`;
  }

  // ---------------------------------------------------------------------------
  // State

  const state = {
    token: store.get('token'),
    me: null,
    settings: null,
    readings: [],
    treatments: [],
    deviceStatus: null,
    alerts: [],
    hours: Number(store.get('hours', '6')) || 6,
    reportDays: Number(store.get('reportDays', '14')) || 14,
    report: null,
    es: null,
    editNotifiers: [],
    sound: false,
    seenAlerts: new Set(),
  };

  // ---------------------------------------------------------------------------
  // API

  class ApiError extends Error {
    constructor(status, message) { super(message); this.status = status; }
  }

  async function api(path, opts = {}) {
    const headers = { Accept: 'application/json' };
    if (state.token) headers.Authorization = 'Bearer ' + state.token;
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(path, {
      method: opts.method || 'GET',
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      cache: 'no-store',
    });
    let data = null;
    const text = await res.text();
    if (text) { try { data = JSON.parse(text); } catch { data = text; } }
    if (!res.ok) throw new ApiError(res.status, (data && data.message) || res.statusText);
    return data;
  }

  // ---------------------------------------------------------------------------
  // Units and glucose classification (all values are mg/dL internally)

  const mmol = () => state.settings && state.settings.units === 'mmol';
  const unitLabel = () => (mmol() ? 'mmol/L' : 'mg/dL');
  const fmtBG = (mg) => (mmol() ? (mg / 18).toFixed(1) : String(Math.round(mg)));
  const toDisplay = (mg) => (mmol() ? Math.round((mg / 18) * 10) / 10 : Math.round(mg));
  const fromDisplay = (v) => (mmol() ? Math.round(v * 18) : Math.round(v));
  function fmtDelta(mg) {
    const v = mmol() ? (mg / 18).toFixed(1) : String(Math.round(mg));
    return (mg >= 0 ? '+' : '−') + v.replace('-', '');
  }

  const STATUS = {
    urgentLow: { label: 'Urgent low', color: 'var(--critical)', glyph: '▼▼' },
    low: { label: 'Low', color: 'var(--critical)', glyph: '▼' },
    inRange: { label: 'In range', color: 'var(--good)', glyph: '' },
    high: { label: 'High', color: 'var(--warning)', glyph: '▲' },
    urgentHigh: { label: 'Urgent high', color: 'var(--serious)', glyph: '▲▲' },
  };

  function classify(mg) {
    const t = state.settings.thresholds;
    if (mg <= t.urgentLow) return STATUS.urgentLow;
    if (mg < t.low) return STATUS.low;
    if (mg >= t.urgentHigh) return STATUS.urgentHigh;
    if (mg > t.high) return STATUS.high;
    return STATUS.inRange;
  }

  const ARROWS = {
    DoubleUp: ['⇈', 'rising quickly'], SingleUp: ['↑', 'rising'], FortyFiveUp: ['↗', 'rising slowly'],
    Flat: ['→', 'steady'], FortyFiveDown: ['↘', 'falling slowly'], SingleDown: ['↓', 'falling'],
    DoubleDown: ['⇊', 'falling quickly'],
  };

  // ---------------------------------------------------------------------------
  // Boot, sign-in and routing

  function captureTokenFromURL() {
    const url = new URL(location.href);
    const t = url.searchParams.get('token');
    if (t) {
      state.token = t;
      store.set('token', t);
      url.searchParams.delete('token');
      history.replaceState(null, '', url.pathname + url.search + url.hash);
    }
  }

  async function boot() {
    captureTokenFromURL();
    wireStatic();
    if (!state.token) return showLogin();
    try {
      await loadMe();
    } catch (e) {
      if (e.status === 401) {
        signOut(false);
        return showLogin('That token or secret was not accepted.');
      }
      return showLogin('Could not reach Tideline: ' + e.message);
    }
    $('#tabs').hidden = false;
    $('#conn').hidden = false;
    route();
    await refreshTimeline();
    connectStream();
    setInterval(tick, 15 * 1000);
  }

  async function loadMe() {
    const me = await api('/api/tl/v1/me');
    state.me = me;
    state.settings = me.settings;
    applySettings();
  }

  function applySettings() {
    const name = state.settings.name || 'Tideline';
    $('#site-name').textContent = name;
    document.title = name;
    $('#bg-units').textContent = unitLabel();
  }

  function showLogin(err) {
    $$('.view').forEach((v) => (v.hidden = true));
    $('#tabs').hidden = true;
    $('#view-login').hidden = false;
    $('#login-error').textContent = err || '';
    $('#login-token').focus();
  }

  function signOut(reload = true) {
    store.del('token');
    state.token = null;
    if (state.es) state.es.close();
    if (reload) location.replace(location.pathname);
  }

  function route() {
    if (!state.me) return;
    const view = (location.hash || '#now').slice(1);
    const valid = ['now', 'reports', 'settings'].includes(view) ? view : 'now';
    $$('.view').forEach((v) => (v.hidden = v.id !== 'view-' + valid));
    $$('#tabs a').forEach((a) => a.dataset.view === valid ? a.setAttribute('aria-current', 'page') : a.removeAttribute('aria-current'));
    if (valid === 'now') renderNow();
    if (valid === 'reports') loadReport();
    if (valid === 'settings') renderSettings();
  }

  // ---------------------------------------------------------------------------
  // Data loading and live updates

  async function refreshTimeline() {
    const hours = Math.max(state.hours, 24);
    const tl = await api(`/api/tl/v1/timeline?hours=${hours}`);
    state.readings = tl.readings;
    state.treatments = tl.treatments;
    state.deviceStatus = tl.deviceStatus;
    setAlerts(tl.alerts);
    renderNow();
  }

  function connectStream() {
    if (state.es) state.es.close();
    const es = new EventSource('/api/tl/v1/stream?token=' + encodeURIComponent(state.token));
    state.es = es;
    let wasDown = false;
    const conn = $('#conn');
    es.onopen = () => {
      conn.className = 'conn live';
      conn.title = 'Live';
      if (wasDown) refreshTimeline().catch(() => {});
      wasDown = false;
    };
    es.onerror = () => {
      conn.className = 'conn down';
      conn.title = 'Reconnecting…';
      wasDown = true;
    };
    es.addEventListener('entries', (e) => {
      const docs = JSON.parse(e.data);
      if (docs.some((d) => d._deleted)) { refreshTimeline().catch(() => {}); return; }
      for (const d of docs) {
        if ((d.type || 'sgv') !== 'sgv' || !(Number(d.sgv) > 0)) continue;
        const t = Number(d.date);
        const r = { t, v: Number(d.sgv), d: d.direction || '' };
        const i = state.readings.findIndex((x) => x.t === t);
        if (i >= 0) state.readings[i] = r; else state.readings.push(r);
      }
      state.readings.sort((a, b) => a.t - b.t);
      trimWindow();
      renderNow();
    });
    es.addEventListener('treatments', (e) => {
      for (const d of JSON.parse(e.data)) {
        const i = state.treatments.findIndex((x) => x._id === d._id);
        if (d._deleted) { if (i >= 0) state.treatments.splice(i, 1); continue; }
        if (i >= 0) state.treatments[i] = d; else state.treatments.push(d);
      }
      state.treatments.sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
      renderNow();
    });
    es.addEventListener('devicestatus', (e) => {
      const docs = JSON.parse(e.data).filter((d) => !d._deleted);
      for (const d of docs) {
        if (!state.deviceStatus || Date.parse(d.created_at) >= Date.parse(state.deviceStatus.created_at)) state.deviceStatus = d;
      }
      renderChips();
    });
    es.addEventListener('alerts', (e) => setAlerts(JSON.parse(e.data)));
    es.addEventListener('settings', (e) => {
      const pub = JSON.parse(e.data);
      state.settings = { ...state.settings, ...pub, notifiers: state.settings.notifiers };
      applySettings();
      route();
    });
  }

  function trimWindow() {
    const cutoff = Date.now() - 72 * 60 * MIN;
    state.readings = state.readings.filter((r) => r.t >= cutoff);
  }

  function tick() {
    renderHero();
    renderChips();
  }

  // ---------------------------------------------------------------------------
  // Now view

  function renderNow() {
    if (!state.settings || $('#view-now').hidden) return;
    renderHero();
    renderChips();
    renderAlerts();
    renderGlucoseChart();
    renderTreatments();
    $('#entry-card').hidden = !state.me.canWrite;
    $$('#range button').forEach((b) => b.setAttribute('aria-pressed', String(Number(b.dataset.hours) === state.hours)));
  }

  function latestReading() {
    return state.readings.length ? state.readings[state.readings.length - 1] : null;
  }

  function renderHero() {
    if (!state.settings) return;
    const r = latestReading();
    const hero = $('#hero');
    const pill = $('#bg-status');
    if (!r) {
      $('#bg-value').textContent = '–';
      $('#bg-trend').textContent = '';
      $('#bg-delta').textContent = '';
      $('#bg-age').textContent = 'Waiting for CGM data';
      pill.querySelector('b').textContent = 'No data';
      pill.querySelector('i').style.background = 'var(--text-3)';
      return;
    }
    const stale = Date.now() - r.t > state.settings.alerts.staleMinutes * MIN;
    hero.classList.toggle('stale', stale);
    $('#bg-value').textContent = fmtBG(r.v);
    const arrow = ARROWS[r.d];
    $('#bg-trend').textContent = arrow ? arrow[0] : '';
    $('#bg-trend').setAttribute('aria-label', arrow ? arrow[1] : '');
    const prev = state.readings.length > 1 ? state.readings[state.readings.length - 2] : null;
    $('#bg-delta').textContent = prev && r.t - prev.t <= 15 * MIN ? fmtDelta(r.v - prev.v) : '';
    $('#bg-age').textContent = ago(r.t);
    const st = stale ? { label: 'Stale', color: 'var(--text-3)', glyph: '◌' } : classify(r.v);
    pill.querySelector('i').style.background = st.color;
    pill.querySelector('b').textContent = st.glyph ? `${st.glyph} ${st.label}` : st.label;
    const title = `${fmtBG(r.v)} ${arrow ? arrow[0] : ''} · ${state.settings.name || 'Tideline'}`;
    if (document.title !== title) document.title = title;
  }

  function pick(obj, ...paths) {
    for (const p of paths) {
      let v = obj;
      for (const k of p.split('.')) v = v == null ? undefined : (Array.isArray(v) && /^\d+$/.test(k) ? v[Number(k)] : v[k]);
      if (v !== undefined && v !== null && v !== '') return v;
    }
    return undefined;
  }

  function renderChips() {
    const el = $('#chips');
    const ds = state.deviceStatus;
    if (!ds) { el.innerHTML = ''; return; }
    const chips = [];
    const num = (v, d = 1) => (typeof v === 'number' ? v.toFixed(d) : Number(v).toFixed(d));
    const iob = pick(ds, 'loop.iob.iob', 'openaps.iob.iob', 'openaps.iob.0.iob', 'openaps.suggested.IOB');
    const cob = pick(ds, 'loop.cob.cob', 'openaps.suggested.COB', 'openaps.enacted.COB');
    const res = pick(ds, 'pump.reservoir');
    const pumpBat = pick(ds, 'pump.battery.percent');
    const upBat = pick(ds, 'uploader.battery', 'uploaderBattery');
    if (iob !== undefined && !isNaN(iob)) chips.push(['IOB', num(iob, 2) + ' U']);
    if (cob !== undefined && !isNaN(cob)) chips.push(['COB', num(cob, 0) + ' g']);
    if (res !== undefined && !isNaN(res)) chips.push(['Reservoir', num(res, 0) + ' U']);
    if (pumpBat !== undefined) chips.push(['Pump', pumpBat + '%']);
    if (upBat !== undefined) chips.push(['Phone', upBat + '%']);
    const created = Date.parse(ds.created_at);
    if (chips.length && created) chips.push(['Updated', ago(created)]);
    el.innerHTML = chips.map(([k, v]) => `<span class="chip"><span>${esc(k)}</span><b>${esc(v)}</b></span>`).join('');
  }

  function describeTreatment(t) {
    const parts = [];
    if (Number(t.insulin) > 0) parts.push(`${Number(t.insulin)} U insulin`);
    if (Number(t.carbs) > 0) parts.push(`${Number(t.carbs)} g carbs`);
    if (/temp basal/i.test(t.eventType || '') && t.rate !== undefined) {
      parts.push(`Temp basal ${Number(t.rate ?? t.absolute).toFixed(2)} U/h for ${Math.round(Number(t.duration) || 0)} min`);
    }
    const label = parts.length ? parts.join(' · ') : (t.eventType && t.eventType !== '<none>' ? t.eventType : 'Note');
    return t.notes ? `${label} — ${t.notes}` : label;
  }

  function renderTreatments() {
    const list = $('#treatments');
    const recent = state.treatments.slice(-15).reverse();
    if (!recent.length) {
      list.innerHTML = '<li class="muted">Nothing logged in the last day.</li>';
      return;
    }
    list.innerHTML = recent.map((t) => `
      <li>
        <span class="when">${esc(fmtTime(Date.parse(t.created_at)))}</span>
        <span class="what">${esc(describeTreatment(t))} <span class="who">${t.enteredBy ? '· ' + esc(t.enteredBy) : ''}</span></span>
        ${state.me.canWrite ? `<button class="small danger" data-del="${esc(t._id)}" aria-label="Delete">Delete</button>` : ''}
      </li>`).join('');
  }

  // ---------------------------------------------------------------------------
  // Alerts, sound and browser notifications

  const ALERT_ICON = { urgent: '⛔', warn: '⚠️' };

  function setAlerts(list) {
    const prev = state.seenAlerts;
    state.alerts = list || [];
    const now = new Set(state.alerts.map((a) => a.kind));
    for (const a of state.alerts) {
      if (!prev.has(a.kind) && !a.snoozedUntil) browserNotify(a);
    }
    state.seenAlerts = now;
    renderAlerts();
  }

  function alertText(a) {
    if (a.value) return a.message ? `${fmtBG(a.value)} ${unitLabel()} · ${a.message}` : `${fmtBG(a.value)} ${unitLabel()}`;
    return a.message;
  }

  function renderAlerts() {
    const el = $('#alerts');
    el.innerHTML = state.alerts.map((a) => {
      const snoozed = a.snoozedUntil && Date.parse(a.snoozedUntil) > Date.now();
      const actions = snoozed
        ? `<span class="muted small">Snoozed until ${esc(fmtTime(Date.parse(a.snoozedUntil)))}</span>
           <button class="small" data-snooze="${esc(a.kind)}" data-min="0">Unsnooze</button>`
        : [15, 30, 60].map((m) => `<button class="small" data-snooze="${esc(a.kind)}" data-min="${m}">Snooze ${m}m</button>`).join('');
      return `<div class="alert ${a.level === 'urgent' ? 'urgent' : ''} ${snoozed ? 'snoozed' : ''}" role="alert">
        <span class="icon" aria-hidden="true">${ALERT_ICON[a.level] || '⚠️'}</span>
        <div class="txt"><b>${esc(a.title)}</b><span class="muted small">${esc(alertText(a))} · since ${esc(fmtTime(Date.parse(a.since)))}</span></div>
        <div class="actions">${actions}</div>
      </div>`;
    }).join('');
  }

  let audio = null;
  let lastBeep = 0;

  function beep(urgent) {
    if (!audio) return;
    const now = audio.currentTime;
    const notes = urgent ? [880, 660, 880, 660] : [660, 520];
    notes.forEach((f, i) => {
      const o = audio.createOscillator();
      const g = audio.createGain();
      o.type = 'sine';
      o.frequency.value = f;
      g.gain.setValueAtTime(0.0001, now + i * 0.28);
      g.gain.exponentialRampToValueAtTime(0.4, now + i * 0.28 + 0.02);
      g.gain.exponentialRampToValueAtTime(0.0001, now + i * 0.28 + 0.24);
      o.connect(g).connect(audio.destination);
      o.start(now + i * 0.28);
      o.stop(now + i * 0.28 + 0.26);
    });
  }

  function alarmLoop() {
    if (!state.sound) return;
    const live = state.alerts.filter((a) => !(a.snoozedUntil && Date.parse(a.snoozedUntil) > Date.now()));
    if (!live.length) return;
    const urgent = live.some((a) => a.level === 'urgent');
    const every = urgent ? 10000 : 30000;
    if (Date.now() - lastBeep >= every) {
      lastBeep = Date.now();
      beep(urgent);
    }
  }

  function enableSound() {
    try {
      audio = audio || new (window.AudioContext || window.webkitAudioContext)();
      audio.resume();
      state.sound = true;
      store.set('sound', '1');
      beep(false);
    } catch { state.sound = false; }
    updateDeviceButtons();
  }

  function browserNotify(a) {
    if (!('Notification' in window) || Notification.permission !== 'granted' || !document.hidden) return;
    try {
      new Notification(a.title, { body: alertText(a), tag: 'tideline-' + a.kind, renotify: true, requireInteraction: a.level === 'urgent' });
    } catch { /* some platforms only allow notifications from a service worker */ }
  }

  function updateDeviceButtons() {
    $('#sound-toggle').textContent = state.sound ? 'Alarm sound on (tap to turn off)' : 'Enable alarm sound';
    const n = $('#notify-toggle');
    if (!('Notification' in window)) { n.hidden = true; return; }
    n.textContent = Notification.permission === 'granted' ? 'Browser notifications on' : 'Enable browser notifications';
    n.disabled = Notification.permission === 'granted' || Notification.permission === 'denied';
  }

  // ---------------------------------------------------------------------------
  // Tooltip

  const tip = () => $('#tooltip');
  function showTip(html, x, y) {
    const el = tip();
    el.innerHTML = html;
    el.hidden = false;
    const w = el.offsetWidth, h = el.offsetHeight;
    let left = x + 14, top = y - h - 10;
    if (left + w > window.innerWidth - 8) left = x - w - 14;
    if (top < 8) top = y + 16;
    el.style.left = Math.max(8, left) + 'px';
    el.style.top = top + 'px';
  }
  const hideTip = () => { tip().hidden = true; };

  // ---------------------------------------------------------------------------
  // Glucose chart

  // Pick the smallest tick step that keeps time labels ~72px apart.
  function niceTimeStep(hours, plotWidth) {
    const steps = [30, 60, 120, 180, 240, 360].map((m) => m * MIN);
    const span = hours * 60 * MIN;
    return steps.find((s) => (plotWidth * s) / span >= 72) || steps[steps.length - 1];
  }

  function renderGlucoseChart() {
    const box = $('#chart');
    const W = box.clientWidth;
    if (!W) return;
    const H = W < 600 ? 250 : 320;
    const m = { l: 6, r: 44, t: 10, b: 24 };
    const lane = 22; // treatment lane under the plot
    const now = Date.now();
    const x0 = now - state.hours * 60 * MIN, x1 = now + 5 * MIN;
    const rs = state.readings.filter((r) => r.t >= x0);
    const tr = state.treatments.filter((t) => Date.parse(t.created_at) >= x0);
    const th = state.settings.thresholds;
    const yMax = Math.max(300, ...rs.map((r) => r.v + 20));
    const yMin = 40;
    const plotB = H - m.b - lane;
    const X = (t) => m.l + ((t - x0) / (x1 - x0)) * (W - m.l - m.r);
    const Y = (v) => m.t + (1 - (Math.min(Math.max(v, yMin), yMax) - yMin) / (yMax - yMin)) * (plotB - m.t);

    const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img', 'aria-label': `Glucose over the last ${state.hours} hours` });

    // Target range band and threshold lines.
    svg('rect', { x: m.l, y: Y(th.high), width: W - m.l - m.r, height: Y(th.low) - Y(th.high), style: 'fill: var(--target)' }, root);
    const axis = svg('g', { class: 'axis' }, root);
    for (const v of [th.urgentLow, th.low, th.high, th.urgentHigh]) {
      svg('line', { class: 'threshold', x1: m.l, x2: W - m.r, y1: Y(v), y2: Y(v) }, root);
      const t = svg('text', { x: W - m.r + 6, y: Y(v) + 4 }, axis);
      t.textContent = fmtBG(v);
    }

    // Time grid.
    const grid = svg('g', { class: 'grid' }, root);
    grid.parentNode.insertBefore(grid, root.firstChild);
    const step = niceTimeStep(state.hours, W - m.l - m.r);
    const tzOffset = new Date().getTimezoneOffset() * MIN;
    let t = Math.ceil((x0 - tzOffset) / step) * step + tzOffset;
    for (; t <= x1; t += step) {
      svg('line', { x1: X(t), x2: X(t), y1: m.t, y2: plotB + lane }, grid);
      const label = svg('text', { x: X(t), y: H - 6, 'text-anchor': 'middle' }, axis);
      label.textContent = fmtTime(t);
    }

    // Readings.
    const dotR = state.hours <= 6 ? 3.5 : state.hours <= 12 ? 3 : 2.5;
    const dots = svg('g', {}, root);
    for (const r of rs) {
      svg('circle', { cx: X(r.t), cy: Y(r.v), r: dotR, style: `fill: ${classify(r.v).color}` }, dots);
    }
    if (!rs.length) {
      const e = svg('text', { class: 'empty', x: (W - m.r) / 2, y: (plotB) / 2, 'text-anchor': 'middle' }, root);
      e.textContent = 'No readings in this period';
    }

    // Treatments lane: insulin as downward triangles, carbs as circles.
    const laneY = plotB + lane / 2;
    for (const tt of tr) {
      const x = X(Date.parse(tt.created_at));
      if (Number(tt.insulin) > 0) {
        svg('path', { d: `M${x - 5},${laneY - 6} L${x + 5},${laneY - 6} L${x},${laneY + 4} Z`, style: 'fill: var(--insulin); stroke: var(--surface); stroke-width: 2; paint-order: stroke' }, root);
      }
      if (Number(tt.carbs) > 0) {
        svg('circle', { cx: x, cy: laneY + (Number(tt.insulin) > 0 ? 6 : 0), r: 4.5, style: 'fill: var(--carbs); stroke: var(--surface); stroke-width: 2; paint-order: stroke' }, root);
      }
    }

    // Hover layer.
    const cross = svg('line', { class: 'crosshair', y1: m.t, y2: plotB, visibility: 'hidden' }, root);
    const focus = svg('circle', { r: dotR + 2.5, visibility: 'hidden', style: 'stroke: var(--surface); stroke-width: 2' }, root);
    const hit = svg('rect', { x: m.l, y: 0, width: W - m.l - m.r, height: H, fill: 'transparent' }, root);
    const onMove = (ev) => {
      const rect = root.getBoundingClientRect();
      const px = ((ev.clientX - rect.left) / rect.width) * W;
      const tAt = x0 + ((px - m.l) / (W - m.l - m.r)) * (x1 - x0);
      let best = null;
      for (const r of rs) if (!best || Math.abs(r.t - tAt) < Math.abs(best.t - tAt)) best = r;
      const nearT = tr.filter((x) => Math.abs(Date.parse(x.created_at) - tAt) <= 10 * MIN * (state.hours / 6));
      if (!best && !nearT.length) return hideTip();
      let html = '';
      if (best && Math.abs(best.t - tAt) <= 15 * MIN) {
        const st = classify(best.v);
        cross.setAttribute('x1', X(best.t)); cross.setAttribute('x2', X(best.t));
        cross.setAttribute('visibility', 'visible');
        focus.setAttribute('cx', X(best.t)); focus.setAttribute('cy', Y(best.v));
        focus.style.fill = st.color;
        focus.setAttribute('visibility', 'visible');
        html += `<div class="t">${esc(fmtTime(best.t))}</div><div class="v">${esc(fmtBG(best.v))} ${esc(unitLabel())} ${ARROWS[best.d] ? ARROWS[best.d][0] : ''}</div><div class="muted small">${esc(st.label)}</div>`;
      } else {
        cross.setAttribute('visibility', 'hidden');
        focus.setAttribute('visibility', 'hidden');
      }
      for (const x of nearT) {
        html += `<div class="line small"><span class="muted">${esc(fmtTime(Date.parse(x.created_at)))}</span> ${esc(describeTreatment(x))}</div>`;
      }
      if (!html) return hideTip();
      showTip(html, ev.clientX, ev.clientY);
    };
    hit.addEventListener('pointermove', onMove);
    hit.addEventListener('pointerdown', onMove);
    hit.addEventListener('pointerleave', () => {
      hideTip();
      cross.setAttribute('visibility', 'hidden');
      focus.setAttribute('visibility', 'hidden');
    });

    box.replaceChildren(root);
  }

  // ---------------------------------------------------------------------------
  // Reports

  async function loadReport() {
    $$('#report-days button').forEach((b) => b.setAttribute('aria-pressed', String(Number(b.dataset.days) === state.reportDays)));
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
    try {
      state.report = await api(`/api/tl/v1/report?days=${state.reportDays}&tz=${encodeURIComponent(tz)}`);
    } catch (e) {
      $('#report-period').textContent = 'Could not load report: ' + e.message;
      return;
    }
    renderReport();
  }

  const pct = (v) => (v >= 10 || v === 0 ? Math.round(v) : v.toFixed(1)) + '%';

  function renderReport() {
    const rep = state.report;
    if (!rep) return;
    const s = rep.summary;
    $('#report-period').textContent = `${dayFmt.format(new Date(s.from))} – ${dayFmt.format(new Date(s.to))} · ${s.count.toLocaleString()} readings`;
    const tiles = [
      ['Time in range', s.count ? pct(s.ranges.inRange) : '–', 'Goal: over 70%'],
      ['Average glucose', s.count ? fmtBG(s.mean) : '–', unitLabel()],
      ['GMI', s.count ? s.gmi.toFixed(1) + '%' : '–', 'Estimated A1c'],
      ['Variability (CV)', s.count ? pct(s.cv) : '–', 'Goal: 36% or less'],
      ['CGM active', pct(s.coverage), 'Goal: over 70%'],
      ['Low events', s.count ? String(s.lowEvents) : '–', 'Below 70 mg/dL for 15+ min'.replace('70 mg/dL', mmol() ? '3.9 mmol/L' : '70 mg/dL')],
    ];
    $('#tiles').innerHTML = tiles.map(([k, v, sub]) => `<div class="tile"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div><div class="s">${esc(sub)}</div></div>`).join('');
    renderTIR(s);
    renderAGP(rep.agp);
    renderDaily(rep.days);
  }

  function renderTIR(s) {
    const r = s.ranges;
    const b = (mg) => fmtBG(mg);
    const rows = [
      ['Very high', `Above ${b(250)}`, r.veryHigh, 'var(--serious)', 'Goal: under 5%'],
      ['High', `${b(181)}–${b(250)}`, r.high, 'var(--warning)', 'Goal: under 25% with very high'],
      ['In range', `${b(70)}–${b(180)}`, r.inRange, 'var(--good)', 'Goal: over 70%'],
      ['Low', `${b(54)}–${b(69)}`, r.low, 'var(--critical)', 'Goal: under 4% with very low'],
      ['Very low', `Below ${b(54)}`, r.veryLow, 'var(--critical-deep)', 'Goal: under 1%'],
    ];
    if (!s.count) { $('#tir').innerHTML = '<p class="muted">No readings in this period.</p>'; return; }
    const bar = rows.map(([, , v, c]) => `<div style="flex:${Math.max(v, 0.0001)} 1 0; background:${c}" title="${pct(v)}"></div>`).join('');
    const list = rows.map(([lbl, range, v, c, goal]) => `
      <div class="tir-row">
        <div><span class="lbl"><i class="sw" style="background:${c}"></i>${esc(lbl)}</span><div class="sub">${esc(range)} · ${esc(goal)}</div></div>
        <b>${pct(v)}</b>
      </div>`).join('');
    $('#tir').innerHTML = `<div class="tir"><div class="tir-bar" aria-hidden="true">${bar}</div><div class="tir-rows">${list}</div></div>
      <p class="muted small">Time in tight range (${b(70)}–${b(140)}): <b>${pct(r.tight)}</b></p>`;
  }

  function renderAGP(agp) {
    const box = $('#agp');
    const W = box.clientWidth;
    if (!W) return;
    const H = W < 600 ? 240 : 300;
    const m = { l: 6, r: 44, t: 10, b: 24 };
    const valid = agp.filter((b) => b.n >= 3);
    const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img', 'aria-label': 'Ambulatory glucose profile' });
    if (!valid.length) {
      const e = svg('text', { class: 'empty', x: W / 2, y: H / 2, 'text-anchor': 'middle' }, root);
      e.textContent = 'Not enough data for a profile yet';
      box.replaceChildren(root);
      return;
    }
    const yMax = Math.max(300, ...valid.map((b) => b.p95 + 10));
    const yMin = 40;
    const X = (min) => m.l + (min / 1440) * (W - m.l - m.r);
    const Y = (v) => m.t + (1 - (Math.min(Math.max(v, yMin), yMax) - yMin) / (yMax - yMin)) * (H - m.b - m.t);
    const grid = svg('g', { class: 'grid' }, root);
    const axis = svg('g', { class: 'axis' }, root);
    const hourStep = (W - m.l - m.r) / 8 >= 56 ? 3 : 6;
    for (let h = 0; h <= 24; h += hourStep) {
      svg('line', { x1: X(h * 60), x2: X(h * 60), y1: m.t, y2: H - m.b }, grid);
      const t = svg('text', { x: X(h * 60), y: H - 6, 'text-anchor': h === 0 ? 'start' : h === 24 ? 'end' : 'middle' }, axis);
      t.textContent = String(h % 24).padStart(2, '0') + ':00';
    }
    // Draw contiguous runs so gaps in data stay gaps.
    const runs = [];
    let cur = [];
    for (const b of agp) {
      if (b.n >= 3) cur.push(b); else if (cur.length) { runs.push(cur); cur = []; }
    }
    if (cur.length) runs.push(cur);
    const mid = (b) => b.minute + 7.5;
    const band = (run, lo, hi, color) => {
      const top = run.map((b) => `${X(mid(b))},${Y(b[hi])}`);
      const bot = run.slice().reverse().map((b) => `${X(mid(b))},${Y(b[lo])}`);
      svg('polygon', { points: top.concat(bot).join(' '), style: `fill: ${color}` }, root);
    };
    for (const run of runs) band(run, 'p5', 'p95', 'var(--agp-outer)');
    for (const run of runs) band(run, 'p25', 'p75', 'var(--agp-inner)');
    for (const run of runs) {
      svg('polyline', { points: run.map((b) => `${X(mid(b))},${Y(b.p50)}`).join(' '), style: 'fill: none; stroke: var(--agp-median); stroke-width: 2; stroke-linejoin: round' }, root);
    }
    for (const v of [70, 180]) {
      svg('line', { class: 'threshold', x1: m.l, x2: W - m.r, y1: Y(v), y2: Y(v) }, root);
      const t = svg('text', { x: W - m.r + 6, y: Y(v) + 4 }, axis);
      t.textContent = fmtBG(v);
    }
    const cross = svg('line', { class: 'crosshair', y1: m.t, y2: H - m.b, visibility: 'hidden' }, root);
    const hit = svg('rect', { x: m.l, y: 0, width: W - m.l - m.r, height: H, fill: 'transparent' }, root);
    const onMove = (ev) => {
      const rect = root.getBoundingClientRect();
      const px = ((ev.clientX - rect.left) / rect.width) * W;
      const minute = Math.max(0, Math.min(1439, ((px - m.l) / (W - m.l - m.r)) * 1440));
      const b = agp[Math.floor(minute / 15)];
      if (!b || b.n < 3) { hideTip(); cross.setAttribute('visibility', 'hidden'); return; }
      cross.setAttribute('x1', X(mid(b))); cross.setAttribute('x2', X(mid(b)));
      cross.setAttribute('visibility', 'visible');
      const hh = (n) => String(Math.floor(n / 60)).padStart(2, '0') + ':' + String(n % 60).padStart(2, '0');
      showTip(`<div class="t">${hh(b.minute)}–${hh(b.minute + 15)}</div>
        <div class="v">Median ${esc(fmtBG(b.p50))} ${esc(unitLabel())}</div>
        <div class="small muted">25–75%: ${esc(fmtBG(b.p25))}–${esc(fmtBG(b.p75))}</div>
        <div class="small muted">5–95%: ${esc(fmtBG(b.p5))}–${esc(fmtBG(b.p95))}</div>`, ev.clientX, ev.clientY);
    };
    hit.addEventListener('pointermove', onMove);
    hit.addEventListener('pointerdown', onMove);
    hit.addEventListener('pointerleave', () => { hideTip(); cross.setAttribute('visibility', 'hidden'); });
    box.replaceChildren(root);
  }

  function renderDaily(days) {
    const rows = days.slice().reverse().map((d) => {
      const date = new Date(d.date + 'T12:00:00');
      return `<tr>
        <td>${esc(dayFmt.format(date))}</td>
        <td class="num">${d.count}</td>
        <td class="num">${esc(fmtBG(d.mean))}</td>
        <td class="num">${pct(d.inRange)}</td>
        <td class="num">${pct(d.low)}</td>
        <td class="num">${pct(d.high)}</td>
        <td><span class="minibar" aria-hidden="true"><i style="width:${d.low}%;background:var(--critical)"></i><i style="width:${d.inRange}%;background:var(--good)"></i><i style="width:${d.high}%;background:var(--warning)"></i></span></td>
      </tr>`;
    }).join('');
    $('#daily').innerHTML = `<thead><tr><th>Day</th><th class="num">Readings</th><th class="num">Average</th><th class="num">In range</th><th class="num">Low</th><th class="num">High</th><th>Distribution</th></tr></thead>
      <tbody>${rows || '<tr><td colspan="7" class="muted">No readings in this period.</td></tr>'}</tbody>`;
  }

  // ---------------------------------------------------------------------------
  // Settings

  const ALERT_LABELS = {
    urgent_low: 'Urgent low', low: 'Low', predicted_low: 'Predicted low',
    high: 'High', urgent_high: 'Urgent high', stale: 'Stale data', urgent_stale: 'No data (urgent)',
  };

  function renderSettings() {
    const s = state.settings;
    const admin = state.me.isAdmin;
    const f = $('#settings-form');
    f.name.value = s.name;
    f.units.value = s.units;
    f.urgentLow.value = toDisplay(s.thresholds.urgentLow);
    f.low.value = toDisplay(s.thresholds.low);
    f.high.value = toDisplay(s.thresholds.high);
    f.urgentHigh.value = toDisplay(s.thresholds.urgentHigh);
    for (const k of ['staleMinutes', 'urgentStaleMinutes', 'predictMinutes', 'repeatWarnMinutes', 'repeatUrgentMinutes']) f[k].value = s.alerts[k];
    $('#alert-toggles').innerHTML = Object.entries(ALERT_LABELS).map(([k, l]) =>
      `<label class="toggle"><input type="checkbox" name="alert-${k}" ${s.alerts.enabled[k] ? 'checked' : ''}> ${esc(l)}</label>`).join('');

    $('#settings-readonly').hidden = admin;
    $$('input, select, button', f).forEach((el) => (el.disabled = !admin));
    $('.save-bar', f).hidden = !admin;
    $('#notifiers-card').hidden = !admin;
    $('#access-card').hidden = !admin;
    $('#audit-card').hidden = !admin;
    $('#connect-card').hidden = !state.me.canWrite;

    $('#ns-url').textContent = location.origin;
    $('#xdrip-url').textContent = `${location.protocol}//YOUR-TOKEN@${location.host}/api/v1/`;
    $('#version').textContent = `Tideline ${state.me.version} · signed in as ${state.me.principal.name} (${state.me.principal.role})`;
    updateDeviceButtons();

    if (admin) {
      state.editNotifiers = (s.notifiers || []).map((n) => ({ ...n }));
      renderNotifiers();
      loadTokens();
      loadAudit();
    }
  }

  function renderNotifiers() {
    const el = $('#notifiers');
    if (!state.editNotifiers.length) {
      el.innerHTML = '<p class="muted small">No notification targets yet. <a href="https://ntfy.sh" target="_blank" rel="noopener">ntfy</a> is a free, quick way to get push notifications on your phone.</p>';
      return;
    }
    el.innerHTML = state.editNotifiers.map((n, i) => `
      <div class="notifier" data-i="${i}">
        <label>Name<input data-k="name" value="${esc(n.name)}" placeholder="My phone"></label>
        <label>Type
          <select data-k="type">
            <option value="ntfy" ${n.type === 'ntfy' ? 'selected' : ''}>ntfy</option>
            <option value="pushover" ${n.type === 'pushover' ? 'selected' : ''}>Pushover</option>
            <option value="webhook" ${n.type === 'webhook' ? 'selected' : ''}>Webhook</option>
          </select>
        </label>
        ${n.type === 'pushover' ? '' : `<label>${n.type === 'ntfy' ? 'Topic URL' : 'URL'}<input data-k="url" value="${esc(n.url || '')}" placeholder="${n.type === 'ntfy' ? 'https://ntfy.sh/your-secret-topic' : 'https://…'}"></label>`}
        <label>${n.type === 'pushover' ? 'App token' : 'Access token (optional)'}<input data-k="token" value="${esc(n.token || '')}" type="password" autocomplete="off"></label>
        ${n.type === 'pushover' ? `<label>User key<input data-k="user" value="${esc(n.user || '')}" autocomplete="off"></label>` : ''}
        <label>Send
          <select data-k="minLevel">
            <option value="warn" ${n.minLevel === 'warn' ? 'selected' : ''}>All alerts</option>
            <option value="urgent" ${n.minLevel === 'urgent' ? 'selected' : ''}>Urgent alerts only</option>
          </select>
        </label>
        <label>Escalate after <span class="muted">min (0 = immediately)</span><input data-k="escalateAfterMinutes" type="number" min="0" value="${n.escalateAfterMinutes || 0}"></label>
        <div class="actions">
          <label class="toggle"><input type="checkbox" data-k="enabled" ${n.enabled ? 'checked' : ''}> Enabled</label>
          <button type="button" class="small" data-test="${i}">Send test</button>
          <button type="button" class="small danger" data-remove="${i}">Remove</button>
        </div>
        <p class="small muted" data-result="${i}"></p>
      </div>`).join('');
  }

  function collectSettings() {
    const f = $('#settings-form');
    const s = state.settings;
    // Units may have just changed: thresholds are entered in the units selected on the form.
    const formMmol = f.units.value === 'mmol';
    // Keep the exact stored mg/dL value when a field wasn't edited, so a
    // mg/dL -> mmol/L -> mg/dL round trip can't drift (55 -> 3.1 -> 56).
    const conv = (v, key) => {
      const orig = s.thresholds[key];
      const shown = formMmol ? Math.round((orig / 18) * 10) / 10 : Math.round(orig);
      if (Number(v) === shown) return orig;
      return Math.round(formMmol ? Number(v) * 18 : Number(v));
    };
    const enabled = {};
    for (const k of Object.keys(ALERT_LABELS)) enabled[k] = f['alert-' + k].checked;
    return {
      name: f.name.value.trim() || 'Tideline',
      units: f.units.value,
      thresholds: { urgentLow: conv(f.urgentLow.value, 'urgentLow'), low: conv(f.low.value, 'low'), high: conv(f.high.value, 'high'), urgentHigh: conv(f.urgentHigh.value, 'urgentHigh') },
      alerts: {
        ...s.alerts,
        enabled,
        staleMinutes: Number(f.staleMinutes.value),
        urgentStaleMinutes: Number(f.urgentStaleMinutes.value),
        predictMinutes: Number(f.predictMinutes.value),
        repeatWarnMinutes: Number(f.repeatWarnMinutes.value),
        repeatUrgentMinutes: Number(f.repeatUrgentMinutes.value),
      },
      notifiers: state.editNotifiers.map((n) => ({ ...n, escalateAfterMinutes: Number(n.escalateAfterMinutes) || 0 })),
    };
  }

  async function loadTokens() {
    try {
      const toks = await api('/api/tl/v1/tokens');
      const roleName = { reader: 'Follower', uploader: 'Uploader', admin: 'Admin' };
      const now = Date.now();
      $('#tokens').innerHTML = `<thead><tr><th>Name</th><th>Access</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr></thead><tbody>` +
        (toks.length ? toks.map((t) => {
          const revoked = !!t.revokedAt;
          const expired = t.expiresAt && t.expiresAt < now;
          const status = revoked ? '<span class="badge off">Revoked</span>' : expired ? '<span class="badge off">Expired</span>' : '';
          return `<tr>
            <td>${esc(t.name)} ${status}<div class="small muted"><code>${esc(t.prefix)}…</code></div></td>
            <td>${esc(roleName[t.role] || t.role)}</td>
            <td>${esc(fmtDateTime(t.createdAt))}</td>
            <td>${t.lastUsedAt ? esc(ago(t.lastUsedAt)) : '<span class="muted">Never</span>'}</td>
            <td>${t.expiresAt ? esc(fmtDateTime(t.expiresAt)) : '<span class="muted">Never</span>'}</td>
            <td>${revoked ? '' : `<button class="small danger" data-revoke="${esc(t.id)}">Revoke</button>`}</td>
          </tr>`;
        }).join('') : '<tr><td colspan="6" class="muted">No tokens yet.</td></tr>') + '</tbody>';
    } catch (e) {
      $('#tokens').innerHTML = `<tr><td class="error">${esc(e.message)}</td></tr>`;
    }
  }

  async function loadAudit() {
    try {
      const ev = await api('/api/tl/v1/audit?limit=50');
      $('#audit').innerHTML = `<thead><tr><th>When</th><th>Who</th><th>What</th><th>Detail</th></tr></thead><tbody>` +
        (ev.length ? ev.map((e) => `<tr><td>${esc(fmtDateTime(e.at))}</td><td>${esc(e.actor)}</td><td>${esc(e.action)}</td><td>${esc(e.detail)}</td></tr>`).join('')
          : '<tr><td colspan="4" class="muted">Nothing yet.</td></tr>') + '</tbody>';
    } catch { /* non-critical */ }
  }

  function showNewToken(token, role) {
    const el = $('#new-token');
    const origin = location.origin;
    let html = `<p><b>Copy this token now.</b> It won't be shown again.</p><p><code>${esc(token)}</code> <button type="button" class="small" data-copy="${esc(token)}">Copy</button></p>`;
    if (role === 'reader') {
      const link = `${origin}/?token=${encodeURIComponent(token)}`;
      html += `<p>Share link for followers:</p><p><code>${esc(link)}</code> <button type="button" class="small" data-copy="${esc(link)}">Copy link</button></p>`;
    } else if (role === 'uploader') {
      const xdrip = `${location.protocol}//${token}@${location.host}/api/v1/`;
      html += `<p>xDrip+ base URL:</p><p><code>${esc(xdrip)}</code> <button type="button" class="small" data-copy="${esc(xdrip)}">Copy</button></p>
        <p class="muted small">Loop, Trio and other Nightscout uploaders: use <code>${esc(origin)}</code> as the site URL and paste this token into the API secret field.</p>
        <p class="muted small">AndroidAPS: use the NSClient v3 plugin with <code>${esc(origin)}</code> as the URL and this token as the access token.</p>`;
    }
    el.innerHTML = html;
    el.hidden = false;
  }

  // ---------------------------------------------------------------------------
  // Event wiring

  function wireStatic() {
    window.addEventListener('hashchange', route);

    $('#login-form').addEventListener('submit', async (e) => {
      e.preventDefault();
      const v = $('#login-token').value.trim();
      if (!v) return;
      state.token = v;
      try {
        await loadMe();
      } catch (err) {
        state.token = null;
        $('#login-error').textContent = err.status === 401 ? 'That token or secret was not accepted.' : 'Could not sign in: ' + err.message;
        return;
      }
      store.set('token', v);
      location.reload();
    });

    $('#range').addEventListener('click', (e) => {
      const b = e.target.closest('button[data-hours]');
      if (!b) return;
      state.hours = Number(b.dataset.hours);
      store.set('hours', String(state.hours));
      renderNow();
    });

    $('#report-days').addEventListener('click', (e) => {
      const b = e.target.closest('button[data-days]');
      if (!b) return;
      state.reportDays = Number(b.dataset.days);
      store.set('reportDays', String(state.reportDays));
      loadReport();
    });

    $('#alerts').addEventListener('click', async (e) => {
      const b = e.target.closest('button[data-snooze]');
      if (!b) return;
      b.disabled = true;
      try {
        setAlerts(await api(`/api/tl/v1/alerts/${encodeURIComponent(b.dataset.snooze)}/snooze`, { method: 'POST', body: { minutes: Number(b.dataset.min) } }));
      } catch (err) { alert('Could not snooze: ' + err.message); b.disabled = false; }
    });

    $('#entry-form').addEventListener('submit', async (e) => {
      e.preventDefault();
      const f = e.target;
      const carbs = Number(f.carbs.value) || 0;
      const insulin = Number(f.insulin.value) || 0;
      const notes = f.notes.value.trim();
      if (!carbs && !insulin && !notes) { $('#entry-error').textContent = 'Enter carbs, insulin or a note.'; return; }
      const eventType = carbs && insulin ? 'Meal Bolus' : carbs ? 'Carb Correction' : insulin ? 'Correction Bolus' : 'Note';
      const doc = { eventType, created_at: new Date().toISOString() };
      if (carbs) doc.carbs = carbs;
      if (insulin) doc.insulin = insulin;
      if (notes) doc.notes = notes;
      try {
        await api('/api/v1/treatments', { method: 'POST', body: [doc] });
        f.reset();
        $('#entry-error').textContent = '';
      } catch (err) { $('#entry-error').textContent = 'Could not save: ' + err.message; }
    });

    $('#treatments').addEventListener('click', async (e) => {
      const b = e.target.closest('button[data-del]');
      if (!b || !confirm('Delete this treatment?')) return;
      try { await api('/api/v1/treatments/' + encodeURIComponent(b.dataset.del), { method: 'DELETE' }); } catch (err) { alert(err.message); }
    });

    $('#settings-form').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('#settings-error').textContent = '';
      $('#settings-ok').textContent = '';
      try {
        const saved = await api('/api/tl/v1/settings', { method: 'PUT', body: collectSettings() });
        state.settings = saved;
        applySettings();
        renderSettings();
        $('#settings-ok').textContent = 'Saved.';
      } catch (err) { $('#settings-error').textContent = err.message; }
    });

    $('#settings-form').units.addEventListener('change', (e) => {
      // Convert the threshold fields so the numbers keep their meaning.
      const f = $('#settings-form');
      const toMmol = e.target.value === 'mmol';
      for (const k of ['urgentLow', 'low', 'high', 'urgentHigh']) {
        const v = Number(f[k].value);
        f[k].value = toMmol ? Math.round((v / 18) * 10) / 10 : Math.round(v * 18);
      }
    });

    $('#add-notifier').addEventListener('click', () => {
      state.editNotifiers.push({ id: '', name: 'My phone', type: 'ntfy', url: '', token: '', user: '', minLevel: 'warn', escalateAfterMinutes: 0, enabled: true });
      renderNotifiers();
    });

    const notifiers = $('#notifiers');
    notifiers.addEventListener('input', (e) => {
      const row = e.target.closest('.notifier');
      const k = e.target.dataset.k;
      if (!row || !k) return;
      const n = state.editNotifiers[Number(row.dataset.i)];
      n[k] = e.target.type === 'checkbox' ? e.target.checked : e.target.value;
      if (k === 'type') renderNotifiers();
    });
    notifiers.addEventListener('click', async (e) => {
      const rm = e.target.closest('button[data-remove]');
      if (rm) { state.editNotifiers.splice(Number(rm.dataset.remove), 1); renderNotifiers(); return; }
      const tb = e.target.closest('button[data-test]');
      if (tb) {
        const i = Number(tb.dataset.test);
        const out = $(`[data-result="${i}"]`, notifiers);
        out.textContent = 'Sending…';
        try {
          const n = state.editNotifiers[i];
          await api('/api/tl/v1/notifiers/test', { method: 'POST', body: { ...n, escalateAfterMinutes: Number(n.escalateAfterMinutes) || 0 } });
          out.textContent = 'Sent. Check your device.';
        } catch (err) { out.textContent = 'Failed: ' + err.message; }
      }
    });

    $('#token-form').addEventListener('submit', async (e) => {
      e.preventDefault();
      const f = e.target;
      try {
        const res = await api('/api/tl/v1/tokens', { method: 'POST', body: { name: f.name.value, role: f.role.value, expiresInDays: Number(f.expiresInDays.value) } });
        showNewToken(res.token, f.role.value);
        f.reset();
        loadTokens();
        loadAudit();
      } catch (err) { alert('Could not create token: ' + err.message); }
    });

    document.addEventListener('click', async (e) => {
      const c = e.target.closest('button[data-copy]');
      if (c) {
        try { await navigator.clipboard.writeText(c.dataset.copy); c.textContent = 'Copied'; } catch { c.textContent = 'Copy failed'; }
      }
      const r = e.target.closest('button[data-revoke]');
      if (r && confirm('Revoke this token? Anything using it will stop working.')) {
        try { await api('/api/tl/v1/tokens/' + encodeURIComponent(r.dataset.revoke), { method: 'DELETE' }); loadTokens(); loadAudit(); } catch (err) { alert(err.message); }
      }
    });

    $('#sound-toggle').addEventListener('click', () => {
      if (state.sound) { state.sound = false; store.set('sound', '0'); updateDeviceButtons(); } else enableSound();
    });
    $('#notify-toggle').addEventListener('click', async () => {
      if ('Notification' in window) await Notification.requestPermission();
      updateDeviceButtons();
    });
    $('#logout').addEventListener('click', () => signOut(true));

    // Browsers only allow audio after a user gesture; re-arm on the first one.
    if (store.get('sound') === '1') {
      const arm = () => { enableSound(); document.removeEventListener('pointerdown', arm); };
      document.addEventListener('pointerdown', arm);
    }
    setInterval(alarmLoop, 1000);

    let resizeT;
    new ResizeObserver(() => {
      clearTimeout(resizeT);
      resizeT = setTimeout(() => {
        if (!$('#view-now').hidden) renderGlucoseChart();
        if (!$('#view-reports').hidden && state.report) renderAGP(state.report.agp);
      }, 100);
    }).observe($('#app'));

    if ('serviceWorker' in navigator) navigator.serviceWorker.register('sw.js').catch(() => {});
  }

  boot();
})();
