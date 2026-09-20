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
	"github.com/lightningnetwork/lnd/lnwallet"
)

// SpentChannel is a channel the node still lists as open although its
// funding output was spent on chain: the channel closed after the backup
// was taken. Its balance is gone from the channel and cannot be closed
// again; whatever was settled is on chain.
type SpentChannel struct {
	ChannelPoint string `json:"channelPoint"`
	ClosingTxID  string `json:"closingTxid"`
	Height       uint32 `json:"height"`
	// Collect is what the closing transaction paid to this app's key and
	// nobody has spent yet: money lnd still has to collect into the
	// wallet. Zero when there is none, or when the output cannot be told
	// apart (channels older than static remote keys).
	Collect int64 `json:"collect"`
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
	// toUsScript is the output script a close by the peer pays this app's
	// share to, nil when it cannot be derived.
	toUsScript []byte
}

// fundingHeightHint is the block to start looking for the funding output
// at: the block it confirmed in, or a real chain height below it. Three
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
// When the confirmation block is not known the hint is the height the
// funding was broadcast at; the scan finds the output wherever it confirmed.
func fundingHeightHint(ch *channeldb.OpenChannel) uint32 {
	broadcast := ch.BroadcastHeight()
	if ch.IsZeroConf() {
		if ch.ZeroConfConfirmed() {
			return ch.ZeroConfRealScid().BlockHeight
		}
		return broadcast
	}
	if scid := ch.ShortChanID().BlockHeight; scid != 0 && scid >= broadcast {
		return scid
	}
	return broadcast
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

// toUsScript is the output script a close by the peer pays this app's share
// to. With static remote keys (every channel since 2020) it is fixed by the
// channel's own payment key; older channels tweak the key per commitment
// and it cannot be told from here, so nil.
func toUsScript(ch *channeldb.OpenChannel) []byte {
	if !ch.ChanType.IsTweakless() && !ch.ChanType.HasAnchors() {
		return nil
	}
	// The commitment is the peer's, so "initiator" is the peer's role.
	desc, _, err := lnwallet.CommitScriptToRemote(ch.ChanType, !ch.IsInitiator,
		ch.LocalChanCfg.PaymentBasePoint.PubKey, ch.ThawHeight, input.NoneTapLeaf())
	if err != nil {
		return nil
	}
	return desc.PkScript()
}

// filterSource is the part of neutrino's chain service the scan uses.
type filterSource interface {
	BestBlock() (*headerfs.BlockStamp, error)
	GetBlockHash(height int64) (*chainhash.Hash, error)
	GetCFilter(hash chainhash.Hash, filterType wire.FilterType, options ...neutrino.QueryOption) (*gcs.Filter, error)
	GetBlock(hash chainhash.Hash, options ...neutrino.QueryOption) (*btcutil.Block, error)
}

// fundingState is what the scan has learnt about one funding output.
type fundingState struct {
	f        channelFunding
	found    bool // the transaction creating the output was seen
	scriptOK bool // and the output carries the channel's script
	spent    *SpentChannel
	// toUs is the closing transaction's output paying this app, nil when
	// there is none; toUsSpent says somebody spent it already (the phone
	// swept it, or lnd did).
	toUs      *wire.OutPoint
	toUsValue int64
	toUsSpent bool
}

// applyBlock records what one block says about the funding outputs: which
// were created in it and which were spent in it.
func applyBlock(states []*fundingState, block *wire.MsgBlock, height uint32) {
	for _, tx := range block.Transactions {
		var txid *chainhash.Hash
		for _, st := range states {
			if st.found {
				continue
			}
			if txid == nil {
				h := tx.TxHash()
				txid = &h
			}
			if *txid != st.f.outpoint.Hash {
				continue
			}
			st.found = true
			if i := st.f.outpoint.Index; int(i) < len(tx.TxOut) {
				st.scriptOK = bytes.Equal(tx.TxOut[i].PkScript, st.f.pkScript)
			}
		}
		for _, in := range tx.TxIn {
			for _, st := range states {
				if st.toUs != nil && in.PreviousOutPoint == *st.toUs {
					st.toUsSpent = true
				}
				if st.spent == nil && in.PreviousOutPoint == st.f.outpoint {
					hash := tx.TxHash()
					st.spent = &SpentChannel{ChannelPoint: st.f.chanPoint, ClosingTxID: hash.String(), Height: height}
					for i, out := range tx.TxOut {
						if st.f.toUsScript != nil && bytes.Equal(out.PkScript, st.f.toUsScript) {
							st.toUs, st.toUsValue = &wire.OutPoint{Hash: hash, Index: uint32(i)}, out.Value
							break
						}
					}
				}
			}
		}
	}
}

// verdict turns what the scan saw into a verdict. Open needs positive
// proof: the output itself, with the channel's script, and no spend of it.
func (st *fundingState) verdict() channelVerdict {
	switch {
	case st.spent != nil:
		if st.toUs != nil && !st.toUsSpent {
			st.spent.Collect = st.toUsValue
		}
		return channelVerdict{verdict: verdictSpent, spent: *st.spent}
	case !st.found:
		// Not found is not the same as unspent.
		return channelVerdict{verdict: verdictUnverified,
			reason: fmt.Sprintf("funding transaction not found on chain from block %d on", st.f.heightHint)}
	case !st.scriptOK:
		return channelVerdict{verdict: verdictUnverified,
			reason: "the funding output on chain does not match the channel's keys"}
	}
	return channelVerdict{verdict: verdictOpen}
}

// scanFundingOutputs walks the node's compact filters from the oldest
// funding height to the tip and looks into every block whose filter matches
// a funding script: the block that created an output and the block that
// spent it both match. A scan error is returned, never turned into a
// verdict.
//
// It does not use neutrino's GetUtxo. lnd runs its own GetUtxo scans through
// the same scanner, which works one batch at a time: requests that arrive
// while a batch is past their height wait for it to end and then get a pass
// of their own, with no progress in between (seen live 2026-09-20, the
// stage sat silent for minutes). GetUtxo also only looks for the output in
// its start block, so it needs the exact confirmation height, which the
// LSP's early channels do not have.
func scanFundingOutputs(ctx context.Context, chain filterSource, fundings []channelFunding, onBlock func(height, from, tip uint32)) (map[string]channelVerdict, error) {
	out := map[string]channelVerdict{}
	best, err := chain.BestBlock()
	if err != nil {
		return nil, fmt.Errorf("read the chain tip: %w", err)
	}
	tip := uint32(best.Height)

	var states []*fundingState
	for _, f := range fundings {
		// Zero means the channel database has no usable height, and a
		// height past the tip is not a chain height (a zero-conf alias).
		if f.heightHint == 0 || f.heightHint > tip {
			out[f.chanPoint] = channelVerdict{verdict: verdictUnverified,
				reason: fmt.Sprintf("no usable funding height (%d, chain tip %d)", f.heightHint, tip)}
			continue
		}
		states = append(states, &fundingState{f: f})
	}
	if len(states) == 0 {
		return out, nil
	}
	sort.Slice(states, func(i, j int) bool { return states[i].f.heightHint < states[j].f.heightHint })
	from := states[0].f.heightHint

	for h := from; h <= tip; h++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Watch the outputs whose funding height is reached and that are
		// not spent yet.
		var scripts [][]byte
		for _, st := range states {
			switch {
			case st.f.heightHint > h:
			case st.spent == nil:
				scripts = append(scripts, st.f.pkScript)
			case st.toUs != nil && !st.toUsSpent:
				// Closed: keep watching this app's output of the close,
				// to know whether it is still there to collect.
				scripts = append(scripts, st.f.toUsScript)
			}
		}
		if len(scripts) == 0 {
			// Nothing left to watch among the channels reached so far:
			// skip to the next funding.
			next := tip + 1
			for _, st := range states {
				if st.f.heightHint > h && st.f.heightHint < next {
					next = st.f.heightHint
				}
			}
			if next > tip {
				break
			}
			h = next - 1
			continue
		}
		hash, err := chain.GetBlockHash(int64(h))
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", h, err)
		}
		// OptimisticBatch fetches the following filters along with this
		// one, as neutrino's own rescan does. Without it every block is a
		// network round trip: 15 blocks a second against several hundred.
		filter, err := chain.GetCFilter(*hash, wire.GCSFilterRegular, neutrino.OptimisticBatch())
		if err != nil {
			return nil, fmt.Errorf("filter of block %d: %w", h, err)
		}
		if filter == nil {
			return nil, fmt.Errorf("no filter for block %d", h)
		}
		match, err := filter.MatchAny(builder.DeriveKey(hash), scripts)
		if err != nil {
			return nil, fmt.Errorf("filter of block %d: %w", h, err)
		}
		if match {
			block, err := chain.GetBlock(*hash)
			if err != nil {
				return nil, fmt.Errorf("fetch block %d: %w", h, err)
			}
			applyBlock(states, block.MsgBlock(), h)
		}
		if onBlock != nil {
			onBlock(h, from, tip)
		}
		// Blocks found while scanning are part of the answer.
		if h == tip {
			if best, err := chain.BestBlock(); err == nil && uint32(best.Height) > tip {
				tip = uint32(best.Height)
			}
		}
	}
	for _, st := range states {
		out[st.f.chanPoint] = st.verdict()
	}
	return out, nil
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
			c.progressf("  %s belongs to another node.", rpcChan.ChannelPoint)
			continue
		}
		script, err := fundingPkScript(ch)
		if err != nil {
			return nil, fmt.Errorf("funding script of %s: %w", rpcChan.ChannelPoint, err)
		}
		fundings = append(fundings, channelFunding{
			chanPoint:  rpcChan.ChannelPoint,
			outpoint:   ch.FundingOutpoint,
			pkScript:   script,
			heightHint: fundingHeightHint(ch),
			toUsScript: toUsScript(ch),
		})
	}

	if len(fundings) > 0 {
		c.progressf("Checking %d channel(s) on chain...", len(fundings))
		// The stage is on screen from the first moment and keeps moving:
		// an update twice a second, whatever the scan's speed.
		started := time.Now()
		report := func(height, from, tip uint32) {
			if onProgress == nil {
				return
			}
			p := SyncProgress{Stage: "channels", Height: height, Target: tip, Percent: 0, Remaining: -1,
				Message: "Making sure your channels are still open"}
			if tip > from && height >= from {
				p.Percent = 100 * float64(height-from) / float64(tip-from)
			}
			// Time left from the rate so far, once there is a rate to speak of.
			if elapsed := time.Since(started).Seconds(); elapsed >= 10 && height > from && tip >= height {
				p.Remaining = int64(float64(tip-height) / (float64(height-from) / elapsed))
			}
			onProgress(p)
		}
		if onProgress != nil {
			onProgress(SyncProgress{Stage: "channels", Percent: 0, Remaining: -1,
				Message: "Making sure your channels are still open"})
		}
		var lastReport time.Time
		scanned, err := scanFundingOutputs(ctx, chain, fundings, func(height, from, tip uint32) {
			if height == tip || time.Since(lastReport) >= 500*time.Millisecond {
				lastReport = time.Now()
				report(height, from, tip)
			}
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
			c.progressf("  %s closed on chain, tx %s.", rpcChan.ChannelPoint, v.spent.ClosingTxID)
			if v.spent.Collect > 0 {
				c.progressf("  Collecting %d sat from this close.", v.spent.Collect)
			}
		case verdictUnverified:
			c.progressf("  %s not confirmed on chain: %s.", rpcChan.ChannelPoint, v.reason)
		}
	}
	c.checks.set(verdicts)
	if len(closed) > 0 {
		c.progressf("%d channel(s) closed after this backup.", len(closed))
	}
	return closed, nil
}
