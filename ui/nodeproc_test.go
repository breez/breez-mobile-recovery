package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	bolt "go.etcd.io/bbolt"
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
	a.ask = func(o wruntime.MessageDialogOptions) (string, error) {
		t.Errorf("unexpected question %q", o.Title)
		return "No", nil
	}
	a.quit = func() { t.Error("unexpected quit") }
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
		// Windows does not delete an open file.
		core.ReleaseLocks(a.c().Config().WorkDir)
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

// waitForSync waits until a sync event with the message msg was sent.
func (w *window) waitForSync(msg string) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range w.syncs() {
			if p.Message == msg {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("no sync event %q", msg)
}

// syncInBackground runs StartAndSync; the result arrives on the channel.
func (w *window) syncInBackground() <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := w.a.StartAndSync()
		done <- err
	}()
	return done
}

// result waits for the outcome of syncInBackground.
func (w *window) result(done <-chan error) error {
	w.t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		w.t.Fatal("the sync did not return")
		return nil
	}
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
	inOrder(t, w.log(), "panic: fake crash", "The node stopped unexpectedly (exit status 2).", "error: The node stopped unexpectedly. Check the logs")
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
	// The kill reads "signal: killed" on Unix, "exit status 1" on Windows.
	inOrder(t, w.log(), "Stopping the node...", "The node did not stop within 300ms; its process is ended.", "The node has stopped (")
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
	done := w.syncInBackground()
	w.waitForSync("Looking for funds") // the last report before the stuck wait
	w.a.Cancel()
	if err := w.result(done); err == nil || err.Error() != "cancelled" {
		t.Errorf("sync: %v", err)
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
	if err := w.a.UseRestored("backup-bbbb", false); err != nil {
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
	if _, err := w.a.ApplySettings(Settings{WorkDir: t.TempDir()}, false); err != nil {
		t.Fatalf("settings after a node ran: %v", err)
	}
	// The old work folder is free for another program.
	db, err := bolt.Open(filepath.Join(root, "instance.lock"), 0600, &bolt.Options{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Errorf("the old work folder is still locked: %v", err)
	} else {
		db.Close()
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

// answers makes the window answer a question by its title, "No" when the
// title is not in the map, and returns the questions asked so far.
func (w *window) answers(by map[string]string) func() []wruntime.MessageDialogOptions {
	var mu sync.Mutex
	var asked []wruntime.MessageDialogOptions
	w.a.ask = func(o wruntime.MessageDialogOptions) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, o)
		if a, ok := by[o.Title]; ok {
			return a, nil
		}
		return "No", nil
	}
	return func() []wruntime.MessageDialogOptions {
		mu.Lock()
		defer mu.Unlock()
		return append([]wruntime.MessageDialogOptions(nil), asked...)
	}
}

// titles joins the titles of questions.
func titles(qs []wruntime.MessageDialogOptions) string {
	var s []string
	for _, q := range qs {
		s = append(s, q.Title)
	}
	return strings.Join(s, ",")
}

// leaving checks the question asked before the node of the backup in use
// stops: it names that app, since it is asked where another one may be
// picked, and ends with what stops it.
func leaving(t *testing.T, q wruntime.MessageDialogOptions, what string) {
	t.Helper()
	if q.Title != what+"?" || !strings.HasPrefix(q.Message, "Funds of the app in use are still on their way") || !strings.HasSuffix(q.Message, "\n\n"+what+" now?") {
		t.Errorf("question %q: %q", q.Title, q.Message)
	}
}

// kept fails the test when the node p is no longer the one running, or
// another one was started or stopped.
func (w *window) kept(p *nodeProc, what string) {
	w.t.Helper()
	if w.a.runningHelper() != p || len(w.started()) != 1 || len(w.named("stopping")) != 0 {
		w.t.Errorf("%s stopped or started the node", what)
	}
}

// Switch backup opens the start screen with the node running: the state and
// the restored apps it reads leave the node alone, the state offers Back to
// funds, and Back to funds shows the funds of the same node.
func TestSwitchBackupKeepsTheNode(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	restored(t, root, "backup-bbbb", false)
	w := newWindow(t, root, "sync")
	if w.a.GetState().NodeSynced {
		t.Error("Back to funds offered before the node ran")
	}
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	p := w.a.runningHelper()
	if !w.a.GetState().NodeSynced {
		t.Error("no Back to funds while the node runs")
	}
	var inUse []string
	for _, rb := range w.a.RestoredApps() {
		if rb.Current {
			inUse = append(inUse, rb.Name)
		}
	}
	if strings.Join(inUse, ",") != "backup-aaaa" {
		t.Errorf("in use: %q", inUse)
	}
	if st, err := w.a.GetStatus(); err != nil || st.NodeID != "fake" {
		t.Errorf("Back to funds: %+v, %v", st, err)
	}
	w.kept(p, "Switch backup")
	w.stop()
	if w.a.GetState().NodeSynced {
		t.Error("Back to funds offered with no node")
	}
}

// Continue recovery on the backup in use, its node running, does not start
// the node again: after a sync that ended well the funds come from it at
// once, and after a sync stopped once the node was up (no Back to funds:
// its channels were not checked) the new sync runs on it.
func TestContinueWithTheBackupInUse(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	restored(t, root, "backup-bbbb", false)
	w := newWindow(t, root, "waitcancel")
	done := w.syncInBackground()
	w.waitForSync("Looking for funds")
	w.a.Cancel()
	if err := w.result(done); err == nil || err.Error() != "cancelled" {
		t.Fatalf("sync: %v", err)
	}
	p := w.a.runningHelper()
	if p == nil || w.a.GetState().NodeSynced {
		t.Fatal("Back to funds offered after a stopped sync, or the node is gone")
	}
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	if !w.a.GetState().NodeSynced {
		t.Error("no Back to funds after the sync")
	}
	if st, err := w.a.GetStatus(); err != nil || st.NodeID != "fake" {
		t.Errorf("funds: %+v, %v", st, err)
	}
	w.kept(p, "Continue recovery")
}

// Continue recovery on another backup stops the node first, and asks first
// when funds of the app are on their way; a No keeps it running.
func TestSwitchToAnotherBackupStopsTheNode(t *testing.T) {
	root := t.TempDir()
	dirA := restored(t, root, "backup-aaaa", true)
	restored(t, root, "backup-bbbb", false)
	w := newWindow(t, root, "sync")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	p := w.a.runningHelper()
	answer := map[string]string{"Switch backup?": "No"}
	asked := w.answers(answer)
	if err := w.a.UseRestored("backup-bbbb", true); err == nil || err.Error() != "switch backup cancelled" {
		t.Errorf("switch answered No: %v", err)
	}
	if w.a.c().CurrentBackup() != "backup-aaaa" {
		t.Error("a No switched the backup")
	}
	w.kept(p, "a No")
	answer["Switch backup?"] = "Yes"
	if err := w.a.UseRestored("backup-bbbb", true); err != nil {
		t.Fatal(err)
	}
	if got := titles(asked()); got != "Switch backup?,Switch backup?" {
		t.Errorf("asked %s", got)
	}
	leaving(t, asked()[0], "Switch backup")
	if w.a.runningHelper() != nil || w.a.c().CurrentBackup() != "backup-bbbb" {
		t.Error("the node still runs, or the backup did not change")
	}
	inOrder(t, readLog(t, dirA), "fake node on backup-aaaa", "The node has stopped.")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.started(), ","); got != "backup-aaaa,backup-bbbb" {
		t.Errorf("helpers %s", got)
	}
}

// A restore stops the node of the backup in use before it begins, and asks
// first when funds of that app are on their way; a No keeps it running.
// Continuing with the backup in use, already restored, keeps its node, and
// with no node running there is nothing to ask.
func TestRestoreStopsTheNodeFirst(t *testing.T) {
	root := t.TempDir()
	dirA := restored(t, root, "backup-aaaa", true)
	restored(t, root, "backup-bbbb", false)
	w := newWindow(t, root, "sync")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	p := w.a.runningHelper()
	answer := map[string]string{"Already restored": "Yes", "Restore another backup?": "No"}
	asked := w.answers(answer)
	restore := func(name string) error {
		return w.a.Restore(RestoreRequest{Source: "google", NodeID: name, Ask: true})
	}
	if err := restore("backup-aaaa"); err != nil {
		t.Fatalf("the backup in use: %v", err)
	}
	w.kept(p, "continuing with the backup in use")
	// Restoring the backup in use again stops its node too.
	answer["Already restored"] = "No"
	if err := restore("backup-aaaa"); err == nil || err.Error() != "restore it again cancelled" {
		t.Errorf("restore again answered No: %v", err)
	}
	w.kept(p, "a No to restoring the backup in use again")
	answer["Already restored"] = "Yes"
	if err := restore("backup-bbbb"); err == nil || err.Error() != "restore another backup cancelled" {
		t.Errorf("restore answered No: %v", err)
	}
	if w.a.c().CurrentBackup() != "backup-aaaa" {
		t.Error("a No changed the backup")
	}
	w.kept(p, "a No")
	answer["Restore another backup?"] = "Yes"
	if err := restore("backup-bbbb"); err != nil {
		t.Fatal(err)
	}
	if w.a.runningHelper() != nil || w.a.c().CurrentBackup() != "backup-bbbb" {
		t.Error("the node still runs, or the backup did not change")
	}
	inOrder(t, readLog(t, dirA), "fake node on backup-aaaa", "Stopping the node...", "The node has stopped.")
	if err := restore("backup-aaaa"); err != nil {
		t.Fatal(err)
	}
	want := "Already restored,Already restored,Restore it again?,Already restored,Restore another backup?,Already restored,Restore another backup?,Already restored"
	qs := asked()
	if got := titles(qs); got != want {
		t.Errorf("asked %s", got)
	} else {
		leaving(t, qs[2], "Restore it again")
		leaving(t, qs[4], "Restore another backup")
	}
}

// Apply in Advanced settings, on the start screen next to Back to funds,
// leaves a running node alone when nothing changed, and asks before it
// stops the node while funds of the app in use are on their way.
func TestApplySettingsWithTheNodeRunning(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "sync")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	p := w.a.runningHelper()
	answer := map[string]string{"Apply settings?": "No"}
	asked := w.answers(answer)
	peers := w.a.c().Config().Peers
	if st, err := w.a.ApplySettings(Settings{WorkDir: " " + root + " ", Peers: peers}, true); err != nil || !st.NodeSynced {
		t.Errorf("the same settings: %+v, %v", st, err)
	}
	w.kept(p, "the same settings")
	other := Settings{WorkDir: root, Peers: "127.0.0.1:8333"}
	if _, err := w.a.ApplySettings(other, true); err == nil || err.Error() != "apply settings cancelled" {
		t.Errorf("settings answered No: %v", err)
	}
	if w.a.c().Config().Peers != peers {
		t.Error("a No changed the settings")
	}
	w.kept(p, "a No")
	answer["Apply settings?"] = "Yes"
	if st, err := w.a.ApplySettings(other, true); err != nil || st.NodeSynced || st.Peers != other.Peers {
		t.Errorf("settings answered Yes: %+v, %v", st, err)
	}
	if w.a.runningHelper() != nil {
		t.Error("the node still runs after the settings changed")
	}
	if qs := asked(); titles(qs) != "Apply settings?,Apply settings?" {
		t.Errorf("asked %s", titles(qs))
	} else {
		leaving(t, qs[0], "Apply settings")
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

// Stop during the node's start, then Continue: the library's app starts
// only once per process, so the sync goes on in a new helper.
func TestNodeHelperStopDuringStart(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "startcancel", "sync")
	done := w.syncInBackground()
	w.waitForSync("Starting the node...")
	w.a.Cancel()
	if err := w.result(done); err == nil || err.Error() != "cancelled" {
		t.Fatalf("sync: %v", err)
	}
	if st, err := w.a.StartAndSync(); err != nil || st == nil {
		t.Fatalf("Continue after a stop during the start: %+v, %v", st, err)
	}
	if n := len(w.started()); n != 2 {
		t.Errorf("%d helpers started, want 2", n)
	}
}

// Stop once the node is up, then Continue: the node is kept.
func TestNodeHelperStopDuringSyncKeepsTheNode(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "waitcancel")
	done := w.syncInBackground()
	w.waitForSync("Looking for funds")
	w.a.Cancel()
	if err := w.result(done); err == nil || err.Error() != "cancelled" {
		t.Fatalf("sync: %v", err)
	}
	if st, err := w.a.StartAndSync(); err != nil || st == nil {
		t.Fatalf("Continue: %+v, %v", st, err)
	}
	if n := len(w.started()); n != 1 {
		t.Errorf("%d helpers started, want 1", n)
	}
}

// Stop pressed while the node of a failed sync stops: no new node starts,
// and the page is told the sync was stopped, not that a peer failed.
func TestNodeHelperStopWhileTheOldNodeStops(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "failstart", "sync")
	if _, err := w.a.StartAndSync(); err == nil || err.Error() != "start failed" {
		t.Fatalf("first sync: %v", err)
	}
	done := w.syncInBackground()
	w.waitForSync("Stopping the node to start it again...")
	w.a.Cancel()
	if err := w.result(done); err == nil || err.Error() != "cancelled" {
		t.Fatalf("sync: %v", err)
	}
	if n := len(w.started()); n != 1 {
		t.Errorf("%d helpers started after the stop, want 1", n)
	}
	if st, err := w.a.StartAndSync(); err != nil || st == nil {
		t.Fatalf("Continue: %+v, %v", st, err)
	}
}

// A call whose Stop was pressed already is not sent.
func TestNodeCallAfterStop(t *testing.T) {
	root := t.TempDir()
	restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "sync")
	w.a.opMu.Lock()
	p, err := w.a.startHelper()
	w.a.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.call(ctx, helperRequest{Call: callStartAndSync}, time.Second, w.a.log.tool); !errors.Is(err, context.Canceled) {
		t.Errorf("call after the stop: %v", err)
	}
	p.mu.Lock()
	sent := p.lastID
	p.mu.Unlock()
	if sent != 0 {
		t.Error("the call was sent")
	}
}

// Closing while the node runs asks first, and the window stays while the
// node stops, so the page can show it: on Windows this runs on the
// window's thread. A second click meanwhile does nothing. The log goes to
// the backup before the program quits.
func TestCloseStopsTheNodeFirst(t *testing.T) {
	root := t.TempDir()
	dir := restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "slowstop")
	if _, err := w.a.StartAndSync(); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	w.a.ask = func(wruntime.MessageDialogOptions) (string, error) {
		asked.Add(1)
		return "Yes", nil
	}
	// The window closes when the quit's own check lets it.
	quit := make(chan bool, 1)
	w.a.quit = func() { quit <- w.a.beforeClose(context.Background()) }
	start := time.Now()
	if !w.a.beforeClose(context.Background()) {
		t.Fatal("the window closed with the node running")
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Errorf("the close waited %v", took)
	}
	if !w.a.beforeClose(context.Background()) {
		t.Error("a second click closed the window while the node stopped")
	}
	select {
	case kept := <-quit:
		if kept {
			t.Error("the quit after the stop was held back")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no quit")
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked %d times", n)
	}
	if w.a.runningHelper() != nil {
		t.Error("the node still runs")
	}
	inOrder(t, readLog(t, dir), "fake node on backup-aaaa", "The node has stopped.")
	var names []string
	for _, e := range w.seen() {
		if e.name == "stopping" || e.name == "logreset" {
			names = append(names, e.name)
		}
	}
	if strings.Join(names, ",") != "stopping,logreset" {
		t.Errorf("events %q", names)
	}
}

// With nothing running the window closes at once, without a question, and
// the log goes to the backup.
func TestCloseWhenIdle(t *testing.T) {
	root := t.TempDir()
	dir := restored(t, root, "backup-aaaa", true)
	w := newWindow(t, root, "sync")
	w.a.logHeader()
	if w.a.beforeClose(context.Background()) {
		t.Fatal("the window was kept")
	}
	inOrder(t, readLog(t, dir), "Work dir:")
}
