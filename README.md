# Breez Recovery

> **Experimental.** This tool is under development and has not been
> tested on real funds yet. Try it with a backup you can afford to lose,
> keep the log (Log, then Save log) and report problems in the issues.

Desktop app and command line tool that restore a Breez app backup from
Google Drive, iCloud or a backup file and move the funds to a bitcoin
address. They exist so users can still recover funds after the Breez app
leaves the app stores.

Both reuse the breez library unchanged: the same restore code and the same
lnd fork the mobile app runs. Builds for macOS, Windows and Linux.

```
core/      the recovery logic (cloud sign-in, restore, node, channel check, sweep)
ui/        the desktop app (Wails: Go + a small HTML frontend)
main.go    the command line tool
```

## Before you start

If the Breez app is still installed on your phone, make a safety copy of
its data before running this tool: open the menu, then **Developers**,
then **Export DB Files**, and keep the zip it shares somewhere safe. The
tool restores from Google Drive or iCloud, not from that zip, but the zip
holds everything the app knows should anything go wrong.

## Test plan

A cell records the furthest step reached, who ran it and when. A run
counts once a real backup went through the app on that OS. Steps:

1. Sign in and list the backups
2. Restore (with the backup phrase where the backup is encrypted)
3. Sync to the chain tip and see the funds screen with the right balances
4. Send the on-chain balance out

| Backup source | Linux | macOS | Windows |
|---|---|---|---|
| Google Drive | steps 1 to 4, roys, 2026-09-17 (node from 2019, 938 addresses: first history check took 90 min; step 4 sent 15,370 sat plus a 351 sat fee, confirmed) | not run | not run |
| iCloud | not run | not run | not run |
| Backup file | steps 2 and 3 on a fresh node with no funds, 2026-09-16 | not run | not run |

Update the table in the same pull request as any fix the run produced.

## How the Breez app backup works

* The backup is a zip of three files: lnd's `wallet.db` and `channel.db`
  plus `breez.db`. It is the channel state as of the backup, which can be
  older than the state the phone reached afterwards.
* The backup phrase (12 or 24 words) is only the encryption key of that zip.
  It is **not** the lnd seed. Without the backup files the phrase restores
  nothing.
* Android stores the zip in the hidden Google Drive `appDataFolder`, which is
  visible only to OAuth clients of the Breez Google Cloud project
  (`breez-technology`). The tool uses a "Desktop app" OAuth client from that
  project.
* iOS stores it in the CloudKit private database of the
  `iCloud.technology.breez.client` container. The tool uses a CloudKit API
  token (Production environment) whose sign-in callback is the static page
  `https://breez.github.io/breez/icloud-callback.html` (branch `gh-pages`
  of the breez library repo). The page forwards Apple's session token to the tool on
  `127.0.0.1:53821`.

## Desktop app

One window, one step at a time:

1. **Welcome.** If a backup is already restored on this computer, offers to
   continue with it or restore another backup. Every backup gets a folder of
   its own, so restoring another one never touches the first.
2. **Where is your backup.** Google Drive, iCloud or a backup file.
3. **Sign in.** The system browser opens for Google or Apple. The link is
   shown in the app in case the browser did not open. Sign-ins are cached
   in the work folder.
4. **Choose the backup.** One entry per node, latest first, with the
   encryption type. PIN-encrypted backups (deprecated years ago) are shown
   but cannot be restored.
5. **Backup phrase**, for encrypted backups, with word count and validation.
6. **Restore**, then **sync** with a progress bar, block heights and stage
   list. A backup that is already restored on this computer is not downloaded
   again unless asked; its earlier copy is then kept in a folder next to it. On the first start after a restore the app derives 2000 extra
   addresses, so funds the phone received after its last backup are found
   too, and restarts itself once. The first sync of an old node takes a
   while: it scans the chain from the wallet's birthday.
7. **Channel check.** The last sync stage. A backup is a snapshot: a channel
   that closed after it was taken still looks open in it. Before any channel
   counts as funds, the node checks that this backup's
   wallet holds the channel's key and that the channel's funding output is
   on the bitcoin chain, unspent. It asks its own bitcoin peers, nobody
   else. A channel that closed is listed under "Closed on chain" with its
   closing transaction; a channel that fails the check is left alone.
   Measured: 10 minutes for channels opened in 2020, less for newer ones.
8. **Your funds.** Balances in channels, in pending closes and on-chain,
   with the channel list and a hint about the next step. **History** is one
   list of everything that moved money in or out of the app, newest first:
   Lightning payments sent and received with their descriptions, deposits
   and withdrawals, channel closes and where their funds went, on-chain
   sends. Each entry shows the amount, the fee and a link to the
   transaction. Totals at the top: received, sent, fees, the total of the
   list, and what the app holds now. **Export** saves the list as a CSV
   file you can open in a spreadsheet.
   The tool closes no channels. Breez closed its channels with the app's
   users from its side, so the funds of a restored app arrive on-chain, and
   closing from a backup is dangerous: a backup can hold an old channel
   state, and publishing it lets the peer take the whole channel. A
   channel that is still open is shown with a request to send the log to
   Breez support (contact@breez.technology).
9. **Send the on-chain balance** to an address with a fee choice, then the
   transaction id with a link to mempool.space.

The **Log** button opens a panel with every tool message and the node's
log lines as they happen. **Save log** writes the whole log plus the last
2000 lines of `lnd.log` to a file the user picks; **Copy** puts it on the
clipboard. Ask users to attach it when reporting a problem.

The work folder (default `~/.breez-recovery`) holds the cached sign-ins, a
`current` file naming the backup in use, and `backups/<node id>/` with one
complete node folder per restored backup (`zip-<hash>` for a backup file).
A node that an earlier release left directly in the work folder is moved
into `backups/` on the first start; its sync progress moves with it.

Advanced settings on the welcome screen: the work folder and the bitcoin peers (default: the Breez nodes
bb1.breez.technology and bb2.breez.technology; the node connects only to
the peers listed, and refuses to start when none of them answers). Set
other peers with compact block filters here if the Breez nodes are gone.

Environment overrides, mainly for testing: `BREEZ_RECOVERY_WORKDIR`,
`BREEZ_GOOGLE_CLIENT_ID`, `BREEZ_GOOGLE_CLIENT_SECRET`.

### Building the app

Prerequisites: Go 1.25, the [Wails](https://wails.io) CLI
(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0`), and on Linux
`libgtk-3-dev` plus `libwebkit2gtk-4.1-dev` (Ubuntu 24.04 and later) or
`libwebkit2gtk-4.0-dev` (older). Windows needs the WebView2 runtime, which
every updated Windows 10/11 has.

```sh
cd ui
wails build -skipbindings -tags walletrpc,chainrpc \
  -ldflags "-X main.version=1.0.0 \
            -X github.com/breez/breez-mobile-recovery/core.GoogleClientID=<id> \
            -X github.com/breez/breez-mobile-recovery/core.GoogleClientSecret=<secret>"
# Linux with webkit2gtk 4.1: -tags webkit2_41,walletrpc,chainrpc
# other targets: -platform darwin/universal | windows/amd64 | linux/amd64
```

The binary lands in `ui/build/bin/`. The frontend is plain HTML,
CSS and JavaScript under `ui/frontend/dist`, embedded in the binary; there
is no npm step. `ui/frontend/mock/serve.sh` serves it with a mocked backend
for UI work without a node. `CLAUDE.md` holds the working notes for anyone
changing the tool, with or without Claude Code.

### Releases

The workflow `.github/workflows/build.yml` builds macOS (universal),
Windows and Linux on every tag named `v*` and attaches the
archives to a GitHub release. It needs two repository secrets,
`RECOVERY_GOOGLE_CLIENT_ID` and `RECOVERY_GOOGLE_CLIENT_SECRET`, the Desktop
app OAuth client of the `breez-technology` project. It can also be run by
hand from the Actions tab, and runs on every pull request, to get test builds as artifacts.

The macOS app is not signed or notarized yet. Users must right-click and
choose Open the first time, or run
`xattr -dr com.apple.quarantine "Breez Recovery.app"`.

## Command line tool

```sh
go build -tags walletrpc,chainrpc -ldflags "-X github.com/breez/breez-mobile-recovery/core.GoogleClientID=<id> -X github.com/breez/breez-mobile-recovery/core.GoogleClientSecret=<secret>" .
```

```
recovery snapshots                                  # sign in to Google, list backups
recovery snapshots --icloud                         # same for iCloud
recovery restore --node-id <id> --mnemonic "..."    # download and decrypt from Drive
recovery restore --icloud --node-id <id>            # from iCloud
recovery restore --zip backup.zip --mnemonic "..."  # from a backup file
recovery backups                                    # backups restored on this computer, * = in use
recovery use <name>                                 # continue with another restored backup
recovery status                                     # start node, sync, check channels, print balances
recovery sweep --address bc1...                     # send the on-chain balance out
recovery history                                    # every payment, close and on-chain move, with totals
recovery history --json                             # the same as JSON, plus the raw app payment list
recovery history --csv > history.csv                # the same as a spreadsheet
recovery lncli <command>                            # any lncli command against the node
```

Global flags: `-workdir` (default `~/.breez-recovery`), `-network`,
`-breezserver`, `-bootstrap`, `-closedchannelsurl`, `-lsptoken`, `-feeurl`,
`-icloud-token`, `-peer` (pin bitcoin peers), `-v` (stream node logs to
stderr).

The defaults are the production values from the `breez.conf` and `lnd.conf`
bundled in the released APK (`assets/flutter_assets/conf`). The LSP token
is not bundled; it is only needed for LSP features, not for syncing or
sweeping. Bake it in with
`-X github.com/breez/breez-mobile-recovery/core.LSPToken=...` if wanted.

## Security notes

* The work folder holds the restored wallet, including its keys, plus the
  cached Google refresh token (`gdrive-token.json`) and Apple session
  (`icloud-session.json`). Files are created with owner-only permissions.
  Delete the folder once the funds are out; "Forget cached sign-ins" in
  Advanced settings removes just the sign-ins.
* Sign-ins happen in the system browser on Google's and Apple's own pages.
  The app never sees the account password. Google's redirect comes back to
  a random localhost port, bound with PKCE and a one-time state; Apple's
  comes back through the static callback page and a fixed localhost port.
  Both listeners exist only while a sign-in is pending, and the browser is
  sent on to `docs/signed-in.html` so no code or token stays in the
  address bar.
* Apple's session token travels through the callback page's URL, so the
  GitHub Pages server sees it in its request log. Using the container's
  custom URL scheme instead would avoid that; it needs an app bundle that
  registers the scheme and a token created for it.
* The Google client id and secret and the CloudKit API token are inside
  the binary. Google does not treat desktop client secrets as confidential
  and Apple's web token is meant for client-side use; both only identify
  the app, never a user.
* The embedded lnd listens on nothing: its RPC is in-memory and it makes
  only outbound bitcoin peer connections.
* The backup phrase is used in memory to derive the decryption key and is
  never written to the log or to disk.
* Releases ship a `SHA256SUMS` file. The macOS app is not signed yet.

## Notes

* The tool depends on the breez library at a pinned commit. Go does not
  propagate `replace` directives, so `go.mod` repeats the library's and
  they must be kept in sync when the library is bumped.
* Restoring a snapshot marks it in the cloud as restored by this machine,
  exactly as a new phone would. A phone still running that node stops
  itself on its next start to avoid a penalty.
* lnd on mainnet with neutrino needs an external fee estimator (`-feeurl`,
  default is the Breez one).
* Channels opened by the Breez LSP are zero-conf with scid aliases; the
  generated `lnd.conf` enables those protocol options like the app does.
* The work folder contains the wallet. Users should delete it once the
  funds are out.
