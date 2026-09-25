package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez/data"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// Layout of the work folder. Every restored backup lives in a folder of its
// own, so two backups never share a wallet, a channel database, lnd's
// channel.backup or any marker file:
//
//	gdrive-token.json, icloud-session.json   cached sign-ins
//	current                                  name of the backup folder in use
//	backups/<name>/                          one restored backup: a complete node folder
//
// <name> is the node id for a cloud backup and "zip-<hash of the file>" for
// a backup file. Releases up to alpha.26 kept a single node directly in the
// work folder; loadLayout moves such a node into backups/ once.
const (
	backupsFolder = "backups"
	currentFile   = "current"
)

// legacyNodeEntries are the files and folders of a node in the old layout.
// Only these are moved; anything else in the work folder stays where it is.
var legacyNodeEntries = []string{
	"data", "logs", "tmp", "backup", "app_data_backup", "letsencrypt",
	"breez.db", "backup.db", "sessions_encryption.db", "breez.conf", "lnd.conf",
	"tls.cert", "tls.key", "latest_backup_snapshot.json",
	addressesExtendedFile, forceRescanFile,
}

var backupNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{3,80}$`)

// ErrLibraryBound is returned when a node runs on one backup folder, in
// this process or in the app's node helper, and another one is asked for.
// The library's databases are process-wide: the process that ran it has to
// end first. The app stops its helper before it switches.
var ErrLibraryBound = errors.New("the node of another backup is still running")

var (
	inUseMu sync.Mutex
	// boundLibDir is the backup folder a node runs on: the folder the breez
	// library was initialised on in this process, or the one of the node
	// helper the app runs (SetInUse); "" when there is none. Package level
	// like the library's own state: a second Core in the same process is
	// bound just the same.
	boundLibDir string
	// boundPayments is the length of the payment list of boundLibDir,
	// counted before the node opened it (RestoredBackups cannot read it
	// after).
	boundPayments int
)

// inUse returns boundLibDir and boundPayments.
func inUse() (dir string, payments int) {
	inUseMu.Lock()
	defer inUseMu.Unlock()
	return boundLibDir, boundPayments
}

// SetInUse records that a node runs on the backup folder dir, in this
// process (initLibrary) or in a node helper this process started, with
// payments the length of the folder's payment list counted before the node
// opened it (PaymentCount). A restore and a switch to another backup are
// refused while it is set, and the restored apps list takes the count from
// here. The window clears it with SetInUse("", 0) once its helper has
// exited; the library in this process never lets go.
func SetInUse(dir string, payments int) {
	inUseMu.Lock()
	defer inUseMu.Unlock()
	boundLibDir, boundPayments = dir, payments
}

// PaymentCount is the length of the app's payment list in the backup
// folder dir, 0 when it cannot be read. A running node holds the file, so
// it is read before the node starts.
func PaymentCount(dir string) int {
	n, _ := countPayments(filepath.Join(dir, "breez.db"))
	return n
}

// hasNode reports whether the folder holds a restored app: all three node
// files. A wallet without its channel database is not one (lnd would start
// on it with an empty channel database and show no channels).
func hasNode(dir, network string) bool {
	if dir == "" {
		return false
	}
	for name, rel := range nodeFileTargets(network) {
		if _, err := os.Stat(filepath.Join(dir, rel, name)); err != nil {
			return false
		}
	}
	return true
}

// dir is the node folder of the backup in use, "" when none is selected.
func (c *Core) dir() string { return c.nodeDir }

func (c *Core) backupDir(name string) string {
	return filepath.Join(c.cfg.WorkDir, backupsFolder, name)
}

// zipBackupName names the folder of a backup file after its content, so the
// same file always lands in the same folder and different files never do.
func zipBackupName(zipPath string) (string, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "zip-" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// selectBackup makes name the backup in use and remembers it for the next
// start.
func (c *Core) selectBackup(name string) error {
	if !backupNameRE.MatchString(name) {
		return fmt.Errorf("invalid backup name %q", name)
	}
	dir := c.backupDir(name)
	bound, _ := inUse()
	if bound != "" && bound != dir {
		return ErrLibraryBound
	}
	if bound != dir {
		if err := backupFree(dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(c.cfg.WorkDir, currentFile), []byte(name+"\n")); err != nil {
		return err
	}
	c.nodeDir = dir
	return nil
}

// RestoredBackup is a backup folder in the work folder.
type RestoredBackup struct {
	Name string `json:"name"`
	// NodeID is the node's public key: the name of a cloud backup's folder,
	// read from the node's channel.db when a backup file was restored.
	NodeID  string `json:"nodeId,omitempty"`
	Dir     string `json:"dir"`
	Current bool   `json:"current"`
	// Source is where it was last restored from: SourceGoogle,
	// SourceICloud or SourceFile, "" when that is not known (restored by
	// a release that did not keep it).
	Source string `json:"source"`
	// LastOpened is when its node last ran here (the time of its lnd log);
	// nil if it never did.
	LastOpened *time.Time `json:"lastOpened,omitempty"`
	// Funds is what its funds screen showed last; nil if it never showed.
	Funds *LastFunds `json:"funds,omitempty"`
	// Payments is the length of the app's own payment list (sent,
	// received, deposits, withdrawals, closed channels): zero for an app
	// that was never used, or when it could not be read.
	Payments int `json:"payments,omitempty"`
}

// RestoredBackups lists the backups restored on this computer.
func (c *Core) RestoredBackups() []RestoredBackup {
	entries, _ := os.ReadDir(filepath.Join(c.cfg.WorkDir, backupsFolder))
	bound, boundN := inUse()
	var out []RestoredBackup
	for _, e := range entries {
		dir := c.backupDir(e.Name())
		if e.IsDir() && backupNameRE.MatchString(e.Name()) && hasNode(dir, c.cfg.Network) {
			rb := RestoredBackup{Name: e.Name(), Dir: dir, Current: dir == c.nodeDir, NodeID: e.Name(), Source: backupSource(dir, e.Name())}
			if strings.HasPrefix(e.Name(), "zip-") {
				rb.NodeID = ""
				if raw, err := os.ReadFile(filepath.Join(dir, nodeIDFile)); err == nil {
					rb.NodeID = strings.TrimSpace(string(raw))
				} else if dir != bound {
					// Restored before the id was kept, or unreadable then.
					if id, err := nodeIDOf(dir, c.cfg.Network); err == nil {
						rb.NodeID = id
						_ = writeFileAtomic(filepath.Join(dir, nodeIDFile), []byte(id+"\n"))
					}
				}
			}
			if fi, err := os.Stat(lndLogPath(dir, c.cfg.Network)); err == nil {
				t := fi.ModTime()
				rb.LastOpened = &t
			}
			// Read from breez.db directly, so it is there before the node
			// ever ran; the folder the library has open here was counted
			// just before it was opened.
			if dir == bound {
				rb.Payments = boundN
			} else if n, err := countPayments(filepath.Join(dir, "breez.db")); err == nil {
				rb.Payments = n
			}
			if raw, err := os.ReadFile(filepath.Join(dir, lastFundsFile)); err == nil {
				var f LastFunds
				if json.Unmarshal(raw, &f) == nil {
					rb.Funds = &f
				}
			}
			out = append(out, rb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeIDFile keeps the node id of a backup restored from a file, whose
// folder is named after the file (the id is only readable once decrypted).
const nodeIDFile = "node-id"

// Where a backup was restored from (RestoredBackup.Source).
const (
	SourceGoogle = "google"
	SourceICloud = "icloud"
	SourceFile   = "file"
)

// sourceFile keeps the Source of the last restore into a backup's folder.
// It is written with the node files (placeBackupFiles), so a folder never
// has the files of one restore and the source of another.
const sourceFile = "source"

func knownSource(s string) bool {
	return s == SourceGoogle || s == SourceICloud || s == SourceFile
}

// backupSource reads where the backup in the folder dir, named name, was
// last restored from. A folder restored before sourceFile existed is known
// only when its name says it: "zip-" names come from a backup file.
// Nothing else is taken as a hint.
func backupSource(dir, name string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, sourceFile))
	if s := strings.TrimSpace(string(raw)); knownSource(s) {
		return s
	}
	if strings.HasPrefix(name, "zip-") {
		return SourceFile
	}
	return ""
}

// nodeIDOf reads a restored node's public key without starting it. First
// from the app's account in breez.db, where the account service keeps
// lnd's identity key (breez/breez account/account.go) and which a backup
// carries whole; else from channel.db's graph source node, which lnd
// writes when it starts (a backup's channel.db leaves the graph out,
// lnd breezbackup/backup.go).
func nodeIDOf(dir, network string) (string, error) {
	var id string
	err := viewBolt(filepath.Join(dir, "breez.db"), func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("account"))
		if b == nil {
			return errors.New("no account bucket")
		}
		var acc data.Account
		if err := proto.Unmarshal(b.Get([]byte("account")), &acc); err != nil {
			return err
		}
		id = acc.Id
		return nil
	})
	if err == nil && isNodeID(id) {
		return id, nil
	}
	err = viewBolt(filepath.Join(dir, nodeFileTargets(network)["channel.db"], "channel.db"), func(tx *bolt.Tx) error {
		nodes := tx.Bucket([]byte("graph-node"))
		if nodes == nil {
			return errors.New("no graph-node bucket")
		}
		id = hex.EncodeToString(nodes.Get([]byte("source")))
		return nil
	})
	if err != nil {
		return "", err
	}
	if !isNodeID(id) {
		return "", errors.New("no node id in breez.db or channel.db")
	}
	return id, nil
}

// isNodeID reports whether s is a compressed public key in hex.
func isNodeID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 33 && (b[0] == 2 || b[0] == 3)
}

// countPayments counts the app's payment list in its breez.db: the
// entries of the "payments" bucket (breez/breez db/db.go, read the same way
// as FetchAllAccountPayments), nested buckets left out.
func countPayments(breezDB string) (int, error) {
	n := 0
	err := viewBolt(breezDB, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("payments"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if v != nil {
				n++
			}
			return nil
		})
	})
	return n, err
}

// BackupName is the folder name a backup gets: the node id for a cloud
// backup, a name derived from the file's content for a backup file.
func BackupName(source, nodeID, zipPath string) (string, error) {
	if source == "zip" {
		return zipBackupName(zipPath)
	}
	if !backupNameRE.MatchString(nodeID) {
		return "", fmt.Errorf("invalid node id %q", nodeID)
	}
	return nodeID, nil
}

// IsRestored reports whether the backup with this folder name is already
// restored on this computer.
func (c *Core) IsRestored(name string) bool {
	return backupNameRE.MatchString(name) && hasNode(c.backupDir(name), c.cfg.Network)
}

// UseBackup continues with a backup that is already restored here.
func (c *Core) UseBackup(name string) error {
	if !c.IsRestored(name) {
		return fmt.Errorf("no restored backup %q in %s", name, c.cfg.WorkDir)
	}
	if err := c.selectBackup(name); err != nil {
		return err
	}
	c.progressf("Continuing with the backup already restored in %s.", c.nodeDir)
	return nil
}

// CurrentBackup is the folder name of the backup in use, "" when none.
func (c *Core) CurrentBackup() string {
	if c.nodeDir == "" {
		return ""
	}
	return filepath.Base(c.nodeDir)
}

// NodeDir is the node folder of the backup in use, "" when none.
func (c *Core) NodeDir() string { return c.nodeDir }

// LibraryBound reports whether this process already runs the breez library,
// which ties it to one backup folder for the rest of the process, or runs a
// node helper (SetInUse).
func (c *Core) LibraryBound() bool {
	dir, _ := inUse()
	return dir != ""
}

// loadLayout moves a node left directly in the work folder by an older
// release into backups/, then reads which backup is in use.
func (c *Core) loadLayout() error {
	root := c.cfg.WorkDir
	if hasNode(root, c.cfg.Network) {
		if err := c.migrateLegacyNode(); err != nil {
			return fmt.Errorf("move the restored app into its own folder: %w", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, currentFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	name := strings.TrimSpace(string(data))
	if backupNameRE.MatchString(name) && hasNode(c.backupDir(name), c.cfg.Network) {
		c.nodeDir = c.backupDir(name)
	}
	return nil
}

// migrateLegacyNode moves the node files of the old single-folder layout
// into backups/<node id>/. Only renames within the work folder: nothing is
// copied or deleted, and the wallet's sync progress moves with its files.
func (c *Core) migrateLegacyNode() error {
	root := c.cfg.WorkDir
	name := ""
	if data, err := os.ReadFile(filepath.Join(root, "latest_backup_snapshot.json")); err == nil {
		var snap struct{ NodeID string }
		if json.Unmarshal(data, &snap) == nil && backupNameRE.MatchString(snap.NodeID) {
			name = snap.NodeID
		}
	}
	if name == "" {
		name = "restored-" + time.Now().Format("20060102-150405")
	}
	dest := c.backupDir(name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	var moved []string
	for _, entry := range legacyNodeEntries {
		if _, err := os.Lstat(filepath.Join(root, entry)); err != nil {
			continue
		}
		if err := os.Rename(filepath.Join(root, entry), filepath.Join(dest, entry)); err != nil {
			// Put back what was moved: a node split over two folders
			// would not start.
			for _, m := range moved {
				os.Rename(filepath.Join(dest, m), filepath.Join(root, m))
			}
			os.Remove(dest)
			return err
		}
		moved = append(moved, entry)
	}
	if err := os.WriteFile(filepath.Join(root, currentFile), []byte(name+"\n"), 0600); err != nil {
		return err
	}
	c.progressf("The restored app now has its own folder: %s", dest)
	return nil
}
