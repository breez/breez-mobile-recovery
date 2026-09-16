// Mock of the Wails bindings for design screenshots.
(function () {
  const handlers = {};
  window.runtime = { EventsOn: (n, cb) => { handlers[n] = cb; } };
  const emit = (n, d) => handlers[n] && handlers[n](d);
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const q = new URLSearchParams(location.search);
  const hasNode = q.get("hasNode") === "1";
  const scenario = q.get("scenario") || "channels";
  const slow = q.get("slow") || "";
  const snaps = [
    { nodeId: "02e66bcb1f4a9d6b3c2a7f8e9d0c1b2a3f4e5d6c7b8a9f0e1d2c3b4a5f6e7d8c9b", modifiedTime: "2026-03-31T14:03:00Z", encrypted: false, encryptionType: "" },
    { nodeId: "02f8439e7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e", modifiedTime: "2023-07-30T09:12:00Z", encrypted: true, encryptionType: "Mnemonics12" },
    { nodeId: "02c6b28e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f", modifiedTime: "2022-12-09T18:40:00Z", encrypted: true, encryptionType: "Mnemonics" },
    { nodeId: "02b134bb9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c", modifiedTime: "2021-10-17T11:00:00Z", encrypted: true, encryptionType: "PIN" },
  ];
  const nodeId = snaps[0].nodeId;
  const statuses = {
    channels: { nodeId, blockHeight: 967302, synced: true, peers: 8, onchainConfirmed: 0, onchainUnconfirmed: 0, inChannels: 1583210, inPending: 0, pending: [],
      channels: [
        { channelPoint: "7f3a1c9e2b8d4f6a0c5e3b1d9f7a2c4e6b8d0f1a3c5e7b9d2f4a6c8e0b1d3f5a7c:1", localBalance: 1250000, remoteBalance: 750000, capacity: 2000000, active: true },
        { channelPoint: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90:0", localBalance: 333210, remoteBalance: 66790, capacity: 400000, active: false },
      ] },
    pending: { nodeId, blockHeight: 967302, synced: true, peers: 8, onchainConfirmed: 0, onchainUnconfirmed: 0, inChannels: 0, inPending: 333210, channels: [],
      pending: [{ channelPoint: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90:0", kind: "force", closingTxid: "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b", amount: 333210, blocksToMature: 612 }] },
    onchain: { nodeId, blockHeight: 967302, synced: true, peers: 8, onchainConfirmed: 1581900, onchainUnconfirmed: 0, inChannels: 0, inPending: 0, channels: [], pending: [] },
  };
  const status = statuses[scenario];
  let logn = 0;
  const log = (l) => emit("log", [l]);
  setInterval(() => log("2026-09-16 20:10:" + String(logn++ % 60).padStart(2, "0") + ".123 [INF] DAEM: Sync to chain interval Synced=false BlockHeight=" + (967290 + logn)), 700);
  const progress = (m) => { emit("progress", m); log(new Date().toLocaleTimeString() + "  [recovery] " + m); };
  window.go = { main: { App: {
    GetState: async () => ({ version: "0.1.0", os: "linux", workDir: "/home/roys/.breez-recovery", peers: "", hasNode, logPath: "", googleConfigured: true }),
    ApplySettings: async () => ({}),
    GetLog: async () => ["20:07:01  [recovery] Breez Recovery 0.1.0 on linux/amd64", "20:07:01  [recovery] Work dir: /home/roys/.breez-recovery"],
    ListGoogle: async () => {
      emit("signin", { provider: "google", url: "https://accounts.google.com/o/oauth2/v2/auth?client_id=463327817067-pp4c.apps.googleusercontent.com&redirect_uri=http%3A%2F%2F127.0.0.1%3A45401%2F&scope=drive.appdata" });
      progress("Waiting for the Google sign-in in your browser...");
      await sleep(slow === "list" ? 600000 : 1500);
      progress("Signed in to Google. Looking for Breez backups...");
      await sleep(400);
      return snaps;
    },
    ListICloud: async () => { emit("signin", { provider: "icloud", url: "https://idmsa.apple.com/IDMSWebAuth/auth?oauth_token=OATTKN..." }); await sleep(1500); return snaps; },
    ChooseZip: async () => ({ path: "/home/roys/Downloads/backup.zip", name: "backup.zip", needsPhrase: true }),
    CheckPhrase: async (p) => { const n = p.split(" ").length; if (n !== 12 && n !== 24) throw new Error("expected 12 or 24 words, got " + n); return n === 12 ? "Mnemonics12" : "Mnemonics"; },
    Restore: async () => {
      progress("Downloading backup of node 02e66bcb1f4a... (from 2026-03-31 14:03)...");
      await sleep(1200);
      progress("Decrypting and placing the node files...");
      await sleep(slow === "restore" ? 600000 : 1000);
      progress("Backup restored into /home/roys/.breez-recovery.");
    },
    StartAndSync: async () => {
      progress("Starting the node..."); await sleep(800); progress("Node is up.");
      const steps = [["connecting", 0, 0, 0, "Connecting to the bitcoin network..."], ["headers", 589000, 967310, 3, ""], ["headers", 700000, 967310, 6, ""], ["headers", 850000, 967310, 8, ""], ["headers", 940000, 967310, 8, ""], ["rescan", 557139, 967310, 8, "Checking every block since block 557139 for your channel and payment history (938 addresses). The bar starts moving with the first transaction found.", -1, 0, 0], ["rescan", 612400, 967310, 8, "Checked at least through block 612400 of 967310. The bar moves each time a transaction is found.", 13.5, 7, 1583000000], ["synced", 967310, 967310, 8, "Synced to the chain at block 967310"]];
      for (const [stage, height, target, peers, msg, pct, found, through] of steps) {
        emit("sync", { stage, height, target, peers, percent: pct !== undefined ? pct : (stage === "synced" ? 100 : Math.min(99, height / target * 100)), message: msg || ("Catching up with the bitcoin chain, block " + height + " of about " + target), found: found || 0, throughTime: through || 0 });
        await sleep(slow === "sync" && height === 700000 ? 600000 : slow === "rescan1" && height === 557139 ? 600000 : slow === "rescan2" && height === 612400 ? 600000 : 600);
      }
      progress("Connecting to channel peers..."); await sleep(500);
      return status;
    },
    GetStatus: async () => status,
    ValidateAddress: async (a) => { if (!/^(bc1|1|3)[a-zA-Z0-9]{20,}$/.test(a)) throw new Error("invalid"); },
    CloseChannels: async (addr, force) => {
      progress("Giving channel peers a moment to connect..."); await sleep(800);
      progress("Closing 2 channel(s) cooperatively, funds to " + addr + "..."); await sleep(800);
      progress("Keeping the node running so the closing transactions are broadcast..."); await sleep(600);
      return { closed: force ? 2 : 1, skipped: force ? 0 : 1, channels: [
        { channelPoint: statuses.channels.channels[0].channelPoint, status: "closing", txid: "e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3" },
        { channelPoint: statuses.channels.channels[1].channelPoint, status: force ? "force_closing" : "skipped", txid: force ? "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b" : "" },
      ] };
    },
    PrepareSweep: async (addr) => { await sleep(600); return { address: addr, amount: 1581900, options: [{ confTarget: 2, fee: 1840, txid: "a" }, { confTarget: 6, fee: 920, txid: "b" }, { confTarget: 25, fee: 410, txid: "c" }] }; },
    BroadcastSweep: async () => { await sleep(600); return "5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e"; },
    Cancel: async () => {}, CopyText: async () => {}, OpenURL: async () => {},
    SaveLog: async () => "/home/roys/breez-recovery-2026-09-16.log", CopyLog: async () => {}, OpenWorkDir: async () => {},
    ChooseWorkDir: async () => "/home/roys/wallet", ForgetSignIns: async () => {},
  } } };
})();
