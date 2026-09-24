package core

import (
	"errors"
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
// releases it when the process ends, however it ends. The wait covers a
// copy that is still winding down: a window just closed, or a command line
// run that just ended.
//
// The app's node helper runs under the window's lock and locks its backup
// folder (backups/<name>/instance.lock) the same way, and so does the
// command line when it starts a node. The window checks that lock before
// it moves or selects a backup folder: a helper left running by a window
// that crashed writes into its folder until its own stop ends, 25 s at
// most.
const (
	lockFileName = "instance.lock"
	lockWait     = 20 * time.Second
	// backupLockProbe covers an operating system that releases the lock of
	// a helper that has just exited a moment late.
	backupLockProbe = 2 * time.Second
)

var (
	locksMu sync.Mutex
	locks   = map[string]*bolt.DB{}
	// backupLockWait is how long a node's start waits for the lock of its
	// backup folder; a variable for the tests.
	backupLockWait = lockWait
)

// ErrBackupInUse is returned while a node in another process still runs on
// a backup's folder.
var ErrBackupInUse = errors.New("the node of this backup is still stopping; try again in a moment")

func lockWorkDir(workDir string) error {
	return holdLock(workDir, lockWait, func(dir string, err error) error {
		return fmt.Errorf("Breez Recovery is already running on %s (close the other window first): %w", dir, err)
	})
}

// holdLock takes the lock of folder for the rest of the process, waiting up
// to wait for another process to release it. busy wraps an error of the
// lock file itself.
func holdLock(folder string, wait time.Duration, busy func(dir string, err error) error) error {
	locksMu.Lock()
	defer locksMu.Unlock()
	dir, err := filepath.Abs(folder)
	if err != nil {
		return err
	}
	if locks[dir] != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	db, err := bolt.Open(filepath.Join(dir, lockFileName), 0600, &bolt.Options{Timeout: wait})
	if err != nil {
		return busy(dir, err)
	}
	locks[dir] = db
	return nil
}

// ReleaseLocks gives up this process's locks of the folders dirs: the app's
// for a work folder it no longer uses, and the tests' before their
// temporary folders are removed (Windows does not delete an open file).
func ReleaseLocks(dirs ...string) {
	locksMu.Lock()
	defer locksMu.Unlock()
	for _, d := range dirs {
		if d == "" {
			continue
		}
		dir, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		if db := locks[dir]; db != nil {
			db.Close()
			delete(locks, dir)
		}
	}
}

// LockBackup makes the restored backup name the one in use in a session
// made with SkipLock, the app's node helper. It holds the backup's folder
// for the rest of the process, first waiting for a node that still runs on
// it in another process. It does not write `current`: that is the window's.
func (c *Core) LockBackup(name string) error {
	if !c.cfg.SkipLock {
		return errors.New("LockBackup needs a session made with SkipLock")
	}
	if c.layoutErr != nil {
		return c.layoutErr
	}
	if !c.IsRestored(name) {
		return fmt.Errorf("no restored backup %q in %s", name, c.cfg.WorkDir)
	}
	dir := c.backupDir(name)
	if err := c.holdBackup(dir); err != nil {
		return err
	}
	c.nodeDir = dir
	return nil
}

// holdBackup takes the lock of the backup folder dir for the rest of the
// process, first waiting for a node that still runs on it in another
// process: the helper of a window that crashed stops within 25 s. The node
// starts only under it, in the helper and on the command line.
func (c *Core) holdBackup(dir string) error {
	same := func(_ string, err error) error { return err }
	err := holdLock(dir, time.Millisecond, same)
	if errors.Is(err, bolt.ErrTimeout) {
		c.progressf("Waiting for the node that ran on this backup to stop...")
		err = holdLock(dir, backupLockWait, same)
	}
	if errors.Is(err, bolt.ErrTimeout) {
		return ErrBackupInUse
	}
	return err
}

// backupFree returns ErrBackupInUse while another process holds the lock of
// the backup folder dir. It changes nothing.
func backupFree(dir string) error {
	path := filepath.Join(dir, lockFileName)
	if _, err := os.Stat(path); err != nil {
		return nil // no helper ever ran on it
	}
	if abs, err := filepath.Abs(dir); err == nil {
		locksMu.Lock()
		mine := locks[abs] != nil
		locksMu.Unlock()
		if mine {
			return nil
		}
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: backupLockProbe})
	if errors.Is(err, bolt.ErrTimeout) {
		return ErrBackupInUse
	}
	if err == nil {
		db.Close()
	}
	// A lock file that cannot be read holds nothing.
	return nil
}
