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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/backup"
	"github.com/breez/breez/bindings"
	"github.com/breez/breez/data"
	"github.com/btcsuite/btcd/wire"
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

// DefaultPeers are the bitcoin peers neutrino connects to when none are
// configured: the two Breez nodes with compact filters, as on the phone.
// The list is exclusive, see writeConfigs.
var DefaultPeers = []string{"bb1.breez.technology", "bb2.breez.technology"}

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
	// means DefaultPeers. Neutrino connects only to the pinned peers.
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

	gsnaps  []Snapshot
	icloud  *icloudClient
	records map[string]ckRecord
	sweep   *sweepPlan
	// restored is set when this process ran a restore. The library's
	// services are torn down by the restore and starting them again in
	// the same process crashes (the account service subscribes to
	// invoices before lnd serves and then reads a nil stream), so the
	// node is started by a fresh process.
	restored bool

	// nodeDir is the folder of the backup in use, see dirs.go.
	nodeDir   string
	layoutErr error

	// dustLogged remembers which dust channels the log already explained.
	dustLogged map[string]bool

	// checks holds the chain check's verdict on every channel lnd lists
	// as open. See CheckChannelsOnChain.
	checks channelChecks
}

// New creates a session. It also captures the library's stdout logging into
// the reporter, once per process.
func New(cfg Config, rep Reporter) *Core {
	c := &Core{cfg: cfg, rep: rep}
	if err := c.loadLayout(); err != nil {
		// Reported by every operation that needs the folder.
		c.layoutErr = err
	}
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
	if c.dir() == "" {
		return ""
	}
	return filepath.Join(c.dir(), "logs", "bitcoin", c.cfg.Network, "lnd.log")
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

// HasRestoredNode reports whether a restored backup is selected.
func (c *Core) HasRestoredNode() bool {
	return hasNode(c.dir(), c.cfg.Network)
}

// ErrNodeExists is returned by the restore functions when this backup is
// already restored on this computer and force is false.
var ErrNodeExists = errors.New("this backup is already restored on this computer")

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
	out, err := listDriveSnapshots(ctx, auth)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no Breez backups found in this Google account")
	}
	c.gsnaps = out
	c.progressf("Found %d backup(s) in Google Drive.", len(out))
	return out, nil
}

// GoogleRestore downloads a snapshot from Drive, decrypts it and places the
// node files in the work dir. Drive marks the snapshot as restored by this
// machine, exactly as a new phone would.
func (c *Core) GoogleRestore(ctx context.Context, nodeID, mnemonic string, force bool) error {
	if c.gsnaps == nil {
		if _, err := c.GoogleSnapshots(ctx); err != nil {
			return err
		}
	}
	snap, err := findSnapshot(c.gsnaps, nodeID)
	if err != nil {
		return err
	}
	key, err := keyForSnapshot(snap, mnemonic)
	if err != nil {
		return err
	}
	if err := c.prepareBackupDir(snap.NodeID, force); err != nil {
		return err
	}
	// The library downloads, decrypts and places the files. It is
	// initialised here, on this backup's own folder, and not before: its
	// databases are process-wide and stay bound to the first folder.
	auth, err := c.newGoogleAuth(ctx)
	if err != nil {
		return err
	}
	if err := c.initLibrary(newServices("gdrive", auth)); err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s)...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	if err := bindings.RestoreBackup(snap.NodeID, key); err != nil {
		return fmt.Errorf("restore failed: %w (wrong backup phrase?)", err)
	}
	c.progressf("Backup restored into %s.", c.dir())
	c.restored = true
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
	if err := c.prepareBackupDir(snap.NodeID, force); err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s) from iCloud...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	paths, err := c.icloud.download(c.dir(), c.records[snap.NodeID])
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
	c.progressf("Backup restored into %s.", c.dir())
	c.restored = true
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
	name, err := zipBackupName(zipPath)
	if err != nil {
		return err
	}
	if err := c.prepareBackupDir(name, force); err != nil {
		return err
	}
	if err := c.writeConfigs(); err != nil {
		return err
	}
	c.progressf("Reading %s...", filepath.Base(zipPath))
	if err := c.restoreFromZip(zipPath, key); err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.dir())
	c.restored = true
	return nil
}

// ---- node ------------------------------------------------------------------

// initLibrary prepares the breez app without starting lnd. A second call
// with a different provider re-initialises.
func (c *Core) initLibrary(svc *services) error {
	if c.dir() == "" {
		return errors.New("no backup selected")
	}
	if boundLibDir != "" && boundLibDir != c.dir() {
		return ErrLibraryBound
	}
	if c.initialized && c.svc != nil && svc.providerName == c.svc.providerName {
		return nil
	}
	if err := c.writeConfigs(); err != nil {
		return err
	}
	tmp := filepath.Join(c.dir(), "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return err
	}
	// Bound from here on, even when Init fails half way: the library may
	// already hold this folder's databases.
	boundLibDir = c.dir()
	if err := bindings.Init(tmp, c.dir(), svc); err != nil {
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
	if c.layoutErr != nil {
		return c.layoutErr
	}
	if !c.HasRestoredNode() {
		return fmt.Errorf("no restored backup in %s", c.cfg.WorkDir)
	}
	if c.restored {
		c.progressf("Backup restored; the app restarts to open it...")
		return ErrRestartRequired
	}
	svc := c.svc
	if svc == nil {
		svc = newServices("", nil)
	}
	if err := c.initLibrary(svc); err != nil {
		return err
	}
	if err := c.checkPeers(ctx); err != nil {
		return err
	}
	c.progressf("Starting the node...")
	nodeCfg := c.cfg
	nodeCfg.WorkDir = c.dir()
	n, err := startNode(ctx, nodeCfg, svc)
	if err != nil {
		return err
	}
	c.node = n
	c.progressf("Node is up.")

	// First start after a restore: derive the address look-ahead, then
	// make the wallet check its history again from the start so the new
	// addresses are covered. Marked so it happens once per restore.
	marker := filepath.Join(c.dir(), addressesExtendedFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	// lnd must not start far behind the chain tip. Started at the bootstrap
	// checkpoint (seen: block 812,000 of 967,857) its chain notifier walks
	// every block up to the tip downloading full blocks, 4 a second, and
	// only acts on a channel close once it gets there: about 10 hours, with
	// the closed channel's funds invisible meanwhile. Started at the tip
	// there is nothing to walk and old closes are found through the filter
	// scan. The headers take about a minute; the next start is the one that
	// counts, so wait for them here.
	c.progressf("Catching up with the bitcoin chain...")
	if err := waitHeadersSynced(ctx, 15*time.Minute); err != nil {
		return err
	}
	c.progressf("Preparing addresses to check, so funds received after the last backup are found too...")
	if err := n.extendAddresses(ctx, c.progressf); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.dir(), forceRescanFile), nil, 0600); err != nil {
		return err
	}
	// The library cannot be re-initialised in-process after a stop (it
	// hangs), so the caller restarts the whole program. Stop can hang too
	// on some nodes after lnd itself is down, so give it a bounded wait;
	// the program exit releases whatever is left.
	c.progressf("Stopping the node; the app restarts to check the history with the new addresses...")
	if !c.StopWithin(20 * time.Second) {
		c.progressf("The node did not stop cleanly; the program exits and starts again.")
	}
	return ErrRestartRequired
}

// StopWithin calls Stop and reports whether it finished within d.
func (c *Core) StopWithin(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// ErrRestartRequired is returned by StartNode when the program must be
// started again for the node to pick up a change (the address look-ahead
// after a restore).
var ErrRestartRequired = errors.New("restart required")

const (
	// addressesExtendedFile marks a work dir whose wallet had the address
	// look-ahead derived.
	addressesExtendedFile = "addresses-extended"
	// forceRescanFile is the marker the breez library checks on Init: it
	// drops the wallet's transaction store and rescans from the birthday.
	forceRescanFile = "FORCE_RESCAN"
)

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
	// Remaining is a rough estimate in seconds from the rate so far, or
	// -1 while there is not enough data.
	Remaining int64 `json:"remaining"`
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

// Channel is an open channel of the restored node.
type Channel struct {
	ChannelPoint  string `json:"channelPoint"`
	Peer          string `json:"peer"`
	Capacity      int64  `json:"capacity"`
	LocalBalance  int64  `json:"localBalance"`
	RemoteBalance int64  `json:"remoteBalance"`
	Active        bool   `json:"active"`
	// Dust says LocalBalance is below DustLimit, the smallest output a
	// close of this channel can carry: it can never be paid out.
	Dust      bool  `json:"dust"`
	DustLimit int64 `json:"dustLimit"`
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
	NodeID             string    `json:"nodeId"`
	BlockHeight        uint32    `json:"blockHeight"`
	Synced             bool      `json:"synced"`
	Peers              uint32    `json:"peers"`
	OnchainConfirmed   int64     `json:"onchainConfirmed"`
	OnchainUnconfirmed int64     `json:"onchainUnconfirmed"`
	Channels           []Channel `json:"channels"`
	// ClosedOnChain lists channels lnd still calls open although the chain
	// says otherwise: they closed after this backup was taken.
	ClosedOnChain []SpentChannel `json:"closedOnChain"`
	Pending       []PendingClose `json:"pending"`
	InChannels    int64          `json:"inChannels"`
	InPending     int64          `json:"inPending"`
	// Warnings lists parts of the status that could not be read.
	Warnings []string `json:"warnings"`
}

// Status queries the running node.
func (c *Core) Status(ctx context.Context) (*Status, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	st, err := c.node.status(ctx, &c.checks)
	if err != nil {
		return nil, err
	}
	// The log says once why a dust channel does not count.
	for _, ch := range st.Channels {
		if !ch.Dust {
			continue
		}
		if c.dustLogged == nil {
			c.dustLogged = map[string]bool{}
		}
		if !c.dustLogged[ch.ChannelPoint] {
			c.dustLogged[ch.ChannelPoint] = true
			c.progressf("Channel %s: %d sat is dust (limit %d), not counted.", ch.ChannelPoint, ch.LocalBalance, ch.DustLimit)
		}
	}
	return st, nil
}

// ValidateAddress checks a bitcoin address for the configured network.
func ValidateAddress(address string) error {
	return bindings.ValidateAddress(address)
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

// Lncli runs an lncli command against the running node. Commands that
// close a channel are refused: the tool closes nothing, see CLAUDE.md.
func (c *Core) Lncli(ctx context.Context, command string) (string, error) {
	if c.node == nil {
		return "", errors.New("node not started")
	}
	for _, word := range strings.Fields(command) {
		switch strings.ToLower(word) {
		case "closechannel", "closeallchannels", "abandonchannel":
			return "", fmt.Errorf("%s is not available: a backup can hold an old channel state, and closing with it can lose the channel's funds", word)
		}
	}
	return bindings.SendCommand(command)
}

// NodeRunning reports whether the embedded node has been started.
func (c *Core) NodeRunning() bool { return c.node != nil }

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

// checkPeers makes sure at least one pinned bitcoin peer completes a
// bitcoin protocol handshake before the node starts. Neutrino connects
// only to the pinned peers, so a dead list would leave the sync waiting
// forever with no message. A TCP connect alone is not enough: a hung node
// still accepts connections (bb1 did in 2026-09).
func (c *Core) checkPeers(ctx context.Context) error {
	var reasons []string
	ok := 0
	for _, p := range c.peers() {
		if err := bitcoinHandshake(ctx, p); err != nil {
			reasons = append(reasons, p+": "+err.Error())
			c.progressf("Bitcoin peer %s does not answer: %v", p, err)
			continue
		}
		ok++
		c.progressf("Bitcoin peer %s answers.", p)
	}
	if ok > 0 {
		return nil
	}
	return fmt.Errorf("none of the bitcoin peers answers (%s). Check the internet connection, or set other peers with compact filters in Advanced settings", strings.Join(reasons, "; "))
}

// bitcoinHandshake sends a version message and waits for the peer's.
func bitcoinHandshake(ctx context.Context, peer string) error {
	host := peer
	if _, _, err := net.SplitHostPort(peer); err != nil {
		host = net.JoinHostPort(peer, "8333")
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", host)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	me := wire.NewNetAddressIPPort(net.IPv4zero, 0, 0)
	ver := wire.NewMsgVersion(me, me, uint64(time.Now().UnixNano()), 0)
	if err := wire.WriteMessage(conn, ver, wire.ProtocolVersion, wire.MainNet); err != nil {
		return fmt.Errorf("send version: %w", err)
	}
	for {
		msg, _, err := wire.ReadMessage(conn, wire.ProtocolVersion, wire.MainNet)
		if err != nil {
			return fmt.Errorf("no version reply: %w", err)
		}
		if v, ok := msg.(*wire.MsgVersion); ok {
			if !v.HasService(wire.SFNodeCF) {
				return errors.New("does not serve compact filters")
			}
			return nil
		}
	}
}
