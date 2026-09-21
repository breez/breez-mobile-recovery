# Finding funds paid after the backup: design

2026-09-21. Replaces the address look-ahead of alpha.12 to alpha.30.
Code: `core/addrscan.go`, `core/chaincheck.go`.

## The problem

A restored wallet only checks addresses it has derived, and a backup only
knows the addresses in use when it was written. Funds paid later sit past
them. Two real cases:

1. The phone swept a closed channel after its last backup (Roy's 15,721 sat,
   the 600 sat of backup 0341cadf).
2. An earlier restore with this tool collected a closed channel's funds and
   its folder is gone (Roy's 864 sat).

alpha.12 to alpha.30 derived 50 more addresses per branch THROUGH the wallet
(`NextAddr`). That moves the wallet's counter, so lnd's next sweep lands on
index counter+50, one past the window of any later restore. A fresh restore
of 0229fda8 showed 0 sat with 864 sat on chain. No window size cures it.

## The mechanism

Look ahead without touching the wallet, in the same filter walk that checks
the channels; touch the wallet only for an address that was really paid.

1. Read the account public keys and counters (`walletrpc.ListAccounts`),
   derive the next addresses offline (BIP 84, 86, 49).
2. Walk the compact filters from the wallet's birthday to the tip, matching
   the channels' funding scripts and the next 40 addresses of the four
   branches lnd and the phone pay to (witness key hash and taproot, receive
   and change).
3. Following the money: when a channel is found closed, follow EVERY output
   of the closing transaction through up to two spends (close, second-level
   HTLC transaction, sweep), and search every block the walk opens for the
   next 1,000 addresses of every branch. Whatever a close paid out is found
   wherever its sweep went. This is what finds Roy's 864 sat, with no
   special number.
4. Gap rule, for funds that did not come out of a channel in the backup (a
   plain payment, the phone's sweep of a close already under way): the
   search is complete when the 20 addresses after the highest paid one were
   looked for in every block, the gap limit wallets use. With 40 watched
   from the start that holds for any find in the first 20 without a second
   pass; a higher find adds a pass over only the blocks before the find.
5. Nothing found: mark the folder done, the wallet is untouched. Found, and
   the wallet does not know the payment: write the finds to
   `found-funds.json`, order a fresh history check (`history-recheck`),
   read the counters again, advance each branch to the paid address with
   `NextAddr`, check that the wallet arrived at the address derived here,
   restart. The next start drops the wallet's history ITSELF, before the
   library opens anything, and keeps the order until that succeeded.
6. After the history check the wallet must show every find
   (`verifyFound`), or the sync stops with an error.
7. Save the finished walk (`chain-walk.json`) so the channel check after the
   restart, and on every later start, carries on from its last block.

## What it relies on, and where that is written

| Fact | Source | If it were false |
|---|---|---|
| lnd sweeps to a taproot receive address of the default account and takes a NEW one for every sweep it publishes (each start publishes again: neutrino has no mempool), so a sweep's address can be arbitrarily far out | lnd `server.go:4991`, `sweep/sweeper.go:797-805,1694-1715` | this is why step 3 exists; an earlier version of this note claimed the address was kept until confirmation, which the adversarial review disproved |
| `NextAddr` hands out the address at the account's key count and advances it | btcwallet `waddrmgr`, lnd `walletkit_server.go:595` | step 5 fails loudly: the address returned must equal the derived one |
| Offline derivation equals the wallet's | BIP vectors in `addrscan_test.go`; at run time the wallet's last derived address per branch is compared (`ListAddresses`) | error before the walk; a wallet that never derived an address has nothing to compare, then the unit vectors stand alone |
| The header files start at the latest built-in checkpoint not after the wallet's birthday; earlier checkpoints stand alone in an otherwise empty file | breez `chainservice/bootstrap.go:139-195` | `walkStart` finds the first header with a successor; an empty header cannot be queried ("target hash not found in index") |
| A filter is verified against the previous filter header, so the first block's filter cannot be fetched | neutrino `query.go:571-660` | the first block is fetched directly, the walk starts after it |
| Nothing pays a wallet before its birthday; the birthday in wallet.db never changes | btcwallet `waddrmgr` sync bucket; read with bbolt before lnd's first start (lnd holds the file lock after) | - |
| Breez backups carry no sync height (reset to genesis) and the zip has no dates | measured on 4 backups, 2026-09-21 | a later, cheaper start would be possible; the birthday is always safe |
| The library's FORCE_RESCAN is unfit for ordering a re-check: Init only LOGS a failed drop, removes the file when its second, unrelated drop succeeded, and also drops lnd's height hints (another pass for lnd) | breez `bindings/api.go:134-150` | not used: the tool calls the library's `dropwtx.Drop` itself, synchronously, and keeps its own order file until it returned nil |
| Filters are not kept on disk; the memory cache holds 1,450 to 2,300 | neutrino `neutrino.go:76-82`, breez `chainservice/init.go:272-323` (no PersistToDisk) | - |

## Cost

The unit of cost is a full pass over the filters: 13 to 21 KB per block,
about 4 to 5 GB from mid 2021 to today, and nothing is reused between
passes. The number of watched scripts is noise: each script opens one block
in 784,931 by false positive, about 1% on top of the filters per 100 scripts.

| Case | alpha.30 | this design |
|---|---|---|
| Fresh restore, nothing paid later | lnd history check + channel walk | lnd history check + one walk (search and channels together) |
| Fresh restore, funds paid later | the same, and the funds are MISSED when an earlier restore collected them | lnd history check (started over once, after minutes) + one walk; the channel check carries on from the saved walk |
| Every later start | a full channel walk | the blocks since the last start |

The search runs right after the headers, while lnd checks the history: a
find costs the minutes of history check done so far, not a whole one.

## Failure behaviour

Every check stops with an error; none degrades to "nothing found". A saved
walk is used only if its format, channels and last block hash match the
node's chain; otherwise it is discarded, logged, and the walk is done again.
The search runs once per restore (`addresses-extended`); folders of older
releases carry that marker and are left alone.

## Tests

Unit: BIP vectors; a find inside the first gap; one past the window; out of
order payments; following to_remote, a delayed output and an HTLC through
its second-level transaction to sweeps 500 to 700 addresses out; bounded
second pass; carried walk; saved walk round trip and rejection; header file
with lone checkpoints; a walk above the node's first block with an older
foreign channel; a channel past the tip reached later. Real chain (gated): `TestFollowTheMoneyLive` finds Roy's 864
sat with no address in the filter match. Matrix: fresh restores of
0229fda8 (864 sat), 0341cadf (600 sat), 0342c4e7 (nothing), 02e66bcb (the
2019 node) against ground truth from block explorers.
