package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/bindings"
	"github.com/breez/breez/config"
	"github.com/breez/breez/data"
	"github.com/breez/breez/lnnode"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
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
}

var (
	rescanStartRe   = regexp.MustCompile(`Started rescan from block \S+ \(height (\d+)\) for (\d+) addresses`)
	rescanThroughRe = regexp.MustCompile(`Rescanned through block \S+ \(height (\d+)\)`)
	rescanBlockRe   = regexp.MustCompile(`\[TRC\] BTCN: Rescan got block (\d+) `)
	caughtUpRe      = regexp.MustCompile(`Fully caught up with cfheaders at height (\d+)`)
	newBlockRe      = regexp.MustCompile(`NTFN: New block: height=(\d+)`)
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

// AddressLookahead is how many addresses are derived on each of the four
// branches (witness key hash and taproot, external and change) after a
// restore, so the history check also finds funds the phone received or
// swept after its last backup. The backup already knows every address in
// use at backup time and the phone used a handful more (Roy's missing
// funds sat one address past the last known one), so 50 per branch is
// plenty. Every extra address slows the history check: 500 per branch
// (2,937 addresses in total) ran at 48 blocks/s against 76 with 938.
const AddressLookahead = 50

// extendAddresses derives the look-ahead addresses. The wallet only scans
// addresses it has derived, and a backup only knows the addresses in use
// at backup time.
func (n *node) extendAddresses(ctx context.Context, progressf func(string, ...interface{})) error {
	wk := walletrpc.NewWalletKitClient(n.conn)
	types := []walletrpc.AddressType{walletrpc.AddressType_WITNESS_PUBKEY_HASH, walletrpc.AddressType_TAPROOT_PUBKEY}
	total := AddressLookahead * len(types) * 2
	done := 0
	for _, t := range types {
		for _, change := range []bool{false, true} {
			for i := 0; i < AddressLookahead; i++ {
				c, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, err := wk.NextAddr(c, &walletrpc.AddrRequest{Type: t, Change: change})
				cancel()
				if err != nil {
					return fmt.Errorf("derive address: %w", err)
				}
				done++
				if done%200 == 0 {
					progressf("Preparing addresses to check, %d of %d...", done, total)
				}
			}
		}
	}
	return nil
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
	for {
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
			st.ClosedOnChain = append(st.ClosedOnChain, v.spent)
			// What the close paid this app and nobody spent yet is money
			// on its way: lnd still lists the channel as open, so it has
			// not collected it. Once it has, the channel leaves this list
			// and the amount shows through lnd's own pending and
			// on-chain balances.
			st.InPending += v.spent.Collect
		case verdictForeign:
			st.Warnings = append(st.Warnings, "channel "+c.ChannelPoint+" belongs to another node and is left alone")
		default:
			st.Warnings = append(st.Warnings, "channel "+c.ChannelPoint+" is not counted: "+v.reason)
		}
	}

	pend, err := n.pending(ctx)
	if err != nil {
		// Old nodes can carry a closed channel lnd no longer has an
		// arbitrator for, which makes the whole RPC fail. Report it
		// instead of hiding balances and open channels.
		st.Warnings = append(st.Warnings, "pending channel closes could not be listed: "+err.Error())
		return st, nil
	}
	for _, c := range pend.WaitingCloseChannels {
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "waiting", Amount: c.LimboBalance})
		st.InPending += c.LimboBalance
	}
	for _, c := range pend.PendingClosingChannels {
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "cooperative", ClosingTxID: c.ClosingTxid, Amount: c.Channel.LocalBalance})
		st.InPending += c.Channel.LocalBalance
	}
	swept, err := n.sweptCloses(ctx, pend)
	if err != nil {
		st.Warnings = append(st.Warnings, "could not check whether closing channels were already swept: "+err.Error())
	}
	for _, c := range pend.PendingForceClosingChannels {
		if swept[c.ClosingTxid] {
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
	want := map[string]bool{}
	for _, c := range pend.PendingForceClosingChannels {
		if c.BlocksTilMaturity <= 0 && len(c.PendingHtlcs) == 0 && c.ClosingTxid != "" {
			want[c.ClosingTxid] = true
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

// sweptBy returns the txids in want that a confirmed transaction spends.
func sweptBy(txs []*lnrpc.Transaction, want map[string]bool) map[string]bool {
	swept := map[string]bool{}
	for _, tx := range txs {
		if tx.NumConfirmations < 1 {
			continue
		}
		for _, prev := range tx.PreviousOutpoints {
			if i := strings.LastIndexByte(prev.Outpoint, ':'); i > 0 && want[prev.Outpoint[:i]] {
				swept[prev.Outpoint[:i]] = true
			}
		}
	}
	return swept
}
