package core

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/bindings"
	"github.com/breez/breez/config"
	"github.com/breez/breez/data"
	"github.com/breez/breez/lnnode"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// services implements bindings.AppServices. Notifications are fanned out to
// waiters; backup sign-in is delegated to Google when a provider is in use.
type services struct {
	providerName string
	auth         *googleAuth

	mu      sync.Mutex
	ready   chan struct{}
	isReady bool
	failed  chan error
}

func newServices(providerName string, auth *googleAuth) *services {
	return &services{
		providerName: providerName,
		auth:         auth,
		ready:        make(chan struct{}),
		failed:       make(chan error, 1),
	}
}

func (s *services) BackupProviderName() string { return s.providerName }

func (s *services) BackupProviderSignIn() (string, error) {
	if s.auth == nil {
		return "", errors.New("no backup provider configured")
	}
	return s.auth.SignIn()
}

func (s *services) fail(msg string) {
	select {
	case s.failed <- errors.New(msg):
	default:
	}
}

func (s *services) Notify(payload []byte) {
	var ev data.NotificationEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return
	}
	nodeLog("[notification] " + ev.Type.String())
	switch ev.Type {
	case data.NotificationEvent_READY:
		s.mu.Lock()
		if !s.isReady {
			s.isReady = true
			close(s.ready)
		}
		s.mu.Unlock()
	case data.NotificationEvent_INITIALIZATION_FAILED:
		s.fail("node initialization failed, see the log")
	case data.NotificationEvent_LIGHTNING_SERVICE_DOWN:
		s.fail("the node shut down, see the log")
	case data.NotificationEvent_BACKUP_NODE_CONFLICT:
		s.fail("another device restored this backup after us; the node stopped itself to avoid a penalty. Restore again from the latest backup")
	case data.NotificationEvent_BACKUP_NOT_LATEST_CONFLICT:
		s.fail("a newer backup exists in the cloud; the node stopped itself. Restore again from the latest backup")
	}
}

func nodeLog(line string) {
	logSinkMu.Lock()
	s := logSink
	logSinkMu.Unlock()
	if s != nil {
		s(line)
	}
}

// rescan tracks the wallet's address rescan, which lnd only reports in its
// log. After the headers are in, this is the slow part of a first sync:
// every block since the wallet's birthday is checked against its addresses.
var rescan struct {
	sync.Mutex
	start, height uint32
	addresses     int
	found         int    // wallet transactions seen so far
	throughTime   int64  // timestamp of the newest one
	startTime     int64  // wallet synced-to timestamp when the check began
	wallStart     int64  // wall clock when the check began
	tip           uint32 // chain tip once neutrino reports it caught up
	// walletSyncFailed is set when btcwallet logged that it cannot take up
	// the chain from its sync state.
	walletSyncFailed bool
}

var (
	rescanStartRe   = regexp.MustCompile(`Started rescan from block \S+ \(height (\d+)\) for (\d+) address`)
	rescanThroughRe = regexp.MustCompile(`Rescanned through block \S+ \(height (\d+)\)`)
	rescanBlockRe   = regexp.MustCompile(`\[TRC\] BTCN: Rescan got block (\d+) `)
	caughtUpRe      = regexp.MustCompile(`Fully caught up with cfheaders at height (\d+)`)
	// btcwallet wallet/chainntfns.go: logged, and retried for ever, when the
	// wallet cannot take up the chain from its sync state.
	walletSyncFailRe = regexp.MustCompile(`Unable to synchronize\s+wallet to chain`)
	newBlockRe       = regexp.MustCompile(`NTFN: New block: height=(\d+)`)
)

// waitHeadersSynced returns once neutrino has logged that its headers and
// filter headers reached the chain tip.
func waitHeadersSynced(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		rescan.Lock()
		tip := rescan.tip
		rescan.Unlock()
		if tip > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the bitcoin chain did not finish catching up; check the connection and start again")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// trackRescan feeds a node log line to the rescan tracker. It returns true
// for the per-block trace line, which is progress data rather than
// something to show in the log.
func trackRescan(line string) bool {
	if walletSyncFailRe.MatchString(line) {
		rescan.Lock()
		rescan.walletSyncFailed = true
		rescan.Unlock()
		return false
	}
	if m := caughtUpRe.FindStringSubmatch(line); m != nil {
		h, _ := strconv.ParseUint(m[1], 10, 32)
		rescan.Lock()
		if uint32(h) > rescan.tip {
			rescan.tip = uint32(h)
		}
		rescan.Unlock()
		return false
	}
	if m := newBlockRe.FindStringSubmatch(line); m != nil {
		h, _ := strconv.ParseUint(m[1], 10, 32)
		rescan.Lock()
		if rescan.tip > 0 && uint32(h) > rescan.tip {
			rescan.tip = uint32(h)
		}
		rescan.Unlock()
		return false
	}
	if m := rescanStartRe.FindStringSubmatch(line); m != nil {
		h, _ := strconv.ParseUint(m[1], 10, 32)
		n, _ := strconv.Atoi(m[2])
		rescan.Lock()
		rescan.start, rescan.height, rescan.addresses = uint32(h), uint32(h), n
		rescan.Unlock()
		return false
	}
	if m := rescanThroughRe.FindStringSubmatch(line); m != nil {
		h, _ := strconv.ParseUint(m[1], 10, 32)
		rescan.Lock()
		rescan.height = uint32(h)
		rescan.Unlock()
		return false
	}
	if m := rescanBlockRe.FindStringSubmatch(line); m != nil {
		h, _ := strconv.ParseUint(m[1], 10, 32)
		rescan.Lock()
		if rescan.start == 0 {
			rescan.start = uint32(h)
		}
		if uint32(h) > rescan.height {
			rescan.height = uint32(h)
		}
		rescan.Unlock()
		return true
	}
	return strings.Contains(line, "[TRC]")
}

// node is a running embedded lnd with a direct gRPC client to it.
type node struct {
	svc    *services
	conn   *grpc.ClientConn
	client lnrpc.LightningClient
	// wroteSyncState: the history shortcut set the wallet's sync state
	// before this start.
	wroteSyncState bool
}

// startNode starts lnd, blocks until the RPC is ready and connects to it.
func startNode(ctx context.Context, cfg Config, svc *services) (*node, error) {
	torCfg, _ := proto.Marshal(&data.TorConfig{})
	if err := bindings.Start(torCfg); err != nil {
		return nil, fmt.Errorf("start node: %w", err)
	}
	select {
	case <-svc.ready:
	case err := <-svc.failed:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, errors.New("timed out waiting for the node to become ready")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	libCfg, err := config.GetConfig(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	conn, err := lnnode.NewClientConnection(libCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to node: %w", err)
	}
	return &node{svc: svc, conn: conn, client: lnrpc.NewLightningClient(conn)}, nil
}

func (n *node) close() {
	if n.conn != nil {
		n.conn.Close()
	}
}

func (n *node) info(ctx context.Context) (*lnrpc.GetInfoResponse, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return n.client.GetInfo(c, &lnrpc.GetInfoRequest{})
}

// estimatedTip guesses the current chain height from a known anchor, for
// progress display only. lnd does not expose the target height while it
// syncs.
var tipAnchor = struct {
	height uint32
	at     time.Time
}{967301, time.Date(2026, 9, 16, 16, 43, 0, 0, time.UTC)}

func estimatedTip() uint32 {
	elapsed := time.Since(tipAnchor.at)
	if elapsed < 0 {
		return tipAnchor.height
	}
	return tipAnchor.height + uint32(elapsed/(10*time.Minute))
}

// trackTransactions polls the wallet's transaction list. While btcwallet
// rescans, transactions show up as their blocks are reached, so the
// highest block among them is how far the scan has verifiably got.
func (n *node) trackTransactions(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := n.client.GetTransactions(c, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return
	}
	var through uint32
	var throughTime int64
	for _, tx := range res.Transactions {
		if tx.BlockHeight > 0 && uint32(tx.BlockHeight) > through {
			through = uint32(tx.BlockHeight)
			throughTime = tx.TimeStamp
		}
	}
	rescan.Lock()
	rescan.found = len(res.Transactions)
	if through > rescan.height {
		rescan.height = through
		rescan.throughTime = throughTime
	}
	rescan.Unlock()
}

// waitSynced polls GetInfo until lnd reports synced_to_chain, reporting
// progress on every change.
func (n *node) waitSynced(ctx context.Context, onProgress func(SyncProgress)) error {
	var last SyncProgress
	// A sync that stands still says so: the bitcoin peers may have gone
	// away, and a screen that does not move reads as a hang.
	lastChange := time.Now()
	for {
		rescan.Lock()
		failed := rescan.walletSyncFailed
		rescan.Unlock()
		// Only after this start wrote the sync state: otherwise the line
		// is a passing network trouble btcwallet gets over by itself.
		if failed && n.wroteSyncState {
			return errWalletSync
		}
		info, err := n.info(ctx)
		if err == nil {
			rescan.Lock()
			headersDone := rescan.tip > 0
			rescan.Unlock()
			if !info.SyncedToChain && (headersDone || info.BlockHeight+3 >= estimatedTip()) {
				n.trackTransactions(ctx)
			}
			p := syncProgress(info)
			if p.Synced() {
				onProgress(p)
				return nil
			}
			if p.Height != last.Height || p.Stage != last.Stage || p.Peers != last.Peers || p.ThroughTime != last.ThroughTime || p.Remaining != last.Remaining {
				onProgress(p)
				last = p
				lastChange = time.Now()
			} else if time.Since(lastChange) >= 10*time.Minute {
				stalled := p
				stalled.Message = "No progress for 10 minutes. Check the internet connection; the sync carries on by itself once the bitcoin network answers."
				onProgress(stalled)
				lastChange = time.Now()
			}
		} else {
			nodeLog("[getinfo] " + err.Error())
		}
		select {
		case err := <-n.svc.failed:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// stillOpenInLnd reports whether lnd lists the channel as open: it has not
// noticed the close yet.
func stillOpenInLnd(chans []*lnrpc.Channel, chanPoint string) bool {
	for _, c := range chans {
		if c.ChannelPoint == chanPoint {
			return true
		}
	}
	return false
}

// errWalletSync: btcwallet cannot take up the chain from its sync state.
var errWalletSync = errors.New("the app cannot continue from its saved sync position")

// Synced reports whether the progress describes a fully synced node.
func (p SyncProgress) Synced() bool { return p.Stage == "synced" }

func syncProgress(info *lnrpc.GetInfoResponse) SyncProgress {
	// The real tip is known once neutrino logs that it caught up with the
	// filter headers; until then an estimate from the clock stands in.
	rescan.Lock()
	tip := rescan.tip
	rescan.Unlock()
	headersDone := tip > 0
	target := tip
	if target == 0 {
		target = estimatedTip()
	}
	if info.BlockHeight > target {
		target = info.BlockHeight
	}
	p := SyncProgress{Height: info.BlockHeight, Target: target, Peers: info.NumPeers}
	switch {
	case info.SyncedToChain:
		p.Stage, p.Percent = "synced", 100
		p.Message = fmt.Sprintf("Synced to the chain at block %d", info.BlockHeight)
	case info.NumPeers == 0 && info.BlockHeight == 0 && !headersDone:
		p.Stage, p.Percent = "connecting", 0
		p.Message = "Connecting to the bitcoin network..."
	case headersDone || info.BlockHeight+3 >= target:
		// Headers are at the tip; lnd is now rescanning the wallet's
		// addresses. Progress comes from the log tracker.
		p.Stage = "rescan"
		rescan.Lock()
		start, addresses := rescan.start, rescan.addresses
		rescan.Unlock()
		// While the wallet is behind the chain, lnd's best_header_timestamp
		// is the timestamp of the block the wallet has scanned through
		// (BtcWallet.IsSynced returns Manager.SyncedTo().Timestamp), so
		// progress is measured in time and moves with every block.
		now := time.Now().Unix()
		rescan.Lock()
		if rescan.startTime == 0 && info.BestHeaderTimestamp > 0 {
			rescan.startTime = info.BestHeaderTimestamp
			rescan.wallStart = now
		}
		found, startTime, wallStart := rescan.found, rescan.startTime, rescan.wallStart
		rescan.Unlock()
		through := info.BestHeaderTimestamp
		p.Found, p.ThroughTime = found, through
		p.Target = target
		p.Remaining = -1
		// Rough time left: chain time covered per wall second so far,
		// once a minute of data exists.
		if wallStart > 0 && now-wallStart >= 60 && through > startTime {
			rate := float64(through-startTime) / float64(now-wallStart)
			p.Remaining = int64(float64(now-through) / rate)
		}
		if through <= 0 || startTime <= 0 || now <= startTime {
			p.Percent = -1
			p.Height = start
			p.Message = "Reached the chain tip. Now checking every block for your channel and payment history; this is the slow part of a first sync."
			return p
		}
		p.Percent = float64(through-startTime) / float64(now-startTime) * 100
		if p.Percent > 99.9 {
			p.Percent = 99.9
		}
		// Block estimate from time: ten minutes per block on average.
		p.Height = target - uint32((now-through)/600)
		if p.Height < start {
			p.Height = start
		}
		what := "Checking every block for your channel and payment history"
		if addresses > 0 {
			what += fmt.Sprintf(" (%d addresses)", addresses)
		}
		p.Message = fmt.Sprintf("%s. Reached %s.", what, time.Unix(through, 0).Format("2 Jan 2006"))
	default:
		p.Stage = "headers"
		p.Percent = float64(info.BlockHeight) / float64(target) * 100
		if p.Percent > 99 {
			p.Percent = 99
		}
		p.Message = fmt.Sprintf("Catching up with the bitcoin chain, block %d of about %d", info.BlockHeight, target)
	}
	return p
}

// whileAvailable runs a call to the node again when its connection reports
// Unavailable. The connection to the embedded lnd sits idle during a long
// walk, and the first call after it can find it being set up again (seen
// 2026-09-21 after a 34 minute walk: "authentication handshake failed:
// io: read/write on closed pipe"). Any other error is returned at once.
func whileAvailable(ctx context.Context, call func() error) error {
	wait := time.Second
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil || status.Code(err) != codes.Unavailable || attempt == 6 {
			return err
		}
		nodeLog(fmt.Sprintf("[node] %v; asking again", err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// walletTxIDs are the transactions the wallet knows.
func (n *node) walletTxIDs(ctx context.Context) (map[string]bool, error) {
	var res *lnrpc.TransactionDetails
	err := whileAvailable(ctx, func() (err error) {
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		res, err = n.client.GetTransactions(c, &lnrpc.GetTransactionsRequest{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list the app's transactions: %w", err)
	}
	ids := map[string]bool{}
	for _, tx := range res.Transactions {
		ids[tx.TxHash] = true
	}
	return ids, nil
}

func (n *node) walletBalance(ctx context.Context) (*lnrpc.WalletBalanceResponse, error) {
	return n.client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
}

func (n *node) openChannels(ctx context.Context) ([]*lnrpc.Channel, error) {
	res, err := n.client.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Channels, nil
}

func (n *node) pending(ctx context.Context) (*lnrpc.PendingChannelsResponse, error) {
	return n.client.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
}

// dustLimit is the smallest balance a close of this channel pays out: the
// larger of the two sides' dust limits. Breez mobile sets its own limit to
// zero, but the LSP's limit (354 sat, 573 on older channels) is the one
// that counts: the LSP's commitment drops anything below it, and a smaller
// output would not relay anyway. Seen in Roy's backups: balances of 999
// and 600 sat got an output when the LSP closed, 500, 172 and 100 did not.
func dustLimit(c *lnrpc.Channel) int64 {
	local := int64(c.GetLocalConstraints().GetDustLimitSat())
	remote := int64(c.GetRemoteConstraints().GetDustLimitSat())
	if remote > local {
		return remote
	}
	return local
}

// isDust reports whether the channel's balance can never be paid out.
func isDust(c *lnrpc.Channel) bool {
	return c.LocalBalance < dustLimit(c)
}

func (n *node) status(ctx context.Context, checks *channelChecks) (*Status, error) {
	info, err := n.info(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{
		NodeID:        info.IdentityPubkey,
		BlockHeight:   info.BlockHeight,
		Synced:        info.SyncedToChain,
		Peers:         info.NumPeers,
		Channels:      []Channel{},
		ClosedOnChain: []SpentChannel{},
		Pending:       []PendingClose{},
		Warnings:      []string{},
	}
	wb, err := n.walletBalance(ctx)
	if err != nil {
		return nil, err
	}
	st.OnchainConfirmed = wb.ConfirmedBalance
	st.OnchainUnconfirmed = wb.UnconfirmedBalance

	// What a closed channel still owes this app comes from the chain walk,
	// which follows the close's output itself: it holds whether or not lnd
	// has noticed the close, and whether or not lnd can list its pending
	// closes (on some old nodes it cannot).
	counted := map[string]bool{} // outputs counted as Pending
	walked := map[string]SpentChannel{}
	for _, sc := range checks.spent() {
		walked[sc.ChannelPoint] = sc
		st.ClosedOnChain = append(st.ClosedOnChain, sc)
		st.InPending += sc.Collect
		if sc.ToUs != "" {
			counted[sc.ToUs] = true
		}
	}

	confirmedTx := map[string]bool{}
	txs, err := n.client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		st.Warnings = append(st.Warnings, "the app's transactions could not be listed: "+err.Error())
	} else {
		if st.OnchainUnconfirmed -= notOnItsWay(txs.Transactions, counted); st.OnchainUnconfirmed < 0 {
			st.OnchainUnconfirmed = 0
		}
		for _, tx := range txs.Transactions {
			switch {
			case tx.NumConfirmations > 0:
				confirmedTx[tx.TxHash] = true
			case tx.Amount < 0:
				// Sent and not confirmed yet: the balance is gone from the
				// numbers already, the recovery is not over.
				st.Outgoing++
			}
		}
	}

	chans, err := n.openChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chans {
		// Only a channel the chain check confirmed counts as funds. A
		// channel that closed after the backup was taken still looks open
		// to lnd, and a backup can carry another node's channels.
		switch v := checks.get(c.ChannelPoint); v.verdict {
		case verdictOpen:
			st.Channels = append(st.Channels, Channel{
				ChannelPoint:  c.ChannelPoint,
				Peer:          c.RemotePubkey,
				Capacity:      c.Capacity,
				LocalBalance:  c.LocalBalance,
				RemoteBalance: c.RemoteBalance,
				Active:        c.Active,
				Dust:          isDust(c),
				DustLimit:     dustLimit(c),
			})
			// A balance below the channel's dust limit can never be paid
			// out: every close drops an output that small. The channel is
			// listed, it is open after all, but it is not funds.
			if !isDust(c) {
				st.InChannels += c.LocalBalance
			}
		case verdictSpent:
			// Counted above, from the walk.
		case verdictForeign:
			st.Warnings = append(st.Warnings, "channel "+c.ChannelPoint+" belongs to another node")
		default:
			st.Warnings = append(st.Warnings, "channel "+c.ChannelPoint+" not counted: "+v.reason)
		}
	}

	pend, err := n.pending(ctx)
	if err != nil {
		// Old nodes can carry a closed channel lnd no longer has an
		// arbitrator for, which makes the whole RPC fail.
		st.Warnings = append(st.Warnings, "pending channel closes could not be listed: "+err.Error())
		pend = &lnrpc.PendingChannelsResponse{}
	}
	// A close whose payout the walk follows is the walk's to report, above:
	// lnd's figure for it would count the same money again, and keeps
	// standing for a while after the sweep confirmed.
	byWalk := func(chanPoint string) bool { return walked[chanPoint].PayoutKnown }
	listed := map[string]bool{}
	for _, c := range pend.WaitingCloseChannels {
		listed[c.Channel.ChannelPoint] = true
		// A close that confirmed and paid the wallet shows on-chain before
		// lnd has noticed: its figure would count the money again.
		if byWalk(c.Channel.ChannelPoint) || (c.ClosingTxid != "" && confirmedTx[c.ClosingTxid]) {
			continue
		}
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "waiting", Amount: c.LimboBalance})
		st.InPending += c.LimboBalance
	}
	for _, c := range pend.PendingClosingChannels {
		listed[c.Channel.ChannelPoint] = true
		if byWalk(c.Channel.ChannelPoint) {
			continue
		}
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "cooperative", ClosingTxID: c.ClosingTxid, Amount: c.Channel.LocalBalance})
		st.InPending += c.Channel.LocalBalance
	}
	for _, c := range pend.PendingForceClosingChannels {
		listed[c.Channel.ChannelPoint] = true
	}
	// A channel closed on chain whose payout the walk cannot tell and lnd
	// does not list (yet, or at all): its settlement is lnd's to finish,
	// and the app must not call the recovery done meanwhile.
	for point, sc := range walked {
		if !sc.PayoutKnown && !listed[point] && checks.get(point).verdict == verdictSpent && stillOpenInLnd(chans, point) {
			st.Unresolved++
		}
	}
	swept, err := n.sweptCloses(ctx, pend)
	if err != nil {
		st.Warnings = append(st.Warnings, "could not check whether closing channels were already swept: "+err.Error())
	}
	for _, c := range pend.PendingForceClosingChannels {
		if swept[c.ClosingTxid] || byWalk(c.Channel.ChannelPoint) {
			continue
		}
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "force", ClosingTxID: c.ClosingTxid, Amount: c.LimboBalance, BlocksToMature: c.BlocksTilMaturity})
		st.InPending += c.LimboBalance
	}
	return st, nil
}

// sweptCloses returns the force closes whose funds a confirmed wallet
// transaction already spent. A backup taken before the phone swept a close
// still lists it as closing, and lnd only notices the sweep when its own
// spend check comes back, which can take hours after a restore. Closes with
// HTLCs or a time lock left are never reported, lnd has more to do there.
func (n *node) sweptCloses(ctx context.Context, pend *lnrpc.PendingChannelsResponse) (map[string]bool, error) {
	want := map[string]int64{}
	for _, c := range pend.PendingForceClosingChannels {
		if c.BlocksTilMaturity <= 0 && len(c.PendingHtlcs) == 0 && c.ClosingTxid != "" {
			want[c.ClosingTxid] = c.LimboBalance
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	res, err := n.client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return nil, err
	}
	return sweptBy(res.Transactions, want), nil
}

// deadUnconfirmed sums what unconfirmed wallet transactions would bring in
// although they can never confirm: one of their inputs is already spent by
// a confirmed wallet transaction.
func deadUnconfirmed(txs []*lnrpc.Transaction) int64 {
	return notOnItsWay(txs, nil)
}

// notOnItsWay is the part of the wallet's unconfirmed balance that is not
// new money on its way:
//
//   - a transaction spending an output a CONFIRMED wallet transaction spent
//     can never confirm (lnd swept what the phone had swept already; a
//     neutrino node never hears of the rejection and keeps the transaction);
//   - a transaction collecting an output that counts as Pending already
//     (counted) would show the same money twice;
//   - of several unconfirmed transactions spending the same output (lnd's
//     fee bumps: with neutrino the replaced ones stay in the wallet) only
//     the newest counts.
func notOnItsWay(txs []*lnrpc.Transaction, counted map[string]bool) int64 {
	spent := map[string]bool{}
	for _, tx := range txs {
		if tx.NumConfirmations < 1 {
			continue
		}
		for _, prev := range tx.PreviousOutpoints {
			spent[prev.Outpoint] = true
		}
	}
	var unconfirmed []*lnrpc.Transaction
	for _, tx := range txs {
		if tx.NumConfirmations < 1 && tx.Amount > 0 {
			unconfirmed = append(unconfirmed, tx)
		}
	}
	sort.SliceStable(unconfirmed, func(i, j int) bool { return unconfirmed[i].TimeStamp > unconfirmed[j].TimeStamp })
	var out int64
	taken := map[string]bool{}
	for _, tx := range unconfirmed {
		drop := false
		for _, prev := range tx.PreviousOutpoints {
			if spent[prev.Outpoint] || counted[prev.Outpoint] || taken[prev.Outpoint] {
				drop = true
			}
		}
		if drop {
			out += tx.Amount
			continue
		}
		for _, prev := range tx.PreviousOutpoints {
			taken[prev.Outpoint] = true
		}
	}
	return out
}

// sweptBy returns the txids in want that a confirmed transaction spends.
func sweptBy(txs []*lnrpc.Transaction, want map[string]int64) map[string]bool {
	got := map[string]int64{}
	for _, tx := range txs {
		if tx.NumConfirmations < 1 {
			continue
		}
		seen := map[string]bool{}
		for _, prev := range tx.PreviousOutpoints {
			i := strings.LastIndexByte(prev.Outpoint, ':')
			if i <= 0 {
				continue
			}
			closing := prev.Outpoint[:i]
			if _, ok := want[closing]; ok && !seen[closing] {
				seen[closing] = true
				got[closing] += tx.Amount
			}
		}
	}
	// Spending just any output of the close is not collecting it: the phone
	// may have spent the close's anchor (a few hundred sat, usually a net
	// loss) and left the channel's funds. A sweep brings in at least half
	// of what is owed: that is the most lnd's sweeper spends on fees.
	swept := map[string]bool{}
	for closing, owed := range want {
		if in := got[closing]; in > 0 && in*2 >= owed {
			swept[closing] = true
		}
	}
	return swept
}
