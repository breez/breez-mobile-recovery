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
    sweepPlan: null,
    sweepTarget: 6,
    lastTxid: "",
    recent: [],           // recent progress lines
    rescanStart: 0,       // when the history check began, for the elapsed display
    logCount: 0,
    logOpen: false,
    busy: false,
    useName: "",          // restored backup picked on the start screen
    currentName: "",      // restored backup in use
    restored: new Set(),  // names (node ids) of the backups restored here
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
    // The whole row, so an absent Cancel leaves no gap under the text.
    $("#working-cancel").parentElement.classList.toggle("hidden", cancellable === false);
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
    $("#welcome-existing").classList.toggle("hidden", !ui.state.hasNode);
    $("#welcome-fresh").classList.toggle("hidden", ui.state.hasNode);
    $("#existing-node-path").textContent = ui.state.nodeDir || ui.state.workDir;
    await renderRestored();
  }

  // The apps restored on this computer: one shows as a summary, several as a
  // list to pick the one to continue with, the last opened first.
  async function renderRestored() {
    const apps = (ui.state.hasNode && (await api.RestoredApps())) || [];
    const opened = (a) => (a.lastOpened ? new Date(a.lastOpened).getTime() : 0);
    apps.sort((x, y) => opened(y) - opened(x));
    const many = apps.length > 1;
    const current = apps.find((a) => a.current);
    ui.currentName = current ? current.name : "";
    if (!apps.some((a) => a.name === ui.useName)) ui.useName = ui.currentName;
    $("#existing-summary").classList.toggle("hidden", many);
    $("#restored-list").classList.toggle("hidden", !many);
    $("#existing-lead").textContent = many
      ? "Breez apps restored on this computer. Choose the one to continue with, or restore another backup."
      : "A Breez app restored earlier is on this computer. Continue to sync it and move the funds, or restore another backup.";
    const list = $("#restored-list");
    list.innerHTML = "";
    if (!many) return;
    apps.forEach((a) => {
      const item = el("div", "item" + (a.name === ui.useName ? " selected" : ""));
      item.title = a.dir;
      const main = el("div", "item-main");
      main.appendChild(el("div", "item-title", a.lastOpened ? "Last opened " + fmtDate(a.lastOpened) : "Not opened yet"));
      main.appendChild(el("div", "item-sub", a.name.startsWith("zip-") ? "From a backup file" : a.name));
      item.appendChild(main);
      // What its funds screen showed last; nothing when that was nothing,
      // "settling" when a close with a payout still unknown was under way.
      const f = a.funds;
      const total = f ? f.inChannels + f.pending + f.onchain : 0;
      if (total > 0 || (f && f.settling)) {
        const amount = el("span", "item-amount" + (total > 0 ? "" : " muted"), total > 0 ? fmtSat(total) : "settling");
        amount.title = "Last known funds, " + fmtDate(f.at);
        item.appendChild(amount);
      }
      if (a.current) item.appendChild(el("span", "tag", "In use"));
      item.addEventListener("click", () => {
        ui.useName = a.name;
        $$("#restored-list .item").forEach((x) => x.classList.toggle("selected", x === item));
      });
      list.appendChild(item);
    });
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
      renderSnapshots(await restoredNames());
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

  async function restoredNames() {
    return new Set(((await api.RestoredApps()) || []).map((a) => a.name));
  }

  // Back on the list after a phrase or a failed restore, which may have
  // placed the folder already: the tags are read again, the pick is kept.
  async function showPick() {
    renderSnapshots(await restoredNames());
    show("pick");
  }

  function renderSnapshots(restored) {
    ui.restored = restored;
    const keep = ui.selected && ui.selected.nodeId;
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
      // Said before the pick: choosing one restored here asks whether to
      // continue with it or restore it again.
      const tags = (i === 0 ? ["Latest"] : []).concat(ui.restored.has(s.nodeId) ? ["Restored"] : []);
      tags.forEach((text) => {
        const t = el("span", "tag", text);
        t.style.marginLeft = "8px";
        title.appendChild(t);
      });
      main.appendChild(title);
      main.appendChild(el("div", "item-sub", s.nodeId));
      item.appendChild(main);
      const lab = encLabel(s);
      item.appendChild(el("span", "tag " + lab.cls, lab.text));
      if (!unsupported) {
        if (s.nodeId === keep) {
          ui.selected = s;
          item.classList.add("selected");
          $("#pick-continue").disabled = false;
        }
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
        if (ui.source === "zip") show("source"); else await showPick();
        showError(msg);
      }
    }
  }

  const stageOrder = ["start", "connecting", "headers", "addresses", "rescan", "channels"];
  function elapsed() {
    if (!ui.rescanStart) return "0:00";
    const s = Math.floor((Date.now() - ui.rescanStart) / 1000);
    const m = Math.floor(s / 60), h = Math.floor(m / 60);
    return (h ? h + ":" + String(m % 60).padStart(2, "0") : String(m)) + ":" + String(s % 60).padStart(2, "0");
  }
  function remainingText(sec) {
    if (sec < 60) return "<1 min";
    const m = Math.round(sec / 60);
    if (m < 60) return "~" + m + " min";
    const h = Math.floor(m / 60), r = m % 60;
    return "~" + h + " h" + (r ? " " + r + " min" : "");
  }
  // "estimating" with dots that come and go, left running across the
  // updates (twice a second) so the animation does not restart.
  function showRemaining(sec) {
    const box = $("#live-remaining");
    if (sec != null && sec >= 0) { box.textContent = remainingText(sec); return; }
    if (box.querySelector(".dots")) return;
    box.textContent = "estimating";
    const dots = el("span", "dots");
    for (let i = 0; i < 3; i++) dots.appendChild(el("i", null, "."));
    box.appendChild(dots);
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
    // The long stages show elapsed time and time left; each stage counts
    // from its own start.
    const rescan = p.stage === "rescan";
    const walk = p.stage === "channels" || p.stage === "addresses";
    const live = rescan || walk;
    if (ui.liveStage !== p.stage) { ui.liveStage = p.stage; ui.rescanStart = live ? Date.now() : 0; }
    $("#sync-live").classList.toggle("hidden", !live);
    $$("#sync-live .rescan-only").forEach((d) => d.classList.toggle("hidden", !rescan));
    if (live) {
      $("#live-elapsed").textContent = elapsed();
      showRemaining(p.remaining);
    }
    if (rescan) {
      $("#sync-blocks").textContent = unknown ? "" : "about block " + p.height.toLocaleString("en-US") + " of " + p.target.toLocaleString("en-US");
      $("#live-through").textContent = p.throughTime ? new Date(p.throughTime * 1000).toLocaleDateString(undefined, { dateStyle: "medium" }) : "not yet";
    } else {
      $("#sync-blocks").textContent = p.height ? "block " + p.height.toLocaleString("en-US") + " of " + (walk ? "" : "about ") + p.target.toLocaleString("en-US") : "";
    }
    $("#sync-message").textContent = p.message || "";
    setStage(p.stage === "synced" ? "channels" : p.stage);
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
      // The app relaunches itself and quits: no error, no welcome screen.
      if (/restart required/.test(errMsg(e))) { working("Restarting", false); return; }
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
    const collecting = (st.closedOnChain || []).filter((c) => c.collect > 0).length;
    $("#stat-pending-sub").textContent = st.pending.length ? st.pending.length + " close" + (st.pending.length > 1 ? "s" : "") + " in progress" : (collecting ? "being collected" : "");
    $("#stat-onchain").textContent = fmtSat(st.onchainConfirmed);
    $("#stat-onchain-sub").textContent = st.onchainUnconfirmed ? "+ " + fmtSat(st.onchainUnconfirmed) + " unconfirmed" : (fmtBtc(st.onchainConfirmed) || "");

    const hasChannels = st.channels.length > 0;
    const hasOnchain = st.onchainConfirmed > 0;
    $("#btn-copy-channels").classList.toggle("hidden", !hasChannels);
    $("#btn-copy-channels").textContent = "Copy the list";
    $("#btn-sweep").classList.toggle("hidden", !hasOnchain);

    let advice;
    if (hasChannels) {
      advice = "Channels still open. Email the list to contact@breez.technology.";
    } else if ((st.closedOnChain || []).some((c) => c.collect > 0)) {
      advice = "Collecting funds from a closed channel. Keep the app open.";
    } else if (st.unresolved) {
      advice = "A closed channel is being settled. Keep the app open.";
    } else if (st.pending.length) {
      advice = "Channels are closing. Leave this window open, or come back later, until the funds show as ready to send. Then send the on-chain balance.";
    } else if (hasOnchain) {
      advice = "Everything is on-chain and spendable. Send it to a bitcoin address you control.";
    } else if (st.outgoing) {
      advice = "Your transaction is waiting for its confirmation.";
    } else if (st.onchainUnconfirmed > 0) {
      advice = "Funds are waiting for their confirmation. Keep the app open.";
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
        main.appendChild(el("div", "item-title", fmtSat(c.localBalance) + " yours" + (c.dust ? ", dust" : "")));
        main.appendChild(el("div", "item-sub", c.channelPoint));
        item.appendChild(main);
        item.appendChild(el("span", "tag " + (c.active ? "ok" : "warn"), c.active ? "peer online" : "peer offline"));
        list.appendChild(item);
      });
      lists.appendChild(list);
    }
    if ((st.closedOnChain || []).length) {
      lists.appendChild(el("div", "list-title", "Closed on chain"));
      const list = el("div", "list");
      st.closedOnChain.forEach((c) => {
        const item = el("div", "item static");
        const main = el("div", "item-main");
        main.appendChild(el("div", "item-title", c.collect > 0 ? fmtSat(c.collect) + " being collected" : "Closed after this backup"));
        main.appendChild(el("div", "item-sub", c.closingTxid));
        item.appendChild(main);
        item.appendChild(txLink(c.closingTxid));
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
    // One line in the footer; the full node id is in the tooltip and the
    // restored folder's name.
    $("#wallet-node").textContent = "Node " + shortId(st.nodeId) + " · block " + st.blockHeight.toLocaleString("en-US") + " · " + st.peers + " channel peer" + (st.peers === 1 ? "" : "s");
    $("#wallet-node").title = st.nodeId;
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

  // While money is on its way (a closed channel being collected, a close
  // maturing, an unconfirmed balance) the funds screen keeps itself current,
  // quietly: no busy state, and errors wait for the next manual refresh.
  function isMoving(st) {
    return !!st && !!(st.pending.length || st.unresolved || st.outgoing || st.onchainUnconfirmed > 0 || (st.closedOnChain || []).some((c) => c.collect > 0));
  }
  setInterval(async () => {
    if (ui.screen !== "wallet" || ui.busy || !isMoving(ui.status)) return;
    try { ui.status = await api.GetStatus(); if (ui.screen === "wallet") renderWallet(); } catch (e) { /* next round */ }
  }, 30000);

  // ---------------------------------------------------------------- history

  function txLink(txid) {
    const a = el("span", "link", "View");
    a.addEventListener("click", () => api.OpenURL("https://mempool.space/tx/" + txid));
    return a;
  }
  function addrLink(address) {
    const a = el("span", "link", "View");
    a.addEventListener("click", () => api.OpenURL("https://mempool.space/address/" + address));
    return a;
  }
  function fmtTime(t) { return t ? new Date(t * 1000).toLocaleDateString(undefined, { dateStyle: "medium" }) : ""; }

  async function openHistory() {
    setBusy(true);
    try {
      const h = await api.GetHistory();
      renderHistory(h);
      show("history");
    } catch (e) { showError(errMsg(e)); }
    finally { setBusy(false); }
  }

  function fmtSigned(n) {
    n = Number(n || 0);
    return (n > 0 ? "+" : n < 0 ? "\u2212" : "") + Math.abs(n).toLocaleString("en-US") + " sat";
  }
  function fmtDay(t) { return t ? new Date(t * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "Date unknown"; }

  function renderHistory(h) {
    const totals = $("#history-totals"), warns = $("#history-warnings"), list = $("#history-list"), foot = $("#history-foot");
    totals.innerHTML = ""; warns.innerHTML = ""; list.innerHTML = ""; foot.textContent = "";

    const t = h.totals;
    const row = (label, value, cls) => {
      const r = el("div", "ledger-total" + (cls ? " " + cls : ""));
      r.appendChild(el("span", null, label));
      r.appendChild(el("span", "mono", value));
      totals.appendChild(r);
    };
    row("Received", fmtSigned(t.in));
    row("Sent", fmtSigned(-t.out));
    row("Fees", fmtSigned(-t.fees));
    row("Total of this list", fmtSigned(t.expected), "ledger-total-strong");
    row("In this app now", fmtSat(t.held), "ledger-total-strong");
    const parts = [];
    if (t.onchain) parts.push(fmtSat(t.onchain) + " on-chain");
    if (t.inChannels) parts.push(fmtSat(t.inChannels) + " in channels");
    if (t.inPending) parts.push(fmtSat(t.inPending) + " in closing channels");
    if (parts.length > 1) totals.appendChild(el("div", "ledger-total-sub", parts.join(", ")));
    if (t.uncollected) row("Set aside by channel closes, not collected yet", fmtSat(t.uncollected));

    (h.warnings || []).forEach((w) => warns.appendChild(el("div", "notice notice-warn", w)));

    if (!h.entries.length) list.appendChild(el("p", "muted", "No payments or on-chain movements were found."));
    h.entries.forEach((e) => {
      const row = el("div", "ledger-row" + (e.delta === 0 ? " ledger-move" : ""));
      row.appendChild(el("div", "ledger-date", fmtDay(e.time)));

      const main = el("div", "ledger-main");
      const head = el("div", "ledger-title");
      head.appendChild(el("span", null, e.title));
      if (e.status !== "done") head.appendChild(el("span", "tag " + (e.status === "closing" || e.status === "unconfirmed" ? "warn" : "ok"), e.status === "closing" ? "in progress" : e.status));
      main.appendChild(head);
      if (e.detail) main.appendChild(el("div", "ledger-detail", e.detail));
      if (e.note) main.appendChild(el("div", "ledger-detail", e.note));
      if (e.fee && e.feeNote) main.appendChild(el("div", "ledger-detail", "Fee taken by the " + e.feeNote + " before it arrived: " + fmtSat(e.fee)));
      if (e.txid) {
        const l = txLink(e.txid); l.textContent = "View transaction";
        main.appendChild(l);
      }
      row.appendChild(main);

      const amt = el("div", "ledger-amount");
      const sign = e.delta > 0 ? "in" : e.delta < 0 ? "out" : "move";
      amt.appendChild(el("div", "ledger-value " + sign, sign === "in" ? fmtSigned(e.amount) : sign === "out" ? fmtSigned(-e.amount) : fmtSat(e.amount)));
      if (e.fee && !e.feeNote && e.fee !== e.amount) amt.appendChild(el("div", "ledger-fee", "fee " + fmtSat(e.fee)));
      row.appendChild(amt);
      list.appendChild(row);
    });

    if (h.zeroCloses) foot.textContent = h.zeroCloses + (h.zeroCloses > 1 ? " channels" : " channel") + " closed with no balance of yours " + (h.zeroCloses > 1 ? "are" : "is") + " not listed.";
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
    "continue-existing": async () => {
      if (ui.busy) return; // a second click while switching
      if (ui.useName && ui.useName !== ui.currentName) {
        setBusy(true);
        let restarting = false;
        try {
          // After a node ran in this process the app restarts to switch.
          restarting = await api.UseRestored(ui.useName);
          if (!restarting) await refreshState();
        } finally {
          if (!restarting) setBusy(false);
        }
        if (restarting) { working("Restarting", false); return; }
      }
      startSync();
    },
    "restore-other": async () => {
      // Each backup has a folder of its own, so nothing is overwritten. When
      // the node already ran, the app restarts itself and opens on this step.
      ui.force = false;
      // Money still on its way moves only while this node runs: ask first.
      const ask = ui.screen === "done" || (ui.screen === "wallet" && isMoving(ui.status));
      try {
        if (await api.RestoreOther(ask)) { working("Restarting", false); return; }
      } catch (e) {
        if (isCancel(e)) return; // answered No
        throw e;
      }
      show("source");
    },
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
    "phrase-back": () => (ui.source === "zip" ? show("source") : showPick()),
    "phrase-continue": phraseContinue,
    "refresh": refreshStatus,
    "go-history": openHistory,
    "go-sweep": () => { resetSweep(); $("#sweep-address-hint").textContent = ""; show("sweep"); $("#sweep-address").focus(); },
    "back-wallet": () => { renderWallet(); show("wallet"); },
    "back-wallet-refresh": refreshStatus,
    "prepare-sweep": prepareSweep,
    "broadcast-sweep": broadcastSweep,
    "copy-txid": () => api.CopyText(ui.lastTxid),
    "copy-channels": async () => {
      // What support needs to find the channels on the LSP's side.
      const st = ui.status;
      if (!st) return;
      const lines = ["Breez Recovery " + ((ui.state && ui.state.version) || "") + ": channels still open", "Node " + st.nodeId, ""];
      st.channels.forEach((c) => {
        lines.push("Channel " + c.channelPoint);
        lines.push("  peer " + c.peer + (c.active ? " (online)" : " (offline)"));
        lines.push("  mine " + c.localBalance + " sat of " + c.capacity + " sat" + (c.dust ? " (below the dust limit of " + c.dustLimit + " sat)" : ""));
      });
      await api.CopyText(lines.join("\n"));
      $("#btn-copy-channels").textContent = "Copied";
    },
    "open-txid": () => api.OpenURL("https://mempool.space/tx/" + ui.lastTxid),
    "save-history": async () => { try { const p = await api.SaveHistory(); if (p) appendLog([new Date().toLocaleTimeString() + "  [recovery] history exported to " + p]); } catch (e) { showError(errMsg(e)); } },
    "save-log": async () => { try { const p = await api.SaveLog(); if (p) appendLog([new Date().toLocaleTimeString() + "  [recovery] log saved to " + p]); } catch (e) { showError(errMsg(e)); } },
    "copy-log": () => api.CopyLog(),
    "open-workdir": () => api.OpenWorkDir(),
    "close-log": () => toggleLog(false),
  };

  document.addEventListener("contextmenu", (ev) => ev.preventDefault());
  document.addEventListener("keydown", (ev) => {
    // No page reload or history navigation: the page is the app's state.
    if (ev.key === "F5" || ((ev.ctrlKey || ev.metaKey) && (ev.key === "r" || ev.key === "R")) || (ev.altKey && (ev.key === "ArrowLeft" || ev.key === "ArrowRight"))) ev.preventDefault();
  });
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
  $("#sweep-address").addEventListener("keydown", (ev) => { if (ev.key === "Enter" && !ui.sweepPlan) prepareSweep(); });
  bindAddress("#sweep-address", "#sweep-address-hint");

  // ---------------------------------------------------------------- boot

  function bindEvents() {
    rt.EventsOn("log", (lines) => appendLog(Array.isArray(lines) ? lines : [String(lines)]));
    rt.EventsOn("progress", (msg) => pushRecent(String(msg)));
    rt.EventsOn("sync", onSync);
    // Sent before the node stops, which takes a while.
    rt.EventsOn("restarting", () => { working("Restarting", false); setBusy(true); });
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
    if (ui.state.autoContinue) {
      working("Restarting", false);
      pushRecent("Continuing after the restart...");
    } else {
      show(ui.state.restoreOther ? "source" : "welcome");
    }
    if (ui.state.switchError) showError(ui.state.switchError);
    // A relaunched copy starts hidden: show it now, on the right screen.
    api.ShowWindow();
    if (ui.state.autoContinue) {
      // Relaunched by the app itself after preparing the node. This copy
      // only got this far once the old one had exited: core.New waits for
      // the work folder lock, which the old copy held to the end.
      startSync();
    }
  }

  boot();
})();
