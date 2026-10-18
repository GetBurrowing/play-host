// Multi-viewer input-to-photon driver for a Burrow game_stream canvas (task
// 01a0dbd6 stage 5). One headless Chrome browser context per viewer, so each
// viewer is its own LiveView and its own RTCPeerConnection. Players join
// their own slot, capture input and keep the Burrow hook's latency probes
// running; spectators only watch. The driver reads the hook's RAW probe
// samples (every sample, not only its 60-sample rolling window).
//
// usage: node run-probes-multi.mjs --cdp 127.0.0.1:9223 \
//          --app http://<host-b>:4033 --canvas <canvas uuid> \
//          --players 2 --spectators 0 --n 50 --label p2-lan \
//          --out results/p2-lan-01a0dbd6.json [--shot results/p2-lan.jpg] \
//          [--users user-1,user-2,user-3,user-4,user-5,user-6] [--mobile 1]
//          [--jitter-ms 34] [--forge-spectator]
//
// Node 22+, no npm deps. Needs a dev core with /dev/login?as=<name> and a
// running multi session behind the canvas. Every browser context is closed
// in `finally`, whatever happens.
//
// What it records per viewer (the JSON `viewers[]`):
//   - samples: [{ms, at, slot}] from the hook's RollingStats.push, wrapped on
//     the live hook instance; `sent` probes from the data-channel send log;
//     `lost` = sent - samples - still pending at the end;
//   - ttff_ms: RTCPeerConnection construction to the first presented frame
//     (requestVideoFrameCallback), and `nav_to_first_frame_ms`;
//   - fps (the hook's 1 s counter, sampled every 2 s), path, connectionState,
//     inbound-rtp framesDecoded / framesDropped / jitterBufferDelay;
//   - sends: every RTCDataChannel.send the page made, by message type. For a
//     spectator this must be empty.
//   - window: the hook's rolling p50/p95 when the last game_stream_stats push
//     went out (compare with the session row's slot_stats).
import fs from 'node:fs';

const arg = (k, d) => { const i = process.argv.indexOf('--' + k); return i > 0 ? process.argv[i + 1] : d; };
const cdpHost = arg('cdp', '127.0.0.1:9223'), app = arg('app', 'http://127.0.0.1:4033');
const canvas = arg('canvas'), players = +arg('players', 1), spectators = +arg('spectators', 0);
const n = +arg('n', 50), keyEvery = +arg('key-every', 10000), label = arg('label', 'run');
const out = arg('out'), shotOut = arg('shot'), maxS = +arg('max-seconds', 240);
const users = arg('users', 'user-1,user-2,user-3,user-4,user-5,user-6,user-7,user-8').split(',');
const jitterMs = +arg('jitter-ms', 0);
// --forge-spectator: AFTER a spectator's honest send count is read, it sends
// ONE forged key on the host's input-events channel, as a positive control
// for the host's per-peer dropped-input counter (the host must drop it).
const forgeSpectator = process.argv.includes('--forge-spectator');
const mobile = new Set((arg('mobile', '') || '').split(',').filter(Boolean).map(Number));
if (!canvas) throw new Error('--canvas is required');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const t0 = Date.now();
const log = (...a) => console.error(`[${((Date.now() - t0) / 1000).toFixed(1)}s]`, ...a);

// Installed before any page script: records every data-channel send, every
// RTCPeerConnection (with its construction time) and the first presented
// video frame.
const INSTRUMENT = `(() => {
  window.__s5 = { sends: [], pcs: [], firstFrameAt: null, pcAt: null, jitterMs: ${jitterMs} }
  // --jitter-ms J: every 1000 ms setInterval (the hook's tick, which sends
  // one probe per tick) instead fires after 1000 + U[0, J) ms. The hook's
  // tick is otherwise phase-locked to a 60 Hz frame clock (1000 ms is 60
  // frames), so every probe of a run lands at the same frame phase and the
  // run samples one 16.7 ms bucket instead of the phase distribution.
  if (${jitterMs} > 0) {
    const si = window.setInterval.bind(window), ci = window.clearInterval.bind(window)
    const live = new Map(); let next = 1e9
    window.setInterval = function (fn, ms, ...rest) {
      if (ms !== 1000 || typeof fn !== 'function') return si(fn, ms, ...rest)
      const id = next++
      const arm = () => live.set(id, setTimeout(() => { if (!live.has(id)) return; try { fn(...rest) } finally { if (live.has(id)) arm() } }, 1000 + Math.random() * ${jitterMs}))
      arm()
      return id
    }
    window.clearInterval = function (id) {
      if (live.has(id)) { clearTimeout(live.get(id)); live.delete(id); return }
      return ci(id)
    }
  }
  const s5 = window.__s5
  const send = RTCDataChannel.prototype.send
  RTCDataChannel.prototype.send = function (data) {
    let t = null, seq = null
    try { const m = JSON.parse(data); t = m.t || null; seq = m.seq == null ? null : m.seq } catch (_) {}
    s5.sends.push({ label: this.label, t, seq, at: performance.now() })
    return send.apply(this, arguments)
  }
  const PC = window.RTCPeerConnection
  window.RTCPeerConnection = function (...args) {
    const pc = new PC(...args)
    const at = performance.now()
    if (s5.pcAt == null) s5.pcAt = at
    s5.pcs.push({ pc, at })
    s5.channels = s5.channels || []
    pc.addEventListener('datachannel', e => s5.channels.push(e.channel))
    return pc
  }
  window.RTCPeerConnection.prototype = PC.prototype
  Object.setPrototypeOf(window.RTCPeerConnection, PC)
  // Every LiveView frame that carries a game_stream push, as it arrives on
  // the socket (before any hook sees it): time, phoenix event, and whether
  // it holds an offer / status. No SDP is kept.
  s5.ws = []
  const WS = window.WebSocket
  window.WebSocket = function (...a) {
    const ws = new WS(...a)
    ws.addEventListener('message', e => {
      if (typeof e.data !== 'string' || !e.data.includes('game_stream')) return
      let ev = null
      try { ev = JSON.parse(e.data)[3] } catch (_) {}
      s5.ws.push({ at: performance.now(), ev, offer: e.data.includes('"kind":"offer"'), status: (e.data.match(/"state":"([a-z]+)"/) || [])[1] || null, peer: (e.data.match(/"peer_id":"([0-9a-f-]{36})"/) || [])[1] || null })
    })
    return ws
  }
  window.WebSocket.prototype = WS.prototype
  Object.setPrototypeOf(window.WebSocket, WS)
  // Wrap the hook's onServer as soon as the hook exists (polled every 5 ms).
  s5.server = []
  const early = setInterval(() => {
    if (!window.liveSocket) return
    for (const el of document.querySelectorAll('[phx-hook=GameStream]')) {
      let view = null
      liveSocket.owner(el, v => (view = v))
      const hook = view && el.phxPrivate && view.viewHooks[el.phxPrivate.hookId]
      if (!hook || hook.__s5early) continue
      hook.__s5early = true
      const on = hook.onServer.bind(hook)
      hook.onServer = msg => {
        s5.server.push({ at: performance.now(), type: msg && msg.type, kind: msg && msg.kind, state: msg && msg.status && msg.status.state, active: hook.active(), mine: !!msg && msg.session_id === hook.sessionId, peerId: hook.peerId || null, panel: hook.panel })
        return on(msg)
      }
      s5.hookedAt = performance.now()
    }
  }, 5)
  setTimeout(() => clearInterval(early), 60000)
  const arm = setInterval(() => {
    const v = document.querySelector('[phx-hook=GameStream] video')
    if (!v || typeof v.requestVideoFrameCallback !== 'function') return
    clearInterval(arm)
    v.requestVideoFrameCallback(now => { s5.firstFrameAt = now })
  }, 20)
})()`;

// Finds the live GameStream hook instance (the copy holding a pc) and wraps
// its RollingStats.push so every sample is kept.
const HOOK = `(() => {
  const els = Array.from(document.querySelectorAll('[phx-hook=GameStream]'))
  for (const el of els) {
    // liveSocket.owner finds the (possibly nested) LiveView that owns el;
    // getViewByEl does not see a hook element inside a nested view.
    let view = null
    if (window.liveSocket) liveSocket.owner(el, v => (view = v))
    const hook = view && el.phxPrivate && view.viewHooks[el.phxPrivate.hookId]
    if (!hook) continue
    window.__s5hooks = window.__s5hooks || []
    if (!hook.__s5) {
      hook.__s5 = { samples: [] }
      const push = hook.rtt.push.bind(hook.rtt)
      hook.rtt.push = ms => { hook.__s5.samples.push({ ms, at: Date.now(), slot: hook.probeSlot }); return push(ms) }
      const pushEv = hook.push ? hook.push.bind(hook) : null
      if (pushEv) hook.push = (ev, payload) => {
        if (ev === 'game_stream_stats') hook.__s5.lastStats = { at: Date.now(), payload: JSON.parse(JSON.stringify(payload)) }
        return pushEv(ev, payload)
      }
      window.__s5hooks.push(hook)
    }
  }
  return (window.__s5hooks || []).length
})()`;

const STATE = `(() => {
  const hooks = window.__s5hooks || []
  const h = hooks.find(h => h.pc) || hooks[0]
  const el = h ? h.el : document.querySelector('[phx-hook=GameStream]')
  if (!el) return null
  const q = s => el.querySelector(s)
  const vis = s => { const x = q(s); return !!x && !x.hidden && x.getClientRects().length > 0 }
  const v = q('video')
  return {
    state: el.dataset.state,
    frames: v ? v.getVideoPlaybackQuality().totalVideoFrames : 0,
    join: vis('[data-gs-join]') ? q('[data-gs-join]').textContent.trim() : null,
    leave: vis('[data-gs-leave]'),
    chips: Array.from(el.querySelectorAll('[data-gs-chips] [data-gs-chip]')).map(c => c.dataset.gsChipState + ':' + c.textContent.replace(/\\s+/g, ' ').trim()),
    watching: (q('[data-gs-watching]') || { textContent: '' }).textContent.trim(),
    ring: !!q('[data-gs-ring]') && !q('[data-gs-ring]').hidden,
    hooked: hooks.length,
    player: h ? !!(h.router && h.router.player) : null,
    probeSlot: h ? h.probeSlot : null,
    samples: h && h.__s5 ? h.__s5.samples.length : 0,
    fps: h ? h.fps : null,
    path: h ? h.path : null,
    pcState: h && h.pc ? h.pc.connectionState : null,
  }
})()`;

// What a viewer that never went live was doing.
const DIAG = `(() => {
  const hooks = window.__s5hooks || []
  const h = hooks[0]
  return {
    now: performance.now(), pcAt: window.__s5.pcAt,
    pcs: window.__s5.pcs.map(p => ({ at: p.at, sig: p.pc.signalingState, ice: p.pc.iceConnectionState, conn: p.pc.connectionState, gather: p.pc.iceGatheringState })),
    hook: h ? { sessionId: h.sessionId, peerId: h.peerId, panel: h.panel, serverState: h.serverState, pcId: h.pcId, active: h.active(), suspended: h.suspended, dead: h.dead, multi: h.multi } : null,
    hookedAt: window.__s5.hookedAt, server: window.__s5.server, ws: window.__s5.ws,
    sends: window.__s5.sends.length,
  }
})()`;

const STATS = `(async () => {
  const hooks = window.__s5hooks || []
  const h = hooks.find(h => h.pc)
  const s5 = window.__s5
  const r = { pcs: s5.pcs.length, pcAt: s5.pcAt, firstFrameAt: s5.firstFrameAt }
  if (h && h.pc) {
    const rep = await h.pc.getStats()
    rep.forEach(s => {
      if (s.type === 'inbound-rtp' && s.kind === 'video') Object.assign(r, {
        framesDecoded: s.framesDecoded, framesDropped: s.framesDropped, framesReceived: s.framesReceived,
        jitterBufferDelay: s.jitterBufferDelay, jitterBufferEmittedCount: s.jitterBufferEmittedCount,
        totalDecodeTime: s.totalDecodeTime, bytesReceived: s.bytesReceived, packetsLost: s.packetsLost,
        frameWidth: s.frameWidth, frameHeight: s.frameHeight, pliCount: s.pliCount, keyFramesDecoded: s.keyFramesDecoded })
    })
    r.connectionState = h.pc.connectionState
  }
  return r
})()`;

// ── minimal CDP client (flattened sessions) ────────────────────────────────
const ver = await (await fetch(`http://${cdpHost}/json/version`)).json();
const ws = new WebSocket(ver.webSocketDebuggerUrl.replace(/ws:\/\/[^/]+/, `ws://${cdpHost}`));
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pend = new Map();
ws.onmessage = m => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) { const { res, rej } = pend.get(d.id); pend.delete(d.id); d.error ? rej(new Error(JSON.stringify(d.error))) : res(d.result); }
};
const send = (method, params = {}, sessionId) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params, sessionId })); });
const contexts = [];

async function newViewer(i, user, role) {
  const { browserContextId } = await send('Target.createBrowserContext', {});
  contexts.push(browserContextId);
  const { targetId } = await send('Target.createTarget', { url: 'about:blank', browserContextId });
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
  const s = (m, p) => send(m, p, sessionId);
  await s('Page.enable'); await s('Runtime.enable');
  const m = mobile.has(i);
  await s('Emulation.setDeviceMetricsOverride', m ? { width: 375, height: 812, deviceScaleFactor: 1, mobile: true } : { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false });
  await s('Page.addScriptToEvaluateOnNewDocument', { source: INSTRUMENT });
  const ev = async expr => {
    const r = await s('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400));
    return r.result.value;
  };
  const goto = async u => { await s('Page.navigate', { url: u }); await sleep(1500); };
  const key = async (code, k, vk) => {
    await s('Input.dispatchKeyEvent', { type: 'keyDown', code, key: k, windowsVirtualKeyCode: vk });
    await sleep(60);
    await s('Input.dispatchKeyEvent', { type: 'keyUp', code, key: k, windowsVirtualKeyCode: vk });
  };
  const click = sel => ev(`(() => { const h = (window.__s5hooks || []).find(h => h.pc) || (window.__s5hooks || [])[0]; const el = h ? h.el : document.querySelector('[phx-hook=GameStream]'); const n = el.querySelector(${JSON.stringify(sel)}); if (!n) throw new Error('no ${sel}'); n.click(); return true })()`);
  return { i, user, role, mobile: m, targetId, s, ev, goto, key, click, fps: [], rec: {} };
}

async function waitFor(v, pred, what, ms = 30000) {
  const t = Date.now(); let st;
  while (Date.now() - t < ms) {
    await v.ev(HOOK).catch(() => 0);
    st = await v.ev(STATE).catch(() => null);
    if (st && pred(st)) return st;
    await sleep(250);
  }
  const diag = await v.ev(DIAG).catch(e => ({ err: String(e) }));
  console.error(`viewer ${v.i} (${v.user}) DIAG`, JSON.stringify(diag));
  throw new Error(`viewer ${v.i} (${v.user}): timeout waiting for ${what}: ${JSON.stringify(st)}`);
}

const pct = (a, p) => { if (!a.length) return null; const s = [...a].sort((x, y) => x - y); return s[Math.max(0, Math.ceil(p * s.length) - 1)]; };
const r1 = x => (x == null ? null : Math.round(x * 10) / 10);

const result = { label, canvas, app, players, spectators, n_target: n, jitter_ms: jitterMs, started_at: new Date().toISOString(), viewers: [], phases: {} };
const viewers = [];
try {
  // Open every viewer, one after another, so each later one is a joiner
  // onto a running encoder. Players join as soon as they are live.
  const roles = [...Array(players).fill('player'), ...Array(spectators).fill('spectator')];
  for (let i = 0; i < roles.length; i++) {
    const v = await newViewer(i, users[i], roles[i]);
    viewers.push(v);
    v.navAt = Date.now();
    await v.goto(`${app}/dev/login?as=${v.user}`);
    await v.goto(`${app}/canvases/${canvas}`);
    const st = await waitFor(v, s => s.state === 'live' && s.frames > 3 && s.hooked > 0, 'live video');
    log(`viewer ${i} ${v.user} ${v.role} live`, JSON.stringify({ frames: st.frames, path: st.path }));
    if (v.role === 'player') {
      await v.click('[data-gs-join]');
      const js = await waitFor(v, s => s.leave && s.player, 'joined a slot');
      v.chips = js.chips;
      await v.click('[data-gs-capture]');
      await waitFor(v, s => s.ring, 'capture ring');
      await v.key('KeyD', 'd', 68);
    } else {
      // A spectator behaves like a person trying to play: it clicks the
      // video and presses keys. None of it may reach a data channel.
      await v.click('[data-gs-capture]').catch(() => {});
      await v.key('KeyD', 'd', 68);
    }
  }
  result.phases.all_open_at = new Date().toISOString();
  log('all viewers open; probing');

  // Probe phase: every player keeps input alive (a key every keyEvery ms,
  // alternating D and A so its square stays near home) until each has n
  // samples.
  const probeStart = Date.now(); let lastKey = Date.now(), flip = false;
  while (true) {
    const sts = [];
    for (const v of viewers) {
      const st = await v.ev(STATE).catch(e => ({ err: String(e) }));
      sts.push(st);
      v.fps.push({ at: Date.now(), fps: st && st.fps, frames: st && st.frames, pcState: st && st.pcState });
    }
    const done = viewers.every((v, k) => v.role !== 'player' || (sts[k] && sts[k].samples >= n));
    if (done) break;
    if ((Date.now() - probeStart) / 1000 > maxS) { log('max-seconds reached before n samples'); result.timed_out = true; break; }
    if (Date.now() - lastKey >= keyEvery) {
      lastKey = Date.now(); flip = !flip;
      for (const v of viewers) await (flip ? v.key('KeyA', 'a', 65) : v.key('KeyD', 'd', 68));
    }
    await sleep(2000);
  }
  result.phases.probe_end_at = new Date().toISOString();

  if (shotOut) {
    const { data } = await viewers[0].s('Page.captureScreenshot', { format: 'jpeg', quality: 80 });
    fs.writeFileSync(shotOut, Buffer.from(data, 'base64'));
  }

  // Freeze the windows: release capture (no more probes), then wait for one
  // more stats push so the session holds exactly the window we read.
  for (const v of viewers) if (v.role === 'player') await v.key('Escape', 'Escape', 27);
  await sleep(6500);

  for (const v of viewers) {
    const raw = await v.ev(`(() => {
      const hooks = window.__s5hooks || []; const h = hooks.find(h => h.pc) || hooks[0]
      const s5 = window.__s5
      return { samples: h && h.__s5 ? h.__s5.samples : [], pending: h ? h.pending.size : null,
        window: h ? { p50: h.rtt.p50(), p95: h.rtt.p95(), count: h.rtt.count } : null,
        lastStats: h && h.__s5 ? h.__s5.lastStats : null, probeSlot: h ? h.probeSlot : null,
        mine: h && h.mine ? Array.from(h.mine.entries()) : null, sends: s5.sends,
        firstFrameAt: s5.firstFrameAt, pcAt: s5.pcAt, navToFirst: s5.firstFrameAt }
    })()`);
    const stats = await v.ev(STATS).catch(e => ({ err: String(e) }));
    const ms = raw.samples.map(x => x.ms);
    const probesSent = raw.sends.filter(x => x.t === 'probe').length;
    const byType = {};
    for (const x of raw.sends) byType[x.t || '?'] = (byType[x.t || '?'] || 0) + 1;
    const fpsVals = v.fps.map(x => x.fps).filter(x => typeof x === 'number');
    const slots = [...new Set(raw.samples.map(x => x.slot))];
    result.viewers.push({
      i: v.i, user: v.user, role: v.role, mobile: v.mobile, chips_after_join: v.chips || null,
      slot: raw.probeSlot, sample_slots: slots, mine: raw.mine,
      n: ms.length, sent: probesSent, pending: raw.pending, lost: probesSent - ms.length - (raw.pending || 0),
      p50_ms: r1(pct(ms, 0.5)), p95_ms: r1(pct(ms, 0.95)), min_ms: r1(Math.min(...ms)), max_ms: r1(Math.max(...ms)),
      window: raw.window, last_stats_push: raw.lastStats,
      ttff_ms: raw.firstFrameAt != null && raw.pcAt != null ? r1(raw.firstFrameAt - raw.pcAt) : null,
      nav_to_first_frame_ms: r1(raw.firstFrameAt),
      fps_median: pct(fpsVals, 0.5), fps_min: fpsVals.length ? Math.min(...fpsVals) : null,
      path: (v.fps.length && (await v.ev(STATE)).path) || null,
      inbound: stats,
      sends_total: raw.sends.length, sends_by_type: byType,
      samples: raw.samples, fps_series: v.fps,
    });
  }
  if (forgeSpectator) {
    result.forged = [];
    for (const v of viewers.filter(v => v.role === 'spectator')) {
      const r = await v.ev(`(() => {
        const ch = (window.__s5.channels || []).filter(c => c.label === 'input-events' && c.readyState === 'open').pop()
        if (!ch) return { sent: false, channels: (window.__s5.channels || []).map(c => c.label + ':' + c.readyState) }
        ch.send(JSON.stringify({ t: 'key', code: 'KeyD', down: true, slot: 0 }))
        ch.send(JSON.stringify({ t: 'key', code: 'KeyD', down: false, slot: 0 }))
        return { sent: true, at: new Date().toISOString() }
      })()`);
      result.forged.push({ user: v.user, ...r });
    }
    await sleep(11000); // the host logs drops at most once per 10 s per peer
  }
  result.ended_at = new Date().toISOString();
  if (out) fs.writeFileSync(out, JSON.stringify(result, null, 1));
  const brief = result.viewers.map(({ samples, fps_series, inbound, ...b }) => ({ ...b, frames: inbound && inbound.framesDecoded, dropped: inbound && inbound.framesDropped }));
  console.log(JSON.stringify({ label, timed_out: !!result.timed_out, viewers: brief }, null, 1));
} finally {
  for (const c of contexts) await send('Target.disposeBrowserContext', { browserContextId: c }).catch(() => {});
  ws.close();
}
