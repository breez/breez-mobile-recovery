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
core/      the recovery logic (cloud sign-in, restore, node, close, sweep)
ui/        the desktop app (Wails: Go + a small HTML frontend)
main.go    the command line tool
```

## Test plan

A cell records the furthest step reached, who ran it and when. A run
counts once a real backup went through the app on that OS. Steps:

1. Sign in and list the backups
2. Restore (with the backup phrase where the backup is encrypted)
3. Sync to the chain tip and see the funds screen with the right balances
4. Close channels cooperatively to an address
5. Force close a channel whose peer is offline, wait for maturity
6. Send the on-chain balance out

| Backup source | Linux | macOS | Windows |
|---|---|---|---|
| Google Drive | in progress, roys, 2026-09-16 | not run | not run |
| iCloud | not run | not run | not run |
| Backup file | steps 2 and 3 on a fresh node with no funds, 2026-09-16 | not run | not run |

Steps 4 to 6 have not been run on real funds anywhere yet. Update the
table in the same pull request as any fix the run produced.

## How the Breez app backup works

* The backup is a zip of three files: lnd's `wallet.db` and `channel.db`
  plus `breez.db`. It is the full channel state, so the restored node can
  close channels on its own.
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

1. **Welcome.** If a restored app already exists in the work folder, offers
   to continue with it or restore a different backup.
2. **Where is your backup.** Google Drive, iCloud or a backup file.
3. **Sign in.** The system browser opens for Google or Apple. The link is
   shown in the app in case the browser did not open. Sign-ins are cached
   in the work folder.
4. **Choose the backup.** One entry per node, latest first, with the
   encryption type. PIN-encrypted backups (deprecated years ago) are shown
   but cannot be restored.
5. **Backup phrase**, for encrypted backups, with word count and validation.
6. **Restore**, then **sync** with a progress bar, block heights and stage
   list. The first sync of an old node takes a while: it scans the chain
   from the wallet's birthday.
7. **Your funds.** Balances in channels, in pending closes and on-chain,
   with the channel list and a hint about the next step.
8. **Close channels and withdraw** to an address. Cooperative closes pay the
   address directly. Channels whose peer is offline are skipped and can be
   force closed (funds mature after the channel delay, up to ~720 blocks).
9. **Send the on-chain balance** to an address with a fee choice, then the
   transaction id with a link to mempool.space.

The **Log** button opens a panel with every tool message and the node's
log lines as they happen. **Save log** writes the whole log plus the last
2000 lines of `lnd.log` to a file the user picks; **Copy** puts it on the
clipboard. Ask users to attach it when reporting a problem.

Advanced settings on the welcome screen: the work folder (default
`~/.breez-recovery`) and pinned bitcoin peers (default: DNS seed discovery
of compact-filter nodes, so the tool keeps working after the Breez hosts
are gone).

Environment overrides, mainly for testing: `BREEZ_RECOVERY_WORKDIR`,
`BREEZ_GOOGLE_CLIENT_ID`, `BREEZ_GOOGLE_CLIENT_SECRET`.

### Building the app

Prerequisites: Go 1.23+, the [Wails](https://wails.io) CLI
(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0`), and on Linux
`libgtk-3-dev` plus `libwebkit2gtk-4.1-dev` (Ubuntu 24.04 and later) or
`libwebkit2gtk-4.0-dev` (older). Windows needs the WebView2 runtime, which
every updated Windows 10/11 has.

```sh
cd ui
wails build -skipbindings \
  -ldflags "-X main.version=1.0.0 \
            -X github.com/breez/breez-recovery/core.GoogleClientID=<id> \
            -X github.com/breez/breez-recovery/core.GoogleClientSecret=<secret>"
# Linux with webkit2gtk 4.1: add -tags webkit2_41
# other targets: -platform darwin/universal | windows/amd64 | linux/amd64
```

The binary lands in `ui/build/bin/`. The frontend is plain HTML,
CSS and JavaScript under `ui/frontend/dist`, embedded in the binary; there
is no npm step.

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
go build -ldflags "-X github.com/breez/breez-recovery/core.GoogleClientID=<id> -X github.com/breez/breez-recovery/core.GoogleClientSecret=<secret>" .
```

```
recovery snapshots                                  # sign in to Google, list backups
recovery snapshots --icloud                         # same for iCloud
recovery restore --node-id <id> --mnemonic "..."    # download and decrypt from Drive
recovery restore --icloud --node-id <id>            # from iCloud
recovery restore --zip backup.zip --mnemonic "..."  # from a backup file
recovery status                                     # start node, sync, print balances
recovery close --address bc1...                     # cooperative close of all channels
recovery close --address bc1... --force             # force close channels whose peer is gone
recovery sweep --address bc1...                     # send the on-chain balance out
recovery lncli <command>                            # any lncli command against the node
```

Global flags: `-workdir` (default `~/.breez-recovery`), `-network`,
`-breezserver`, `-bootstrap`, `-closedchannelsurl`, `-lsptoken`, `-feeurl`,
`-icloud-token`, `-peer` (pin bitcoin peers), `-v` (stream node logs to
stderr).

The defaults are the production values from the `breez.conf` and `lnd.conf`
bundled in the released APK (`assets/flutter_assets/conf`). The LSP token
is not bundled; it is only needed for LSP features, not for closing
channels or sweeping. Bake it in with
`-X github.com/breez/breez-recovery/core.LSPToken=...` if wanted.

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
