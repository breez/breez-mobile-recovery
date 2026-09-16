package core

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// Sweep is money leaving a closed channel: the transaction that spent a
// channel output and where it paid.
type Sweep struct {
	TxID       string `json:"txid"`
	Amount     int64  `json:"amount"`
	Address    string `json:"address"`
	ToThisNode bool   `json:"toThisNode"` // paid to an address of this node's own wallet
	Height     int32  `json:"height"`
	Time       int64  `json:"time"`
}

// ClosedChannel is a channel that is closed, with what happened to its funds.
type ClosedChannel struct {
	ChannelPoint   string  `json:"channelPoint"`
	Peer           string  `json:"peer"`
	Capacity       int64   `json:"capacity"`
	SettledBalance int64   `json:"settledBalance"` // our share paid out directly by the close
	CloseType      string  `json:"closeType"`      // cooperative, local force, remote force, breach, funding cancelled, abandoned
	CloseHeight    uint32  `json:"closeHeight"`
	ClosingTxID    string  `json:"closingTxid"`
	Sweeps         []Sweep `json:"sweeps"`
}

// OnchainTx is one of the node's on-chain transactions.
type OnchainTx struct {
	TxID    string     `json:"txid"`
	Amount  int64      `json:"amount"` // net effect on this node's wallet
	Fee     int64      `json:"fee"`
	Height  int32      `json:"height"`
	Time    int64      `json:"time"`
	Outputs []TxOutput `json:"outputs"`
	Label   string     `json:"label"`
}

// TxOutput is one output of an on-chain transaction.
type TxOutput struct {
	Address string `json:"address"`
	Amount  int64  `json:"amount"`
	Ours    bool   `json:"ours"`
}

// History is the node's channel closes and on-chain transactions.
type History struct {
	Channels     []ClosedChannel `json:"channels"`
	Transactions []OnchainTx     `json:"transactions"`
}

func closeTypeName(t lnrpc.ChannelCloseSummary_ClosureType) string {
	switch t {
	case lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE:
		return "cooperative"
	case lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE:
		return "local force"
	case lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE:
		return "remote force"
	case lnrpc.ChannelCloseSummary_BREACH_CLOSE:
		return "breach"
	case lnrpc.ChannelCloseSummary_FUNDING_CANCELED:
		return "funding cancelled"
	case lnrpc.ChannelCloseSummary_ABANDONED:
		return "abandoned"
	}
	return "unknown"
}

// History lists closed channels and on-chain transactions. Sweeps are
// linked to channels through lnd's resolutions and, for spends lnd did
// not make itself (the phone did, or a peer), through transactions that
// spend an output of the closing transaction.
func (c *Core) History(ctx context.Context) (*History, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	closed, err := c.node.client.ClosedChannels(cctx, &lnrpc.ClosedChannelsRequest{})
	if err != nil {
		return nil, err
	}
	txsRes, err := c.node.client.GetTransactions(cctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return nil, err
	}

	byID := map[string]*lnrpc.Transaction{}
	spenders := map[string][]*lnrpc.Transaction{} // closing txid -> txs spending one of its outputs
	for _, tx := range txsRes.Transactions {
		byID[tx.TxHash] = tx
		for _, prev := range tx.PreviousOutpoints {
			if i := strings.Index(prev.Outpoint, ":"); i > 0 {
				spenders[prev.Outpoint[:i]] = append(spenders[prev.Outpoint[:i]], tx)
			}
		}
	}

	h := &History{Channels: []ClosedChannel{}, Transactions: []OnchainTx{}}
	for _, ch := range closed.Channels {
		cc := ClosedChannel{
			ChannelPoint:   ch.ChannelPoint,
			Peer:           ch.RemotePubkey,
			Capacity:       ch.Capacity,
			SettledBalance: ch.SettledBalance,
			CloseType:      closeTypeName(ch.CloseType),
			CloseHeight:    ch.CloseHeight,
			ClosingTxID:    ch.ClosingTxHash,
			Sweeps:         []Sweep{},
		}
		seen := map[string]bool{}
		add := func(tx *lnrpc.Transaction) {
			if tx == nil || seen[tx.TxHash] {
				return
			}
			seen[tx.TxHash] = true
			for _, o := range tx.OutputDetails {
				cc.Sweeps = append(cc.Sweeps, Sweep{TxID: tx.TxHash, Amount: o.Amount, Address: o.Address, ToThisNode: o.IsOurAddress, Height: tx.BlockHeight, Time: tx.TimeStamp})
			}
		}
		for _, r := range ch.Resolutions {
			if r.SweepTxid != "" {
				add(byID[r.SweepTxid])
			}
		}
		for _, tx := range spenders[ch.ClosingTxHash] {
			add(tx)
		}
		h.Channels = append(h.Channels, cc)
	}
	sort.Slice(h.Channels, func(i, j int) bool { return h.Channels[i].CloseHeight > h.Channels[j].CloseHeight })

	for _, tx := range txsRes.Transactions {
		t := OnchainTx{TxID: tx.TxHash, Amount: tx.Amount, Fee: tx.TotalFees, Height: tx.BlockHeight, Time: tx.TimeStamp, Label: tx.Label, Outputs: []TxOutput{}}
		for _, o := range tx.OutputDetails {
			t.Outputs = append(t.Outputs, TxOutput{Address: o.Address, Amount: o.Amount, Ours: o.IsOurAddress})
		}
		h.Transactions = append(h.Transactions, t)
	}
	sort.Slice(h.Transactions, func(i, j int) bool { return h.Transactions[i].Time > h.Transactions[j].Time })
	return h, nil
}
