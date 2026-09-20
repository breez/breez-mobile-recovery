package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// fakeNode puts the file HasRestoredNode looks for into dir.
func fakeNode(t *testing.T, dir, marker string) {
	t.Helper()
	chain := filepath.Join(dir, "data", "chain", "bitcoin", "mainnet")
	if err := os.MkdirAll(chain, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chain, "wallet.db"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
}

const nodeA = "02e66bcb1e3c97de679c0d5b2f831ac913e53acdc99542ed839c17b1079df489ea"
const nodeB = "02c6b28eb051854c632f12a91fb7b3fe0768f1e957bf842040c3289e5cf99179b6"

func TestBackupsGetTheirOwnFolders(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if c.HasRestoredNode() {
		t.Fatal("empty work folder reports a node")
	}
	if err := c.prepareBackupDir(nodeA, false); err != nil {
		t.Fatal(err)
	}
	fakeNode(t, c.dir(), "A")
	// lnd's channel backup of node A must never be seen by node B.
	scb := filepath.Join(c.dir(), "data", "chain", "bitcoin", "mainnet", "channel.backup")
	os.WriteFile(scb, []byte("A"), 0600)

	if err := c.prepareBackupDir(nodeB, false); err != nil {
		t.Fatal(err)
	}
	if c.dir() != filepath.Join(root, "backups", nodeB) {
		t.Fatalf("node B runs in %s", c.dir())
	}
	if entries, _ := os.ReadDir(c.dir()); len(entries) != 0 {
		t.Fatalf("node B's folder is not empty: %v", entries)
	}
	fakeNode(t, c.dir(), "B")

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

func TestRestoreAgainMovesTheOldFolderAside(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if err := c.prepareBackupDir(nodeA, false); err != nil {
		t.Fatal(err)
	}
	fakeNode(t, c.dir(), "first")

	if err := c.prepareBackupDir(nodeA, false); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("restoring again without force: %v, want ErrNodeExists", err)
	}
	if err := c.prepareBackupDir(nodeA, true); err != nil {
		t.Fatal(err)
	}
	if hasNode(c.dir(), "mainnet") {
		t.Fatal("the folder for the new restore still holds the old wallet")
	}
	kept := ""
	entries, _ := os.ReadDir(filepath.Join(root, "backups"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), nodeA+".replaced-") {
			kept = filepath.Join(root, "backups", e.Name())
		}
	}
	data, err := os.ReadFile(filepath.Join(kept, "data", "chain", "bitcoin", "mainnet", "wallet.db"))
	if err != nil || string(data) != "first" {
		t.Fatalf("the earlier wallet was not kept: %v", err)
	}
}

// A process that runs the library on one backup must not touch another.
func TestLibraryBindsTheProcessToOneFolder(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	if err := c.prepareBackupDir(nodeA, false); err != nil {
		t.Fatal(err)
	}
	boundLibDir = c.dir()
	defer func() { boundLibDir = "" }()
	if err := c.prepareBackupDir(nodeB, false); !errors.Is(err, ErrLibraryBound) {
		t.Fatalf("other backup while bound: %v, want ErrLibraryBound", err)
	}
	fakeNode(t, c.dir(), "A")
	if err := c.prepareBackupDir(nodeA, true); !errors.Is(err, ErrLibraryBound) {
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
		if err := c.prepareBackupDir(name, false); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}
