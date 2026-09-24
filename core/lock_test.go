package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type progressRecorder struct {
	nopReporter
	mu    sync.Mutex
	lines []string
}

func (r *progressRecorder) Progress(msg string) {
	r.mu.Lock()
	r.lines = append(r.lines, msg)
	r.mu.Unlock()
}

// helperCore is a session as the node helper makes it.
func helperCore(root string, rep Reporter) *Core {
	cfg := DefaultConfig()
	cfg.WorkDir = root
	cfg.SkipLock = true
	return New(cfg, rep)
}

// placeAB restores backups A and B, in that order: B is in use.
func placeAB(t *testing.T, c *Core) {
	t.Helper()
	if err := place(t, c, nodeA, "A", false); err != nil {
		t.Fatal(err)
	}
	if err := place(t, c, nodeB, "B", false); err != nil {
		t.Fatal(err)
	}
}

// holdElsewhere holds a folder's lock the way another process does: an
// open of its own, outside this process's table of locks.
func holdElsewhere(t *testing.T, dir string) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, lockFileName), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// The node helper works under the window's lock: it neither takes the work
// folder's lock nor tidies the folder, and runs the backup it is given,
// whatever `current` says.
func TestHelperSessionLeavesTheWorkFolderToTheWindow(t *testing.T) {
	root := t.TempDir()
	w := testCore(t, root)
	placeAB(t, w)
	// An older release's node in the work folder is the window's to move.
	fakeNode(t, root, "legacy")

	h := helperCore(root, nopReporter{})
	if h.layoutErr != nil || h.dir() != "" {
		t.Fatalf("helper session: dir %q, err %v", h.dir(), h.layoutErr)
	}
	if err := h.LockBackup(nodeA); err != nil {
		t.Fatal(err)
	}
	if h.dir() != w.backupDir(nodeA) || !h.HasRestoredNode() {
		t.Fatalf("helper runs %q", h.dir())
	}
	if _, err := os.Stat(filepath.Join(w.backupDir(nodeA), lockFileName)); err != nil {
		t.Fatalf("the backup folder is not locked: %v", err)
	}
	if cur, _ := os.ReadFile(filepath.Join(root, currentFile)); strings.TrimSpace(string(cur)) != nodeB {
		t.Fatalf("current is %q, the helper must not change it", cur)
	}
	if !hasNode(root, "mainnet") {
		t.Fatal("the helper moved the old layout")
	}
	other := t.TempDir()
	helperCore(other, nopReporter{})
	if _, err := os.Stat(filepath.Join(other, lockFileName)); err == nil {
		t.Fatal("the helper locked the work folder")
	}

	if err := h.LockBackup("0000"); err == nil {
		t.Error("a folder that holds no restored app was accepted")
	}
	if err := w.LockBackup(nodeA); err == nil {
		t.Error("the window locked a backup folder")
	}
}

// A node left running by a window that crashed keeps its folder: the next
// window neither moves nor selects it until that node has stopped, and a
// new helper waits for it.
func TestBackupHeldByAnotherProcess(t *testing.T) {
	root := t.TempDir()
	w := testCore(t, root)
	placeAB(t, w)
	held := holdElsewhere(t, w.backupDir(nodeA))

	if err := w.UseBackup(nodeA); !errors.Is(err, ErrBackupInUse) {
		t.Fatalf("select a held backup: %v", err)
	}
	if err := place(t, w, nodeA, "A2", true); !errors.Is(err, ErrBackupInUse) {
		t.Fatalf("restore over a held backup: %v", err)
	}
	if got := markerOf(t, filepath.Join(w.backupDir(nodeA), "breez.db")); got != "A" {
		t.Fatalf("the held backup holds %q", got)
	}
	if w.CurrentBackup() != nodeB {
		t.Fatalf("in use: %q", w.CurrentBackup())
	}
	if err := w.UseBackup(nodeB); err != nil {
		t.Fatalf("another backup: %v", err)
	}

	rep := &progressRecorder{}
	h := helperCore(root, rep)
	go func() {
		time.Sleep(300 * time.Millisecond)
		held.Close()
	}()
	if err := h.LockBackup(nodeA); err != nil {
		t.Fatalf("helper after the old node stopped: %v", err)
	}
	rep.mu.Lock()
	said := strings.Join(rep.lines, "|")
	rep.mu.Unlock()
	if !strings.Contains(said, "Waiting for the node") {
		t.Errorf("the wait was not reported: %q", said)
	}

	// Free again once the helper is gone.
	locksMu.Lock()
	abs, _ := filepath.Abs(w.backupDir(nodeA))
	locks[abs].Close()
	delete(locks, abs)
	locksMu.Unlock()
	if err := w.UseBackup(nodeA); err != nil {
		t.Fatalf("select after the helper exited: %v", err)
	}
}

// While the window's helper runs a node, the window's guards treat its
// folder as the one the library runs on.
func TestSetInUse(t *testing.T) {
	root := t.TempDir()
	c := testCore(t, root)
	placeAB(t, c)
	SetInUse(c.backupDir(nodeA), 7)
	defer SetInUse("", 0)
	if !c.LibraryBound() {
		t.Error("not bound while a helper runs")
	}
	if err := c.UseBackup(nodeB); !errors.Is(err, ErrLibraryBound) {
		t.Errorf("switch while a helper runs: %v", err)
	}
	if err := place(t, c, "zip-00000000000000c1", "C", false); !errors.Is(err, ErrLibraryBound) {
		t.Errorf("restore while a helper runs: %v", err)
	}
	for _, b := range c.RestoredBackups() {
		if b.Name == nodeA && b.Payments != 7 {
			t.Errorf("payments of the running backup: %d, want the count taken before", b.Payments)
		}
	}

	SetInUse("", 0)
	if c.LibraryBound() {
		t.Error("still bound after the helper exited")
	}
	if err := c.UseBackup(nodeA); err != nil {
		t.Fatal(err)
	}
}
