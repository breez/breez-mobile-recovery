# Breez Recovery

> **Experimental.** Tested on a handful of real backups so far. Keep the
> log (Log, then Save log) and report problems in the issues.

Restores a Breez app backup from Google Drive, iCloud or a backup file on
your computer, and sends the funds to a bitcoin address. It exists so funds
stay recoverable after the Breez app leaves the app stores. Desktop app and
command line tool, for macOS, Windows and Linux.

## Before you start

If the Breez app is still on your phone, make a safety copy first: open the
menu, then **Developers**, then **Export DB Files**, and keep the zip
somewhere safe. The tool does not need it, and it can restore from it: choose
"backup file" and pick that zip.

## Get it

Download the latest build from the
[releases](https://github.com/breez/breez-mobile-recovery/releases) and
check it with `sha256sum -c SHA256SUMS`.

* **Linux** needs GTK 3 and libwebkit2gtk-4.1 (Ubuntu 22.04 or later).
* **macOS**: the app is not notarized yet. Right-click it and choose Open
  the first time.
* **Windows** needs the WebView2 runtime, which Windows 10 and 11 have.

## How it goes

1. **Choose where the backup is**: Google Drive, iCloud or a backup file.
   Sign-in happens in your browser, on Google's or Apple's own page.
2. **Pick the backup** and enter its backup phrase if it is encrypted.
   PIN-encrypted backups (deprecated years ago) cannot be restored.
3. **Sync.** The restored app catches up with the bitcoin chain, looks for
   funds received after the last backup and checks its history, with
   progress and a time estimate. The first sync of an old app takes a
   while: one from 2019 took 90 minutes. The app restarts itself along the
   way.
4. **Your funds.** Three numbers: In channels, Pending, On-chain. **History**
   lists every payment, deposit, withdrawal and channel close, and exports
   as CSV.
5. **Send the on-chain balance** to an address of yours, with a fee choice.

Every backup is restored into a folder of its own, so restoring another one
never touches the first.

### Channels

Breez closed its channels with the app's users from its side, so a restored
app's funds arrive on-chain:

* A channel that was closed after your last backup shows under **Closed on
  chain**. If the close paid you, the amount shows as Pending, "being
  collected"; it moves to On-chain within minutes. Keep the app open.
* The tool never closes a channel. A backup can hold an old channel state,
  and closing with it can lose the channel's funds. If a channel is still
  open, the app asks you to email the list to contact@breez.technology.
* A balance too small to be paid out on chain (dust) is not counted.

## Command line

```
recovery snapshots [--icloud]                       # sign in, list backups
recovery restore --node-id <id> [--mnemonic "..."]  # from Google Drive
recovery restore --icloud --node-id <id>            # from iCloud
recovery restore --zip backup.zip                   # from a backup file
recovery backups                                    # restored backups, * = in use
recovery use <name>                                 # continue with another one
recovery status                                     # sync, check channels, balances
recovery sweep --address bc1...                     # send the on-chain balance
recovery history [--csv | --json]                   # every money movement
```

`recovery -h` lists the global flags.

## Where things are kept

`~/.breez-recovery` (changeable under Advanced settings) holds your cached
sign-ins and one folder per restored backup under `backups/`. Those folders
contain the wallet's keys: delete the work folder once the funds are out.

## Security

* Sign-in happens in your browser; the app never sees your password.
  "Forget cached sign-ins" removes the saved sign-ins.
* The backup phrase is used in memory only, never logged or written to disk.
* The node makes outbound connections only and listens on nothing.
* Channel and chain checks use the node's own bitcoin peers, no third party.
* With iCloud, Apple's session token passes through a static GitHub Pages
  page on its way back to the app.

## Tested so far

Steps: 1 sign in and list, 2 restore, 3 sync and see the right funds,
4 send the on-chain balance.

| Backup source | Linux | macOS | Windows |
|---|---|---|---|
| Google Drive | steps 1 to 4, 2026-09-17 (sent 15,370 sat); closed channel's 999 sat collected, 2026-09-20 | not run | not run |
| iCloud | not run | steps 1 and 2, 2026-09-24 | not run |
| Backup file | steps 2 and 3, 2026-09-20 | not run | not run |

## Working on it

`CLAUDE.md` has the build steps, the release process and the working notes.
