package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/breez/breez/bindings"
	"github.com/breez/breez/data"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"google.golang.org/protobuf/proto"
)

// Entry kinds. Every kind is either money entering or leaving the app
// (Delta != 0) or a move between the app's Lightning and on-chain
// balances (Delta == 0, only the fee is lost).
const (
	KindReceived     = "received"      // Lightning payment received
	KindSent         = "sent"          // Lightning payment sent
	KindDeposit      = "deposit"       // bitcoin swapped into a channel
	KindRefund       = "refund"        // a deposit that never made it into a channel, refunded
	KindWithdrawal   = "withdrawal"    // channel funds swapped out to an address
	KindChannelClose = "channel_close" // channel closed, its balance set aside on-chain
	KindCollected    = "collected"     // closed-channel funds moved into the on-chain balance
	KindOnchainIn    = "onchain_in"    // on-chain funds received from outside
	KindOnchainOut   = "onchain_out"   // on-chain funds sent to an outside address
	KindOnchainSelf  = "onchain_self"  // on-chain funds moved between the app's own addresses
)

// Entry is one line of the ledger.
type Entry struct {
	Time   int64  `json:"time"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`  // short, for the list ("Received", "Channel closed")
	Detail string `json:"detail"` // description, payee, or what happened, in plain words
	// Amount is the size of the movement, always positive. Delta is the net
	// effect on the funds the app holds, negative for money leaving; it is
	// zero for moves between the app's own balances. Fee is what the
	// movement cost. For outgoing entries Delta already includes the fee.
	Amount  int64  `json:"amount"`
	Delta   int64  `json:"delta"`
	Fee     int64  `json:"fee"`
	FeeNote string `json:"feeNote,omitempty"` // set when the fee was taken before the money reached the app
	Status  string `json:"status"`            // "done", "pending", "closing", "unconfirmed"
	Note    string `json:"note,omitempty"`    // extra line, e.g. when locked funds unlock
	TxID    string `json:"txid,omitempty"`
	Address string `json:"address,omitempty"`
}

// Totals reconcile the ledger with the balances the node reports now.
type Totals struct {
	In          int64 `json:"in"`          // money that entered the app
	Out         int64 `json:"out"`         // money that left the app, fees excluded
	Fees        int64 `json:"fees"`        // fees paid from the app's funds
	Expected    int64 `json:"expected"`    // In - Out - Fees
	Onchain     int64 `json:"onchain"`     // confirmed + unconfirmed on-chain balance now
	InChannels  int64 `json:"inChannels"`  // local balance of open channels now
	InPending   int64 `json:"inPending"`   // balance of channels still closing
	Held        int64 `json:"held"`        // Onchain + InChannels + InPending
	Uncollected int64 `json:"uncollected"` // set aside by channel closes, not yet in the on-chain balance
	Unexplained int64 `json:"unexplained"` // Held + Uncollected - Expected
}

// History is the app's money movements, newest first, with totals.
type History struct {
	Entries []Entry `json:"entries"`
	Totals  Totals  `json:"totals"`
	// ZeroCloses counts channels that closed with no balance of the user's.
	ZeroCloses int `json:"zeroCloses"`
	// Warnings lists parts of the history that could not be read.
	Warnings []string `json:"warnings"`
}

// RawPayments returns the mobile app's payment list as it is stored, for
// debugging the ledger.
func (c *Core) RawPayments() ([]*data.Payment, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	raw, err := bindings.GetPayments()
	if err != nil {
		return nil, fmt.Errorf("read the payment list: %w", err)
	}
	var list data.PaymentsList
	if err := proto.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode the payment list: %w", err)
	}
	return list.PaymentsList, nil
}

// LndTotals sums what lnd itself recorded: settled invoices and succeeded
// payments. Used to check the mobile app's payment list against the node.
type LndTotals struct {
	InvoicesSkipped    int      `json:"invoicesSkipped"` // invoices lnd could not serialise
	InvoicesSettled    int      `json:"invoicesSettled"`
	InvoicesSettledSat int64    `json:"invoicesSettledSat"`
	PaymentsSucceeded  int      `json:"paymentsSucceeded"`
	PaymentsValueSat   int64    `json:"paymentsValueSat"`
	PaymentsFeeSat     int64    `json:"paymentsFeeSat"`
	InvoicesNotInApp   []string `json:"invoicesNotInApp"` // settled invoices missing from the app's list, "hash amount date"
	PaymentsNotInApp   []string `json:"paymentsNotInApp"` // succeeded payments missing from the app's list
	AppNotInLnd        []string `json:"appNotInLnd"`      // app entries lnd has no record of
}

// walkLnd visits every settled invoice and succeeded payment lnd holds.
// Old invoices can carry memos that are not valid UTF-8, which lnd cannot
// serialise; the invoice pages narrow down to skip just those, and the
// count of skipped invoices is returned.
func (n *node) walkLnd(ctx context.Context, onInvoice func(*lnrpc.Invoice), onPayment func(*lnrpc.Payment)) (skipped int, err error) {
	var offset uint64
	pageSize := uint64(1000)
	for {
		res, err := n.client.ListInvoices(ctx, &lnrpc.ListInvoiceRequest{IndexOffset: offset, NumMaxInvoices: pageSize})
		if err != nil {
			if strings.Contains(err.Error(), "UTF-8") {
				if pageSize > 1 {
					pageSize /= 2
					continue
				}
				skipped++
				offset++
				pageSize = 1000
				continue
			}
			return skipped, err
		}
		for _, inv := range res.Invoices {
			if inv.State == lnrpc.Invoice_SETTLED {
				onInvoice(inv)
			}
		}
		if len(res.Invoices) == 0 || res.LastIndexOffset == offset {
			break
		}
		offset = res.LastIndexOffset
		pageSize = 1000
	}
	offset = 0
	for {
		res, err := n.client.ListPayments(ctx, &lnrpc.ListPaymentsRequest{IndexOffset: offset, MaxPayments: 1000})
		if err != nil {
			return skipped, err
		}
		for _, p := range res.Payments {
			if p.Status == lnrpc.Payment_SUCCEEDED {
				onPayment(p)
			}
		}
		if len(res.Payments) == 0 || res.LastIndexOffset == offset {
			break
		}
		offset = res.LastIndexOffset
	}
	return skipped, nil
}

func appHashes(payments []*data.Payment) map[string]bool {
	m := map[string]bool{}
	for _, p := range payments {
		m[p.PaymentHash] = true
	}
	return m
}

// lndOnlyPayments lists the payments and receipts lnd recorded that the
// app's own list lacks.
func (c *Core) lndOnlyPayments(ctx context.Context, payments []*data.Payment) ([]Entry, error) {
	known := appHashes(payments)
	var out []Entry
	_, err := c.node.walkLnd(ctx, func(inv *lnrpc.Invoice) {
		hash := fmt.Sprintf("%x", inv.RHash)
		if known[hash] {
			return
		}
		out = append(out, Entry{Time: inv.SettleDate, Kind: KindReceived, Title: "Received", Detail: firstOf(strings.TrimSpace(inv.Memo), "Lightning payment, no description"), Amount: inv.AmtPaidSat, Delta: inv.AmtPaidSat, Status: "done"})
	}, func(p *lnrpc.Payment) {
		if known[p.PaymentHash] {
			return
		}
		out = append(out, Entry{Time: p.CreationDate, Kind: KindSent, Title: "Sent", Detail: "Lightning payment, no description", Amount: p.ValueSat, Delta: -(p.ValueSat + p.FeeSat), Fee: p.FeeSat, Status: "done"})
	})
	return out, err
}

// LndTotals compares lnd's invoice and payment records with the app's list.
func (c *Core) LndTotals(ctx context.Context, payments []*data.Payment) (*LndTotals, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	t := &LndTotals{InvoicesNotInApp: []string{}, PaymentsNotInApp: []string{}, AppNotInLnd: []string{}}
	known := appHashes(payments)
	lndHashes := map[string]bool{}
	skipped, err := c.node.walkLnd(ctx, func(inv *lnrpc.Invoice) {
		hash := fmt.Sprintf("%x", inv.RHash)
		lndHashes[hash] = true
		t.InvoicesSettled++
		t.InvoicesSettledSat += inv.AmtPaidSat
		if !known[hash] {
			t.InvoicesNotInApp = append(t.InvoicesNotInApp, fmt.Sprintf("%s %d %s", hash, inv.AmtPaidSat, time.Unix(inv.SettleDate, 0).Format("2006-01-02")))
		}
	}, func(p *lnrpc.Payment) {
		lndHashes[p.PaymentHash] = true
		t.PaymentsSucceeded++
		t.PaymentsValueSat += p.ValueSat
		t.PaymentsFeeSat += p.FeeSat
		if !known[p.PaymentHash] {
			t.PaymentsNotInApp = append(t.PaymentsNotInApp, fmt.Sprintf("%s %d %s", p.PaymentHash, p.ValueSat, time.Unix(p.CreationDate, 0).Format("2006-01-02")))
		}
	})
	t.InvoicesSkipped = skipped
	if err != nil {
		return nil, err
	}
	for _, p := range payments {
		if p.Type == data.Payment_CLOSED_CHANNEL || lndHashes[p.PaymentHash] {
			continue
		}
		t.AppNotInLnd = append(t.AppNotInLnd, fmt.Sprintf("%s %d %s type %v", p.PaymentHash, p.Amount, time.Unix(p.CreationTimestamp, 0).Format("2006-01-02"), p.Type))
	}
	return t, nil
}

// History builds the ledger from three sources: the payment list the mobile
// app kept in its database (sent, received, deposits, withdrawals, closed
// channels), lnd's closed and closing channels, and the on-chain wallet's
// transactions. On-chain transactions that belong to a channel close or a
// deposit swap are folded into that entry, so the same money is never
// listed twice.
func (c *Core) History(ctx context.Context) (*History, error) {
	payments, err := c.RawPayments()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	h := &History{Entries: []Entry{}, Warnings: []string{}}

	txsRes, err := c.node.client.GetTransactions(cctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return nil, err
	}
	closed, err := c.node.client.ClosedChannels(cctx, &lnrpc.ClosedChannelsRequest{})
	if err != nil {
		return nil, err
	}
	pend, err := c.node.client.PendingChannels(cctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		h.Warnings = append(h.Warnings, "Channels still closing could not be listed: "+err.Error())
		pend = &lnrpc.PendingChannelsResponse{}
	}
	st, err := c.node.status(cctx)
	if err != nil {
		return nil, err
	}

	// lnd is the record of what was actually paid and received; the app's
	// list adds descriptions. Anything lnd knows and the app missed is
	// listed from lnd, without a description.
	lndOnly, err := c.lndOnlyPayments(cctx, payments)
	if err != nil {
		h.Warnings = append(h.Warnings, "The node's own payment records could not be compared with the app's list: "+err.Error())
	}

	b := newLedger(txsRes.Transactions)
	b.blockTime = c.node.blockTimer(cctx, txsRes.Transactions)
	b.addClosedChannels(closed.Channels, payments)
	b.addPendingChannels(pend)
	b.addPayments(payments)
	b.entries = append(b.entries, lndOnly...)
	b.addOnchain()

	h.Entries = b.entries
	h.ZeroCloses = b.zeroCloses
	sort.SliceStable(h.Entries, func(i, j int) bool { return h.Entries[i].Time > h.Entries[j].Time })

	t := &h.Totals
	for _, e := range h.Entries {
		if e.Delta > 0 {
			t.In += e.Delta
		} else if e.Delta < 0 {
			t.Out += -e.Delta - e.Fee
		}
		if e.FeeNote == "" {
			t.Fees += e.Fee
		}
	}
	t.Expected = t.In - t.Out - t.Fees
	t.Onchain = st.OnchainConfirmed + st.OnchainUnconfirmed
	t.InChannels = st.InChannels
	t.InPending = st.InPending
	t.Held = t.Onchain + t.InChannels + t.InPending
	t.Uncollected = b.uncollected
	t.Unexplained = t.Held + t.Uncollected - t.Expected
	h.Warnings = append(h.Warnings, st.Warnings...)
	return h, nil
}

// ledger accumulates entries while keeping track of which wallet
// transactions are already explained by another entry.
type ledger struct {
	txs       []*lnrpc.Transaction
	byID      map[string]*lnrpc.Transaction
	spenders  map[string][]*lnrpc.Transaction // txid -> txs spending one of its outputs
	explained map[string]bool                 // wallet txs folded into another entry
	blockTime func(height uint32, txid string) int64

	entries     []Entry
	zeroCloses  int
	uncollected int64

	// sweepIndex maps a sweep txid to its "collected" entry, and
	// sweepInputs the closing txids and amounts each sweep spends, so the
	// sweep fee can be worked out (lnd reports no fee for inputs the
	// wallet does not own).
	sweepIndex  map[string]int
	sweepInputs map[string]sweepSources
}

type sweepSources struct {
	closingTxids map[string]bool
	amount       int64
}

func newLedger(txs []*lnrpc.Transaction) *ledger {
	b := &ledger{txs: txs, byID: map[string]*lnrpc.Transaction{}, spenders: map[string][]*lnrpc.Transaction{}, explained: map[string]bool{}, entries: []Entry{}, sweepIndex: map[string]int{}, sweepInputs: map[string]sweepSources{}}
	for _, tx := range txs {
		b.byID[tx.TxHash] = tx
		for _, prev := range tx.PreviousOutpoints {
			if i := strings.Index(prev.Outpoint, ":"); i > 0 {
				b.spenders[prev.Outpoint[:i]] = append(b.spenders[prev.Outpoint[:i]], tx)
			}
		}
	}
	return b
}

// collectors returns the wallet transactions that brought a closed
// channel's funds into the on-chain balance: the closing transaction
// itself when it paid the wallet directly (cooperative close), otherwise
// the sweeps of its outputs.
func (b *ledger) collectors(closingTxid string, resolutions []*lnrpc.Resolution) (direct *lnrpc.Transaction, sweeps []*lnrpc.Transaction) {
	if tx := b.byID[closingTxid]; tx != nil && tx.Amount > 0 {
		return tx, nil
	}
	seen := map[string]bool{}
	for _, tx := range b.spenders[closingTxid] {
		if !seen[tx.TxHash] {
			seen[tx.TxHash] = true
			sweeps = append(sweeps, tx)
		}
	}
	for _, r := range resolutions {
		if tx := b.byID[r.SweepTxid]; tx != nil && !seen[tx.TxHash] {
			seen[tx.TxHash] = true
			sweeps = append(sweeps, tx)
		}
	}
	return nil, sweeps
}

func (b *ledger) addClose(e Entry, closingTxid string, resolutions []*lnrpc.Resolution) {
	direct, sweeps := b.collectors(closingTxid, resolutions)
	switch {
	case direct != nil:
		b.explained[direct.TxHash] = true
		e.Detail += " Your balance was paid straight into the on-chain balance."
	case len(sweeps) > 0:
		e.Detail += " Your balance was set aside on-chain and collected later, see below."
		for _, tx := range sweeps {
			src := b.sweepInputs[tx.TxHash]
			if src.closingTxids == nil {
				src.closingTxids = map[string]bool{}
			}
			src.closingTxids[closingTxid] = true
			src.amount += e.Amount
			b.sweepInputs[tx.TxHash] = src
			if b.explained[tx.TxHash] {
				continue // one sweep can collect several channels
			}
			b.explained[tx.TxHash] = true
			b.sweepIndex[tx.TxHash] = len(b.entries)
			b.entries = append(b.entries, Entry{
				Time: tx.TimeStamp, Kind: KindCollected, Title: "Channel funds collected",
				Detail: "Funds set aside by a channel close arrived in the on-chain balance.",
				Amount: sumOurs(tx), Fee: tx.TotalFees, Status: txStatus(tx), TxID: tx.TxHash,
			})
		}
	default:
		e.Detail += " Your balance was set aside on-chain and has not been collected yet."
		b.uncollected += e.Amount
	}
	b.entries = append(b.entries, e)
}

// addClosedChannels lists closed channels. lnd's list is the source of
// truth; the mobile app's own records fill in channels lnd no longer
// knows (its database was pruned on the phone).
func (b *ledger) addClosedChannels(closed []*lnrpc.ChannelCloseSummary, payments []*data.Payment) {
	known := map[string]bool{}
	for _, ch := range closed {
		known[ch.ChannelPoint] = true
		amount := ch.SettledBalance
		if amount == 0 {
			for _, r := range ch.Resolutions {
				amount += int64(r.AmountSat)
			}
		}
		if amount == 0 {
			b.zeroCloses++
			continue
		}
		b.addClose(Entry{
			Time: b.blockTime(ch.CloseHeight, ch.ClosingTxHash), Kind: KindChannelClose, Title: "Channel closed",
			Detail: closeDetail(ch.CloseType), Amount: amount, Status: "done", TxID: ch.ClosingTxHash,
		}, ch.ClosingTxHash, ch.Resolutions)
	}
	for _, p := range payments {
		if p.Type != data.Payment_CLOSED_CHANNEL || known[p.ClosedChannelPoint] {
			continue
		}
		known[p.ClosedChannelPoint] = true
		if p.Amount == 0 {
			b.zeroCloses++
			continue
		}
		e := Entry{Time: p.CreationTimestamp, Kind: KindChannelClose, Title: "Channel closed", Detail: "Closed.", Amount: p.Amount, Status: "done", TxID: p.ClosedChannelTxID}
		if p.IsChannelPending {
			e.Title, e.Status = "Channel closing", "closing"
		}
		var res []*lnrpc.Resolution
		if p.ClosedChannelSweepTxID != "" {
			res = append(res, &lnrpc.Resolution{SweepTxid: p.ClosedChannelSweepTxID})
		}
		b.addClose(e, p.ClosedChannelTxID, res)
	}
	// A sweep that spends only closing outputs costs the difference between
	// what the closes set aside and what arrived.
	for txid, i := range b.sweepIndex {
		e := &b.entries[i]
		if e.Fee != 0 {
			continue
		}
		src := b.sweepInputs[txid]
		onlyCloses := true
		for _, in := range b.byID[txid].PreviousOutpoints {
			if j := strings.Index(in.Outpoint, ":"); j < 0 || !src.closingTxids[in.Outpoint[:j]] {
				onlyCloses = false
			}
		}
		if onlyCloses && src.amount > e.Amount {
			e.Fee = src.amount - e.Amount
		}
	}
}

func (b *ledger) addPendingChannels(pend *lnrpc.PendingChannelsResponse) {
	now := time.Now().Unix()
	for _, p := range pend.WaitingCloseChannels {
		b.entries = append(b.entries, Entry{Time: now, Kind: KindChannelClose, Title: "Channel closing", Detail: "Waiting for the closing transaction to be published.", Amount: p.LimboBalance, Status: "closing"})
	}
	for _, p := range pend.PendingClosingChannels {
		b.entries = append(b.entries, Entry{Time: now, Kind: KindChannelClose, Title: "Channel closing", Detail: "Closed together with the peer. Waiting for the closing transaction to confirm.", Amount: p.Channel.LocalBalance, Status: "closing", TxID: p.ClosingTxid})
		b.explained[p.ClosingTxid] = true
	}
	for _, p := range pend.PendingForceClosingChannels {
		note := ""
		if p.BlocksTilMaturity > 0 {
			note = "The funds unlock in " + blocksToText(p.BlocksTilMaturity) + "."
		}
		b.entries = append(b.entries, Entry{Time: now, Kind: KindChannelClose, Title: "Channel closing", Detail: "Force closed. The funds are locked for a while before they can be collected.", Amount: p.LimboBalance, Status: "closing", TxID: p.ClosingTxid, Note: note})
		b.explained[p.ClosingTxid] = true
		for _, tx := range b.spenders[p.ClosingTxid] {
			b.explained[tx.TxHash] = true
		}
	}
}

// swapOutput is a wallet output on a deposit (swap) address: a script
// address the app watches, which only a swap service can turn into
// channel funds.
type swapOutput struct {
	tx       *lnrpc.Transaction
	amount   int64
	fromUs   bool // funded from the app's own on-chain balance
	deposit  *Entry
	spentBy  *lnrpc.Transaction
	outpoint string
}

// swapOutputs finds deposit-address outputs and who spent them.
func (b *ledger) swapOutputs() []*swapOutput {
	var outs []*swapOutput
	byOutpoint := map[string]*swapOutput{}
	for _, tx := range b.txs {
		if b.explained[tx.TxHash] {
			continue
		}
		fromUs := false
		for _, in := range tx.PreviousOutpoints {
			if in.IsOurOutput {
				fromUs = true
			}
		}
		for _, o := range tx.OutputDetails {
			if o.IsOurAddress && o.OutputType == lnrpc.OutputScriptType_SCRIPT_TYPE_WITNESS_V0_SCRIPT_HASH {
				so := &swapOutput{tx: tx, amount: o.Amount, fromUs: fromUs, outpoint: tx.TxHash + ":" + strconv.FormatInt(o.OutputIndex, 10)}
				outs = append(outs, so)
				byOutpoint[so.outpoint] = so
			}
		}
	}
	for _, tx := range b.txs {
		for _, in := range tx.PreviousOutpoints {
			if so := byOutpoint[in.Outpoint]; so != nil {
				so.spentBy = tx
			}
		}
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].tx.TimeStamp < outs[j].tx.TimeStamp })
	return outs
}

// addPayments lists the mobile app's payments. Deposits are matched to
// the on-chain transaction that funded the deposit address, so a swap
// shows as one entry: money in when the bitcoin came from outside, a move
// when it came from the app's own on-chain balance.
func (b *ledger) addPayments(payments []*data.Payment) {
	var deposits []int // indexes into b.entries, which only grows
	for _, p := range payments {
		e, ok := paymentEntry(p)
		if !ok {
			continue
		}
		b.entries = append(b.entries, e)
		if e.Kind == KindDeposit {
			deposits = append(deposits, len(b.entries)-1)
		}
	}

	// The deposit is recorded when the swap service paid the invoice,
	// shortly after the on-chain funds confirmed, for at most the on-chain
	// amount. Exact amounts pair first, then the nearest in time.
	outs := b.swapOutputs()
	used := map[int]bool{}
	matched := map[*swapOutput]int{}
	inWindow := func(d Entry, so *swapOutput) bool {
		return d.Amount <= so.amount && d.Time >= so.tx.TimeStamp-2*3600 && d.Time <= so.tx.TimeStamp+14*24*3600
	}
	for pass := 0; pass < 2; pass++ {
		for _, so := range outs {
			if _, ok := matched[so]; ok {
				continue
			}
			bestIdx := -1
			for _, i := range deposits {
				d := b.entries[i]
				if used[i] || !inWindow(d, so) {
					continue
				}
				exact := d.Amount == so.amount || d.Amount+d.Fee == so.amount
				if pass == 0 && !exact {
					continue
				}
				if bestIdx < 0 || abs64(d.Time-so.tx.TimeStamp) < abs64(b.entries[bestIdx].Time-so.tx.TimeStamp) {
					bestIdx = i
				}
			}
			if bestIdx >= 0 {
				used[bestIdx] = true
				matched[so] = bestIdx
			}
		}
	}
	for _, so := range outs {
		bestIdx, ok := matched[so]
		if !ok {
			// Never swapped in. Either still sitting there or refunded.
			b.explained[so.tx.TxHash] = true
			e := Entry{Time: so.tx.TimeStamp, Kind: KindDeposit, Title: "Deposit received", Amount: so.amount, Status: txStatus(so.tx), TxID: so.tx.TxHash}
			if so.fromUs {
				e.Kind, e.Title, e.Detail, e.Fee = KindOnchainSelf, "Moved to the deposit address", "Moved from the on-chain balance to the app's deposit address, not into a channel.", so.tx.TotalFees
			} else {
				e.Detail, e.Delta = "Bitcoin arrived at the app's deposit address, not moved into a channel.", so.amount
			}
			b.entries = append(b.entries, e)
			e = Entry{} // the appended copy is what later edits must touch
			last := &b.entries[len(b.entries)-1]
			// Spent back to one of the app's own addresses: the app moved it
			// on, and that transaction is listed on its own. Spent to an
			// outside address: refunded.
			if so.spentBy != nil && sumOurs(so.spentBy) == 0 && firstExternal(so.spentBy) != "" {
				b.explained[so.spentBy.TxHash] = true
				addr := firstExternal(so.spentBy)
				last.Detail = "Bitcoin arrived at the app's deposit address, never moved into a channel. It was refunded, see below."
				if so.fromUs {
					last.Detail = "Moved from the on-chain balance to the app's deposit address, never into a channel. It was refunded, see below."
				}
				b.entries = append(b.entries, Entry{
					Time: so.spentBy.TimeStamp, Kind: KindRefund, Title: "Deposit refunded", Detail: "The deposit was sent back to " + addr,
					Amount: so.amount - so.spentBy.TotalFees, Delta: -so.amount, Fee: so.spentBy.TotalFees, Status: txStatus(so.spentBy), TxID: so.spentBy.TxHash, Address: addr,
				})
			}
			continue
		}
		best := &b.entries[bestIdx]
		best.TxID = so.tx.TxHash
		best.Fee = so.amount - best.Amount
		b.explained[so.tx.TxHash] = true
		if so.spentBy != nil {
			b.explained[so.spentBy.TxHash] = true // the swap service collecting its side
		}
		if so.fromUs {
			best.Detail = "Moved into a channel from the app's on-chain balance."
			best.Delta = 0
			best.Fee += so.tx.TotalFees
			best.FeeNote = ""
		} else {
			best.Detail = "Bitcoin sent to the app's deposit address, moved into a channel."
			best.Delta = best.Amount
			if best.Fee > 0 {
				best.FeeNote = "taken by the swap service before the funds reached the app"
			}
		}
	}
}

// addOnchain lists the wallet transactions no other entry explains.
func (b *ledger) addOnchain() {
	for _, tx := range b.txs {
		if b.explained[tx.TxHash] {
			continue
		}
		if e, ok := onchainEntry(tx); ok {
			b.entries = append(b.entries, e)
		}
	}
}

// paymentEntry turns one payment of the app's database into a ledger entry.
// Channel closes are handled by addClosedChannels.
func paymentEntry(p *data.Payment) (Entry, bool) {
	e := Entry{Time: p.CreationTimestamp, Amount: p.Amount, Fee: p.Fee, Status: "done"}
	if p.PendingExpirationHeight != 0 || p.PendingExpirationTimestamp != 0 {
		e.Status = "pending"
	}
	memo := p.InvoiceMemo
	desc, payee, payer := "", "", ""
	if memo != nil {
		desc, payee, payer = strings.TrimSpace(memo.Description), strings.TrimSpace(memo.PayeeName), strings.TrimSpace(memo.PayerName)
	}
	if p.LnurlPayInfo != nil {
		desc = firstOf(desc, strings.TrimSpace(p.LnurlPayInfo.InvoiceDescription))
		payee = firstOf(payee, strings.TrimSpace(p.LnurlPayInfo.LightningAddress), strings.TrimSpace(p.LnurlPayInfo.Host))
	}
	switch p.Type {
	case data.Payment_RECEIVED:
		e.Kind, e.Title = KindReceived, "Received"
		e.Detail = firstOf(desc, withPrefix("from ", payer), "Lightning payment, no description")
		e.Delta = p.Amount
		if p.Fee > 0 {
			e.FeeNote = "taken by the channel provider before the payment reached the app"
		}
	case data.Payment_SENT:
		e.Kind, e.Title = KindSent, "Sent"
		e.Detail = firstOf(desc, withPrefix("to ", payee), keysendText(p), "Lightning payment, no description")
		e.Delta = -(p.Amount + p.Fee)
	case data.Payment_DEPOSIT:
		e.Kind, e.Title = KindDeposit, "Deposit"
		e.Detail = "Bitcoin sent to the app's deposit address, moved into a channel."
		e.Delta = p.Amount
		if p.Fee > 0 {
			e.FeeNote = "taken by the swap service before the funds reached the app"
		}
	case data.Payment_WITHDRAWAL:
		e.Kind, e.Title = KindWithdrawal, "Withdrawal"
		e.Detail = "Sent from a channel to a bitcoin address."
		e.Delta = -(p.Amount + p.Fee)
		e.TxID = p.RedeemTxID
	default:
		return Entry{}, false
	}
	return e, true
}

func keysendText(p *data.Payment) string {
	if !p.IsKeySend {
		return ""
	}
	if p.GroupName != "" {
		return "to " + p.GroupName
	}
	return "Spontaneous payment, no description"
}

func withPrefix(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func firstOf(opts ...string) string {
	for _, o := range opts {
		if o != "" {
			return o
		}
	}
	return ""
}

func closeDetail(t lnrpc.ChannelCloseSummary_ClosureType) string {
	switch t {
	case lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE:
		return "Closed together with the peer."
	case lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE:
		return "Force closed by the app."
	case lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE:
		return "Force closed by the peer."
	case lnrpc.ChannelCloseSummary_BREACH_CLOSE:
		return "The peer published an old state and the app claimed the whole channel."
	case lnrpc.ChannelCloseSummary_FUNDING_CANCELED:
		return "The channel was never opened."
	case lnrpc.ChannelCloseSummary_ABANDONED:
		return "The channel was abandoned."
	}
	return "Closed."
}

func txStatus(tx *lnrpc.Transaction) string {
	if tx.BlockHeight == 0 {
		return "unconfirmed"
	}
	return "done"
}

func sumOurs(tx *lnrpc.Transaction) int64 {
	var n int64
	for _, o := range tx.OutputDetails {
		if o.IsOurAddress {
			n += o.Amount
		}
	}
	return n
}

func firstExternal(tx *lnrpc.Transaction) string {
	for _, o := range tx.OutputDetails {
		if !o.IsOurAddress {
			return o.Address
		}
	}
	return ""
}

// onchainEntry classifies a wallet transaction by where its money came
// from and went. Transactions that touch nothing of the app's are skipped
// (lnd keeps some it only published for others).
func onchainEntry(tx *lnrpc.Transaction) (Entry, bool) {
	e := Entry{Time: tx.TimeStamp, Fee: tx.TotalFees, TxID: tx.TxHash, Status: txStatus(tx)}
	var toOurs, toOthers int64
	for _, o := range tx.OutputDetails {
		if o.IsOurAddress {
			toOurs += o.Amount
		} else {
			toOthers += o.Amount
		}
	}
	fromUs := false
	for _, in := range tx.PreviousOutpoints {
		if in.IsOurOutput {
			fromUs = true
		}
	}
	switch {
	case tx.Amount == 0 && toOurs == 0 && !fromUs:
		return Entry{}, false
	case tx.Amount < 0 && toOthers > 0:
		addr := firstExternal(tx)
		e.Kind, e.Title = KindOnchainOut, "Sent on-chain"
		e.Detail = "Sent from the on-chain balance to " + addr
		e.Amount = toOthers
		e.Delta = -(toOthers + tx.TotalFees)
		e.Address = addr
	case fromUs && toOthers == 0:
		e.Kind, e.Title = KindOnchainSelf, "Moved on-chain"
		e.Detail = "Moved between the app's own addresses."
		e.Amount = toOurs
	default:
		e.Kind, e.Title = KindOnchainIn, "Received on-chain"
		e.Detail = "Bitcoin arrived at an address of the app's on-chain balance."
		e.Amount = tx.Amount
		e.Delta = tx.Amount
		e.Fee = 0
	}
	return e, true
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func blocksToText(blocks int32) string {
	mins := int(blocks) * 10
	switch {
	case mins < 90:
		return fmt.Sprintf("about %d minutes", mins)
	case mins < 60*36:
		return fmt.Sprintf("about %d hours", (mins+30)/60)
	}
	return fmt.Sprintf("about %d days", (mins+720)/1440)
}

// blockTimer returns a function giving the time of a block: from a wallet
// transaction in that block when there is one, else from the chain.
func (n *node) blockTimer(ctx context.Context, txs []*lnrpc.Transaction) func(height uint32, txid string) int64 {
	byHeight := map[uint32]int64{}
	byID := map[string]int64{}
	for _, tx := range txs {
		if tx.BlockHeight > 0 {
			byHeight[uint32(tx.BlockHeight)] = tx.TimeStamp
		}
		byID[tx.TxHash] = tx.TimeStamp
	}
	chain := chainrpc.NewChainKitClient(n.conn)
	return func(height uint32, txid string) int64 {
		if t := byID[txid]; t != 0 {
			return t
		}
		if t := byHeight[height]; t != 0 {
			return t
		}
		if height == 0 {
			return 0
		}
		hash, err := chain.GetBlockHash(ctx, &chainrpc.GetBlockHashRequest{BlockHeight: int64(height)})
		if err != nil {
			return 0
		}
		hdr, err := chain.GetBlockHeader(ctx, &chainrpc.GetBlockHeaderRequest{BlockHash: hash.BlockHash})
		if err != nil {
			return 0
		}
		// The header is the 80-byte serialization; the timestamp sits at 68.
		if len(hdr.RawBlockHeader) < 72 {
			return 0
		}
		b := hdr.RawBlockHeader[68:72]
		return int64(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
	}
}
