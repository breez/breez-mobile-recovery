package core

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// One restore path for every source. A backup is fetched into memory (the
// three files are a few dozen megabytes at most), decrypted and checked as
// a whole, written into a staging folder next to its final place and moved
// there with one rename. Only then is an earlier restore of the same
// backup moved aside, the backup selected, and the cloud copy marked. So:
//
//   - a wrong phrase, a failed download or a damaged file changes nothing
//     on disk and nothing in the cloud;
//   - a crash or a full disk never leaves a folder that looks like a
//     restored app but lacks its channels (lnd would create an empty
//     channel database and the app would show "nothing to recover");
//   - no wallet file is ever overwritten or deleted.

// checkRestoreTarget refuses a restore that cannot go ahead, before
// anything is fetched. It changes nothing.
func (c *Core) checkRestoreTarget(name string, force bool) error {
	if c.layoutErr != nil {
		return c.layoutErr
	}
	if !backupNameRE.MatchString(name) {
		return fmt.Errorf("invalid backup name %q", name)
	}
	dir := c.backupDir(name)
	if bound, _ := inUse(); bound != "" {
		// The library holds its first folder's databases for the whole
		// process; the program has to start again first.
		return ErrLibraryBound
	}
	if err := backupFree(dir); err != nil {
		return err
	}
	if hasWallet(dir, c.cfg.Network) && !force {
		return ErrNodeExists
	}
	return nil
}

// decodeBackupFiles decrypts (key != nil) and checks the node files.
func decodeBackupFiles(files map[string][]byte, key []byte) (map[string][]byte, error) {
	if err := checkComplete(files); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for name := range nodeFileTargets("") {
		content := files[name]
		if key != nil {
			plain, err := decryptGCM(content, key)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s: %w (wrong backup phrase?)", name, err)
			}
			content = plain
		} else if looksLikeCiphertext(name, content) {
			return nil, fmt.Errorf("%s does not look like a database; the backup is encrypted and needs the backup phrase", name)
		}
		if looksLikeCiphertext(name, content) {
			return nil, fmt.Errorf("%s is not a database after decryption; the backup is damaged", name)
		}
		out[name] = content
	}
	return out, nil
}

// placeBackup writes the decoded node files as the restored backup name,
// restored from source (SourceGoogle, SourceICloud or SourceFile). It
// returns the id this restore goes by in the cloud: the library keeps one
// per work folder (backup/breez_backup_id, backup/manager.go:673-694) and
// compares it with the id the cloud copy was last restored under, so the
// cloud has to be marked with the very id the restored folder carries.
func (c *Core) placeBackup(name, source string, decoded map[string][]byte, force bool) (string, error) {
	id, err := c.placeBackupFiles(name, source, decoded, force)
	if err != nil {
		return "", err
	}
	return "backup-id-" + hex.EncodeToString(id), nil
}

func (c *Core) placeBackupFiles(name, source string, decoded map[string][]byte, force bool) ([]byte, error) {
	if !knownSource(source) {
		return nil, fmt.Errorf("unknown backup source %q", source)
	}
	if err := c.checkRestoreTarget(name, force); err != nil {
		return nil, err
	}
	dir := c.backupDir(name)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	// The staging folder of an attempt that died is no restored app: it
	// was never started, and the backup it came from is still where it was.
	staging := dir + ".restoring"
	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	for name, rel := range nodeFileTargets(c.cfg.Network) {
		destDir := filepath.Join(staging, rel)
		if err := os.MkdirAll(destDir, 0700); err != nil {
			return nil, err
		}
		if err := writeFileSynced(filepath.Join(destDir, name), decoded[name]); err != nil {
			return nil, err
		}
	}
	if err := checkDatabases(staging, c.cfg.Network); err != nil {
		return nil, err
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(staging, "backup"), 0700); err != nil {
		return nil, err
	}
	if err := writeFileSynced(filepath.Join(staging, "backup", "breez_backup_id"), id); err != nil {
		return nil, err
	}
	if err := writeFileSynced(filepath.Join(staging, sourceFile), []byte(source+"\n")); err != nil {
		return nil, err
	}
	// Whatever is at the final place is moved aside, never deleted.
	if _, err := os.Stat(dir); err == nil {
		kind := ".incomplete-"
		if hasWallet(dir, c.cfg.Network) {
			kind = ".replaced-"
		}
		aside := dir + kind + time.Now().Format("20060102-150405")
		if err := os.Rename(dir, aside); err != nil {
			return nil, err
		}
		if kind == ".replaced-" {
			c.progressf("The earlier restore of this backup was moved to %s.", aside)
		}
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, err
	}
	if err := c.selectBackup(name); err != nil {
		return nil, err
	}
	return id, c.writeConfigs()
}

func writeFileSynced(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
	return f.Close()
}

// hasWallet reports whether the folder holds a wallet file at all: the
// test for "never overwrite". hasNode, the test for "a restored app", asks
// for all three node files.
func hasWallet(dir, network string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "data", "chain", "bitcoin", network, "wallet.db"))
	return err == nil
}

// unzipNodeFiles returns the node files of a backup zip, keyed by name. Two
// layouts exist:
//
//   - the backup.zip the phone uploads to its cloud: wallet.db, channel.db,
//     breez.db (and a channel.backup this tool has no use for);
//   - the zip of the phone's "Export DB Files" (Developers menu, which the
//     README tells users to keep as a safety copy): the same files with a
//     running number in front, 1_channel.db, 2_wallet.db, 3_wallet.db,
//     4_breez.db (breezmobile lib/routes/dev/dev.dart). Its first wallet is
//     lnd's backup copy, the very kind of file a cloud backup holds; the
//     second is the live file of the running app and is left alone.
//
// Apart from that one known pair, two entries with one name are an error:
// which one is the wallet?
func unzipNodeFiles(r *zip.Reader) (map[string][]byte, error) {
	type entry struct {
		f      *zip.File
		number int // 0 for a plain name
	}
	found := map[string][]entry{}
	for _, f := range r.File {
		name, number := filepath.Base(f.Name), 0
		if m := numberedEntryRE.FindStringSubmatch(name); m != nil {
			number, _ = strconv.Atoi(m[1])
			name = m[2]
		}
		if _, ok := nodeFileTargets("")[name]; !ok {
			continue
		}
		found[name] = append(found[name], entry{f, number})
	}
	files := map[string][]byte{}
	for name, entries := range found {
		pick := entries[0]
		if len(entries) > 1 {
			// Only the export's two wallets, both numbered, may share a name.
			if name != "wallet.db" || len(entries) != 2 || entries[0].number == 0 || entries[1].number == 0 || entries[0].number == entries[1].number {
				return nil, fmt.Errorf("the backup holds %s twice", name)
			}
			if entries[1].number < pick.number {
				pick = entries[1]
			}
		}
		rc, err := pick.f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		// The export zips the app database of the RUNNING app: the phone
		// computes the entry's checksum and reads its content at different
		// moments, so the zip's checksum fails although the database inside
		// is whole (seen on a real export, 2026-09-21: all 30,515,200 bytes
		// there, every page sound). For that one entry the zip's checksum
		// does not decide; the database check before placing does.
		if errors.Is(err, zip.ErrChecksum) && pick.number != 0 && name == "breez.db" {
			err = nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s from the backup: %w", name, err)
		}
		files[name] = content
	}
	return files, nil
}

var numberedEntryRE = regexp.MustCompile(`^(\d+)_(.+)$`)

// checkDatabases opens each placed node file the way its owner will and
// walks its pages. A file cut short, torn by a copy taken while the app was
// writing, or damaged in transit must not become a restored app: lnd or the
// library would fail on it later, or worse, run on part of it.
func checkDatabases(dir, network string) error {
	for name, rel := range nodeFileTargets(network) {
		path := filepath.Join(dir, rel, name)
		head := make([]byte, 16)
		if f, err := os.Open(path); err == nil {
			io.ReadFull(f, head)
			f.Close()
		}
		if string(head[:6]) == "SQLite" {
			continue // very old breez.db; the library reads it as it is
		}
		if err := checkBolt(path); err != nil {
			return fmt.Errorf("%s is damaged: %w", name, err)
		}
	}
	return nil
}

// checkBolt makes sure a bbolt file is whole and walks its pages. The size
// comes first: bbolt maps the file into memory, and reading a page past the
// end of a file cut short is a memory fault that takes the whole program
// down (seen in this package's tests), in this tool and in lnd alike. A
// fault in the walk itself is turned into an error for the same reason.
func checkBolt(path string) (err error) {
	if err := boltWhole(path); err != nil {
		return err
	}
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unreadable: %v", r)
		}
	}()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(func(tx *bolt.Tx) error {
		// Read to the end: the walk runs in a goroutine of its own that
		// blocks on a reader who left.
		var first error
		for e := range tx.Check() {
			if first == nil {
				first = e
			}
		}
		return first
	})
}

// viewBolt reads a bbolt file that may be damaged, read-only and without
// taking the program down: the size check and the fault guard of
// checkBolt, without its full walk.
func viewBolt(path string, fn func(*bolt.Tx) error) (err error) {
	if err := boltWhole(path); err != nil {
		return err
	}
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unreadable: %v", r)
		}
	}()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(fn)
}

// boltWhole checks that a bbolt file holds every page its meta pages say
// it has, before it is mapped into memory.
func boltWhole(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	// Page 0 and page 1 are the two meta pages: a 16 byte page header, then
	// magic (4), version (4), page size (4), flags (4), root (16),
	// freelist (8), and the number of pages in use (8).
	head := make([]byte, 64)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return errors.New("too short to be a database")
	}
	pageSize := int64(binary.LittleEndian.Uint32(head[24:28]))
	if pageSize < 512 || pageSize > 1<<20 {
		f.Close()
		return fmt.Errorf("page size %d", pageSize)
	}
	pages := binary.LittleEndian.Uint64(head[56:64])
	second := make([]byte, 64)
	if _, err := f.ReadAt(second, pageSize); err == nil && isBolt(second) {
		if p := binary.LittleEndian.Uint64(second[56:64]); p > pages {
			pages = p
		}
	}
	f.Close()
	if pages > uint64(info.Size()/pageSize) {
		return fmt.Errorf("the file has %d of its %d pages: it was cut short", info.Size()/pageSize, pages)
	}
	return nil
}

// nodeFilesFrom turns what a cloud holds for a backup, either a backup.zip
// or the three loose files of very old backups, into the node files.
func nodeFilesFrom(downloaded map[string][]byte) (map[string][]byte, error) {
	if z, ok := downloaded["backup.zip"]; ok {
		r, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
		if err != nil {
			return nil, fmt.Errorf("open the downloaded backup: %w", err)
		}
		return unzipNodeFiles(r)
	}
	files := map[string][]byte{}
	for name := range nodeFileTargets("") {
		if content, ok := downloaded[name]; ok {
			files[name] = content
		}
	}
	return files, nil
}
