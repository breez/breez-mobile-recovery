// Package core restores a Breez mobile node from its cloud backup on a
// desktop machine and moves the funds out to an on-chain address.
//
// It reuses the breez library unchanged: the same Google Drive provider, the
// same restore code and the same lnd fork the mobile app runs. The command
// line tool and the desktop app are thin layers over this package.
package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/backup"
	"github.com/breez/breez/bindings"
	"github.com/breez/breez/data"
	"github.com/breez/breez/dropwtx"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
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
	// Without a home folder the work folder would silently become a
	// relative one, a different place on every start: leave it empty, which
	// every operation refuses, unless one is set.
	workDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		workDir = filepath.Join(home, ".breez-recovery")
	}
	return Config{
		WorkDir:            firstNonEmpty(os.Getenv("BREEZ_RECOVERY_WORKDIR"), workDir),
		Peers:              os.Getenv("BREEZ_RECOVERY_PEERS"), // set by the app for its restarted copy
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
	// nodeDir is the folder of the backup in use, see dirs.go.
	nodeDir   string
	layoutErr error

	// dustLogged remembers which dust channels the log already explained.
	dustLogged map[string]bool

	// checks holds the chain check's verdict on every channel lnd lists
	// as open. See CheckChannelsOnChain.
	checks channelChecks
	// walk is the last walk over the chain, kept so the next one carries
	// on from the block it ended at.
	walk *chainWalk
	// ownScripts are the scripts the wallet's own history check watches,
	// read before the node's start; nil when they could not be read.
	ownScripts [][]byte
	// skippedTo is the block the wallet was set to by the history
	// shortcut in this start, 0 when it was not used.
	skippedTo uint32
}

// New creates a session. It also captures the library's stdout logging into
// the reporter, once per process.
func New(cfg Config, rep Reporter) *Core {
	c := &Core{cfg: cfg, rep: rep}
	if cfg.WorkDir == "" {
		c.layoutErr = errors.New("no work folder: the home folder could not be found, set one in Advanced settings")
	} else if err := lockWorkDir(cfg.WorkDir); err != nil {
		c.layoutErr = err
	} else if err := c.loadLayout(); err != nil {
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
	return lndLogPath(c.dir(), c.cfg.Network)
}

func lndLogPath(dir, network string) string {
	return filepath.Join(dir, "logs", "bitcoin", network, "lnd.log")
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
	if err := c.checkRestoreTarget(snap.NodeID, force); err != nil {
		return err
	}
	auth, err := c.newGoogleAuth(ctx)
	if err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s)...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	backup, err := downloadDriveBackup(ctx, auth, snap.NodeID)
	if err != nil {
		return err
	}
	files, err := nodeFilesFrom(backup.files)
	if err != nil {
		return err
	}
	c.progressf("Decrypting and placing the node files...")
	decoded, err := decodeBackupFiles(files, key)
	if err != nil {
		return err
	}
	id, err := c.placeBackup(snap.NodeID, decoded, force)
	if err != nil {
		return err
	}
	// Last, once the backup is safely in place.
	if err := backup.markRestored(ctx, id); err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.dir())
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
	// Asked again now: the download links of a listing expire, and a newer
	// backup may have arrived since.
	snaps, records, err := c.icloud.snapshots()
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
	if err := c.checkRestoreTarget(snap.NodeID, force); err != nil {
		return err
	}
	c.progressf("Downloading backup of node %s (from %s) from iCloud...", short(snap.NodeID), snap.ModifiedTime.Local().Format("2006-01-02 15:04"))
	downloaded, err := c.icloud.download(records[snap.NodeID])
	if err != nil {
		return err
	}
	files, err := nodeFilesFrom(downloaded)
	if err != nil {
		return err
	}
	c.progressf("Decrypting and placing the node files...")
	decoded, err := decodeBackupFiles(files, key)
	if err != nil {
		return err
	}
	if _, err := c.placeBackup(snap.NodeID, decoded, force); err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.dir())
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
	if err := c.checkRestoreTarget(name, force); err != nil {
		return err
	}
	c.progressf("Reading %s...", filepath.Base(zipPath))
	files, err := readZip(zipPath)
	if err != nil {
		return err
	}
	decoded, err := decodeBackupFiles(files, key)
	if err != nil {
		return err
	}
	if _, err := c.placeBackup(name, decoded, force); err != nil {
		return err
	}
	c.progressf("Backup restored into %s.", c.dir())
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
	// A bitcoin node the user once set on the phone travels inside
	// breez.db and REPLACES the peers of breez.conf (chainservice/init.go
	// reads db.GetPeers with the config's as mere defaults). Such a peer is
	// often a home node long gone, and the peers this tool pins and tests
	// would never be used. An empty stored list makes the library fall back
	// to the configured ones.
	none, err := proto.Marshal(&data.Peers{})
	if err != nil {
		return err
	}
	if err := bindings.SetPeers(none); err != nil {
		return fmt.Errorf("reset the stored bitcoin peers: %w", err)
	}
	c.svc = svc
	c.initialized = true
	return nil
}

// StartNode starts the embedded lnd on the restored node and returns once
// its RPC answers. Sync is a separate step, see WaitSynced.
func (c *Core) StartNode(ctx context.Context) error {
	if c.node == nil {
		if err := c.startNode(ctx); err != nil {
			return err
		}
	}
	// Also when the node runs already: a first start whose wait was stopped
	// or timed out must not carry on as if it were done.
	return c.firstStart(ctx)
}

func (c *Core) startNode(ctx context.Context) error {
	if c.layoutErr != nil {
		return c.layoutErr
	}
	if !c.HasRestoredNode() {
		return fmt.Errorf("no restored backup in %s", c.cfg.WorkDir)
	}
	svc := c.svc
	if svc == nil {
		svc = newServices("", nil)
	}
	// A fresh history check was ordered (the search found funds on
	// addresses the wallet did not have): drop the wallet's history now,
	// before the library opens anything. Done here and not through the
	// library's FORCE_RESCAN file: its Init only logs a failed drop, removes
	// the file when an unrelated second drop succeeded, and also throws away
	// lnd's scan positions, which costs lnd another pass over the chain. The
	// order stands until the drop has succeeded.
	// Carried out first: should it fail, it orders the full check below.
	c.carryOutKnownHistory()
	if c.marked(historyRecheckFile) {
		if err := c.writeConfigs(); err != nil {
			return err
		}
		if err := dropwtx.Drop(c.dir()); err != nil {
			return fmt.Errorf("prepare the fresh history check: %w", err)
		}
		if err := os.Remove(filepath.Join(c.dir(), historyRecheckFile)); err != nil {
			return err
		}
		c.progressf("The history is checked again from the start, with the addresses found.")
	}
	if err := c.initLibrary(svc); err != nil {
		return err
	}
	if err := c.checkPeers(ctx); err != nil {
		return err
	}
	// The wallet's creation time is where the search for later funds
	// starts; the file can only be read while lnd does not hold it.
	if !c.searchDone() {
		if _, err := c.walletBirthdayTime(); err != nil {
			birthday, err := walletBirthday(c.walletDBPath())
			if err != nil {
				return err
			}
			if err := writeFileAtomic(filepath.Join(c.dir(), walletBirthdayFile), []byte(birthday.UTC().Format(time.RFC3339)+"\n")); err != nil {
				return err
			}
		}
	}
	if !c.searchDone() {
		// For the history shortcut (syncskip.go). Not being able to read
		// them only means the shortcut is not taken.
		scripts, err := walletScripts(c.walletDBPath(), c.cfg.Network)
		if err != nil {
			nodeLog("[addresses] the wallet's addresses were not read: " + err.Error())
		} else {
			c.ownScripts = scripts
			if scripts == nil {
				c.ownScripts = [][]byte{}
			}
		}
	}
	c.progressf("Starting the node...")
	nodeCfg := c.cfg
	nodeCfg.WorkDir = c.dir()
	n, err := startNode(ctx, nodeCfg, svc)
	if err != nil {
		return err
	}
	n.wroteSyncState = c.skippedTo > 0
	c.node = n
	c.progressf("Node is up.")
	return nil
}

// firstStart handles the first start after a restore. lnd must not start
// far behind the chain tip. Started at the bootstrap checkpoint (seen:
// block 812,000 of 967,857) its chain notifier walks every block up to the
// tip downloading full blocks, 4 a second, and only acts on a channel close
// once it gets there: about 10 hours, with the closed channel's funds
// invisible meanwhile. Started at the tip there is nothing to walk and old
// closes are found through the filter scan. The headers take about a
// minute; the next start is the one that counts, so wait for them here and
// start again.
func (c *Core) firstStart(ctx context.Context) error {
	if c.searchDone() || c.marked(chainReadyFile) {
		return nil
	}
	c.progressf("Catching up with the bitcoin chain...")
	if err := waitHeadersSynced(ctx, 15*time.Minute); err != nil {
		return err
	}
	if err := c.mark(chainReadyFile); err != nil {
		return err
	}
	// The library cannot be re-initialised in-process after a stop (it
	// hangs), so the caller restarts the whole program. Stop can hang too
	// on some nodes after lnd itself is down, so give it a bounded wait;
	// the program exit releases whatever is left.
	c.progressf("Caught up. Stopping the node; the app restarts to continue...")
	if !c.StopWithin(20 * time.Second) {
		c.progressf("The node did not stop cleanly; the program exits and starts again.")
	}
	return ErrRestartRequired
}

func (c *Core) marked(name string) bool {
	_, err := os.Stat(filepath.Join(c.dir(), name))
	return err == nil
}

func (c *Core) mark(name string) error {
	return writeFileAtomic(filepath.Join(c.dir(), name), []byte(time.Now().Format(time.RFC3339)+"\n"))
}

// writeFileAtomic writes a small state file so that a crash or power loss
// leaves either the old content or the new, never an empty file.
func writeFileAtomic(path string, content []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// searchDone reports whether the search for funds paid after the backup
// was done on this folder (or the look-ahead of releases up to alpha.30).
func (c *Core) searchDone() bool { return c.marked(addressesExtendedFile) }

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

// ErrRestartRequired is returned by StartNode and WaitSynced when the
// program must be started again for the node to pick up a change (a
// restore, the first catch-up with the chain, addresses found paid after
// the backup).
var ErrRestartRequired = errors.New("restart required")

const (
	// addressesExtendedFile marks a folder whose search for funds paid after
	// the backup is done (see addrscan.go). The name dates from the
	// look-ahead of releases up to alpha.30, whose folders carry it too.
	addressesExtendedFile = "addresses-extended"
	// chainReadyFile marks a folder whose first start caught up with the
	// chain, so lnd starts at the tip from now on.
	chainReadyFile = "chain-ready"
	// forceRescanFile is the marker the breez library checks on Init: it
	// drops the wallet's transaction store and rescans from the birthday.
	// This tool no longer writes it (see historyRecheckFile); it is still
	// handy for testing and may be present in folders of older releases.
	forceRescanFile = "FORCE_RESCAN"
	// historyRecheckFile orders a fresh history check: StartNode drops the
	// wallet's transaction store before the library starts, and removes
	// the file once that succeeded.
	historyRecheckFile = "history-recheck"
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
// on every change. On the first sync after a restore it also searches for
// funds paid after the backup, as soon as the chain is caught up and while
// lnd checks the history: finding them first saves checking the history
// twice. It returns ErrRestartRequired when the search found some.
func (c *Core) WaitSynced(ctx context.Context, onProgress func(SyncProgress)) error {
	if c.node == nil {
		return errors.New("node not started")
	}
	report := func(p SyncProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}
	if !c.searchDone() {
		headers, cancel := context.WithCancel(ctx)
		err := c.node.waitSynced(headers, func(p SyncProgress) {
			if p.Stage == "rescan" || p.Synced() {
				cancel()
				return
			}
			report(p)
		})
		cancel()
		if err != nil && (ctx.Err() != nil || !errors.Is(err, context.Canceled)) {
			return err
		}
		if err := c.findLaterFunds(ctx, report); err != nil {
			return err
		}
	}
	if err := c.node.waitSynced(ctx, report); err != nil {
		if errors.Is(err, errWalletSync) {
			// Whatever left the wallet in that state (the history shortcut
			// is the one thing here that writes it): dropping the history
			// resets the sync state to the birthday block, the long and
			// known way.
			c.progressf("The app cannot continue from its sync position; its history is checked from the start.")
			if err := c.mark(historyRecheckFile); err != nil {
				return err
			}
			if !c.StopWithin(20 * time.Second) {
				c.progressf("The node did not stop cleanly; the program exits and starts again.")
			}
			return ErrRestartRequired
		}
		return err
	}
	return c.verifyFound(ctx)
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
	// Unresolved counts channels closed on chain that lnd has not taken up
	// yet and whose payout the app cannot tell: the recovery is not done.
	Unresolved int `json:"unresolved"`
	// Outgoing counts transactions sent from here that wait for their
	// confirmation.
	Outgoing int `json:"outgoing"`
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
	// For the start screen's list of restored backups; a failed write only
	// leaves that list without the amount.
	_ = saveLastFunds(c.dir(), st, time.Now())
	return st, nil
}

// lastFundsFile keeps what a backup's funds screen showed last.
const lastFundsFile = "last-funds.json"

// LastFunds is what a restored backup's funds screen showed last. On-chain
// is the confirmed part only: while a close's payout is unconfirmed the
// same money can be in Pending and in the unconfirmed balance, which the
// screen shows apart but a sum would count twice.
type LastFunds struct {
	InChannels int64 `json:"inChannels"`
	Pending    int64 `json:"pending"`
	Onchain    int64 `json:"onchain"`
	// Settling is set while a close whose payout is not known yet is being
	// settled: the tiles can read zero with money still to come.
	Settling bool      `json:"settling"`
	At       time.Time `json:"at"`
}

func saveLastFunds(dir string, st *Status, at time.Time) error {
	data, err := json.Marshal(LastFunds{st.InChannels, st.InPending, st.OnchainConfirmed, st.Unresolved > 0, at})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, lastFundsFile), data)
}

// ValidateAddress checks a bitcoin address for the configured network. It
// does not need the node or the library (the library's own check
// dereferences an app object that only exists after Init).
func (c *Core) ValidateAddress(address string) error {
	_, err := c.payScript(address)
	return err
}

// payScript validates the address and returns the script paying it.
func (c *Core) payScript(address string) ([]byte, error) {
	params, err := netParams(c.cfg.Network)
	if err != nil {
		return nil, err
	}
	addr, err := btcutil.DecodeAddress(address, params)
	if err != nil {
		return nil, err
	}
	if !addr.IsForNet(params) {
		return nil, fmt.Errorf("%s is not a %s address", address, c.cfg.Network)
	}
	// A bare public key is not an address anybody meant to pay.
	if _, ok := addr.(*btcutil.AddressPubKey); ok {
		return nil, errors.New("cannot send to a bare public key")
	}
	return txscript.PayToAddrScript(addr)
}

// SweepOption is one fee choice for the sweep transaction.
type SweepOption struct {
	ConfTarget int    `json:"confTarget"` // blocks
	Fee        int64  `json:"fee"`
	TxID       string `json:"txid"`
}

// SweepPlan is a prepared sweep of the whole on-chain balance.
type SweepPlan struct {
	Address string `json:"address"`
	// Amount is the confirmed balance the transactions spend.
	Amount  int64         `json:"amount"`
	Options []SweepOption `json:"options"`
	// Kept is what the node holds back as change instead of sending it (a
	// reserve lnd keeps for public anchor channels); 0 in the normal case.
	Kept int64 `json:"kept"`
}

type sweepPlan struct {
	SweepPlan
	txs map[int][]byte
}

// sweepTargets are the fee choices offered, in blocks.
var sweepTargets = []int{2, 6, 25}

// PrepareSweep builds the sweep transactions for the confirmed on-chain
// balance at the three fee targets, without broadcasting.
//
// It asks lnd for each target itself. The library's helper gives up on the
// first target that fails, and with a small balance the fast target often
// fails alone (its fee leaves less than the dust limit) while a slower one
// works.
func (c *Core) PrepareSweep(ctx context.Context, address string) (*SweepPlan, error) {
	if c.node == nil {
		return nil, errors.New("node not started")
	}
	script, err := c.payScript(address)
	if err != nil {
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
	// lnd falls back to its minimum fee rate without a word when its fee
	// source does not answer; a "fast" choice would then be the slowest.
	if err := checkFeeSource(ctx, c.cfg.FeeURL); err != nil {
		return nil, err
	}
	plan := &sweepPlan{SweepPlan: SweepPlan{Address: address, Amount: wb.ConfirmedBalance}, txs: map[int][]byte{}}
	var failures []string
	for _, target := range sweepTargets {
		res, err := c.node.client.SendCoins(ctx, &lnrpc.SendCoinsRequest{
			Addr: address, SendAll: true, TargetConf: int32(target), DryRun: true,
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%d blocks: %v", target, err))
			continue
		}
		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(res.Tx)); err != nil {
			return nil, fmt.Errorf("read the prepared transaction: %w", err)
		}
		var sent, kept int64
		for _, out := range tx.TxOut {
			if bytes.Equal(out.PkScript, script) {
				sent += out.Value
			} else {
				kept += out.Value
			}
		}
		if sent == 0 {
			return nil, errors.New("the prepared transaction does not pay the address")
		}
		plan.Kept = kept
		plan.Options = append(plan.Options, SweepOption{ConfTarget: target, Fee: wb.ConfirmedBalance - sent - kept, TxID: tx.TxHash().String()})
		plan.txs[target] = res.Tx
	}
	if len(plan.Options) == 0 {
		return nil, fmt.Errorf("no transaction could be prepared (%s)", strings.Join(failures, "; "))
	}
	for _, f := range failures {
		c.progressf("No transaction at the fee for %s.", f)
	}
	if plan.Kept > 0 {
		c.progressf("The node keeps %d sat back as a reserve for its channels.", plan.Kept)
	}
	c.sweep = plan
	c.progressf("Prepared sweep of %d sat to %s.", plan.Amount, address)
	return &plan.SweepPlan, nil
}

// checkFeeSource makes sure the fee source lnd uses answers with rates.
func checkFeeSource(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the current fee rates could not be fetched; check the connection and try again (%w)", err)
	}
	defer res.Body.Close()
	var rates struct {
		FeeByBlockTarget map[string]uint32 `json:"fee_by_block_target"`
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("the current fee rates could not be fetched (HTTP %d); try again later", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rates); err != nil || len(rates.FeeByBlockTarget) == 0 {
		return errors.New("the fee source answered without rates; try again later")
	}
	return nil
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
