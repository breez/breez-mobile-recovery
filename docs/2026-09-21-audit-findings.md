# Source audit, 2026-09-21: findings and their state

Each module was read line by line against the pinned source of the Breez
library, lnd, btcwallet and neutrino. A finding is listed here only after it
was checked against the code a second time. State: OPEN, FIXED (with the
test that pins it), DECISION (needs the product owner), or NOTE.

Paths: LIB = breez@v0.0.0-20260906202014-eade35430c1b, LND =
breez/lnd@v0.18.5-breez-2, in the Go module cache.

## Restore and decryption

| # | Finding | Evidence | State |
|---|---|---|---|
| R1 | A failed Google Drive restore reports success: the library's RestoreBackup logs the restore error and returns the result of UpdateLatestBackupTime instead. Wrong phrase that passes the checksum, failed download, corrupt zip all print "Backup restored"; the old folder was already moved aside and the snapshot marked as restored. | LIB/bindings/api.go:317-328; core/core.go GoogleRestore | FIXED: the tool downloads from Drive itself (core/drive.go) and restores through its one path (core/restore.go); verified on the real Drive, files byte-identical to the pristine backup |
| R2 | Zip and iCloud restores write the files one by one into the final folder, and a folder is a node as soon as wallet.db exists. A kill or full disk in between leaves a folder lnd starts on with an EMPTY channel.db: 0 channels, "nothing to recover". | core/localzip.go restoreFiles; core/dirs.go hasNode; lnd kvdb creates a missing db | FIXED: decode everything in memory, staging folder, one rename; hasNode needs all three files; tests TestFailedRestoreLeavesTheOldOneAlone, TestHalfARestoreIsNoNode |
| R3 | A forced re-restore moves the working folder aside before the new restore succeeded; the moved folder is never listed again (name has a dot). | core/dirs.go prepareBackupDir, backupNameRE | FIXED: the old folder is moved aside only after the new restore is complete in staging |
| R4 | iCloud: read and JSON errors of a response are dropped (a truncated reply reads as "no Breez backups found"); continuationMarker never followed; the restore downloads with the records cached at listing time; the session token is in the URL query and Go prints the URL in network errors, so it can reach the saved log. | core/icloud.go post, snapshots, download; net/http url.Error | FIXED by reading (iCloud still never run end to end): errors returned, continuation markers followed, records asked again at restore, downloads in memory with length check, URL stripped from network errors |
| R5 | readZip keys entries by base name: two entries with one name, last wins silently. | core/localzip.go readZip | FIXED: an error |
| R6 | Backups of former PIN users are refused even when a later app version wrote them unencrypted (the library's test lets an empty encryption type override the legacy flag, the tool's does not); real PIN backups (key = sha256(pin)) are unsupported. | LIB/backup/drive.go:156-162; core/drive.go:62-65 | DECIDED by Roy 2026-09-21: not handled |
| R7 | The phone's "Export DB Files" zip (1_channel.db, 2_wallet.db, 3_wallet.db, 4_breez.db), which the README tells users to keep as a safety copy, cannot be restored. Fails loudly. | breezmobile lib/routes/dev/dev.dart:491-496 | FIXED: restore from file accepts the numbered layout and takes the backup copy of the wallet (2_wallet.db), the kind of file a cloud backup holds; every restore now checks the three databases for integrity (size against page count, then a page walk) before it is placed; tests TestBackupZipLayouts, truncated database in TestFailedRestoreLeavesTheOldOneAlone |
| R8 | Library restore path leaves the downloaded backup.zip behind and writes decrypted files with loose permissions (protected only by the 0700 parent). Goes away with R1's fix (own download). | LIB/backup/drive.go:339, crypto.go:83 | FIXED with R1 |

Verified true: AES-GCM framing and key derivation match the phone for both
phrase types; file names and targets match; Drive and iCloud layouts match;
the tool's own code never deletes or overwrites a wallet file; zip path
traversal is not possible; tokens are 0600 in a 0700 folder.

## Balances, status, send

| # | Finding | Evidence | State |
|---|---|---|---|
| B1 | When lnd's PendingChannels fails (old nodes, "unable to find arbitrator") Status returns early: once lnd moves a closed channel out of ListChannels its funds are in no number, the screen says "Nothing left to recover", the refresh stops. | core/node.go status early return; LND/rpcserver.go:4276 | FIXED: what a closed channel owes comes from the chain walk, which keeps following a channel after lnd stops listing it (saved walk); a failed PendingChannels no longer ends the status |
| B2 | CLI `status` stops the node while a close is in limbo but the sweep is not published yet (collecting() ignores Pending); lnd only sweeps while running, at the next block. | main.go collecting | FIXED |
| B3 | CLI `sweep` panics: ValidateAddress calls the library before it is initialised. No send possible from the CLI. | main.go cmdSweep; LIB/bindings/api.go getBreezApp | FIXED: own address validation, no library needed |
| B4 | PrepareSweep fails for small balances when only the fast fee target leaves dust: the library aborts on the first failing target, the slower ones are never tried. | LIB/account/sweep.go:89-99 | FIXED: own dry run per fee target, failures per target tolerated |
| B5 | A channel found closed on chain whose payout cannot be derived (no static remote key) counts as nothing in the advice, refresh and CLI loop, before lnd has swept it. | core/chaincheck.go toUsScript nil; ui app.js advice | FIXED: Status.Unresolved; the advice, the refresh and the CLI loop treat it as not done |
| B6 | lnd's sweep is counted twice until it confirms: as Pending (limbo) and as On-chain unconfirmed. | core/node.go status; LND commit_sweep_resolver.go:395-433 | FIXED: a sweep of an output counted as Pending is taken off the unconfirmed balance; lnd's own figure for a close the walk follows is skipped; test |
| B7 | Each fee-bumped replacement of lnd's sweep stays in the wallet's unmined store (neutrino has no mempool) and adds to the unconfirmed balance. | LND/sweep/fee_bumper.go; btcwallet wtxmgr unconfirmed.go | FIXED: of unconfirmed transactions spending the same output only the newest counts; test |
| B8 | Fee URL unreachable: all three fee options silently become the 1 sat/vB floor; after a send the screen says "Nothing left to recover" with no "sent, waiting" state; a broadcast no peer accepted still reads as sent. | LND/lnwallet/chainfee/estimator.go; neutrino query.go:1073 | FIXED: PrepareSweep asks the fee source itself and stops when it has no rates; Status.Outgoing gives a "sent, waiting for its confirmation" state. A broadcast nobody accepted cannot be told with neutrino; the wallet sends it again on the next start |
| B9 | sweptBy matches on the closing txid, not the commit output's outpoint: a confirmed wallet spend of the ANCHOR hides a pending close. | core/node.go sweptBy | FIXED: a close counts as swept only when confirmed wallet transactions spending it bring in at least half of what is owed; test TestSweptByNeedsTheFunds |
| B10 | Waiting-close channels bypass the chain check; their limbo balance can be counted while the payout is already on-chain. | core/node.go status | FIXED: a waiting close whose closing transaction the wallet holds confirmed is not counted again |
| B11 | An unspent swap deposit (P2WSH) is in none of the numbers and the tool has no refund step, though the library has one. | LND btcwallet.go:1163; LIB/bindings/api.go:419 | DECIDED by Roy 2026-09-21: not handled |
| B12 | Public anchor channels would make lnd keep a reserve back or refuse the send; Status never reads ReservedBalanceAnchorChan. LSP channels are private, so the reserve should be 0: enforce with a check. | LND/rpcserver.go:1441-1497, lnwallet/wallet.go:1153 | FIXED: the plan reports what the prepared transaction keeps back (SweepPlan.Kept), from the transaction itself |

Verified true: send-all never picks unconfirmed or dead outputs;
deadUnconfirmed only subtracts transactions that cannot confirm; addresses
are validated per network twice; the lncli refusal list has no way through;
toUsScript derives the peer's commitment correctly.

## Lifecycle, markers, platforms

| # | Finding | Evidence | State |
|---|---|---|---|
| L1 | The ordered history re-check can be lost: the library removes FORCE_RESCAN when only its SECOND drop (hint cache) succeeded, so a failed wallet-history drop is reported once (or not at all: the failure is read from a log line handled by another goroutine, a race) and the next start neither re-checks nor searches. Found funds would stay hidden. | LIB/bindings/api.go:143-150; core/core.go StartNode check | FIXED: the tool drops the history itself (dropwtx.Drop, synchronous) and keeps `history-recheck` until it succeeded; FORCE_RESCAN no longer used |
| L2 | Stop or the 15 min timeout during the first start's header wait, then Continue: StartNode returns at once because the node runs, the chain-ready restart is skipped and the whole sync runs on an lnd started at the bootstrap checkpoint (closed-channel funds invisible for hours). | core/core.go StartNode early return | FIXED: StartNode always runs the first-start step, also when the node already runs |
| L3 | Bitcoin peers the user once set on the phone travel inside breez.db and REPLACE the pinned peers; checkPeers tests the wrong list; a dead custom peer leaves the folder stuck. | LIB/chainservice/init.go:170, db/network.go:29-50 | FIXED: the stored peers are reset after Init, so the pinned and tested peers are the ones used |
| L4 | No single-instance protection: a second instance blocks forever on breez.db with no message; on Linux and macOS a second instance re-restoring the same backup renames the folder under the first, whose marker writes then land in the NEW restore (search skipped without having run). | LIB/db/db.go:103 (no timeout); core/dirs.go:253-267 | FIXED: one program per work folder (core/lock.go, an open bbolt file: locked on every platform, released by the OS at exit); the wait covers the app restarting itself |
| L5 | Same as R2: partial restore counts as a node. | core/dirs.go hasNode | FIXED with R2 |
| L6 | ApplySettings while a node runs: unbounded Stop under the lock (window cannot be closed if it hangs), then a second in-process Init, which is known to hang or crash. | ui/app.go:278-299 | FIXED: refused once the library ran in this process |
| L7 | A failed relaunch leaves the raw error "restart required" and a Continue that re-inits in-process. | ui/app.go relaunch | FIXED: the user is told to close and reopen the app |
| L8 | Windows: Google restore fails when the work folder is on another drive than %TEMP% (rename across volumes), with the wrong hint, after the snapshot was marked. Goes away with R1's fix. | LIB/backup/drive.go:339, manager.go:164 | FIXED with R1: no temp folder, no cross-volume rename (staging sits next to its final place) |
| L9 | "Open folder" does nothing on any platform: Wails 2.16 rejects file:// URLs. | wails internal/frontend/utils/urlValidator.go:25 | FIXED: explorer / open / xdg-open |
| L10 | wallet-birthday and current are written with a plain WriteFile; an empty wallet-birthday after power loss fails every sync for that folder (loud, permanent). | core/core.go:555; core/addrscan.go | FIXED: writeFileAtomic; an unreadable wallet-birthday is read again |
| L11 | Header detection depends on neutrino's logger at info level (CLI -loglevel warn breaks the first start, loudly); rescanStartRe misses "for 1 address" (display only). | LND/log.go:137; btcwallet wallet/rescan.go:253 | FIXED for the regex; the log level dependency stays (CLI only, fails loudly) |
| L12 | waitSynced has no stall detection: if the bitcoin peers go away the screen stands still with no message. | core/node.go waitSynced | FIXED: a message after 10 minutes without progress |
| L13 | os.UserHomeDir error discarded: a relative work folder. | core/core.go:80 | FIXED: an empty work folder is refused with a message |

Verified true: the relaunched child cannot fail on the parent's file locks
(bbolt retries, the OS releases locks at exit); os.Exit during library work
is safe for the databases (single bbolt transactions); a death anywhere in
the search before its marker reruns it from the start; every parsed log
line matches its format string in the pinned source; every generated config
key has a consumer; the config injection filter holds; rename over an
existing file works on Windows; the library's path.Join is harmless with
the folders this tool passes; move-aside never deletes.

## Adversarial review of the address search

| # | Finding | Evidence | State |
|---|---|---|---|
| A1 | Same as L1. | | FIXED |
| A2 | The design claimed lnd keeps its sweep address until a sweep confirms; it releases it at PUBLISH, and with neutrino every start publishes again with a new address, so a sweep can land past any window. | LND/sweep/sweeper.go:797-805,1694-1715 | FIXED: every output of a closing transaction is followed through two spends; gap back to the standard 20 (51 was a workaround for alpha.30's own flaw) |
| A3 | A channel older than the node's first block (a foreign channel in a mixed-up backup) made every sync fail: the walk asked for filters that do not exist. | neutrino headerfs, query.go:571-660 | FIXED: walk floor = first block + 1, first block fetched directly; test TestWalkAboveTheNodesFirstBlock |
| A4 | Nothing checked that the restarted wallet shows what was found. | core/addrscan.go | FIXED: found-funds.json + verifyFound after the history check |
| A5 | A channel past the tip at first sight was written off for good, also in the saved walk. | core/chaincheck.go newChainWalk | FIXED: never reached yet, looked at again on every run; test |
| A6 | The search could start before this process had seen the headers catch up. | core/node.go syncProgress estimate branch | FIXED: waitHeadersSynced before the search; error when the start is past the tip |
| A7 | lnd's own sweep confirming during the walk forced a needless second history check. | core/addrscan.go | FIXED: no re-check when the wallet already knows every found transaction |
| A8 | Recognition depth capped at counter+1000. | core/addrscan.go extend | FIXED: grows with the window |

Verified true by the review: key counts are the next index; the default
account filter returns exactly the 49', 84', 86' accounts; every lnd payout
(sweep, justice, anchor, cooperative delivery) goes to a branch the search
watches; the bootstrap rule and the monotonic first-block test; the saved
walk round-trips every field that feeds a verdict and cannot show a closed
channel as open; no ordering of payments beats the gap logic.

## Still to come

Skipping lnd's redundant history check: studied, see
docs/2026-09-21-skip-history-check-study.md; needs a decision.
