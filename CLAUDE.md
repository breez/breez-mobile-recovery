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
go build -o recovery-cli .                    # CLI
cd ui && wails build -tags webkit2_41 -skipbindings -ldflags "-X main.version=dev"
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
`go vet ./core . && go vet -tags webkit2_41 ./ui` before committing.

## UI work without a node

`ui/frontend/mock/serve.sh` serves the frontend with a mocked Go backend
on http://127.0.0.1:8765/. Query parameters pick the state: `?hasNode=1`,
`?scenario=channels|pending|onchain`, `?slow=list|restore|sync|rescan1|rescan2`
holds a stage so it can be screenshotted. The mock's method list must
match `ui/app.go`; add a stub when adding a bound method.

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
- Rescan progress persists in wallet.db, a restart resumes where it was.
  A `FORCE_RESCAN` file in the work folder makes the library drop the
  transaction store and rescan from the birthday; handy for testing.
- Peer quality dominates sync time: 76 blocks/s with public peers versus
  600/s with the Breez node. `neutrino.addpeer=bb2.breez.technology` is on
  by default next to DNS seed discovery.
- Old nodes can make lnd's PendingChannels RPC fail ("unable to find
  arbitrator"). Status reports a warning instead of failing.
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
