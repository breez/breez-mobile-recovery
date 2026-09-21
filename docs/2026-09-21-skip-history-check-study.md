# Skipping lnd's history check when the backup already has it all: study

2026-09-21. A study from source, nothing built. It needs a decision before
any code: the design writes the wallet's sync state.

BW = breez/btcwallet@v0.16.10-0.20241026173810-25a62401fb87, WTX = its
wtxmgr, LND = breez/lnd@v0.18.5-breez-2, NEU = breez/neutrino, in the Go
module cache.

## Why

After a restore btcwallet rescans from the wallet's birthday: one full pass
over the filters (4 to 5 GB for a 2021 wallet; 90 minutes for the 2019 one).
The app's own walk is a second pass over the same filters. For the typical
user, whose wallet never had an on-chain transaction, lnd's pass finds
nothing. One pass instead of two would halve a first sync.

## What the source says

- The phone resets the backup's sync state on purpose: `dropSyncedBlock`
  (LND/breezbackup/backup.go:112-158) recreates the sync bucket with only
  the birthday and genesis. The transaction store (wtxmgr) arrives intact.
- At start btcwallet finds no birthday block, locates it by binary search
  (BW/wallet/wallet.go:426-434, tolerating the empty header file), sets
  synced-to to it and rescans from there (rescan.go:299-302).
- With synced-to = H, a hash that is on the chain, and a birthday block
  that is set AND verified, it rescans from H+1 only and never looks at
  older blocks (wallet.go:490-510).
- A rescan is a provable no-op for transactions the store already has as
  mined in the same block (chainntfns.go:326-337, WTX/tx.go:409-413).
- lnd itself depends on the wallet's sync height only for IsSynced and
  confirmation counts.

## Conditions under which skipping loses nothing

1. Every transaction from the birthday to H that pays a wallet script or
   spends a wallet outpoint is in the store, mined in that block.
2. Every mined store transaction is in a block on the current chain.
3. Every output paying a wallet script is a credit in the store.
4. No unmined store transaction is on chain.
5. The search for funds after the backup found nothing.

## Smallest design

Before lnd's start on the second process: open wallet.db as the library's
dropwtx does; read the scripts with `ForEachRelevantActiveAddress` (this
also covers imported swap scripts, which xpubs do not) and the store with
wtxmgr; add the scripts to the app's one walk; if the five conditions hold,
write in ONE transaction: DeleteBirthdayBlock, PutSyncedTo for the last K
heights up to H = walk end - 6, SetBirthdayBlock(verified); read back.
After start, check lnd's "Started rescan from block ... (height H)" line,
the address count, and that the wallet's unspent set equals the walk's.
Any failed pre-check: write nothing (the ordinary history check runs). Any
failed post-check: order a fresh history check and restart.

## Traps found

- A birthday block present but NOT verified makes btcwallet retry forever.
- With only H stored, a reorg below H makes it retry forever: store K
  hashes and stand 6 below the tip.
- Addresses derived after start are not rescanned backwards, so a find
  must keep today's fresh history check.
- Not verified: whether peers still send reject messages for the phone's
  old unconfirmed transactions; AccountProperties on a locked manager.

## What was built (2026-09-21, core/syncskip.go)

Roy asked for it. The smallest form that carries the gain for the usual
user, a wallet that never had an on-chain transaction:

1. Before lnd starts, with the wallet closed, the scripts its own check
   watches are read (`ForEachRelevantActiveAddress`) and join the app's walk.
2. When the search for later funds found nothing, the shortcut is proposed
   only if: the walk watched exactly as many scripts as lnd logs for its
   check; no block paid any of them; the wallet's transaction list is empty.
   (Stricter than the five conditions above: no transaction at all, so
   nothing has to be compared with the store.)
3. The order (`history-known.json`: the last 144 headers up to 6 below the
   walk's end) is carried out by the next StartNode before the library
   opens anything: one wallet transaction, birthday block taken out and put
   back VERIFIED, read back. Not applicable or failed: lnd's ordinary check
   runs (a failure also orders the full drop).
4. If btcwallet then logs that it cannot synchronise from that state, the
   full history check is ordered and the app restarts.

A wallet with any on-chain history keeps lnd's ordinary check; comparing a
non-empty store with the chain is the next step if it is wanted.

Verified so far: the write and read-back on a copy of a real wallet (52
scripts, the number lnd logs for it). NOT yet verified end to end: a live
run on backup 0342c4e7 (nothing to find, the shortcut's case) is the test.

## Recommendation (as written before it was built)

Worth building, after the correctness findings of the audit are fixed and
only behind the five proofs, with the ordinary history check as the
default whenever any proof is missing. It is a change to wallet state and
needs its own design review and matrix run.
