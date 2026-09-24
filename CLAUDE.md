# Breez Recovery, notes for working on it with Claude Code

Read README.md first: what the tool does, the test plan matrix, the
security notes. This file is the working knowledge that is not obvious
from the code.

## What this is

A desktop app (and CLI) that restores a Breez mobile app backup from
Google Drive, iCloud or a zip, runs the node locally, syncs it and moves
the funds out. It exists so users can recover funds after the mobile app
leaves the stores. Users are not technical: every screen must say what is
happening and every long step must show progress.

## Layout

```
core/      all logic. Config, Reporter interface, cloud sign-in, restore,
           node start, sync progress, channel check, status, sweep.
main.go    CLI over core (recovery-cli).
ui/        Wails v2 desktop app over core. app.go = methods bound to JS,
           main.go = window setup, helper.go and nodeproc.go = the node
           helper process. Frontend is plain HTML/CSS/JS in
           ui/frontend/dist, embedded in the binary, no npm.
docs/      GitHub Pages: signed-in.html, where the browser lands after a
           sign-in redirect.
.github/workflows/build.yml  macOS universal, Windows, Linux builds;
           tags v* publish a release with SHA256SUMS.
```

## Build and run

```sh
go build -tags walletrpc,chainrpc -o recovery-cli .    # CLI
cd ui && wails build -tags webkit2_41,walletrpc,chainrpc -skipbindings -ldflags "-X main.version=dev"
```

Wails CLI: `go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0`.
Linux needs libgtk-3-dev and libwebkit2gtk-4.1-dev and the `webkit2_41`
tag (CI builds on Ubuntu 22.04 with 4.1 so the binary runs on 24.04 too).
The Google client is not in the source: for a local run set
`BREEZ_GOOGLE_CLIENT_ID` and `BREEZ_GOOGLE_CLIENT_SECRET` (the Desktop app
OAuth client of the `breez-technology` Google Cloud project), or bake them
in with `-ldflags "-X .../core.GoogleClientID=<id> -X .../core.GoogleClientSecret=<secret>"`. `BREEZ_RECOVERY_WORKDIR` overrides the work
folder, useful for testing next to a real one.

`go test ./core && go test -tags webkit2_41 ./ui` runs the unit tests;
the ones on a real node or wallet skip unless their `BREEZ_LIVE_*`
variables are set. CI runs vet and the tests on all three platforms
before it builds, then `TestBuiltHelper` on the built app (starts it in
helper mode, pings it, closes its stdin); locally it runs with
`BREEZ_RECOVERY_SMOKE_EXE=<built executable>`. A test that makes a
session releases its folder locks (`core.ReleaseLocks`) before its temp
folder is removed: Windows does not delete an open file.
`go vet -tags walletrpc,chainrpc ./core . && go vet -tags webkit2_41,walletrpc,chainrpc ./ui` before
committing. The `walletrpc` tag compiles lnd's WalletKit RPC in; without
it the search for funds paid after the backup fails with an unimplemented RPC.

## UI work without a node

`ui/frontend/mock/serve.sh` serves the frontend with a mocked Go backend
on http://127.0.0.1:8765/. Query parameters pick the state: `?hasNode=1`,
`?scenario=channels|pending|onchain`, `?slow=list|restore|sync|addresses|rescan1|rescan2|channels`
holds a stage so it can be screenshotted, `?restart=1` starts the node
again twice in the first sync, `?crash=1` (funds screen) and `?crash=sync`
stop it by itself. The History screen is reached
with `?hasNode=1`, Continue, then History (mock ledger in mock.js). The
mock's method list must match `ui/app.go`; add a stub when adding a bound
method.

The frontend talks to Go through `window.go.main.App.<Method>` and receives
events through `window.runtime.EventsOn`: `progress` (a line of text),
`log` (batched lines), `sync` (SyncProgress), `signin` (provider, url),
`stopping` (a running node stops for a switch, a settings change or the
close), `logreset` (the log went to the old backup's folder: empty the
panel), `nodestopped` (the node exited by itself with no call waiting:
the funds screen offers Continue recovery).
Every value is rendered with textContent; keep it that way.

## Gotchas learned the hard way

- The module requires the breez library at a pinned commit and repeats its
  replace directives, because Go does not propagate replaces. When bumping
  the library, copy its replace block again.
- `bindings.Init` and the rest of the library are process-wide singletons.
  `bindings.RestoreBackup` stops and re-creates the app object itself;
  starting after it works. Two lnd instances cannot share a work folder.
  The library's config and log file are set up once per process
  (sync.Once in config/init.go and log/init.go), and its databases
  (`db.Get`, `chainservice.Get`, `channeldbservice.Get`) are refcounted
  singletons that ignore the folder argument while a reference is held: a
  process is bound to the first folder the library was initialised on
  (`boundLibDir`).
- One restore path for every source (core/restore.go): fetch into memory,
  decrypt and check ALL files, write a staging folder, one rename into
  place; only then is an earlier restore moved aside (never deleted), the
  backup selected and the Drive copy marked. The library's RestoreBackup
  is NOT used: it returns the result of an unrelated call and so reported
  a failed restore as done (bindings/api.go:317-328), marked the snapshot
  before decrypting, and downloaded through the system temp folder
  (cross-volume rename failure on Windows). Drive is downloaded with the
  tool's own client (core/drive.go) and marked with the id written into
  the restored folder (`backup/breez_backup_id`), the one the library
  compares with. A folder is a restored app only with all three node
  files (`hasNode`); a wallet without its channel database would start
  with an empty one and show no channels. One program per work folder
  (core/lock.go). Bitcoin peers the user once set on the phone travel in
  breez.db and replace the configured ones: initLibrary resets them.
- One folder per backup (core/dirs.go): the work folder holds the sign-ins,
  `current` and `backups/<node id or zip-hash>/`. Up to alpha.26 every
  restore landed in the one work folder, so a second backup inherited the
  first one's `channel.backup`, markers, chain files and logs (the
  2026-09-17 crash). Because of the binding above the library must not be
  initialised before the backup is chosen: Drive backups are listed with a
  direct read-only Drive call (core/drive.go), not through the library,
  and the library is initialised when the node first starts, on the
  chosen folder. A restore never touches the library, and the window
  never runs it: the node runs in a helper process (entry below), so
  switching backups stops the helper and starts a new one on the chosen
  folder.
  Restoring a backup that is already there moves the old folder aside
  (`<name>.replaced-<time>`), it never deletes a wallet. A legacy
  single-folder install is moved into `backups/` on start, by renaming a
  fixed list of node files. The Drive listing shows when the backup files
  were written; the snapshot folder's own time changes when a backup is
  restored, which made a 2022 backup look like the newest one.
- Sync progress. lnd exposes no per-block progress for the wallet rescan,
  the slow part of a first sync. What works: while the wallet is behind,
  `GetInfo.best_header_timestamp` is the wallet's synced-to block time
  (BtcWallet.IsSynced returns Manager.SyncedTo().Timestamp), so progress is
  measured in time. The start height and address count come from lnd's
  "Started rescan from block ... for N addresses" log line. Things that do
  not work: "Rescanned through" lines (only every 10k blocks before the
  birthday), neutrino trace level (hex dumps of peer messages, and the
  per-block line never fired), GetRecoveryInfo (needs a recovery window).
- Funds paid after the last backup (core/addrscan.go). A restored wallet
  only checks addresses it has already derived, and the backup only knows
  the addresses in use at backup time. Two real cases sit past them: the
  phone swept a closed channel after its last backup (Roy's 15,721 sat),
  and an earlier restore with this tool collected funds and its folder is
  gone (Roy's 864 sat). Releases alpha.12 to alpha.30 derived a window of
  addresses THROUGH the wallet (WalletKit NextAddr, 500 then 50 per
  branch). That was a flaw: NextAddr moves the wallet's address counter,
  so the next address lnd hands out, for its sweep of a closed channel, is
  the one right past the window, exactly where a LATER restore of the same
  backup does not look. No window size cures it. Proven 2026-09-21: a
  fresh restore of Roy's backup 0229fda8 returned bc1p8x83...6uy, the very
  address holding his 864 sat, as its next address and showed 0 sat.
  Since alpha.31 the app looks ahead WITHOUT touching the wallet: it reads
  the account public keys (walletrpc ListAccounts), derives the addresses
  itself and looks for them in the same filter walk that checks the
  channels (design, source references and cost table:
  docs/2026-09-21-funds-after-backup-design.md; everything the source
  audits found and how it was fixed: docs/2026-09-21-audit-findings.md).
  Two ways find a paid address. (1) Following the money: when a channel is
  found closed the walk follows EVERY output of the closing transaction
  through two spends (close, second-level HTLC transaction, sweep), and
  every block it opens is searched for the next 1,000 addresses of every
  branch, the nested account included. That finds what a close paid out
  wherever its sweep went, and it has to: lnd takes a NEW address for
  every sweep it PUBLISHES (sweeper.go:1694-1715), and with neutrino every
  start publishes again, so a sweep's address can be far out. (2) The gap,
  for funds that did not come out of a channel in the backup (a plain
  payment, the phone's sweep of a close already under way): 40 addresses
  of the four branches (witness key hash and taproot, receive and change)
  are in the filter match from the wallet's birthday on, and the search
  is complete when the 20 addresses after the highest paid one were
  looked for in every block (the standard gap; an interim 51 was a
  workaround for alpha.30's own flaw and is gone). A find past the first
  20 brings more addresses into the match, and those get a pass over only
  the blocks before they joined. THE COST UNIT IS A PASS, NOT A SCRIPT: a
  filter is 13 to 21 KB, a walk from 2021 is 4 to 5 GB, neutrino keeps
  none on disk (the library sets no PersistToDisk, memory cache about
  2,000 filters), while each script opens one block in 784,931 by false
  positive. Only when something was paid that the wallet does not know is
  the wallet touched: the finds go to `found-funds.json`, a fresh history
  check is ordered FIRST (`history-recheck`; a crash half way still
  re-checks on the next start), the counters are read again (lnd may have
  handed out addresses meanwhile), NextAddr advances each branch up to the
  paid address and the last address returned must equal the one derived
  here, then the node starts again. The next StartNode drops the wallet's
  history ITSELF (the library's dropwtx.Drop, called before Init, error
  returned) and removes the order only then. The library's FORCE_RESCAN
  file is NOT used: its Init only logs a failed drop, removes the file
  when its second, unrelated drop succeeded, and throws away lnd's scan
  positions. After the history check `verifyFound` requires every find in
  the wallet's transactions: a missing one orders one more full history
  check (a restart), and if it is still missing the sync stops with an
  error. Before any of it the derivation is checked against the wallet:
  the last address the wallet derived on a branch must be the one this
  code derives at that index, or the search fails loudly. The search runs
  once per restore
  (`addresses-extended` marker, which folders of older releases carry too,
  so they are left alone) inside WaitSynced, after this process has seen
  the headers catch up and before the history check is waited for: a hit
  found first costs minutes, a hit found after the history check would
  cost a second history check. Its walk is kept (`Core.walk`) and saved
  (`chain-walk.json`, core/walkstate.go), and the channel check carries on
  from the block it ended at: after the sync, after the restart a find
  causes, and on every later start. Up to alpha.30 every start walked the
  whole range again. The saved walk is used only if its format, every
  channel's scripts and height, and the hash of the block it stands on (6
  below the tip it reached) all match; otherwise it is discarded with a
  log line and the walk starts over. The walk keeps following a channel
  after lnd stops listing it as open, and the funds screen takes what a
  closed channel still owes from the walk (core/node.go status): lnd's
  PendingChannels fails on some old nodes, lags after a sweep confirmed,
  and counts a published sweep a second time. The walk never asks below
  the node's first block (`floor`; a foreign channel in a mixed-up backup
  can be older) and fetches that one block directly.
  The walk starts at the wallet's birthday (`wallet-birthday` file, read
  from wallet.db with bbolt BEFORE lnd's first start, lnd holds the
  file's lock afterwards). Breez backups carry no sync height (the app
  reset it to the genesis block) and the zip entries carry no dates, so
  the birthday is the only start that is always safe. The library does
  not keep the whole chain: it bootstraps the headers from a point shortly
  before the wallet's birthday (first header 687,000 for a wallet of 13
  Jun 2021, 557,000 for one of 5 Jan 2019) and the header file is empty
  below it but for the earlier checkpoints, written as lone headers. Every
  empty header has the same hash, and asking neutrino for a filter or
  header there fails with "target hash not found in index".
  The first real header's own filter cannot be fetched either
  ("got entire filter, but job was not finished"): a filter is verified
  against the filter header of the block before. So the walk starts two
  days before the birthday but never below the block after the first
  real header (`walkStart`, `firstBlock`), which on the four backups looked at is
  18 hours to 3 days before the wallet was created; the backup matrix caught this, the
  first test backup happened to sit above the hole. Any change to the address set forces a rescan on
  existing restores: state that cost before making one.
- History shortcut (core/syncskip.go, study and what was built:
  docs/2026-09-21-skip-history-check-study.md). lnd's own history check
  after a restore is a second full pass over the filters and finds nothing
  for a wallet that never had an on-chain transaction, the usual Breez
  user. The app's walk watches the wallet's own scripts too (read with the
  wallet closed, before lnd starts); when the search found nothing, the
  script count equals the one lnd logs for its check, no block paid any of
  them and the wallet's transaction list is empty, the wallet's sync state
  is set to 6 blocks below the walk's end (144 hashes, birthday block put
  back VERIFIED or btcwallet locates it again and resets everything; one
  transaction, read back) before the next start. Anything else: nothing is
  written and lnd's ordinary check runs. A wallet that then logs "Unable
  to synchronize wallet to chain" gets the full check ordered.
- First start after a restore: lnd must start at the chain tip (see the
  closed-channel entry below), so the first start only waits for the
  headers, writes `chain-ready` and the node starts again in a new
  helper (the CLI asks to be run again). Why a new process: see the node
  helper entry below.
- Rescan progress persists in wallet.db, a restart resumes where it was.
  A `FORCE_RESCAN` file in the backup's folder (`backups/<name>/`) makes
  the library drop the transaction store and lnd's height hints and rescan
  from the birthday; handy for testing.
- Peer quality dominates sync time: 76 blocks/s with public peers versus
  600/s with the Breez node. The library builds neutrino itself from
  breez.conf `[Job Options] peer=` lines (chainservice/init.go, exclusive
  list, MaxPeers 3) and IGNORES lnd.conf's `[Neutrino]` section: alphas
  up to 17 wrote `neutrino.addpeer` there, which did nothing, and runs
  only reached bb2 by luck through the DNS seeds. Since alpha.18 core
  pins bb1 and bb2.breez.technology through the job options and, before
  starting, does a bitcoin version handshake with each (a TCP connect is
  not enough: a hung node still accepts connections) and fails loudly
  when none answers with compact filters. The library also queries each
  job peer over HTTPS (`https://<peer>/rest/blockfilter/...`, "rest
  peers") for fast filter fetches. State on 2026-09-17: bb2 healthy
  (P2P + REST, cert to 2026-11-09); bb1 accepts TCP on 8333 but never
  replies to a version message and its TLS certificate expired
  2026-07-12, so it contributes nothing until ops fixes it.
- Channel check (core/chaincheck.go). lnd's channel list is not proof of
  funds: a backup is a snapshot, and a channel that closed later still
  looks open until lnd's own spend scan finishes, which takes long after a
  restore. Roy force closed seven such channels on 2026-09-17. Rule: only
  a channel with the verdict "open" counts in the balances. Open means the wallet
  derives the funding key at the channel's KeyLocator AND neutrino found
  the funding output, with the expected script, unspent up to the tip.
  Anything else, including "not found" and "not checked", is left alone.
  The scan (`scanFundingOutputs`) walks the node's compact filters itself
  from the oldest funding height to the tip, with
  `GetCFilter(..., OptimisticBatch())`, and opens every block whose filter
  matches a funding script: the block that created the output and the one
  that spent it both match. Measured 845 blocks/s alone, about 330 while
  something else pulls filters from the same peer. Things that bit:
  (1) It does NOT use neutrino's GetUtxo (alpha.27 did). lnd runs its own
  GetUtxo scans through the same scanner, which works one batch at a time:
  requests that arrive while a batch is past their height wait for it to
  end and then get a pass of their own, with no progress in between. On
  Roy's node the stage sat silent for minutes. GetUtxo also returns a nil
  report when the output is not in its start block (nil is not "unspent")
  and never dequeues a request whose start is past the tip. (2) Without
  OptimisticBatch every filter is one network round trip: 15 blocks/s.
  (3) The start height must be a real chain height at or below the
  funding's confirmation. Zero-conf channels (every Breez LSP channel
  since late 2022) have an alias ShortChannelID with height 16,000,000;
  use ZeroConfRealScid. The LSP's 2021 to 2022 zero-conf channels carry
  made-up SCIDs too (155808:14239027:26761), far below the funding
  broadcast height; for those the scan starts at FundingBroadcastHeight.
  (4) The stage reports from its first moment and twice a second after
  that; a long stage that shows nothing reads as a hang.
  `TestScanFundingOutputsLive` runs the scan against the real chain on
  copies of channel databases (skipped unless its environment is set).
- Funds of a channel the LSP closed (the main case for users: the LSP
  force closed every channel) are NOT in the wallet after a restore. The
  close pays the user's share to the channel's payment key (to_remote),
  not to a wallet address, so the history check finds nothing (0
  transactions is normal for an LSP-funded node) and only lnd can collect
  it: chain watcher detects the close, commitSweepResolver offers the
  output to the sweeper, the sweeper publishes at the NEXT BLOCK
  (immediate=false), so the node must stay up until then. Proven on Roy's
  node 2026-09-20: 999 sat output, sweep tx 431ca969..., 135 sat fee,
  864 sat to the wallet; the sweeper's budget is half the amount, small
  outputs are swept. Two things made this take ~10 hours and look like
  "Nothing left to recover": (1) lnd started at the bootstrap checkpoint
  (triggerHeight=812000) in the copy the look-ahead restart started,
  seconds after the first start and before the one-minute header sync;
  its neutrino notifier then walks every block to the tip downloading
  full blocks, ~4 a second, and its historical spend rescan only covers
  up to where it started. Why it started there is not established: lnd
  starts the notifier only after its own wait for the chain
  (initial-headers-sync-delta=2h, lnd.conf). The first start now waits
  for "Fully caught up with cfheaders" before its chain-ready restart
  (firstStart), so the next start runs lnd at the tip and the close
  is found by the filter scan in minutes (lnd persists
  its scan position as a height hint across restarts). (2) The screen
  only knew lnd's balances. The channel check now derives the to_remote
  script (`toUsScript`, lnwallet.CommitScriptToRemote with the PEER as
  commitment owner; static-remote-key and anchor channels only, nil
  otherwise: never guess an amount), reads what the closing transaction
  paid to it, keeps watching whether that output is spent (the phone may
  have swept it), and reports the rest as `SpentChannel.Collect`, counted
  as Pending. Verified with `TestToUsScriptLive` on 14 real force closes:
  the 3 that paid match the derived address exactly. A COOPERATIVE close
  pays a wallet address instead, which the search for funds paid after
  the backup and the history check find; Collect stays 0. The app's check
  and lnd's scan halve each other's speed when they run together (845
  blocks/s alone, ~250 and ~70-190 together); total time is the same
  either way, so they are not ordered. The CLI `status` keeps the node
  running while collecting.
- The tool closes nothing (decided by Roy 2026-09-20): no cooperative
  close, no force close, and `recovery lncli` refuses closechannel,
  closeallchannels and abandonchannel. Breez closed its channels with the
  app's users from its side, so funds arrive on-chain and are sent out
  with the sweep. Closing from a backup is dangerous: a force close
  broadcasts the backup's commitment, and if the phone used the channel
  after its last backup that commitment is revoked and the peer can take
  the channel. lnd refuses only after the peer has reported the data loss
  (ChanStatusLocalDataLoss), so with the peer offline nothing checks it;
  on 2026-09-17 lnd signed and broadcast seven stale commitments without
  a question. A channel the check still finds open is listed with a
  request to email the list to Breez support and a Copy button. Do not
  add closing back. Only the main LSP (031015a7...) served users and its
  channels are all closed; Roy's own open ones are with an internal node.
  Dust: Breez mobile sets its own dust limit and reserve to ZERO, so the
  limit that decides a payout is the LSP's (354 sat, 573 on older
  channels): `dustLimit` takes the larger of the two. A balance below it
  is listed but not counted as funds.
- Old nodes can make lnd's PendingChannels RPC fail ("unable to find
  arbitrator"). Status reports a warning instead of failing.
- A node's folder must never hold another node's
  `data/chain/bitcoin/<net>/channel.backup`; one folder per backup rules
  it out, as every restore writes a fresh staging folder with only the
  node files and the backup id (placeBackupFiles). Why it matters:
  lnd's SCB is encrypted with the seed of the node that wrote it, and with
  a foreign one lnd aborts at startup ("unable to extract on disk
  encrypted SCB: chacha20poly1305: message authentication failed"), which
  takes the library down with it and then the process panics (2026-09-17,
  Roy restoring his 2022 backup over the 2019 one).
- The node helper (ui/helper.go, ui/nodeproc.go, ui/helperproto.go). The
  window process keeps Wails, dialogs, sign-in, listing, restore, the log
  and `instance.lock`, and never starts the library. The node (core with
  the breez library and lnd) runs in a child process of the same
  executable, chosen by `BREEZ_RECOVERY_NODE_HELPER=<backup name>` at the
  top of ui/main.go main(), before anything of Wails (its runtime
  functions log.Fatalf without a window). Why a process: the library's app
  object starts and stops only once (breez app.go Start, Stop), a process
  stays bound to its first folder (`boundLibDir` above), and a second
  `bindings.Init` in one process was never tried. So every new start of
  the node is a new helper and the window stays. The CLI still runs the
  node in-process and asks to be run again.
  Pipes: JSON lines on the helper's stdin and on the stdout it saved
  before core.New took os.Stdout; no length limit (History is large); a
  line that is not a frame goes to the log; stderr (a panic trace) goes
  to the log. Calls: startAndSync, status, history, prepareSweep,
  broadcastSweep, cancel, stop, and ping (the version, no node; CI).
  Planned restarts: core stops the node and returns ErrRestartRequired
  (chain-ready, history shortcut, funds found, failed verify, wallet sync
  reset); the helper replies `restart` and exits, and the window starts a
  new helper in the same StartAndSync with "Starting the node again..."
  on the sync screen, 6 in a row at most. With no sync report coming
  (starting, waiting, stopping) the sync screen shows the node's lines; a
  line mid-stage leaves the bar, 1.5 s without a report makes it one
  with no measure (App.syncLine).
  Order and locks: a new helper starts only after the last one's Wait
  returned (breez.db opens with no timeout), and the window marks its
  folder in use (`SetInUse`) until then, which keeps the in-process
  guards working. The helper locks `backups/<name>/instance.lock`
  (`LockBackup`, up to 20 s), the CLI takes the same lock before its
  node starts, and the window checks that lock before it moves or
  selects a folder (`ErrBackupInUse`): a helper of a crashed window
  lives up to 25 s.
  Stop: a stop frame and stdin closed; the helper cancels, gives the call
  3 s (a broadcast until the end), runs StopWithin(20 s) and exits; the
  window kills it after 25 s, and after 30 s of an ignored cancel (never
  during a broadcast). stdin's end or a failed write (SIGPIPE ignored)
  stops it too, and a timer ends it 25 s after any stop began. The
  library's Stop sometimes takes long after lnd is down (0.1 s in one
  real run, past 20 s in others; the cause is not established), so
  StopWithin returns once lnd logs "LTND: Shutdown complete" and the
  helper's exit ends the rest.
  A crash never starts the node again: a waiting call fails with "the
  node stopped unexpectedly", with none waiting the page gets
  `nodestopped`. A failed sync, or one stopped before the node was up
  (`nodeUp` in the reply), gets a new helper next time: the library's app
  may have started half way. One stopped later keeps its node. Stop
  pressed while an old helper stops starts no new one.
  Log per backup: it runs on across the helper's restarts. When the
  backup in use changes (Restore another backup, switching, a restore of
  another backup, a settings change) and on close, the lines so far are
  appended to `recovery.log` in the old backup's folder and the page gets
  `logreset`.
  Closing while something runs asks first, then keeps the window, which
  shows the node stopping, and quits once it has stopped and the log is
  with the backup. beforeClose returns at once: on Windows Wails runs it
  on the window's thread, which would freeze.
  Support: a second "Breez Recovery" process runs while a node runs; it
  is the node.
  The 2026-09-17 crash when Roy picked a second backup came from the
  foreign `channel.backup` above: lnd aborts in server.Start,
  SubscribeInvoices then fails and the account service reads the nil
  stream (breez/breez account/payments.go:1303-1310). That nil read can
  happen on any start where the subscription fails; one folder per backup
  removed this cause of it, and now it ends the helper, not the window.
- History (core/history.go) is a ledger with one entry per money
  movement. Sources: the app's own payment list (`bindings.GetPayments`,
  the same list Breez mobile showed, with descriptions), lnd's closed and
  pending channels, lnd's invoices and payments (truth for what was paid;
  entries the app list lacks are added without a description), and the
  wallet's transactions. Every wallet transaction is either folded into
  another entry (a cooperative close paying the wallet, a sweep of a
  force-closed channel, the funding and the swap service's claim of a
  deposit address) or listed as sent, received or moved on-chain, so no
  sat appears twice. Deposit addresses are the P2WSH outputs lnd marks as
  ours; they pair with the app's Deposit records by exact amount first,
  then nearest in time within 14 days; an unpaired output spent to an
  outside address is a refund. Entries carry Delta (net effect on the
  app's funds, 0 for moves between its own balances) and Fee; totals
  show In, Out, Fees, the list total and what the node holds now; the
  Unexplained field stays in the JSON but the screen and the CLI do not
  show it (Roy: no notes about a gap, even when there is one). On Roy's 2019 node (3,479 entries, 28M sat through) the
  list total was -12,798 against 0 held, fully traced 2026-09-17:
  (1) one payment the app list (and lnd's value_sat) records at the
  requested 132,000 sat while only a 118,474 sat part settled; sends now
  take amount and routing fee from lnd's settled HTLCs (withdrawals keep
  the app's split, their fee is the swap service's); (2) money that left below whole sats,
  counted as fees (`leftMsat`): the msat part of each send (345 sat) and
  the final channel balance each close could not pay out (5 sat of
  fractions), read in-process from lnd's historical channels via
  `channeldbservice.Get` (`closeLeftovers`); (3) a cooperative close
  that settled 0 while the channel held 340 sat, below the 573 sat dust
  limit, found the same way and listed as an entry whose whole amount is
  fee. A shortfall of up to one sat per channel
  closed before lnd kept channel history (here two, af756ad2... and
  21552873..., whose sub-sat balances cannot be read) is that leftover
  too and counts as fees, so the list now totals 0 against 0 held. A
  bigger gap is never absorbed; it stays visible in the totals. Receives match lnd's settled invoices to
  the sat; the 16 invoices with non-UTF-8 memos are all in the app list.
  Tools used: channeldb.Open on a COPY of channel.db (QueryInvoices,
  FetchPayments, FetchHistoricalChannel for the final LocalBalance), and
  `recovery lncli listchaintxns` (confirmed amounts sum to the balance).
  History exports as CSV: `core.WriteHistoryCSV` (entries newest first,
  then a blank line and the totals), the Export button on the History
  screen (`App.SaveHistory`, which writes what the screen last showed) and
  `recovery history --csv`.
  `recovery history --json` also prints the raw
  app list and an lnd cross-check (`lnd` key) for chasing residuals.
  Close block times come from lnd's ChainKit RPC (`chainrpc` build tag).
- X11 drops a 1024px window icon silently; Linux uses the 256px
  `ui/build/windowicon.png`.
- Wails on Linux replaces custom dialog buttons with Yes/No; check for
  "Yes" as well as the label you asked for.
- After the Google or Apple redirect reaches localhost the app sends the
  browser to docs/signed-in.html so the code or token leaves the address
  bar. Apple's callback is the static page on the breez library repo's
  gh-pages branch, registered with the CloudKit token; changing it means a
  new token in the CloudKit Console (Production environment).
- iCloud has not been tested end to end; the team member with an iOS
  backup does that.

## Reference (moved out of the README, which is for users)

- Backup format: a zip of lnd's `wallet.db` and `channel.db` plus
  `breez.db` (very old backups: the three files as separate assets). It is
  the channel state as of the backup. The backup phrase (12 or 24 words)
  is only the zip's encryption key, NOT the lnd seed; without the files it
  restores nothing.
- Android keeps it in Google Drive's hidden `appDataFolder`, visible only
  to OAuth clients of the Breez Google Cloud project `breez-technology`;
  the tool uses a "Desktop app" client of that project. iOS keeps it in the
  CloudKit private database of container `iCloud.technology.breez.client`;
  the tool uses a CloudKit web API token (Production) whose sign-in
  callback is `https://breez.github.io/breez/icloud-callback.html` (branch
  `gh-pages` of the breez library repo), which forwards Apple's session
  token to `127.0.0.1:53821`. That token therefore shows in the GitHub
  Pages request log; the container's custom URL scheme would avoid it but
  needs an app bundle that registers the scheme and a token made for it.
- A Google Drive restore marks the snapshot as restored by this machine,
  as a new phone would (core/drive.go markRestored, once the files are in
  place); a phone still running that node stops itself on its next start.
  An iCloud restore and the Drive listing mark nothing.
- The production defaults (breez server, bootstrap, closed channels URL,
  fee URL, zero-conf and scid-alias options in lnd.conf) come from the
  `breez.conf` and `lnd.conf` bundled in the released APK. The LSP token is
  not bundled and not needed for syncing or sweeping
  (`-X .../core.LSPToken=...` bakes one in). lnd with neutrino on mainnet
  needs the external fee estimator (`-feeurl`).
- Other build targets: `wails build -platform darwin/universal |
  windows/amd64 | linux/amd64`. The CLI accepts `-workdir`, `-network`,
  `-breezserver`, `-bootstrap`, `-closedchannelsurl`, `-lsptoken`,
  `-feeurl`, `-icloud-token`, `-peer`, `-loglevel`, `-v`.
- README rules (Roy, 2026-09-20): it is for the user, keep it short, no
  builder detail there.

## Copy and design rules from the product owner

- Say "app", never "wallet", for the thing being restored. The slow stage
  is "checking your channel and payment history".
- Progress must visibly move: fractions of a percent, a time estimate, a
  date reached. A bar parked at 99% with a static label is a bug.
- No noisy tiles (a node activity indicator was removed as annoying) and no
  scary notes on the way through the flow.
- Short text. One plain sentence explains a screen; delete anything that
  does not carry information. No hedges ("roughly", "about") and no made-up
  ranges: "10 to 40 minutes" was wrong, a real run took 90. A label is
  "Time left"; its value carries a tilde ("~3 min", "<1 min"), asked for by
  Roy 2026-09-20. Funds tiles are "In channels", "Pending", "On-chain".
- No em dashes in UI copy or docs.
- The Log panel and Save log are the support channel: anything a user
  would need to report must be in there.
- Keep the README test plan matrix current in the same change as a fix.

## Releases

Tag `vX.Y.Z` (a dash suffix marks a pre-release) and push; the workflow
builds the three platforms and attaches the archives plus SHA256SUMS.
GitHub's upload API fails now and then with an HTML error page and leaves
a release missing files (alpha.24 and alpha.25 both), so the upload action
may fail and a retry step fills the gaps and decides the outcome. The
macOS app is not signed; the release notes carry the right-click Open
workaround until notarization is set up.
