package core

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
)

// fakeChain has a tip and fails every other query.
type fakeChain struct {
	tip int32
}

func (f *fakeChain) BestBlock() (*headerfs.BlockStamp, error) {
	return &headerfs.BlockStamp{Height: f.tip}, nil
}

func (f *fakeChain) GetBlockHash(int64) (*chainhash.Hash, error) { return nil, errors.New("unused") }
func (f *fakeChain) GetCFilter(chainhash.Hash, wire.FilterType, ...neutrino.QueryOption) (*gcs.Filter, error) {
	return nil, errors.New("unused")
}
func (f *fakeChain) GetBlock(chainhash.Hash, ...neutrino.QueryOption) (*btcutil.Block, error) {
	return nil, errors.New("unused")
}

// fundingTx builds a transaction with one output carrying script.
func fundingTx(script []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 7}})
	tx.AddTxOut(&wire.TxOut{Value: 100000, PkScript: script})
	return tx
}

func spendOf(op wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	tx.AddTxOut(&wire.TxOut{Value: 90000, PkScript: []byte{0x51}})
	return tx
}

func TestVerdictNeedsProof(t *testing.T) {
	script := []byte{0x00, 0x20, 0x01}
	funding := fundingTx(script)
	op := wire.OutPoint{Hash: funding.TxHash(), Index: 0}
	other := fundingTx([]byte{0x00, 0x20, 0x02})
	closing := spendOf(op)

	cases := []struct {
		name   string
		blocks [][]*wire.MsgTx
		want   string
	}{
		{"nothing seen: not found is not unspent", nil, verdictUnverified},
		{"only other transactions", [][]*wire.MsgTx{{other}}, verdictUnverified},
		{"created, never spent", [][]*wire.MsgTx{{other, funding}}, verdictOpen},
		{"created, spent later", [][]*wire.MsgTx{{funding}, {other}, {closing}}, verdictSpent},
		{"created and spent in one block", [][]*wire.MsgTx{{funding, closing}}, verdictSpent},
		{"spend seen without the creation", [][]*wire.MsgTx{{closing}}, verdictSpent},
	}
	for _, tc := range cases {
		st := &fundingState{f: channelFunding{chanPoint: "aa:0", outpoint: op, pkScript: script, heightHint: 700000}}
		for i, txs := range tc.blocks {
			applyBlock([]*fundingState{st}, &wire.MsgBlock{Transactions: txs}, 700000+uint32(i))
		}
		got := st.verdict()
		if got.verdict != tc.want {
			t.Errorf("%s: verdict %q, want %q", tc.name, got.verdict, tc.want)
		}
		if tc.want == verdictSpent && got.spent.ClosingTxID != closing.TxHash().String() {
			t.Errorf("%s: closing tx %s", tc.name, got.spent.ClosingTxID)
		}
	}

	// The right transaction with another script at that output is not
	// this channel's funding.
	st := &fundingState{f: channelFunding{chanPoint: "aa:0", outpoint: op, pkScript: []byte{0x00, 0x20, 0x09}, heightHint: 700000}}
	applyBlock([]*fundingState{st}, &wire.MsgBlock{Transactions: []*wire.MsgTx{funding}}, 700000)
	if got := st.verdict(); got.verdict != verdictUnverified {
		t.Errorf("wrong script: verdict %q, want unverified", got.verdict)
	}
}

// A start height past the tip is never picked up by neutrino's scanner:
// such a channel must be set aside without asking.
func TestScanSkipsUnusableHeights(t *testing.T) {
	chain := &fakeChain{tip: 900000}
	fundings := []channelFunding{
		{chanPoint: "alias:0", heightHint: 16000000},
		{chanPoint: "zero:0", heightHint: 0},
	}
	got, err := scanFundingOutputs(context.Background(), chain, fundings, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fundings {
		if got[f.chanPoint].verdict != verdictUnverified {
			t.Errorf("%s: verdict %q, want unverified", f.chanPoint, got[f.chanPoint].verdict)
		}
	}
}

// A scan error must surface as an error, never as a verdict.
func TestScanErrorIsReturned(t *testing.T) {
	chain := &fakeChain{tip: 900000}
	_, err := scanFundingOutputs(context.Background(), chain, []channelFunding{{chanPoint: "aa:0", heightHint: 700000}}, nil)
	if err == nil {
		t.Fatal("expected the scan error")
	}
}

func TestChannelChecksFailClosed(t *testing.T) {
	var c channelChecks
	if v := c.get("aa:0"); v.verdict != verdictUnverified {
		t.Errorf("before the check: %q, want unverified", v.verdict)
	}
	c.set(map[string]channelVerdict{"aa:0": {verdict: verdictOpen}})
	if v := c.get("aa:0"); v.verdict != verdictOpen {
		t.Errorf("checked channel: %q, want open", v.verdict)
	}
	if v := c.get("bb:1"); v.verdict != verdictUnverified {
		t.Errorf("channel the check never saw: %q, want unverified", v.verdict)
	}
}

func TestFundingHeightHint(t *testing.T) {
	cases := []struct {
		name      string
		scid      uint32
		broadcast uint32
		zeroConf  bool
		want      uint32
	}{
		{"ordinary channel", 653404, 653403, false, 653404},
		{"made-up scid of the LSP's early zero-conf channels", 155808, 726471, false, 726471},
		{"zero-conf alias, funding not confirmed in the backup", 16000000, 758330, true, 758330},
	}
	for _, tc := range cases {
		ch := &channeldb.OpenChannel{
			ShortChannelID:         lnwire.ShortChannelID{BlockHeight: tc.scid},
			FundingBroadcastHeight: tc.broadcast,
		}
		if tc.zeroConf {
			ch.ChanType = channeldb.ZeroConfBit
		}
		if got := fundingHeightHint(ch); got != tc.want {
			t.Errorf("%s: hint %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestLncliRefusesClosingCommands(t *testing.T) {
	c := &Core{node: &node{}}
	for _, cmd := range []string{"closechannel --force abc 0", "CloseAllChannels", "abandonchannel abc 0"} {
		if _, err := c.Lncli(context.Background(), cmd); err == nil {
			t.Errorf("%q was not refused", cmd)
		}
	}
}

func TestIsDust(t *testing.T) {
	for _, tc := range []struct {
		balance       int64
		local, remote uint64
		want          bool
	}{
		// Breez mobile: own limit zero, the LSP's decides.
		{100, 0, 354, true}, {179, 0, 354, true}, {353, 0, 354, true}, {354, 0, 354, false},
		{500, 0, 573, true}, {600, 0, 573, false}, {999, 0, 573, false},
		// The larger limit counts whichever side has it.
		{400, 546, 354, true}, {5000, 546, 354, false},
	} {
		c := &lnrpc.Channel{
			LocalBalance:      tc.balance,
			LocalConstraints:  &lnrpc.ChannelConstraints{DustLimitSat: tc.local},
			RemoteConstraints: &lnrpc.ChannelConstraints{DustLimitSat: tc.remote},
		}
		if got := isDust(c); got != tc.want {
			t.Errorf("balance %d, limits %d/%d: %v, want %v", tc.balance, tc.local, tc.remote, got, tc.want)
		}
	}
	// A channel without constraints reported is never called dust.
	if isDust(&lnrpc.Channel{LocalBalance: 0}) {
		t.Error("no dust limit known, yet called dust")
	}
}

// What a close paid this app counts as money to collect only while nobody
// has spent it: a backup taken before the phone swept a close must not show
// that money again.
func TestCollectOnlyWhatIsStillThere(t *testing.T) {
	fundingScript := []byte{0x00, 0x20, 0x01}
	toUs := []byte{0x00, 0x14, 0x0a}
	funding := fundingTx(fundingScript)
	op := wire.OutPoint{Hash: funding.TxHash(), Index: 0}

	closing := wire.NewMsgTx(2)
	closing.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	closing.AddTxOut(&wire.TxOut{Value: 94317, PkScript: []byte{0x00, 0x20, 0x07}}) // the peer's side
	closing.AddTxOut(&wire.TxOut{Value: 999, PkScript: toUs})
	sweep := spendOf(wire.OutPoint{Hash: closing.TxHash(), Index: 1})

	newState := func(script []byte) *fundingState {
		return &fundingState{f: channelFunding{chanPoint: "aa:0", outpoint: op, pkScript: fundingScript, toUsScript: script, heightHint: 700000}}
	}
	run := func(st *fundingState, blocks ...[]*wire.MsgTx) channelVerdict {
		for i, txs := range blocks {
			applyBlock([]*fundingState{st}, &wire.MsgBlock{Transactions: txs}, 700000+uint32(i))
		}
		return st.verdict()
	}

	if v := run(newState(toUs), []*wire.MsgTx{funding}, []*wire.MsgTx{closing}); v.verdict != verdictSpent || v.spent.Collect != 999 {
		t.Errorf("paid and unspent: %q collect %d, want spent 999", v.verdict, v.spent.Collect)
	}
	if v := run(newState(toUs), []*wire.MsgTx{funding}, []*wire.MsgTx{closing}, []*wire.MsgTx{sweep}); v.spent.Collect != 0 {
		t.Errorf("already swept: collect %d, want 0", v.spent.Collect)
	}
	if v := run(newState(toUs), []*wire.MsgTx{funding}, []*wire.MsgTx{closing, sweep}); v.spent.Collect != 0 {
		t.Errorf("swept in the closing block: collect %d, want 0", v.spent.Collect)
	}
	if v := run(newState(nil), []*wire.MsgTx{funding}, []*wire.MsgTx{closing}); v.spent.Collect != 0 {
		t.Errorf("no derivable script: collect %d, want 0", v.spent.Collect)
	}
	if v := run(newState([]byte{0x00, 0x14, 0x0b}), []*wire.MsgTx{funding}, []*wire.MsgTx{closing}); v.spent.Collect != 0 {
		t.Errorf("close paid another key: collect %d, want 0", v.spent.Collect)
	}
}
