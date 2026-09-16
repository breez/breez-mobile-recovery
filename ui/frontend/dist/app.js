/* Breez Recovery frontend. Plain JS over the Wails bindings (window.go.main.App)
   and runtime events (window.runtime). */
(function () {
  "use strict";

  const $ = (sel) => document.querySelector(sel);
  const $$ = (sel) => Array.from(document.querySelectorAll(sel));

  let api = null;
  let rt = null;

  const ui = {
    state: null,          // from GetState
    screen: "welcome",
    source: null,         // "google" | "icloud" | "zip"
    snapshots: [],
    selected: null,       // chosen snapshot
    zip: null,            // {path,name,needsPhrase}
    force: false,         // overwrite an existing restored wallet
    status: null,         // last wallet status
    closeAddress: "",
    closeForce: false,
    sweepPlan: null,
    sweepTarget: 6,
    lastTxid: "",
    recent: [],           // recent progress lines
    rescanStart: 0,       // when the history check began, for the elapsed display
    logCount: 0,
    logOpen: false,
    busy: false,
  };

  // ---------------------------------------------------------------- helpers

  function show(name) {
    ui.screen = name;
    $$(".screen").forEach((s) => s.classList.toggle("active", s.dataset.screen === name));
    $("#main").scrollTop = 0;
  }

  function showError(msg) {
    $("#error-text").textContent = String(msg || "Something went wrong");
    $("#error").classList.remove("hidden");
  }
  function clearError() { $("#error").classList.add("hidden"); }

  function errMsg(e) {
    if (!e) return "Unknown error";
    if (typeof e === "string") return e;
    return e.message || String(e);
  }
  function isCancel(e) { return /cancelled|context canceled/i.test(errMsg(e)); }

  function fmtSat(n) {
    n = Number(n || 0);
    return n.toLocaleString("en-US") + " sat";
  }
  function fmtBtc(n) {
    n = Number(n || 0);
    if (n === 0) return "";
    return (n / 1e8).toFixed(8).replace(/0+$/, "").replace(/\.$/, "") + " BTC";
  }
  function fmtDate(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return iso;
    return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
  }
  function fmtBlocks(blocks) {
    const mins = blocks * 10;
    if (mins < 90) return "about " + mins + " minutes";
    if (mins < 60 * 36) return "about " + Math.round(mins / 60) + " hours";
    return "about " + Math.round(mins / 60 / 24) + " days";
  }
  function shortId(id) { return id ? id.slice(0, 10) + "…" + id.slice(-6) : ""; }
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function setBusy(b) { ui.busy = b; }

  function pushRecent(msg) {
    ui.recent.push(msg);
    if (ui.recent.length > 5) ui.recent.shift();
    const list = $("#working-recent");
    list.innerHTML = "";
    ui.recent.slice(0, -1).forEach((m) => list.appendChild(el("li", null, m)));
    $("#working-message").textContent = msg;
    $("#signin-progress").textContent = msg;
  }

  function working(title, cancellable) {
    ui.recent = [];
    $("#working-title").textContent = title;
    $("#working-message").textContent = "";
    $("#working-recent").innerHTML = "";
    $("#working-cancel").classList.toggle("hidden", cancellable === false);
    show("working");
  }

  // ---------------------------------------------------------------- log panel

  const logLines = $("#log-lines");
  function appendLog(lines) {
    const atBottom = logLines.scrollHeight - logLines.scrollTop - logLines.clientHeight < 40;
    const frag = document.createDocumentFragment();
    lines.forEach((line) => {
      const span = el("span", line.includes("[recovery]") ? "tool" : null, line + "\n");
      frag.appendChild(span);
    });
    logLines.appendChild(frag);
    while (logLines.childNodes.length > 5000) logLines.removeChild(logLines.firstChild);
    ui.logCount += lines.length;
    $("#log-count").textContent = ui.logCount > 9999 ? "9999+" : String(ui.logCount);
    if (atBottom) logLines.scrollTop = logLines.scrollHeight;
  }
  function toggleLog(open) {
    ui.logOpen = open == null ? !ui.logOpen : open;
    $("#log-panel").classList.toggle("collapsed", !ui.logOpen);
    if (ui.logOpen) logLines.scrollTop = logLines.scrollHeight;
  }

  // ---------------------------------------------------------------- welcome

  async function refreshState() {
    ui.state = await api.GetState();
    $("#version").textContent = ui.state.version;
    $("#workdir").value = ui.state.workDir;
    $("#peers").value = ui.state.peers || "";
    $("#log-path").textContent = ui.state.workDir;
    $("#existing-node").classList.toggle("hidden", !ui.state.hasNode);
    $("#existing-node-path").textContent = ui.state.workDir;
    $("#welcome-actions").classList.toggle("hidden", ui.state.hasNode);
  }

  async function applySettings() {
    try {
      ui.state = await api.ApplySettings({ workDir: $("#workdir").value, peers: $("#peers").value });
      await refreshState();
    } catch (e) { showError(errMsg(e)); }
  }

  // ---------------------------------------------------------------- source and sign in

  async function listFrom(source) {
    ui.source = source;
    const google = source === "google";
    $("#signin-title").textContent = google ? "Sign in to Google" : "Sign in with your Apple ID";
    $("#signin-text").textContent = google
      ? "A browser window opens for the Google account the phone backed up to. Approve access to the Breez app data, then come back here."
      : "A browser window opens for the Apple ID the phone used. Sign in, including two-factor, then come back here.";
    $("#signin-link").classList.add("hidden");
    $("#signin-progress").textContent = "";
    ui.recent = [];
    show("signin");
    setBusy(true);
    try {
      const snaps = google ? await api.ListGoogle() : await api.ListICloud();
      ui.snapshots = snaps || [];
      renderSnapshots();
      show("pick");
    } catch (e) {
      show("source");
      if (!isCancel(e)) showError(errMsg(e));
    } finally { setBusy(false); }
  }

  function encLabel(s) {
    switch (s.encryptionType) {
      case "Mnemonics": return { text: "24-word phrase", cls: "" };
      case "Mnemonics12": return { text: "12-word phrase", cls: "" };
      case "PIN": return { text: "PIN encryption, not supported", cls: "err" };
      default: return { text: "Not encrypted", cls: "ok" };
    }
  }

  function renderSnapshots() {
    const list = $("#snapshot-list");
    list.innerHTML = "";
    ui.selected = null;
    $("#pick-continue").disabled = true;
    ui.snapshots.forEach((s, i) => {
      const item = el("div", "item");
      const unsupported = s.encryptionType === "PIN";
      if (unsupported) item.classList.add("disabled");
      const main = el("div", "item-main");
      const title = el("div", "item-title", "Last backup " + fmtDate(s.modifiedTime));
      if (i === 0) {
        const t = el("span", "tag", "Latest");
        t.style.marginLeft = "8px";
        title.appendChild(t);
      }
      main.appendChild(title);
      main.appendChild(el("div", "item-sub", s.nodeId));
      item.appendChild(main);
      const lab = encLabel(s);
      item.appendChild(el("span", "tag " + lab.cls, lab.text));
      if (!unsupported) {
        item.addEventListener("click", () => {
          ui.selected = s;
          $$("#snapshot-list .item").forEach((x) => x.classList.remove("selected"));
          item.classList.add("selected");
          $("#pick-continue").disabled = false;
        });
      }
      list.appendChild(item);
    });
  }

  async function chooseZip() {
    try {
      const info = await api.ChooseZip();
      if (!info || !info.path) return;
      ui.source = "zip";
      ui.zip = info;
      ui.selected = null;
      if (info.needsPhrase) {
        $("#phrase-lead").textContent = "The file " + info.name + " is encrypted with the backup phrase the Breez app showed you when you enabled backup. It has 12 or 24 words.";
        openPhrase();
      } else {
        await restore("");
      }
    } catch (e) { showError(errMsg(e)); }
  }

  // ---------------------------------------------------------------- phrase

  function openPhrase() {
    $("#phrase").value = "";
    $("#phrase-count").textContent = "0 words";
    $("#phrase-count").className = "hint";
    show("phrase");
    $("#phrase").focus();
  }

  function pickContinue() {
    const s = ui.selected;
    if (!s) return;
    if (s.encrypted) {
      const words = s.encryptionType === "Mnemonics12" ? 12 : 24;
      $("#phrase-lead").textContent = "This backup is encrypted with the " + words + "-word backup phrase the Breez app showed you when you enabled backup.";
      openPhrase();
    } else {
      restore("");
    }
  }

  async function phraseContinue() {
    const phrase = $("#phrase").value.trim().toLowerCase().replace(/\s+/g, " ");
    const hint = $("#phrase-count");
    try {
      const encType = await api.CheckPhrase(phrase);
      if (ui.selected && ui.selected.encrypted && encType !== ui.selected.encryptionType) {
        const want = ui.selected.encryptionType === "Mnemonics12" ? 12 : 24;
        hint.textContent = "This backup expects a " + want + "-word phrase.";
        hint.className = "hint err";
        return;
      }
      hint.className = "hint ok";
      hint.textContent = "Phrase looks valid";
      await restore(phrase);
    } catch (e) {
      hint.textContent = errMsg(e);
      hint.className = "hint err";
    }
  }

  // ---------------------------------------------------------------- restore and sync

  async function restore(phrase) {
    const req = {
      source: ui.source,
      nodeId: ui.selected ? ui.selected.nodeId : "",
      zipPath: ui.zip ? ui.zip.path : "",
      phrase: phrase,
      force: ui.force,
    };
    working("Restoring your backup", true);
    setBusy(true);
    try {
      await api.Restore(req);
      ui.force = false;
      await refreshState();
      await startSync();
    } catch (e) {
      setBusy(false);
      if (isCancel(e)) { await refreshState(); show("welcome"); return; }
      const msg = errMsg(e);
      if (ui.selected && ui.selected.encrypted || (ui.zip && ui.zip.needsPhrase)) {
        show("phrase");
        $("#phrase-count").textContent = msg;
        $("#phrase-count").className = "hint err";
      } else {
        show(ui.source === "zip" ? "source" : "pick");
        showError(msg);
      }
    }
  }

  const stageOrder = ["start", "connecting", "headers", "rescan", "peers"];
  function elapsed() {
    if (!ui.rescanStart) return "0:00";
    const s = Math.floor((Date.now() - ui.rescanStart) / 1000);
    const m = Math.floor(s / 60), h = Math.floor(m / 60);
    return (h ? h + ":" + String(m % 60).padStart(2, "0") : String(m)) + ":" + String(s % 60).padStart(2, "0");
  }
  function remainingText(sec) {
    if (sec == null || sec < 0) return "estimating";
    if (sec < 60) return "under a minute";
    const m = Math.round(sec / 60);
    if (m < 60) return "about " + m + " min";
    const h = Math.floor(m / 60), r = m % 60;
    return "about " + h + " h" + (r ? " " + r + " min" : "");
  }
  setInterval(() => {
    if (ui.screen !== "sync") return;
    if (ui.rescanStart) $("#live-elapsed").textContent = elapsed();
  }, 1000);
  function setStage(stage) {
    const idx = stageOrder.indexOf(stage);
    $$("#sync-stages .stage").forEach((s) => {
      const i = stageOrder.indexOf(s.dataset.stage);
      s.classList.toggle("done", i < idx);
      s.classList.toggle("active", i === idx);
    });
  }
  function resetSync() {
    $("#sync-fill").style.width = "0%";
    $("#sync-percent").textContent = "0%";
    $("#sync-blocks").textContent = "";
    $("#sync-message").textContent = "Starting the node...";
    setStage("start");
  }
  function onSync(p) {
    if (ui.screen !== "sync") return;
    const unknown = p.percent < 0;
    const pct = unknown ? 0 : Math.max(0, Math.min(100, p.percent || 0));
    $("#sync-fill").style.width = unknown ? "100%" : pct + "%";
    $("#sync-fill").classList.toggle("indeterminate", unknown);
    $("#sync-fill").classList.toggle("shimmer", !unknown && p.stage !== "synced");
    $("#sync-percent").textContent = unknown ? "working" : (p.stage === "synced" ? "100%" : pct.toFixed(1) + "%");
    const rescan = p.stage === "rescan";
    $("#sync-live").classList.toggle("hidden", !rescan);
    if (rescan) {
      if (!ui.rescanStart) ui.rescanStart = Date.now();
      $("#sync-blocks").textContent = unknown ? "" : "about block " + p.height.toLocaleString("en-US") + " of " + p.target.toLocaleString("en-US");
      $("#live-found").textContent = String(p.found || 0);
      $("#live-through").textContent = p.throughTime ? new Date(p.throughTime * 1000).toLocaleDateString(undefined, { dateStyle: "medium" }) : "not yet";
      $("#live-elapsed").textContent = elapsed();
      $("#live-remaining").textContent = remainingText(p.remaining);
    } else {
      ui.rescanStart = 0;
      $("#sync-blocks").textContent = p.height ? "block " + p.height.toLocaleString("en-US") + " of about " + p.target.toLocaleString("en-US") : "";
    }
    $("#sync-message").textContent = p.message || "";
    setStage(p.stage === "synced" ? "peers" : p.stage);
  }

  async function startSync() {
    resetSync();
    show("sync");
    setBusy(true);
    try {
      const st = await api.StartAndSync();
      ui.status = st;
      renderWallet();
      show("wallet");
    } catch (e) {
      await refreshState();
      show("welcome");
      if (!isCancel(e)) showError(errMsg(e));
    } finally { setBusy(false); }
  }

  // ---------------------------------------------------------------- wallet

  function renderWallet() {
    const st = ui.status;
    if (!st) return;
    $("#stat-channels").textContent = fmtSat(st.inChannels);
    $("#stat-channels-sub").textContent = st.channels.length ? st.channels.length + " channel" + (st.channels.length > 1 ? "s" : "") + (fmtBtc(st.inChannels) ? ", " + fmtBtc(st.inChannels) : "") : "no open channels";
    $("#stat-pending").textContent = fmtSat(st.inPending);
    $("#stat-pending-sub").textContent = st.pending.length ? st.pending.length + " close" + (st.pending.length > 1 ? "s" : "") + " in progress" : "";
    $("#stat-onchain").textContent = fmtSat(st.onchainConfirmed);
    $("#stat-onchain-sub").textContent = st.onchainUnconfirmed ? "+ " + fmtSat(st.onchainUnconfirmed) + " unconfirmed" : (fmtBtc(st.onchainConfirmed) || "");

    const hasChannels = st.channels.length > 0;
    const hasOnchain = st.onchainConfirmed > 0;
    $("#btn-withdraw").classList.toggle("hidden", !hasChannels);
    $("#btn-sweep").classList.toggle("hidden", !hasOnchain);

    let advice;
    if (hasChannels) {
      advice = "Your funds sit in Lightning channels. Close them to move the money to a bitcoin address you control. Channels whose peer is online close within minutes.";
    } else if (st.pending.length) {
      advice = "Channels are closing. Leave this window open, or come back later, until the funds show as ready to send. Then send the on-chain balance.";
    } else if (hasOnchain) {
      advice = "Everything is on-chain and spendable. Send it to a bitcoin address you control.";
    } else {
      advice = "Nothing left to recover in this app.";
    }
    $("#wallet-advice").textContent = advice;

    const lists = $("#wallet-lists");
    lists.innerHTML = "";
    (st.warnings || []).forEach((w) => lists.appendChild(el("div", "notice notice-warn", w)));
    if (st.channels.length) {
      lists.appendChild(el("div", "list-title", "Channels"));
      const list = el("div", "list");
      st.channels.forEach((c) => {
        const item = el("div", "item static");
        const main = el("div", "item-main");
        main.appendChild(el("div", "item-title", fmtSat(c.localBalance) + " yours"));
        main.appendChild(el("div", "item-sub", c.channelPoint));
        item.appendChild(main);
        item.appendChild(el("span", "tag " + (c.active ? "ok" : "warn"), c.active ? "peer online" : "peer offline"));
        list.appendChild(item);
      });
      lists.appendChild(list);
    }
    if (st.pending.length) {
      lists.appendChild(el("div", "list-title", "Closing"));
      const list = el("div", "list");
      st.pending.forEach((p) => {
        const item = el("div", "item static");
        const main = el("div", "item-main");
        main.appendChild(el("div", "item-title", fmtSat(p.amount)));
        main.appendChild(el("div", "item-sub", p.closingTxid || p.channelPoint));
        item.appendChild(main);
        let tag;
        if (p.kind === "force") tag = p.blocksToMature > 0 ? "spendable in " + fmtBlocks(p.blocksToMature) : "waiting for confirmation";
        else if (p.kind === "cooperative") tag = "confirming";
        else tag = "waiting for close transaction";
        item.appendChild(el("span", "tag", tag));
        list.appendChild(item);
      });
      lists.appendChild(list);
    }
    $("#wallet-node").textContent = "Node " + st.nodeId + ", block " + st.blockHeight.toLocaleString("en-US") + ", " + st.peers + " channel peer" + (st.peers === 1 ? "" : "s");
  }

  async function refreshStatus() {
    setBusy(true);
    try {
      ui.status = await api.GetStatus();
      renderWallet();
      show("wallet");
    } catch (e) { showError(errMsg(e)); }
    finally { setBusy(false); }
  }

  // ---------------------------------------------------------------- address validation

  function bindAddress(inputSel, hintSel) {
    const input = $(inputSel), hint = $(hintSel);
    let timer = null;
    input.addEventListener("input", () => {
      clearTimeout(timer);
      const v = input.value.trim();
      if (!v) { hint.textContent = ""; hint.className = "hint"; input.classList.remove("invalid"); return; }
      timer = setTimeout(async () => {
        try {
          await api.ValidateAddress(v);
          hint.textContent = "Address looks valid";
          hint.className = "hint ok";
          input.classList.remove("invalid");
        } catch (e) {
          hint.textContent = "This is not a valid bitcoin address";
          hint.className = "hint err";
          input.classList.add("invalid");
        }
      }, 250);
    });
  }
  async function checkAddress(inputSel, hintSel) {
    const input = $(inputSel), hint = $(hintSel);
    const v = input.value.trim();
    if (!v) { hint.textContent = "Enter the address that should receive the funds"; hint.className = "hint err"; return null; }
    try { await api.ValidateAddress(v); return v; }
    catch (e) { hint.textContent = "This is not a valid bitcoin address"; hint.className = "hint err"; input.classList.add("invalid"); return null; }
  }

  // ---------------------------------------------------------------- close channels

  async function doClose(force) {
    const address = ui.closeAddress || await checkAddress("#close-address", "#close-address-hint");
    if (!address) return;
    ui.closeAddress = address;
    ui.closeForce = force;
    working("Closing channels", true);
    setBusy(true);
    try {
      const res = await api.CloseChannels(address, force);
      renderCloseResult(res, force);
      show("closeresult");
    } catch (e) {
      if (isCancel(e)) { await refreshStatus(); return; }
      show("withdraw");
      showError(errMsg(e));
    } finally { setBusy(false); }
  }

  function renderCloseResult(res, forced) {
    const chans = res.channels || [];
    const list = $("#closeresult-list");
    list.innerHTML = "";
    chans.forEach((c) => {
      const item = el("div", "item static");
      const main = el("div", "item-main");
      let title, tag, cls;
      switch (c.status) {
        case "closing": title = "Closing cooperatively"; tag = "closing"; cls = "ok"; break;
        case "force_closing": title = "Force closing, funds arrive after the delay"; tag = "force close"; cls = "warn"; break;
        case "skipped": title = "Skipped, peer offline"; tag = "skipped"; cls = "warn"; break;
        default: title = "Failed: " + (c.error || "unknown error"); tag = "failed"; cls = "err";
      }
      main.appendChild(el("div", "item-title", title));
      main.appendChild(el("div", "item-sub", c.txid || c.channelPoint));
      item.appendChild(main);
      item.appendChild(el("span", "tag " + cls, tag));
      list.appendChild(item);
    });
    const closed = res.closed || 0, skipped = res.skipped || 0;
    if (!chans.length) {
      $("#closeresult-title").textContent = "No open channels";
      $("#closeresult-lead").textContent = "There was nothing to close. Check your funds for closes in progress or an on-chain balance.";
    } else if (closed && !skipped) {
      $("#closeresult-title").textContent = "Channels closing";
      $("#closeresult-lead").textContent = "The closing transactions are broadcast. Cooperative closes confirm within about an hour and pay straight to " + ui.closeAddress + ". Force closes land in this app first and can be sent on once they mature.";
    } else if (closed && skipped) {
      $("#closeresult-title").textContent = "Some channels closing";
      $("#closeresult-lead").textContent = closed + " channel" + (closed > 1 ? "s are" : " is") + " closing. " + skipped + " could not be closed cooperatively because the peer is offline. You can force close them; those funds become spendable after a delay of up to about five days.";
    } else {
      $("#closeresult-title").textContent = "Channels could not be closed";
      $("#closeresult-lead").textContent = "No peer answered. You can force close the channels; the funds become spendable in this app after a delay of up to about five days, then send them on.";
    }
    $("#btn-force-skipped").classList.toggle("hidden", !(skipped > 0 && !forced));
  }

  // ---------------------------------------------------------------- sweep

  function resetSweep() {
    ui.sweepPlan = null;
    $("#sweep-plan").classList.add("hidden");
    $("#btn-prepare-sweep").classList.remove("hidden");
    $("#btn-broadcast-sweep").classList.add("hidden");
    $("#sweep-address").disabled = false;
  }

  async function prepareSweep() {
    const address = await checkAddress("#sweep-address", "#sweep-address-hint");
    if (!address) return;
    setBusy(true);
    try {
      const plan = await api.PrepareSweep(address);
      ui.sweepPlan = plan;
      $("#sweep-amount").textContent = fmtSat(plan.amount) + (fmtBtc(plan.amount) ? " (" + fmtBtc(plan.amount) + ")" : "");
      const opts = $("#sweep-options");
      opts.innerHTML = "";
      const names = { 2: ["Fast", "next few blocks, about 20 minutes"], 6: ["Normal", "about an hour"], 25: ["Slow", "a few hours"] };
      ui.sweepTarget = plan.options.some((o) => o.confTarget === 6) ? 6 : plan.options[0].confTarget;
      plan.options.forEach((o) => {
        const opt = el("div", "option" + (o.confTarget === ui.sweepTarget ? " selected" : ""));
        const main = el("div", "option-main");
        const n = names[o.confTarget] || [o.confTarget + " blocks", ""];
        main.appendChild(el("div", "option-title", n[0]));
        main.appendChild(el("div", "option-sub", n[1]));
        opt.appendChild(main);
        opt.appendChild(el("span", "amount", fmtSat(o.fee) + " fee"));
        opt.addEventListener("click", () => {
          ui.sweepTarget = o.confTarget;
          $$("#sweep-options .option").forEach((x) => x.classList.remove("selected"));
          opt.classList.add("selected");
        });
        opts.appendChild(opt);
      });
      $("#sweep-plan").classList.remove("hidden");
      $("#btn-prepare-sweep").classList.add("hidden");
      $("#btn-broadcast-sweep").classList.remove("hidden");
      $("#sweep-address").disabled = true;
    } catch (e) { showError(errMsg(e)); }
    finally { setBusy(false); }
  }

  async function broadcastSweep() {
    setBusy(true);
    try {
      const txid = await api.BroadcastSweep(ui.sweepTarget);
      ui.lastTxid = txid;
      $("#done-txid").value = txid;
      resetSweep();
      $("#sweep-address").value = "";
      show("done");
    } catch (e) { showError(errMsg(e)); }
    finally { setBusy(false); }
  }

  // ---------------------------------------------------------------- actions

  const actions = {
    "dismiss-error": clearError,
    "start": () => { ui.force = false; show("source"); },
    "continue-existing": () => startSync(),
    "restore-other": () => { ui.force = true; show("source"); },
    "back-welcome": async () => { await refreshState(); show("welcome"); },
    "choose-workdir": async () => { const p = await api.ChooseWorkDir(); if (p) $("#workdir").value = p; },
    "apply-settings": applySettings,
    "forget-signins": async () => { await api.ForgetSignIns(); },
    "source-google": () => listFrom("google"),
    "source-icloud": () => listFrom("icloud"),
    "source-zip": chooseZip,
    "cancel": () => api.Cancel(),
    "copy-signin": () => api.CopyText($("#signin-url").value),
    "open-signin": () => api.OpenURL($("#signin-url").value),
    "back-source": () => show("source"),
    "pick-continue": pickContinue,
    "phrase-back": () => show(ui.source === "zip" ? "source" : "pick"),
    "phrase-continue": phraseContinue,
    "refresh": refreshStatus,
    "go-withdraw": () => { $("#close-address-hint").textContent = ""; ui.closeAddress = ""; show("withdraw"); $("#close-address").focus(); },
    "go-sweep": () => { resetSweep(); $("#sweep-address-hint").textContent = ""; show("sweep"); $("#sweep-address").focus(); },
    "back-wallet": () => { renderWallet(); show("wallet"); },
    "back-wallet-refresh": refreshStatus,
    "do-close": () => { ui.closeAddress = ""; doClose($("#close-force").checked); },
    "force-skipped": () => doClose(true),
    "prepare-sweep": prepareSweep,
    "broadcast-sweep": broadcastSweep,
    "copy-txid": () => api.CopyText(ui.lastTxid),
    "open-txid": () => api.OpenURL("https://mempool.space/tx/" + ui.lastTxid),
    "save-log": async () => { try { const p = await api.SaveLog(); if (p) appendLog([new Date().toLocaleTimeString() + "  [recovery] log saved to " + p]); } catch (e) { showError(errMsg(e)); } },
    "copy-log": () => api.CopyLog(),
    "open-workdir": () => api.OpenWorkDir(),
    "close-log": () => toggleLog(false),
  };

  document.addEventListener("click", (ev) => {
    const btn = ev.target.closest("[data-action]");
    if (!btn) return;
    const fn = actions[btn.dataset.action];
    if (fn) { clearError(); Promise.resolve(fn()).catch((e) => showError(errMsg(e))); }
  });
  $("#log-toggle").addEventListener("click", () => toggleLog());
  $("#phrase").addEventListener("input", () => {
    const n = $("#phrase").value.trim().split(/\s+/).filter(Boolean).length;
    $("#phrase-count").textContent = n + (n === 1 ? " word" : " words");
    $("#phrase-count").className = "hint";
  });
  $("#phrase").addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); phraseContinue(); } });
  $("#close-address").addEventListener("keydown", (ev) => { if (ev.key === "Enter") actions["do-close"](); });
  $("#sweep-address").addEventListener("keydown", (ev) => { if (ev.key === "Enter" && !ui.sweepPlan) prepareSweep(); });
  bindAddress("#close-address", "#close-address-hint");
  bindAddress("#sweep-address", "#sweep-address-hint");

  // ---------------------------------------------------------------- boot

  function bindEvents() {
    rt.EventsOn("log", (lines) => appendLog(Array.isArray(lines) ? lines : [String(lines)]));
    rt.EventsOn("progress", (msg) => pushRecent(String(msg)));
    rt.EventsOn("sync", onSync);
    rt.EventsOn("signin", (info) => {
      $("#signin-url").value = info.url;
      $("#signin-link").classList.remove("hidden");
    });
  }

  async function boot() {
    let tries = 0;
    while ((!window.go || !window.go.main || !window.runtime) && tries++ < 200) {
      await new Promise((r) => setTimeout(r, 25));
    }
    if (!window.go || !window.go.main) {
      document.body.innerHTML = "<p style='padding:24px'>The app runtime did not load. Please restart Breez Recovery.</p>";
      return;
    }
    api = window.go.main.App;
    rt = window.runtime;
    bindEvents();
    const existing = await api.GetLog();
    if (existing && existing.length) appendLog(existing);
    await refreshState();
    show("welcome");
  }

  boot();
})();
