package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// The window's side, with the test binary as its node helpers (TestMain).

type event struct {
	name string
	data interface{}
}

// window is an App whose helpers are fake helper processes.
type window struct {
	t *testing.T
	a *App

	mu     sync.Mutex
	events []event
	spawns []string // the backup of every helper started
}

// newWindow makes an App on workDir. Its n-th helper runs in modes[n], the
// last mode for the rest.
func newWindow(t *testing.T, workDir string, modes ...string) *window {
	t.Helper()
	w := &window{t: t}
	cfg := core.DefaultConfig()
	cfg.WorkDir = workDir
	a := &App{ctx: context.Background(), cfg: cfg, log: newLogBuffer(20000),
		stopWait: 10 * time.Second, cancelWait: 10 * time.Second}
	a.emitFn = w.record
	a.log.emit = w.record
	a.core = core.New(cfg, &reporter{app: a})
	a.logDir = a.core.NodeDir()
	a.newHelper = func(name string, cfg core.Config) (*exec.Cmd, error) {
		w.mu.Lock()
		mode := modes[min(len(w.spawns), len(modes)-1)]
		w.spawns = append(w.spawns, name)
		w.mu.Unlock()
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = withEnv(os.Environ(), fakeHelperEnv+"="+mode, helperEnv+"="+name)
		return cmd, nil
	}
	w.a = a
	t.Cleanup(func() {
		a.opMu.Lock()
		a.stopHelper(false)
		a.opMu.Unlock()
	})
	return w
}

func (w *window) record(name string, data ...interface{}) {
	if name == "log" {
		return
	}
	e := event{name: name}
	if len(data) > 0 {
		e.data = data[0]
	}
	w.mu.Lock()
	w.events = append(w.events, e)
	w.mu.Unlock()
}

func (w *window) seen() []event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]event(nil), w.events...)
}

func (w *window) started() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.spawns...)
}

// named returns the events called name.
func (w *window) named(name string) []event {
	var out []event
	for _, e := range w.seen() {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

// waitFor waits until an event called name was sent.
func (w *window) waitFor(name string) event {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e := w.named(name); len(e) > 0 {
			return e[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("no %q event", name)
	return event{}
}

func (w *window) log() []string { return w.a.log.snapshot() }

// stop stops the helper as a switch of backups does.
func (w *window) stop() {
	w.a.opMu.Lock()
	defer w.a.opMu.Unlock()
	w.a.stopHelper(false)
}

// syncs returns the messages of the sync events.
func (w *window) syncs() []core.SyncProgress {
	var out []core.SyncProgress
	for _, e := range w.named("sync") {
		out = append(out, e.data.(core.SyncProgress))
	}
	return out
}

// restored makes a restored backup in workDir, the one in use when current.
func restored(t *testing.T, workDir, name string, current bool) string {
	t.Helper()
	dir := filepath.Join(workDir, "backups", name)
	for _, rel := range []string{"data/chain/bitcoin/mainnet/wallet.db", "data/graph/mainnet/channel.db", "breez.db"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if current {
		if err := os.WriteFile(filepath.Join(workDir, "current"), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func count(lines []string, s string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

func inOrder(t *testing.T, lines []string, parts ...string) {
	t.Helper()
	at := -1
	for _, p := range parts {
		i := contains(lines[at+1:], p)
		if i < 0 {
			t.Fatalf("%q missing or out of order in the log:\n%s", p, strings.Join(lines, "\n"))
		}
		at += 1 + i
	}
}

// The node runs in a helper: its calls are answered there, its lines are
// in the log once, the sync screen moves while it starts, and a stop ends
// the process and frees the backup.
func TestNodeHelperCalls(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "sync")
	st, err := w.a.StartAndSync()
	if err != nil || st == nil || st.NodeID != "fake" {
		t.Fatalf("sync: %+v, %v", st, err)
	}
	if !w.a.c().LibraryBound() {
		t.Error("the backup is not marked in use while its node runs")
	}
	syncs := w.syncs()
	if len(syncs) == 0 || syncs[0].Stage != "start" || syncs[0].Percent != -1 || syncs[0].Message != "Starting the node..." {
		t.Errorf("first sync event %+v", syncs)
	}
	lines := w.log()
	if n := count(lines, "[recovery] Starting the node..."); n != 1 {
		t.Errorf("the helper's progress line is in the log %d times", n)
	}
	inOrder(t, lines, "--- start node and sync", "The node runs in a helper process", "fake node on backup-aaaa", "Catching up")

	if st, err := w.a.GetStatus(); err != nil || st.NodeID != "fake" {
		t.Errorf("status: %+v, %v", st, err)
	}
	if plan, err := w.a.PrepareSweep(" bc1qaddress "); err != nil || plan.Address != "bc1qaddress" {
		t.Errorf("prepare: %+v, %v", plan, err)
	}
	if txid, err := w.a.BroadcastSweep(6); err != nil || txid != "tx6" {
		t.Errorf("broadcast: %q, %v", txid, err)
	}
	if st, err := w.a.StartAndSync(); err != nil || st == nil {
		t.Errorf("a second sync on the same node: %+v, %v", st, err)
	}
	if got := w.started(); len(got) != 1 || got[0] != "backup-aaaa" {
		t.Errorf("helpers started: %q", got)
	}

	w.stop()
	if w.a.runningHelper() != nil || w.a.c().LibraryBound() {
		t.Error("the helper is still marked as running")
	}
	inOrder(t, w.log(), "Stopping the node...", "Shutdown complete", "The node has stopped.")
	if _, err := w.a.GetStatus(); !errors.Is(err, errNodeNotRunning) {
		t.Errorf("status with no node: %v", err)
	}
	if e := w.named("nodestopped"); len(e) != 0 {
		t.Errorf("a stop the window asked for sent %v", e)
	}
}

// A planned restart is a new helper within the same sync, and the page
// sees the node start again.
func TestNodeHelperPlannedRestart(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "restart", "restart", "sync")
	st, err := w.a.StartAndSync()
	if err != nil || st == nil {
		t.Fatalf("sync: %+v, %v", st, err)
	}
	if n := len(w.started()); n != 3 {
		t.Errorf("%d helpers started, want 3", n)
	}
	again := 0
	for _, p := range w.syncs() {
		if p.Message == "Starting the node again..." && p.Stage == "start" && p.Percent == -1 {
			again++
		}
	}
	if again != 2 {
		t.Errorf("%d restart events, want 2", again)
	}
	lines := w.log()
	if count(lines, "The node starts again.") != 2 || count(lines, "error:") != 0 || count(lines, "unexpectedly") != 0 {
		t.Errorf("log:\n%s", strings.Join(lines, "\n"))
	}
	if e := w.named("nodestopped"); len(e) != 0 {
		t.Errorf("a restart sent %v", e)
	}
}

// A node that keeps asking to start again ends the sync with an error.
func TestNodeHelperRestartCap(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "restart")
	_, err := w.a.StartAndSync()
	if err == nil || !strings.Contains(err.Error(), "7 times in a row") {
		t.Fatalf("sync: %v", err)
	}
	if n := len(w.started()); n != maxNodeRestarts+1 {
		t.Errorf("%d helpers started, want %d", n, maxNodeRestarts+1)
	}
	if w.a.runningHelper() != nil || w.a.c().LibraryBound() {
		t.Error("a helper is left after the last restart")
	}
}

// A crash fails the call that waits, with the trace in the log before the
// error, and nothing starts again by itself.
func TestNodeHelperCrashWithACall(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "crash", "sync")
	if _, err := w.a.StartAndSync(); !errors.Is(err, errNodeCrashed) {
		t.Fatalf("sync: %v", err)
	}
	inOrder(t, w.log(), "panic: fake crash", "The node stopped unexpectedly (exit status 2).", "error: the node stopped unexpectedly")
	if e := w.named("nodestopped"); len(e) != 0 {
		t.Errorf("nodestopped sent while a call waited: %v", e)
	}
	if w.a.runningHelper() != nil || w.a.c().LibraryBound() {
		t.Error("the crashed helper is still marked as running")
	}
	if n := len(w.started()); n != 1 {
		t.Errorf("%d helpers started after a crash", n)
	}
	// Continue recovery starts a new one.
	if st, err := w.a.StartAndSync(); err != nil || st == nil {
		t.Errorf("sync after the crash: %+v, %v", st, err)
	}
}

// A crash while no call waits (the funds screen) is sent to the page.
func TestNodeHelperCrashWhileIdle(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "crashidle")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	e := w.waitFor("nodestopped")
	if e.data != errNodeCrashed.Error() {
		t.Errorf("nodestopped %v", e.data)
	}
	inOrder(t, w.log(), "panic: fake crash while idle", "The node stopped unexpectedly")
	time.Sleep(300 * time.Millisecond)
	if w.a.runningHelper() != nil || len(w.started()) != 1 {
		t.Error("the node started again by itself")
	}
	if _, err := w.a.GetStatus(); !errors.Is(err, errNodeNotRunning) {
		t.Errorf("status after the crash: %v", err)
	}
}

// A helper that does not stop is killed, and the next one starts only once
// it is gone.
func TestNodeHelperStopKills(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "hang", "sync")
	w.a.stopWait = 300 * time.Millisecond
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	p := w.a.runningHelper()
	start := time.Now()
	w.stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stop took %v", took)
	}
	if p.cmd.ProcessState == nil || p.cmd.ProcessState.Success() {
		t.Errorf("helper state %v", p.cmd.ProcessState)
	}
	inOrder(t, w.log(), "Stopping the node...", "The node did not stop within 300ms; its process is ended.", "The node has stopped (signal: killed).")
	if e := w.named("nodestopped"); len(e) != 0 {
		t.Errorf("a kill the window did sent %v", e)
	}
	if _, err := w.a.StartAndSync(); err != nil {
		t.Errorf("sync after the kill: %v", err)
	}
}

// A call that ignores its cancel ends with the helper.
func TestNodeHelperCancelKills(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "stuck")
	w.a.cancelWait = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := w.a.StartAndSync()
		done <- err
	}()
	w.waitFor("sync")
	for len(w.named("sync")) < 3 { // the reports before the stuck wait
		time.Sleep(20 * time.Millisecond)
	}
	w.a.Cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "cancelled" {
			t.Errorf("sync: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled sync did not return")
	}
	inOrder(t, w.log(), "cancel requested", "did not stop the call within 300ms", "The node has stopped")
	if w.a.runningHelper() != nil {
		t.Error("the killed helper is still marked as running")
	}
}

// The log belongs to a backup: switching backups, or the work folder,
// stops the node and moves the log so far into the old backup's folder.
// Settings change after a node ran.
func TestLogPerBackup(t *testing.T) {
	root := t.TempDir()
	dirA := restored(t, root, "backup-aaaa", true)
	dirB := restored(t, root, "backup-bbbb", false)
	w := newWindow(t, root, "sync")
	w.a.logHeader()
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	if err := w.a.UseRestored("backup-bbbb"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range w.seen() {
		if e.name == "stopping" || e.name == "logreset" {
			names = append(names, e.name)
		}
	}
	if strings.Join(names, ",") != "stopping,logreset" {
		t.Errorf("events %q", names)
	}
	logA := readLog(t, dirA)
	inOrder(t, logA, "Work dir:", "fake node on backup-aaaa", "The node has stopped.")
	lines := w.log()
	if count(lines, "backup-aaaa") != 0 || count(lines, "Work dir:") != 1 || count(lines, "Continuing with the backup already restored") != 1 {
		t.Errorf("log after the switch:\n%s", strings.Join(lines, "\n"))
	}

	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.started(), ","); got != "backup-aaaa,backup-bbbb" {
		t.Errorf("helpers %s", got)
	}
	if _, err := w.a.ApplySettings(Settings{WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("settings after a node ran: %v", err)
	}
	if w.a.runningHelper() != nil || w.a.c().LibraryBound() {
		t.Error("the node still runs after the settings changed")
	}
	logB := readLog(t, dirB)
	inOrder(t, logB, "Continuing with the backup already restored", "fake node on backup-bbbb", "The node has stopped.")
	if count(logB, "backup-aaaa") != 0 || count(readLog(t, dirA), "backup-bbbb") != 0 {
		t.Error("a backup's log has the other's lines")
	}
	if lines := w.log(); count(lines, "backup-bbbb") != 0 {
		t.Errorf("log after the settings:\n%s", strings.Join(lines, "\n"))
	}
}

func readLog(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, backupLogFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}
