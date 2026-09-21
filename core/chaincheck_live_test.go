package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/breez/breez/chainservice"
	breezdb "github.com/breez/breez/db"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
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

// TestFollowTheMoneyLive proves, on the real chain, that funds a closed
// channel's sweep paid to the wallet are found WITHOUT the gap search: no
// address is in the filter match, the walk only watches the channels, and
// the address is recognised in the block of the sweep. Read-only like the
// test above, with the same BREEZ_LIVE_WORKDIR and BREEZ_LIVE_CHANDBS
// (dirs with a copy of the BACKUP's channel.db), plus
//
//	BREEZ_LIVE_WALLETDB  a COPY of a wallet.db of the node: the account keys
//	                     and address counters are read from it. A backup from
//	                     before taproot has no taproot account until lnd has
//	                     opened it once, so take a restored folder's file and
//	                     set BREEZ_LIVE_FRESH=1 to count from zero as a fresh
//	                     restore of that backup would
//	BREEZ_LIVE_PAID      what must be found, e.g. "taproot receive:51"
//	                     (branch name : one past the paid address)
func TestFollowTheMoneyLive(t *testing.T) {
	workDir := os.Getenv("BREEZ_LIVE_WORKDIR")
	if workDir == "" || os.Getenv("BREEZ_LIVE_WALLETDB") == "" {
		t.Skip("BREEZ_LIVE_WORKDIR or BREEZ_LIVE_WALLETDB not set")
	}
	wdb, err := walletdb.Open("bdb", os.Getenv("BREEZ_LIVE_WALLETDB"), true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var branches []*addrBranch
	err = walletdb.View(wdb, func(tx walletdb.ReadTx) error {
		mgr, err := waddrmgr.Open(tx.ReadBucket([]byte("waddrmgr")), []byte("public"), &chaincfg.MainNetParams)
		if err != nil {
			return err
		}
		for scope, addrType := range map[waddrmgr.KeyScope]walletrpc.AddressType{
			waddrmgr.KeyScopeBIP0084: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
			waddrmgr.KeyScopeBIP0086: walletrpc.AddressType_TAPROOT_PUBKEY,
		} {
			scoped, err := mgr.FetchScopedKeyManager(scope)
			if err != nil {
				t.Logf("scope %v: %v", scope, err)
				continue
			}
			props, err := scoped.AccountProperties(tx.ReadBucket([]byte("waddrmgr")), 0)
			if err != nil {
				return err
			}
			t.Logf("scope %v: counters %d receive, %d change", scope, props.ExternalKeyCount, props.InternalKeyCount)
			if os.Getenv("BREEZ_LIVE_FRESH") != "" {
				props.ExternalKeyCount, props.InternalKeyCount = 0, 0
			}
			brs, err := accountBranches(addrType, props.AccountPubKey.String(), props.ExternalKeyCount, props.InternalKeyCount, &chaincfg.MainNetParams)
			if err != nil {
				return err
			}
			for _, b := range brs {
				b.gap = false // the point of this test
			}
			branches = append(branches, brs...)
		}
		return nil
	})
	wdb.Close()
	if err != nil {
		t.Fatal(err)
	}

	var fundings []channelFunding
	for _, dir := range strings.Split(os.Getenv("BREEZ_LIVE_CHANDBS"), ":") {
		db, err := channeldb.Open(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		chans, err := db.ChannelStateDB().FetchAllOpenChannels()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range chans {
			f, err := fundingOf(ch)
			if err != nil {
				t.Fatal(err)
			}
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
	if err := chain.Start(); err != nil {
		t.Fatal(err)
	}
	for !chain.IsCurrent() {
		best, _ := chain.BestBlock()
		t.Logf("waiting for headers, at %d", best.Height)
		time.Sleep(10 * time.Second)
	}
	watch, err := newAddressWatch(branches, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(watch.scripts) != 0 {
		t.Fatalf("%d addresses in the filter match, want none", len(watch.scripts))
	}
	watch.onHit = func(hit foundOutput) { t.Log("found " + hit.String()) }
	// With nothing of its own in the match the watch must not make the
	// walk start before the channels do.
	watch.from = 0
	for _, f := range fundings {
		if watch.from == 0 || f.heightHint < watch.from {
			watch.from = f.heightHint
		}
	}
	start := time.Now()
	walk := newChainWalk(fundings, watch, 0)
	if err := walk.run(context.Background(), chain, func(h, from, tip uint32) {
		if h%20000 == 0 {
			t.Logf("block %d of %d (from %d), %s", h, tip, from, time.Since(start).Round(time.Second))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("walk took %s", time.Since(start).Round(time.Second))
	for point, v := range walk.verdicts() {
		t.Logf("%s: %s %s collect %d", point, v.verdict, v.spent.ClosingTxID, v.spent.Collect)
	}
	want := os.Getenv("BREEZ_LIVE_PAID")
	for _, b := range branches {
		t.Logf("%s: paid up to %d", b.name, b.paidTo)
		if name, upTo, ok := strings.Cut(want, ":"); ok && name == b.name {
			if fmt.Sprint(b.paidTo) != upTo {
				t.Errorf("%s: paid up to %d, want %s", b.name, b.paidTo, upTo)
			}
			want = ""
		}
	}
	if want != "" {
		t.Errorf("branch of %q not among the accounts", want)
	}
}
