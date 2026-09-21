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
	// ToUs is the outpoint Collect sits on, "" when Collect is 0.
	ToUs string `json:"toUs,omitempty"`
	// PayoutKnown says the walk saw what the close paid this app and follows
	// that output itself. When false the amount is lnd's to report.
	PayoutKnown bool `json:"payoutKnown"`
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

// spent lists the channels found closed on chain, in a stable order.
func (s *channelChecks) spent() []SpentChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SpentChannel
	for _, v := range s.verdicts {
		if v.verdict == verdictSpent {
			out = append(out, v.spent)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelPoint < out[j].ChannelPoint })
	return out
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

// isForeign reports whether the channel belongs to another node: the wallet
// must derive the channel's funding key at the channel's own key locator. A
// backup can pair one node's wallet with another node's channels (seen in a
// real Breez backup from 2022); nothing of such a channel is this app's.
func isForeign(ctx context.Context, wk walletrpc.WalletKitClient, ch *channeldb.OpenChannel) (bool, error) {
	key := ch.LocalChanCfg.MultiSigKey
	res, err := wk.DeriveKey(ctx, &signrpc.KeyLocator{KeyFamily: int32(key.Family), KeyIndex: int32(key.Index)})
	if err != nil {
		return false, fmt.Errorf("derive the key of channel %s: %w", ch.FundingOutpoint, err)
	}
	return !bytes.Equal(res.RawKeyBytes, key.PubKey.SerializeCompressed()), nil
}

// fundingOf is what the chain walk needs to know about a channel.
func fundingOf(ch *channeldb.OpenChannel) (channelFunding, error) {
	script, err := fundingPkScript(ch)
	if err != nil {
		return channelFunding{}, fmt.Errorf("funding script of %s: %w", ch.FundingOutpoint, err)
	}
	return channelFunding{
		chanPoint:  ch.FundingOutpoint.String(),
		outpoint:   ch.FundingOutpoint,
		pkScript:   script,
		heightHint: fundingHeightHint(ch),
		toUsScript: toUsScript(ch),
	}, nil
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
		// Computed anew each time: a walk that is carried on may find the
		// output spent later.
		st.spent.Collect, st.spent.ToUs = 0, ""
		// Known only when the close shows this app's output of the peer's
		// commitment. Without one the close may be the app's own commitment
		// (a delayed output), a cooperative close, or one that paid nothing:
		// lnd has to say.
		st.spent.PayoutKnown = st.toUs != nil
		if st.toUs != nil && !st.toUsSpent {
			st.spent.Collect, st.spent.ToUs = st.toUsValue, st.toUs.String()
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

// chainWalk walks the node's compact filters from the oldest height anything
// is watched at to the tip and looks into every block whose filter matches a
// watched script: the block that created a funding output and the block that
// spent it both match, and so does a block paying one of the wallet's next
// addresses (watch, nil when the address search is done). It can be run
// again later and then carries on from where it stopped.
//
// It does not use neutrino's GetUtxo. lnd runs its own GetUtxo scans through
// the same scanner, which works one batch at a time: requests that arrive
// while a batch is past their height wait for it to end and then get a pass
// of their own, with no progress in between (seen live 2026-09-20, the
// stage sat silent for minutes). GetUtxo also only looks for the output in
// its start block, so it needs the exact confirmation height, which the
// LSP's early channels do not have.
type chainWalk struct {
	states []*fundingState
	// unusable holds the verdicts of channels without a height to look from.
	unusable map[string]channelVerdict
	watch    *addressWatch
	next     uint32 // first block not walked yet, 0 before the first run
	all      []channelFunding
	// floor is the first block whose filter the node can fetch: the block
	// after the first one it has (see walkStart). Nothing is looked for
	// below it, and the block before it is fetched directly. 0 when the
	// walk was not told (then every height is taken as it is).
	floor uint32
}

// newChainWalk prepares a walk. A channel without any height is set aside
// when the walk has no floor to start it from. A height past the tip (a
// zero-conf alias, or a channel newer than the headers at hand) is simply
// never reached, and looked at again on every later run.
func newChainWalk(fundings []channelFunding, watch *addressWatch, floor uint32) *chainWalk {
	w := &chainWalk{unusable: map[string]channelVerdict{}, watch: watch, all: fundings, floor: floor}
	for _, f := range fundings {
		if f.heightHint == 0 && floor == 0 {
			w.unusable[f.chanPoint] = channelVerdict{verdict: verdictUnverified, reason: "no usable funding height"}
			continue
		}
		w.states = append(w.states, &fundingState{f: f})
	}
	sort.Slice(w.states, func(i, j int) bool { return w.states[i].f.heightHint < w.states[j].f.heightHint })
	return w
}

// start is the block a channel is looked for from. A channel of another
// node, which a mixed-up backup can carry, may be older than this node's
// first block: its funding cannot be seen then (it stays unverified, never
// open), but its close still can.
func (w *chainWalk) start(st *fundingState) uint32 {
	if st.f.heightHint < w.floor {
		return w.floor
	}
	return st.f.heightHint
}

// fundings are all channels the walk was made for.
func (w *chainWalk) fundings() []channelFunding {
	out := append([]channelFunding{}, w.all...)
	return out
}

// has reports whether the walk looks after the channel.
func (w *chainWalk) has(chanPoint string) bool {
	if _, ok := w.unusable[chanPoint]; ok {
		return true
	}
	for _, st := range w.states {
		if st.f.chanPoint == chanPoint {
			return true
		}
	}
	return false
}

func (w *chainWalk) verdicts() map[string]channelVerdict {
	out := map[string]channelVerdict{}
	for point, v := range w.unusable {
		out[point] = v
	}
	for _, st := range w.states {
		out[st.f.chanPoint] = st.verdict()
	}
	return out
}

// run walks to the chain tip. A scan error is returned, never turned into
// a verdict.
func (w *chainWalk) run(ctx context.Context, chain filterSource, onBlock func(height, from, tip uint32)) error {
	best, err := chain.BestBlock()
	if err != nil {
		return fmt.Errorf("read the chain tip: %w", err)
	}
	tip := uint32(best.Height)

	// nextStart is the lowest height above h something starts being
	// watched at, past the tip when there is none.
	nextStart := func(h uint32) uint32 {
		next := tip + 1
		for _, st := range w.states {
			if at := w.start(st); at > h && at < next {
				next = at
			}
		}
		if w.watch != nil {
			at := w.watch.from
			if at < w.floor {
				at = w.floor
			}
			if at > h && at < next {
				next = at
			}
		}
		return next
	}
	from := w.next
	if from == 0 {
		if from = nextStart(0); from > tip {
			return nil
		}
		// The block before the floor has no filter to fetch (see
		// walkStart) but may matter: it is one block, look into it.
		if w.floor > 1 && from == w.floor {
			if err := w.open(ctx, chain, w.floor-1); err != nil {
				return err
			}
		}
	}

	for h := from; h <= tip; h++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Watch the outputs whose funding height is reached and that are
		// not spent yet.
		var scripts [][]byte
		for _, st := range w.states {
			switch {
			case w.start(st) > h:
			case st.spent == nil:
				scripts = append(scripts, st.f.pkScript)
			case st.toUs != nil && !st.toUsSpent:
				// Closed: keep watching this app's output of the close,
				// to know whether it is still there to collect.
				scripts = append(scripts, st.f.toUsScript)
			}
		}
		if w.watch != nil {
			scripts = append(scripts, w.watch.scriptsAt(h)...)
		}
		if len(scripts) == 0 {
			// Nothing left to watch among what was reached so far: skip
			// to where the next thing starts.
			next := nextStart(h)
			if next > tip {
				break
			}
			h = next - 1
			continue
		}
		hash, err := chain.GetBlockHash(int64(h))
		if err != nil {
			return fmt.Errorf("block %d: %w", h, err)
		}
		// OptimisticBatch fetches the following filters along with this
		// one, as neutrino's own rescan does. Without it every block is a
		// network round trip: 15 blocks a second against several hundred.
		var filter *gcs.Filter
		err = patiently(ctx, fmt.Sprintf("filter of block %d", h), func() (err error) {
			filter, err = chain.GetCFilter(*hash, wire.GCSFilterRegular, neutrino.OptimisticBatch())
			if err == nil && filter == nil {
				err = errors.New("no filter")
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("filter of block %d: %w", h, err)
		}
		match, err := filter.MatchAny(builder.DeriveKey(hash), scripts)
		if err != nil {
			return fmt.Errorf("filter of block %d: %w", h, err)
		}
		if match {
			if err := w.open(ctx, chain, h); err != nil {
				return err
			}
		}
		if onBlock != nil {
			// A pass over addresses that only need the blocks before they
			// joined ends there, not at the tip.
			target := tip
			if len(w.states) == 0 && w.watch != nil {
				if end := w.watch.end(); end != 0 && end <= tip {
					target = end - 1
				}
			}
			onBlock(h, from, target)
		}
		// Blocks found while scanning are part of the answer.
		if h == tip {
			if best, err := chain.BestBlock(); err == nil && uint32(best.Height) > tip {
				tip = uint32(best.Height)
			}
		}
	}
	w.next = tip + 1
	return nil
}

// fetchPatience is how long the walk keeps asking the network for one
// header, filter or block before it gives up. A laptop that slept, a Wi-Fi
// that dropped: the first request afterwards times out, and a walk of many
// minutes must not end, and start over, for that (seen 2026-09-21: five
// runs lost to one closed lid).
var fetchPatience = 15 * time.Minute

// patiently runs a network request until it succeeds, the context ends or
// fetchPatience has passed since its first failure.
func patiently(ctx context.Context, what string, request func() error) error {
	var since time.Time
	wait := 2 * time.Second
	for {
		err := request()
		if err == nil {
			if !since.IsZero() {
				nodeLog(fmt.Sprintf("[chain] %s: the network answers again", what))
			}
			return nil
		}
		if since.IsZero() {
			since = time.Now()
			if fetchPatience > 0 {
				nodeLog(fmt.Sprintf("[chain] %s: %v; asking again for up to %s", what, err, fetchPatience))
			}
		}
		if time.Since(since) >= fetchPatience {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

// open fetches a block and records what it says about the channels and the
// wallet's addresses.
func (w *chainWalk) open(ctx context.Context, chain filterSource, h uint32) error {
	hash, err := chain.GetBlockHash(int64(h))
	if err != nil {
		return fmt.Errorf("block %d: %w", h, err)
	}
	var block *btcutil.Block
	err = patiently(ctx, fmt.Sprintf("block %d", h), func() (err error) {
		block, err = chain.GetBlock(*hash)
		return err
	})
	if err != nil {
		return fmt.Errorf("fetch block %d: %w", h, err)
	}
	msg := block.MsgBlock()
	open := map[*fundingState]bool{}
	for _, st := range w.states {
		open[st] = st.spent == nil
	}
	applyBlock(w.states, msg, h)
	if w.watch == nil {
		return nil
	}
	// A channel closed in this block: from here on the search follows
	// every output of the closing transaction.
	for _, st := range w.states {
		if open[st] && st.spent != nil {
			for _, tx := range msg.Transactions {
				if tx.TxHash().String() == st.spent.ClosingTxID {
					w.watch.follow(tx, 0)
				}
			}
		}
	}
	w.watch.applyBlock(msg, h)
	return nil
}

// scanFundingOutputs is one walk over the channels alone.
func scanFundingOutputs(ctx context.Context, chain filterSource, fundings []channelFunding, onBlock func(height, from, tip uint32)) (map[string]channelVerdict, error) {
	w := newChainWalk(fundings, nil, 0)
	if err := w.run(ctx, chain, onBlock); err != nil {
		return nil, err
	}
	return w.verdicts(), nil
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
	if len(chans) == 0 && c.walk == nil && len(savedFundings(c.dir())) == 0 {
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
		foreign, err := isForeign(ctx, wk, ch)
		if err != nil {
			return nil, err
		}
		if foreign {
			verdicts[rpcChan.ChannelPoint] = channelVerdict{verdict: verdictForeign}
			c.progressf("  %s belongs to another node.", rpcChan.ChannelPoint)
			continue
		}
		f, err := fundingOf(ch)
		if err != nil {
			return nil, err
		}
		fundings = append(fundings, f)
	}

	// Channels an earlier walk covered and lnd no longer lists as open: lnd
	// has seen their close and is collecting their funds. The walk keeps
	// following them, so what is still to collect does not depend on lnd's
	// pending list (which fails on some old nodes).
	open := map[string]bool{}
	for _, f := range fundings {
		open[f.chanPoint] = true
	}
	var earlier []channelFunding
	if c.walk != nil {
		earlier = c.walk.fundings()
	} else {
		earlier = savedFundings(c.dir())
	}
	for _, f := range earlier {
		if _, listed := verdicts[f.chanPoint]; !listed && !open[f.chanPoint] {
			fundings = append(fundings, f)
		}
	}
	if len(fundings) > 0 {
		c.progressf("Checking %d channel(s) on chain...", len(fundings))
		// An earlier walk, of this run or saved by an earlier one, covers
		// these channels up to the block it ended at: carry on from there.
		walk := c.walk
		for _, f := range fundings {
			if walk != nil && !walk.has(f.chanPoint) {
				walk = nil
			}
		}
		if walk == nil {
			var why string
			if walk, why = loadWalk(c.dir(), chain, fundings); why != "" {
				c.progressf("The saved chain check is not used (%s); checking from the start.", why)
			}
		}
		if walk == nil {
			best, err := chain.BestBlock()
			if err != nil {
				return nil, fmt.Errorf("read the chain tip: %w", err)
			}
			first, err := firstBlock(chain.BlockHeaders.FetchHeaderByHeight, uint32(best.Height))
			if err != nil {
				return nil, err
			}
			walk = newChainWalk(fundings, nil, first+1)
		}
		if err := walk.run(ctx, chain, walkProgress("channels", "Making sure your channels are still open", onProgress)); err != nil {
			return nil, err
		}
		c.walk = walk
		if err := walk.save(c.dir(), chain, walk.fundings()); err != nil {
			return nil, fmt.Errorf("save the chain check: %w", err)
		}
		scanned := walk.verdicts()
		for _, f := range fundings {
			verdicts[f.chanPoint] = scanned[f.chanPoint]
		}
	}

	var closed []SpentChannel
	for _, f := range fundings {
		switch v := verdicts[f.chanPoint]; v.verdict {
		case verdictSpent:
			closed = append(closed, v.spent)
			c.progressf("  %s closed on chain, tx %s.", f.chanPoint, v.spent.ClosingTxID)
			if v.spent.Collect > 0 {
				c.progressf("  Collecting %d sat from this close.", v.spent.Collect)
			}
		case verdictUnverified:
			c.progressf("  %s not confirmed on chain: %s.", f.chanPoint, v.reason)
		}
	}
	c.checks.set(verdicts)
	if len(closed) > 0 {
		c.progressf("%d channel(s) closed after this backup.", len(closed))
	}
	return closed, nil
}

// walkProgress reports a walk as a sync stage. The stage is on screen from
// the first moment and keeps moving: an update twice a second, whatever the
// walk's speed.
func walkProgress(stage, message string, onProgress func(SyncProgress)) func(height, from, tip uint32) {
	if onProgress == nil {
		return nil
	}
	onProgress(SyncProgress{Stage: stage, Percent: 0, Remaining: -1, Message: message})
	started := time.Now()
	var lastReport time.Time
	return func(height, from, tip uint32) {
		if height != tip && time.Since(lastReport) < 500*time.Millisecond {
			return
		}
		lastReport = time.Now()
		p := SyncProgress{Stage: stage, Height: height, Target: tip, Percent: 0, Remaining: -1, Message: message}
		if tip > from && height >= from {
			p.Percent = 100 * float64(height-from) / float64(tip-from)
		}
		// Time left from the rate so far, once there is a rate to speak of.
		if elapsed := time.Since(started).Seconds(); elapsed >= 10 && height > from && tip >= height {
			p.Remaining = int64(float64(tip-height) / (float64(height-from) / elapsed))
		}
		onProgress(p)
	}
}
