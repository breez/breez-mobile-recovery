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
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	found         int // wallet transactions seen so far
}

var (
	rescanStartRe   = regexp.MustCompile(`Started rescan from block \S+ \(height (\d+)\) for (\d+) addresses`)
	rescanThroughRe = regexp.MustCompile(`Rescanned through block \S+ \(height (\d+)\)`)
	rescanBlockRe   = regexp.MustCompile(`\[TRC\] BTCN: Rescan got block (\d+) `)
)

// trackRescan feeds a node log line to the rescan tracker. It returns true
// for the per-block trace line, which is progress data rather than
// something to show in the log.
func trackRescan(line string) bool {
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
	for _, tx := range res.Transactions {
		if tx.BlockHeight > 0 && uint32(tx.BlockHeight) > through {
			through = uint32(tx.BlockHeight)
		}
	}
	rescan.Lock()
	rescan.found = len(res.Transactions)
	if through > rescan.height {
		rescan.height = through
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
			if !info.SyncedToChain && info.BlockHeight+3 >= estimatedTip() {
				n.trackTransactions(ctx)
			}
			p := syncProgress(info)
			if p.Synced() {
				onProgress(p)
				return nil
			}
			if p.Height != last.Height || p.Stage != last.Stage || p.Peers != last.Peers {
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
	target := estimatedTip()
	if info.BlockHeight > target {
		target = info.BlockHeight
	}
	p := SyncProgress{Height: info.BlockHeight, Target: target, Peers: info.NumPeers}
	switch {
	case info.SyncedToChain:
		p.Stage, p.Percent = "synced", 100
		p.Message = fmt.Sprintf("Synced to the chain at block %d", info.BlockHeight)
	case info.NumPeers == 0 && info.BlockHeight == 0:
		p.Stage, p.Percent = "connecting", 0
		p.Message = "Connecting to the bitcoin network..."
	case info.BlockHeight+3 >= target:
		// Headers are at the tip; lnd is now rescanning the wallet's
		// addresses. Progress comes from the log tracker.
		p.Stage = "rescan"
		rescan.Lock()
		start, height, addresses := rescan.start, rescan.height, rescan.addresses
		rescan.Unlock()
		// lnd reports no per-block progress for this phase. The wallet's
		// transactions are discovered in block order as the scan reaches
		// them, so the highest block among them is a verified lower
		// bound of how far it got.
		rescan.Lock()
		found := rescan.found
		rescan.Unlock()
		p.Height, p.Target = height, target
		switch {
		case start == 0:
			p.Percent = -1
			p.Message = "Reached the chain tip. Checking the wallet's history block by block; this is the slow part of a first sync."
		case found == 0 || height <= start || target <= start:
			p.Percent = -1
			p.Message = fmt.Sprintf("Checking every block since block %d for the wallet's %d addresses. Progress shows once the first transaction is found.", start, addresses)
		default:
			p.Percent = float64(height-start) / float64(target-start) * 100
			if p.Percent > 99 {
				p.Percent = 99
			}
			p.Message = fmt.Sprintf("Checked the wallet's history at least through block %d of %d, %d transactions found so far. Progress moves each time a transaction is found.", height, target, found)
		}
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

// waitChannelsActive gives lnd a moment to reconnect to channel peers (the
// LSP) so a cooperative close has a chance. Returns once every channel is
// active or the timeout passes.
func (n *node) waitChannelsActive(ctx context.Context, timeout time.Duration, progressf func(string, ...interface{})) {
	deadline := time.Now().Add(timeout)
	reported := false
	for time.Now().Before(deadline) {
		if info, err := n.info(ctx); err == nil {
			if info.NumInactiveChannels == 0 {
				return
			}
			if !reported {
				progressf("Waiting for channel peers to come online (%d active, %d inactive)...", info.NumActiveChannels, info.NumInactiveChannels)
				reported = true
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
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

// forceClose requests a unilateral close and returns the closing txid.
func (n *node) forceClose(ctx context.Context, channelPoint string) (string, error) {
	parts := strings.SplitN(channelPoint, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("bad channel point %q", channelPoint)
	}
	index, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stream, err := n.client.CloseChannel(c, &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{FundingTxidStr: parts[0]},
			OutputIndex: uint32(index),
		},
		Force: true,
	})
	if err != nil {
		return "", err
	}
	update, err := stream.Recv()
	if err != nil {
		return "", err
	}
	if p := update.GetClosePending(); p != nil {
		h, err := chainhash.NewHash(p.Txid)
		if err != nil {
			return "", err
		}
		return h.String(), nil
	}
	return "", errors.New("unexpected close update")
}

func (n *node) status(ctx context.Context) (*Status, error) {
	info, err := n.info(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{
		NodeID:      info.IdentityPubkey,
		BlockHeight: info.BlockHeight,
		Synced:      info.SyncedToChain,
		Peers:       info.NumPeers,
		Channels:    []Channel{},
		Pending:     []PendingClose{},
		Warnings:    []string{},
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
		st.Channels = append(st.Channels, Channel{
			ChannelPoint:  c.ChannelPoint,
			Peer:          c.RemotePubkey,
			Capacity:      c.Capacity,
			LocalBalance:  c.LocalBalance,
			RemoteBalance: c.RemoteBalance,
			Active:        c.Active,
		})
		st.InChannels += c.LocalBalance
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
	for _, c := range pend.PendingForceClosingChannels {
		st.Pending = append(st.Pending, PendingClose{ChannelPoint: c.Channel.ChannelPoint, Kind: "force", ClosingTxID: c.ClosingTxid, Amount: c.LimboBalance, BlocksToMature: c.BlocksTilMaturity})
		st.InPending += c.LimboBalance
	}
	return st, nil
}
