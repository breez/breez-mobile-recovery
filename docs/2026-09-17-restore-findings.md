# Restoring a second backup: what happened, what was found, what changed

2026-09-17, reviewed and corrected 2026-09-20. Everything here is either a
fact taken from a log, a database or the chain, or is marked as unverified.
The review re-derived each finding from the raw data (both wallets, both
channel databases, Google Drive's file listing, the chain) and corrected
the first version where it was wrong; see "Review" at the end.

## Summary

Roy restored a second backup (node files written 2022-05-26) over a work
dir that already held the Mar 2026 one. Three separate problems came out of it:

1. The app crashed twice (both causes fixed and released, alpha.25).
2. The app showed seven channels holding 768,370 sat as open, although they
   closed on chain in 2022 and 2025, and offered to close them. Roy force closed
   seven channels that cannot be closed. No funds were at risk here, but
   the same behaviour on a channel that is still open would broadcast a
   revoked commitment, which the peer can punish by taking the channel.
3. That 2022 backup pairs one node's `wallet.db` with another node's
   `channel.db`. The wallet in it cannot sign for any of the channels in
   it. This came from Drive that way; the app did not mix anything.

Problem 1 was released in alpha.25. Problems 2 and 3 are addressed by the
channel check released in alpha.26, together with one folder per backup,
which removes the cause of problem 1 rather than its symptom.

## Timeline (from the logs)

| Time | What |
|---|---|
| 21:29:06 | Roy picks the 2022 backup. Restore starts, old node stops. |
| 21:29:22 | Process panics. lnd had aborted at startup: `unable to extract on disk encrypted SCB: chacha20poly1305: message authentication failed`. The leftover `channel.backup` was encrypted with the 2026 node's seed. The library's account service then called `SubscribeInvoices` on a stopping node, ignored the error and read from a nil stream (`breez/breez account/payments.go:1310`), which kills the process. |
| 21:31 | Same crash again after a plain relaunch, so it was not caused by restoring in the same process. |
| 21:34 | With `channel.backup` deleted, the node starts. lnd loads seven channels as open (their state is from May 2022) and the sync begins. |
| 22:04 | Roy force closes. lnd signs and broadcasts seven commitments (`CNCT: Broadcasting force close transaction`, seven times in lnd.log). Nothing in lnd questioned them: it only refuses a force close after the peer has told it the state is stale. |
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

## Finding 3: the 2022 backup mixes two nodes

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

Drive holds a sibling of this backup: `snapshot-0358d61f…`, written
2022-05-26 04:10:48 UTC, forty seconds before this one (04:11:28), with a
zip of nearly the same size (6,945,107 and 6,940,519 bytes). It has a third
wallet and the same seven channels, none of which it can sign. So on that
day the phone wrote the same channel database twice under two new node
ids.

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

## What the app does about it (alpha.26)

**The channel check** (`core/chaincheck.go`). lnd's channel list is no
longer taken as proof of funds. After sync and before anything is shown or
closed, every channel lnd calls open gets a verdict, and only "open" counts
in the balances or is ever closed:

1. The wallet must derive the channel's funding key at the channel's own
   `KeyLocator` (`walletrpc.DeriveKey`). Otherwise the channel belongs to
   another node (Finding 3).
2. The node's own neutrino must find the funding output on chain, with the
   expected script, and no spend of it up to the tip. A spend means the
   channel closed after the backup (Finding 2); it is listed under "Closed
   on chain" with the closing transaction.
3. Anything the check cannot positively confirm (output not found, no
   usable height, not checked yet) is left alone and reported.

**No closing at all** (decided 2026-09-20). Breez closed its channels
with the app's users from its side, so the funds of a restored app arrive
on-chain and the tool only needs to send them out. Cooperative close,
force close and the closing lncli commands are removed. This also ends
the risk below for good: a force close broadcasts the backup's
commitment, and if the phone used the channel after its last backup the
peer can take the whole channel. lnd refuses only once the peer has
reported the data loss, and force close was offered exactly when the peer
was offline. A channel the check still finds open is shown with a request
to send the log to Breez support.

**One folder per backup** (`core/dirs.go`). The crash in Finding 1 came
from two backups sharing one folder: the second inherited the first one's
`channel.backup`. Every backup now has a folder of its own under
`backups/`, the library is initialised only on the chosen backup's folder,
and restoring a backup again moves the old folder aside instead of
writing over it.

## Review, 2026-09-20

The first version of this document and of the channel check were reviewed
against the data and the neutrino and lnd sources. What held, what did not:

Held, re-derived independently:

- Finding 3. Walking scope 1017'/0' of both wallets: the backup's own
  wallet (node key 02c6b28e…) derives none of the seven funding keys, the
  2026 wallet (02e66bcb…) derives all seven at exactly the stored
  locators.
- Finding 2. All seven funding outputs are spent; the closing txids on
  chain equal the ones the 2026 node recorded for the same channels
  (blocks 745717, 761964 and five at 904333, all closed by the peer).

Wrong in the first version:

- "1,583,210 sat in channels" was the hard-coded balance of the UI mock
  (`ui/frontend/mock/mock.js`), not a number from Roy's node. The seven
  channels held 768,370 sat in that snapshot.
- The backup is not from Dec 2022. Its node files were written 2022-05-26;
  only its app-data zip is from 2022-12-09. The date the picker showed was
  Drive's folder time, which a restore updates. The picker now shows when
  the backup files were written.
- The check treated "funding output not found" as "still open". neutrino
  returns a nil report in that case; the old code counted it as funds and
  would have closed the channel.
- The check started its scan at `ShortChannelID.BlockHeight`. For a
  zero-conf channel that is an alias height of 16,000,000; neutrino's
  scanner never picks up a request that starts past the tip and its batch
  manager spins forever. Every Breez LSP channel since late 2022 is
  zero-conf, so the check would have hung for most users. The LSP's 2021
  to 2022 channels carry made-up SCIDs as well, with heights far below the
  real funding block.
- "Never closed" was not true: the close path filtered its own list but
  then called the library's `CloseChannels`, which acts on every channel,
  and ran the force close loop over the library's reply.
- The funds screen was fail-open: before the check had run, every channel
  lnd listed counted as funds.
- The scan reported no progress, and could take two passes over the chain
  because the first request to reach neutrino's scanner sets where the
  pass starts.

Verified on the real chain (`TestScanFundingOutputsLive`, on copies of the
channel databases of all 13 unencrypted backups in Roy's Drive): 17
channels, 17 verdicts equal to what the chain shows. 15 closed, each
reported with the closing transaction the chain has, among them both
channels with a made-up SCID (their funding blocks were located from the
broadcast height). The 2 that are still open are zero-conf channels; they
were found unspent at their real funding blocks, 758334 and 758364, where
the first version would have started at block 16,000,000 and never
returned. The folder layout was exercised with the CLI and the desktop app
on copies: two backup files side by side, a repeated restore refused and
then moved aside, an alpha.25 folder moved into `backups/` and its node
started, synced and shown from there.

Not verified: the "Closed on chain" list has been exercised through the
code paths and the mock, not yet on screen with a real node that has such
channels; the wallet key check has run offline
against the databases, not through `walletrpc.DeriveKey` on a live node
with a foreign channel.

## Open

1. The library's nil-stream panic (`account/payments.go:1310`): any lnd
   startup failure becomes a process crash. Issue or PR on breez/breez.
2. The backup picker could group snapshots that carry the same channels
   and steer to the one whose wallet can sign them.

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
