// ==UserScript==
// @name         Ronin Agent
// @namespace    ronin.agent
// @version      3.8.0
// @description  Full-featured bash agent overlay with per-chat on/off toggle
// @match        *://chat.deepseek.com/*
// @match        *://chatgpt.com/*
// @match        *://chat.openai.com/*
// @match        *://claude.ai/*
// @match        *://gemini.google.com/*
// @grant        GM_xmlhttpRequest
// @grant        GM_setValue
// @grant        GM_getValue
// @grant        GM_deleteValue
// @grant        unsafeWindow
// @connect      *
// @run-at       document-start
// ==/UserScript==

(function () {
  'use strict';

  const pageWin = (typeof unsafeWindow !== 'undefined') ? unsafeWindow : window;
  const pageDoc = pageWin.document;
  const nativeFetch = pageWin.fetch.bind(pageWin);

  // ============ CONFIG ============
  const cfg = {
    serverUrl: GM_getValue('serverUrl', 'http://localhost:8080'),
    token:     GM_getValue('token', ''),
    projectPath: GM_getValue('projectPath', ''),
    autoSend:  GM_getValue('autoSend', true),
    sendDelay: GM_getValue('sendDelay', 3500),
    confirmDangerous: GM_getValue('confirmDangerous', true),
    confirmAll: GM_getValue('confirmAll', false),
    readOnly: GM_getValue('readOnly', false),
    skipNgrokWarning: GM_getValue('skipNgrokWarning', true),
    debugEnabled: GM_getValue('debugEnabled', false),
    maxOutputSize: 1024 * 1024,
    maxBlocksPerReply: 6,
  };

  const MAX_HISTORY = 100;

  function historyKeyFor(chatId) { return 'ronin_history_' + (chatId || 'default'); }
  function tokenKeyFor(chatId) { return 'ronin_tokens_' + (chatId || 'default'); }
  function enabledKeyFor(chatId) { return 'ronin_enabled_' + (chatId || 'default'); }

  // ============ STATE ============
  const state = {
    running: false,
    currentCommand: '',
    outputBuffer: '',
    outputTruncated: false,
    history: [],
    seen: new Set(),
    debugEnabled: cfg.debugEnabled,
    enabled: true,   // per-chat on/off
    tokenUsage: {
      chatTotal: 0, lastResponse: null,
      input: null, output: null, cacheHit: null, cacheMiss: null,
      lastUpdated: 0, source: null,
    },
    currentStreamMax: 0,
    currentTurnUsage: null,
    _assembledBuffer: '',
    currentFragmentType: null,
    currentCwd: cfg.projectPath || '',
    currentChatId: null,
    batchApproved: false,
  };

  // ============ TOKEN FORMATTING ============
  function formatTokens(n) {
    if (n == null || isNaN(n)) return '—';
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + 'M';
    if (n >= 10_000) return (n / 1000).toFixed(1) + 'K';
    if (n >= 1000) return (n / 1000).toFixed(2) + 'K';
    return String(n);
  }

  function extractUsage(obj) {
    if (!obj || typeof obj !== 'object') return null;
    if (obj.v && obj.v.response && 'accumulated_token_usage' in obj.v.response) {
      const n = obj.v.response.accumulated_token_usage;
      if (n != null) return { source: 'deepseek-web', total: n };
    }
    if (obj.p === 'response' && obj.o === 'BATCH' && Array.isArray(obj.v)) {
      for (const item of obj.v) {
        if (item && item.p === 'accumulated_token_usage' && item.v != null) {
          return { source: 'deepseek-web', total: item.v };
        }
      }
    }
    if (obj.accumulated_token_usage != null) {
      const u = obj.accumulated_token_usage;
      return { source: 'deepseek', input: u.prompt_tokens ?? null,
        output: u.completion_tokens ?? null, total: u.total_tokens ?? null,
        cacheHit: u.prompt_cache_hit_tokens ?? null, cacheMiss: u.prompt_cache_miss_tokens ?? null };
    }
    if (obj.token_usage != null) {
      const u = obj.token_usage;
      return { source: 'deepseek-api', input: u.prompt_tokens ?? null,
        output: u.completion_tokens ?? null, total: u.total_tokens ?? null,
        cacheHit: u.prompt_cache_hit_tokens ?? null, cacheMiss: u.prompt_cache_miss_tokens ?? null };
    }
    if ((obj.type === 'message_delta' || obj.type === 'message_start') &&
        (obj.usage || (obj.message && obj.message.usage))) {
      const u = obj.usage || obj.message.usage;
      const inputTotal = (u.input_tokens || 0) +
        (u.cache_creation_input_tokens || 0) + (u.cache_read_input_tokens || 0);
      return { source: 'claude', input: inputTotal || u.input_tokens || null,
        output: u.output_tokens ?? null,
        total: inputTotal + (u.output_tokens || 0) || null,
        cacheHit: u.cache_read_input_tokens ?? null,
        cacheMiss: u.cache_creation_input_tokens ?? null };
    }
    if (obj.usage && obj.usage.prompt_tokens != null) {
      return { source: 'openai', input: obj.usage.prompt_tokens ?? null,
        output: obj.usage.completion_tokens ?? null,
        total: obj.usage.total_tokens ?? null,
        cacheHit: obj.usage.prompt_tokens_details?.cached_tokens ?? null, cacheMiss: null };
    }
    return null;
  }

  function mergeUsage(usage) {
    if (!usage) return;
    if (usage.total != null) {
      state.currentStreamMax = Math.max(state.currentStreamMax, usage.total);
      state.tokenUsage.lastResponse = state.currentStreamMax;
      state.tokenUsage.lastUpdated = Date.now();
      overlay.renderTokenUsage(state.tokenUsage);
    }
    if (usage.input != null) state.tokenUsage.input = usage.input;
    if (usage.output != null) state.tokenUsage.output = usage.output;
    if (usage.cacheHit != null) state.tokenUsage.cacheHit = usage.cacheHit;
    if (usage.cacheMiss != null) state.tokenUsage.cacheMiss = usage.cacheMiss;
    if (usage.source) state.tokenUsage.source = usage.source;
  }

  function finalizeStreamUsage() {
    if (state.currentStreamMax > 0) {
      state.tokenUsage.chatTotal = (state.tokenUsage.chatTotal || 0) + state.currentStreamMax;
      state.tokenUsage.lastResponse = state.currentStreamMax;
      state.tokenUsage.lastUpdated = Date.now();
      saveChatTokens();
    }
    state.currentStreamMax = 0;
    overlay.renderTokenUsage(state.tokenUsage);
  }

  function loadTokensFor(chatId) {
    const raw = GM_getValue(tokenKeyFor(chatId), null);
    if (!raw) return null;
    try { return JSON.parse(raw); } catch (_) { return null; }
  }
  function saveChatTokens() {
    if (!state.currentChatId) return;
    GM_setValue(tokenKeyFor(state.currentChatId), JSON.stringify(state.tokenUsage));
  }
  function resetTokenUsage() {
    state.tokenUsage = {
      chatTotal: 0, lastResponse: null, input: null, output: null,
      cacheHit: null, cacheMiss: null, lastUpdated: 0, source: null,
    };
    state.currentStreamMax = 0;
    if (state.currentChatId) GM_deleteValue(tokenKeyFor(state.currentChatId));
    overlay.renderTokenUsage(state.tokenUsage);
  }

  // ============ PER-CHAT ENABLED ============
  function loadEnabledFor(chatId) {
    const raw = GM_getValue(enabledKeyFor(chatId), null);
    if (raw === null) return true;  // default ON
    return raw === 'true';
  }
  function saveEnabled() {
    if (!state.currentChatId) return;
    GM_setValue(enabledKeyFor(state.currentChatId), state.enabled ? 'true' : 'false');
  }
  function setEnabled(on) {
    state.enabled = !!on;
    saveEnabled();
    if (typeof overlay !== 'undefined' && overlay.syncEnabled) {
      overlay.syncEnabled();
    }
    if (state.enabled && typeof overlay !== 'undefined') {
      overlay.setStatus('enabled');
    } else if (typeof overlay !== 'undefined') {
      overlay.setStatus('disabled for this chat');
    }
  }
  function toggleEnabled() { setEnabled(!state.enabled); }

  // ============ PER-CHAT HISTORY ============
  function loadHistoryFor(chatId) {
    const raw = GM_getValue(historyKeyFor(chatId), '[]');
    try {
      const p = JSON.parse(raw);
      return Array.isArray(p) ? p : [];
    } catch (_) { return []; }
  }
  function saveHistory() {
    if (!state.currentChatId) return;
    if (state.history.length > MAX_HISTORY) state.history = state.history.slice(0, MAX_HISTORY);
    GM_setValue(historyKeyFor(state.currentChatId), JSON.stringify(state.history));
  }
  function clearHistoryForCurrentChat() {
    state.history = [];
    if (state.currentChatId) GM_deleteValue(historyKeyFor(state.currentChatId));
    overlay.renderHistory();
  }

  // ============ CHAT ID DETECTION ============
  function detectChatId() {
    const path = pageWin.location.pathname || '';
    const m = path.match(/\/(?:a\/)?chat\/s\/([^\/?#]+)/);
    if (m) return m[1];
    return path || 'default';
  }
  function checkChatChange() {
    const id = detectChatId();
    if (id !== state.currentChatId) {
      state.currentChatId = id;
      state.history = loadHistoryFor(id);
      state.enabled = loadEnabledFor(id);
      state.seen.clear();
      state.currentStreamMax = 0;
      const saved = loadTokensFor(id);
      state.tokenUsage = saved ? Object.assign({
        chatTotal: 0, lastResponse: null, input: null, output: null,
        cacheHit: null, cacheMiss: null, lastUpdated: 0, source: null,
      }, saved) : {
        chatTotal: 0, lastResponse: null, input: null, output: null,
        cacheHit: null, cacheMiss: null, lastUpdated: 0, source: null,
      };
      if (typeof overlay !== 'undefined') {
        if (overlay.renderHistory) overlay.renderHistory();
        if (overlay.renderTokenUsage) overlay.renderTokenUsage(state.tokenUsage);
        if (overlay.syncEnabled) overlay.syncEnabled();
      }
    }
  }
  try {
    const origPush = pageWin.history.pushState;
    const origReplace = pageWin.history.replaceState;
    pageWin.history.pushState = function (...a) { const r = origPush.apply(this, a); checkChatChange(); return r; };
    pageWin.history.replaceState = function (...a) { const r = origReplace.apply(this, a); checkChatChange(); return r; };
    pageWin.addEventListener('popstate', checkChatChange);
  } catch (_) {}

  // ============ DANGEROUS PATTERN MIRROR ============
  const DANGEROUS_PATTERNS = [
    /\b(sudo|su|doas)\b/,
    /\b(apt|apt-get|aptitude|yum|dnf|pacman|zypper|brew|portage|emerge|pkg)\s+(install|remove|purge|update|upgrade|autoremove|dist-upgrade|full-upgrade|reinstall)\b/,
    /\b(pip|pip3)\s+(install|uninstall)\b/,
    /\bnpm\s+(install|i|uninstall)\s+(-g|--global)\b/,
    /\byarn\s+global\b/,
    /\bpnpm\s+(add|install|remove)\s+(-g|--global)\b/,
    /\bcargo\s+(install|publish)\b/,
    /\bgem\s+install\b/,
    /\bgo\s+install\b/,
    /\bcomposer\s+global\b/,
    /\bsystemctl\s+(start|stop|restart|reload|enable|disable|mask|unmask|daemon-reload)\b/,
    /\bservice\s+\S+\s+(start|stop|restart|reload)\b/,
    /\bdocker\s+(run|exec|rm|rmi|stop|kill|prune|system|volume|network)\b/,
    /\bpodman\s+(run|exec|rm|rmi|stop|kill|prune)\b/,
    /\bkubectl\s+(apply|delete|create|edit|patch|replace|scale|rollout)\b/,
    /\b(useradd|userdel|usermod|groupadd|groupdel|gpasswd|passwd|chsh|chfn)\b/,
    /\b(iptables|ip6tables|nft|ufw|firewall-cmd)\b/,
    /\b(modprobe|insmod|rmmod|sysctl)\b/,
    /\b(mount|umount|fdisk|parted|losetup)\b/,
    /\bcrontab\b/,
    /\bssh-keygen\b/,
    /\b(curl|wget)\b[^|]*\|\s*(ba|z|fi)?sh\b/,
    /\b(killall|pkill)\s+-9\b/,
    /\bgit\s+push\s+(--force|-f)\b/,
    /\bgit\s+reset\s+--hard\b/,
    /\bgit\s+clean\s+-[a-zA-Z]*[fF]/,
    /\bgit\s+filter-branch\b/,
    /\bnpm\s+publish\b/,
    /\brm\s+(-[a-zA-Z]*f[a-zA-Z]*\s+)?\//,
    /\brm\s+-rf\s+[~\/]/,
  ];
  function isDangerous(cmd) { return DANGEROUS_PATTERNS.some(re => re.test(cmd)); }

  // ============ DELTA PARSING ============
  function extractDelta(obj) {
    if (!obj || typeof obj !== 'object') return '';
    if (obj.v && obj.v.response && Array.isArray(obj.v.response.fragments)) {
      let out = '';
      for (const f of obj.v.response.fragments) {
        if (!f) continue;
        state.currentFragmentType = f.type || null;
        if (f.type === 'RESPONSE' && typeof f.content === 'string') out += f.content;
      }
      return out;
    }
    if (obj.p === 'response/fragments' && obj.o === 'APPEND' && Array.isArray(obj.v)) {
      let out = '';
      for (const f of obj.v) {
        if (!f) continue;
        state.currentFragmentType = f.type || null;
        if (f.type === 'RESPONSE' && typeof f.content === 'string') out += f.content;
      }
      return out;
    }
    if (typeof obj.p === 'string' && obj.p.endsWith('/content') &&
        (typeof obj.v === 'string' || typeof obj.v === 'number')) {
      if (state.currentFragmentType === 'RESPONSE') return String(obj.v);
      return '';
    }
    if (!obj.p && typeof obj.v === 'string') {
      if (state.currentFragmentType === 'RESPONSE') return obj.v;
      return '';
    }
    if (obj?.choices?.[0]) {
      const c = obj.choices[0];
      return c.delta?.content || c.message?.content || '';
    }
    if (obj?.type === 'content_block_delta' && obj.delta?.text) return obj.delta.text;
    if (obj?.v?.content) return obj.v.content;
    if (typeof obj?.text === 'string') return obj.text;
    return '';
  }

  function feedSSEChunk(text) {
    const lines = text.split('\n');
    for (const line of lines) {
      if (state.debugEnabled && line.trim()) overlay.appendDebug(line + '\n');
      if (!line.startsWith('data:')) continue;
      const payload = line.slice(5).trim();
      if (!payload || payload === '[DONE]') continue;
      try {
        const obj = JSON.parse(payload);
        const usage = extractUsage(obj);
        if (usage) { state.currentTurnUsage = usage; mergeUsage(usage); }
        const delta = extractDelta(obj);
        if (delta) state._assembledBuffer = (state._assembledBuffer || '') + delta;
      } catch (_) {}
    }
  }

  function resetStreamState() {
    state._assembledBuffer = '';
    state.currentFragmentType = null;
    state.currentTurnUsage = null;
    state.currentStreamMax = 0;
  }
  function flushAssistant() {
    if (state._assembledBuffer && state._assembledBuffer.trim()) {
      onAssistantMessage(state._assembledBuffer.trim());
    }
    state._assembledBuffer = '';
  }

  // ============ FETCH / XHR HOOKS (for AI site) ============
  // When state.enabled is false, we skip hooking entirely and let the
  // page's own network calls flow through untouched.
  try {
    const origFetch = pageWin.fetch;
    pageWin.fetch = async function (...args) {
      if (!state.enabled) {
        return origFetch.apply(this, args);
      }
      const url = (args[0] && args[0].url) || args[0] || '';
      if (state.debugEnabled && /chat|message|conversation|completion|stream/i.test(String(url))) {
        try { overlay.appendDebug('[fetch] ' + String(url).slice(0, 160) + '\n'); } catch (_) {}
      }
      const resp = await origFetch.apply(this, args);
      const ct = resp.headers.get('content-type') || '';
      if (state.debugEnabled) { try { overlay.appendDebug('[fetch ct] ' + ct + '\n'); } catch (_) {} }
      if ((ct.includes('event-stream') || ct.includes('ndjson') || ct.includes('stream')) && resp.body) {
        const [a, b] = resp.body.tee();
        consumeFetchStream(a);
        return new Response(b, { status: resp.status, statusText: resp.statusText, headers: resp.headers });
      }
      return resp;
    };
  } catch (_) {}

  async function consumeFetchStream(stream) {
    resetStreamState();
    const reader = stream.getReader();
    const decoder = new TextDecoder('utf-8');
    let buf = '';
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const idx = buf.lastIndexOf('\n');
        if (idx === -1) continue;
        feedSSEChunk(buf.slice(0, idx));
        buf = buf.slice(idx + 1);
      }
      if (buf) feedSSEChunk(buf);
    } catch (_) {}
    finalizeStreamUsage();
    flushAssistant();
  }

  try {
    const XHR = pageWin.XMLHttpRequest;
    const origOpen = XHR.prototype.open;
    const origSend = XHR.prototype.send;
    XHR.prototype.open = function (method, url, ...rest) { this.__ckUrl = url; return origOpen.call(this, method, url, ...rest); };
    XHR.prototype.send = function (...args) {
      if (!state.enabled) {
        return origSend.apply(this, args);
      }
      this.__ckBuf = ''; this.__ckPrev = ''; this.__ckLoggedCt = false; this.__ckStarted = false;
      this.addEventListener('progress', () => {
        const ct = (this.getResponseHeader && this.getResponseHeader('content-type')) || '';
        if (state.debugEnabled && !this.__ckLoggedCt) { try { overlay.appendDebug('[xhr ct] ' + ct + '\n'); } catch (_) {} this.__ckLoggedCt = true; }
        if (!ct.includes('event-stream') && !ct.includes('stream') && !ct.includes('ndjson')) return;
        if (!this.__ckStarted) { this.__ckStarted = true; resetStreamState(); }
        const text = this.responseText || '';
        const prev = this.__ckPrev || '';
        if (text.length > prev.length) {
          this.__ckPrev = text;
          this.__ckBuf = (this.__ckBuf || '') + text.slice(prev.length);
          const idx = this.__ckBuf.lastIndexOf('\n');
          if (idx !== -1) { feedSSEChunk(this.__ckBuf.slice(0, idx)); this.__ckBuf = this.__ckBuf.slice(idx + 1); }
        }
      });
      this.addEventListener('load', () => {
        const ct = (this.getResponseHeader && this.getResponseHeader('content-type')) || '';
        if (ct.includes('event-stream') || ct.includes('stream') || ct.includes('ndjson')) {
          if (this.__ckBuf) { feedSSEChunk(this.__ckBuf); this.__ckBuf = ''; }
          finalizeStreamUsage();
          flushAssistant();
        }
      });
      return origSend.apply(this, args);
    };
  } catch (_) {}

  // ============ BASH EXTRACTION ============
  const bashRe = /```(?:bash|sh)\s*\n([\s\S]*?)```/g;
  function extractBash(text) {
    const out = [];
    for (const m of text.matchAll(bashRe)) { const b = m[1].trim(); if (b) out.push(b); }
    return out;
  }

  // ============ EXECUTION ============
  function buildHeaders(approval) {
    const h = { 'Content-Type': 'application/json' };
    if (cfg.skipNgrokWarning) h['ngrok-skip-browser-warning'] = 'true';
    if (cfg.token) h['Authorization'] = 'Bearer ' + cfg.token;
    if (state.batchApproved) h['X-Ronin-Confirm'] = 'yes';
    if (cfg.readOnly) h['X-Ronin-Read-Only'] = 'yes';
    if (approval && approval.name && approval.value) h[approval.name] = approval.value;
    return h;
  }

  async function execOnServer(command, onChunk, onCwd, onExit, onBlocked, onError, approval) {
    const body = { command };
    if (state.currentCwd) body.root = state.currentCwd;
    const url = cfg.serverUrl.replace(/\/$/, '') + '/exec';
    const headers = buildHeaders(approval);
    const payload = JSON.stringify(body);

    try {
      const resp = await nativeFetch(url, { method: 'POST', headers, body: payload });
      if (!resp.ok) { onError('http ' + resp.status + ' ' + (resp.statusText || '')); return; }
      if (!resp.body) { onError('no response body'); return; }
      await streamFetchResponse(resp, onChunk, onCwd, onExit, onBlocked, onError);
      return;
    } catch (e) {
      if (state.debugEnabled) overlay.appendDebug('[fetch failed: ' + ((e && e.message) || 'unknown') + ']\n');
    }
    execViaGM(url, headers, payload, onChunk, onCwd, onExit, onBlocked, onError);
  }

  async function streamFetchResponse(resp, onChunk, onCwd, onExit, onBlocked, onError) {
    const reader = resp.body.getReader();
    const decoder = new TextDecoder('utf-8');
    let buf = '';
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const idx = buf.lastIndexOf('\n');
        if (idx === -1) continue;
        parseNDJSON(buf.slice(0, idx), onChunk, onCwd, onExit, onBlocked, onError);
        buf = buf.slice(idx + 1);
      }
      if (buf) parseNDJSON(buf, onChunk, onCwd, onExit, onBlocked, onError);
    } catch (e) { onError((e && e.message) || 'stream error'); }
  }

  function execViaGM(url, headers, payload, onChunk, onCwd, onExit, onBlocked, onError) {
    let prevLen = 0;
    let emittedExit = false;
    const handleChunk = (text) => {
      parseNDJSON(text, onChunk, onCwd,
        (code) => { if (!emittedExit) { emittedExit = true; onExit(code); } },
        onBlocked, onError);
    };
    GM_xmlhttpRequest({
      method: 'POST', url, headers, data: payload, responseType: 'text',
      onprogress: (e) => {
        const text = e.responseText || '';
        if (text.length > prevLen) { handleChunk(text.slice(prevLen)); prevLen = text.length; }
      },
      onload: (e) => {
        const text = e.responseText || '';
        if (text.length > prevLen) handleChunk(text.slice(prevLen));
        if (!emittedExit) onExit(-1);
      },
      onerror: (e) => onError((e && e.error) || 'network error'),
      ontimeout: () => onError('timeout'),
    });
  }

  function parseNDJSON(chunk, onChunk, onCwd, onExit, onBlocked, onError) {
    for (const line of chunk.split('\n')) {
      if (!line.trim()) continue;
      let m;
      try { m = JSON.parse(line); } catch { continue; }
      if (m.type === 'output') onChunk(m.data || '');
      else if (m.type === 'cwd') onCwd((m.data || '').trim());
      else if (m.type === 'exit') onExit(m.code ?? 0);
      else if (m.type === 'error') onError(m.data || 'error');
      else if (m.type === 'blocked') onBlocked('BLOCKED', m.data || '');
      else if (m.type === 'confirm_required') onBlocked('CONFIRM', m.data || '');
    }
  }

  async function testConnection() {
    overlay.setStatus('testing...');
    const url = cfg.serverUrl.replace(/\/$/, '') + '/health';
    try {
      const resp = await nativeFetch(url, { method: 'GET', headers: buildHeaders() });
      if (resp.ok) { overlay.setStatus('connected ✓'); return; }
      overlay.setStatus('http ' + resp.status); return;
    } catch (_) {}
    GM_xmlhttpRequest({
      method: 'GET', url, headers: buildHeaders(),
      onload: (e) => { if (e.status === 200) overlay.setStatus('connected ✓'); else overlay.setStatus('http ' + e.status); },
      onerror: () => overlay.setStatus('connection failed'),
    });
  }

  // ============ ORCHESTRATION ============
  async function onAssistantMessage(text) {
    if (!state.enabled) return;
    if (state.running) return;
    const hash = text.length + ':' + text.slice(-64);
    if (state.seen.has(hash)) return;
    state.seen.add(hash);
    if (state.seen.size > 500) { const it = state.seen.values(); for (let i = 0; i < 100; i++) { const v = it.next(); if (v.done) break; state.seen.delete(v.value); } }

    const cmds = extractBash(text);
    if (!cmds.length) return;

    const capped = cmds.slice(0, cfg.maxBlocksPerReply);
    if (cmds.length > capped.length) {
      overlay.appendOutput(`\n[only first ${capped.length} of ${cmds.length} blocks will run]\n`);
    }

    state.batchApproved = false;
    const dangerous = capped.filter(isDangerous);
    const needsApproval = (cfg.confirmAll && capped.length > 0) ||
                          (cfg.confirmDangerous && dangerous.length > 0);

    if (needsApproval) {
      const title = cfg.confirmAll
        ? `Run ${capped.length} block(s)?`
        : `⚠️ ${dangerous.length} sensitive command(s) detected`;
      const body = cfg.confirmAll
        ? capped.map((c, i) => `[${i + 1}] ${c}`).join('\n\n')
        : dangerous.join('\n\n---\n\n');
      const confirmed = await overlay.showConfirmDialog(title, body);
      if (!confirmed) { overlay.appendOutput('\n[skipped by user]\n'); return; }
    }
    state.batchApproved = true;

    state.running = true;
    state.outputBuffer = '';
    state.outputTruncated = false;

    const results = [];
    try {
      for (let i = 0; i < capped.length; i++) {
        const command = capped[i];
        overlay.setCommand(`[${i + 1}/${capped.length}] $ ${command}`);
        overlay.setStatus(`running block ${i + 1}/${capped.length}`);
        overlay.timerStart(`running block ${i + 1}/${capped.length}`);
        overlay.appendOutput(`\n=== Block ${i + 1} ===\n$ ${command}\n`);

        const result = await runOneBlock(command);
        const elapsed = overlay.timerStop(false);
        if (elapsed > 1000) overlay.appendOutput(`[took ${(elapsed / 1000).toFixed(1)}s]\n`);
        results.push(result);

        if (state.outputTruncated) { overlay.appendOutput('\n[output truncated at 1MB total]\n'); break; }
      }
    } finally {
      state.running = false;
      state.batchApproved = false;
      if (overlay.timerRunning && overlay.timerRunning()) overlay.timerStop(false);
    }

    const okCount = results.filter(r => r.code === 0).length;
    overlay.setStatus(`done ${okCount}/${results.length} ok`);

    const resultBlock = formatResultsForAI(results);
    state.history.unshift({
      command: capped.join('\n\n---\n\n'),
      output: results.map(r => r.output).join('\n---\n'),
      code: results.every(r => r.code === 0) ? 0 : 1,
      timestamp: Date.now(),
      usage: state.currentTurnUsage,
      root: state.currentCwd,
      blocks: results.length,
      chatId: state.currentChatId,
    });
    if (state.history.length > MAX_HISTORY) state.history.pop();
    saveHistory();

    if (cfg.autoSend) {
      const base = Math.max(0, cfg.sendDelay | 0);
      const jitter = Math.floor(base * 0.25 * Math.random());
      const wait = base + jitter;
      if (wait > 0) {
        overlay.setStatus("waiting " + (wait / 1000).toFixed(1) + "s before sending...");
        await new Promise(r => setTimeout(r, wait));
      }
      typeAndSend(resultBlock);
    }
  }

  function runOneBlockOnce(command, approval) {
    return new Promise((resolve) => {
      let localOutput = '';
      let truncated = false;
      execOnServer(
        command,
        (out) => {
          if (state.outputBuffer.length + out.length > cfg.maxOutputSize) {
            truncated = true; state.outputTruncated = true; return;
          }
          localOutput += out;
          state.outputBuffer += out;
          overlay.appendOutput(out);
        },
        (newCwd) => {
          if (newCwd && newCwd !== state.currentCwd) {
            state.currentCwd = newCwd;
            overlay.syncPathInput();
          }
        },
        (code) => resolve({ command, output: localOutput, code, truncated }),
        (kind, reason) => {
          const tag = kind === 'BLOCKED' ? '[BLOCKED BY SERVER] ' : '[SERVER WANTS CONFIRMATION] ';
          localOutput += '\n' + tag + reason;
          overlay.appendOutput('\n' + tag + reason + '\n');
          resolve({ command, output: localOutput, code: kind === 'BLOCKED' ? 126 : 125, truncated, reason });
        },
        (err) => {
          localOutput += '\n[error] ' + err + '\n';
          overlay.appendOutput('\n[error] ' + err + '\n');
          resolve({ command, output: localOutput, code: -1, truncated });
        },
        approval
      );
    });
  }

  // Parse the "[ronin-mcp-confirm] server=.. tool=.. approval=.." marker
  // that the ronin CLI emits (on stderr, so it arrives in output) when an
  // MCP tool requires confirmation.
  function extractApproval(result) {
    if (result.code !== 125 || !result.output) return null;
    let m = result.output.match(/\[ronin-mcp-confirm\]\s+server=(\S+)\s+tool=(\S+)\s+approval=(\S+)/);
    if (m) return {
      title: 'Approve MCP tool call?',
      body: 'Server: ' + m[1] + '\nTool: ' + m[2] + '\n\nApproval key:\n' + m[3],
      approval: { name: 'X-Ronin-MCP-Approval', value: m[3] },
    };
    m = result.output.match(/\[ronin-gh-confirm\]\s+action=(\S+)\s+approval=(\S+)/);
    if (m) return {
      title: 'Approve GitHub action?',
      body: 'Action: ' + m[1] + '\n\nApproval key:\n' + m[2],
      approval: { name: 'X-Ronin-GH-Approval', value: m[2] },
    };
    return null;
  }

  async function runOneBlock(command) {
    let result = await runOneBlockOnce(command);
    for (let guard = 0; guard < 3; guard++) {
      const req = extractApproval(result);
      if (!req) break;
      const ok = await overlay.showConfirmDialog(
        req.title,
        req.body + '\n\nApproving re-runs this block once with a signed approval header.'
      );
      if (!ok) {
        overlay.appendOutput('\n[call skipped by user]\n');
        return { command, output: result.output + '\n[call skipped by user]', code: 125, truncated: result.truncated };
      }
      result = await runOneBlockOnce(command, req.approval);
    }
    return result;
  }
  function formatResultsForAI(results) {
    const parts = [];
    for (let i = 0; i < results.length; i++) {
      const r = results[i];
      parts.push(
        `=== Block ${i + 1} (exit ${r.code}) ===\n` +
        'Command:\n```bash\n' + r.command + '\n```\n\n' +
        'Output:\n```\n' + r.output + '\n```' +
        (r.truncated ? '\n[truncated]' : '')
      );
    }
    return parts.join('\n\n');
  }

  // ============ CHAT INPUT ============
  function findChatInput() {
    return pageDoc.querySelector('textarea:not([readonly])')
        || pageDoc.querySelector('div[contenteditable="true"]')
        || pageDoc.querySelector('div[role="textbox"]');
  }
  function findSendButton() {
    return pageDoc.querySelector('button[type="submit"]')
        || pageDoc.querySelector('button[aria-label*="send" i]')
        || pageDoc.querySelector('button[data-testid*="send" i]')
        || pageDoc.querySelector('button[aria-label*="submit" i]');
  }
  function typeAndSend(text) {
    const input = findChatInput();
    if (!input) { overlay.setStatus('chat input not found'); return; }
    input.focus();
    if (input.tagName === 'TEXTAREA' || input.tagName === 'INPUT') {
      const proto = input.tagName === 'TEXTAREA'
        ? pageWin.HTMLTextAreaElement.prototype : pageWin.HTMLInputElement.prototype;
      const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
      setter.call(input, text);
      input.dispatchEvent(new Event('input', { bubbles: true }));
    } else {
      pageDoc.execCommand('insertText', false, text);
      input.dispatchEvent(new InputEvent('input', { bubbles: true }));
    }
    setTimeout(() => {
      const btn = findSendButton();
      if (btn && !btn.disabled) btn.click();
      else input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    }, 100);
  }

  // ============ OVERLAY ============
  const overlay = (() => {
    const host = pageDoc.createElement('div');
    host.id = 'ronin-root';
    host.style.cssText = 'all: initial; position: fixed; z-index: 2147483647; inset: auto 20px 20px auto;';
    (pageDoc.documentElement || pageDoc.body).appendChild(host);
    const root = host.attachShadow({ mode: 'open' });

    root.innerHTML = `
      <style>
        :host { all: initial; }
        .fab { width: 48px; height: 48px; border-radius: 50%;
          background: #1f6feb; color: white; font-size: 22px;
          display: flex; align-items: center; justify-content: center;
          cursor: pointer; box-shadow: 0 4px 14px rgba(0,0,0,.3); user-select: none;
          transition: background 0.2s, opacity 0.2s; }
        .fab.off { background: #444; opacity: 0.55; }
        .panel { position: fixed; top: 20px; right: 20px; bottom: 20px;
          width: 460px; background: #1e1e1e; color: #d4d4d4;
          border-radius: 8px; display: none; flex-direction: column;
          font: 12px/1.5 ui-monospace, Menlo, Consolas, monospace;
          box-shadow: 0 8px 32px rgba(0,0,0,.5); overflow: hidden; }
        .panel.open { display: flex; }
        .head { display: flex; align-items: center; padding: 8px 10px;
          background: #252526; border-bottom: 1px solid #333; gap: 6px; flex-wrap: wrap; }
        .head .title { font-weight: 600; color: #fff; }
        .head .spacer { flex: 1; }
        .head button { background: #333; color: #ddd; border: 0;
          border-radius: 4px; padding: 3px 8px; cursor: pointer; font-size: 11px; }
        .head button:hover { background: #444; }
        .head button.accent { background: #1f6feb; color: #fff; }
        .head button.accent:hover { background: #388bfd; }
        .powerpill { padding: 3px 10px; border-radius: 12px; cursor: pointer;
          font-weight: 700; font-size: 10px; letter-spacing: 0.5px;
          user-select: none; border: 1px solid transparent;
          transition: background 0.15s, color 0.15s; }
        .powerpill.on { background: #173a2b; color: #4ec9b0; border-color: #2d5a43; }
        .powerpill.on:hover { background: #1e4a37; }
        .powerpill.off { background: #3a1e1e; color: #f48771; border-color: #5a2d2d; }
        .powerpill.off:hover { background: #4a2525; }
        .pathbar { display: flex; align-items: center; gap: 6px;
          padding: 6px 10px; background: #1a1a1a;
          border-bottom: 1px solid #333; font-size: 11px; }
        .pathbar label { color: #666; flex-shrink: 0; }
        .pathbar input { flex: 1; background: #252526; color: #4ec9b0;
          border: 1px solid #333; border-radius: 4px; padding: 3px 6px;
          font: inherit; font-size: 11px; min-width: 0; }
        .pathbar input:focus { outline: none; border-color: #1f6feb; }
        .pathbar input.empty { color: #666; font-style: italic; }
        .pathbar button { background: #333; color: #ddd; border: 0;
          border-radius: 4px; padding: 3px 8px; cursor: pointer;
          font-size: 11px; flex-shrink: 0; }
        .pathbar button:hover { background: #444; }
        .pathbar button.save { background: #1f6feb; color: #fff; }
        .pathbar button.save:hover { background: #388bfd; }
        .secbadge { padding: 1px 6px; border-radius: 3px; font-size: 10px; font-weight: 600; }
        .secbadge.open { background: #5a2d2d; color: #f48771; }
        .secbadge.confirm { background: #4a442d; color: #dcdcaa; }
        .secbadge.readonly { background: #2d4a3e; color: #4ec9b0; }
        .tabs { display: flex; background: #252526; border-bottom: 1px solid #333; }
        .tab { flex: 1; padding: 6px; text-align: center; cursor: pointer;
          color: #888; font-size: 11px; border-bottom: 2px solid transparent; }
        .tab.active { color: #fff; border-bottom-color: #1f6feb; }
        .cmd { padding: 8px 10px; background: #2a2a2a; color: #dcdcaa;
          white-space: pre-wrap; word-break: break-all; max-height: 100px;
          overflow: auto; border-bottom: 1px solid #333; display: none; }
        .out { flex: 1; padding: 8px 10px; overflow: auto; white-space: pre-wrap;
          word-break: break-all; color: #cfcfcf; }
        .status { padding: 6px 10px; background: #252526; color: #888;
          font-size: 11px; border-top: 1px solid #333; display: flex;
          justify-content: space-between; align-items: center; }
        .status .tokens { color: #4ec9b0; cursor: pointer; }
        .status .tokens .label { color: #666; }
        .status .tokens:hover { text-decoration: underline; }
        .status .timer { color: #dcdcaa; margin-left: 10px; cursor: pointer; }
        .status .timer.running { color: #4ec9b0; }
        .status .timer.slow { color: #f48771; }
        .status .timer:hover { text-decoration: underline; }
        .history-wrap { flex: 1; display: none; flex-direction: column; }
        .history-head { padding: 5px 10px; background: #1a1a1a;
          border-bottom: 1px solid #333; font-size: 10px; color: #888;
          display: flex; justify-content: space-between; align-items: center; }
        .history-head .chatid { color: #4ec9b0; overflow: hidden;
          text-overflow: ellipsis; white-space: nowrap; flex: 1; }
        .history-head button { background: #333; color: #ddd; border: 0;
          border-radius: 4px; padding: 2px 8px; cursor: pointer; font-size: 10px; }
        .history-head button:hover { background: #5a2d2d; color: #f48771; }
        .history { flex: 1; overflow: auto; }
        .history-item { padding: 6px 10px; border-bottom: 1px solid #2a2a2a;
          cursor: pointer; color: #aaa; font-size: 11px; }
        .history-item:hover { background: #2a2a2a; }
        .history-item .code { color: #dcdcaa; }
        .history-item .exit-ok { color: #4ec9b0; }
        .history-item .exit-fail { color: #f48771; }
        .history-item .token-badge { color: #4ec9b0; }
        .history-item .root-badge { color: #666; font-size: 10px; }
        .debug { flex: 1; padding: 8px 10px; overflow: auto; display: none;
          white-space: pre-wrap; word-break: break-all; color: #888;
          font-size: 10px; font-family: ui-monospace, Menlo, monospace; }
        .settings { padding: 10px; display: none; flex-direction: column; gap: 6px; }
        .settings.open { display: flex; }
        .settings input[type=text] { background: #2a2a2a; color: #ddd; border: 1px solid #444;
          border-radius: 4px; padding: 4px 6px; font: inherit; }
        .settings label { color: #999; font-size: 11px; }
        .settings .row { display: flex; align-items: center; gap: 6px; }
        .token-detail { display: flex; flex-direction: column; gap: 2px;
          padding: 6px 10px; background: #1a1a1a;
          border-top: 1px solid #333; font-size: 10px; }
        .token-detail .line { display: flex; justify-content: space-between; color: #888; }
        .token-detail .line .val { color: #4ec9b0; }
        .token-detail .line.hidden { display: none; }
        .token-detail .note { color: #666; font-size: 10px;
          padding-top: 4px; border-top: 1px solid #2a2a2a; margin-top: 4px;
          display: flex; justify-content: space-between; align-items: center; }
        .token-detail .note a { color: #4ec9b0; cursor: pointer; text-decoration: none; }
        .token-detail .note a:hover { text-decoration: underline; }
        .confirm-overlay { position: fixed; inset: 0; background: rgba(0,0,0,.7);
          display: none; align-items: center; justify-content: center; }
        .confirm-overlay.open { display: flex; }
        .confirm-box { background: #1e1e1e; border: 1px solid #444; border-radius: 8px;
          padding: 16px; max-width: 400px; color: #ddd; }
        .confirm-box h3 { margin: 0 0 8px; color: #f48771; font-size: 13px; }
        .confirm-box pre { background: #2a2a2a; padding: 8px; border-radius: 4px;
          font-size: 11px; overflow: auto; max-height: 150px; }
        .confirm-box .btns { display: flex; gap: 8px; margin-top: 12px; justify-content: flex-end; }
        .confirm-box button { padding: 4px 12px; border-radius: 4px; border: 0;
          cursor: pointer; font-size: 11px; }
        .confirm-box .cancel { background: #333; color: #ddd; }
        .confirm-box .confirm { background: #f48771; color: #fff; }
      </style>
      <div class="fab" id="fab">🐦</div>
      <div class="panel" id="panel">
        <div class="head">
          <span class="title">Ronin</span>
          <span class="powerpill on" id="powerPill" title="Toggle Ronin for this chat">ON</span>
          <span class="spacer"></span>
          <button id="testBtn">Test</button>
          <button id="promptBtn">Prompt</button>
          <button id="describeBtn" class="accent">Describe</button>
          <button id="wrapupBtn">Wrap up</button>
          <button id="settingsBtn">⚙</button>
          <button id="clearBtn">Clear</button>
          <button id="closeBtn">✕</button>
        </div>
        <div class="pathbar">
          <label>cwd:</label>
          <input id="pathInput" type="text" placeholder="(server default)">
          <button id="pathSave" class="save">Set</button>
          <span class="secbadge confirm" id="secBadge">confirm</span>
        </div>
        <div class="tabs">
          <div class="tab active" data-tab="output">Output</div>
          <div class="tab" data-tab="history">History</div>
          <div class="tab" data-tab="debug">Debug</div>
        </div>
        <div class="cmd" id="cmd"></div>
        <div class="out" id="out"></div>
        <div class="history-wrap" id="historyWrap">
          <div class="history-head">
            <span>chat: <span class="chatid" id="historyChatId">—</span></span>
            <button id="historyClear">Clear this chat</button>
          </div>
          <div class="history" id="history"></div>
        </div>
        <div class="debug" id="debug"></div>
        <div class="settings" id="settings">
          <label>Server URL</label>
          <input id="serverUrl" type="text" placeholder="https://your-name.ngrok-free.app">
          <label>Token</label>
          <input id="token" type="text">
          <label>Project path (default cwd for all commands)</label>
          <input id="projectPath" type="text" placeholder="(server default)">
          <div class="row">
            <input id="skipNgrokWarning" type="checkbox">
            <label for="skipNgrokWarning">Send ngrok skip-warning header</label>
          </div>
          <div class="row">
            <input id="autoSend" type="checkbox">
            <label for="autoSend">Auto-send results to chat</label>
          </div>
          <div class="row">
            <label for="sendDelay" style="min-width:140px">Send delay (ms)</label>
            <input id="sendDelay" type="number" min="0" step="250" style="width:90px">
          </div>
          <div class="row">
            <input id="confirmDangerous" type="checkbox">
            <label for="confirmDangerous">Confirm sensitive commands</label>
          </div>
          <div class="row">
            <input id="confirmAll" type="checkbox">
            <label for="confirmAll">Confirm every command (manual mode)</label>
          </div>
          <div class="row">
            <input id="readOnly" type="checkbox">
            <label for="readOnly">Read-only mode (block writes)</label>
          </div>
          <div class="row">
            <input id="debugEnabled" type="checkbox">
            <label for="debugEnabled">Debug mode (log raw SSE)</label>
          </div>
          <button id="saveCfg">Save</button>
        </div>
        <div class="token-detail" id="tokenDetail">
          <div class="line" id="tokRowChat"><span>This chat</span><span class="val" id="tokChat">0</span></div>
          <div class="line" id="tokRowLast"><span>Last response</span><span class="val" id="tokLast">—</span></div>
          <div class="line hidden" id="tokRowInput"><span>Input</span><span class="val" id="tokInput">—</span></div>
          <div class="line hidden" id="tokRowOutput"><span>Output</span><span class="val" id="tokOutput">—</span></div>
          <div class="line hidden" id="tokRowCacheHit"><span>Cache hit</span><span class="val" id="tokCacheHit">—</span></div>
          <div class="line hidden" id="tokRowCacheMiss"><span>Cache miss</span><span class="val" id="tokCacheMiss">—</span></div>
          <div class="note">
            <span id="tokNoteText">Cumulative for this chat</span>
            <a id="tokReset">reset</a>
          </div>
        </div>
        <div class="status">
          <span id="statusText">idle</span>
          <span class="timer" id="statusTimer" title="Click to reset session total"></span>
          <span class="tokens" id="statusTokens" title="Click to reset"><span class="label">tokens: </span>0</span>
        </div>
      </div>
      <div class="confirm-overlay" id="confirmOverlay">
        <div class="confirm-box">
          <h3 id="confirmTitle">⚠️ Sensitive command</h3>
          <pre id="confirmCmd"></pre>
          <div class="btns">
            <button class="cancel" id="confirmCancel">Cancel</button>
            <button class="confirm" id="confirmOk">Run anyway</button>
          </div>
        </div>
      </div>
    `;

    const $ = (id) => root.getElementById(id);
    const panel = $('panel'), out = $('out'), cmdEl = $('cmd'),
          statusEl = $('statusText'), statusTokens = $('statusTokens'),
          statusTimer = $('statusTimer'),
          historyEl = $('history'), historyWrap = $('historyWrap'),
          debugEl = $('debug'), fab = $('fab');
    let output = '';
    let currentTab = 'output';
    const DEBUG_CAP = 60000;
    let debugHistory = '';
    let debugDirty = false;
    let debugTimer = null;

    function scheduleDebugFlush() {
      if (debugTimer !== null) return;
      debugTimer = setTimeout(flushDebug, 250);
    }

    function flushDebug(force) {
      if (debugTimer !== null) { clearTimeout(debugTimer); debugTimer = null; }
      if (currentTab !== 'debug' && !force) return;
      if (!debugDirty && !force) return;
      debugDirty = false;
      const atBottom = debugEl.scrollHeight - debugEl.scrollTop - debugEl.clientHeight < 40;
      debugEl.textContent = debugHistory;
      if (atBottom) debugEl.scrollTop = debugEl.scrollHeight;
    }

    function syncPathInput() {
      const inp = $('pathInput');
      const shown = state.currentCwd || cfg.projectPath || '';
      inp.value = shown;
      inp.classList.toggle('empty', !shown);
      inp.placeholder = '(server default)';
    }
    function syncSecBadge() {
      const el = $('secBadge');
      if (cfg.readOnly) { el.className = 'secbadge readonly'; el.textContent = 'read-only'; }
      else if (cfg.confirmAll || cfg.confirmDangerous) { el.className = 'secbadge confirm'; el.textContent = cfg.confirmAll ? 'confirm all' : 'confirm'; }
      else { el.className = 'secbadge open'; el.textContent = 'no confirm'; }
    }
    function syncEnabled() {
      const pill = $('powerPill');
      if (state.enabled) {
        pill.className = 'powerpill on';
        pill.textContent = 'ON';
        pill.title = 'Ronin is active for this chat. Click to disable.';
        fab.classList.remove('off');
        statusEl.textContent = 'idle';
      } else {
        pill.className = 'powerpill off';
        pill.textContent = 'OFF';
        pill.title = 'Ronin is disabled for this chat. Click to enable.';
        fab.classList.add('off');
        statusEl.textContent = 'disabled for this chat';
      }
    }

    $('fab').onclick = () => panel.classList.toggle('open');
    $('closeBtn').onclick = () => panel.classList.remove('open');
    $('clearBtn').onclick = () => { out.textContent = ''; output = ''; cmdEl.style.display = 'none'; };
    $('testBtn').onclick = () => testConnection();
    $('promptBtn').onclick = () => { typeAndSend(SYSTEM_PROMPT); };
    $('describeBtn').onclick = () => { typeAndSend(DESCRIBE_PROMPT); };
    $('wrapupBtn').onclick = () => { typeAndSend(WRAPUP_PROMPT); };
    $('powerPill').onclick = () => toggleEnabled();

    $('pathSave').onclick = () => {
      const v = $('pathInput').value.trim();
      cfg.projectPath = v;
      state.currentCwd = v;
      GM_setValue('projectPath', v);
      syncPathInput();
      overlay.setStatus(v ? ('cwd set: ' + v) : 'cwd reset to server default');
    };
    $('pathInput').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); $('pathSave').click(); }
    });

    $('historyClear').onclick = () => {
      if (confirm('Clear history for the current chat?')) clearHistoryForCurrentChat();
    };

    root.querySelectorAll('.tab').forEach(tab => {
      tab.onclick = () => {
        root.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
        tab.classList.add('active');
        currentTab = tab.dataset.tab;
        out.style.display = 'none'; historyWrap.style.display = 'none'; debugEl.style.display = 'none';
        if (currentTab === 'output') out.style.display = 'block';
        else if (currentTab === 'history') { historyWrap.style.display = 'flex'; renderHistory(); }
        else { debugEl.style.display = 'block'; flushDebug(true); }
      };
    });

    $('settingsBtn').onclick = () => {
      const s = $('settings');
      s.classList.toggle('open');
      if (s.classList.contains('open')) {
        $('serverUrl').value = cfg.serverUrl;
        $('token').value = cfg.token;
        $('projectPath').value = cfg.projectPath;
        $('autoSend').checked = cfg.autoSend;
        $('sendDelay').value = cfg.sendDelay;
        $('confirmDangerous').checked = cfg.confirmDangerous;
        $('confirmAll').checked = cfg.confirmAll;
        $('readOnly').checked = cfg.readOnly;
        $('skipNgrokWarning').checked = cfg.skipNgrokWarning;
        $('debugEnabled').checked = cfg.debugEnabled;
      }
    };

    $('saveCfg').onclick = () => {
      cfg.serverUrl = $('serverUrl').value.trim();
      cfg.token = $('token').value.trim();
      cfg.projectPath = $('projectPath').value.trim();
      cfg.autoSend = $('autoSend').checked;
      cfg.sendDelay = Math.max(0, parseInt($('sendDelay').value, 10) || 0);
      cfg.confirmDangerous = $('confirmDangerous').checked;
      cfg.confirmAll = $('confirmAll').checked;
      cfg.readOnly = $('readOnly').checked;
      cfg.skipNgrokWarning = $('skipNgrokWarning').checked;
      cfg.debugEnabled = $('debugEnabled').checked;
      state.debugEnabled = cfg.debugEnabled;
      state.currentCwd = cfg.projectPath;
      GM_setValue('serverUrl', cfg.serverUrl);
      GM_setValue('token', cfg.token);
      GM_setValue('projectPath', cfg.projectPath);
      GM_setValue('autoSend', cfg.autoSend);
      GM_setValue('sendDelay', cfg.sendDelay);
      GM_setValue('confirmDangerous', cfg.confirmDangerous);
      GM_setValue('confirmAll', cfg.confirmAll);
      GM_setValue('readOnly', cfg.readOnly);
      GM_setValue('skipNgrokWarning', cfg.skipNgrokWarning);
      GM_setValue('debugEnabled', cfg.debugEnabled);
      syncPathInput();
      syncSecBadge();
      statusEl.textContent = 'saved';
      $('settings').classList.remove('open');
    };

    $('tokReset').onclick = (e) => { e.preventDefault(); resetTokenUsage(); };
    statusTokens.onclick = () => resetTokenUsage();

    let confirmResolve = null;
    function showConfirmDialog(title, cmd) {
      return new Promise((resolve) => {
        $('confirmTitle').textContent = title;
        $('confirmCmd').textContent = cmd;
        $('confirmOverlay').classList.add('open');
        confirmResolve = resolve;
      });
    }
    $('confirmOk').onclick = () => { $('confirmOverlay').classList.remove('open'); if (confirmResolve) confirmResolve(true); };
    $('confirmCancel').onclick = () => { $('confirmOverlay').classList.remove('open'); if (confirmResolve) confirmResolve(false); };

    function renderTokenUsage(session) {
      const chatTotal = session.chatTotal || 0;
      statusTokens.innerHTML = '<span class="label">tokens: </span>' + formatTokens(chatTotal);
      $('tokChat').textContent = formatTokens(chatTotal);
      $('tokLast').textContent = formatTokens(session.lastResponse);
      const hasSplits = session.input != null || session.output != null ||
                        session.cacheHit != null || session.cacheMiss != null;
      $('tokRowInput').classList.toggle('hidden', !hasSplits);
      $('tokRowOutput').classList.toggle('hidden', !hasSplits);
      $('tokRowCacheHit').classList.toggle('hidden', !hasSplits);
      $('tokRowCacheMiss').classList.toggle('hidden', !hasSplits);
      if (hasSplits) {
        $('tokInput').textContent = formatTokens(session.input);
        $('tokOutput').textContent = formatTokens(session.output);
        $('tokCacheHit').textContent = formatTokens(session.cacheHit);
        $('tokCacheMiss').textContent = formatTokens(session.cacheMiss);
      }
    }

    function renderHistory() {
      $('historyChatId').textContent = state.currentChatId || '—';
      historyEl.innerHTML = '';
      if (state.history.length === 0) {
        historyEl.innerHTML = '<div style="padding:10px;color:#666;">No history for this chat yet</div>';
        return;
      }
      state.history.forEach((item) => {
        const div = document.createElement('div');
        div.className = 'history-item';
        const time = new Date(item.timestamp).toLocaleTimeString();
        const exitClass = item.code === 0 ? 'exit-ok' : 'exit-fail';
        const tokenLine = item.usage ? `<span class="token-badge">${formatTokens(item.usage.total)} tok</span>` : '';
        const rootLine = item.root ? `<span class="root-badge">${escapeHtml(item.root)}</span>` : '';
        const blockBadge = item.blocks && item.blocks > 1 ? `<span class="token-badge">${item.blocks} blocks</span>` : '';
        div.innerHTML = `
          <div class="code">$ ${escapeHtml(item.command.slice(0, 80))}${item.command.length > 80 ? '…' : ''}</div>
          <div style="display:flex;gap:8px;margin-top:2px;flex-wrap:wrap;">
            <span class="${exitClass}">exit ${item.code}</span>
            <span style="color:#666;">${time}</span>
            ${tokenLine}${blockBadge}${rootLine}
          </div>`;
        div.onclick = () => {
          out.textContent = item.output;
          output = item.output;
          cmdEl.textContent = '$ ' + item.command;
          cmdEl.style.display = 'block';
          root.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
          root.querySelector('.tab[data-tab="output"]').classList.add('active');
          out.style.display = 'block';
          historyWrap.style.display = 'none';
          debugEl.style.display = 'none';
        };
        historyEl.appendChild(div);
      });
    }

    function escapeHtml(s) {
      return s.replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
    }

    pageDoc.addEventListener('keydown', (e) => {
      if (e.ctrlKey && e.shiftKey && e.code === 'KeyC') { e.preventDefault(); panel.classList.toggle('open'); }
      if (e.key === 'Escape') {
        if ($('confirmOverlay').classList.contains('open')) $('confirmCancel').click();
        else panel.classList.remove('open');
      }
    });

    syncPathInput();
    syncSecBadge();
    syncEnabled();
    renderTokenUsage(state.tokenUsage);

    // ---- build timer ----
    const timer = { start: 0, running: false, sessionTotal: 0, tick: null, lastStatus: 'idle' };

    function fmtDur(ms) {
      const t = Math.floor(ms / 1000);
      const h = Math.floor(t / 3600), m = Math.floor((t % 3600) / 60), sec = t % 60;
      if (h > 0) return h + 'h ' + m + 'm ' + sec + 's';
      if (m > 0) return m + 'm ' + sec + 's';
      return sec + 's';
    }

    function paintTimer() {
      const cur = timer.running ? Date.now() - timer.start : 0;
      const total = timer.sessionTotal + cur;
      let txt = '';
      if (timer.running) {
        txt = '\u23f1 ' + fmtDur(cur);
        if (timer.sessionTotal > 0) txt += ' (total ' + fmtDur(total) + ')';
      } else if (timer.sessionTotal > 0) {
        txt = '\u23f1 total ' + fmtDur(timer.sessionTotal);
      }
      statusTimer.textContent = txt;
      statusTimer.className = 'timer' + (timer.running ? (cur > 60000 ? ' slow' : ' running') : '');
      if (txt) statusEl.textContent = timer.lastStatus;
    }

    function timerStart(label) {
      if (timer.running) timerStop(true);
      timer.start = Date.now();
      timer.running = true;
      if (label) timer.lastStatus = label;
      if (timer.tick) clearInterval(timer.tick);
      timer.tick = setInterval(paintTimer, 1000);
      paintTimer();
    }

    function timerStop(keepStatus) {
      if (!timer.running) return 0;
      const elapsed = Date.now() - timer.start;
      timer.sessionTotal += elapsed;
      timer.running = false;
      if (timer.tick) { clearInterval(timer.tick); timer.tick = null; }
      if (!keepStatus && timer.lastStatus) statusEl.textContent = timer.lastStatus;
      paintTimer();
      return elapsed;
    }

    function timerReset() {
      timer.sessionTotal = 0;
      timer.running = false;
      if (timer.tick) { clearInterval(timer.tick); timer.tick = null; }
      timer.lastStatus = 'idle';
      statusEl.textContent = 'idle';
      paintTimer();
    }

    statusTimer.onclick = () => timerReset();
    paintTimer();

    return {
      setCommand(c) { cmdEl.textContent = '$ ' + c; cmdEl.style.display = 'block'; output = ''; out.textContent = ''; },
      appendOutput(chunk) { if (!chunk) return; output += chunk; out.textContent = output; out.scrollTop = out.scrollHeight; },
      appendDebug(text) {
        if (!text) return;
        debugHistory += text;
        if (debugHistory.length > DEBUG_CAP) {
          debugHistory = debugHistory.slice(-Math.floor(DEBUG_CAP * 0.6));
        }
        debugDirty = true;
        scheduleDebugFlush();
      },
      getOutput() { return output; },
      setStatus(s) { timer.lastStatus = s; statusEl.textContent = s; },
      timerStart,
      timerStop,
      timerReset,
      timerRunning() { return timer.running; },

      renderTokenUsage,
      showConfirmDialog,
      renderHistory,
      syncPathInput,
      syncSecBadge,
      syncEnabled,
    };
  })();

  // ============ PROMPTS ============
  const SYSTEM_PROMPT = `You are an AI programming assistant powered by Ronin Agent, with direct access to a bash shell running on a Linux machine. The shell is rooted at a specific project directory. A userscript running in this chat page executes every \`\`\`bash block you emit, sequentially, and appends the output back to the conversation as the next user message. You are, in effect, pair-programming with a real Linux box.

The environment enforces a layered security policy:

1. A hard blocklist ALWAYS rejects destructive commands (fork bombs, rm -rf on root, mkfs, writes to /etc, shutdown/reboot). Do not attempt these — they will never run.
2. A confirm list intercepts sensitive commands (package managers, sudo, service control, docker run, curl|sh, git push --force, etc.). These run only after the user explicitly approves.
3. Read-only mode may be on: only commands that read (cat, ls, grep, git log, etc.) are allowed. No redirects, no sed -i, no find -delete.

Do not waste turns trying to circumvent these. If a command is blocked or needs confirmation, the user will see it. Adapt.

# MCP tools (optional)

This server may also act as an MCP client, exposing tools from configured
MCP servers. Discover them by running this in a bash block:

    ./ronin mcp list

That prints one line per tool as:  server.tool  [policy]  description
where policy is allow (runs immediately), confirm (the user is prompted),
or deny (will fail with exit 126 - do not call these).

Invoke a tool with:

    ./ronin mcp call server.tool '{"arg":"value"}'

The JSON is passed as tool arguments. Tools marked confirm will return
exit 125 with a [ronin-mcp-confirm] marker; the userscript then asks the
user to approve and re-runs the same block automatically. Do not try to
work around that prompt. If no MCP servers are configured, list prints a
short notice - that is fine, just continue with normal shell work.

# Skills (optional)

Skills are reusable instruction packs installed on this machine. Each one
is a directory with a SKILL.md holding a name, a one-line description, and
a body of instructions for a specific kind of task.

Before starting a task that looks like it has a skill, check what exists:

    ./ronin skill list

That prints one line per skill: name, source, and description, plus any
bundled files. This is cheap, so check it early rather than guessing.

When a skill matches, load its instructions before working:

    ./ronin skill show <name>

Follow the loaded instructions. If the skill lists bundled files, they are
relative to the skill's location, which show prints. Do not load every
skill up front - list is enough to decide, and show is only for the one you
are actually using. If list reports no skills, carry on normally.

# CRITICAL: First turn behavior

On the FIRST message of a new chat, do NOT run any bash commands. Instead, reply with plain text (no code block) asking the user what they want to work on, and wait for their answer.

Suggest a few concrete options based on what you can infer from the setup (e.g. explore the codebase, run the tests, debug an issue, add a feature, review recent git changes). Keep it short — one or two sentences.

Example first reply:
"What would you like to work on? I can explore the codebase, run tests, debug an issue, add a feature, or review recent changes. Let me know what you need."

The ONLY exception: if the user's first message already contains a specific task or question, then skip the question and work on that task directly.

After the first turn, follow the normal rules below.

# Executable vs display-only code

The \`\`\`bash and \`\`\`sh fences are EXECUTABLE. Anything inside them runs on the real shell and the output comes back to you. Do not put example code, snippets the user should paste themselves, or unrelated code samples in a \`\`\`bash block.

When you want to SHOW the user a command without running it, use \`\`\`console or \`\`\`text instead. Those fences are DISPLAY ONLY — nothing executes.

Rule of thumb:
- \`\`\`bash = "run this now"
- \`\`\`console = "here is a command for you to look at or paste somewhere else"
- \`\`\`text = "here is generic text output or config to read"

# How you talk to the shell

To run commands, reply with ONE OR MORE fenced code blocks tagged \`bash\`. Each block runs as an independent \`bash -c\`-style invocation, in the order you write them. Blocks share a persistent working directory: if block 1 does \`cd /some/path\`, block 2 starts there too. But environment variables and shell state do NOT persist between blocks.

You may include plain text outside the code blocks ONLY to explain what the blocks do or to ask a clarifying question.

Example reply with two executable blocks:

I'll first change into the logs directory, then list the most recent entries.

\`\`\`bash
cd /var/log
\`\`\`

\`\`\`bash
ls -lt | head -10
\`\`\`

After you reply, the next user message will contain each block's output, labeled:

\`\`\`
=== Block 1 (exit 0) ===
Command:
\`\`\`bash
cd /var/log
\`\`\`
Output:
\`\`\`
\`\`\`

=== Block 2 (exit 0) ===
Command:
\`\`\`bash
ls -lt | head -10
\`\`\`
Output:
\`\`\`
<listing>
\`\`\`
\`\`\`

Read each block's output carefully, then decide your next blocks.

Example reply that SHOWS a command without running it:

To start the server yourself, run:

\`\`\`console
go run ./cmd/server -port 8080
\`\`\`

The \`\`\`console fence is display-only — nothing will execute when you use it.

# When to use multiple blocks vs. one block

- Use ONE block when commands depend on each other's output, share environment variables, or must run in the same shell context (trap, function definitions).
- Use MULTIPLE blocks when commands are independent and you want clean, labeled, per-command exit codes.
- \`cd\` persists between blocks, but \`export FOO=bar\` does NOT. If you need an environment variable in block 2, either define it in block 2 or combine both commands into one block with \`FOO=bar; ...\`.
- Do NOT emit more than 6 blocks in one reply. If you need more, split into multiple turns.

# Incorrect replies (never do these)

- A code block tagged \`ronin\`, \`javascript\`, \`python\`, or anything other than \`bash\`/\`sh\` that you intend to execute.
- Putting example code, snippets-for-the-user, or code samples the user should run themselves inside a \`\`\`bash or \`\`\`sh block. Use \`\`\`console or \`\`\`text instead.
- JSON, function-call syntax, or pseudo-tool-call markup.
- A bash block wrapped in quotes or escaped characters.
- More than 6 executable blocks in one reply.
- Interactive commands that will hang waiting for stdin.
- Running commands on the very first turn when the user hasn't asked for anything yet.

# Core principles

1. Think first, then act. Before emitting a command, silently reason about what you expect the output to be, what could go wrong, and what the next step is depending on the result. Ambiguous request? Ask ONE clarifying question as plain text (no code block) and wait for the answer.

2. Simplicity above all. Prefer the simplest command that gets the job done. \`cat\` beats \`awk\` for reading a file. \`ls\` beats a find pipeline for listing. Do not introduce abstractions, wrappers, or speculative features.

3. Surgical edits. When modifying code, change the minimum necessary. Do not reformat untouched lines. Do not rename things "while you're here." Do not upgrade dependencies unless explicitly asked. If a task requires touching 20 files, pause and confirm the scope with the user first.

4. Goal-driven execution. Every command should move toward a defined success state. State the success criteria to yourself before you begin. If you lose track, run a quick check command (\`git status\`, \`ls\`, \`pwd\`) to reorient rather than guessing.

5. Small steps. Complete one small task per block, observe the output, then decide the next step. Do not chain ten unrelated commands in a single block hoping they all work.

6. Trust the output. The shell output is ground truth. If \`ls\` shows the file doesn't exist, it doesn't exist — adapt. If a command fails, read the error, form a hypothesis, test with one diagnostic command.

# What you may and may not do

Allowed:
- Anything read-only: \`cat\`, \`ls\`, \`find\`, \`grep\`, \`rg\`, \`head\`, \`tail\`, \`wc\`, \`git log\`, \`git diff\`, \`git status\`, \`tree\`, \`file\`, \`stat\`.
- Writing and editing files: \`echo >\`, \`tee\`, \`sed -i\`, \`awk\` in-place, \`cat <<EOF\`, \`printf\`. Prefer \`sed -i\` for surgical line edits and heredocs for new files.
- Build and test: \`go build\`, \`go test\`, \`npm test\`, \`cargo check\`, \`make\`, \`pytest\`, \`golangci-lint\`.
- Package managers: \`go get\`, \`npm install\`, \`pip install\` — but ONLY when the task requires it, and state why. Global installs and system package managers require user confirmation.
- Git operations: \`git add\`, \`git commit\`, \`git checkout -b\`, \`git diff\`. Never \`git push --force\`, \`git reset --hard\` on shared branches, or \`git rebase\` on a branch you didn't create.
- Process control: \`ps\`, \`pkill\` on processes you started, background jobs with \`&\`, \`nohup\`, \`disown\`.

Requires explicit user confirmation before running (the overlay will prompt, but you should also mentally flag these):
- \`rm -rf\` on anything outside the project root.
- \`sudo\` at all.
- Overwriting or deleting \`.git\`, \`node_modules\`, build artifacts you didn't create.
- Anything piping a network fetch into a shell: \`curl ... | sh\`, \`wget ... | bash\`.
- Editing files outside the project root.
- Installing system packages (\`apt\`, \`yum\`, \`brew\`), global installs (\`npm -g\`, \`pip install\`, \`cargo install\`, \`go install\`), \`systemctl\`, \`docker run\`, \`git push --force\`.

Never:
- Emit an interactive command. No \`vim\`, \`nano\`, \`less\` without \`-F\`, \`top\`, \`ssh\`, \`mysql\` without \`-e\`, \`psql\` without \`-c\`.
- Use \`read\` to wait for stdin. It will hang until timeout.
- Run something that produces unbounded output (\`cat\` on a 10GB log, \`yes\`, \`find /\`). Pipe through \`head\`, \`tail\`, or \`wc -l\` first if unsure of size.
- Attempt to bypass the server's blocklist with obfuscation.

# Sandbox rules

- Working directory is whatever the user has set in the overlay's cwd field, OR wherever a previous block's \`cd\` left it. Relative paths resolve from there. Use \`pwd\` if unsure.
- Environment variables and shell functions do NOT persist between blocks. Only the working directory does.
- Default timeout is 5 minutes per block. Long-running tasks must be split:
  - Block 1: start in background, redirect to file, return immediately: \`nohup ./myserver > /tmp/server.log 2>&1 & echo $! > /tmp/server.pid\`
  - Block 2 (or a later turn): check log/process: \`sleep 3; cat /tmp/server.log | tail -50; ps -p $(cat /tmp/server.pid)\`
- Output over 1MB total per reply is truncated. Pipe through \`head -n 200\` or \`| wc -l\` if you expect a lot.
- The shell does NOT have a TTY. Prefer plain mode flags: \`--no-color\`, \`--quiet\`, \`-q\`, \`CI=1\`.

# File editing strategy

For reading: \`cat\` for short files, \`sed -n '1,80p'\` for ranges, \`head\`/\`tail\` for ends.

For surgical edits, prefer \`sed -i\` with a unique anchor:
\`\`\`bash
sed -i 's/^func oldName(/func newName(/' path/to/file.go
\`\`\`

For multi-line insertions, use a heredoc with a temp file and \`mv\`:
\`\`\`bash
cat > /tmp/patch.go <<'EOF'
func newFunction() {
    // ...
}
EOF
sed -i '/^func main/ r /tmp/patch.go' path/to/file.go
\`\`\`

For whole-file rewrites, use \`cat > file <<'EOF' ... EOF\`. Always quote the heredoc delimiter (\`<<'EOF'\`) to prevent shell expansion.

After ANY edit, verify with \`git diff\` or by re-reading the changed region.

# Debugging protocol

1. Read the error completely. The last line often says exactly what's wrong.
2. Form ONE hypothesis.
3. Test with ONE command.
4. If it holds, fix it. If not, form the next hypothesis.
5. If three diagnostic commands fail, stop and ask the user.

Do not "try things" blindly. Do not install packages to see if they help.

# Communication style

Use plain text sparingly, only for:
- Asking the first-turn question (see the CRITICAL section at the top).
- Asking ONE clarifying question.
- Reporting a blocker you cannot resolve.
- Confirming a destructive action.
- Summarizing the outcome of a multi-step task.
- Showing the user a command to run themselves (use a \`\`\`console block).

Do not narrate each step. Do not explain output unless asked. Do not apologize; diagnose.

# Output self-check (run this before every reply)

- Is this the first turn and the user hasn't given a task? If so, reply with a question and NO executable code block.
- Is every EXECUTABLE block tagged \`bash\` or \`sh\`?
- Is every DISPLAY-ONLY block tagged \`console\`, \`text\`, or another non-executable fence?
- Are there 6 or fewer executable blocks?
- Is each block non-interactive?
- Does each block produce bounded output?
- If long-running, is it backgrounded?
- If it modifies files, is the edit surgical?
- If destructive or system-affecting, did I flag it (it will need user confirmation anyway)?
- Did I avoid chaining more than ~3 unrelated operations in one block with \`;\`?

If any answer is "no", rewrite before sending.

# When to stop and ask

Stop and ask the user when:
- It's the first turn and they haven't asked for anything specific (always).
- The request is ambiguous.
- The task would touch more than ~5 files.
- You need to make an architectural decision.
- You've failed three attempts at the same sub-task.
- The task requires credentials or elevated permissions.
- The user's mental model differs from what the code does.

You are a careful, surgical, goal-driven assistant. You inspect, hypothesize, test, and act — one small, verified step at a time.`;

  const DESCRIBE_PROMPT = `Explore the project at the current working directory and produce a comprehensive description of it for me. You SHOULD run bash commands to inspect the files — this is an exploration task, not a summary-from-memory task.

# Suggested exploration

You do not have to run all of these, and you may run others as needed. Pick what fits the project. Batch independent reads into multiple blocks per reply.

- \`pwd && ls -la\` — see the top-level layout
- \`find . -maxdepth 2 -type f \\( -name "*.md" -o -name "*.json" -o -name "*.toml" -o -name "*.yaml" -o -name "*.yml" -o -name "*.mod" -o -name "*.lock" \\) -not -path "*/node_modules/*" -not -path "*/.git/*" | head -40\` — find configs and docs
- \`cat README.md 2>/dev/null | head -120\` — read the readme if present
- \`git remote -v 2>/dev/null; git log --oneline -10 2>/dev/null\` — origin and recent history
- \`ls -la src 2>/dev/null || ls -la cmd 2>/dev/null || ls -la app 2>/dev/null\` — find the main source tree
- \`find . -maxdepth 3 -type f \\( -name "main.*" -o -name "index.*" -o -name "app.*" -o -name "server.*" \\) -not -path "*/node_modules/*" | head\` — entry points
- \`cat go.mod 2>/dev/null || cat package.json 2>/dev/null || cat Cargo.toml 2>/dev/null || cat pyproject.toml 2>/dev/null || cat requirements.txt 2>/dev/null\` — dependencies and metadata
- \`ls -la .github/workflows 2>/dev/null; ls -la Dockerfile docker-compose.yml Makefile justfile 2>/dev/null\` — build/CI setup

Stop exploring once you can confidently answer all the sections below. Do not over-explore.

# Final report

When you are done, STOP running commands and write a plain-text report with these sections:

## Project name and purpose
One or two sentences describing what this project is and what it does. Pull the name from the README, package manifest, or git remote — not from the folder name alone.

## Tech stack
Languages, frameworks, key libraries, build tools, package managers. List ONLY what you saw evidence of in the files you read. Do not guess or pad the list.

## Directory structure
A short annotated tree of the most important directories and files. Skip \`node_modules\`, \`.git\`, \`vendor\`, \`dist\`, \`build\`, and similar generated trees. Aim for 10-20 lines maximum.

## Entry points
Where the program starts (main file, server bootstrap, CLI entry, etc.) and the primary commands to build, run, and test it. Include the exact command strings if you found them (in README, Makefile, package.json scripts, etc.).

## Existing documentation
Note what docs exist (README, docs/, comments, code of conduct, etc.) and how thorough they are. If documentation is missing or sparse, say so plainly.

## Observations
Anything notable you found: TODOs and FIXMEs, dead code, inconsistent patterns, missing tests, outdated dependencies, obvious next steps. Be brief and honest — no filler. Skip this section if you have nothing concrete to say.

Keep the whole report under 600 words. Only state what you verified with commands. If you could not determine something, write "Could not determine from the files available" rather than guessing.`;

  const WRAPUP_PROMPT = `Please wrap up this session now. Do NOT run any more bash commands unless absolutely necessary — this is a summary task, not a working task.

Write a plain-text report with these sections:

## What we accomplished
A short paragraph (2-4 sentences) describing the overall outcome.

## Files changed
A bullet list of every file that was created, modified, or deleted during this session. For each, one line describing what changed and why. If you don't remember exact paths, say so and suggest the user run \`git status\` or \`git diff --stat\` to verify.

## Unfinished work
Anything that was started but not completed, or any known issues, TODOs, or edge cases that still need attention. If nothing, write "None."

## Next steps
2-4 concrete suggested next actions for the user, ordered by priority.

## Notes
Any warnings, caveats, or context the user should know before continuing. If nothing, omit this section.

Keep the whole report under 400 words. Be honest about what is uncertain — do not invent details.`;

  window.__roninInsertPrompt = function () { typeAndSend(SYSTEM_PROMPT); };
  window.__roninDescribe = function () { typeAndSend(DESCRIBE_PROMPT); };
  window.__roninWrapup = function () { typeAndSend(WRAPUP_PROMPT); };
  window.__roninOverlay = overlay;
  window.__roninConfig = cfg;
  window.__roninState = state;
  window.__roninResetTokens = resetTokenUsage;
  window.__roninToggle = toggleEnabled;

  checkChatChange();

  if (state.debugEnabled) {
    overlay.appendDebug('=== Ronin Agent v3.8.0 loaded ' + new Date().toISOString() + ' ===\n');
    overlay.appendDebug('chat id: ' + state.currentChatId + ' enabled=' + state.enabled + '\n');
  }
})();
