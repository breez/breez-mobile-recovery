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
           main.go = window setup. Frontend is plain HTML/CSS/JS in
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
OAuth client of the `breez-technology` Google Cloud project; CI gets them
from repository secrets). `BREEZ_RECOVERY_WORKDIR` overrides the work
folder, useful for testing next to a real one.

`go test ./core` covers the phrase to key derivation and decryption.
`go vet -tags walletrpc,chainrpc ./core . && go vet -tags webkit2_41,walletrpc,chainrpc ./ui` before
committing. The `walletrpc` tag compiles lnd's WalletKit RPC in; without
it the address look-ahead after restore fails with an unimplemented RPC.

## UI work without a node

`ui/frontend/mock/serve.sh` serves the frontend with a mocked Go backend
on http://127.0.0.1:8765/. Query parameters pick the state: `?hasNode=1`,
`?scenario=channels|pending|onchain`, `?slow=list|restore|sync|rescan1|rescan2|channels`
holds a stage so it can be screenshotted. The History screen is reached
with `?hasNode=1`, Continue, then History (mock ledger in mock.js). The
mock's method list must match `ui/app.go`; add a stub when adding a bound
method.

The frontend talks to Go through `window.go.main.App.<Method>` and receives
events through `window.runtime.EventsOn`: `progress` (a line of text),
`log` (batched lines), `sync` (SyncProgress), `signin` (provider, url).
Every value is rendered with textContent; keep it that way.

## Gotchas learned the hard way

- The module requires the breez library at a pinned commit and repeats its
  replace directives, because Go does not propagate replaces. When bumping
  the library, copy its replace block again.
- `bindings.Init` and the rest of the library are process-wide singletons.
  `bindings.RestoreBackup` stops and re-creates the app object itself;
  starting after it works. Two lnd instances cannot share a work folder.
  The library's databases (`db.Get`, `chainservice.Get`,
  `channeldbservice.Get`) and its logger are refcounted singletons that
  ignore the folder argument after the first call: a process is bound to
  the first folder the library was initialised on (`boundLibDir`).
- One folder per backup (core/dirs.go): the work folder holds the sign-ins,
  `current` and `backups/<node id or zip-hash>/`. Up to alpha.25 every
  restore landed in the one work folder, so a second backup inherited the
  first one's `channel.backup`, markers, chain files and logs (the
  2026-09-17 crash). Because of the binding above the library must not be
  initialised before the backup is chosen: Drive backups are listed with a
  direct read-only Drive call (core/drive.go), not through the library,
  and the library is initialised in `GoogleRestore` on the chosen folder.
  Switching backups after the library ran needs a program restart
  (`App.RestoreOther` relaunches with `BREEZ_RECOVERY_RELAUNCH=restore-other`).
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
- A restored wallet only scans addresses it has already derived, and the
  backup only knows the addresses in use at backup time. Funds the phone
  received or swept after its last backup sit on later addresses and were
  invisible (that is how Roy's 15,721 sat went missing on a second
  restore). On the first start after a restore core derives 50 addresses
  on each of four branches through WalletKit NextAddr (needs the
  `walletrpc` build tag), writes FORCE_RESCAN and the `addresses-extended`
  marker, stops the node and returns ErrRestartRequired; the app relaunches
  itself with BREEZ_RECOVERY_RELAUNCH=continue and the CLI asks to be run
  again. Re-initialising the library in-process after a stop hangs, which
  is why it is a program restart. The look-ahead was 500 per branch in
  alpha.12 to alpha.20; that made Roy's folder redo the whole history
  check (a second multi-hour pass) and slowed matching from 76 to 48
  blocks/s, so it is 50 since alpha.21. Any change to the address set
  forces a rescan on existing restores: state that cost before making one.
- Rescan progress persists in wallet.db, a restart resumes where it was.
  A `FORCE_RESCAN` file in the work folder makes the library drop the
  transaction store and rescan from the birthday; handy for testing.
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
  Things that bit: (1) neutrino's GetUtxo returns a nil report when the
  output is not in the start block; nil is not "unspent". (2) The start
  height must be the funding's confirmation block. Zero-conf channels
  (every Breez LSP channel since late 2022) have an alias ShortChannelID
  with height 16,000,000; use ZeroConfRealScid. A start height past the
  tip is never dequeued by neutrino's scanner and the batch manager spins
  forever. (3) The LSP's 2021 to 2022 zero-conf channels carry made-up
  SCIDs too (155808:14239027:26761) with heights far below the funding
  broadcast height; for those `findFundingBlock` walks the filters from
  the broadcast height (2016 blocks at most). (4) GetUtxo blocks, and the
  scanner starts its pass at the first request to arrive, deferring lower
  heights to a second pass over the chain: the lowest request goes first.
  (5) Progress only reaches requests still pending, so every request
  carries the handler. `TestScanFundingOutputsLive` runs the scan against
  the real chain on copies of channel databases (skipped unless its
  environment is set).
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
  a question. A channel the check still finds open is shown with a
  request to send the log to Breez support. Do not add closing back.
- Old nodes can make lnd's PendingChannels RPC fail ("unable to find
  arbitrator"). Status reports a warning instead of failing.
- Restoring a DIFFERENT node over a work dir that already held one must
  delete `data/chain/bitcoin/<net>/channel.backup` (guardRestore does):
  lnd's SCB is encrypted with the seed of the node that wrote it, and with
  a foreign one lnd aborts at startup ("unable to extract on disk
  encrypted SCB: chacha20poly1305: message authentication failed"), which
  takes the library down with it and then the process panics (2026-09-17,
  Roy restoring his 2022 backup over the 2019 one).
- The library cannot be started again in a process that stopped it: after
  the address look-ahead it hangs, and after a restore it crashes (the
  library's account service subscribes to invoices before lnd serves and
  then reads a nil stream: breez/breez account/payments.go:1310, seen
  2026-09-17 when Roy picked a second backup). Both paths therefore return
  ErrRestartRequired and the app relaunches itself; the CLI restores and
  exits, so it is not affected.
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
  "Time left".
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
