package core

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/breez/breez/chainservice"
	breezdb "github.com/breez/breez/db"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/channeldb"
)

// TestScanFundingOutputsLive runs the chain scan against the real bitcoin
// network. It is skipped unless the environment names its inputs, and it
// only reads: no node is started and nothing is broadcast.
//
//	BREEZ_LIVE_WORKDIR   a COPY of a work dir with breez.conf, breez.db and
//	                     the neutrino files (headers synced, or it waits)
//	BREEZ_LIVE_CHANDBS   dirs holding a COPY of a channel.db, colon separated
//	BREEZ_LIVE_EXPECT    JSON file: {"<channel point>": "open"|"spent"|...}
//
//	go test -tags walletrpc,chainrpc -run TestScanFundingOutputsLive -timeout 2h -v ./core
func TestScanFundingOutputsLive(t *testing.T) {
	workDir := os.Getenv("BREEZ_LIVE_WORKDIR")
	if workDir == "" {
		t.Skip("BREEZ_LIVE_WORKDIR not set")
	}
	expect := map[string]string{}
	raw, err := os.ReadFile(os.Getenv("BREEZ_LIVE_EXPECT"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var fundings []channelFunding
	for _, dir := range strings.Split(os.Getenv("BREEZ_LIVE_CHANDBS"), ":") {
		db, err := channeldb.Open(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		chans, err := db.ChannelStateDB().FetchAllChannels()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range chans {
			point := ch.FundingOutpoint.String()
			if seen[point] {
				continue
			}
			seen[point] = true
			script, err := fundingPkScript(ch)
			if err != nil {
				t.Fatal(err)
			}
			f := channelFunding{chanPoint: point, outpoint: ch.FundingOutpoint, pkScript: script, heightHint: fundingHeightHint(ch)}
			t.Logf("%s zero-conf=%v scid height %d, hint %d", point, ch.IsZeroConf(), ch.ShortChanID().BlockHeight, f.heightHint)
			fundings = append(fundings, f)
		}
		db.Close()
	}

	bdb, releaseDB, err := breezdb.Get(workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDB()
	chain, release, err := chainservice.Get(workDir, bdb)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// In the app lnd starts the chain service; here nothing else does.
	if err := chain.Start(); err != nil {
		t.Fatal(err)
	}
	for !chain.IsCurrent() {
		best, _ := chain.BestBlock()
		t.Logf("waiting for headers, at %d", best.Height)
		time.Sleep(10 * time.Second)
	}

	start := time.Now()
	verdicts, err := scanFundingOutputs(context.Background(), chain, fundings, func(h, from, tip uint32) {
		if h%20000 == 0 {
			t.Logf("block %d of %d (from %d), %s", h, tip, from, time.Since(start).Round(time.Second))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scan took %s", time.Since(start).Round(time.Second))
	for _, f := range fundings {
		v := verdicts[f.chanPoint]
		t.Logf("%s: %s %s %s", f.chanPoint, v.verdict, v.spent.ClosingTxID, v.reason)
		if want, ok := expect[f.chanPoint]; ok && want != v.verdict {
			t.Errorf("%s: verdict %q, want %q", f.chanPoint, v.verdict, want)
		}
	}
	for point := range expect {
		if !seen[point] {
			t.Errorf("expected channel %s not found in the channel databases", point)
		}
	}
}

// TestToUsScriptLive checks the script toUsScript derives against real
// closing transactions. It is skipped unless the environment names its
// inputs and reads only copies of channel databases.
//
//	BREEZ_LIVE_CHANDBS  dirs holding a COPY of a channel.db, colon separated
//	BREEZ_LIVE_TOUS     JSON file: {"<channel point>": {"forced": true,
//	                    "outputs": [{"address": "bc1...", "value": 999}]}}
//	                    the plain key outputs of the channel's closing
//	                    transaction, and whether it was a commitment (the
//	                    closer is paid through a script output) rather than a
//	                    cooperative close (both sides paid to addresses)
func TestToUsScriptLive(t *testing.T) {
	path := os.Getenv("BREEZ_LIVE_TOUS")
	if path == "" {
		t.Skip("BREEZ_LIVE_TOUS not set")
	}
	type output struct {
		Address string `json:"address"`
		Value   int64  `json:"value"`
	}
	type closing struct {
		Forced  bool     `json:"forced"`
		Outputs []output `json:"outputs"`
	}
	expect := map[string]closing{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	matched := 0
	for _, dir := range strings.Split(os.Getenv("BREEZ_LIVE_CHANDBS"), ":") {
		db, err := channeldb.Open(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		chans, err := db.ChannelStateDB().FetchAllChannels()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range chans {
			point := ch.FundingOutpoint.String()
			cl, ok := expect[point]
			if !ok || seen[point] {
				continue
			}
			seen[point] = true
			outs := cl.Outputs
			script := toUsScript(ch)
			if script == nil {
				t.Logf("%s: type %d, no script derivable; %d key outputs in its close", point, ch.ChanType, len(outs))
				if len(outs) > 0 {
					t.Errorf("%s: its close paid a key output but no script was derived", point)
				}
				continue
			}
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, &chaincfg.MainNetParams)
			if err != nil || len(addrs) != 1 {
				t.Fatalf("%s: derived script has no single address: %v", point, err)
			}
			derived := addrs[0].EncodeAddress()
			hit := false
			for _, o := range outs {
				if o.Address == derived {
					hit = true
					matched++
					t.Logf("%s: derived %s = the %d sat output of its close", point, derived, o.Value)
				}
			}
			switch {
			case len(outs) > 0 && !hit && cl.Forced:
				t.Errorf("%s: derived %s, but its close paid %v", point, derived, outs)
			case len(outs) > 0 && !hit:
				// A cooperative close pays this side to a wallet address,
				// which the wallet finds itself: nothing for lnd to
				// collect, and no match is the right answer.
				t.Logf("%s: cooperative close, paid to wallet addresses; derived %s matches none, as it should", point, derived)
			}
			if len(outs) == 0 {
				t.Logf("%s: derived %s; its close has no key output (nothing was paid to this side)", point, derived)
			}
		}
		db.Close()
	}
	t.Logf("%d closes paid this side, all matched", matched)
	if matched == 0 {
		t.Error("no close with a payout was checked")
	}
}
