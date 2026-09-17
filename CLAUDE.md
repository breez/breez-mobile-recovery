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
           node start, sync progress, status, close, sweep.
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
`?scenario=channels|pending|onchain`, `?slow=list|restore|sync|rescan1|rescan2`
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
  restore). On the first start after a restore core derives 500 addresses
  on each of four branches through WalletKit NextAddr (needs the
  `walletrpc` build tag), writes FORCE_RESCAN and the `addresses-extended`
  marker, stops the node and returns ErrRestartRequired; the app relaunches
  itself with BREEZ_RECOVERY_AUTOCONTINUE=1 and the CLI asks to be run
  again. Re-initialising the library in-process after a stop hangs, which
  is why it is a program restart.
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
- Old nodes can make lnd's PendingChannels RPC fail ("unable to find
  arbitrator"). Status reports a warning instead of failing.
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
  residual is 12,798 sat, from invoices with memos lnd cannot serialise
  and events the app never recorded. `recovery history --json` also prints the raw
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
- No em dashes in UI copy or docs.
- The Log panel and Save log are the support channel: anything a user
  would need to report must be in there.
- Keep the README test plan matrix current in the same change as a fix.

## Releases

Tag `vX.Y.Z` (a dash suffix marks a pre-release) and push; the workflow
builds the three platforms and attaches the archives plus SHA256SUMS. The
macOS app is not signed; the release notes carry the right-click Open
workaround until notarization is set up.
