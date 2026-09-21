package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// One program at a time works on a work folder. Two would block each other
// on the library's databases with no message (breez.db is opened without a
// timeout), and a second one re-restoring a backup would move the folder
// from under the first, whose state files would then land in the new
// restore.
//
// The lock is an open bbolt file: bbolt holds an exclusive file lock on
// every platform for as long as the file is open, and the operating system
// releases it when the process ends, however it ends. The wait covers the
// app restarting itself: the new copy starts while the old one is still
// winding down.
const (
	lockFileName = "instance.lock"
	lockWait     = 20 * time.Second
)

var (
	locksMu sync.Mutex
	locks   = map[string]*bolt.DB{}
)

func lockWorkDir(workDir string) error {
	locksMu.Lock()
	defer locksMu.Unlock()
	dir, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	if locks[dir] != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	db, err := bolt.Open(filepath.Join(dir, lockFileName), 0600, &bolt.Options{Timeout: lockWait})
	if err != nil {
		return fmt.Errorf("Breez Recovery is already running on %s (close the other window first): %w", dir, err)
	}
	locks[dir] = db
	return nil
}
