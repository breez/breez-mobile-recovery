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
	"github.com/lightningnetwork/lnd/lnwire"
)

// fakeChain has a tip and fails every query.
type fakeChain struct {
	tip int32
}

func (f *fakeChain) BestBlock() (*headerfs.BlockStamp, error) {
	return &headerfs.BlockStamp{Height: f.tip}, nil
}

func (f *fakeChain) GetUtxo(options ...neutrino.RescanOption) (*neutrino.SpendReport, error) {
	return nil, errors.New("no peers")
}

func (f *fakeChain) GetBlockHash(int64) (*chainhash.Hash, error) { return nil, errors.New("unused") }
func (f *fakeChain) GetCFilter(chainhash.Hash, wire.FilterType, ...neutrino.QueryOption) (*gcs.Filter, error) {
	return nil, errors.New("unused")
}
func (f *fakeChain) GetBlock(chainhash.Hash, ...neutrino.QueryOption) (*btcutil.Block, error) {
	return nil, errors.New("unused")
}

func TestVerdictFromReport(t *testing.T) {
	script := []byte{0x00, 0x20, 0x01}
	f := channelFunding{chanPoint: "aa:0", pkScript: script, heightHint: 700000}
	spend := wire.NewMsgTx(2)
	cases := []struct {
		name   string
		report *neutrino.SpendReport
		want   string
	}{
		{"no report: the output was not found, which is not unspent", nil, verdictUnverified},
		{"report without output", &neutrino.SpendReport{}, verdictUnverified},
		{"output with another script", &neutrino.SpendReport{Output: &wire.TxOut{PkScript: []byte{0x51}}}, verdictUnverified},
		{"output found and unspent", &neutrino.SpendReport{Output: &wire.TxOut{PkScript: script}}, verdictOpen},
		{"spent", &neutrino.SpendReport{SpendingTx: spend, SpendingTxHeight: 800000}, verdictSpent},
	}
	for _, tc := range cases {
		got := verdictFromReport(f, tc.report)
		if got.verdict != tc.want {
			t.Errorf("%s: verdict %q, want %q", tc.name, got.verdict, tc.want)
		}
		if tc.want == verdictSpent && (got.spent.ClosingTxID != spend.TxHash().String() || got.spent.Height != 800000) {
			t.Errorf("%s: wrong spend details %+v", tc.name, got.spent)
		}
	}
}

// A start height past the tip is never picked up by neutrino's scanner:
// such a channel must be set aside without asking.
func TestScanSkipsUnusableHeights(t *testing.T) {
	chain := &fakeChain{tip: 900000}
	fundings := []channelFunding{
		{chanPoint: "alias:0", heightHint: 16000000, exact: true},
		{chanPoint: "zero:0", heightHint: 0, exact: true},
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
	_, err := scanFundingOutputs(context.Background(), chain, []channelFunding{{chanPoint: "aa:0", heightHint: 700000, exact: true}}, nil)
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
		exact     bool
	}{
		{"ordinary channel", 653404, 653403, false, 653404, true},
		{"made-up scid of the LSP's early zero-conf channels", 155808, 726471, false, 726471, false},
		{"zero-conf alias, funding not confirmed in the backup", 16000000, 758330, true, 758330, false},
	}
	for _, tc := range cases {
		ch := &channeldb.OpenChannel{
			ShortChannelID:         lnwire.ShortChannelID{BlockHeight: tc.scid},
			FundingBroadcastHeight: tc.broadcast,
		}
		if tc.zeroConf {
			ch.ChanType = channeldb.ZeroConfBit
		}
		got, exact := fundingHeightHint(ch)
		if got != tc.want || exact != tc.exact {
			t.Errorf("%s: hint %d exact %v, want %d %v", tc.name, got, exact, tc.want, tc.exact)
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
