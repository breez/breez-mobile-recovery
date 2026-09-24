package core

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/breez/breez/data"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

type nopReporter struct{}

func (nopReporter) Progress(string)       {}
func (nopReporter) NodeLog(string)        {}
func (nopReporter) SignIn(string, string) {}

func testCore(t *testing.T, root string) *Core {
	t.Helper()
	boundLibDir = ""
	cfg := DefaultConfig()
	cfg.WorkDir = root
	return New(cfg, nopReporter{})
}

// testDB is a small valid database holding marker, as bytes.
func testDB(t *testing.T, marker string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("test"))
		if err != nil {
			return err
		}
		return b.Put([]byte("marker"), []byte(marker))
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// markerOf reads the marker back from a database file.
func markerOf(t *testing.T, path string) string {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer db.Close()
	var marker string
	db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("test")); b != nil {
			marker = string(b.Get([]byte("marker")))
		}
		return nil
	})
	return marker
}

// fakeNode puts the three node files into dir.
func fakeNode(t *testing.T, dir, marker string) {
	t.Helper()
	content := testDB(t, marker)
	for name, rel := range nodeFileTargets("mainnet") {
		if err := os.MkdirAll(filepath.Join(dir, rel), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// place restores a small backup the way every restore does.
func place(t *testing.T, c *Core, name, marker string, force bool) error {
	t.Helper()
	content := testDB(t, marker)
	files := map[string][]byte{}
	for file := range nodeFileTargets("") {
		files[file] = content
	}
	_, err := c.placeBackup(name, files, force)
	return err
}

const nodeA = "02e66bcb1e3c97de679c0d5b2f831ac913e53acdc99542ed839c17b1079df489ea"
const nodeB = "02c6b28eb051854c632f12a91fb7b3fe0768f1e957bf842040c3289e5cf99179b6"

func TestBackupsGetTheirOwnFolders(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if c.HasRestoredNode() {
		t.Fatal("empty work folder reports a node")
	}
	if err := place(t, c, nodeA, "A", false); err != nil {
		t.Fatal(err)
	}
	// lnd's channel backup of node A must never be seen by node B.
	scb := filepath.Join(c.dir(), "data", "chain", "bitcoin", "mainnet", "channel.backup")
	os.WriteFile(scb, []byte("A"), 0600)

	if err := place(t, c, nodeB, "B", false); err != nil {
		t.Fatal(err)
	}
	if c.dir() != filepath.Join(root, "backups", nodeB) {
		t.Fatalf("node B runs in %s", c.dir())
	}
	if _, err := os.Stat(filepath.Join(c.dir(), "data", "chain", "bitcoin", "mainnet", "channel.backup")); err == nil {
		t.Fatal("node B's folder holds node A's channel backup")
	}

	// A new session continues with the last backup used.
	c2 := testCore(t, root)
	if c2.CurrentBackup() != nodeB || !c2.HasRestoredNode() {
		t.Fatalf("current backup %q", c2.CurrentBackup())
	}
	if err := c2.UseBackup(nodeA); err != nil {
		t.Fatal(err)
	}
	if got := len(c2.RestoredBackups()); got != 2 {
		t.Fatalf("%d restored backups, want 2", got)
	}
}

// The start screen tells restored backups apart by when their node last ran.
func TestRestoredBackupsLastOpened(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	for name, marker := range map[string]string{nodeA: "A", nodeB: "B"} {
		if err := place(t, c, name, marker, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.UseBackup(nodeB); err != nil {
		t.Fatal(err)
	}
	logA := lndLogPath(c.backupDir(nodeA), "mainnet")
	opened := time.Date(2026, 9, 24, 12, 1, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Dir(logA), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logA, []byte("lnd"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logA, opened, opened); err != nil {
		t.Fatal(err)
	}
	got := map[string]RestoredBackup{}
	for _, b := range c.RestoredBackups() {
		got[b.Name] = b
	}
	if a := got[nodeA]; a.LastOpened == nil || !a.LastOpened.Equal(opened) || a.Current {
		t.Errorf("node A: %+v", a)
	}
	if b := got[nodeB]; b.LastOpened != nil || !b.Current {
		t.Errorf("node B: %+v", b)
	}
}

// The list of restored backups carries what each funds screen showed last,
// On-chain without its unconfirmed part (it may be a Pending payout on its
// way), and whether a close is still settling; nothing for one never shown.
func TestRestoredBackupsLastFunds(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	for name, marker := range map[string]string{nodeA: "A", nodeB: "B"} {
		if err := place(t, c, name, marker, false); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 9, 24, 16, 40, 0, 0, time.UTC)
	st := &Status{InChannels: 1000, InPending: 200, OnchainConfirmed: 300, OnchainUnconfirmed: 50, Unresolved: 1}
	if err := saveLastFunds(c.backupDir(nodeA), st, at); err != nil {
		t.Fatal(err)
	}
	got := map[string]RestoredBackup{}
	for _, b := range c.RestoredBackups() {
		got[b.Name] = b
	}
	want := LastFunds{InChannels: 1000, Pending: 200, Onchain: 300, Settling: true, At: at}
	if f := got[nodeA].Funds; f == nil || *f != want {
		t.Errorf("node A funds %+v, want %+v", f, want)
	}
	if f := got[nodeB].Funds; f != nil {
		t.Errorf("node B funds %+v, want none", f)
	}
}

// A backup restored from a file is listed with its node id, read the way
// a backup carries it: from the app's account in breez.db (a backup's
// channel.db has no graph source node), else from channel.db's source node
// once lnd wrote it. A damaged file costs the id, never the program.
func TestRestoredBackupsNodeID(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	const fromAccount, fromGraph, none = "zip-00000000000000a1", "zip-00000000000000a2", "zip-00000000000000a3"
	for _, name := range []string{nodeA, fromAccount, fromGraph, none} {
		if err := place(t, c, name, name[:4], false); err != nil {
			t.Fatal(err)
		}
	}
	put := func(path, bucket, key string, val []byte) {
		t.Helper()
		db, err := bolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return err
			}
			return b.Put([]byte(key), val)
		}); err != nil {
			t.Fatal(err)
		}
	}
	chanDB := func(name string) string {
		return filepath.Join(c.backupDir(name), nodeFileTargets("mainnet")["channel.db"], "channel.db")
	}
	// As a backup has them: the id in the account, the graph without "source".
	acc, _ := proto.Marshal(&data.Account{Id: nodeB})
	put(filepath.Join(c.backupDir(fromAccount), "breez.db"), "account", "account", acc)
	put(chanDB(fromAccount), "graph-node", "02aaaa", []byte("a channel peer"))
	// No account, but lnd ran and wrote its source node.
	pubA, _ := hex.DecodeString(nodeA)
	put(chanDB(fromGraph), "graph-node", "source", pubA)
	// Neither, and a breez.db cut short: no id, no count, no crash.
	breezNone := filepath.Join(c.backupDir(none), "breez.db")
	if info, err := os.Stat(breezNone); err != nil || os.Truncate(breezNone, info.Size()/2) != nil {
		t.Fatal("could not cut breez.db short")
	}

	got := map[string]RestoredBackup{}
	for _, b := range c.RestoredBackups() {
		got[b.Name] = b
	}
	want := map[string]string{nodeA: nodeA, fromAccount: nodeB, fromGraph: nodeA, none: ""}
	for name, id := range want {
		if got[name].NodeID != id {
			t.Errorf("%s: node id %q, want %q", name, got[name].NodeID, id)
		}
	}
	if got[none].Payments != 0 {
		t.Errorf("payments of a damaged breez.db: %d", got[none].Payments)
	}
	// The ids read are kept, so the next listing does not open the files.
	for _, name := range []string{fromAccount, fromGraph} {
		if raw, err := os.ReadFile(filepath.Join(c.backupDir(name), nodeIDFile)); err != nil || strings.TrimSpace(string(raw)) != want[name] {
			t.Errorf("%s: kept id %q, %v", name, raw, err)
		}
	}
}

// list in breez.db, read without the library.
func TestRestoredBackupsPayments(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	for name, marker := range map[string]string{nodeA: "A", nodeB: "B"} {
		if err := place(t, c, name, marker, false); err != nil {
			t.Fatal(err)
		}
	}
	db, err := bolt.Open(filepath.Join(c.backupDir(nodeA), "breez.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("payments"))
		if err != nil {
			return err
		}
		if _, err := b.CreateBucket([]byte("index")); err != nil { // nested, not a payment
			return err
		}
		for _, k := range []string{"p1", "p2", "p3"} {
			if err := b.Put([]byte(k), []byte("payment")); err != nil {
				return err
			}
		}
		return nil
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, b := range c.RestoredBackups() {
		got[b.Name] = b.Payments
	}
	if got[nodeA] != 3 || got[nodeB] != 0 {
		t.Errorf("payments %v, want 3 for A and 0 for B", got)
	}
}

func TestRestoreAgainMovesTheOldFolderAside(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if err := place(t, c, nodeA, "first", false); err != nil {
		t.Fatal(err)
	}
	if err := c.checkRestoreTarget(nodeA, false); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("restoring again without force: %v, want ErrNodeExists", err)
	}
	if err := place(t, c, nodeA, "second", false); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("placing again without force: %v, want ErrNodeExists", err)
	}
	if err := place(t, c, nodeA, "second", true); err != nil {
		t.Fatal(err)
	}
	wallet := filepath.Join("data", "chain", "bitcoin", "mainnet", "wallet.db")
	if got := markerOf(t, filepath.Join(c.dir(), wallet)); got != "second" {
		t.Fatalf("the folder holds %q, want the new restore", got)
	}
	kept := ""
	entries, _ := os.ReadDir(filepath.Join(root, "backups"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), nodeA+".replaced-") {
			kept = filepath.Join(root, "backups", e.Name())
		}
	}
	if got := markerOf(t, filepath.Join(kept, wallet)); got != "first" {
		t.Fatalf("the earlier wallet was not kept: %q", got)
	}
}

// A restore that fails before its files are complete changes nothing: the
// working restore stays in place and in use.
func TestFailedRestoreLeavesTheOldOneAlone(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if err := place(t, c, nodeA, "first", false); err != nil {
		t.Fatal(err)
	}
	// A wrong phrase, a damaged file, a missing file: all stop in
	// decodeBackupFiles, before anything is placed.
	if _, err := decodeBackupFiles(map[string][]byte{"wallet.db": []byte("x")}, nil); err == nil {
		t.Fatal("an incomplete backup was accepted")
	}
	garbage := map[string][]byte{}
	for name := range nodeFileTargets("") {
		garbage[name] = []byte("neither a database nor valid ciphertext")
	}
	if _, err := decodeBackupFiles(garbage, nil); err == nil {
		t.Fatal("files that are no databases were accepted")
	}
	if _, err := decodeBackupFiles(garbage, make([]byte, 32)); err == nil {
		t.Fatal("files that do not decrypt were accepted")
	}
	wallet := filepath.Join(c.dir(), "data", "chain", "bitcoin", "mainnet", "wallet.db")
	if got := markerOf(t, wallet); got != "first" {
		t.Fatalf("the working restore now holds %q", got)
	}
	// A database cut short passes the header check and must still not be
	// placed: the staged files are opened and walked first.
	cut := map[string][]byte{}
	for name := range nodeFileTargets("") {
		whole := testDB(t, "cut")
		cut[name] = whole[:len(whole)/2]
	}
	if _, err := c.placeBackup(nodeA, cut, true); err == nil {
		t.Fatal("databases cut short were placed")
	}
	if got := markerOf(t, wallet); got != "first" {
		t.Fatalf("after the refused restore the working one holds %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "backups"))
	for _, e := range entries {
		if e.Name() != nodeA && e.Name() != nodeA+".restoring" {
			t.Fatalf("backups folder holds %s: the working restore was moved", e.Name())
		}
	}
}

// A folder with a wallet but without its channel database is no restored
// app: lnd would start on it with an empty channel database.
func TestHalfARestoreIsNoNode(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	dir := c.backupDir(nodeA)
	chain := filepath.Join(dir, "data", "chain", "bitcoin", "mainnet")
	os.MkdirAll(chain, 0700)
	os.WriteFile(filepath.Join(chain, "wallet.db"), []byte("half"), 0600)
	if hasNode(dir, "mainnet") {
		t.Fatal("a wallet alone counts as a restored app")
	}
	// Restoring over it keeps the half one aside, never deletes it.
	if err := place(t, c, nodeA, "whole", true); err != nil {
		t.Fatal(err)
	}
	if !c.HasRestoredNode() {
		t.Fatal("the complete restore does not count")
	}
	found := false
	entries, _ := os.ReadDir(filepath.Join(root, "backups"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), nodeA+".replaced-") {
			found = true
		}
	}
	if !found {
		t.Fatal("the half restore's wallet was not kept aside")
	}
}

// A process that runs the library on one backup must not touch another.
func TestLibraryBindsTheProcessToOneFolder(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if err := place(t, c, nodeA, "A", false); err != nil {
		t.Fatal(err)
	}
	boundLibDir = c.dir()
	defer func() { boundLibDir = "" }()
	if err := place(t, c, nodeB, "B", false); !errors.Is(err, ErrLibraryBound) {
		t.Fatalf("other backup while bound: %v, want ErrLibraryBound", err)
	}
	if err := place(t, c, nodeA, "A2", true); !errors.Is(err, ErrLibraryBound) {
		t.Fatalf("restore over the running backup: %v, want ErrLibraryBound", err)
	}
}

func TestLegacyLayoutIsMovedIntoBackups(t *testing.T) {
	root := t.TempDir()
	fakeNode(t, root, "legacy")
	os.WriteFile(filepath.Join(root, "latest_backup_snapshot.json"), []byte(`{"NodeID":"`+nodeA+`"}`), 0600)
	os.WriteFile(filepath.Join(root, "breez.db"), []byte("db"), 0600)
	os.WriteFile(filepath.Join(root, tokenFileName), []byte("token"), 0600)
	os.WriteFile(filepath.Join(root, "holiday.jpg"), []byte("not ours"), 0600)

	c := testCore(t, root)
	if c.layoutErr != nil {
		t.Fatal(c.layoutErr)
	}
	dir := filepath.Join(root, "backups", nodeA)
	if c.dir() != dir || !c.HasRestoredNode() {
		t.Fatalf("after the move the node is in %q", c.dir())
	}
	for _, moved := range []string{"breez.db", "latest_backup_snapshot.json"} {
		if _, err := os.Stat(filepath.Join(dir, moved)); err != nil {
			t.Errorf("%s was not moved", moved)
		}
	}
	for _, stays := range []string{tokenFileName, "holiday.jpg"} {
		if _, err := os.Stat(filepath.Join(root, stays)); err != nil {
			t.Errorf("%s should stay in the work folder", stays)
		}
	}
}

func TestBackupNamesCannotLeaveTheWorkFolder(t *testing.T) {
	c := testCore(t, t.TempDir())
	for _, name := range []string{"", "..", "../x", "a/b", `a\b`, "UPPER", "x"} {
		if err := place(t, c, name, "x", false); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}
