// Package core restores a Breez mobile node from its cloud backup on a
// desktop machine and moves the funds out to an on-chain address.
//
// It reuses the breez library unchanged: the same Google Drive provider, the
// same restore code and the same lnd fork the mobile app runs. The command
// line tool and the desktop app are thin layers over this package.
package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/backup"
	"github.com/breez/breez/bindings"
	"github.com/breez/breez/data"
	"google.golang.org/protobuf/proto"
)

// Production values from the mobile app's bundled breez.conf and lnd.conf.
const (
	DefaultBreezServer       = "bs1.breez.technology:443"
	DefaultBootstrapURL      = "https://bt2.breez.technology"
	DefaultClosedChannelsURL = "https://cc1.breez.technology/pruned"
	// lnd requires an external fee estimator when running neutrino on mainnet.
	DefaultFeeURL = "https://nd1.breez.technology/fees/v1/btc-fee-estimates.json"
)

// DefaultExtraPeers are added to neutrino next to DNS seed discovery when
// no peers are pinned. bb1 is already gone; bb2 is the remaining Breez
// compact-filter node.
var DefaultExtraPeers = []string{"bb2.breez.technology"}

// Build-time values. The Google client is a "Desktop app" OAuth client of
// the Breez Google Cloud project; its secret is not confidential by Google's
// own definition. The LSP token is only needed for LSP features, not for
// closing channels or sweeping.
//
//	go build -ldflags "-X github.com/breez/breez-mobile-recovery/core.GoogleClientID=... \
//	                   -X github.com/breez/breez-mobile-recovery/core.GoogleClientSecret=... \
//	                   -X github.com/breez/breez-mobile-recovery/core.LSPToken=..."
var (
	GoogleClientID     = ""
	GoogleClientSecret = ""
	LSPToken           = ""
)

// Config holds everything the tool needs to know about where the node lives
// and which services it talks to.
type Config struct {
	WorkDir           string
	Network           string
	BreezServer       string
	BootstrapURL      string
	ClosedChannelsURL string
	FeeURL            string
	// Peers pins bitcoin peers with compact filters, comma separated. Empty
	// means discovery through the DNS seeds, which keeps the tool working
	// after the Breez hosts are gone.
	Peers    string
	LSPToken string
	// LogLevel is lnd's debuglevel, "info" by default.
	LogLevel           string
	GoogleClientID     string
	GoogleClientSecret string
	ICloudAPIToken     string
}

// DefaultConfig returns the production configuration with the work dir
// under the user's home.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		WorkDir:            firstNonEmpty(os.Getenv("BREEZ_RECOVERY_WORKDIR"), filepath.Join(home, ".breez-recovery")),
		Network:            "mainnet",
		BreezServer:        DefaultBreezServer,
		BootstrapURL:       DefaultBootstrapURL,
		ClosedChannelsURL:  DefaultClosedChannelsURL,
		FeeURL:             DefaultFeeURL,
		LSPToken:           LSPToken,
		LogLevel:           "info",
		GoogleClientID:     firstNonEmpty(os.Getenv("BREEZ_GOOGLE_CLIENT_ID"), GoogleClientID),
		GoogleClientSecret: firstNonEmpty(os.Getenv("BREEZ_GOOGLE_CLIENT_SECRET"), GoogleClientSecret),
		ICloudAPIToken:     icloudAPIToken,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Reporter receives what the tool wants to tell the user.
type Reporter interface {
	// Progress is a human readable line about what the tool is doing.
	Progress(msg string)
	// NodeLog is a raw log line from the breez library or lnd.
	NodeLog(line string)
	// SignIn is called when a browser sign-in was started at url, so the
	// caller can show the link in case the browser did not open.
	SignIn(provider, url string)
}

// Snapshot describes one node backup in the cloud.
type Snapshot struct {
	NodeID         string    `json:"nodeId"`
	BackupID       string    `json:"backupId"`
	ModifiedTime   time.Time `json:"modifiedTime"`
	Encrypted      bool      `json:"encrypted"`
	EncryptionType string    `json:"encryptionType"` // "Mnemonics", "Mnemonics12", "" (none) or "PIN" (unsupported)
}

// Core is one restore session. It is not safe for concurrent use; the
// desktop app serialises calls.
type Core struct {
	cfg Config
	rep Reporter

	svc         *services
	initialized bool
	node        *node

	gsnaps  []backup.SnapshotInfo
	icloud  *icloudClient
	records map[string]ckRecord
	sweep   *sweepPlan
}

// New creates a session. It also captures the library's stdout logging into
// the reporter, once per process.
func New(cfg Config, rep Reporter) *Core {
	c := &Core{cfg: cfg, rep: rep}
	setNodeLogSink(func(line string) {
		if trackRescan(line) {
			return // progress only, not worth a log line
		}
		rep.NodeLog(line)
	})
	return c
}

// Config returns the session configuration.
func (c *Core) Config() Config { return c.cfg }

// LogPath is the lnd log file of the restored node.
func (c *Core) LogPath() string {
	return filepath.Join(c.cfg.WorkDir, "logs", "bitcoin", c.cfg.Network, "lnd.log")
}

func (c *Core) progressf(format string, args ...interface{}) {
	c.rep.Progress(fmt.Sprintf(format, args...))
}

// ---- library stdout capture ----------------------------------------------

var (
	logSinkMu   sync.Mutex
	logSink     func(string)
	captureOnce sync.Once
)

// setNodeLogSink redirects os.Stdout, which the breez library and lnd use
// for their logs, into the given sink. The redirect happens once; later
// calls only swap the sink.
func setNodeLogSink(sink func(string)) {
	logSinkMu.Lock()
	logSink = sink
	logSinkMu.Unlock()
	captureOnce.Do(func() {
		r, w, err := os.Pipe()
		if err != nil {
			return
		}
		os.Stdout = w
		go func() {
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				logSinkMu.Lock()
				s := logSink
				logSinkMu.Unlock()
				if s != nil {
					s(sc.Text())
				}
			}
			io.Copy(io.Discard, r)
		}()
	})
}

// ---- restore state -------------------------------------------------------

// HasRestoredNode reports whether the work dir already holds a wallet.
func (c *Core) HasRestoredNode() bool {
	_, err := os.Stat(filepath.Join(c.cfg.WorkDir, "data", "chain", "bitcoin", c.cfg.Network, "wallet.db"))
	return err == nil
}

// ErrNodeExists is returned by the restore functions when the work dir
// already holds a node and force is false.
var ErrNodeExists = errors.New("the work dir already holds a restored wallet")

func (c *Core) guardRestore(force bool) error {
	if c.HasRestoredNode() && !force {
		return ErrNodeExists
	}
	if c.node != nil {
		c.progressf("Stopping the running node before restoring over it...")
		c.Stop()
	}
	return os.MkdirAll(c.cfg.WorkDir, 0700)
}

// ValidateMnemonic checks a backup phrase and returns the encryption type it
// corresponds to ("Mnemonics" for 24 words, "Mnemonics12" for 12).
func ValidateMnemonic(mnemonic string) (string, error) {
	_, encType, err := backupKeyFromMnemonic(mnemonic)
	return encType, err
}

// keyForSnapshot returns the decryption key for a snapshot, or nil for an
// unencrypted one.
func keyForSnapshot(snap Snapshot, mnemonic string) ([]byte, error) {
	if !snap.Encrypted {
		return nil, nil
	}
	if !strings.HasPrefix(snap.EncryptionType, "Mnemonics") {
		return nil, errors.New("this backup uses the deprecated PIN encryption, which the tool does not support")
	}
	if strings.TrimSpace(mnemonic) == "" {
		return nil, fmt.Errorf("this backup is encrypted, the %s backup phrase is needed", describe(snap.EncryptionType))
	}
	key, encType, err := backupKeyFromMnemonic(mnemonic)
	if err != nil {
		return nil, err
	}
	if encType != snap.EncryptionType {
		return nil, fmt.Errorf("the backup expects a %s phrase but a %s phrase was given", describe(snap.EncryptionType), describe(encType))
	}
	return key, nil
}

func describe(encType string) string {
	if encType == "Mnemonics12" {
		return "12-word"
	}
	return "24-word"
}

func convertSnapshots(in []backup.SnapshotInfo) []Snapshot {
	out := make([]Snapshot, 0, len(in))
	for _, s := range in {
		enc := s.EncryptionType
		if s.Encrypted && enc == "" {
			enc = "PIN"
		}
		out = append(out, Snapshot{
			NodeID:         s.NodeID,
			BackupID:       s.BackupID,
			ModifiedTime:   s.ModifiedTime,
			Encrypted:      s.Encrypted,
			EncryptionType: enc,
		})
	}
	return out
}

func findSnapshot(snaps []Snapshot, nodeID string) (Snapshot, error) {
	if nodeID == "" {
		if len(snaps) == 1 {
			return snaps[0], nil
		}
		return Snapshot{}, errors.New("several backups found, pick one by node id")
	}
	for _, s := range snaps {
		if s.NodeID == nodeID {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("node id %s not found among the backups", nodeID)
}

// ---- Google Drive ----------------------------------------------------------

// GoogleSnapshots signs in to Google (browser, or cached token) and lists
// the backups in the account's hidden Breez app folder.
func (c *Core) GoogleSnapshots(ctx context.Context) ([]Snapshot, error) {
	auth, err := c.newGoogleAuth(ctx)
	if err != nil {
		return nil, err
	}
	c.progressf("Signed in to Google. Looking for Breez backups...")
	if err := c.initLibrary(newServices("gdrive", auth)); err != nil {
		return nil, err
	}
	res, err := bindings.AvailableSnapshots()
	if err != nil {
		if err.Error() == "empty" {
			return nil, errors.New("no Breez backups found in this Google account")
		}
		return nil, err
	}
	var snaps []backup.SnapshotInfo
	if err := json.Unmarshal([]byte(res), &snaps); err != nil {
		return nil, err
	}
	c.gsnaps = snaps
	out := convertSnapshots(snaps)
	c.progressf("Found %d backup(s) in Google Drive.", len(out))
	return out, nil
}

// GoogleRestore downloads a snapshot from Drive, decrypts it and places the
// node files in the work dir. Drive marks the snapshot as restored by this
// machine, exactly as a new phone would.
func (c *Core) GoogleRestore(ctx context.Context, nodeID, mnemonic string, force bool) error {
	if c.gsnaps == nil || c.svc == nil || c.svc.providerName != "gdrive" {
		if _, err := c.GoogleSnapshots(ctx); err != nil {
			return err
		}
	}
	snap, err := findSnapshot(convertSnapshots(c.gsnaps), nodeID)
	if err != nil {
		return err
	}
	key, err := keyForSnapshot(snap, mnemonic)
	if err != nil {
		return err
	}
	if err := c.guardRestore(force); err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s)...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	if err := bindings.RestoreBackup(snap.NodeID, key); err != nil {
		return fmt.Errorf("restore failed: %w (wrong backup phrase?)", err)
	}
	c.progressf("Backup restored into %s.", c.cfg.WorkDir)
	return nil
}

// ---- iCloud ----------------------------------------------------------------

// ICloudSnapshots signs in with the Apple ID (browser, or cached session)
// and lists the BackupSnapshot records of the Breez container.
func (c *Core) ICloudSnapshots(ctx context.Context) ([]Snapshot, error) {
	client, err := c.icloudSignIn(ctx)
	if err != nil {
		return nil, err
	}
	c.progressf("Signed in to iCloud. Looking for Breez backups...")
	snaps, records, err := client.snapshots()
	if err != nil {
		return nil, err
	}
	c.icloud, c.records = client, records
	out := convertSnapshots(snaps)
	c.progressf("Found %d backup(s) in iCloud.", len(out))
	return out, nil
}

// ICloudRestore downloads a snapshot from iCloud, decrypts it and places
// the node files in the work dir.
func (c *Core) ICloudRestore(ctx context.Context, nodeID, mnemonic string, force bool) error {
	if c.icloud == nil {
		if _, err := c.ICloudSnapshots(ctx); err != nil {
			return err
		}
	}
	snaps, _, err := c.icloud.snapshots()
	if err != nil {
		return err
	}
	snap, err := findSnapshot(convertSnapshots(snaps), nodeID)
	if err != nil {
		return err
	}
	key, err := keyForSnapshot(snap, mnemonic)
	if err != nil {
		return err
	}
	if err := c.guardRestore(force); err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s) from iCloud...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	paths, err := c.icloud.download(c.cfg.WorkDir, c.records[snap.NodeID])
	if err != nil {
		return err
	}
	defer func() {
		for _, p := range paths {
			os.Remove(p)
		}
	}()
	if err := c.writeConfigs(); err != nil {
		return err
	}
	c.progressf("Decrypting and placing the node files...")
	if len(paths) == 1 {
		err = c.restoreFromZip(paths[0], key)
	} else {
		err = c.restoreFromPaths(paths, key)
	}
	if err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.cfg.WorkDir)
	return nil
}

// ---- local zip -------------------------------------------------------------

// ZipNeedsPhrase inspects a backup zip and reports whether its files are
// encrypted, without restoring anything.
func ZipNeedsPhrase(zipPath string) (bool, error) {
	return zipIsEncrypted(zipPath)
}

// ZipRestore places the node files from a backup zip into the work dir.
func (c *Core) ZipRestore(zipPath, mnemonic string, force bool) error {
	var key []byte
	if strings.TrimSpace(mnemonic) != "" {
		var err error
		key, _, err = backupKeyFromMnemonic(mnemonic)
		if err != nil {
			return err
		}
	}
	if err := c.guardRestore(force); err != nil {
		return err
	}
	if err := c.writeConfigs(); err != nil {
		return err
	}
	c.progressf("Reading %s...", filepath.Base(zipPath))
	if err := c.restoreFromZip(zipPath, key); err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.cfg.WorkDir)
	return nil
}

// ---- node ------------------------------------------------------------------

// initLibrary prepares the breez app without starting lnd. A second call
// with a different provider re-initialises.
func (c *Core) initLibrary(svc *services) error {
	if c.initialized && c.svc != nil && svc.providerName == c.svc.providerName {
		return nil
	}
	if err := c.writeConfigs(); err != nil {
		return err
	}
	tmp := filepath.Join(c.cfg.WorkDir, "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return err
	}
	if err := bindings.Init(tmp, c.cfg.WorkDir, svc); err != nil {
		return err
	}
	c.svc = svc
	c.initialized = true
	return nil
}

// StartNode starts the embedded lnd on the restored node and returns once
// its RPC answers. Sync is a separate step, see WaitSynced.
func (c *Core) StartNode(ctx context.Context) error {
	if c.node != nil {
		return nil
	}
	if !c.HasRestoredNode() {
		return fmt.Errorf("no restored wallet in %s", c.cfg.WorkDir)
	}
	svc := c.svc
	if svc == nil {
		svc = newServices("", nil)
	}
	if err := c.initLibrary(svc); err != nil {
		return err
	}
	c.progressf("Starting the node...")
	n, err := startNode(ctx, c.cfg, svc)
	if err != nil {
		return err
	}
	c.node = n
	c.progressf("Node is up.")
	return nil
}

// SyncProgress is reported while the node catches up with the chain.
type SyncProgress struct {
	Stage   string  `json:"stage"` // "connecting", "headers", "rescan", "synced"
	Height  uint32  `json:"height"`
	Target  uint32  `json:"target"`  // estimated chain tip
	Percent float64 `json:"percent"` // -1 while unknown
	Peers   uint32  `json:"peers"`
	Message string  `json:"message"`
	// During the history check: transactions found so far and the time
	// of the newest one, which is how far in time the check has got.
	Found       int   `json:"found"`
	ThroughTime int64 `json:"throughTime"`
}

// WaitSynced blocks until lnd reports synced_to_chain, calling onProgress
// on every change.
func (c *Core) WaitSynced(ctx context.Context, onProgress func(SyncProgress)) error {
	if c.node == nil {
		return errors.New("node not started")
	}
	return c.node.waitSynced(ctx, func(p SyncProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	})
}

// WaitChannelsActive gives lnd up to timeout to reconnect to channel peers
// so cooperative closes have a chance. It returns early once every channel
// is active.
func (c *Core) WaitChannelsActive(ctx context.Context, timeout time.Duration) {
	if c.node == nil {
		return
	}
	c.node.waitChannelsActive(ctx, timeout, c.progressf)
}

// Channel is an open channel of the restored node.
type Channel struct {
	ChannelPoint  string `json:"channelPoint"`
	Peer          string `json:"peer"`
	Capacity      int64  `json:"capacity"`
	LocalBalance  int64  `json:"localBalance"`
	RemoteBalance int64  `json:"remoteBalance"`
	Active        bool   `json:"active"`
}

// PendingClose is a channel whose close is in flight.
type PendingClose struct {
	ChannelPoint   string `json:"channelPoint"`
	Kind           string `json:"kind"` // "waiting", "cooperative", "force"
	ClosingTxID    string `json:"closingTxid"`
	Amount         int64  `json:"amount"`
	BlocksToMature int32  `json:"blocksToMature"`
}

// Status is a snapshot of the restored node.
type Status struct {
	NodeID             string         `json:"nodeId"`
	BlockHeight        uint32         `json:"blockHeight"`
	Synced             bool           `json:"synced"`
	Peers              uint32         `json:"peers"`
	OnchainConfirmed   int64          `json:"onchainConfirmed"`
	OnchainUnconfirmed int64          `json:"onchainUnconfirmed"`
	Channels           []Channel      `json:"channels"`
	Pending            []PendingClose `json:"pending"`
	InChannels         int64          `json:"inChannels"`
	InPending          int64          `json:"inPending"`
	// Warnings lists parts of the status that could not be read.
	Warnings []string `json:"warnings"`
}

// Status queries the running node.
func (c *Core) Status(ctx context.Context) (*Status, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	return c.node.status(ctx)
}

// ValidateAddress checks a bitcoin address for the configured network.
func ValidateAddress(address string) error {
	return bindings.ValidateAddress(address)
}

// ChannelCloseResult is the outcome of closing one channel.
type ChannelCloseResult struct {
	ChannelPoint string `json:"channelPoint"`
	Status       string `json:"status"` // "closing", "force_closing", "skipped", "failed"
	TxID         string `json:"txid"`
	Error        string `json:"error"`
}

// CloseResult summarises a close run.
type CloseResult struct {
	Channels []ChannelCloseResult `json:"channels"`
	Closed   int                  `json:"closed"`
	Skipped  int                  `json:"skipped"`
}

// CloseChannels asks every channel peer for a cooperative close with the
// funds paid straight to address. Channels whose peer is offline are
// skipped, or force closed when force is set. It keeps the node running a
// little so the closing transactions get broadcast.
func (c *Core) CloseChannels(ctx context.Context, address string, force bool) (*CloseResult, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	if err := bindings.ValidateAddress(address); err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}
	chans, err := c.node.openChannels(ctx)
	if err != nil {
		return nil, err
	}
	if len(chans) == 0 {
		return &CloseResult{}, nil
	}
	c.progressf("Closing %d channel(s) cooperatively, funds to %s...", len(chans), address)
	res, err := bindings.CloseChannels(address)
	if err != nil {
		return nil, fmt.Errorf("close channels: %w", err)
	}
	var reply data.CloseChannelsReply
	if err := proto.Unmarshal(res, &reply); err != nil {
		return nil, err
	}
	result := &CloseResult{}
	var inactive []string
	for _, ch := range reply.Channels {
		switch {
		case ch.IsSkipped:
			c.progressf("  %s: skipped, peer offline", ch.ChannelPoint)
			result.Channels = append(result.Channels, ChannelCloseResult{ChannelPoint: ch.ChannelPoint, Status: "skipped"})
			inactive = append(inactive, ch.ChannelPoint)
		case ch.FailErr != "":
			c.progressf("  %s: failed: %s", ch.ChannelPoint, ch.FailErr)
			result.Channels = append(result.Channels, ChannelCloseResult{ChannelPoint: ch.ChannelPoint, Status: "failed", Error: ch.FailErr})
			inactive = append(inactive, ch.ChannelPoint)
		default:
			c.progressf("  %s: closing, tx %s", ch.ChannelPoint, ch.ClosingTxid)
			result.Channels = append(result.Channels, ChannelCloseResult{ChannelPoint: ch.ChannelPoint, Status: "closing", TxID: ch.ClosingTxid})
			result.Closed++
		}
	}
	if force {
		for i := range result.Channels {
			r := &result.Channels[i]
			if r.Status != "skipped" && r.Status != "failed" {
				continue
			}
			txid, err := c.node.forceClose(ctx, r.ChannelPoint)
			if err != nil {
				c.progressf("  %s: force close failed: %v", r.ChannelPoint, err)
				r.Status, r.Error = "failed", err.Error()
				continue
			}
			c.progressf("  %s: force closing, tx %s", r.ChannelPoint, txid)
			r.Status, r.TxID, r.Error = "force_closing", txid, ""
			result.Closed++
		}
	}
	for _, r := range result.Channels {
		if r.Status == "skipped" || r.Status == "failed" {
			result.Skipped++
		}
	}
	if result.Closed > 0 {
		c.progressf("Keeping the node running so the closing transactions are broadcast...")
		select {
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
		}
	}
	return result, nil
}

// SweepOption is one fee choice for the sweep transaction.
type SweepOption struct {
	ConfTarget int    `json:"confTarget"` // blocks
	Fee        int64  `json:"fee"`
	TxID       string `json:"txid"`
}

// SweepPlan is a prepared sweep of the whole on-chain balance.
type SweepPlan struct {
	Address string        `json:"address"`
	Amount  int64         `json:"amount"`
	Options []SweepOption `json:"options"`
}

type sweepPlan struct {
	SweepPlan
	txs map[int][]byte
}

// PrepareSweep builds the sweep transactions for the confirmed on-chain
// balance at the three fee targets, without broadcasting.
func (c *Core) PrepareSweep(ctx context.Context, address string) (*SweepPlan, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	if err := bindings.ValidateAddress(address); err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}
	wb, err := c.node.walletBalance(ctx)
	if err != nil {
		return nil, err
	}
	if wb.ConfirmedBalance == 0 {
		if pend, err := c.node.pending(ctx); err == nil && pend.TotalLimboBalance > 0 {
			return nil, fmt.Errorf("no confirmed on-chain balance yet; %d sat are still in pending channel closes, try again later", pend.TotalLimboBalance)
		}
		return nil, errors.New("no confirmed on-chain balance to sweep")
	}
	res, err := bindings.SweepAllCoinsTransactions(address)
	if err != nil {
		return nil, fmt.Errorf("prepare sweep: %w", err)
	}
	var txs data.SweepAllCoinsTransactions
	if err := proto.Unmarshal(res, &txs); err != nil {
		return nil, err
	}
	plan := &sweepPlan{SweepPlan: SweepPlan{Address: address, Amount: txs.Amt}, txs: map[int][]byte{}}
	for _, target := range []int{2, 6, 25} {
		tx, ok := txs.Transactions[int32(target)]
		if !ok {
			continue
		}
		plan.Options = append(plan.Options, SweepOption{ConfTarget: target, Fee: tx.Fees, TxID: tx.TxHash})
		plan.txs[target] = tx.Tx
	}
	if len(plan.Options) == 0 {
		return nil, errors.New("no sweep transaction could be prepared")
	}
	c.sweep = plan
	c.progressf("Prepared sweep of %d sat to %s.", plan.Amount, address)
	return &plan.SweepPlan, nil
}

// BroadcastSweep publishes the prepared sweep transaction for the given
// confirmation target and returns its txid.
func (c *Core) BroadcastSweep(confTarget int) (string, error) {
	if c.sweep == nil {
		return "", errors.New("no sweep prepared")
	}
	raw, ok := c.sweep.txs[confTarget]
	if !ok {
		return "", fmt.Errorf("no transaction prepared for a %d block target", confTarget)
	}
	var txid string
	for _, o := range c.sweep.Options {
		if o.ConfTarget == confTarget {
			txid = o.TxID
		}
	}
	if err := bindings.PublishTransaction(raw); err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}
	c.progressf("Broadcast sweep transaction %s.", txid)
	c.sweep = nil
	return txid, nil
}

// Lncli runs an lncli command against the running node.
func (c *Core) Lncli(ctx context.Context, command string) (string, error) {
	if c.node == nil {
		return "", errors.New("node not started")
	}
	return bindings.SendCommand(command)
}

// Stop shuts the node down if it is running.
func (c *Core) Stop() {
	if c.node != nil {
		c.node.close()
		c.node = nil
	}
	if c.initialized {
		bindings.Stop()
		c.initialized = false
		c.svc = nil
	}
}

func short(nodeID string) string {
	if len(nodeID) > 12 {
		return nodeID[:12] + "..."
	}
	return nodeID
}
