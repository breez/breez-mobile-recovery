package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/breez/breez/chainservice"
	"github.com/breez/breez/channeldbservice"
	breezdb "github.com/breez/breez/db"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	bolt "go.etcd.io/bbolt"
)

// Funds paid to the wallet after its last backup sit on addresses the
// restored wallet has not derived, and a wallet only checks addresses it
// has derived. Two cases are real: the phone swept a closed channel after
// its last backup, and an earlier restore with this tool collected funds
// and its folder is gone.
//
// Up to alpha.30 the app derived 50 more addresses per branch through the
// wallet (NextAddr). That moves the wallet's address counter, so the next
// address lnd hands out, for its sweep of a closed channel, is the one
// right past the window: exactly where a later restore of the same backup
// does not look (Roy's 864 sat, 2026-09-21). No window size cures that.
//
// So the app looks ahead WITHOUT touching the wallet: it derives the
// addresses itself from the account public keys and looks for them in the
// node's compact filters. The wallet is advanced only up to an address
// that was really paid. Two ways find one:
//
//   - following the money: the walk follows every output of a channel's
//     closing transaction through its spends, and every block it opens is
//     searched for the next addressDepth addresses of every branch. That
//     finds what a close paid out wherever the sweep went, which matters
//     because lnd takes a new address for every sweep it publishes;
//   - the gap: for funds that did not come out of a channel in the backup
//     (a plain payment to the app, the phone's sweep of a close that was
//     already under way), the next addresses of each branch are matched
//     against every block's filter from the wallet's birthday on, and the
//     search ends when the AddressGap addresses after the last paid one
//     were looked for in every block, as in any gap-limit wallet.
const (
	// AddressGap is how many unused addresses in a row end the search on a
	// branch: the gap limit wallets use.
	AddressGap = 20
	// addressWindow is how many addresses of a branch are in the filter
	// match from the first block on. Twice the gap: a find anywhere in the
	// first 20 then needs no second pass, because the 20 after it were
	// looked for all along. A pass costs gigabytes of filters (13 to 21 KB a
	// block, none kept); the scripts in the match cost next to nothing (one
	// block opened by false positive per 784,931 blocks and script).
	addressWindow = 2 * AddressGap
	// addressDepth is how far past the last watched address an output in
	// an opened block is still recognised as this wallet's.
	addressDepth = 1000
)

// addrBranch is one chain of addresses of the wallet's default account.
type addrBranch struct {
	name     string                // for the log, e.g. "taproot change"
	addrType walletrpc.AddressType // what NextAddr is asked for to advance it
	change   bool
	key      *hdkeychain.ExtendedKey // account key / branch
	script   func(*btcec.PublicKey) ([]byte, btcutil.Address, error)
	next     uint32 // the wallet's counter: first index it has not derived
	gap      bool   // matched against the filters (the others only in opened blocks)

	knownTo   uint32 // first index past the ones recognised in opened blocks
	watchedTo uint32 // first index past the ones in the filter match
	paidTo    uint32 // one past the highest paid index at or past next, 0 for none
}

func (b *addrBranch) derive(index uint32) ([]byte, btcutil.Address, error) {
	k, err := b.key.Derive(index)
	if err != nil {
		return nil, nil, err
	}
	pub, err := k.ECPubKey()
	if err != nil {
		return nil, nil, err
	}
	return b.script(pub)
}

type addrRef struct {
	branch *addrBranch
	index  uint32
}

// watchedScript is an address in the filter match. until is the first
// block that need not be looked at for it, 0 for up to the tip.
type watchedScript struct {
	script []byte
	until  uint32
}

// addressWatch is the address side of the filter walk.
type addressWatch struct {
	from     uint32 // first block to look at
	branches []*addrBranch
	known    map[string]addrRef // script -> address, addressDepth past each counter
	scripts  []watchedScript    // matched against the filters
	// added are the scripts that joined the match during the walk, each
	// with the block it joined at: the blocks before it have not been
	// checked for it yet.
	added []watchedScript
	hits  []foundOutput     // what was found
	onHit func(foundOutput) // told about each find at once, may be nil
	// followed are outputs whose spending the walk wants to see: every
	// output of a channel's closing transaction and of the transactions
	// spending those. Whatever a close pays out, to_remote, a delayed
	// to_local or an HTLC, ends in a sweep to a wallet address, and lnd
	// takes a NEW address for every sweep it publishes (sweeper.go:1715),
	// so that address can lie anywhere. The block of the sweep is opened
	// because it spends a followed output, and every opened block is
	// searched for all known addresses.
	followed map[wire.OutPoint]followedOutput
	// own are the scripts of the addresses the wallet has already, watched
	// for the history shortcut (syncskip.go); ownPaid counts the outputs
	// found paying one.
	own     map[string]bool
	ownPaid int
	// ownTxs are the transactions paying one of those addresses, with the
	// block they are in. The wallet's own history check has to find every
	// one of them; should it have derived an address only after its check
	// had passed the block paying it, it would not (see verifyFound).
	ownTxs map[string]foundOutput
}

// followDepth is how many spends past a closing transaction are followed:
// close, then a second-level HTLC transaction, then the sweep.
const followDepth = 2

type followedOutput struct {
	script []byte
	depth  int
}

// ownBranch marks, in the finds file, a payment to an address the wallet had
// already: nothing to advance for it, only to see in the wallet afterwards.
const ownBranch = "an address the app has"

// foundOutput is a payment to one of the wallet's next addresses.
type foundOutput struct {
	TxID    string `json:"txid"`
	Index   uint32 `json:"index"`
	Value   int64  `json:"value"`
	Branch  string `json:"branch"`
	Address uint32 `json:"address"`
	Height  uint32 `json:"height"`
}

func (f foundOutput) String() string {
	return fmt.Sprintf("%d sat to %s address %d in %s:%d, block %d", f.Value, f.Branch, f.Address, f.TxID, f.Index, f.Height)
}

func scriptFuncs(params *chaincfg.Params) (p2wkh, nested, p2tr func(*btcec.PublicKey) ([]byte, btcutil.Address, error)) {
	finish := func(addr btcutil.Address, err error) ([]byte, btcutil.Address, error) {
		if err != nil {
			return nil, nil, err
		}
		script, err := txscript.PayToAddrScript(addr)
		return script, addr, err
	}
	p2wkh = func(pub *btcec.PublicKey) ([]byte, btcutil.Address, error) {
		return finish(btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), params))
	}
	nested = func(pub *btcec.PublicKey) ([]byte, btcutil.Address, error) {
		inner, _, err := p2wkh(pub)
		if err != nil {
			return nil, nil, err
		}
		return finish(btcutil.NewAddressScriptHash(inner, params))
	}
	p2tr = func(pub *btcec.PublicKey) ([]byte, btcutil.Address, error) {
		return finish(btcutil.NewAddressTaproot(schnorr.SerializePubKey(txscript.ComputeTaprootKeyNoScript(pub)), params))
	}
	return
}

func netParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "simnet":
		return &chaincfg.SimNetParams, nil
	}
	return nil, fmt.Errorf("unknown network %q", network)
}

// accountBranches turns one account of the wallet into its two branches.
// The witness key hash and taproot accounts are in the gap search, the
// same four branches the look-ahead covered up to alpha.30. The nested
// account (external nested, change witness key hash) is only recognised
// in opened blocks: lnd never sweeps or sends change to it.
func accountBranches(addrType walletrpc.AddressType, xpub string, externalCount, internalCount uint32, params *chaincfg.Params) ([]*addrBranch, error) {
	p2wkh, nested, p2tr := scriptFuncs(params)
	var (
		label            string
		nextType         walletrpc.AddressType
		external, change func(*btcec.PublicKey) ([]byte, btcutil.Address, error)
		gap              bool
	)
	switch addrType {
	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		label, nextType, external, change, gap = "witness", addrType, p2wkh, p2wkh, true
	case walletrpc.AddressType_TAPROOT_PUBKEY:
		label, nextType, external, change, gap = "taproot", addrType, p2tr, p2tr, true
	case walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:
		label, nextType, external, change = "nested", walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH, nested, p2wkh
	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH:
		label, nextType, external, change = "nested", addrType, nested, nested
	default:
		return nil, nil
	}
	acct, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	var out []*addrBranch
	for _, br := range []struct {
		change bool
		script func(*btcec.PublicKey) ([]byte, btcutil.Address, error)
		next   uint32
	}{{false, external, externalCount}, {true, change, internalCount}} {
		key, err := acct.Derive(map[bool]uint32{false: 0, true: 1}[br.change])
		if err != nil {
			return nil, err
		}
		name := label + " receive"
		if br.change {
			name = label + " change"
		}
		out = append(out, &addrBranch{name: name, addrType: nextType, change: br.change, key: key,
			script: br.script, next: br.next, gap: gap})
	}
	return out, nil
}

// newAddressWatch derives the addresses to look for.
func newAddressWatch(branches []*addrBranch, from uint32) (*addressWatch, error) {
	w := &addressWatch{from: from, branches: branches, known: map[string]addrRef{}, followed: map[wire.OutPoint]followedOutput{}}
	for _, b := range branches {
		b.knownTo, b.watchedTo = b.next, b.next
		w.ensureKnown(b, b.next+addressDepth)
		if b.gap {
			w.extend(b, b.next+addressWindow, 0)
		}
	}
	w.added = nil
	return w, nil
}

// ensureKnown makes the branch's addresses below upTo recognisable.
func (w *addressWatch) ensureKnown(b *addrBranch, upTo uint32) {
	for ; b.knownTo < upTo; b.knownTo++ {
		script, _, err := b.derive(b.knownTo)
		if err != nil {
			// One index in 2^127 has no key; the wallet skips it too.
			continue
		}
		w.known[string(script)] = addrRef{b, b.knownTo}
	}
}

// extend puts the branch's addresses below upTo into the filter match,
// while the walk is at block at, and keeps addressDepth more recognisable
// past them.
func (w *addressWatch) extend(b *addrBranch, upTo, at uint32) {
	w.ensureKnown(b, upTo+addressDepth)
	for ; b.watchedTo < upTo; b.watchedTo++ {
		script, _, err := b.derive(b.watchedTo)
		if err != nil {
			continue
		}
		w.scripts = append(w.scripts, watchedScript{script: script})
		w.added = append(w.added, watchedScript{script: script, until: at})
	}
}

// follow makes the walk open the blocks that spend the transaction's
// outputs, other than those paying this wallet.
func (w *addressWatch) follow(tx *wire.MsgTx, depth int) {
	if w.followed == nil {
		return
	}
	hash := tx.TxHash()
	for i, out := range tx.TxOut {
		if _, ours := w.known[string(out.PkScript)]; ours {
			continue
		}
		w.followed[wire.OutPoint{Hash: hash, Index: uint32(i)}] = followedOutput{script: out.PkScript, depth: depth}
	}
}

// end is the first block no script needs any more, 0 when one needs the
// blocks up to the tip.
func (w *addressWatch) end() uint32 {
	if len(w.followed) > 0 || len(w.own) > 0 {
		return 0
	}
	var end uint32
	for _, ws := range w.scripts {
		if ws.until == 0 {
			return 0
		}
		if ws.until > end {
			end = ws.until
		}
	}
	return end
}

// scriptsAt are the scripts to match against the filter of block h.
func (w *addressWatch) scriptsAt(h uint32) [][]byte {
	if h < w.from {
		return nil
	}
	var out [][]byte
	for _, ws := range w.scripts {
		if ws.until == 0 || h < ws.until {
			out = append(out, ws.script)
		}
	}
	for _, f := range w.followed {
		out = append(out, f.script)
	}
	for script := range w.own {
		out = append(out, []byte(script))
	}
	return out
}

// applyBlock records every output of the block that pays one of the
// wallet's next addresses, and moves on along the followed outputs.
func (w *addressWatch) applyBlock(block *wire.MsgBlock, height uint32) {
	// A followed output spent here: follow the spender's outputs in turn.
	// Repeated, because a spender can itself be spent in the same block.
	for changed := true; changed && len(w.followed) > 0; {
		changed = false
		for _, tx := range block.Transactions {
			for _, in := range tx.TxIn {
				f, ok := w.followed[in.PreviousOutPoint]
				if !ok {
					continue
				}
				delete(w.followed, in.PreviousOutPoint)
				if f.depth+1 < followDepth {
					w.follow(tx, f.depth+1)
					changed = true
				}
			}
		}
	}
	for _, tx := range block.Transactions {
		var txid string
		for i, out := range tx.TxOut {
			if w.own[string(out.PkScript)] {
				w.ownPaid++
				if w.ownTxs == nil {
					w.ownTxs = map[string]foundOutput{}
				}
				id := tx.TxHash().String()
				if _, seen := w.ownTxs[id]; !seen {
					w.ownTxs[id] = foundOutput{TxID: id, Index: uint32(i), Value: out.Value, Branch: ownBranch, Height: height}
				}
			}
			ref, ok := w.known[string(out.PkScript)]
			if !ok {
				continue
			}
			if txid == "" {
				txid = tx.TxHash().String()
			}
			b := ref.branch
			hit := foundOutput{TxID: txid, Index: uint32(i), Value: out.Value, Branch: b.name, Address: ref.index, Height: height}
			isNew := true
			for _, have := range w.hits {
				if have.TxID == hit.TxID && have.Index == hit.Index {
					isNew = false
				}
			}
			if isNew {
				w.hits = append(w.hits, hit)
				if w.onHit != nil {
					w.onHit(hit)
				}
			}
			if ref.index+1 > b.paidTo {
				b.paidTo = ref.index + 1
			}
			if b.gap {
				w.extend(b, ref.index+1+AddressGap, height)
			} else {
				w.ensureKnown(b, ref.index+1+addressDepth)
			}
		}
	}
}

// rest is the watch for what the walk could not cover: an address that
// joined the match at some block was looked for from there to the tip
// (every block the walk opens is searched for all known addresses, so the
// block it joined at is covered too), but not in the blocks before. Nil
// when nothing is left.
func (w *addressWatch) rest() *addressWatch {
	var left []watchedScript
	for _, ws := range w.added {
		if ws.until > w.from {
			left = append(left, ws)
		}
	}
	if len(left) == 0 {
		return nil
	}
	return &addressWatch{from: w.from, branches: w.branches, known: w.known, scripts: left, hits: w.hits, onHit: w.onHit}
}

// walletBirthday reads the creation time of the wallet from its file. It
// must be called while lnd does not run: lnd holds the file's lock.
// Nothing was ever paid to the wallet before it. (The backup's own sync
// height would be a later and better start, but Breez backups carry none:
// the app reset it to the genesis block.)
func walletBirthday(walletDB string) (time.Time, error) {
	db, err := bolt.Open(walletDB, 0600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return time.Time{}, fmt.Errorf("open the wallet file: %w", err)
	}
	defer db.Close()
	var birthday time.Time
	err = db.View(func(tx *bolt.Tx) error {
		mgr := tx.Bucket([]byte("waddrmgr"))
		if mgr == nil {
			return errors.New("the wallet file has no address manager")
		}
		sync := mgr.Bucket([]byte("sync"))
		if sync == nil {
			return errors.New("the wallet file has no sync state")
		}
		v := sync.Get([]byte("birthday"))
		if len(v) != 8 {
			return errors.New("the wallet file has no birthday")
		}
		birthday = time.Unix(int64(binary.BigEndian.Uint64(v)), 0)
		return nil
	})
	return birthday, err
}

// walkStart returns the block to start looking for payments to a wallet
// created at birthday, and the first block the node has.
//
// The library does not keep the whole chain (breez chainservice/bootstrap.go):
// it starts the header files at the latest of its built-in checkpoints (every
// 1,000 blocks and every difficulty period) whose block time is not after
// the wallet's birthday, and leaves them empty below, but for the earlier
// checkpoints themselves. An empty header has
// no usable hash, so nothing below can be asked for, and the first block's
// own filter cannot be had either: a filter is verified against the filter
// header of the block before it. The walk therefore starts two days before
// the birthday (block times run up to two hours off and are not strictly
// increasing) but never below the block after the first one; the caller
// looks into the first block itself when the start was cut short.
//
// headerAt reads a header by height, which also works where the file is
// empty; asking neutrino by the hash of an empty header fails with "target
// hash not found in index".
func walkStart(headerAt func(height uint32) (*wire.BlockHeader, error), birthday time.Time, tip uint32) (from, first uint32, err error) {
	var searchErr error
	header := func(h int) *wire.BlockHeader {
		hd, err := headerAt(uint32(h))
		if err != nil {
			searchErr = err
			return &wire.BlockHeader{}
		}
		return hd
	}
	n := sort.Search(int(tip)+1, func(h int) bool { return !header(h).Timestamp.Before(birthday) })
	f, err := firstBlock(headerAt, tip)
	if err != nil {
		return 0, 0, err
	}
	if searchErr != nil {
		return 0, 0, fmt.Errorf("find the block of %s: %w", birthday.Format("2006-01-02"), searchErr)
	}
	const margin = 2 * 144
	start := uint32(1)
	if n > margin {
		start = uint32(n - margin)
	}
	if start < f+1 {
		start = f + 1
	}
	return start, f, nil
}

// firstBlock is the first block of the chain the node has. Below its start
// the library also writes each earlier checkpoint into the otherwise empty
// header file, as lone headers: the chain begins at the first header whose
// successor is there too (no two checkpoints are neighbours: multiples of
// 1,000 and of 2,016 never differ by one).
func firstBlock(headerAt func(height uint32) (*wire.BlockHeader, error), tip uint32) (uint32, error) {
	var searchErr error
	real := func(h int) bool {
		hd, err := headerAt(uint32(h))
		if err != nil {
			searchErr = err
			return true
		}
		return hd.Timestamp.Unix() > 0
	}
	f := 1 + sort.Search(int(tip)-1, func(i int) bool { return real(i+1) && real(i+2) })
	if searchErr != nil {
		return 0, fmt.Errorf("find the first block the node has: %w", searchErr)
	}
	return uint32(f), nil
}

const (
	// walletBirthdayFile keeps the wallet's creation time, read before the
	// node's first start.
	walletBirthdayFile = "wallet-birthday"
)

// walletBirthdayTime reads the wallet's creation time kept by StartNode.
func (c *Core) walletBirthdayTime() (time.Time, error) {
	raw, err := os.ReadFile(filepath.Join(c.dir(), walletBirthdayFile))
	if err != nil {
		return time.Time{}, fmt.Errorf("read the wallet's creation time: %w", err)
	}
	birthday, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, fmt.Errorf("read the wallet's creation time: %w", err)
	}
	return birthday, nil
}

func (c *Core) walletDBPath() string {
	return filepath.Join(c.dir(), "data", "chain", "bitcoin", c.cfg.Network, "wallet.db")
}

// walletBranches asks the wallet for its accounts and checks, where it
// can, that the addresses derived here are the wallet's own: the last
// address the wallet derived on a branch must be the one this code derives
// at that index. A mismatch is an error, never a search that finds nothing.
func (n *node) walletBranches(ctx context.Context, network string) ([]*addrBranch, error) {
	params, err := netParams(network)
	if err != nil {
		return nil, err
	}
	wk := walletrpc.NewWalletKitClient(n.conn)
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var accounts *walletrpc.ListAccountsResponse
	err = whileAvailable(ctx, func() (err error) {
		accounts, err = wk.ListAccounts(c, &walletrpc.ListAccountsRequest{Name: "default"})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list the wallet's accounts: %w", err)
	}
	var branches []*addrBranch
	for _, a := range accounts.Accounts {
		if a.ExtendedPublicKey == "" {
			continue
		}
		brs, err := accountBranches(a.AddressType, a.ExtendedPublicKey, a.ExternalKeyCount, a.InternalKeyCount, params)
		if err != nil {
			return nil, err
		}
		for _, b := range brs {
			b.name += " (" + a.DerivationPath + ")"
		}
		branches = append(branches, brs...)
	}
	if len(branches) == 0 {
		return nil, errors.New("the wallet lists no accounts")
	}

	var listed *walletrpc.ListAddressesResponse
	err = whileAvailable(ctx, func() (err error) {
		listed, err = wk.ListAddresses(c, &walletrpc.ListAddressesRequest{AccountName: "default"})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list the wallet's addresses: %w", err)
	}
	own := map[string]bool{}
	for _, acct := range listed.AccountWithAddresses {
		for _, a := range acct.Addresses {
			own[a.Address] = true
		}
	}
	checked := 0
	for _, b := range branches {
		if b.next == 0 {
			continue
		}
		_, addr, err := b.derive(b.next - 1)
		if err != nil {
			continue
		}
		if !own[addr.String()] {
			return nil, fmt.Errorf("address %d of %s derived as %s, which the wallet does not list", b.next-1, b.name, addr)
		}
		checked++
	}
	// A wallet that never derived an address (an app that only ever used
	// channels the LSP opened) has nothing to compare with; the derivation
	// itself is pinned to the BIP 84, 86 and 49 test vectors in the tests.
	nodeLog(fmt.Sprintf("[addresses] %d branch(es), derivation checked against the wallet on %d", len(branches), checked))
	return branches, nil
}

// advanceTo derives addresses through the wallet until the branch's counter
// is upTo, and makes sure the wallet arrived where this code expects.
func (n *node) advanceTo(ctx context.Context, b *addrBranch, upTo uint32) error {
	wk := walletrpc.NewWalletKitClient(n.conn)
	var last string
	for i := b.next; i < upTo; i++ {
		// Not run again on a failure: an address the node did hand out
		// although the answer got lost would be handed out twice over,
		// and the check below would (rightly) fail.
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := wk.NextAddr(c, &walletrpc.AddrRequest{Type: b.addrType, Change: b.change})
		cancel()
		if err != nil {
			return fmt.Errorf("derive address: %w", err)
		}
		last = res.Addr
	}
	_, want, err := b.derive(upTo - 1)
	if err != nil {
		return err
	}
	if !strings.EqualFold(last, want.String()) {
		return fmt.Errorf("the wallet's address %d of %s is %s, expected %s", upTo-1, b.name, last, want)
	}
	b.next = upTo
	return nil
}

// findLaterFunds searches the chain for funds paid to the wallet after its
// backup (see the top of this file), in the same walk that checks the
// channels. When it finds some it moves the wallet up to the paid
// addresses, orders a fresh history check and returns ErrRestartRequired.
func (c *Core) findLaterFunds(ctx context.Context, report func(SyncProgress)) error {
	birthday, err := c.walletBirthdayTime()
	if err != nil {
		return err
	}
	// The search must reach the real chain tip: wait until this process
	// has seen the headers catch up (the sync stage can be entered on a
	// clock estimate before that).
	if err := waitHeadersSynced(ctx, 15*time.Minute); err != nil {
		return err
	}

	chandb, releaseDB, err := channeldbservice.Get(c.dir())
	if err != nil {
		return fmt.Errorf("open the channel database: %w", err)
	}
	defer releaseDB()
	bdb, releaseBreezDB, err := breezdb.Get(c.dir())
	if err != nil {
		return fmt.Errorf("open the app database: %w", err)
	}
	defer releaseBreezDB()
	chain, releaseChain, err := chainservice.Get(c.dir(), bdb)
	if err != nil {
		return fmt.Errorf("reach the bitcoin network: %w", err)
	}
	defer releaseChain()

	best, err := chain.BestBlock()
	if err != nil {
		return fmt.Errorf("read the chain tip: %w", err)
	}
	tip := uint32(best.Height)
	from, first, err := walkStart(chain.BlockHeaders.FetchHeaderByHeight, birthday, tip)
	if err != nil {
		return err
	}
	if from > tip {
		return fmt.Errorf("the chain the node has ends at block %d, before the search could start (block %d)", tip, from)
	}
	branches, err := c.node.walletBranches(ctx, c.cfg.Network)
	if err != nil {
		return err
	}
	watch, err := newAddressWatch(branches, from)
	if err != nil {
		return err
	}
	watch.onHit = func(hit foundOutput) { c.progressf("  Found %s.", hit) }
	if c.ownScripts != nil {
		watch.own = map[string]bool{}
		for _, script := range c.ownScripts {
			watch.own[string(script)] = true
		}
	}
	stored, err := chandb.ChannelStateDB().FetchAllOpenChannels()
	if err != nil {
		return err
	}
	var fundings []channelFunding
	wk := walletrpc.NewWalletKitClient(c.node.conn)
	for _, ch := range stored {
		// Another node's channel pays nothing to this wallet.
		foreign, err := isForeign(ctx, wk, ch)
		if err != nil {
			return err
		}
		if foreign {
			continue
		}
		f, err := fundingOf(ch)
		if err != nil {
			return err
		}
		fundings = append(fundings, f)
	}

	const message = "Looking for funds received after the last backup"
	c.progressf("%s, from block %d (the app was set up on %s)...", message, from, birthday.Format("2 Jan 2006"))
	// Nothing else talks to the node during the walk, and its connection
	// does not take a long silence well (see whileAvailable): keep it busy.
	walking, walked := context.WithCancel(ctx)
	defer walked()
	go func() {
		for {
			select {
			case <-walking.Done():
				return
			case <-time.After(20 * time.Second):
				c.node.info(walking)
			}
		}
	}()
	walk := newChainWalk(fundings, watch, first+1)
	if err := walk.run(ctx, chain, walkProgress("addresses", message, report)); err != nil {
		return err
	}
	hits := watch.hits
	for rest := watch.rest(); rest != nil; rest = rest.rest() {
		c.progressf("Looking at the addresses after the ones found...")
		if err := newChainWalk(nil, rest, first+1).run(ctx, chain, walkProgress("addresses", message, report)); err != nil {
			return err
		}
		hits = rest.hits
	}
	walked()
	// The channel check carries on from here, without the addresses, in
	// this run or after the restart.
	walk.watch = nil
	c.walk = walk
	if err := walk.save(c.dir(), chain, fundings); err != nil {
		return fmt.Errorf("save the chain check: %w", err)
	}

	// Kept on file: once the history is checked, the wallet must show every
	// find, and every payment to an address it had already (see
	// verifyFound).
	expected := append([]foundOutput{}, hits...)
	for _, own := range watch.ownTxs {
		expected = append(expected, own)
	}
	if len(expected) > 0 {
		sort.Slice(expected, func(i, j int) bool {
			if expected[i].Height != expected[j].Height {
				return expected[i].Height < expected[j].Height
			}
			return expected[i].TxID < expected[j].TxID
		})
		raw, err := json.MarshalIndent(expected, "", " ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(c.dir(), foundFundsFile), raw); err != nil {
			return err
		}
	}
	if len(hits) == 0 {
		c.progressf("None found.")
		if err := c.mark(addressesExtendedFile); err != nil {
			return err
		}
		// Did the walk also prove that lnd's own history check has nothing
		// to find? Then the wallet is told so before the next start.
		known, err := c.node.walletTxIDs(ctx)
		if err != nil {
			return err
		}
		ok, why := c.proposeKnownHistory(watch, walk.next-1, first, chain.BlockHeaders.FetchHeaderByHeight, len(known))
		if !ok {
			c.progressf("The app's history is checked in full (%s).", why)
			return nil
		}
		c.progressf("Nothing in the chain concerns this app's addresses. Stopping the node; the app restarts past the history check...")
		if !c.StopWithin(20 * time.Second) {
			c.progressf("The node did not stop cleanly; the program exits and starts again.")
		}
		return ErrRestartRequired
	}
	// lnd itself may have handed out an address while the walk ran (its
	// own sweep of a closed channel) and then knows the payment already.
	// Only a payment the wallet does not know needs the history again.
	known, err := c.node.walletTxIDs(ctx)
	if err != nil {
		return err
	}
	unknown := 0
	for _, hit := range hits {
		if !known[hit.TxID] {
			unknown++
		}
	}
	if unknown == 0 {
		c.progressf("The app already knows these payments.")
		return c.mark(addressesExtendedFile)
	}

	// Ordered before the wallet is touched: if the app dies half way, the
	// next start still checks the history again, with whatever addresses
	// were derived, and this search runs again for the rest.
	if err := c.mark(historyRecheckFile); err != nil {
		return err
	}
	// lnd may have handed out addresses of its own since the walk began.
	current, err := c.node.walletBranches(ctx, c.cfg.Network)
	if err != nil {
		return err
	}
	for _, b := range branches {
		for _, now := range current {
			if now.name == b.name && now.next > b.next {
				b.next = now.next
			}
		}
		if b.paidTo <= b.next {
			continue
		}
		c.progressf("Preparing %d more address(es) of %s...", b.paidTo-b.next, b.name)
		if err := c.node.advanceTo(ctx, b, b.paidTo); err != nil {
			return err
		}
	}
	if err := c.mark(addressesExtendedFile); err != nil {
		return err
	}
	c.progressf("Stopping the node; the app restarts to check the history with these addresses...")
	if !c.StopWithin(20 * time.Second) {
		c.progressf("The node did not stop cleanly; the program exits and starts again.")
	}
	return ErrRestartRequired
}

// foundFundsFile lists the payments the search found, until the wallet has
// been seen to show them.
const foundFundsFile = "found-funds.json"

// verifyFound makes sure, once the history is checked, that the wallet
// shows every payment the walk saw: the finds on addresses past the
// wallet's counter, and the payments to addresses it had already. A find
// that never reaches the wallet would otherwise end as a silent "0 sat". It
// can happen without any fault of the wallet: lnd hands out an address for
// a sweep of its own while its history check is already past the block
// that pays this very address (an earlier restore's sweep, the phone's),
// and the search, when it has to run again after an interruption, takes the
// address for one the wallet has. A missing payment orders the full history
// check once; should it still be missing after that, the sync stops with an
// error for a find, and says so in the log for the others.
func (c *Core) verifyFound(ctx context.Context) error {
	path := filepath.Join(c.dir(), foundFundsFile)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var expected []foundOutput
	if err := json.Unmarshal(raw, &expected); err != nil {
		return fmt.Errorf("read %s: %w", foundFundsFile, err)
	}
	known, err := c.node.walletTxIDs(ctx)
	if err != nil {
		return err
	}
	var missingFinds, missingOwn []string
	finds := 0
	for _, e := range expected {
		if e.Branch != ownBranch {
			finds++
		}
		switch {
		case known[e.TxID]:
		case e.Branch == ownBranch:
			missingOwn = append(missingOwn, e.String())
		default:
			missingFinds = append(missingFinds, e.String())
		}
	}
	if len(missingFinds)+len(missingOwn) == 0 {
		if finds > 0 {
			c.progressf("The app shows the %d payment(s) found after the backup.", finds)
		}
		return os.Rename(path, path+".verified")
	}
	if !c.marked(recheckTriedFile) {
		c.progressf("%d payment(s) on chain are not in the app yet; its history is checked again from the start.", len(missingFinds)+len(missingOwn))
		if err := c.mark(recheckTriedFile); err != nil {
			return err
		}
		if err := c.mark(historyRecheckFile); err != nil {
			return err
		}
		if !c.StopWithin(20 * time.Second) {
			c.progressf("The node did not stop cleanly; the program exits and starts again.")
		}
		return ErrRestartRequired
	}
	for _, m := range missingOwn {
		c.progressf("Note: on chain but not in the app after a full history check: %s.", m)
	}
	if len(missingFinds) > 0 {
		return fmt.Errorf("funds found on chain are not in the app after a full history check: %s. Save the log and send it to Breez support", strings.Join(missingFinds, "; "))
	}
	return os.Rename(path, path+".verified")
}

// recheckTriedFile marks that verifyFound ordered its one full history
// check already.
const recheckTriedFile = "history-recheck-tried"
