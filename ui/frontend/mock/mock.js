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
    channels: { nodeId, blockHeight: 967302, synced: true, peers: 8, onchainConfirmed: 0, onchainUnconfirmed: 0, inChannels: 1583210, inPending: 0, unresolved: 0, outgoing: 0, pending: [],
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
    GetState: async () => ({ version: "0.1.0", os: "linux", workDir: "/home/roys/.breez-recovery", nodeDir: hasNode ? "/home/roys/.breez-recovery/backups/02e66bcb1e3c97de679c0d5b2f831ac913e53acdc99542ed839c17b1079df489ea" : "", restoreOther: false, peers: "", hasNode, logPath: "", googleConfigured: true }),
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
      progress("Backup restored into /home/roys/.breez-recovery/backups/" + nodeId + ".");
    },
    StartAndSync: async () => {
      progress("Starting the node..."); await sleep(800); progress("Node is up.");
      const steps = [["connecting", 0, 0, 0, "Connecting to the bitcoin network..."], ["headers", 589000, 967310, 3, ""], ["headers", 700000, 967310, 6, ""], ["headers", 850000, 967310, 8, ""], ["headers", 940000, 967310, 8, ""], ["rescan", 557139, 967310, 8, "Reached the chain tip. Now checking every block for your channel and payment history; this is the slow part of a first sync.", -1, 0, 0], ["rescan", 612400, 967310, 8, "Checking every block for your channel and payment history (938 addresses). Reached 29 Feb 2020.", 13.5, 7, 1583000000], ["synced", 967310, 967310, 8, "Synced to the chain at block 967310"]];
      let searched = false;
      for (const [stage, height, target, peers, msg, pct, found, through] of steps) {
        if (stage === "rescan" && !searched) {
          searched = true;
          for (const h of [758000, 860000, 967000]) {
            emit("sync", { stage: "addresses", height: h, target: 967310, peers: 8, percent: (h - 758000) / (967310 - 758000) * 100, message: "Looking for funds received after the last backup", found: 0, throughTime: 0, remaining: h === 758000 ? -1 : Math.round((967310 - h) / 550) });
            await sleep(slow === "addresses" && h === 860000 ? 600000 : 600);
          }
        }
        emit("sync", { stage, height, target, peers, percent: pct !== undefined ? pct : (stage === "synced" ? 100 : Math.min(99, height / target * 100)), message: msg || ("Catching up with the bitcoin chain, block " + height + " of about " + target), found: found || 0, throughTime: through || 0 });
        await sleep(slow === "sync" && height === 700000 ? 600000 : slow === "rescan1" && height === 557139 ? 600000 : slow === "rescan2" && height === 612400 ? 600000 : 600);
      }
      for (const height of [758000, 860000, 967000]) {
        emit("sync", { stage: "channels", height, target: 967310, peers: 8, percent: (height - 758000) / (967310 - 758000) * 100, message: "Making sure your channels are still open", found: 0, throughTime: 0, remaining: height === 758000 ? -1 : Math.round((967310 - height) / 550) });
        await sleep(slow === "channels" && height === 860000 ? 600000 : 600);
      }
      return status;
    },
    GetStatus: async () => status,
    GetHistory: async () => ({
      entries: [
        { time: 1789600000, kind: "onchain_out", title: "Sent on-chain", detail: "To bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq", amount: 14801, delta: -15721, fee: 920, status: "unconfirmed", txid: "5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e", address: "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" },
        { time: 1774963087, kind: "collected", title: "Channel funds collected", detail: "Funds of a closed channel, now in the on-chain balance.", amount: 15721, delta: 0, fee: 211, status: "done", txid: "d4f478ede3dcb5076d4faae54e4f5e2f6b3886e7fbea16942a13bf3a391a8e2b" },
        { time: 1771200000, kind: "channel_close", title: "Channel closed", detail: "Force closed by the peer. Funds collected later.", amount: 10453, delta: 0, fee: 0, status: "done", txid: "e327189c57dcf56ae84ae61f05f4938d8d30174a91b6961a6b3558745a6887e9" },
        { time: 1771100000, kind: "channel_close", title: "Channel closed", detail: "Force closed by the peer. Funds collected later.", amount: 5479, delta: 0, fee: 0, status: "done", txid: "fd407c0e7a5cd524044283e2ef3ac6a101d17a622758b5f5f858251c1857da21" },
        { time: 1768000000, kind: "sent", title: "Sent", detail: "Coffee at Blue Bottle", amount: 12000, delta: -12003, fee: 3, status: "done" },
        { time: 1767000000, kind: "received", title: "Received", detail: "Invoice #1042", amount: 28000, delta: 28000, fee: 1200, feeNote: "channel provider", status: "done" },
        { time: 1766000000, kind: "withdrawal", title: "Withdrawal", detail: "From a channel to a bitcoin address.", amount: 50000, delta: -50600, fee: 600, status: "done", txid: "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b" },
        { time: 1765000000, kind: "deposit", title: "Deposit", detail: "Into a channel.", amount: 60000, delta: 60000, fee: 0, status: "done", txid: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901234567890abcdefabcdef" },
        { time: 1764000000, kind: "received", title: "Received", detail: "from Alice", amount: 4000, delta: 4000, fee: 0, status: "done" },
      ],
      totals: { in: 92000, out: 76801, fees: 1734, expected: 13465, onchain: 13465, inChannels: 0, inPending: 0, held: 13465, uncollected: 0, unexplained: 0 },
      zeroCloses: 1,
      warnings: [],
    }),
    ValidateAddress: async (a) => { if (!/^(bc1|1|3)[a-zA-Z0-9]{20,}$/.test(a)) throw new Error("invalid"); },
    PrepareSweep: async (addr) => { await sleep(600); return { address: addr, amount: 1581900, options: [{ confTarget: 2, fee: 1840, txid: "a" }, { confTarget: 6, fee: 920, txid: "b" }, { confTarget: 25, fee: 410, txid: "c" }] }; },
    BroadcastSweep: async () => { await sleep(600); return "5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e"; },
    RestoreOther: async () => false,
    ShowWindow: async () => {},
    RestoredApps: async () => hasNode ? [
      { name: "02e66bcb1f4a9d6b3c2a7f8e9d0c1b2a3f4e5d6c7b8a9f0e1d2c3b4a5f6e7d8c9b", nodeId: "02e66bcb1f4a9d6b3c2a7f8e9d0c1b2a3f4e5d6c7b8a9f0e1d2c3b4a5f6e7d8c9b", dir: "/home/roys/.breez-recovery/backups/02e66bcb1f4a9d6b3c2a7f8e9d0c1b2a3f4e5d6c7b8a9f0e1d2c3b4a5f6e7d8c9b", current: true, lastOpened: "2026-09-24T12:01:00Z", funds: { inChannels: 0, pending: 0, onchain: 0, settling: false, at: "2026-09-24T12:01:00Z" } },
      { name: "02f8439e7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e", nodeId: "02f8439e7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e", dir: "/home/roys/.breez-recovery/backups/02f8439e7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e", current: false, payments: 14, lastOpened: "2026-09-24T13:40:00Z", funds: { inChannels: 0, pending: 15721, onchain: 1581900, at: "2026-09-24T13:40:00Z" } },
      { name: "zip-4c1d9a7e22b0f513", nodeId: "03a1c9e44b2f7d6e8a0b5c3d1e9f7a2b4c6d8e0f1a3b5c7d9e1f3a5b7c9d1e3f5a", dir: "/home/roys/.breez-recovery/backups/zip-4c1d9a7e22b0f513", current: false },
    ] : [],
    UseRestored: async () => false,
    Cancel: async () => {}, CopyText: async () => {}, OpenURL: async () => {},
    SaveHistory: async () => "/home/roys/breez-history-2026-09-17.csv",
    SaveLog: async () => "/home/roys/breez-recovery-2026-09-16.log", CopyLog: async () => {}, OpenWorkDir: async () => {},
    ChooseWorkDir: async () => "/home/roys/wallet", ForgetSignIns: async () => {},
  } } };
})();
