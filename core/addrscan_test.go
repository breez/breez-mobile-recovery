package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// The account keys and addresses of the test vectors in BIP 84, 86 and 49.
const (
	bip84Account = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
	bip86Account = "xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ"
	bip49Account = "ypub6Ww3ibxVfGzLrAH1PNcjyAWenMTbbAosGNB6VvmSEgytSER9azLDWCxoJwW7Ke7icmizBMXrzBx9979FfaHxHcrArf3zbeJJJUZPf663zsP"
)

func TestAddressDerivation(t *testing.T) {
	cases := []struct {
		addrType walletrpc.AddressType
		xpub     string
		change   bool
		index    uint32
		want     string
	}{
		{walletrpc.AddressType_WITNESS_PUBKEY_HASH, bip84Account, false, 0, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"},
		{walletrpc.AddressType_WITNESS_PUBKEY_HASH, bip84Account, false, 1, "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g"},
		{walletrpc.AddressType_WITNESS_PUBKEY_HASH, bip84Account, true, 0, "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el"},
		{walletrpc.AddressType_TAPROOT_PUBKEY, bip86Account, false, 0, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"},
		{walletrpc.AddressType_TAPROOT_PUBKEY, bip86Account, false, 1, "bc1p4qhjn9zdvkux4e44uhx8tc55attvtyu358kutcqkudyccelu0was9fqzwh"},
		{walletrpc.AddressType_TAPROOT_PUBKEY, bip86Account, true, 0, "bc1p3qkhfews2uk44qtvauqyr2ttdsw7svhkl9nkm9s9c3x4ax5h60wqwruhk7"},
		{walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH, bip49Account, false, 0, "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf"},
	}
	for _, tc := range cases {
		branches, err := accountBranches(tc.addrType, tc.xpub, 0, 0, &chaincfg.MainNetParams)
		if err != nil {
			t.Fatal(err)
		}
		b := branches[0]
		if tc.change {
			b = branches[1]
		}
		_, addr, err := b.derive(tc.index)
		if err != nil {
			t.Fatal(err)
		}
		if addr.String() != tc.want {
			t.Errorf("%s %d: derived %s, want %s", b.name, tc.index, addr, tc.want)
		}
	}
}

// memChain serves blocks with real compact filters.
type memChain struct {
	first  uint32
	blocks []*wire.MsgBlock
	prev   map[wire.OutPoint][]byte // scripts of the outputs the blocks spend
	opened int
	// hole, when set, is the first block the node really has, as in
	// production: below it every height answers with the hash of an empty
	// header, which nothing can be fetched for, and the block at hole has
	// no fetchable filter (it would be verified against the one before).
	hole uint32
}

func (m *memChain) add(txs ...*wire.MsgTx) uint32 {
	block := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: uint32(len(m.blocks)), Timestamp: time.Unix(1600000000+int64(len(m.blocks))*600, 0)}, Transactions: txs}
	m.blocks = append(m.blocks, block)
	for _, tx := range txs {
		hash := tx.TxHash()
		for i, out := range tx.TxOut {
			m.prev[wire.OutPoint{Hash: hash, Index: uint32(i)}] = out.PkScript
		}
	}
	return m.first + uint32(len(m.blocks)) - 1
}

func (m *memChain) at(hash chainhash.Hash) *wire.MsgBlock {
	for _, b := range m.blocks {
		if b.BlockHash() == hash {
			return b
		}
	}
	return nil
}

func (m *memChain) BestBlock() (*headerfs.BlockStamp, error) {
	return &headerfs.BlockStamp{Height: int32(m.first) + int32(len(m.blocks)) - 1}, nil
}

func (m *memChain) GetBlockHash(height int64) (*chainhash.Hash, error) {
	if m.hole != 0 && uint32(height) < m.hole {
		empty := (&wire.BlockHeader{}).BlockHash()
		return &empty, nil
	}
	i := int(height) - int(m.first)
	if i < 0 || i >= len(m.blocks) {
		return nil, errors.New("no such block")
	}
	hash := m.blocks[i].BlockHash()
	return &hash, nil
}

func (m *memChain) headerAt(height uint32) (*wire.BlockHeader, error) {
	i := int(height) - int(m.first)
	if i < 0 || i >= len(m.blocks) {
		return nil, errors.New("no such block")
	}
	return &m.blocks[i].Header, nil
}

func (m *memChain) GetCFilter(hash chainhash.Hash, _ wire.FilterType, _ ...neutrino.QueryOption) (*gcs.Filter, error) {
	block := m.at(hash)
	if block == nil {
		return nil, errors.New("target hash not found in index")
	}
	if m.hole != 0 && hash == m.blocks[m.hole-m.first].BlockHash() {
		return nil, errors.New("got entire filter, but job was not finished")
	}
	var prevScripts [][]byte
	for _, tx := range block.Transactions {
		for _, in := range tx.TxIn {
			if script, ok := m.prev[in.PreviousOutPoint]; ok {
				prevScripts = append(prevScripts, script)
			}
		}
	}
	return builder.BuildBasicFilter(block, prevScripts)
}

func (m *memChain) GetBlock(hash chainhash.Hash, _ ...neutrino.QueryOption) (*btcutil.Block, error) {
	block := m.at(hash)
	if block == nil {
		return nil, errors.New("no such block")
	}
	m.opened++
	return btcutil.NewBlock(block), nil
}

func pay(script []byte, value int64, from ...wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	if len(from) == 0 {
		from = []wire.OutPoint{{Index: uint32(value)}}
	}
	for _, op := range from {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	}
	tx.AddTxOut(&wire.TxOut{Value: value, PkScript: script})
	return tx
}

// testBranches is a wallet whose counters stand at 7 (witness) and 3
// (taproot) on every branch.
func testBranches(t *testing.T) (witness, taproot []*addrBranch, all []*addrBranch) {
	t.Helper()
	witness, err := accountBranches(walletrpc.AddressType_WITNESS_PUBKEY_HASH, bip84Account, 7, 7, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	taproot, err = accountBranches(walletrpc.AddressType_TAPROOT_PUBKEY, bip86Account, 3, 3, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return witness, taproot, append(append([]*addrBranch{}, witness...), taproot...)
}

func scriptAt(t *testing.T, b *addrBranch, index uint32) []byte {
	t.Helper()
	script, _, err := b.derive(index)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func runSearch(t *testing.T, chain *memChain, fundings []channelFunding, branches []*addrBranch) *chainWalk {
	t.Helper()
	watch, err := newAddressWatch(branches, chain.first)
	if err != nil {
		t.Fatal(err)
	}
	walk := newChainWalk(fundings, watch, 0)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	for rest := watch.rest(); rest != nil; rest = rest.rest() {
		if err := newChainWalk(nil, rest, 0).run(context.Background(), chain, nil); err != nil {
			t.Fatal(err)
		}
	}
	return walk
}

func TestAddressSearch(t *testing.T) {
	other := []byte{0x00, 0x14, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

	t.Run("nothing paid", func(t *testing.T) {
		_, _, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		for i := 0; i < 5; i++ {
			chain.add(pay(other, int64(1000+i)))
		}
		walk := runSearch(t, chain, nil, all)
		if got := len(walk.watch.scripts); got != 4*addressWindow {
			t.Errorf("%d scripts in the match, want %d", got, 4*addressWindow)
		}
		for _, b := range all {
			if b.paidTo != 0 {
				t.Errorf("%s: paid up to %d, want nothing", b.name, b.paidTo)
			}
		}
		if chain.opened != 0 {
			t.Errorf("%d blocks opened, want 0", chain.opened)
		}
	})

	// A find within the first gap: the addresses after it were in the match
	// all along, no second pass.
	t.Run("a find inside the first gap", func(t *testing.T) {
		_, taproot, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		chain.add(pay(other, 1))
		chain.add(pay(scriptAt(t, taproot[0], 3+AddressGap-1), 864))
		chain.add(pay(other, 2))
		walk := runSearch(t, chain, nil, all)
		if taproot[0].paidTo != 3+AddressGap {
			t.Errorf("paid up to %d, want %d", taproot[0].paidTo, 3+AddressGap)
		}
		if walk.watch.rest() != nil {
			t.Error("a second pass is asked for")
		}
		if taproot[1].paidTo != 0 {
			t.Errorf("the change branch counts as paid up to %d", taproot[1].paidTo)
		}
		if len(walk.watch.hits) != 1 || walk.watch.hits[0].Value != 864 || walk.watch.hits[0].Address != 3+AddressGap-1 {
			t.Errorf("finds %+v", walk.watch.hits)
		}
	})

	t.Run("one past the window is not found", func(t *testing.T) {
		witness, _, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		chain.add(pay(scriptAt(t, witness[1], 7+addressWindow), 500))
		runSearch(t, chain, nil, all)
		if witness[1].paidTo != 0 {
			t.Errorf("paid up to %d, want nothing", witness[1].paidTo)
		}
	})

	t.Run("an address the wallet already has is the wallet's business", func(t *testing.T) {
		witness, _, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		chain.add(pay(scriptAt(t, witness[0], 6), 500))
		runSearch(t, chain, nil, all)
		if witness[0].paidTo != 0 {
			t.Errorf("paid up to %d, want nothing", witness[0].paidTo)
		}
	})

	// A later address paid in an EARLIER block than the one that brings it
	// into the match: only a second pass finds it.
	t.Run("paid out of order", func(t *testing.T) {
		witness, _, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		later, earlier := uint32(7+addressWindow+5), uint32(7+addressWindow-5)
		chain.add(pay(scriptAt(t, witness[0], later), 300))
		chain.add(pay(other, 1))
		chain.add(pay(scriptAt(t, witness[0], earlier), 200))
		runSearch(t, chain, nil, all)
		if witness[0].paidTo != later+1 {
			t.Errorf("paid up to %d, want %d", witness[0].paidTo, later+1)
		}
	})

	// A channel the peer closed, swept to an address far past the gap: the
	// walk opens the sweep's block because it watches the close's output,
	// and recognises the address there.
	t.Run("following a closed channel's funds", func(t *testing.T) {
		_, taproot, all := testBranches(t)
		chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
		fundingScript := []byte{0x00, 0x20, 0xaa}
		toUs := []byte{0x00, 0x14, 0xbb}
		funding := pay(fundingScript, 100000)
		fundingPoint := wire.OutPoint{Hash: funding.TxHash()}
		fundedAt := chain.add(funding)
		chain.add(pay(other, 1))
		closing := pay(toUs, 999, fundingPoint)
		chain.add(closing)
		chain.add(pay(other, 2))
		sweep := pay(scriptAt(t, taproot[0], 3+500), 864, wire.OutPoint{Hash: closing.TxHash()})
		chain.add(sweep)

		fundings := []channelFunding{{chanPoint: "aa:0", outpoint: fundingPoint, pkScript: fundingScript, heightHint: fundedAt, toUsScript: toUs}}
		walk := runSearch(t, chain, fundings, all)
		if taproot[0].paidTo != 3+501 {
			t.Errorf("paid up to %d, want %d", taproot[0].paidTo, 3+501)
		}
		v := walk.verdicts()["aa:0"]
		if v.verdict != verdictSpent || v.spent.Collect != 0 {
			t.Errorf("verdict %q collect %d, want spent and nothing to collect", v.verdict, v.spent.Collect)
		}
	})
}

// Addresses that join the match at some block only need the blocks before
// it: the second pass must not walk the whole chain again.
func TestSecondPassStopsWhereTheAddressJoined(t *testing.T) {
	other := []byte{0x00, 0x14, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	_, taproot, all := testBranches(t)
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	chain.add(pay(other, 1))
	chain.add(pay(other, 2))
	hitAt := chain.add(pay(scriptAt(t, taproot[0], 3+addressWindow-5), 864))
	for i := 0; i < 20; i++ {
		chain.add(pay(other, int64(10+i)))
	}
	watch, err := newAddressWatch(all, chain.first)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = chain.BestBlock()
	if err := newChainWalk(nil, watch, 0).run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	rest := watch.rest()
	if rest == nil {
		t.Fatal("no second pass although addresses joined the match")
	}
	var walked []uint32
	var target uint32
	if err := newChainWalk(nil, rest, 0).run(context.Background(), chain, func(h, _, to uint32) {
		walked, target = append(walked, h), to
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 2 || walked[0] != chain.first || walked[1] != hitAt-1 {
		t.Errorf("second pass walked %v, want blocks %d and %d", walked, chain.first, hitAt-1)
	}
	if target != hitAt-1 {
		t.Errorf("second pass reports %d as its last block, want %d", target, hitAt-1)
	}
	if rest.rest() != nil {
		t.Error("a third pass although nothing joined in the second")
	}
}

// The channel check carries on a walk from the block it ended at.
func TestWalkCarriesOn(t *testing.T) {
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	fundingScript := []byte{0x00, 0x20, 0xaa}
	toUs := []byte{0x00, 0x14, 0xbb}
	funding := pay(fundingScript, 100000)
	fundingPoint := wire.OutPoint{Hash: funding.TxHash()}
	fundedAt := chain.add(funding)
	closing := pay(toUs, 999, fundingPoint)
	chain.add(closing)

	walk := newChainWalk([]channelFunding{{chanPoint: "aa:0", outpoint: fundingPoint, pkScript: fundingScript, heightHint: fundedAt, toUsScript: toUs}}, nil, 0)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if v := walk.verdicts()["aa:0"]; v.verdict != verdictSpent || v.spent.Collect != 999 {
		t.Fatalf("verdict %q collect %d, want spent with 999 to collect", v.verdict, v.spent.Collect)
	}
	opened := chain.opened

	chain.add(pay([]byte{0x51}, 864, wire.OutPoint{Hash: closing.TxHash()}))
	var walked []uint32
	if err := walk.run(context.Background(), chain, func(h, _, _ uint32) { walked = append(walked, h) }); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || walked[0] != fundedAt+2 {
		t.Errorf("second run walked %v, want only block %d", walked, fundedAt+2)
	}
	if chain.opened != opened+1 {
		t.Errorf("second run opened %d blocks, want 1", chain.opened-opened)
	}
	if v := walk.verdicts()["aa:0"]; v.spent.Collect != 0 {
		t.Errorf("collect %d after the sweep, want 0", v.spent.Collect)
	}
}

// Whatever a close pays out is followed to the wallet: a delayed output of
// the app's own commitment, and an HTLC through its second-level
// transaction, each swept to an address far past any window.
func TestFollowingEveryOutputOfAClose(t *testing.T) {
	_, taproot, all := testBranches(t)
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	other := []byte{0x00, 0x14, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}
	fundingScript := []byte{0x00, 0x20, 0xaa}
	funding := pay(fundingScript, 100000)
	point := wire.OutPoint{Hash: funding.TxHash()}
	fundedAt := chain.add(funding)

	// The close: a delayed output and an HTLC output, neither derivable
	// from the backup, and nothing to toUsScript.
	closing := wire.NewMsgTx(2)
	closing.AddTxIn(&wire.TxIn{PreviousOutPoint: point})
	closing.AddTxOut(&wire.TxOut{Value: 5000, PkScript: []byte{0x00, 0x20, 0x01}}) // delayed
	closing.AddTxOut(&wire.TxOut{Value: 3000, PkScript: []byte{0x00, 0x20, 0x02}}) // htlc
	chain.add(closing)
	chain.add(pay(other, 1))
	closeID := closing.TxHash()
	chain.add(pay(scriptAt(t, taproot[0], 3+700), 4800, wire.OutPoint{Hash: closeID, Index: 0}))
	secondLevel := pay([]byte{0x00, 0x20, 0x03}, 2900, wire.OutPoint{Hash: closeID, Index: 1})
	chain.add(secondLevel)
	chain.add(pay(other, 2))
	chain.add(pay(scriptAt(t, taproot[0], 3+701), 2700, wire.OutPoint{Hash: secondLevel.TxHash()}))

	fundings := []channelFunding{{chanPoint: "aa:0", outpoint: point, pkScript: fundingScript, heightHint: fundedAt}}
	walk := runSearch(t, chain, fundings, all)
	if taproot[0].paidTo != 3+702 {
		t.Errorf("paid up to %d, want %d", taproot[0].paidTo, 3+702)
	}
	if len(walk.watch.hits) != 2 {
		t.Errorf("finds %+v, want the two sweeps", walk.watch.hits)
	}
	if len(walk.watch.followed) != 0 {
		t.Errorf("%d outputs still followed, want none", len(walk.watch.followed))
	}
}

// The node has no chain below its first block. A channel of another node
// can be older than that: the walk must not ask for what is not there, the
// channel must not count as open, and its close must still be seen. The
// first block itself has no filter and is looked into directly.
func TestWalkAboveTheNodesFirstBlock(t *testing.T) {
	witness, _, all := testBranches(t)
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	fundingScript := []byte{0x00, 0x20, 0xaa}
	funding := pay(fundingScript, 100000)
	point := wire.OutPoint{Hash: funding.TxHash()}
	chain.add(funding)                                     // 700000, below the hole
	chain.add(pay([]byte{0x51}, 1))                        // 700001
	first := chain.add(pay(scriptAt(t, witness[0], 7), 1)) // 700002: the node's first block, pays the wallet
	chain.add(pay([]byte{0x51}, 2))
	closedAt := chain.add(pay([]byte{0x51}, 90000, point))
	chain.add(pay([]byte{0x51}, 3))
	chain.hole = first

	watch, err := newAddressWatch(all, first+1)
	if err != nil {
		t.Fatal(err)
	}
	fundings := []channelFunding{
		{chanPoint: "old:0", outpoint: point, pkScript: fundingScript, heightHint: 700000},
		{chanPoint: "none:0", heightHint: 0},
	}
	walk := newChainWalk(fundings, watch, first+1)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if witness[0].paidTo != 8 {
		t.Errorf("the payment in the node's first block: paid up to %d, want 8", witness[0].paidTo)
	}
	v := walk.verdicts()
	if got := v["old:0"]; got.verdict != verdictSpent || got.spent.Height != closedAt {
		t.Errorf("old channel: %+v, want spent at %d", got, closedAt)
	}
	if got := v["none:0"]; got.verdict != verdictUnverified {
		t.Errorf("channel without a height: %q, want unverified", got.verdict)
	}
}

// A channel newer than the headers at hand is not written off: a later run
// reaches it.
func TestChannelPastTheTipIsReachedLater(t *testing.T) {
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	chain.add(pay([]byte{0x51}, 1))
	fundingScript := []byte{0x00, 0x20, 0xaa}
	funding := pay(fundingScript, 100000)
	point := wire.OutPoint{Hash: funding.TxHash()}
	fundings := []channelFunding{{chanPoint: "new:0", outpoint: point, pkScript: fundingScript, heightHint: 700002}}
	walk := newChainWalk(fundings, nil, 0)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if got := walk.verdicts()["new:0"]; got.verdict != verdictUnverified {
		t.Fatalf("before its block exists: %q, want unverified", got.verdict)
	}
	chain.add(pay([]byte{0x51}, 2))
	chain.add(funding)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if got := walk.verdicts()["new:0"]; got.verdict != verdictOpen {
		t.Errorf("after its block arrived: %q, want open", got.verdict)
	}
}

func TestWalkStart(t *testing.T) {
	chain := &memChain{first: 0, prev: map[wire.OutPoint][]byte{}}
	for i := 0; i < 1000; i++ {
		chain.add(pay([]byte{0x51}, int64(i+1)))
	}
	at := func(h int) time.Time { return chain.blocks[h].Header.Timestamp }
	check := func(name string, birthday time.Time, wantFrom, wantFirst uint32) {
		t.Helper()
		from, first, err := walkStart(chain.headerAt, birthday, 999)
		if err != nil {
			t.Fatal(err)
		}
		if from != wantFrom || first != wantFirst {
			t.Errorf("%s: from %d first %d, want from %d first %d", name, from, first, wantFrom, wantFirst)
		}
	}
	check("two days before the birthday", at(700), 700-288, 1)
	check("near the start", at(100), 2, 1)
	check("birthday past the tip", at(999).Add(time.Hour), 1000-288, 1)

	// The library's header files are empty below its bootstrap checkpoint,
	// but for the earlier checkpoints, which stand alone.
	for h := 1; h < 600; h++ {
		if h%100 != 0 {
			// As read from an empty header file: all zero bytes.
			chain.blocks[h].Header = wire.BlockHeader{Timestamp: time.Unix(0, 0)}
		}
	}
	check("cut short by the first block the node has", at(700), 601, 600)
	check("far above the first block", at(950), 950-288, 600)
}

// Payments to addresses the wallet has already are recorded: the wallet's
// own history check must show them afterwards (verifyFound), and they rule
// out the history shortcut.
func TestPaymentsToTheWalletsOwnAddressesAreRecorded(t *testing.T) {
	witness, _, all := testBranches(t)
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	own := scriptAt(t, witness[0], 2) // below the counter of 7: the wallet has it
	chain.add(pay([]byte{0x51}, 1))
	paying := pay(own, 15721)
	paidAt := chain.add(paying)
	chain.add(pay([]byte{0x51}, 2))

	watch, err := newAddressWatch(all, chain.first)
	if err != nil {
		t.Fatal(err)
	}
	watch.own = map[string]bool{string(own): true}
	if err := newChainWalk(nil, watch, 0).run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if len(watch.hits) != 0 {
		t.Errorf("an address the wallet has counts as a find: %+v", watch.hits)
	}
	got, ok := watch.ownTxs[paying.TxHash().String()]
	if !ok || watch.ownPaid != 1 || got.Height != paidAt || got.Value != 15721 || got.Branch != ownBranch {
		t.Errorf("recorded %+v (paid %d), want the payment of 15721 sat in block %d", watch.ownTxs, watch.ownPaid, paidAt)
	}
}
