# Restoring a second backup: what happened, what was found, what changed

2026-09-17. Written for review before any of the channel-safety work is
merged. Everything here is either a fact taken from a log, a database or
the chain, or is marked as unverified.

## Summary

Roy restored a second backup (Dec 2022) over a work dir that already held
the Mar 2026 one. Three separate problems came out of it:

1. The app crashed twice (both causes fixed and released, alpha.25).
2. The app showed 1,583,210 sat "in channels" for channels that closed on
   chain in 2022 and 2025, and offered to close them. Roy force closed
   seven channels that cannot be closed. No funds were at risk here, but
   the same behaviour on a channel that is still open would broadcast a
   revoked commitment, which the peer can punish by taking the channel.
3. That Dec 2022 backup pairs one node's `wallet.db` with another node's
   `channel.db`. The wallet in it cannot sign for any of the channels in
   it. This came from Drive that way; the app did not mix anything.

Problem 1 is on main and released. Problems 2 and 3 are addressed by the
code on this branch, which is **not fully verified** (see "What is
verified").

## Timeline (from the logs)

| Time | What |
|---|---|
| 21:29:06 | Roy picks the Dec 2022 backup. Restore starts, old node stops. |
| 21:29:22 | Process panics. lnd had aborted at startup: `unable to extract on disk encrypted SCB: chacha20poly1305: message authentication failed`. The leftover `channel.backup` was encrypted with the 2026 node's seed. The library's account service then called `SubscribeInvoices` on a stopping node, ignored the error and read from a nil stream (`breez/breez account/payments.go:1310`), which kills the process. |
| 21:31 | Same crash again after a plain relaunch, so it was not caused by restoring in the same process. |
| 21:34 | With `channel.backup` deleted, the node starts. lnd loads seven channels as open (their state is from Dec 2022) and the sync begins. |
| 22:04 | Roy force closes. lnd broadcasts seven commitments. |
| 22:08 onwards | Investigation below. |

## Finding 1: the crash (fixed, released in alpha.25)

Two fixes, commit `3f2f785`:

- `guardRestore` deletes `data/chain/bitcoin/<net>/channel.backup` before a
  restore. lnd's static channel backup is encrypted with the seed of the
  node that wrote it; a foreign one aborts lnd at startup, which takes the
  library down and then the process panics.
- A restore no longer starts the node in the same process; it returns
  `ErrRestartRequired` and the app relaunches, as the address look-ahead
  already did. This was not the cause of this crash, but starting the
  library again in a process that stopped it is unsafe in general.

The nil-stream panic itself is a bug in the breez library: it logs the
`SubscribeInvoices` failure and then uses the nil stream. Any startup
failure of lnd therefore becomes a process crash. Not fixed here; worth an
issue or PR on breez/breez.

## Finding 2: channels that closed after the backup was taken

A backup is a snapshot. Channels that closed after it still look open in
the restored `channel.db`. lnd only learns otherwise when its chain
watcher sees the funding output spent, which needs a filter scan that had
not finished (the same slow spend check that made the two swept closes sit
in "closing" earlier in the day).

Meanwhile the app read lnd's channel list and reported the balances as
funds, and offered "Close channels and withdraw".

All seven funding outputs were already spent, checked against
mempool.space:

| Channel | Spent in block |
|---|---|
| 3919417704…:0 | 745717 (2022) |
| 508237f817…:0 | 761964 (2022) |
| 1c295889…:0, 330f6195…:0, 78275087…:0, e0d0afa4…:0, e2b4bd0a…:1 | 904333 (Jul 2025) |

So the force closes cannot confirm: the inputs they spend do not exist.
Nothing was lost. The danger is the same flow on a channel that is still
open: an old backup holds an old commitment, and publishing it lets the
peer take the whole channel.

## Finding 3: the Dec 2022 backup mixes two nodes

Downloaded again from Drive into an empty work dir
(`recovery restore --node-id 02c6b28e…`) and inspected without starting a
node:

- `wallet.db` seed derives node key `m/1017'/0'/6'/0/0` =
  `02c6b28eb051854c632f12a91fb7b3fe0768f1e957bf842040c3289e5cf99179b6`,
  which is the node id the backup is filed under.
- `channel.db` holds 7 open and 14 closed channels. For each open channel,
  `LocalChanCfg.MultiSigKey` was compared with the keys both wallets can
  derive (scope 1017'/0', families 0 to 9, first 300 indexes of branch 0):

| Wallet | Open channels it can sign |
|---|---|
| `02c6b28e…` (the backup's own wallet.db) | 0 of 7 |
| `02e66bcb…` (the Mar 2026 backup's wallet) | 7 of 7 |

The same holds for the closed channels (12 with a readable historical
record: 0 signable by its own wallet, 12 by the 2026 wallet).

So the channels in that backup belong to the node Roy had already
recovered. The wallet shipped with them is a different seed. The mix is in
the file in Drive, not something the app did: the check above was run on a
freshly downloaded copy. The likely origin is a 2022 reinstall on the
phone where Breez created a new wallet while the old channel database was
still in the folder, and the backup captured both.

Consequence for the user: that backup entry is not a second pot of money.
Its channels closed on chain years ago and their funds went to the 2026
wallet, whose remainder (15,721 sat) Roy swept this morning. Whether the
`02c6b28e…` wallet itself holds any on-chain coins is a separate question;
the sync running now derives its addresses and would show them.

## What changed on this branch

`core/chaincheck.go`, plus wiring in `core/core.go`, `core/node.go`,
`ui/app.go`, `main.go`, `ui/frontend/dist/app.js`:

- `CheckChannelsOnChain` runs after sync and before any close. For every
  channel lnd calls open it:
  1. asks lnd (`walletrpc.DeriveKey`) for the key at the channel's own
     `KeyLocator` and compares it with the channel's stored funding key.
     A mismatch means the channel belongs to another node: it is reported,
     left out of balances, and never closed.
  2. asks the node's own neutrino service (`GetUtxo`, in-process through
     `chainservice.Get`) whether the funding output is still unspent. A
     spent one is reported as closed on chain, left out of balances, and
     never closed.
- `Status` carries `ClosedOnChain []SpentChannel` and warnings; the funds
  screen lists them under "Closed on chain" with a link to the closing
  transaction.
- `CloseChannels` runs the check first if it has not run, and fails with
  an error rather than closing anything if the check cannot complete.
- Nothing is asked of a third party: both checks use the node itself.

## What is verified, and what is not

Verified:

- The mismatch finding (Finding 3): re-downloaded from Drive, keys
  compared offline, numbers above.
- The seven funding outputs are spent: mempool.space, and independently
  the first result of the in-process check (`3919417704…:0` reported spent
  by `b4feb56a…`, matching block 745717).
- The build compiles, `go vet` and `go test ./core` pass.

Not verified:

- The full `CheckChannelsOnChain` path inside the app. The probe that runs
  the same calls was still scanning when this was written; only one of the
  seven outpoints had come back. The scan runs at roughly 260 blocks per
  second, so a check from 2022 to today takes 15 to 20 minutes.
- The UI for "Closed on chain" and the refusal to close: not seen on
  screen yet, only the code path.
- The foreign-channel check has not run against a real node; the walletrpc
  `DeriveKey` comparison is unexercised.

## Questions for review

1. Is 15 to 20 minutes of scanning after every sync acceptable, or should
   the check run only when lnd reports open channels (it does today), and
   should its progress be a stage on the sync screen?
2. Should a channel that fails either check be hidden entirely, or shown
   greyed out with the reason? It is currently listed under "Closed on
   chain" (spent) or only as a warning (foreign).
3. Should the app refuse to restore a backup whose wallet cannot sign its
   own channels, or restore it and report, as it does now?
4. Should the backup picker group snapshots that belong to the same
   wallet, and steer to the newest? Roy expected two backups to mean two
   nodes; here they are one wallet's history plus a stray wallet.
5. The library's nil-stream panic: issue, PR, or pin a patched fork?

## How to reproduce the checks

```sh
# restore a snapshot into a scratch dir without starting a node
BREEZ_RECOVERY_WORKDIR=/tmp/verify ./recovery restore --node-id <node id>

# compare a channel's funding key with what a wallet can derive:
# open channel.db with channeldb.Open, read LocalChanCfg.MultiSigKey,
# then walk scope 1017'/0' families 0..9 of wallet.db with waddrmgr
# (public passphrase "public") and look for the same pubkey.

# ask the chain whether a funding output is spent, through the node's own
# neutrino: chainservice.Get(workdir, breezDB) then GetUtxo with
# WatchInputs(InputWithScript{OutPoint, PkScript}) and StartBlock at the
# channel's open height.
```
