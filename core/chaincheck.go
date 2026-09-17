package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/breez/breez/chainservice"
	"github.com/breez/breez/channeldbservice"
	breezdb "github.com/breez/breez/db"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
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

// spentChannels holds the result of the last check, keyed by channel point.
type spentChannels struct {
	mu      sync.Mutex
	checked bool
	spent   map[string]SpentChannel
	// foreign holds channels whose keys this wallet cannot derive.
	foreign map[string]bool
}

func (s *spentChannels) isForeign(chanPoint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign[chanPoint]
}

func (s *spentChannels) setForeign(m map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.foreign = m
}

func (s *spentChannels) get(chanPoint string) (SpentChannel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.spent[chanPoint]
	return c, ok
}

func (s *spentChannels) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checked
}

func (s *spentChannels) set(m map[string]SpentChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spent, s.checked = m, true
}

// CheckChannelsOnChain asks the node's own chain service, for every channel
// lnd lists as open, whether the funding output is still unspent. A backup
// is a snapshot: channels that closed after it was taken still look open,
// and their balances are not there any more. Closing such a channel
// broadcasts a commitment for an output that no longer exists, so the app
// refuses to and reports them as closed.
//
// The answer comes from the node's own filters, nothing is asked of a
// third party. A failure is returned, never swallowed: the close path
// depends on this being right.
func (c *Core) CheckChannelsOnChain(ctx context.Context) ([]SpentChannel, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	chans, err := c.node.openChannels(ctx)
	if err != nil {
		return nil, err
	}
	if len(chans) == 0 {
		c.spent.set(map[string]SpentChannel{})
		return nil, nil
	}
	chandb, releaseDB, err := channeldbservice.Get(c.cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("open the channel database: %w", err)
	}
	defer releaseDB()
	bdb, releaseBreezDB, err := breezdb.Get(c.cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("open the app database: %w", err)
	}
	defer releaseBreezDB()
	chain, releaseChain, err := chainservice.Get(c.cfg.WorkDir, bdb)
	if err != nil {
		return nil, fmt.Errorf("reach the bitcoin network: %w", err)
	}
	defer releaseChain()

	open, err := chandb.ChannelStateDB().FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}
	byPoint := map[string]*channelFunding{}
	for _, ch := range open {
		_, script, err := input.GenFundingPkScript(
			ch.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
			ch.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
			int64(ch.Capacity),
		)
		if err != nil {
			return nil, fmt.Errorf("funding script of %s: %w", ch.FundingOutpoint, err)
		}
		byPoint[ch.FundingOutpoint.String()] = &channelFunding{
			outpoint:  ch.FundingOutpoint,
			pkScript:  script.PkScript,
			openBlock: ch.ShortChannelID.BlockHeight,
		}
	}

	// A backup can pair one node's wallet with another node's channel
	// database (seen in a real Breez backup from 2022). Such a channel is
	// not this wallet's: it cannot sign for it, so it is not funds and is
	// never closed.
	foreign := map[string]bool{}
	wk := walletrpc.NewWalletKitClient(c.node.conn)
	for _, ch := range open {
		loc := ch.LocalChanCfg.MultiSigKey.KeyLocator
		res, err := wk.DeriveKey(ctx, &signrpc.KeyLocator{KeyFamily: int32(loc.Family), KeyIndex: int32(loc.Index)})
		if err != nil {
			return nil, fmt.Errorf("derive the key of channel %s: %w", ch.FundingOutpoint, err)
		}
		if !bytes.Equal(res.RawKeyBytes, ch.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed()) {
			foreign[ch.FundingOutpoint.String()] = true
			c.progressf("  %s belongs to another node: this backup's app cannot sign for it.", ch.FundingOutpoint)
		}
	}
	c.spent.setForeign(foreign)

	c.progressf("Checking on chain that your %d channel(s) are still open...", len(chans))
	// One scan covers every outpoint: neutrino batches the requests that
	// are in flight together, so they are asked for at the same time.
	type result struct {
		channel string
		spent   *SpentChannel
		err     error
	}
	results := make(chan result, len(chans))
	pending := 0
	for _, ch := range chans {
		if foreign[ch.ChannelPoint] {
			continue
		}
		pending++
		f := byPoint[ch.ChannelPoint]
		if f == nil {
			return nil, fmt.Errorf("channel %s is open in lnd but not in its database", ch.ChannelPoint)
		}
		go func(chanPoint string, f *channelFunding) {
			report, err := chain.GetUtxo(
				neutrino.WatchInputs(neutrino.InputWithScript{OutPoint: f.outpoint, PkScript: f.pkScript}),
				neutrino.StartBlock(&headerfs.BlockStamp{Height: int32(f.openBlock)}),
				neutrino.QuitChan(ctx.Done()),
			)
			switch {
			case err != nil:
				results <- result{channel: chanPoint, err: err}
			case report != nil && report.SpendingTx != nil:
				results <- result{channel: chanPoint, spent: &SpentChannel{
					ChannelPoint: chanPoint,
					ClosingTxID:  report.SpendingTx.TxHash().String(),
					Height:       report.SpendingTxHeight,
				}}
			default:
				results <- result{channel: chanPoint}
			}
		}(ch.ChannelPoint, f)
	}
	spent := map[string]SpentChannel{}
	var firstErr error
	for i := 0; i < pending; i++ {
		r := <-results
		switch {
		case r.err != nil && firstErr == nil:
			firstErr = fmt.Errorf("check channel %s on chain: %w", r.channel, r.err)
		case r.spent != nil:
			spent[r.channel] = *r.spent
			c.progressf("  %s closed on chain already, in transaction %s.", r.channel, r.spent.ClosingTxID)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	c.spent.set(spent)
	out := make([]SpentChannel, 0, len(spent))
	for _, s := range spent {
		out = append(out, s)
	}
	if len(out) > 0 {
		c.progressf("%d of your channels closed after this backup was taken; their funds are not in the app.", len(out))
	}
	return out, nil
}

type channelFunding struct {
	outpoint  wire.OutPoint
	pkScript  []byte
	openBlock uint32
}

// liveChannels splits what lnd reports as open into channels that are
// really open and channels whose funding output is already spent.
func (c *Core) liveChannels(chans []*lnrpc.Channel) (live []*lnrpc.Channel, gone []SpentChannel) {
	for _, ch := range chans {
		if s, ok := c.spent.get(ch.ChannelPoint); ok {
			gone = append(gone, s)
			continue
		}
		if c.spent.isForeign(ch.ChannelPoint) {
			continue
		}
		live = append(live, ch)
	}
	return live, gone
}
