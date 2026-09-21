package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // the wallet file's driver
)

// Skipping lnd's history check when it cannot find anything.
//
// After a restore the wallet checks the whole chain since its birthday for
// its addresses: one full pass over the filters, gigabytes and, next to the
// app's own walk, about half of a first sync. For the usual Breez user that
// pass finds nothing, because the wallet never had an on-chain transaction:
// its money lived in channels the LSP opened.
//
// The app's walk can prove that. It watches every script the wallet's own
// check watches, over the same blocks. When
//
//  1. the search for later funds found nothing,
//  2. the walk watched exactly as many scripts as lnd says its check covers,
//  3. no block paid any of them,
//  4. the wallet holds no transaction at all, confirmed or not,
//
// then the wallet's check has nothing to find below the block the walk
// ended at, and the wallet is told so: its sync state is set to a block a
// little below that (study and source references:
// docs/2026-09-21-skip-history-check-study.md). lnd then only checks the
// blocks after it. In every other case nothing is written and lnd's
// ordinary check runs, as it always did.
//
// The backup's sync state was reset on purpose by the phone
// (lnd breezbackup/backup.go dropSyncedBlock); btcwallet then locates the
// birthday block and rescans from it. With a synced-to block that is on the
// chain and a birthday block that is set and verified it rescans from the
// synced-to block only (btcwallet wallet/wallet.go syncWithChain).
const (
	historyKnownFile = "history-known.json"
	// knownBelowTip is how far below the walk's end the wallet is set, and
	// knownHashes how many block hashes it is given: with one hash only, a
	// reorganisation below it would leave btcwallet unable to walk back.
	knownBelowTip = 6
	knownHashes   = 144
)

type knownHistory struct {
	Blocks []knownBlock `json:"blocks"` // ascending; the last is the new synced-to
}

type knownBlock struct {
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
	Time   int64  `json:"time"`
}

// openWalletFile opens wallet.db the way the library's dropwtx does. lnd
// must not run: it holds the file's lock.
func openWalletFile(path string) (walletdb.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return walletdb.Open("bdb", path, true, 10*time.Second)
}

var waddrmgrNamespace = []byte("waddrmgr")

// walletScripts are the scripts the wallet's own history check watches
// (btcwallet ForEachRelevantActiveAddress: every address of the default key
// scopes, imported scripts included, and the change addresses of the
// others). Read with the wallet closed.
func walletScripts(walletDB string, network string) ([][]byte, error) {
	params, err := netParams(network)
	if err != nil {
		return nil, err
	}
	db, err := openWalletFile(walletDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var scripts [][]byte
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespace)
		if ns == nil {
			return errors.New("no address manager")
		}
		mgr, err := waddrmgr.Open(ns, []byte("public"), params)
		if err != nil {
			return err
		}
		defer mgr.Close()
		return mgr.ForEachRelevantActiveAddress(ns, func(addr btcutil.Address) error {
			script, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}
			scripts = append(scripts, script)
			return nil
		})
	})
	return scripts, err
}

// knownUpTo builds the sync state for a walk that ended at block end.
func knownUpTo(headerAt func(height uint32) (*wire.BlockHeader, error), end, first uint32) (*knownHistory, error) {
	if end < first+knownBelowTip+2 {
		return nil, errors.New("the walk is too short")
	}
	top := end - knownBelowTip
	bottom := first + 1
	if top > knownHashes && top-knownHashes+1 > bottom {
		bottom = top - knownHashes + 1
	}
	var out knownHistory
	for h := bottom; h <= top; h++ {
		header, err := headerAt(h)
		if err != nil {
			return nil, err
		}
		out.Blocks = append(out.Blocks, knownBlock{Height: h, Hash: header.BlockHash().String(), Time: header.Timestamp.Unix()})
	}
	return &out, nil
}

// applyKnownHistory writes the sync state into the closed wallet, in one
// transaction. It reports false, and writes nothing, when the wallet is not
// in the state the write needs: a birthday block that lnd has located and
// verified, below the blocks to write.
func applyKnownHistory(walletDB, network string, known *knownHistory) (bool, error) {
	if len(known.Blocks) == 0 {
		return false, nil
	}
	params, err := netParams(network)
	if err != nil {
		return false, err
	}
	db, err := openWalletFile(walletDB)
	if err != nil {
		return false, err
	}
	defer db.Close()
	applied := false
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespace)
		if ns == nil {
			return errors.New("no address manager")
		}
		mgr, err := waddrmgr.Open(ns, []byte("public"), params)
		if err != nil {
			return err
		}
		defer mgr.Close()
		birthday, verified, err := mgr.BirthdayBlock(ns)
		if err != nil || !verified || uint32(birthday.Height) >= known.Blocks[0].Height {
			return nil
		}
		// The birthday block makes PutSyncedTo insist on the hash of the
		// block before (waddrmgr/db.go PutSyncedTo): taken out for the
		// jump and put back, as the library's dropwtx does.
		if err := waddrmgr.DeleteBirthdayBlock(ns); err != nil {
			return err
		}
		for _, b := range known.Blocks {
			hash, err := chainhash.NewHashFromStr(b.Hash)
			if err != nil {
				return err
			}
			stamp := &waddrmgr.BlockStamp{Height: int32(b.Height), Hash: *hash, Timestamp: time.Unix(b.Time, 0)}
			if err := mgr.SetSyncedTo(ns, stamp); err != nil {
				return err
			}
		}
		// Verified, explicitly: an unverified birthday block makes
		// btcwallet locate it again and reset the sync state to it.
		if err := mgr.SetBirthdayBlock(ns, birthday, true); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if !applied {
		return false, nil
	}
	// Read back what the wallet will see.
	top := known.Blocks[len(known.Blocks)-1]
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespace)
		mgr, err := waddrmgr.Open(ns, []byte("public"), params)
		if err != nil {
			return err
		}
		defer mgr.Close()
		synced := mgr.SyncedTo()
		if uint32(synced.Height) != top.Height || synced.Hash.String() != top.Hash {
			return fmt.Errorf("the wallet reads block %d after the write, want %d", synced.Height, top.Height)
		}
		if _, verified, err := mgr.BirthdayBlock(ns); err != nil || !verified {
			return errors.New("the wallet's birthday block did not survive the write")
		}
		return nil
	})
	return err == nil, err
}

// proposeKnownHistory is called when the search found nothing: if the walk
// also proved that lnd's history check has nothing to find, it writes the
// order that StartNode carries out before the next start. It reports
// whether it did.
func (c *Core) proposeKnownHistory(watch *addressWatch, walkEnd, first uint32, headerAt func(uint32) (*wire.BlockHeader, error), walletTxs int) (bool, string) {
	rescan.Lock()
	covered := rescan.addresses
	rescan.Unlock()
	switch {
	case c.ownScripts == nil:
		return false, "the wallet's addresses could not be read before the start"
	case watch.ownPaid > 0:
		return false, fmt.Sprintf("%d payment(s) to the wallet's addresses are on chain", watch.ownPaid)
	case walletTxs > 0:
		return false, fmt.Sprintf("the wallet holds %d transaction(s)", walletTxs)
	case covered == 0:
		return false, "lnd has not said how many addresses its check covers"
	case covered != len(c.ownScripts):
		return false, fmt.Sprintf("lnd's check covers %d addresses, the walk watched %d", covered, len(c.ownScripts))
	}
	known, err := knownUpTo(headerAt, walkEnd, first)
	if err != nil {
		return false, err.Error()
	}
	raw, err := json.Marshal(known)
	if err != nil {
		return false, err.Error()
	}
	if err := writeFileAtomic(filepath.Join(c.dir(), historyKnownFile), raw); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// carryOutKnownHistory applies an order written by proposeKnownHistory.
// Whatever happens, the order is gone afterwards: not applying it only
// means lnd's ordinary history check runs.
func (c *Core) carryOutKnownHistory() {
	path := filepath.Join(c.dir(), historyKnownFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.Remove(path)
	var known knownHistory
	if err := json.Unmarshal(raw, &known); err != nil {
		c.progressf("The history shortcut is not used: %v.", err)
		return
	}
	applied, err := applyKnownHistory(c.walletDBPath(), c.cfg.Network, &known)
	switch {
	case err != nil:
		// A half-applied state cannot exist (one transaction), but an
		// error after it is reason enough to go the long, known way.
		c.progressf("The history shortcut failed (%v); the history is checked from the start.", err)
		if err := c.mark(historyRecheckFile); err != nil {
			c.progressf("%v", err)
		}
	case !applied:
		c.progressf("The history shortcut is not used: the app's sync state is not ready for it.")
	default:
		top := known.Blocks[len(known.Blocks)-1].Height
		c.skippedTo = top
		c.progressf("Nothing in the chain concerns this app up to block %d; its history check starts there.", top)
	}
}
