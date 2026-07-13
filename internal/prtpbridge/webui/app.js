(() => {
  const $ = (id) => document.getElementById(id);
  const uiDebug = new URLSearchParams(window.location.search).has('debug-ui');
  const audioTabStorageKey = 'prtp-audio-tab';
  const testSourceStorageKey = 'prtp-test-audio-source';
  const browserAudioInputStorageKey = 'prtp-browser-audio-input';
  const browserAudioOutputStorageKey = 'prtp-browser-audio-output';
  const maxLogChars = 20000;
  const appendLogLine = (parts) => {
    const e = $('log');
    if (!e) return;
    e.insertAdjacentText('beforeend', parts.join(' ') + "\n");
    if (e.textContent.length > maxLogChars) {
      e.textContent = e.textContent.slice(-maxLogChars);
    }
    e.scrollTop = e.scrollHeight;
  };
  const log = (...a) => {
    if (uiDebug) console.log(...a);
    appendLogLine(a);
  };
  const trace = (...a) => {
    if (!uiDebug) return;
    console.debug(...a);
    appendLogLine(a);
  };
  if (uiDebug) {
    document.querySelectorAll('[data-debug-ui]').forEach((el) => {
      el.hidden = false;
    });
  }

  let ws = null;
  let audioWs = null;
  let ctx = null;
  let rxEnabled = false;
  let txEnabled = false;
  let txStarting = false;
  let micStream = null;
  let micSource = null;
  let micProcessor = null;
  let micWorkletLoaded = false;
  let manualDisconnect = false;
  let reconnectTimer = null;
  let serverTxActive = false;
  let activeTxMode = '';
  let txRunID = 0;
  let txTimer = null;
  let micMuted = false;
  let micEnableNeeded = true;
  let micEnableReasonText = 'Microphone has not been enabled yet.';
  let speakerMuted = false;
  let autoAudioStartAttempted = false;
  let autoAudioStartInProgress = false;
  let audioSettingsDirty = false;
  let appliedAudioSettingsKey = '';
  let serverAudio = {
    supported: false,
    backend: 'unknown',
    capture: [],
    playback: [],
    selected: { capture: 'default', playback: 'default' },
    enabled: { capture: false, playback: false },
    running: { capture: false, playback: false },
    tx_source: 'ws',
  };
  let browserAudio = {
    supported: false,
    input: [],
    output: [],
    selected: {
      input: storageGet(browserAudioInputStorageKey, 'default'),
      output: storageGet(browserAudioOutputStorageKey, 'default'),
    },
  };
  let rxOutputGain = null;
  let browserOutputStreamDest = null;
  let browserOutputElement = null;
  let browserOutputRoute = '';
  let floatingWin = null;
  let floatingKeyCount = -1;

  // RX jitter buffer
  let rxQueue = [];
  let remoteCfg = { sample_rate: 8333, channels: 1, frame_samples: 256 };
  const autoMatrixNamesRetryMs = 30000;
  let autoMatrixNamesFetched = false;
  let autoMatrixNamesTimer = null;
  let matrixNamesReceived = false;
  let lastMatrixNamesAddr = '';
  let lastMatrixNamesAttemptAt = 0;
  let scheduleTimer = null;
  let playHead = 0;
  const scheduledRxSources = new Set();
  let rxWorkletLoaded = false;
  let rxWorkletNode = null;
  let rxResampler = null;
  const playbackStartDelaySec = 0.08;
  const rxAdaptiveMinSec = 0.10;
  const rxAdaptiveInitialSec = 0.24;
  const rxAdaptiveMaxSec = 0.85;
  const rxAdaptiveHardMaxSec = 1.20;
  const scheduleIntervalMs = 5;
  const rxAdaptive = {
    targetSec: rxAdaptiveInitialSec,
    lastArrivalMs: 0,
    frameSec: 256 / 8333,
    lateEWMA: 0,
    peakLateSec: 0,
    underrunBoostSec: 0,
    lastPostedTargetSec: 0,
    bufferedSec: 0,
    underruns: 0,
    droppedFrames: 0,
    mode: 'scheduler',
  };
  const vuFloorDb = -60;
  let vuAnim = null;
  const vu = {
    rx: { level: 0, target: 0, peak: 0, db: vuFloorDb, updatedAt: 0 },
    tx: { level: 0, target: 0, peak: 0, db: vuFloorDb, updatedAt: 0 },
  };

  // PRTP stream state (control frames)
  const prtp = {
    esc: false,
    acc: [],
  };

  // panel model (limited by the emulated device profile)
  const profileKeyCounts = {
    TP5008: 8,
    TP5012: 12,
    TP5024: 24,
    BP7100: 4,
  };
  const activeKeyPointers = new Map();
  const monostableHoldMs = 300;
  let profileModel = '';
  let panelTarget = { port: '', name: '' };

  const panel = {
    n: 24,
    pressed: new Array(24).fill(false),
    label: new Array(24).fill(''),
    gFix: new Array(24).fill(false),
    rFix: new Array(24).fill(false),
    gBlink: new Array(24).fill(false),
    rBlink: new Array(24).fill(false),
  };

  function defaultWsUrl(path = '/control') {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    if (window.location.host) {
      return `${scheme}://${window.location.host}${path}`;
    }
    let host = window.location.hostname || 'localhost';
    if (host.includes(':') && !host.startsWith('[')) host = `[${host}]`;
    return `${scheme}://${host}:8090${path}`;
  }

  function defaultControlWsUrl() {
    return defaultWsUrl('/control');
  }

  function defaultAudioWsUrl() {
    return defaultWsUrl('/audio-stream');
  }

  function setConnectionState(text, kind) {
    const el = $('connState');
    if (!el) return;
    el.textContent = text;
    el.classList.toggle('ok', kind === 'ok');
    el.classList.toggle('warn', kind === 'warn');
    updateFloatingHeader();
    updateAudioControls();
  }

  function storageGet(key, fallback = '') {
    try {
      return window.localStorage.getItem(key) || fallback;
    } catch {
      return fallback;
    }
  }

  function storageSet(key, value) {
    try {
      window.localStorage.setItem(key, value || 'default');
    } catch {}
  }

  function wsIsOpen() {
    return !!(ws && ws.readyState === WebSocket.OPEN);
  }

  function audioWsIsOpen() {
    return !!(audioWs && audioWs.readyState === WebSocket.OPEN);
  }

  function sendControlJSON(obj) {
    if (!wsIsOpen()) return false;
    ws.send(JSON.stringify(obj));
    return true;
  }

  function sendAudioBytes(data) {
    if (!audioWsIsOpen()) return false;
    audioWs.send(data);
    return true;
  }

  function updateAudioControls() {
    const connected = wsIsOpen();
    const audioConnected = audioWsIsOpen();
    const canLocalLoopback = frontendLoopbackEnabled();
    const mode = selectedSourceMode();
    const startRx = $('btnStartRx');
    const stopRx = $('btnStopRx');
    const startTx = $('btnStartTx');
    const stopTx = $('btnStopTx');
    if (startRx) startRx.disabled = !audioConnected || rxEnabled;
    if (stopRx) stopRx.disabled = !rxEnabled;
    if (startTx) {
      const serverBlocked = mode === 'server' && (!connected || !serverAudio.supported);
      const micBlocked = mode === 'mic' && !canUseMicTx();
      const browserBlocked = mode !== 'server' && ((!connected || !audioConnected) && !canLocalLoopback);
      startTx.disabled = serverBlocked || micBlocked || browserBlocked || txEnabled || txStarting;
    }
    if (stopTx) stopTx.disabled = !txEnabled && !txStarting;
    updateAudioTabControls();
    updateBrowserAudioControls();
    updateServerAudioControls();
    updateMuteButtons();
    updateAudioApplyButton();
  }

  function frontendLoopbackEnabled() {
    return !!$('frontendLoopback')?.checked;
  }

  function selectedAudioTab() {
    const selected = document.querySelector('.audio-tab[aria-selected="true"]')?.dataset.audioTab || storageGet(audioTabStorageKey, 'live');
    return ['live', 'test', 'server'].includes(selected) ? selected : 'live';
  }

  function selectedTestSourceMode() {
    const selected = $('testSourceMode')?.value || storageGet(testSourceStorageKey, 'tone');
    return ['tone', 'sweep', 'silence'].includes(selected) ? selected : 'tone';
  }

  function updateTestAudioControls({ persist = false } = {}) {
    const mode = selectedTestSourceMode();
    if (persist) storageSet(testSourceStorageKey, mode);
    document.querySelectorAll('[data-test-audio]').forEach((el) => {
      el.hidden = el.dataset.testAudio !== mode;
    });
  }

  function setAudioTab(tab, { persist = true } = {}) {
    const next = ['live', 'test', 'server'].includes(tab) ? tab : 'live';
    document.querySelectorAll('[data-audio-tab]').forEach((button) => {
      const selected = button.dataset.audioTab === next;
      button.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    document.querySelectorAll('.audio-tab-panel').forEach((panelEl) => {
      const panelName = panelEl.id === 'audioLivePanel' ? 'live' : panelEl.id === 'audioTestPanel' ? 'test' : 'server';
      panelEl.hidden = panelName !== next;
    });
    if (persist) storageSet(audioTabStorageKey, next);
    updateTestAudioControls();
    updateAudioControls();
  }

  function updateAudioTabControls() {
    document.querySelectorAll('[data-audio-tab]').forEach((button) => {
      button.disabled = txStarting;
    });
  }

  function audioSettingsKey() {
    return [
      selectedAudioTab(),
      selectedTestSourceMode(),
      selectedBrowserInputID(),
      selectedBrowserOutputID(),
      $('serverCaptureDevice')?.value || serverAudio.selected?.capture || 'default',
      $('serverPlaybackDevice')?.value || serverAudio.selected?.playback || 'default',
      $('serverPlaybackEnable')?.checked ? 'playback-on' : 'playback-off',
    ].join('|');
  }

  function updateAudioApplyButton() {
    const button = $('btnApplyAudio');
    const status = $('audioModeStatus');
    if (button) button.disabled = txStarting || !audioSettingsDirty;
    if (status) {
      status.textContent = audioSettingsDirty ? 'Pending' : 'Applied';
      status.classList.toggle('pending', audioSettingsDirty);
    }
    updateFloatingAudioState();
  }

  function syncAppliedAudioSettings() {
    appliedAudioSettingsKey = audioSettingsKey();
    audioSettingsDirty = false;
    updateAudioApplyButton();
  }

  function markAudioSettingsDirty() {
    audioSettingsDirty = appliedAudioSettingsKey !== audioSettingsKey();
    updateAudioApplyButton();
  }

  function commitSelectedAudioSettings() {
    storageSet(audioTabStorageKey, selectedAudioTab());
    storageSet(testSourceStorageKey, selectedTestSourceMode());
    browserAudio.selected.input = selectedBrowserInputID();
    browserAudio.selected.output = selectedBrowserOutputID();
    storageSet(browserAudioInputStorageKey, browserAudio.selected.input);
    storageSet(browserAudioOutputStorageKey, browserAudio.selected.output);
    serverAudio = {
      ...serverAudio,
      selected: {
        ...serverAudio.selected,
        capture: $('serverCaptureDevice')?.value || serverAudio.selected?.capture || 'default',
        playback: $('serverPlaybackDevice')?.value || serverAudio.selected?.playback || 'default',
      },
      enabled: {
        ...serverAudio.enabled,
        playback: !!$('serverPlaybackEnable')?.checked,
      },
    };
    syncAppliedAudioSettings();
  }

  function setMicMuted(muted) {
    if (micEnableNeeded) return;
    micMuted = !!muted;
    if (micMuted) resetVu('tx');
    updateMuteButtons();
  }

  function setMicEnableNeeded(needed, reason = '') {
    micEnableNeeded = !!needed;
    micEnableReasonText = reason || (micEnableNeeded ? 'Microphone has not been enabled yet.' : '');
    updateMuteButtons();
  }

  function setSpeakerMuted(muted) {
    speakerMuted = !!muted;
    applySpeakerMute();
    updateMuteButtons();
  }

  function updateMuteButtons() {
    const mic = $('btnMicMute');
    const speaker = $('btnSpeakerMute');
    if (mic) {
      const disabledState = micEnableNeeded || micMuted;
      mic.setAttribute('aria-pressed', disabledState ? 'true' : 'false');
      const label = micEnableNeeded ? 'Enable microphone' : micMuted ? 'Microphone muted' : 'Mute microphone';
      mic.setAttribute('aria-label', label);
      mic.title = micEnableNeeded && micEnableReasonText ? `${label}: ${micEnableReasonText}` : label;
      mic.classList.toggle('is-muted', disabledState);
      mic.classList.toggle('needs-enable', micEnableNeeded);
      const text = mic.querySelector('.sr-only');
      if (text) text.textContent = label;
    }
    if (speaker) {
      speaker.setAttribute('aria-pressed', speakerMuted ? 'true' : 'false');
      const label = speakerMuted ? 'Headphones muted' : 'Mute headphones';
      speaker.setAttribute('aria-label', label);
      speaker.title = label;
      speaker.classList.toggle('is-muted', speakerMuted);
      const text = speaker.querySelector('.sr-only');
      if (text) text.textContent = label;
    }
    updateFloatingAudioState();
  }

  function showAudioEnableOverlay(reason = '') {
    const overlay = $('audioEnableOverlay');
    const text = $('audioEnableReason');
    if (text) {
      text.textContent = reason || 'Your browser needs one click before microphone and headphone audio can start.';
    }
    if (overlay) overlay.hidden = false;
  }

  function hideAudioEnableOverlay() {
    const overlay = $('audioEnableOverlay');
    if (overlay) overlay.hidden = true;
  }

  function floatingDocument() {
    if (!floatingWin || floatingWin.closed) {
      floatingWin = null;
      floatingKeyCount = -1;
      updateFloatingButton();
      return null;
    }
    return floatingWin.document;
  }

  function updateFloatingButton() {
    const button = $('btnFloatingPanel');
    if (!button) return;
    const open = !!(floatingWin && !floatingWin.closed);
    button.classList.toggle('is-active', open);
    button.setAttribute('aria-pressed', open ? 'true' : 'false');
  }

  function floatingPanelHTML() {
    return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kroma Floating Panel</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #16161d;
      --panel: #20212a;
      --panel-2: #282934;
      --line: #3c3e4d;
      --ink: #f5f7fb;
      --muted: #a8adbc;
      --accent: #8b5cf6;
      --accent-2: #35d0ba;
      --green: #34d399;
      --red: #fb7185;
      --warn: #f59e0b;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-width: 300px;
      min-height: 100vh;
      background: var(--bg);
      color: var(--ink);
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      line-height: 1.25;
      overflow: hidden;
    }
    button {
      font: inherit;
      color: inherit;
      border: 1px solid var(--line);
      background: var(--panel-2);
      border-radius: 8px;
      cursor: pointer;
    }
    button:disabled {
      cursor: not-allowed;
      opacity: .45;
    }
    .pip-shell {
      height: 100vh;
      display: grid;
      grid-template-rows: auto 1fr auto;
      gap: 8px;
      padding: 10px;
    }
    .pip-top {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      align-items: start;
      gap: 8px;
      padding: 10px;
      border: 1px solid var(--line);
      border-radius: 8px;
      background: linear-gradient(180deg, #252634, #1f2029);
    }
    .pip-title {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      font-size: 1rem;
      font-weight: 800;
      letter-spacing: 0;
    }
    .pip-sub {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      margin-top: 6px;
      color: var(--muted);
      font-size: .76rem;
    }
    .pill {
      display: inline-flex;
      align-items: center;
      min-height: 23px;
      padding: 3px 7px;
      border: 1px solid var(--line);
      border-radius: 999px;
      background: rgba(255,255,255,.04);
      color: var(--muted);
      white-space: nowrap;
      font-variant-numeric: tabular-nums;
    }
    .pill.ok {
      color: #c7f9df;
      border-color: rgba(52, 211, 153, .55);
      background: rgba(52, 211, 153, .12);
    }
    .pill.warn {
      color: #fde68a;
      border-color: rgba(245, 158, 11, .55);
      background: rgba(245, 158, 11, .12);
    }
    .pip-close {
      width: 32px;
      height: 32px;
      display: grid;
      place-items: center;
      padding: 0;
      color: var(--muted);
    }
    .pip-keys {
      min-height: 0;
      overflow: auto;
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(92px, 1fr));
      align-content: start;
      gap: 8px;
      padding: 2px;
    }
    .pip-key {
      min-height: 78px;
      display: grid;
      align-content: space-between;
      position: relative;
      padding: 9px;
      text-align: left;
      user-select: none;
      touch-action: none;
      background: linear-gradient(180deg, #2c2d39, #23242e);
      border-color: #464857;
      box-shadow: inset 0 1px 0 rgba(255,255,255,.04);
    }
    .pip-key:hover { border-color: #5f6272; }
    .pip-key.pressed,
    .pip-key.local-press {
      border-color: rgba(139, 92, 246, .95);
      background: linear-gradient(180deg, rgba(139, 92, 246, .28), rgba(73, 54, 136, .34));
      box-shadow: inset 0 0 0 1px rgba(139, 92, 246, .32), 0 0 0 1px rgba(139, 92, 246, .16);
    }
    .pip-key-index {
      color: var(--ink);
      font-weight: 900;
      font-size: .95rem;
    }
    .pip-key-label {
      min-height: 1.1em;
      overflow: hidden;
      color: var(--muted);
      font-size: .78rem;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .pip-leds {
      position: absolute;
      right: 8px;
      top: 8px;
      display: flex;
      gap: 5px;
    }
    .pip-led {
      width: 10px;
      height: 10px;
      border-radius: 50%;
      opacity: .22;
      border: 1px solid rgba(255,255,255,.28);
      box-shadow: inset 0 0 0 1px rgba(0,0,0,.24);
    }
    .pip-led.green { color: var(--green); background: var(--green); }
    .pip-led.red { color: var(--red); background: var(--red); }
    .pip-led[data-state="on"],
    .pip-led[data-state="blink"] {
      opacity: 1;
      box-shadow: 0 0 8px currentColor, inset 0 0 0 1px rgba(255,255,255,.38);
    }
    .pip-led[data-state="blink"] { animation: blink 1s steps(1, end) infinite; }
    @keyframes blink { 50% { opacity: .22 } }
    .pip-bottom {
      display: grid;
      gap: 8px;
      padding: 10px;
      border: 1px solid var(--line);
      border-radius: 8px;
      background: var(--panel);
    }
    .pip-vu-grid {
      display: grid;
      gap: 7px;
    }
    .pip-vu {
      display: grid;
      grid-template-columns: 24px minmax(94px, 1fr) 40px;
      align-items: center;
      gap: 7px;
      font-size: .76rem;
      color: var(--muted);
      font-variant-numeric: tabular-nums;
    }
    .pip-vu strong {
      color: var(--ink);
      font-size: .74rem;
    }
    .pip-vu-track {
      height: 12px;
      overflow: hidden;
      position: relative;
      border: 1px solid #4a4d5b;
      border-radius: 999px;
      background: #11131a;
    }
    .pip-vu-fill {
      width: 0%;
      height: 100%;
      border-radius: inherit;
      background: linear-gradient(90deg, var(--accent-2) 0%, var(--green) 62%, var(--warn) 82%, var(--red) 100%);
      transition: width 60ms linear;
    }
    .pip-vu-peak {
      position: absolute;
      top: 1px;
      bottom: 1px;
      left: 0%;
      width: 2px;
      border-radius: 2px;
      background: #f8fafc;
      opacity: .78;
      transform: translateX(-1px);
    }
    .pip-actions {
      display: grid;
      grid-template-columns: 1fr 1fr auto auto;
      gap: 7px;
      align-items: center;
    }
    .pip-action {
      min-height: 38px;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 6px;
      padding: 7px 9px;
      font-weight: 800;
    }
    .pip-action svg {
      width: 17px;
      height: 17px;
      fill: none;
      stroke: currentColor;
      stroke-width: 2;
      stroke-linecap: round;
      stroke-linejoin: round;
    }
    .pip-action.active {
      color: #f5f3ff;
      border-color: rgba(139, 92, 246, .9);
      background: rgba(139, 92, 246, .30);
    }
    .pip-action.muted,
    .pip-action.needs-enable {
      color: #fecaca;
      border-color: rgba(251, 113, 133, .72);
      background: rgba(251, 113, 133, .14);
    }
    .pip-apply {
      min-width: 66px;
      height: 38px;
      padding: 0 10px;
      font-weight: 800;
      color: #f5f3ff;
      border-color: rgba(139, 92, 246, .72);
      background: rgba(139, 92, 246, .24);
    }
    .pip-state {
      color: var(--muted);
      min-width: 56px;
      text-align: right;
      font-size: .76rem;
      font-weight: 800;
    }
    .pip-state.pending { color: #fde68a; }
    @media (max-width: 360px) {
      .pip-shell { padding: 8px; }
      .pip-keys { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .pip-actions { grid-template-columns: 1fr 1fr; }
      .pip-state { text-align: left; }
    }
  </style>
</head>
<body>
  <div class="pip-shell">
    <header class="pip-top">
      <div>
        <div id="pipTitle" class="pip-title">Panel</div>
        <div class="pip-sub">
          <span id="pipConn" class="pill warn">Connecting</span>
          <span id="pipProfile" class="pill">Panel</span>
          <span id="pipRtt" class="pill">RTT 0 ms</span>
        </div>
      </div>
      <button id="pipClose" class="pip-close" type="button" aria-label="Close floating panel" title="Close floating panel">&times;</button>
    </header>
    <main id="pipKeys" class="pip-keys"></main>
    <footer class="pip-bottom">
      <div class="pip-vu-grid">
        <div class="pip-vu"><strong>RX</strong><div class="pip-vu-track"><div id="pipRxFill" class="pip-vu-fill"></div><span id="pipRxPeak" class="pip-vu-peak"></span></div><span id="pipRxDb">-inf</span></div>
        <div class="pip-vu"><strong>TX</strong><div class="pip-vu-track"><div id="pipTxFill" class="pip-vu-fill"></div><span id="pipTxPeak" class="pip-vu-peak"></span></div><span id="pipTxDb">-inf</span></div>
      </div>
      <div class="pip-actions">
        <button id="pipMic" class="pip-action" type="button" aria-label="Mute microphone" title="Mute microphone">
          <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path><path d="M12 19v3"></path><path d="M8 22h8"></path></svg>
          <span>MIC</span>
        </button>
        <button id="pipSpeaker" class="pip-action" type="button" aria-label="Mute headphones" title="Mute headphones">
          <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M3 14v-2a9 9 0 0 1 18 0v2"></path><path d="M5 14h3v7H5a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2z"></path><path d="M16 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2h-3v-7z"></path></svg>
          <span>SPK</span>
        </button>
        <button id="pipApply" class="pip-apply" type="button">Apply</button>
        <span id="pipAudioState" class="pip-state">Ready</span>
      </div>
    </footer>
  </div>
</body>
</html>`;
  }

  function buildFloatingPanel(doc) {
    doc.open();
    doc.write(floatingPanelHTML());
    doc.close();
    floatingKeyCount = -1;
    doc.getElementById('pipClose')?.addEventListener('click', () => {
      if (floatingWin && !floatingWin.closed) floatingWin.close();
    });
    doc.getElementById('pipMic')?.addEventListener('click', handleMicButtonClick);
    doc.getElementById('pipSpeaker')?.addEventListener('click', () => setSpeakerMuted(!speakerMuted));
    doc.getElementById('pipApply')?.addEventListener('click', () => applyAudioSettings());
    renderFloatingPanel(true);
    updateFloatingHeader();
    updateFloatingStats();
    updateFloatingAudioState();
    updateFloatingVuMeter('rx', vu.rx);
    updateFloatingVuMeter('tx', vu.tx);
  }

  async function openFloatingPanel() {
    if (floatingWin && !floatingWin.closed) {
      floatingWin.focus();
      return;
    }
    try {
      if (window.documentPictureInPicture && typeof window.documentPictureInPicture.requestWindow === 'function') {
        floatingWin = await window.documentPictureInPicture.requestWindow({ width: 420, height: 660 });
      } else {
        floatingWin = window.open('', 'prtp-floating-panel', 'popup,width=420,height=660');
      }
      if (!floatingWin) {
        log('Floating panel blocked by browser');
        return;
      }
      floatingWin.addEventListener('pagehide', () => {
        floatingWin = null;
        floatingKeyCount = -1;
        updateFloatingButton();
      }, { once: true });
      buildFloatingPanel(floatingWin.document);
      updateFloatingButton();
      floatingWin.focus();
    } catch (err) {
      floatingWin = null;
      updateFloatingButton();
      log('Floating panel unavailable:', err?.message || err);
    }
  }

  function updateFloatingHeader() {
    const doc = floatingDocument();
    if (!doc) return;
    const title = doc.getElementById('pipTitle');
    const conn = doc.getElementById('pipConn');
    const profile = doc.getElementById('pipProfile');
    if (title) title.textContent = panelTargetTitle() || `${profileModel || 'Panel'} Panel`;
    if (conn) {
      const source = $('connState');
      conn.textContent = source?.textContent || 'Disconnected';
      conn.classList.toggle('ok', source?.classList.contains('ok'));
      conn.classList.toggle('warn', !source?.classList.contains('ok'));
    }
    if (profile) profile.textContent = `${profileModel || 'Panel'} / ${panel.n} ${panel.n === 1 ? 'key' : 'keys'}`;
    updateFloatingStats();
  }

  function updateFloatingStats() {
    const doc = floatingDocument();
    if (!doc) return;
    const rtt = doc.getElementById('pipRtt');
    if (rtt) rtt.textContent = `RTT ${$('statRTT')?.textContent || '0'} ms`;
  }

  function renderFloatingPanel(force = false) {
    const doc = floatingDocument();
    if (!doc) return;
    updateFloatingHeader();
    const root = doc.getElementById('pipKeys');
    if (!root) return;
    if (force || floatingKeyCount !== panel.n || root.children.length !== panel.n) {
      root.textContent = '';
      for (let i = 0; i < panel.n; i++) {
        const button = doc.createElement('button');
        button.type = 'button';
        button.className = 'pip-key';
        button.dataset.idx = i;
        button.title = `Key ${i}: tap to toggle, hold for momentary`;
        button.innerHTML = '<div class="pip-leds"><span class="pip-led green" data-led="green" data-state="off"></span><span class="pip-led red" data-led="red" data-state="off"></span></div><div class="pip-key-index"></div><div class="pip-key-label"></div>';
        button.addEventListener('pointerdown', onKeyPointerDown);
        button.addEventListener('pointerup', onKeyPointerRelease);
        button.addEventListener('pointercancel', onKeyPointerRelease);
        button.addEventListener('contextmenu', (ev) => ev.preventDefault());
        root.appendChild(button);
      }
      floatingKeyCount = panel.n;
    }
    const children = root.children;
    for (let i = 0; i < panel.n && i < children.length; i++) {
      const button = children[i];
      button.classList.toggle('pressed', !!panel.pressed[i]);
      const idx = button.querySelector('.pip-key-index');
      const label = button.querySelector('.pip-key-label');
      if (idx) idx.textContent = defaultPanelLabel(i);
      if (label) label.textContent = panel.label[i] || '';
      setLedState(button.querySelector('.pip-led.green'), panel.gBlink[i] ? 'blink' : panel.gFix[i] ? 'on' : 'off');
      setLedState(button.querySelector('.pip-led.red'), panel.rBlink[i] ? 'blink' : panel.rFix[i] ? 'on' : 'off');
    }
  }

  function updateFloatingVuMeter(route, state) {
    const doc = floatingDocument();
    if (!doc || !state) return;
    const prefix = route === 'rx' ? 'pipRx' : 'pipTx';
    const fill = doc.getElementById(`${prefix}Fill`);
    const peak = doc.getElementById(`${prefix}Peak`);
    const db = doc.getElementById(`${prefix}Db`);
    const percent = Math.round(clamp01(state.level) * 1000) / 10;
    if (fill) fill.style.width = `${percent}%`;
    if (peak) peak.style.left = `${Math.round(clamp01(state.peak) * 1000) / 10}%`;
    const displayDb = state.level > 0 ? vuFloorDb + (clamp01(state.level) * -vuFloorDb) : vuFloorDb;
    if (db) db.textContent = displayDb <= vuFloorDb ? '-inf' : `${Math.round(displayDb)} dB`;
  }

  function updateFloatingAudioState() {
    const doc = floatingDocument();
    if (!doc) return;
    const mic = doc.getElementById('pipMic');
    const speaker = doc.getElementById('pipSpeaker');
    const apply = doc.getElementById('pipApply');
    const state = doc.getElementById('pipAudioState');
    if (mic) {
      const needsEnable = micEnableNeeded || !(txEnabled && activeTxMode === 'mic');
      mic.classList.toggle('active', txEnabled && activeTxMode === 'mic' && !micMuted && !micEnableNeeded);
      mic.classList.toggle('muted', micMuted && !micEnableNeeded);
      mic.classList.toggle('needs-enable', needsEnable);
      const label = micEnableNeeded ? 'Enable microphone' : micMuted ? 'Microphone muted' : 'Mute microphone';
      mic.setAttribute('aria-label', label);
      mic.title = micEnableNeeded && micEnableReasonText ? `${label}: ${micEnableReasonText}` : label;
    }
    if (speaker) {
      speaker.classList.toggle('muted', speakerMuted);
      const label = speakerMuted ? 'Headphones muted' : 'Mute headphones';
      speaker.setAttribute('aria-label', label);
      speaker.title = label;
    }
    if (apply) apply.disabled = txStarting || !audioSettingsDirty;
    if (state) {
      state.textContent = audioSettingsDirty ? 'Pending' : txEnabled ? activeTxMode.toUpperCase() || 'Live' : 'Ready';
      state.classList.toggle('pending', audioSettingsDirty);
    }
  }

  function clamp01(v) {
    return Math.max(0, Math.min(1, v));
  }

  function levelToDb(level) {
    return level > 0.000001 ? Math.max(vuFloorDb, Math.min(0, 20 * Math.log10(level))) : vuFloorDb;
  }

  function dbToMeter(db) {
    return clamp01((db - vuFloorDb) / -vuFloorDb);
  }

  function sampleStats(samples) {
    if (!samples || !samples.length) return { rms: 0, peak: 0 };
    let sum = 0;
    let peak = 0;
    const scale = samples instanceof Int16Array ? 32768 : 1;
    for (let i = 0; i < samples.length; i++) {
      const v = Math.abs(samples[i] / scale);
      peak = Math.max(peak, v);
      sum += v * v;
    }
    return { rms: Math.sqrt(sum / samples.length), peak };
  }

  function queueVu(route, samples) {
    const state = vu[route];
    if (!state) return;
    const stats = sampleStats(samples);
    state.db = levelToDb(stats.rms);
    state.target = dbToMeter(state.db);
    state.peak = Math.max(state.peak, dbToMeter(levelToDb(stats.peak)));
    state.updatedAt = performance.now();
    if (!vuAnim) vuAnim = requestAnimationFrame(renderVuMeters);
  }

  function resetVu(route) {
    const routes = route ? [route] : Object.keys(vu);
    for (const name of routes) {
      const state = vu[name];
      state.level = 0;
      state.target = 0;
      state.peak = 0;
      state.db = vuFloorDb;
      state.updatedAt = performance.now();
      renderVuMeter(name, state);
    }
  }

  function renderVuMeter(route, state) {
    const prefix = route === 'rx' ? 'vuRx' : 'vuTx';
    const track = $(prefix);
    const fill = $(`${prefix}Fill`);
    const peak = $(`${prefix}Peak`);
    const db = $(`${prefix}Db`);
    const percent = Math.round(clamp01(state.level) * 1000) / 10;
    if (fill) fill.style.width = `${percent}%`;
    if (peak) peak.style.left = `${Math.round(clamp01(state.peak) * 1000) / 10}%`;
    const displayDb = state.level > 0 ? vuFloorDb + (clamp01(state.level) * -vuFloorDb) : vuFloorDb;
    const shownDb = displayDb <= vuFloorDb ? '-inf' : `${Math.round(displayDb)} dB`;
    if (db) db.textContent = shownDb;
    if (track) track.setAttribute('aria-valuenow', String(Math.round(displayDb)));
    updateFloatingVuMeter(route, state);
  }

  function renderVuMeters(now) {
    let active = false;
    for (const [route, state] of Object.entries(vu)) {
      const recent = now - state.updatedAt < 180;
      const target = recent ? state.target : 0;
      state.level += (target - state.level) * (target > state.level ? 0.5 : 0.16);
      state.peak = Math.max(state.level, state.peak - 0.008);
      if (!recent && state.level < 0.004) {
        state.level = 0;
        state.target = 0;
        state.db = vuFloorDb;
      }
      if (state.level > 0 || state.target > 0 || state.peak > 0) active = true;
      renderVuMeter(route, state);
    }
    vuAnim = active ? requestAnimationFrame(renderVuMeters) : null;
  }

  function rxQueuedSeconds() {
    let total = 0;
    for (const item of rxQueue) {
      const rate = item.sampleRate || remoteCfg.sample_rate || 16000;
      const samples = item.f32 || item.pcm;
      total += samples.length / rate;
    }
    return total;
  }

  function clampValue(v, lo, hi) {
    return Math.max(lo, Math.min(hi, v));
  }

  function rxAudioLatencyFloorSec() {
    const base = ctx && Number.isFinite(ctx.baseLatency) ? ctx.baseLatency : 0;
    const output = ctx && Number.isFinite(ctx.outputLatency) ? ctx.outputLatency : 0;
    return clampValue(base + output + 0.06, rxAdaptiveMinSec, 0.20);
  }

  function resetRxAdaptive() {
    rxAdaptive.targetSec = rxAdaptiveInitialSec;
    rxAdaptive.lastArrivalMs = 0;
    rxAdaptive.frameSec = (remoteCfg.frame_samples || 256) / (remoteCfg.sample_rate || 8333);
    rxAdaptive.lateEWMA = 0;
    rxAdaptive.peakLateSec = 0;
    rxAdaptive.underrunBoostSec = 0;
    rxAdaptive.lastPostedTargetSec = 0;
    rxAdaptive.bufferedSec = 0;
    rxAdaptive.underruns = 0;
    rxAdaptive.droppedFrames = 0;
  }

  function observeRxArrival(sampleCount, sampleRate) {
    const now = performance.now();
    const rate = sampleRate || remoteCfg.sample_rate || 16000;
    const frameSec = sampleCount > 0 && rate > 0 ? sampleCount / rate : rxAdaptive.frameSec;
    rxAdaptive.frameSec = frameSec;
    if (rxAdaptive.lastArrivalMs > 0) {
      const intervalSec = Math.max(0, (now - rxAdaptive.lastArrivalMs) / 1000);
      const lateSec = Math.max(0, intervalSec - frameSec * 1.2);
      rxAdaptive.lateEWMA = rxAdaptive.lateEWMA * 0.88 + lateSec * 0.12;
      rxAdaptive.peakLateSec = Math.max(lateSec, rxAdaptive.peakLateSec * 0.985);
    }
    rxAdaptive.lastArrivalMs = now;
    rxAdaptive.underrunBoostSec *= 0.997;

    const floor = rxAudioLatencyFloorSec();
    const desired = clampValue(
      floor + rxAdaptive.lateEWMA * 3.5 + rxAdaptive.peakLateSec * 1.4 + rxAdaptive.underrunBoostSec,
      floor,
      rxAdaptiveMaxSec
    );
    const alpha = desired > rxAdaptive.targetSec ? 0.35 : 0.015;
    rxAdaptive.targetSec += (desired - rxAdaptive.targetSec) * alpha;
  }

  function postRxWorkletConfig(force = false) {
    if (!rxWorkletNode || !ctx) return;
    const targetFrames = Math.max(128, Math.round(rxAdaptive.targetSec * ctx.sampleRate));
    const maxFrames = Math.max(
      targetFrames * 2,
      Math.round(Math.min(rxAdaptiveHardMaxSec, Math.max(rxAdaptive.targetSec + 0.35, rxAdaptive.targetSec * 2.4)) * ctx.sampleRate)
    );
    if (!force && Math.abs(rxAdaptive.targetSec - rxAdaptive.lastPostedTargetSec) < 0.015) return;
    rxAdaptive.lastPostedTargetSec = rxAdaptive.targetSec;
    rxWorkletNode.port.postMessage({ type: 'config', targetFrames, maxFrames });
  }

  function handleRxWorkletMessage(ev) {
    const msg = ev.data || {};
    if (msg.type === 'underrun') {
      rxAdaptive.underruns += 1;
      rxAdaptive.underrunBoostSec = clampValue(rxAdaptive.underrunBoostSec + 0.08, 0, 0.35);
      rxAdaptive.targetSec = clampValue(rxAdaptive.targetSec + 0.06, rxAudioLatencyFloorSec(), rxAdaptiveMaxSec);
      postRxWorkletConfig(true);
    } else if (msg.type === 'stats') {
      const rate = ctx?.sampleRate || 48000;
      rxAdaptive.bufferedSec = (msg.bufferedFrames || 0) / rate;
      rxAdaptive.droppedFrames = msg.droppedFrames || rxAdaptive.droppedFrames;
    }
  }

  function enqueueRxPCM(pcm, sampleRate) {
    if (!pcm || !pcm.length) return;
    queueVu('rx', pcm);
    const rate = sampleRate || remoteCfg.sample_rate || 16000;
    observeRxArrival(pcm.length, rate);
    let res = null;
    if (ctx) {
      res = resampleRxF32(pcm16ToFloat32(pcm), rate, ctx.sampleRate);
    }
    if (rxWorkletNode && ctx) {
      if (!res || !res.length) return;
      rxWorkletNode.port.postMessage({ type: 'audio', data: res }, [res.buffer]);
      postRxWorkletConfig();
      return;
    }
    if (res && ctx) {
      rxQueue.push({ f32: res, sampleRate: ctx.sampleRate });
    } else {
      rxQueue.push({ pcm, sampleRate: rate });
    }
    const maxBufferSec = clampValue(rxAdaptive.targetSec * 2.4, rxAdaptive.targetSec + 0.12, rxAdaptiveHardMaxSec);
    const targetBufferSec = clampValue(rxAdaptive.targetSec * 1.15, rxAdaptive.targetSec, rxAdaptiveMaxSec);
    if (rxQueuedSeconds() > maxBufferSec) {
      while (rxQueuedSeconds() > targetBufferSec && rxQueue.length > 1) rxQueue.shift();
    }
    if (rxEnabled && ctx) scheduleAudio();
  }

  function float32ToInt16Array(f32) {
    const out = new Int16Array(f32.length);
    for (let i = 0; i < f32.length; i++) {
      const s = Math.max(-1, Math.min(1, f32[i]));
      out[i] = s < 0 ? s * 32768 : s * 32767;
    }
    return out;
  }

  function loopbackTxFrame(f32, sampleRate, frameSamples) {
    if (!frontendLoopbackEnabled()) return;
    if (!rxEnabled) startRx();
    if (f32 && f32.length) {
      enqueueRxPCM(float32ToInt16Array(f32), sampleRate);
      return;
    }
    enqueueRxPCM(new Int16Array(frameSamples || 0), sampleRate);
  }

  function keyCountForModel(model, fallback) {
    const token = String(model || '').toUpperCase().replace(/[^A-Z0-9]/g, '');
    if (profileKeyCounts[token]) return profileKeyCounts[token];
    const found = Object.keys(profileKeyCounts).find((name) => token.startsWith(name));
    if (found) return profileKeyCounts[found];
    return fallback;
  }

  function defaultPanelLabel(index) {
    if (String(profileModel || '').toUpperCase() === 'BP7100') {
      return ['A', 'B', 'CALL', 'REPLY'][index] || `K${String(index + 1).padStart(2, '0')}`;
    }
    return `K${String(index + 1).padStart(2, '0')}`;
  }

  function activeKeyLimit() {
    return profileModel ? keyCountForModel(profileModel, 0) : 0;
  }

  function boundedPanelSize(n) {
    const limit = activeKeyLimit();
    return limit > 0 ? Math.min(n, limit) : n;
  }

  function panelTargetTitle() {
    const port = String(panelTarget.port || '').trim();
    const name = String(panelTarget.name || '').trim();
    if (!port) return '';
    if (!name || name.toUpperCase() === port.toUpperCase()) return port;
    return `${port} - ${name}`;
  }

  function setPanelTarget(port, name = '') {
    const nextPort = String(port || '').trim();
    const nextName = String(name || '').trim();
    if (!nextPort && !nextName) return;
    panelTarget = {
      port: nextPort || panelTarget.port,
      name: nextName || panelTarget.name,
    };
    updatePanelHeader(profileModel, panel.n);
  }

  function updatePanelHeader(model, keys) {
    const label = model || 'TP5024';
    const title = $('panelTitle');
    const profile = $('profileBadge');
    const count = $('buttonCount');
    if (title) title.textContent = panelTargetTitle() || `${label} Panel`;
    if (profile) profile.textContent = label;
    if (count) count.textContent = `${keys} ${keys === 1 ? 'key' : 'keys'}`;
    updateFloatingHeader();
  }

  function ensurePanelSize(n) {
    n = boundedPanelSize(n);
    if (n <= panel.n) return;
    resizePanel(n);
  }

  function resizePanel(n) {
    n = Math.max(1, n | 0);
    const old = panel.n;
    const resizeBool = (arr) => {
      arr.length = n;
      for (let i = old; i < n; i++) arr[i] = false;
    };
    resizeBool(panel.pressed);
    panel.label.length = n;
    for (let i = old; i < n; i++) panel.label[i] = '';
    resizeBool(panel.gFix);
    resizeBool(panel.rFix);
    resizeBool(panel.gBlink);
    resizeBool(panel.rBlink);
    panel.n = n;
    updatePanelHeader(profileModel, panel.n);
    renderPanel(true);
  }

  function applyEmulationConfig(msg) {
    const model = msg.model || msg.emulate_device || msg.simulate_device || profileModel || '';
    const name = msg.name || msg.emulate_name || msg.simulate_name || '';
    const rawKeys = (msg.keys ?? msg.emulate_keys ?? msg.simulate_keys ?? 0) | 0;
    const keys = keyCountForModel(model, rawKeys || panel.n) | 0;
    if (model) profileModel = String(model).toUpperCase();
    if (keys > 0) {
      resizePanel(keys);
    }
    updatePanelHeader(profileModel || model, panel.n);
    if (model && (msg.type === 'prtp_emulation' || msg.type === 'prtp_simulation')) log('Emulation profile', model, name ? `(${name})` : '', `${keys || panel.n} keys`);
  }

  function renderPanel(full=false) {
    const root = $('panel');
    // Build missing nodes
    if (full) {
      root.innerHTML = '';
      for (let i = 0; i < panel.n; i++) {
        const d = document.createElement('button');
        d.type = 'button';
        d.className = 'key'; d.dataset.idx = i;
        d.title = `Key ${i}: tap to toggle, hold for momentary`;
        const leds = document.createElement('div'); leds.className = 'leds';
        leds.innerHTML = '<span class="led green" data-led="green" data-state="off"></span><span class="led red" data-led="red" data-state="off"></span>';
        const idx = document.createElement('div'); idx.className = 'idx'; idx.textContent = `#${i}`;
        const txt = document.createElement('div'); txt.className = 'text'; txt.textContent = panel.label[i] || '';
        d.appendChild(leds); d.appendChild(idx); d.appendChild(txt);
        d.addEventListener('pointerdown', onKeyPointerDown);
        d.addEventListener('pointerup', onKeyPointerRelease);
        d.addEventListener('pointercancel', onKeyPointerRelease);
        d.addEventListener('contextmenu', (ev) => ev.preventDefault());
        root.appendChild(d);
      }
    }
    // Update states
    const children = root.children;
    for (let i = 0; i < panel.n && i < children.length; i++) {
      const d = children[i];
      d.classList.toggle('pressed', !!panel.pressed[i]);
      const txt = d.querySelector('.text');
      if (txt && txt.textContent !== (panel.label[i] || '')) txt.textContent = panel.label[i] || '';
      setLedState(d.querySelector('.led.green'), panel.gBlink[i] ? 'blink' : panel.gFix[i] ? 'on' : 'off');
      setLedState(d.querySelector('.led.red'), panel.rBlink[i] ? 'blink' : panel.rFix[i] ? 'on' : 'off');
    }
    renderFloatingPanel(full);
  }

  function setLedState(led, state) {
    if (!led) return;
    const next = state || 'off';
    if (led.dataset.state === next) return;
    led.dataset.state = next;
    led.title = `${led.dataset.led || 'LED'} ${next}`;
  }

  function clearPanelStatus() {
    activeKeyPointers.clear();
    panel.pressed.fill(false);
    panel.gFix.fill(false);
    panel.rFix.fill(false);
    panel.gBlink.fill(false);
    panel.rBlink.fill(false);
    renderPanel();
  }

  function sendKey(index, pressed) {
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      log(`KEY ${index} ${pressed ? 'press' : 'release'} skipped: WS not connected`);
      return false;
    }
    return sendControlJSON({type:'prtp_send', cmd:'KEY', index, pressed});
  }

  function setLocalKey(index, pressed) {
    ensurePanelSize(Math.max(panel.n, index + 1));
    if (index >= 0 && index < panel.pressed.length) {
      panel.pressed[index] = !!pressed;
      renderPanel();
    }
  }

  function applyKeyStateGroups(groups) {
    ensurePanelSize(Math.max(panel.n, groups.length * 8));
    for (let i = 0; i < groups.length * 8 && i < panel.pressed.length; i++) panel.pressed[i] = false;
    for (let j = 0; j < groups.length; j++) {
      const g = groups[j] & 0xFF;
      for (let k = 0; k < 8; k++) {
        const index = j * 8 + k;
        if (index < panel.pressed.length) panel.pressed[index] = (g & (1 << k)) !== 0;
      }
    }
    renderPanel();
  }

  function onKeyPointerDown(ev) {
    if (ev.button !== undefined && ev.button !== 0) return;
    ev.preventDefault();
    if (activeKeyPointers.has(ev.pointerId)) return;
    const el = ev.currentTarget;
    const index = parseInt(el.dataset.idx, 10);
    if (!Number.isFinite(index)) return;
    try { el.setPointerCapture(ev.pointerId); } catch {}
    el.classList.add('local-press');
    const wasDown = !!panel.pressed[index];
    const state = {
      el,
      index,
      wasDown,
      pressedAt: performance.now(),
      sentDown: false,
    };
    if (!wasDown && sendKey(index, true)) {
      state.sentDown = true;
      setLocalKey(index, true);
      log(`KEY ${index} press`);
    }
    activeKeyPointers.set(ev.pointerId, state);
  }

  function onKeyPointerRelease(ev) {
    const state = activeKeyPointers.get(ev.pointerId);
    if (!state) {
      ev.currentTarget.classList.remove('local-press', 'long-press');
      return;
    }
    activeKeyPointers.delete(ev.pointerId);
    try { state.el.releasePointerCapture(ev.pointerId); } catch {}
    state.el.classList.remove('local-press', 'long-press');
    const heldMs = performance.now() - state.pressedAt;
    if (state.wasDown) {
      if (sendKey(state.index, false)) {
        setLocalKey(state.index, false);
        log(`KEY ${state.index} release toggle (${Math.round(heldMs)}ms)`);
      }
      return;
    }
    if (state.sentDown && heldMs >= monostableHoldMs) {
      if (sendKey(state.index, false)) {
        setLocalKey(state.index, false);
        log(`KEY ${state.index} monostable release (${Math.round(heldMs)}ms)`);
      }
      return;
    }
    log(`KEY ${state.index} latched down (${Math.round(heldMs)}ms < ${monostableHoldMs}ms)`);
  }

  function prtpCrc(payload) {
    if (!payload || payload.length === 0) return 0;
    return prtpCrcResidue([...payload, 0]);
  }

  function prtpCrcResidue(frame) {
    if (!frame || frame.length === 0) return 0;
    let res = frame[0] & 0xFF;
    for (let i = 0; i < frame.length - 1; i++) {
      for (let bit = 0; bit < 8;) {
        if ((res & 0x80) === 0) {
          const next = ((frame[i + 1] >> (7 - bit)) & 1);
          res = ((res << 1) | next) & 0xFF;
          bit++;
        }
        res ^= 0x8D;
      }
    }
    return res & 0xFF;
  }

  function isPrtpMessageType(b) {
    return b === 0x41 || b === 0x43 || b === 0x49 || b === 0x4E || b === 0x50 || b === 0x52 || b === 0x53;
  }

  function normalizePrtpPayload(raw) {
    if (raw.length >= 2 && !isPrtpMessageType(raw[0] & 0xFF) && isPrtpMessageType(raw[1] & 0xFF)) {
      return { payload: raw.slice(1), prefix: raw[0] & 0xFF };
    }
    return { payload: raw, prefix: null };
  }

  function prtpIdentityText(payload) {
    if (payload.length >= 18 && payload[0] === 0x49 && payload[1] === 0xD0) {
      return String.fromCharCode.apply(null, payload.slice(3, 13)).replace(/[\x00 ]+$/g, '');
    }
    if (payload.length >= 3 && payload[0] === 0x49 && payload[1] === 0xD0 && ((payload[2] & 0xF0) === 0x20)) {
      const tailLen = payload[2] & 0x0F;
      const start = 3;
      const end = Math.min(payload.length, start + tailLen);
      const textEnd = Math.max(start, tailLen >= 5 ? end - 5 : end);
      return String.fromCharCode.apply(null, payload.slice(start, textEnd)).replace(/[\x00 ]+$/g, '');
    }
    return String.fromCharCode.apply(null, payload.slice(2));
  }

  function prtpHandleFrame(frame) {
    if (!frame || frame.length < 2) { trace('PRTP short frame'); return; }
    const crc = frame[frame.length - 1] & 0xFF;
    const rawPayload = frame.slice(0, frame.length - 1);
    const calc = prtpCrc(rawPayload);
    if (prtpCrcResidue(frame) !== 0) trace(`PRTP CRC mismatch got=0x${crc.toString(16)} expect=0x${calc.toString(16)} (accepting)`);
    const { payload, prefix } = normalizePrtpPayload(rawPayload);
    if (prefix !== null) trace(`PRTP prefix 0x${prefix.toString(16)}`);
    const type = payload[0] & 0xFF;
    switch (type) {
      case 0x50: // 'P'
        trace('PRTP: Ping');
        break;
      case 0x41: // 'A'
        trace('PRTP: Ack');
        break;
      case 0x4E: // 'N'
        trace('PRTP: Nack');
        break;
      case 0x52: // 'R'
        trace(`PRTP: R ${payload.length > 1 ? payload[1] : ''}`);
        break;
      case 0x43: // 'C'
        trace('PRTP: Cmd');
        break;
      case 0x49: { // 'I'
        if (payload.length >= 3 && payload[1] === 0x00) {
          const key = payload[2] & 0xFF;
          trace(`PRTP key event: ${key & 0x7F} ${(key & 0x80) ? 'pressed' : 'released'}`);
          break;
        }
        if (payload.length < 2) { trace('PRTP I: too short'); break; }
        const sub = payload[1] & 0xF0;
        const cnt = payload[1] & 0x0F;
        if (sub === 0x40) { // keys
          const groups = [];
          for (let j = 0; j < cnt && 2 + j < payload.length; j++) groups.push(payload[2 + j] & 0xFF);
          ensurePanelSize(Math.max(panel.n, cnt * 8));
          const changes = [];
          for (let j = 0; j < groups.length; j++) {
            const g = groups[j];
            for (let k = 0; k < 8; k++) {
              const idx = j * 8 + k;
              const down = (g & (1 << k)) !== 0;
              changes.push(`${idx}:${down ? '1' : '0'}`);
            }
          }
          trace('PRTP I keys:', changes.join(' '));
          applyKeyStateGroups(groups);
        } else if (sub === 0x90) { // label
          const index = payload[2] & 0xFF;
          const len = cnt - 1;
          const start = 3;
          const end = Math.max(start, Math.min(payload.length, start + len));
          const text = String.fromCharCode.apply(null, payload.slice(start, end));
          if (index >= 0) {
            ensurePanelSize(Math.max(panel.n, index + 1));
            if (index < panel.n) {
              panel.label[index] = text;
              renderPanel();
            }
          }
          log(`PRTP I label[${index}]=${text}`);
        } else if (sub === 0x50 || sub === 0x60 || sub === 0x70 || sub === 0x80) {
          // LEDs
          const kind = {0x50:'G-FIX',0x60:'R-FIX',0x70:'G-BLINK',0x80:'R-BLINK'}[sub] || 'LED';
          const groups = [];
          for (let j = 0; j < cnt && 2 + j < payload.length; j++) groups.push(payload[2 + j] & 0xFF);
          const on = [];
          // reset the relevant class across addressed indices
          const setArr = sub===0x50?panel.gFix: sub===0x60?panel.rFix: sub===0x70?panel.gBlink: panel.rBlink;
          ensurePanelSize(Math.max(panel.n, cnt * 8));
          for (let i = 0; i < cnt*8 && i < setArr.length; i++) setArr[i] = false;
          for (let j = 0; j < cnt; j++) {
            const g = groups[j];
            for (let k = 0; k < 8; k++) {
              if (g & (1 << k)) {
                const idx = j * 8 + k;
                if (idx < setArr.length) setArr[idx] = true;
                on.push(idx);
              }
            }
          }
          trace(`PRTP I ${kind}: on=[${on.join(',')}]`);
          renderPanel();
        } else if (sub === 0xD0) {
          const text = prtpIdentityText(payload);
          if (text) log('PRTP I ident:', text);
        } else {
          trace('PRTP I subtype 0x' + sub.toString(16));
        }
        break;
      }
      default:
        trace('PRTP type 0x' + type.toString(16));
    }
  }

  function applyPrtpInfo(msg) {
    switch (msg.kind) {
      case 'ping':
      case 'ack':
      case 'nack':
      case 'cmd':
      case 'r':
        trace(`PRTP: ${msg.kind}`);
        break;
      case 'key_event': {
        const index = msg.index | 0;
        setLocalKey(index, !!msg.pressed);
        trace(`PRTP key baseline: ${index} ${msg.pressed ? 'down' : 'up'}`);
        break;
      }
      case 'keys': {
        const groups = Array.isArray(msg.groups) ? msg.groups : [];
        trace('PRTP key groups:', groups.map((v) => `0x${(v & 0xFF).toString(16).padStart(2, '0')}`).join(' '));
        applyKeyStateGroups(groups);
        break;
      }
      case 'leds': {
        const groups = Array.isArray(msg.groups) ? msg.groups : [];
        trace('PRTP leds:', msg.mode || '', groups.map((v) => `0x${(v & 0xFF).toString(16).padStart(2, '0')}`).join(' '));
        const setArr = msg.mode === 'g_fix' ? panel.gFix : msg.mode === 'r_fix' ? panel.rFix : msg.mode === 'g_blink' ? panel.gBlink : panel.rBlink;
        ensurePanelSize(Math.max(panel.n, groups.length * 8));
        for (let i = 0; i < groups.length * 8 && i < setArr.length; i++) setArr[i] = false;
        for (let j = 0; j < groups.length; j++) {
          const g = groups[j] & 0xFF;
          for (let k = 0; k < 8; k++) {
            const index = j * 8 + k;
            if (index < panel.n && (g & (1 << k))) setArr[index] = true;
          }
        }
        renderPanel();
        break;
      }
      case 'label': {
        const index = msg.index | 0;
        ensurePanelSize(Math.max(panel.n, index + 1));
        if (index < panel.n) {
          panel.label[index] = msg.text || '';
          renderPanel();
        }
        break;
      }
      case 'ident':
        if (msg.text) log('PRTP I ident:', msg.text);
        break;
      default:
        trace('PRTP:', msg.kind || 'unknown');
    }
  }

  function applyMatrixNames(msg) {
    matrixNamesReceived = !!msg.ok;
    if (autoMatrixNamesTimer) {
      clearTimeout(autoMatrixNamesTimer);
      autoMatrixNamesTimer = null;
    }
    if (msg.addr) {
      lastMatrixNamesAddr = String(msg.addr).trim();
    }
    if (!msg.ok) {
      log('Matrix names failed:', msg.error || 'unknown error');
      return;
    }
    const ports = Array.isArray(msg.ports) ? msg.ports : [];
    const target = msg.target || {};
    if (target.port) {
      const name = target.name ? ` ${target.name}` : '';
      const type = target.type_name ? ` (${target.type_name}${target.type_code ? ` 0x${Number(target.type_code).toString(16)}` : ''})` : '';
      setPanelTarget(target.port, target.name || '');
      log(`Matrix target: ${target.port}${name}${type}`);
    }
    if (ports.length) {
      const summary = ports
        .filter((p) => p && p.name)
        .map((p) => {
          const type = p.type_name ? `/${p.type_name}` : '';
          return `${p.port || p.index}=${p.name}${type}`;
        })
        .join(', ');
      if (summary) log('Matrix ports:', summary);
    }
    if (msg.map_bank) {
      const bankName = msg.map_bank_name ? ` ${msg.map_bank_name}` : '';
      const size = Number.isFinite(msg.map_size) && msg.map_size >= 0 ? ` (${msg.map_size} bytes)` : '';
      log(`Matrix map: bank ${msg.map_bank}${bankName}${size}`);
    } else if (msg.map_error) {
      log('Matrix map read failed:', msg.map_error);
    }
    const labels = Array.isArray(msg.button_labels) ? msg.button_labels.map((v) => v || '') : [];
    const labelCount = labels.filter(Boolean).length;
    if (!labelCount) {
      log('Matrix map labels: none returned');
      return;
    }
    ensurePanelSize(Math.max(panel.n, labels.length));
    for (let i = 0; i < labels.length && i < panel.label.length; i++) {
      panel.label[i] = labels[i] || defaultPanelLabel(i);
    }
    renderPanel();
    log(`Matrix button labels: ${labelCount}/${labels.length}`);
  }

  function pcm16ToFloat32(pcm) {
    const out = new Float32Array(pcm.length);
    for (let i = 0; i < pcm.length; i++) {
      out[i] = Math.max(-1, Math.min(1, pcm[i] / 32768));
    }
    return out;
  }

  function resampleF32(input, inRate, outRate) {
    if (inRate === outRate) return input;
    const ratio = outRate / inRate;
    const outLen = Math.max(1, Math.round(input.length * ratio));
    const out = new Float32Array(outLen);
    for (let i = 0; i < outLen; i++) {
      const srcPos = i / ratio;
      const j = Math.floor(srcPos);
      const frac = srcPos - j;
      const s0 = input[Math.min(j, input.length - 1)] || 0;
      const s1 = input[Math.min(j + 1, input.length - 1)] || 0;
      out[i] = s0 * (1 - frac) + s1 * frac;
    }
    return out;
  }

  function makeF32StreamResampler(inRate, outRate) {
    return {
      inRate,
      outRate,
      pos: 0,
      buf: new Float32Array(0),
      process(input) {
        if (!input || !input.length) return new Float32Array(0);
        if (this.inRate === this.outRate) {
          const out = new Float32Array(input.length);
          out.set(input);
          return out;
        }
        const merged = new Float32Array(this.buf.length + input.length);
        merged.set(this.buf);
        merged.set(input, this.buf.length);
        this.buf = merged;
        const step = this.inRate / this.outRate;
        const out = [];
        while (this.pos + 1 < this.buf.length) {
          const j = Math.floor(this.pos);
          const frac = this.pos - j;
          const s0 = this.buf[j] || 0;
          const s1 = this.buf[j + 1] || 0;
          out.push(s0 * (1 - frac) + s1 * frac);
          this.pos += step;
        }
        let drop = Math.floor(this.pos);
        if (drop > 0) {
          drop = Math.min(drop, Math.max(0, this.buf.length - 1));
          this.buf = this.buf.slice(drop);
          this.pos -= drop;
        }
        return Float32Array.from(out);
      },
    };
  }

  function resetRxResampler() {
    rxResampler = null;
  }

  function normalizeBrowserSinkID(id) {
    return id && id !== 'default' ? id : '';
  }

  function selectedBrowserInputID() {
    return $('browserCaptureDevice')?.value || browserAudio.selected.input || 'default';
  }

  function selectedBrowserOutputID() {
    return $('browserPlaybackDevice')?.value || browserAudio.selected.output || 'default';
  }

  function canEnumerateBrowserAudioDevices() {
    return !!(navigator.mediaDevices && typeof navigator.mediaDevices.enumerateDevices === 'function');
  }

  function canSelectBrowserAudioOutput() {
    const AudioContextType = window.AudioContext || window.webkitAudioContext;
    return !!(
      AudioContextType?.prototype && typeof AudioContextType.prototype.setSinkId === 'function' ||
      window.HTMLMediaElement?.prototype && typeof window.HTMLMediaElement.prototype.setSinkId === 'function'
    );
  }

  function browserAudioDeviceList(devices, kind, fallbackName) {
    const out = [];
    const seen = new Set();
    for (const dev of devices || []) {
      if (!dev || dev.kind !== kind) continue;
      const id = dev.deviceId || 'default';
      if (seen.has(id)) continue;
      seen.add(id);
      const isDefault = id === 'default';
      out.push({
        id,
        name: dev.label || (isDefault ? `Default ${fallbackName}` : `${fallbackName} ${out.length + 1}`),
        is_default: isDefault,
      });
    }
    if (!out.some((dev) => dev.id === 'default')) {
      out.unshift({ id: 'default', name: `Default ${fallbackName}`, is_default: true });
    }
    return out;
  }

  function setBrowserAudioStatus(text, kind) {
    const badge = $('browserAudioStatus');
    if (!badge) return;
    badge.textContent = text;
    badge.classList.toggle('ok', kind === 'ok');
    badge.classList.toggle('warn', kind === 'warn');
  }

  function updateBrowserAudioControls() {
    const supported = canEnumerateBrowserAudioDevices();
    const liveMode = selectedAudioTab() === 'live';
    const capture = $('browserCaptureDevice');
    const playback = $('browserPlaybackDevice');
    const refresh = $('btnBrowserAudioRefresh');
    const outputSelectable = canSelectBrowserAudioOutput();
    if (refresh) refresh.disabled = !liveMode || !supported;
    if (capture) capture.disabled = !liveMode || !supported || !canUseMicTx() || !browserAudio.input.length;
    if (playback) playback.disabled = !liveMode || !supported || !browserAudio.output.length || !outputSelectable;

    if (!supported) {
      setBrowserAudioStatus('Browser audio unavailable', 'warn');
    } else if (!outputSelectable) {
      setBrowserAudioStatus('Browser audio default out', 'warn');
    } else {
      const inputs = Math.max(0, browserAudio.input.length);
      const outputs = Math.max(0, browserAudio.output.length);
      setBrowserAudioStatus(`Browser audio ${inputs} in / ${outputs} out`, 'ok');
    }
  }

  async function refreshBrowserAudioDevices({ logErrors = true } = {}) {
    if (!canEnumerateBrowserAudioDevices()) {
      browserAudio = {
        ...browserAudio,
        supported: false,
        input: [{ id: 'default', name: 'Default input', is_default: true }],
        output: [{ id: 'default', name: 'Default output', is_default: true }],
      };
      setDeviceOptions($('browserCaptureDevice'), browserAudio.input, browserAudio.selected.input);
      setDeviceOptions($('browserPlaybackDevice'), browserAudio.output, browserAudio.selected.output);
      updateAudioControls();
      return false;
    }
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      browserAudio = {
        ...browserAudio,
        supported: true,
        input: browserAudioDeviceList(devices, 'audioinput', 'input'),
        output: browserAudioDeviceList(devices, 'audiooutput', 'output'),
      };
      setDeviceOptions($('browserCaptureDevice'), browserAudio.input, browserAudio.selected.input);
      setDeviceOptions($('browserPlaybackDevice'), browserAudio.output, browserAudio.selected.output);
      browserAudio.selected.input = selectedBrowserInputID();
      browserAudio.selected.output = selectedBrowserOutputID();
      storageSet(browserAudioInputStorageKey, browserAudio.selected.input);
      storageSet(browserAudioOutputStorageKey, browserAudio.selected.output);
      updateAudioControls();
      return true;
    } catch (err) {
      browserAudio = { ...browserAudio, supported: false };
      if (logErrors) log('Browser audio devices:', err?.message || err);
      updateAudioControls();
      return false;
    }
  }

  function ensureRxOutputGain() {
    if (!ctx) return null;
    if (!rxOutputGain) {
      rxOutputGain = ctx.createGain();
      applySpeakerMute();
    }
    return rxOutputGain;
  }

  function applySpeakerMute() {
    if (rxOutputGain) rxOutputGain.gain.value = speakerMuted ? 0 : 1;
  }

  function ensureBrowserOutputElement() {
    if (browserOutputElement) return browserOutputElement;
    browserOutputElement = document.createElement('audio');
    browserOutputElement.id = 'browserAudioOutputElement';
    browserOutputElement.autoplay = true;
    browserOutputElement.playsInline = true;
    browserOutputElement.hidden = true;
    document.body.appendChild(browserOutputElement);
    return browserOutputElement;
  }

  function ensureBrowserOutputStreamDest() {
    if (!ctx) return null;
    if (!browserOutputStreamDest) {
      browserOutputStreamDest = ctx.createMediaStreamDestination();
      ensureBrowserOutputElement().srcObject = browserOutputStreamDest.stream;
    }
    return browserOutputStreamDest;
  }

  async function applyBrowserAudioOutput({ logResult = false } = {}) {
    if (!ctx) return true;
    const output = ensureRxOutputGain();
    if (!output) return false;
    const selected = selectedBrowserOutputID();
    browserAudio.selected.output = selected;
    storageSet(browserAudioOutputStorageKey, selected);
    const sinkID = normalizeBrowserSinkID(selected);
    try { output.disconnect(); } catch {}

    if (typeof ctx.setSinkId === 'function') {
      try {
        await ctx.setSinkId(sinkID);
        output.connect(ctx.destination);
        browserOutputRoute = `context:${sinkID || 'default'}`;
        if (logResult) log('Browser audio out:', selected || 'default');
        return true;
      } catch (err) {
        log('Browser audio out failed:', err?.message || err);
      }
    }

    const element = ensureBrowserOutputElement();
    if (typeof element.setSinkId === 'function') {
      try {
        const streamDest = ensureBrowserOutputStreamDest();
        await element.setSinkId(sinkID);
        output.connect(streamDest);
        if (element.paused) await element.play();
        browserOutputRoute = `element:${sinkID || 'default'}`;
        if (logResult) log('Browser audio out:', selected || 'default');
        return true;
      } catch (err) {
        log('Browser audio out failed:', err?.message || err);
      }
    }

    output.connect(ctx.destination);
    browserOutputRoute = 'context:default';
    if (sinkID) log('Browser audio out selection is unsupported by this browser; using default output');
    return !sinkID;
  }

  function connectRxNode(node) {
    const dest = ensureRxOutputGain();
    if (dest) {
      node.connect(dest);
    } else if (ctx) {
      node.connect(ctx.destination);
    }
  }

  function resampleRxF32(input, inRate, outRate) {
    if (!rxResampler || rxResampler.inRate !== inRate || rxResampler.outRate !== outRate) {
      rxResampler = makeF32StreamResampler(inRate, outRate);
    }
    return rxResampler.process(input);
  }

  function float32ToPCM16(f32) {
    const buf = new ArrayBuffer(f32.length * 2);
    const view = new DataView(buf);
    for (let i = 0; i < f32.length; i++) {
      let s = Math.max(-1, Math.min(1, f32[i]));
      view.setInt16(i * 2, s * 32767, true);
    }
    return new Uint8Array(buf);
  }

  function connect() {
    const controlBusy = ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING);
    const audioBusy = audioWs && (audioWs.readyState === WebSocket.OPEN || audioWs.readyState === WebSocket.CONNECTING);
    if (controlBusy && audioBusy) return;
    manualDisconnect = false;
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
    const controlInput = $('wsUrl');
    const audioInput = $('audioWsUrl');
    const controlUrl = (controlInput?.value || defaultControlWsUrl()).trim();
    const audioUrl = (audioInput?.value || defaultAudioWsUrl()).trim();
    if (controlInput) controlInput.value = controlUrl;
    if (audioInput) audioInput.value = audioUrl;
    setConnectionState('Connecting', 'warn');
    if (!audioBusy) {
      try {
        audioWs = new WebSocket(audioUrl);
      } catch (err) {
        audioWs = null;
        setConnectionState('Error', 'warn');
        log('Audio WS error', err.message || err);
        return;
      }
      audioWs.binaryType = 'arraybuffer';
      const origAudioSend = audioWs.send.bind(audioWs);
      audioWs.send = (data) => {
        try {
          if (typeof data === 'string') {
            trace('[Audio WS->] text', data);
          } else {
            const len = data?.byteLength ?? data?.length ?? 0;
            if (len <= 128) trace('[Audio WS->] binary', len, 'bytes');
          }
        } catch (err) {
          trace('[Audio WS->] send log error:', err);
        }
        return origAudioSend(data);
      };
      audioWs.onopen = () => {
        log('Audio WS connected');
        sendAudioConfig(parseInt($('sr').value, 10), parseInt($('fs').value, 10));
        updateAudioControls();
        maybeAutoStartAudio();
      };
      audioWs.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          trace('[Audio WS<-] text', ev.data);
          return;
        }
        handleAudioFrame(ev.data);
      };
      audioWs.onclose = () => {
        audioWs = null;
        if (txEnabled || txStarting) stopTx();
        updateAudioControls();
        log('Audio WS closed');
        if (!manualDisconnect) scheduleReconnect();
      };
      audioWs.onerror = (e) => {
        updateAudioControls();
        log('Audio WS error', e.message || e);
      };
    }
    if (controlBusy) return;
    try {
      ws = new WebSocket(controlUrl);
    } catch (err) {
      ws = null;
      setConnectionState('Error', 'warn');
      log('Control WS error', err.message || err);
      return;
    }
    ws.binaryType = 'arraybuffer';
    const origSend = ws.send.bind(ws);
    ws.send = (data) => {
      try {
        if (data instanceof ArrayBuffer) {
          const len = data.byteLength || 0;
          if (len <= 128) trace('[Control WS->] binary', len, 'bytes');
        } else if (ArrayBuffer.isView(data)) {
          const len = data.byteLength ?? data.length ?? 0;
          if (len <= 128) trace('[Control WS->] view', len, 'bytes');
        } else {
          trace('[Control WS->] text', data);
        }
      } catch (err) {
        trace('[Control WS->] send log error:', err);
      }
      return origSend(data);
    };
    ws.onopen = () => {
      setConnectionState('Connected', 'ok');
      updateAudioControls();
      log('Control WS connected');
      const cfg = {
        type: 'config',
        sample_rate: parseInt($('sr').value, 10),
        channels: 1,
        frame_samples: parseInt($('fs').value, 10)
      };
      sendControlJSON(cfg);
      // start latency pings
      startPings();
      maybeAutoStartAudio();
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        trace('[Control WS<-] text', ev.data);
        try {
          const msg = JSON.parse(ev.data);
          if (msg && msg.type === 'config') {
            remoteCfg = msg;
            log('Remote config', JSON.stringify(msg));
            if (!txEnabled && !txStarting) {
              if (Number.isFinite(msg.sample_rate) && $('sr')) $('sr').value = msg.sample_rate;
              if (Number.isFinite(msg.frame_samples) && $('fs')) $('fs').value = msg.frame_samples;
            }
            applyEmulationConfig(msg);
            if (msg.matrix_addr && $('matrixAddr')) $('matrixAddr').value = msg.matrix_addr;
            if (msg.matrix_port) {
              setPanelTarget(msg.matrix_port);
              log(`Matrix target configured: ${msg.matrix_port}`);
            }
            if (msg.server_audio_supported !== undefined) {
              applyServerAudioMessage({
                supported: msg.server_audio_supported,
                backend: msg.server_audio_backend,
                tx_source: msg.tx_source,
              });
            }
            maybeAutoFetchMatrixNames();
          } else if (msg && (msg.type === 'server_audio_devices' || msg.type === 'server_audio_status')) {
            applyServerAudioMessage(msg);
          } else if (msg && msg.type === 'prtp_control') {
            // show control bytes length; could dump hex if desired
            const n = msg.n || (msg.b64 ? (atob(msg.b64).length) : 0);
            trace('PRTP control bytes:', n);
          } else if (msg && msg.type === 'prtp_info') {
            applyPrtpInfo(msg);
          } else if (msg && msg.type === 'matrix_names') {
            applyMatrixNames(msg);
          } else if (msg && (msg.type === 'prtp_emulation' || msg.type === 'prtp_simulation')) {
            applyEmulationConfig(msg);
          } else if (msg && msg.type === 'stats') {
            $('statRxFrames').textContent = msg.rx_frames ?? 0;
            $('statTxFrames').textContent = msg.tx_frames ?? 0;
            $('statRxBytes').textContent = msg.rx_bytes ?? 0;
            $('statTxBytes').textContent = msg.tx_bytes ?? 0;
            updateFloatingStats();
          } else if (msg && msg.type === 'pong') {
            // compute RTT
            const t = msg.t || 0;
            const now = performance.now();
            const rtt = Math.max(0, now - t);
            $('statRTT').textContent = rtt.toFixed(1);
            updateFloatingStats();
          }
        } catch {}
        return;
      }
      trace('Ignoring non-text control WebSocket frame');
    };
    ws.onclose = () => {
      ws = null;
      stopPings();
      if (txEnabled || txStarting) stopTx();
      clearPanelStatus();
      autoMatrixNamesFetched = false;
      if (autoMatrixNamesTimer) {
        clearTimeout(autoMatrixNamesTimer);
        autoMatrixNamesTimer = null;
      }
      setConnectionState('Disconnected', 'warn');
      updateAudioControls();
      log('Control WS closed');
      if (!manualDisconnect) {
        scheduleReconnect();
      }
    };
    ws.onerror = (e) => {
      setConnectionState('Error', 'warn');
      updateAudioControls();
      log('Control WS error', e.message || e);
    };
  }

  function scheduleReconnect() {
    window.clearTimeout(reconnectTimer);
    reconnectTimer = window.setTimeout(connect, 1500);
  }

  function handleAudioFrame(data) {
    const len = data?.byteLength ?? 0;
    if (len <= 128) trace('[Audio WS<-] binary', len, 'bytes');
    if (!rxEnabled) return;
    const pcm = new Int16Array(data.byteLength / 2);
    const dv = new DataView(data);
    for (let i = 0; i < pcm.length; i++) {
      pcm[i] = dv.getInt16(i * 2, true);
    }
    enqueueRxPCM(pcm, remoteCfg.sample_rate || 16000);
  }

  function disconnect() {
    manualDisconnect = true;
    window.clearTimeout(reconnectTimer);
    if (txEnabled || txStarting) stopTx();
    if (ws) { try { ws.close(); } catch {} ws = null; }
    if (audioWs) { try { audioWs.close(); } catch {} audioWs = null; }
    stopPings();
    setConnectionState('Disconnected', 'warn');
    updateAudioControls();
  }

  function scheduleAudio() {
    if (!rxEnabled || !ctx || rxWorkletNode) return;
    if (ctx.state !== 'running') return;
    const srOut = ctx.sampleRate;
    const defaultSrIn = remoteCfg.sample_rate || 16000;
    // Keep playhead close to the audio clock; do not prefill with silence,
    // because that directly adds latency before the first real packet.
    const minStart = ctx.currentTime + playbackStartDelaySec;
    if (playHead < minStart) playHead = minStart;
    while ((playHead - ctx.currentTime) < rxAdaptive.targetSec) {
      if (!rxQueue.length) break;
      const item = rxQueue.shift();
      const res = item.f32 || resampleF32(pcm16ToFloat32(item.pcm), item.sampleRate || defaultSrIn, srOut);
      const buf = ctx.createBuffer(1, res.length, srOut);
      buf.getChannelData(0).set(res);
      const src = ctx.createBufferSource();
      src.buffer = buf;
      connectRxNode(src);
      scheduledRxSources.add(src);
      src.onended = () => scheduledRxSources.delete(src);
      const dur = res.length / srOut;
      src.start(playHead);
      playHead += dur;
    }
  }

  function stopScheduledRxSources() {
    for (const src of scheduledRxSources) {
      try { src.stop(); } catch {}
      try { src.disconnect(); } catch {}
    }
    scheduledRxSources.clear();
  }

  function resetRxPlaybackClock() {
    rxQueue = [];
    resetRxResampler();
    stopScheduledRxSources();
    if (rxWorkletNode) {
      rxWorkletNode.port.postMessage({ type: 'reset' });
      postRxWorkletConfig(true);
    }
    if (ctx) playHead = ctx.currentTime + playbackStartDelaySec;
  }

  async function ensureRxWorklet() {
    if (!ctx?.audioWorklet || typeof AudioWorkletNode !== 'function') return false;
    if (rxWorkletLoaded) return true;
    const processor = `
class KromaRxJitterBuffer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.capacity = Math.max(16384, Math.ceil(sampleRate * 3));
    this.buffer = new Float32Array(this.capacity);
    this.read = 0;
    this.write = 0;
    this.count = 0;
    this.targetFrames = Math.round(sampleRate * 0.24);
    this.maxFrames = Math.round(sampleRate * 0.85);
    this.started = false;
    this.underruns = 0;
    this.droppedFrames = 0;
    this.reportCountdown = 0;
    this.port.onmessage = (ev) => this.handleMessage(ev.data || {});
  }

  handleMessage(msg) {
    if (msg.type === 'config') {
      if (Number.isFinite(msg.targetFrames)) {
        this.targetFrames = Math.max(128, Math.min(this.capacity, Math.floor(msg.targetFrames)));
      }
      if (Number.isFinite(msg.maxFrames)) {
        this.maxFrames = Math.max(this.targetFrames, Math.min(this.capacity, Math.floor(msg.maxFrames)));
      }
      if (this.count > this.maxFrames) this.dropOldest(this.count - this.maxFrames);
    } else if (msg.type === 'audio' && msg.data && msg.data.length) {
      this.enqueue(msg.data);
    } else if (msg.type === 'reset') {
      this.reset();
    }
  }

  reset() {
    this.read = 0;
    this.write = 0;
    this.count = 0;
    this.started = false;
    this.report();
  }

  dropOldest(frames) {
    const n = Math.max(0, Math.min(this.count, Math.floor(frames)));
    if (!n) return;
    this.read = (this.read + n) % this.capacity;
    this.count -= n;
    this.droppedFrames += n;
  }

  enqueue(input) {
    let data = input;
    if (data.length > this.capacity) {
      this.droppedFrames += data.length - this.capacity;
      data = data.subarray(data.length - this.capacity);
    }
    const overflow = this.count + data.length - this.capacity;
    if (overflow > 0) this.dropOldest(overflow);
    const overMax = this.count + data.length - this.maxFrames;
    if (overMax > 0) this.dropOldest(overMax);
    let offset = 0;
    while (offset < data.length) {
      const take = Math.min(data.length - offset, this.capacity - this.write);
      this.buffer.set(data.subarray(offset, offset + take), this.write);
      this.write = (this.write + take) % this.capacity;
      this.count += take;
      offset += take;
    }
  }

  report() {
    this.port.postMessage({
      type: 'stats',
      bufferedFrames: this.count,
      targetFrames: this.targetFrames,
      underruns: this.underruns,
      droppedFrames: this.droppedFrames,
      active: this.started,
    });
  }

  process(_, outputs) {
    const out = outputs[0] && outputs[0][0];
    if (!out) return true;
    if (!this.started && this.count < this.targetFrames) {
      out.fill(0);
      this.reportCountdown -= out.length;
      if (this.reportCountdown <= 0) {
        this.reportCountdown = Math.floor(sampleRate / 4);
        this.report();
      }
      return true;
    }
    this.started = true;
    let i = 0;
    for (; i < out.length; i++) {
      if (this.count <= 0) break;
      out[i] = this.buffer[this.read];
      this.read = (this.read + 1) % this.capacity;
      this.count--;
    }
    if (i < out.length) {
      out.fill(0, i);
      if (this.started) {
        this.started = false;
        this.underruns++;
        this.port.postMessage({ type: 'underrun', underruns: this.underruns });
      }
    }
    this.reportCountdown -= out.length;
    if (this.reportCountdown <= 0) {
      this.reportCountdown = Math.floor(sampleRate / 4);
      this.report();
    }
    return true;
  }
}
registerProcessor('kroma-rx-jitter-buffer', KromaRxJitterBuffer);
`;
    const url = URL.createObjectURL(new Blob([processor], { type: 'application/javascript' }));
    try {
      await ctx.audioWorklet.addModule(url);
      rxWorkletLoaded = true;
      return true;
    } catch (err) {
      trace('RX AudioWorklet unavailable:', err?.message || err);
      return false;
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  async function startRxWorklet() {
    if (!(await ensureRxWorklet())) return false;
    stopScheduledRxSources();
    if (scheduleTimer) { clearInterval(scheduleTimer); scheduleTimer = null; }
    try {
      rxWorkletNode = new AudioWorkletNode(ctx, 'kroma-rx-jitter-buffer', {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [1],
      });
    } catch (err) {
      trace('RX AudioWorklet node failed:', err?.message || err);
      rxWorkletNode = null;
      return false;
    }
    rxWorkletNode.port.onmessage = handleRxWorkletMessage;
    connectRxNode(rxWorkletNode);
    rxAdaptive.mode = 'worklet';
    postRxWorkletConfig(true);
    if (rxQueue.length) {
      const pending = rxQueue.splice(0);
      for (const item of pending) {
        const res = item.f32 || resampleRxF32(pcm16ToFloat32(item.pcm), item.sampleRate || remoteCfg.sample_rate || 16000, ctx.sampleRate);
        if (res && res.length) {
          rxWorkletNode.port.postMessage({ type: 'audio', data: res }, [res.buffer]);
        }
      }
    }
    log('RX using adaptive AudioWorklet buffer');
    return true;
  }

  function stopRxWorklet() {
    if (!rxWorkletNode) return;
    try { rxWorkletNode.port.postMessage({ type: 'reset' }); } catch {}
    try { rxWorkletNode.port.onmessage = null; } catch {}
    try { rxWorkletNode.port.close(); } catch {}
    try { rxWorkletNode.disconnect(); } catch {}
    rxWorkletNode = null;
  }

  async function startRx() {
    if (rxEnabled) return true;
    if (!ctx) ctx = new (window.AudioContext || window.webkitAudioContext)();
    await applyBrowserAudioOutput({ logResult: selectedBrowserOutputID() !== 'default' });
    rxEnabled = true;
    resetRxAdaptive();
    resetRxPlaybackClock();
    updateAudioControls();
    if (ctx.state === 'suspended') {
      try {
        await ctx.resume();
      } catch (err) {
        rxEnabled = false;
        resetRxPlaybackClock();
        updateAudioControls();
        log('RX start failed:', err?.message || err);
        return false;
      }
      playHead = ctx.currentTime + playbackStartDelaySec;
    }
    if (ctx.state !== 'running') {
      rxEnabled = false;
      resetRxPlaybackClock();
      updateAudioControls();
      log('RX start failed: browser audio is not running; click the microphone button to enable audio');
      setMicEnableNeeded(true, 'Click the microphone button to enable browser audio.');
      return false;
    }
    if (!(await startRxWorklet())) {
      rxAdaptive.mode = 'scheduler';
      if (scheduleTimer) { clearInterval(scheduleTimer); scheduleTimer = null; }
      scheduleTimer = setInterval(scheduleAudio, scheduleIntervalMs);
      scheduleAudio();
      log('RX using adaptive scheduler buffer');
    }
    updateAudioControls();
    return true;
  }

  function stopRx() {
    rxEnabled = false;
    stopRxWorklet();
    resetRxPlaybackClock();
    if (scheduleTimer) { clearInterval(scheduleTimer); scheduleTimer = null; }
    resetVu('rx');
    updateAudioControls();
  }

  // --- TX ---
  function setTxEnabled(enabled) {
    txEnabled = !!enabled;
    updateAudioControls();
  }

  function stopPacedTx() {
    txRunID++;
    if (txTimer) {
      clearTimeout(txTimer);
      txTimer = null;
    }
  }

  function startPacedTx(sr, frameSamples, emitFrame) {
    stopPacedTx();
    const runID = txRunID;
    const frameMs = 1000 * frameSamples / sr;
    let nextAt = performance.now();
    const maxCatchUpFrames = 3;
    const tick = () => {
      if (!txEnabled || runID !== txRunID) return;
      const now = performance.now();
      if (nextAt < now - frameMs * maxCatchUpFrames) {
        nextAt = now;
      }
      let sent = 0;
      do {
        emitFrame();
        nextAt += frameMs;
        sent++;
      } while (txEnabled && runID === txRunID && sent < maxCatchUpFrames && performance.now() >= nextAt);
      if (!txEnabled || runID !== txRunID) return;
      txTimer = setTimeout(tick, Math.max(0, nextAt - performance.now()));
    };
    tick();
  }

  function startToneTx(sr, frameSamples, freq) {
    let phase = 0; const step = 2 * Math.PI * freq / sr;
    activeTxMode = 'tone';
    setTxEnabled(true);
    startPacedTx(sr, frameSamples, () => {
      const f32 = new Float32Array(frameSamples);
      for (let i = 0; i < frameSamples; i++) { f32[i] = 0.2 * Math.sin(phase); phase += step; if (phase > 2*Math.PI) phase -= 2*Math.PI; }
      queueVu('tx', f32);
      loopbackTxFrame(f32, sr, frameSamples);
      sendAudioBytes(float32ToPCM16(f32));
    });
    return true;
  }

  function startSweepTx(sr, frameSamples, startFreq, endFreq, periodSec) {
    const nyquist = Math.max(100, sr / 2 - 100);
    const lo = Math.max(20, Math.min(nyquist, Number.isFinite(startFreq) ? startFreq : 250));
    const hi = Math.max(20, Math.min(nyquist, Number.isFinite(endFreq) ? endFreq : 3200));
    const periodSamples = Math.max(frameSamples, Math.round(Math.max(0.5, periodSec || 4) * sr));
    const minFreq = Math.min(lo, hi);
    const maxFreq = Math.max(lo, hi);
    const ratio = maxFreq > minFreq ? maxFreq / minFreq : 1;
    let phase = 0;
    let sampleIndex = 0;
    activeTxMode = 'sweep';
    setTxEnabled(true);
    startPacedTx(sr, frameSamples, () => {
      const f32 = new Float32Array(frameSamples);
      for (let i = 0; i < frameSamples; i++) {
        const cycle = (sampleIndex % periodSamples) / periodSamples;
        const tri = cycle < 0.5 ? cycle * 2 : 2 - cycle * 2;
        const freq = ratio === 1 ? minFreq : minFreq * Math.pow(ratio, tri);
        f32[i] = 0.2 * Math.sin(phase);
        phase += 2 * Math.PI * freq / sr;
        if (phase > 2 * Math.PI) phase %= 2 * Math.PI;
        sampleIndex++;
      }
      queueVu('tx', f32);
      loopbackTxFrame(f32, sr, frameSamples);
      sendAudioBytes(float32ToPCM16(f32));
    });
    return true;
  }

  function startSilenceTx(sr, frameSamples) {
    activeTxMode = 'silence';
    setTxEnabled(true);
    const zero = new Uint8Array(frameSamples * 2);
    startPacedTx(sr, frameSamples, () => {
      queueVu('tx', null);
      loopbackTxFrame(null, sr, frameSamples);
      sendAudioBytes(zero);
    });
    return true;
  }

  function micUnavailableMessage() {
    if (!window.isSecureContext) {
      return `Mic TX unavailable on ${window.location.origin}: browser requires HTTPS or localhost for microphone capture`;
    }
    return 'Mic TX unavailable: browser does not expose navigator.mediaDevices.getUserMedia';
  }

  function canUseMicTx() {
    return !!(navigator.mediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function');
  }

  function updateMicAvailability() {
    if (canUseMicTx()) return;
    const live = $('audioTabLive');
    if (live) live.title = micUnavailableMessage();
    log(micUnavailableMessage());
  }

  function updateServerAudioControls() {
    const connected = wsIsOpen();
    const supported = !!serverAudio.supported;
    const serverMode = selectedSourceMode() === 'server';

    const refresh = $('btnServerAudioRefresh');
    const capture = $('serverCaptureDevice');
    const playback = $('serverPlaybackDevice');
    const playbackEnable = $('serverPlaybackEnable');
    if (refresh) refresh.disabled = !serverMode || !connected;
    if (capture) capture.disabled = !serverMode || !connected || !supported || !serverAudio.capture.length || txEnabled && activeTxMode === 'server';
    if (playback) playback.disabled = !serverMode || !connected || !supported || !serverAudio.playback.length;
    if (playbackEnable) playbackEnable.disabled = !serverMode || !connected || !supported || !serverAudio.playback.length;
  }

  function setDeviceOptions(select, devices, selectedID) {
    if (!select) return;
    const current = selectedID || select.value || 'default';
    select.textContent = '';
    const list = Array.isArray(devices) ? devices : [];
    if (!list.length) {
      const opt = document.createElement('option');
      opt.value = 'default';
      opt.textContent = 'No devices';
      select.appendChild(opt);
      select.value = 'default';
      return;
    }
    for (const dev of list) {
      const opt = document.createElement('option');
      opt.value = dev.id || 'default';
      opt.textContent = `${dev.name || opt.value}${dev.is_default && opt.value !== 'default' ? ' (default)' : ''}`;
      select.appendChild(opt);
    }
    const hasSelected = Array.from(select.options).some((opt) => opt.value === current);
    select.value = hasSelected ? current : (list[0]?.id || 'default');
  }

  function applyServerAudioMessage(msg) {
    serverAudio = {
      ...serverAudio,
      supported: !!msg.supported,
      backend: msg.backend || serverAudio.backend || 'unknown',
      capture: Array.isArray(msg.capture) ? msg.capture : serverAudio.capture,
      playback: Array.isArray(msg.playback) ? msg.playback : serverAudio.playback,
      selected: msg.selected || serverAudio.selected,
      enabled: msg.enabled || serverAudio.enabled,
      running: msg.running || serverAudio.running,
      tx_source: msg.tx_source || serverAudio.tx_source,
    };
    serverTxActive = !!serverAudio.running.capture || serverAudio.tx_source === 'server' && !!serverAudio.enabled.capture;
    setDeviceOptions($('serverCaptureDevice'), serverAudio.capture, serverAudio.selected?.capture);
    setDeviceOptions($('serverPlaybackDevice'), serverAudio.playback, serverAudio.selected?.playback);
    const playbackEnable = $('serverPlaybackEnable');
    if (playbackEnable) playbackEnable.checked = !!serverAudio.enabled?.playback || !!serverAudio.running?.playback;
    const badge = $('serverAudioBackend');
    if (badge) {
      const backend = serverAudio.backend || 'unknown';
      badge.textContent = serverAudio.supported ? `Server audio ${backend}` : 'Server audio unavailable';
      badge.classList.toggle('ok', !!serverAudio.supported);
      badge.classList.toggle('warn', !serverAudio.supported);
    }
    if (msg.ok === false && selectedSourceMode() === 'server') {
      serverTxActive = false;
      setTxEnabled(false);
    }
    if (msg.error) log('Server audio:', msg.error);
    updateAudioControls();
  }

  function sendServerAudioSelect({ captureEnabled = serverTxActive, playbackEnabled = $('serverPlaybackEnable')?.checked || false, txSource = null } = {}) {
    if (!wsIsOpen()) return;
    const msg = {
      type: 'server_audio_select',
      capture_id: $('serverCaptureDevice')?.value || serverAudio.selected?.capture || 'default',
      playback_id: $('serverPlaybackDevice')?.value || serverAudio.selected?.playback || 'default',
      capture_enabled: !!captureEnabled,
      playback_enabled: !!playbackEnabled,
    };
    if (txSource) msg.tx_source = txSource;
    sendControlJSON(msg);
  }

  function requestServerAudioDevices() {
    if (!wsIsOpen()) return;
    sendControlJSON({ type: 'server_audio_refresh' });
  }

  async function applyAudioSettings() {
    if (txStarting) return false;
    const restartTx = txEnabled;
    commitSelectedAudioSettings();
    try {
      if (ctx) await applyBrowserAudioOutput({ logResult: selectedBrowserOutputID() !== 'default' });
      if (wsIsOpen()) {
        const mode = selectedSourceMode();
        sendServerAudioSelect({
          captureEnabled: restartTx && mode === 'server',
          playbackEnabled: $('serverPlaybackEnable')?.checked || false,
          txSource: mode === 'server' ? 'server' : 'ws',
        });
      }
      if (restartTx) {
        stopTx();
        return await startSelectedTx({ commitSettings: false });
      }
      updateAudioControls();
      return true;
    } catch (err) {
      log('Audio apply failed:', err?.message || err);
      markAudioSettingsDirty();
      updateAudioControls();
      return false;
    }
  }

  async function ensureMicWorklet() {
    if (!ctx.audioWorklet || typeof AudioWorkletNode !== 'function') return false;
    if (micWorkletLoaded) return true;
    const processor = `
class KromaMicCapture extends AudioWorkletProcessor {
  process(inputs) {
    const input = inputs[0] && inputs[0][0];
    if (input && input.length) {
      const copy = new Float32Array(input.length);
      copy.set(input);
      this.port.postMessage(copy, [copy.buffer]);
    }
    return true;
  }
}
registerProcessor('kroma-mic-capture', KromaMicCapture);
`;
    const url = URL.createObjectURL(new Blob([processor], { type: 'application/javascript' }));
    try {
      await ctx.audioWorklet.addModule(url);
      micWorkletLoaded = true;
      return true;
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  function sendMicFrames(input, srOut, frameSamples, q) {
    if (!txEnabled) return;
    const res = resampleF32(input, ctx.sampleRate, srOut);
    q.push(res);
    // flush in frames of frameSamples
    let total = 0; q.forEach(x => total += x.length);
    while (total >= frameSamples) {
      const f = new Float32Array(frameSamples);
      let copied = 0;
      while (copied < frameSamples) {
        const head = q[0];
        const take = Math.min(frameSamples - copied, head.length);
        f.set(head.subarray(0, take), copied);
        copied += take;
        if (take === head.length) { q.shift(); }
        else { q[0] = head.subarray(take); }
      }
      const out = micMuted ? new Float32Array(frameSamples) : f;
      queueVu('tx', out);
      loopbackTxFrame(out, srOut, frameSamples);
      sendAudioBytes(float32ToPCM16(out));
      total -= frameSamples;
    }
  }

  async function startMicTx(srOut, frameSamples) {
    if (!canUseMicTx()) {
      log(micUnavailableMessage());
      setMicEnableNeeded(true, micUnavailableMessage());
      return false;
    }
    if (!ctx) ctx = new (window.AudioContext || window.webkitAudioContext)();
    if (ctx.state === 'suspended') await ctx.resume();
    const inputID = selectedBrowserInputID();
    browserAudio.selected.input = inputID;
    storageSet(browserAudioInputStorageKey, inputID);
    const constraints = inputID && inputID !== 'default' ? { audio: { deviceId: { exact: inputID } } } : { audio: true };
    micStream = await navigator.mediaDevices.getUserMedia(constraints);
    refreshBrowserAudioDevices({ logErrors: false });
    micSource = ctx.createMediaStreamSource(micStream);
    const q = [];

    if (await ensureMicWorklet()) {
      const node = new AudioWorkletNode(ctx, 'kroma-mic-capture', { numberOfInputs: 1, numberOfOutputs: 1 });
      node.port.onmessage = (ev) => sendMicFrames(ev.data, srOut, frameSamples, q);
      micSource.connect(node);
      node.connect(ctx.destination);
      micProcessor = node;
    } else {
      const proc = ctx.createScriptProcessor(1024, 1, 1);
      proc.onaudioprocess = (ev) => sendMicFrames(ev.inputBuffer.getChannelData(0), srOut, frameSamples, q);
      micSource.connect(proc);
      proc.connect(ctx.destination);
      micProcessor = proc;
      log('Mic TX using deprecated ScriptProcessor fallback: AudioWorklet is unavailable');
    }

    activeTxMode = 'mic';
    setMicEnableNeeded(false);
    setTxEnabled(true);
    return true;
  }

  function startServerTx() {
    if (!wsIsOpen() || !serverAudio.supported) {
      log('Server TX unavailable');
      return false;
    }
    serverTxActive = true;
    activeTxMode = 'server';
    sendServerAudioSelect({ captureEnabled: true, txSource: 'server' });
    setTxEnabled(true);
    return true;
  }

  function stopServerTx() {
    if (!serverTxActive) return;
    serverTxActive = false;
    sendServerAudioSelect({ captureEnabled: false, txSource: 'ws' });
  }

  function stopTx() {
    txStarting = false;
    stopPacedTx();
    stopServerTx();
    const stoppedMode = activeTxMode;
    setTxEnabled(false);
    activeTxMode = '';
    if (stoppedMode === 'mic') {
      setMicEnableNeeded(true, 'Microphone is off. Click to enable microphone.');
    }
    if (micProcessor) {
      try { if (micProcessor.port) micProcessor.port.onmessage = null; } catch {}
      try { if (micProcessor.port) micProcessor.port.close(); } catch {}
      try { micProcessor.disconnect(); } catch {}
      micProcessor = null;
    }
    if (micSource) { try { micSource.disconnect(); } catch {} micSource = null; }
    if (micStream) { micStream.getTracks().forEach(t => t.stop()); micStream = null; }
    resetVu('tx');
    updateAudioControls();
  }

  function selectedSourceMode() {
    const tab = selectedAudioTab();
    if (tab === 'server') return 'server';
    if (tab === 'test') return selectedTestSourceMode();
    return 'mic';
  }

  function sendMatrixNamesRequest(source) {
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      if (source !== 'auto') log('GET NAMES skipped: WS not connected');
      return;
    }
    const input = $('matrixAddr');
    const addr = String(input?.value || remoteCfg.matrix_addr || '').trim();
    if (source === 'auto' && addr && lastMatrixNamesAddr === addr && (Date.now() - lastMatrixNamesAttemptAt) < autoMatrixNamesRetryMs) {
      log(`Matrix auto GET NAMES throttled: ${addr}`);
      return;
    }
    if (addr) {
      lastMatrixNamesAddr = addr;
      lastMatrixNamesAttemptAt = Date.now();
    }
    sendControlJSON({type:'matrix_fetch_names', addr});
    const target = remoteCfg.matrix_port ? ` ${remoteCfg.matrix_port}` : '';
    const prefix = source === 'auto' ? 'Matrix auto GET NAMES' : 'Matrix GET NAMES';
    log(`${prefix}${target}: ${addr || '(server default)'}`);
  }

  function maybeAutoFetchMatrixNames() {
    if (autoMatrixNamesFetched || autoMatrixNamesTimer) return;
    const input = $('matrixAddr');
    const addr = String(input?.value || remoteCfg.matrix_addr || '').trim();
    if (!addr) return;
    if (matrixNamesReceived && lastMatrixNamesAddr === addr) return;
    if (lastMatrixNamesAddr === addr && (Date.now() - lastMatrixNamesAttemptAt) < autoMatrixNamesRetryMs) return;
    autoMatrixNamesTimer = setTimeout(() => {
      autoMatrixNamesTimer = null;
      if (autoMatrixNamesFetched) return;
      const currentAddr = String(($('matrixAddr')?.value || remoteCfg.matrix_addr || '')).trim();
      if (!currentAddr) return;
      if (matrixNamesReceived && lastMatrixNamesAddr === currentAddr) return;
      autoMatrixNamesFetched = true;
      sendMatrixNamesRequest('auto');
    }, 250);
  }

  function fetchMatrixNames() {
    if (autoMatrixNamesTimer) {
      clearTimeout(autoMatrixNamesTimer);
      autoMatrixNamesTimer = null;
    }
    autoMatrixNamesFetched = true;
    sendMatrixNamesRequest('manual');
  }

  function sendAudioConfig(sr, fs) {
    const cfg = {
      type: 'config',
      sample_rate: sr,
      channels: 1,
      frame_samples: fs,
    };
    sendControlJSON(cfg);
  }

  async function startSelectedTx({ commitSettings = true } = {}) {
    if (txEnabled) return true;
    if (txStarting) return false;
    if (commitSettings) commitSelectedAudioSettings();
    const sr = parseInt($('sr').value, 10);
    const fs = parseInt($('fs').value, 10);
    const mode = selectedSourceMode();
    txStarting = true;
    updateAudioControls();
    try {
      sendAudioConfig(sr, fs);
      if (mode !== 'server') sendServerAudioSelect({ captureEnabled: false, txSource: 'ws' });
      let started = true;
      if (mode === 'silence') started = startSilenceTx(sr, fs);
      else if (mode === 'tone') started = startToneTx(sr, fs, parseFloat($('toneFreq').value));
      else if (mode === 'sweep') started = startSweepTx(sr, fs, parseFloat($('sweepStartFreq').value), parseFloat($('sweepEndFreq').value), parseFloat($('sweepPeriod').value));
      else if (mode === 'mic') started = await startMicTx(sr, fs);
      else if (mode === 'server') started = startServerTx();
      if (!started) stopTx();
      return !!started;
    } catch (err) {
      stopTx();
      log('TX start failed:', err?.message || err);
      return false;
    } finally {
      txStarting = false;
      updateAudioControls();
    }
  }

  function audioEnableReason(err) {
    const msg = String(err?.message || err || '');
    if (err?.name === 'NotAllowedError' || /permission|allowed|gesture|user activation/i.test(msg)) {
      return 'Click the microphone button to allow microphone access and start panel audio.';
    }
    return 'Audio did not start automatically. Click the microphone button to start microphone and headphone audio.';
  }

  async function microphonePermissionState() {
    if (!canUseMicTx()) return 'unavailable';
    try {
      if (navigator.permissions && typeof navigator.permissions.query === 'function') {
        const status = await navigator.permissions.query({ name: 'microphone' });
        return status?.state || 'unknown';
      }
    } catch {}
    return 'unknown';
  }

  function audioEnablePromptForPermission(state) {
    if (state === 'denied') {
      return 'Microphone access is blocked. Allow microphone access in the browser, then enable panel audio.';
    }
    if (state === 'unavailable') {
      return micUnavailableMessage();
    }
    return 'Click the microphone button to allow microphone access and start RX/TX audio.';
  }

  async function startLiveAudio({ fromUser = false } = {}) {
    if (autoAudioStartInProgress) return false;
    autoAudioStartInProgress = true;
    try {
      if (!wsIsOpen() || !audioWsIsOpen()) {
        if (fromUser) connect();
        const reason = 'Connecting to the bridge. Try again once the panel is connected.';
        setMicEnableNeeded(true, reason);
        if (!fromUser) showAudioEnableOverlay(reason);
        return false;
      }
      setAudioTab('live', { persist: false });
      hideAudioEnableOverlay();
      const rxStarted = rxEnabled || await startRx();
      if (txEnabled && activeTxMode !== 'mic') stopTx();
      const txStarted = txEnabled && activeTxMode === 'mic' || await startSelectedTx();
      if (!rxStarted || !txStarted) {
        const reason = 'Click the microphone button to allow microphone access and start panel audio.';
        setMicEnableNeeded(true, reason);
        if (!fromUser) showAudioEnableOverlay(reason);
        return false;
      }
      hideAudioEnableOverlay();
      return true;
    } catch (err) {
      const reason = audioEnableReason(err);
      setMicEnableNeeded(true, reason);
      if (!fromUser) showAudioEnableOverlay(reason);
      return false;
    } finally {
      autoAudioStartInProgress = false;
      updateAudioControls();
    }
  }

  function maybeAutoStartAudio() {
    if (autoAudioStartAttempted || !wsIsOpen() || !audioWsIsOpen()) return;
    autoAudioStartAttempted = true;
    microphonePermissionState().then((state) => {
      const reason = state === 'granted'
        ? 'Click the microphone button to start panel audio.'
        : audioEnablePromptForPermission(state);
      setMicEnableNeeded(true, reason);
      hideAudioEnableOverlay();
    });
  }

  async function handleMicButtonClick() {
    if (micEnableNeeded || !(txEnabled && activeTxMode === 'mic')) {
      hideAudioEnableOverlay();
      setMicEnableNeeded(true, 'Starting microphone...');
      await startLiveAudio({ fromUser: true });
      return;
    }
    setMicMuted(!micMuted);
  }

  // UI
  $('btnConnect').onclick = connect;
  $('btnDisconnect').onclick = disconnect;
  $('btnFloatingPanel').onclick = openFloatingPanel;
  $('btnStartRx').onclick = startRx; $('btnStopRx').onclick = stopRx;
  $('frontendLoopback').onchange = updateAudioControls;
  $('btnBrowserAudioRefresh').onclick = () => refreshBrowserAudioDevices();
  $('browserCaptureDevice').onchange = () => {
    markAudioSettingsDirty();
    updateAudioControls();
  };
  $('browserPlaybackDevice').onchange = () => {
    markAudioSettingsDirty();
    updateAudioControls();
  };
  $('btnServerAudioRefresh').onclick = requestServerAudioDevices;
  $('serverPlaybackEnable').onchange = () => {
    markAudioSettingsDirty();
    updateAudioControls();
  };
  $('serverCaptureDevice').onchange = () => {
    markAudioSettingsDirty();
    updateAudioControls();
  };
  $('serverPlaybackDevice').onchange = () => {
    markAudioSettingsDirty();
    updateAudioControls();
  };
  document.querySelectorAll('[data-audio-tab]').forEach((button) => {
    button.onclick = () => {
      setAudioTab(button.dataset.audioTab, { persist: false });
      markAudioSettingsDirty();
      updateAudioControls();
    };
  });
  $('testSourceMode').onchange = () => {
    updateTestAudioControls();
    markAudioSettingsDirty();
    updateAudioControls();
  };
  $('btnApplyAudio').onclick = () => applyAudioSettings();
  $('btnStartTx').onclick = startSelectedTx;
  $('btnStopTx').onclick = stopTx;
  $('btnMicMute').onclick = handleMicButtonClick;
  $('btnSpeakerMute').onclick = () => setSpeakerMuted(!speakerMuted);
  $('btnEnableAudio').onclick = () => startLiveAudio({ fromUser: true });
  $('btnAudioNotNow').onclick = hideAudioEnableOverlay;

  $('btnMatrixNames').onclick = fetchMatrixNames;

  // ping timer for latency
  let pingTimer = null;
  function startPings() {
    stopPings();
    pingTimer = setInterval(() => {
      try {
        sendControlJSON({type:'ping', t: performance.now()});
      } catch {}
    }, 2000);
  }
  function stopPings() { if (pingTimer) { clearInterval(pingTimer); pingTimer = null; } }

  $('wsUrl').value = defaultControlWsUrl();
  if ($('audioWsUrl')) $('audioWsUrl').value = defaultAudioWsUrl();
  if ($('testSourceMode')) {
    const savedTestSource = storageGet(testSourceStorageKey, 'tone');
    $('testSourceMode').value = ['tone', 'sweep', 'silence'].includes(savedTestSource) ? savedTestSource : 'tone';
  }
  setAudioTab('live', { persist: false });
  syncAppliedAudioSettings();
  updateMicAvailability();
  refreshBrowserAudioDevices({ logErrors: false });
  if (navigator.mediaDevices && typeof navigator.mediaDevices.addEventListener === 'function') {
    navigator.mediaDevices.addEventListener('devicechange', () => refreshBrowserAudioDevices({ logErrors: false }));
  }
  updateAudioControls();
  updateFloatingButton();
  resetVu();
  updatePanelHeader(profileModel, panel.n);
  renderPanel(true);
  connect();
})();
