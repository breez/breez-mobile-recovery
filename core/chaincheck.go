package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/breez/breez/chainservice"
	"github.com/breez/breez/channeldbservice"
	breezdb "github.com/breez/breez/db"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// SpentChannel is a channel the node still lists as open although its
// funding output was spent on chain: the channel closed after the backup
// was taken. Its balance is gone from the channel and cannot be closed
// again; whatever was settled is on chain.
type SpentChannel struct {
	ChannelPoint string `json:"channelPoint"`
	ClosingTxID  string `json:"closingTxid"`
	Height       uint32 `json:"height"`
}

// A channel lnd lists as open gets one of these verdicts. Only verdictOpen
// counts as funds and only verdictOpen is ever closed: the default for
// anything the check did not positively confirm is to leave it alone.
const (
	verdictOpen       = "open"       // funding output found on chain, unspent, and the wallet holds its key
	verdictSpent      = "spent"      // funding output spent: closed after the backup was taken
	verdictForeign    = "foreign"    // this backup's wallet cannot derive the channel's funding key
	verdictUnverified = "unverified" // the check could not confirm the funding output
)

type channelVerdict struct {
	verdict string
	spent   SpentChannel // for verdictSpent
	reason  string       // for verdictUnverified
}

// channelChecks holds the result of the last check, keyed by channel point.
// A channel without an entry has not been checked and is treated like an
// unverified one.
type channelChecks struct {
	mu       sync.Mutex
	checked  bool
	verdicts map[string]channelVerdict
}

func (s *channelChecks) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checked
}

func (s *channelChecks) set(m map[string]channelVerdict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verdicts, s.checked = m, true
}

func (s *channelChecks) get(chanPoint string) channelVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		return channelVerdict{verdict: verdictUnverified, reason: "not checked on chain yet"}
	}
	v, ok := s.verdicts[chanPoint]
	if !ok {
		return channelVerdict{verdict: verdictUnverified, reason: "not checked on chain yet"}
	}
	return v
}

// channelFunding is what the chain scan needs to know about one channel.
type channelFunding struct {
	chanPoint  string
	outpoint   wire.OutPoint
	pkScript   []byte
	heightHint uint32
	// exact says heightHint is the block the funding confirmed in, see
	// fundingHeightHint.
	exact bool
}

// fundingHeightHint is the block to start looking for the funding output,
// and whether that is the block the funding transaction confirmed in. Three
// kinds of channel exist in Breez backups:
//
//   - ordinary ones, whose ShortChannelID holds the confirmation block;
//   - zero-conf ones (Breez LSP channels since late 2022), whose
//     ShortChannelID is an alias with a made-up height of 16,000,000 and up;
//     lnd keeps the real one separately once the funding confirms;
//   - the LSP's earlier zero-conf channels (2021 to 2022), whose
//     ShortChannelID is made up as well, with a height far below the block
//     the funding was broadcast at.
//
// When the confirmation block is not known the hint is the broadcast
// height, a real chain height at or below it, and findFundingBlock locates
// the block from there.
func fundingHeightHint(ch *channeldb.OpenChannel) (hint uint32, exact bool) {
	broadcast := ch.BroadcastHeight()
	if ch.IsZeroConf() {
		if ch.ZeroConfConfirmed() {
			return ch.ZeroConfRealScid().BlockHeight, true
		}
		return broadcast, false
	}
	if scid := ch.ShortChanID().BlockHeight; scid != 0 && scid >= broadcast {
		return scid, true
	}
	return broadcast, false
}

// fundingSearchWindow is how far past the broadcast height the funding
// transaction is looked for: two weeks of blocks, the limit after which lnd
// itself gives up on a funding transaction.
const fundingSearchWindow = 2016

// findFundingBlock locates the block that created the funding output,
// using the node's own compact filters, from the block it was broadcast at.
func findFundingBlock(chain utxoSource, f channelFunding, tip uint32) (uint32, bool, error) {
	last := f.heightHint + fundingSearchWindow
	if last > tip {
		last = tip
	}
	for h := f.heightHint; h <= last; h++ {
		hash, err := chain.GetBlockHash(int64(h))
		if err != nil {
			return 0, false, err
		}
		filter, err := chain.GetCFilter(*hash, wire.GCSFilterRegular)
		if err != nil {
			return 0, false, err
		}
		if filter == nil {
			return 0, false, fmt.Errorf("no filter for block %d", h)
		}
		match, err := filter.Match(builder.DeriveKey(hash), f.pkScript)
		if err != nil {
			return 0, false, err
		}
		if !match {
			continue
		}
		block, err := chain.GetBlock(*hash)
		if err != nil {
			return 0, false, err
		}
		for _, tx := range block.Transactions() {
			if *tx.Hash() == f.outpoint.Hash {
				return h, true, nil
			}
		}
	}
	return 0, false, nil
}

// fundingPkScript is the script of the channel's funding output, as lnd's
// chain watcher derives it.
func fundingPkScript(ch *channeldb.OpenChannel) ([]byte, error) {
	local := ch.LocalChanCfg.MultiSigKey.PubKey
	remote := ch.RemoteChanCfg.MultiSigKey.PubKey
	if ch.ChanType.IsTaproot() {
		script, _, err := input.GenTaprootFundingScript(local, remote, 0, ch.TapscriptRoot)
		return script, err
	}
	multiSig, err := input.GenMultiSigScript(local.SerializeCompressed(), remote.SerializeCompressed())
	if err != nil {
		return nil, err
	}
	return input.WitnessScriptHash(multiSig)
}

// utxoSource is the part of neutrino's chain service the scan uses.
type utxoSource interface {
	GetUtxo(options ...neutrino.RescanOption) (*neutrino.SpendReport, error)
	BestBlock() (*headerfs.BlockStamp, error)
	GetBlockHash(height int64) (*chainhash.Hash, error)
	GetCFilter(hash chainhash.Hash, filterType wire.FilterType, options ...neutrino.QueryOption) (*gcs.Filter, error)
	GetBlock(hash chainhash.Hash, options ...neutrino.QueryOption) (*btcutil.Block, error)
}

// scanFundingOutputs asks the chain, for each funding output, whether it is
// still unspent. The verdict is open only when the scan found the output
// itself, with the expected script, and no spend of it up to the tip. A
// scan error is returned, never turned into a verdict.
func scanFundingOutputs(ctx context.Context, chain utxoSource, fundings []channelFunding, onBlock func(height, from, tip uint32)) (map[string]channelVerdict, error) {
	out := map[string]channelVerdict{}
	best, err := chain.BestBlock()
	if err != nil {
		return nil, fmt.Errorf("read the chain tip: %w", err)
	}
	tip := uint32(best.Height)

	var scan []channelFunding
	for _, f := range fundings {
		// A start height beyond the tip would never be picked up by
		// neutrino's scanner (the request would wait forever), and zero
		// means the channel database has no usable height.
		if f.heightHint == 0 || f.heightHint > tip {
			out[f.chanPoint] = channelVerdict{verdict: verdictUnverified,
				reason: fmt.Sprintf("no usable funding height (%d, chain tip %d)", f.heightHint, tip)}
			continue
		}
		if !f.exact {
			height, found, err := findFundingBlock(chain, f, tip)
			if err != nil {
				return nil, fmt.Errorf("look for the funding of channel %s: %w", f.chanPoint, err)
			}
			if !found {
				out[f.chanPoint] = channelVerdict{verdict: verdictUnverified,
					reason: fmt.Sprintf("funding transaction not found on chain from block %d on", f.heightHint)}
				continue
			}
			f.heightHint = height
		}
		scan = append(scan, f)
	}
	if len(scan) == 0 {
		return out, nil
	}
	// Lowest height first: neutrino's scanner starts a pass at the lowest
	// height queued and picks up the requests for later heights on its way,
	// so all outputs are covered by one pass over the chain.
	sort.Slice(scan, func(i, j int) bool { return scan[i].heightHint < scan[j].heightHint })
	from := scan[0].heightHint

	type result struct {
		f      channelFunding
		report *neutrino.SpendReport
		err    error
	}
	results := make(chan result, len(scan))
	// neutrino reports progress only to requests that are still waiting,
	// and any of them can be answered early (a spend found), so all of
	// them carry the handler and each height is passed on once.
	var progressMu sync.Mutex
	reached := from
	progress := func(h uint32) {
		progressMu.Lock()
		fresh := h > reached
		if fresh {
			reached = h
		}
		progressMu.Unlock()
		if fresh {
			onBlock(h, from, tip)
		}
	}
	for i, f := range scan {
		opts := []neutrino.RescanOption{
			neutrino.WatchInputs(neutrino.InputWithScript{OutPoint: f.outpoint, PkScript: f.pkScript}),
			neutrino.StartBlock(&headerfs.BlockStamp{Height: int32(f.heightHint)}),
			neutrino.QuitChan(ctx.Done()),
		}
		if onBlock != nil {
			opts = append(opts, neutrino.ProgressHandler(progress))
		}
		go func(f channelFunding, opts []neutrino.RescanOption) {
			report, err := chain.GetUtxo(opts...)
			results <- result{f: f, report: report, err: err}
		}(f, opts)
		if i == 0 {
			// GetUtxo blocks, so every request needs a goroutine, and the
			// scanner starts its pass at whichever request reaches it
			// first. Seen live: a pass started at the newest channel and
			// all older ones waited for a second pass over the chain. Let
			// the lowest one get there first.
			time.Sleep(250 * time.Millisecond)
		}
	}
	var firstErr error
	for range scan {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("check channel %s on chain: %w", r.f.chanPoint, r.err)
			}
			continue
		}
		out[r.f.chanPoint] = verdictFromReport(r.f, r.report)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// verdictFromReport turns neutrino's answer about a funding output into a
// verdict. Open needs positive proof: the output itself, with the expected
// script, and no spend.
func verdictFromReport(f channelFunding, report *neutrino.SpendReport) channelVerdict {
	switch {
	case report != nil && report.SpendingTx != nil:
		return channelVerdict{verdict: verdictSpent, spent: SpentChannel{
			ChannelPoint: f.chanPoint,
			ClosingTxID:  report.SpendingTx.TxHash().String(),
			Height:       report.SpendingTxHeight,
		}}
	case report == nil || report.Output == nil:
		// neutrino returns no report when it did not find the output in
		// the start block. Not found is not the same as unspent.
		return channelVerdict{verdict: verdictUnverified,
			reason: fmt.Sprintf("funding output not found on chain at block %d", f.heightHint)}
	case !bytes.Equal(report.Output.PkScript, f.pkScript):
		return channelVerdict{verdict: verdictUnverified,
			reason: "the funding output on chain does not match the channel's keys"}
	}
	return channelVerdict{verdict: verdictOpen}
}

// CheckChannelsOnChain verifies every channel lnd lists as open before the
// app shows it as funds or closes it. A backup is a snapshot: channels that
// closed after it was taken still look open in it, and a backup can even
// pair one node's wallet with another node's channels (seen in a real Breez
// backup from 2022). Two checks, both answered by the node itself, nothing
// is asked of a third party:
//
//  1. the wallet must derive the channel's funding key at the channel's own
//     key locator (walletrpc DeriveKey);
//  2. the node's neutrino must find the funding output on chain, unspent.
//
// Only a channel that passes both counts as funds and can be closed. A
// failure of the check itself is returned, never swallowed.
//
// It returns the channels found closed on chain. onProgress may be nil.
func (c *Core) CheckChannelsOnChain(ctx context.Context, onProgress func(SyncProgress)) ([]SpentChannel, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	chans, err := c.node.openChannels(ctx)
	if err != nil {
		return nil, err
	}
	if len(chans) == 0 {
		c.checks.set(map[string]channelVerdict{})
		return nil, nil
	}
	chandb, releaseDB, err := channeldbservice.Get(c.dir())
	if err != nil {
		return nil, fmt.Errorf("open the channel database: %w", err)
	}
	defer releaseDB()
	bdb, releaseBreezDB, err := breezdb.Get(c.dir())
	if err != nil {
		return nil, fmt.Errorf("open the app database: %w", err)
	}
	defer releaseBreezDB()
	chain, releaseChain, err := chainservice.Get(c.dir(), bdb)
	if err != nil {
		return nil, fmt.Errorf("reach the bitcoin network: %w", err)
	}
	defer releaseChain()

	stored, err := chandb.ChannelStateDB().FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}
	byPoint := map[string]*channeldb.OpenChannel{}
	for _, ch := range stored {
		byPoint[ch.FundingOutpoint.String()] = ch
	}

	verdicts := map[string]channelVerdict{}
	var fundings []channelFunding
	wk := walletrpc.NewWalletKitClient(c.node.conn)
	for _, rpcChan := range chans {
		ch := byPoint[rpcChan.ChannelPoint]
		if ch == nil {
			return nil, fmt.Errorf("channel %s is open in lnd but not in its database", rpcChan.ChannelPoint)
		}
		key := ch.LocalChanCfg.MultiSigKey
		res, err := wk.DeriveKey(ctx, &signrpc.KeyLocator{KeyFamily: int32(key.Family), KeyIndex: int32(key.Index)})
		if err != nil {
			return nil, fmt.Errorf("derive the key of channel %s: %w", rpcChan.ChannelPoint, err)
		}
		if !bytes.Equal(res.RawKeyBytes, key.PubKey.SerializeCompressed()) {
			verdicts[rpcChan.ChannelPoint] = channelVerdict{verdict: verdictForeign}
			c.progressf("  %s belongs to another node: this backup cannot sign for it.", rpcChan.ChannelPoint)
			continue
		}
		script, err := fundingPkScript(ch)
		if err != nil {
			return nil, fmt.Errorf("funding script of %s: %w", rpcChan.ChannelPoint, err)
		}
		hint, exact := fundingHeightHint(ch)
		fundings = append(fundings, channelFunding{
			chanPoint:  rpcChan.ChannelPoint,
			outpoint:   ch.FundingOutpoint,
			pkScript:   script,
			heightHint: hint,
			exact:      exact,
		})
	}

	if len(fundings) > 0 {
		c.progressf("Checking on chain that your %d channel(s) are still open...", len(fundings))
		started := time.Now()
		scanned, err := scanFundingOutputs(ctx, chain, fundings, func(height, from, tip uint32) {
			if onProgress == nil || height%500 != 0 {
				return
			}
			p := SyncProgress{Stage: "channels", Height: height, Target: tip, Percent: -1, Remaining: -1,
				Message: "Making sure your channels are still open"}
			if tip > from {
				p.Percent = 100 * float64(height-from) / float64(tip-from)
			}
			// Time left from the rate so far, once there is a rate to speak of.
			if elapsed := time.Since(started).Seconds(); elapsed >= 15 && height > from && tip >= height {
				p.Remaining = int64(float64(tip-height) / (float64(height-from) / elapsed))
			}
			onProgress(p)
		})
		if err != nil {
			return nil, err
		}
		for point, v := range scanned {
			verdicts[point] = v
		}
	}

	var closed []SpentChannel
	for _, rpcChan := range chans {
		switch v := verdicts[rpcChan.ChannelPoint]; v.verdict {
		case verdictSpent:
			closed = append(closed, v.spent)
			c.progressf("  %s closed on chain already, in transaction %s.", rpcChan.ChannelPoint, v.spent.ClosingTxID)
		case verdictUnverified:
			c.progressf("  %s could not be confirmed on chain (%s); it is left alone.", rpcChan.ChannelPoint, v.reason)
		}
	}
	c.checks.set(verdicts)
	if len(closed) > 0 {
		c.progressf("%d of your channels closed after this backup was taken; their funds are not in the app.", len(closed))
	}
	return closed, nil
}
