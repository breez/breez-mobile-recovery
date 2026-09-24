package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// App is the object bound to the frontend. Every exported method is
// callable from JavaScript as window.go.main.App.<Method>.
//
// The node does not run in this process: it runs in a node helper, a child
// process of this program (helper.go, nodeproc.go), and every call that
// needs it goes there. The library it runs starts only once per process and
// stays bound to its first backup folder, so each new start of the node is
// a new helper, and the window stays.
type App struct {
	ctx context.Context

	emitMu sync.Mutex
	emitFn func(name string, data ...interface{}) // set once the window runs

	opMu     sync.Mutex // one long operation at a time
	cancelMu sync.Mutex
	cancel   context.CancelFunc

	coreMu sync.Mutex
	cfg    core.Config
	core   *core.Core

	log *logBuffer
	// logDir is the backup folder the log is about, "" for none yet. Only
	// with opMu held.
	logDir string

	historyMu   sync.Mutex
	lastHistory *core.History // what the History screen shows, for export

	nodeMu sync.Mutex
	node   *nodeProc // the running helper, nil when none
	// syncFailed: the last sync ended with an error, and the node may be
	// half started; the next sync starts a new helper. Only with opMu held.
	syncFailed bool
	// newHelper makes the command of a helper; tests run a fake one.
	newHelper            func(name string, cfg core.Config) (*exec.Cmd, error)
	stopWait, cancelWait time.Duration

	syncMu   sync.Mutex
	syncing  bool               // a sync call runs
	lastSync *core.SyncProgress // the helper's last sync report, nil before its first
}

func newApp() *App {
	a := &App{cfg: core.DefaultConfig(), log: newLogBuffer(20000),
		newHelper: helperCommand, stopWait: helperStopTimeout, cancelWait: helperCancelTimeout}
	a.core = core.New(a.cfg, &reporter{app: a})
	a.logDir = a.core.NodeDir()
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.emitMu.Lock()
	a.emitFn = func(name string, data ...interface{}) { wruntime.EventsEmit(ctx, name, data...) }
	a.emitMu.Unlock()
	a.log.start(ctx, a.emit)
	a.logHeader()
}

// emit sends an event to the page, once there is one.
func (a *App) emit(name string, data ...interface{}) {
	a.emitMu.Lock()
	f := a.emitFn
	a.emitMu.Unlock()
	if f != nil {
		f(name, data...)
	}
}

// progress is a line for the log and the page.
func (a *App) progress(msg string) {
	a.log.tool(msg)
	a.emit("progress", msg)
}

// logHeader starts the log of a session or of a backup.
func (a *App) logHeader() {
	a.log.tool(fmt.Sprintf("Breez Recovery %s on %s/%s", version, runtime.GOOS, runtime.GOARCH))
	a.log.tool("Work dir: " + a.c().Config().WorkDir)
}

func (a *App) beforeClose(ctx context.Context) bool {
	busy := !a.opMu.TryLock()
	if !busy {
		a.opMu.Unlock()
	}
	// Closing mid-way stops the node. Ask first while something runs, so
	// a stray click on the window controls does not end a long sync.
	if busy || a.runningHelper() != nil {
		answer, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
			Type:          wruntime.QuestionDialog,
			Title:         "Close Breez Recovery?",
			Message:       "The app is still working. Closing stops the node; you can reopen the app later and it continues where it left off.\n\nClose it anyway?",
			Buttons:       []string{"No", "Yes"},
			DefaultButton: "No",
			CancelButton:  "No",
		})
		// Linux maps a question dialog to its own Yes/No buttons whatever
		// labels are passed, so accept either spelling of a confirmation.
		if err != nil || (answer != "Yes" && answer != "Close" && answer != "Ok") {
			return true
		}
	}
	a.cancelCurrent()
	// The call that ran returns first: a broadcast is never cut short, and
	// a cancelled call ends within helperCancelTimeout.
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.stopHelper(true)
	// The log stays with the backup.
	a.logFor("")
	return false
}

// ---- reporter --------------------------------------------------------------

type reporter struct{ app *App }

// Progress: the core can report before the window exists (it tidies the
// work folder when it is created); the log keeps the line.
func (r *reporter) Progress(msg string) { r.app.progress(msg) }

func (r *reporter) NodeLog(line string) { r.app.log.node(line) }

func (r *reporter) SignIn(provider, url string) {
	r.app.emit("signin", map[string]string{"provider": provider, "url": url})
}

// ---- log buffer -------------------------------------------------------------

// logBuffer keeps the tool and node log lines for the log panel and the
// export, and pushes new lines to the frontend in batches.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	pending []string
	emit    func(name string, data ...interface{})
}

func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

func (l *logBuffer) start(ctx context.Context, emit func(name string, data ...interface{})) {
	l.mu.Lock()
	l.emit = emit
	l.mu.Unlock()
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.flush()
			}
		}
	}()
}

// flush sends the lines not sent yet. Under the lock, so a reset cannot
// come between taking the lines and sending them.
func (l *logBuffer) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 && l.emit != nil {
		l.emit("log", l.pending)
	}
	l.pending = nil
}

func (l *logBuffer) add(lines ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, lines...)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.pending = append(l.pending, lines...)
}

func (l *logBuffer) tool(msg string) { l.add(toolLine(msg)) }

// toolLine is how a line of the app's own reads in the log.
func toolLine(msg string) string {
	return time.Now().Format("15:04:05") + "  [recovery] " + msg
}

func (l *logBuffer) node(line string) {
	l.add(line)
}

// backupLogFile is the log of every session of a backup, in its folder.
const backupLogFile = "recovery.log"

// moveTo appends every line to the file at path, empties the log and tells
// the page to empty its panel. On a failed write nothing is emptied.
func (l *logBuffer) moveTo(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString("===== " + time.Now().Format(time.RFC3339) + "\n" + strings.Join(l.lines, "\n") + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	l.lines, l.pending = nil, nil
	if l.emit != nil {
		l.emit("logreset")
	}
	return nil
}

func (l *logBuffer) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n") + "\n"
}

// snapshot returns every line so far, for the frontend when it starts. The
// lines still waiting to go out as an event are part of it, so they are
// dropped from the queue; they showed twice otherwise.
func (l *logBuffer) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = nil
	return append([]string(nil), l.lines...)
}

// logFor makes the log the one of the backup folder dir, "" for none yet.
// The lines so far are about the backup in use before; when that was
// another one they go to its recovery.log and the log starts again. It
// reports whether it did. Only with opMu held.
func (a *App) logFor(dir string) bool {
	if a.logDir == dir {
		return false
	}
	old := a.logDir
	a.logDir = dir
	if old == "" {
		return false // the lines so far lead up to this backup
	}
	if err := a.log.moveTo(filepath.Join(old, backupLogFile)); err != nil {
		a.log.tool("the log could not be kept with its backup: " + err.Error())
		return false
	}
	a.logHeader()
	return true
}

// ---- operations -------------------------------------------------------------

var errBusy = errors.New("another operation is still running")

// run executes one long operation with a cancellable context.
func (a *App) run(name string, fn func(ctx context.Context) error) error {
	if !a.opMu.TryLock() {
		return errBusy
	}
	defer a.opMu.Unlock()
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancelMu.Lock()
	a.cancel = cancel
	a.cancelMu.Unlock()
	defer func() {
		a.cancelMu.Lock()
		a.cancel = nil
		a.cancelMu.Unlock()
		cancel()
	}()
	a.log.tool("--- " + name)
	err := fn(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			err = errors.New("cancelled")
		}
		a.log.tool("error: " + err.Error())
	}
	return err
}

func (a *App) cancelCurrent() {
	a.cancelMu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.cancelMu.Unlock()
}

// Cancel aborts the running operation, if any.
func (a *App) Cancel() {
	a.log.tool("cancel requested")
	a.cancelCurrent()
}

func (a *App) c() *core.Core {
	a.coreMu.Lock()
	defer a.coreMu.Unlock()
	return a.core
}

// State is what the frontend needs to render the first screen.
type State struct {
	Version          string `json:"version"`
	OS               string `json:"os"`
	WorkDir          string `json:"workDir"`
	NodeDir          string `json:"nodeDir"` // folder of the backup in use
	Peers            string `json:"peers"`
	HasNode          bool   `json:"hasNode"`
	LogPath          string `json:"logPath"`
	GoogleConfigured bool   `json:"googleConfigured"`
}

// GetState returns the current state.
func (a *App) GetState() State {
	c := a.c()
	cfg := c.Config()
	return State{
		Version:          version,
		OS:               runtime.GOOS,
		WorkDir:          cfg.WorkDir,
		NodeDir:          c.NodeDir(),
		Peers:            cfg.Peers,
		HasNode:          c.HasRestoredNode(),
		LogPath:          c.LogPath(),
		GoogleConfigured: cfg.GoogleClientID != "",
	}
}

// Settings are the advanced options: the work folder and the bitcoin peers.
type Settings struct {
	WorkDir string `json:"workDir"`
	Peers   string `json:"peers"`
}

// ApplySettings replaces the session configuration. A running node stops
// first: it runs with the settings it was started with. It refuses while an
// operation runs.
func (a *App) ApplySettings(s Settings) (State, error) {
	if !a.opMu.TryLock() {
		return State{}, errBusy
	}
	defer a.opMu.Unlock()
	if strings.TrimSpace(s.WorkDir) == "" {
		return State{}, errors.New("the work folder cannot be empty")
	}
	a.stopHelper(true)
	a.coreMu.Lock()
	a.cfg.WorkDir = strings.TrimSpace(s.WorkDir)
	a.cfg.Peers = strings.TrimSpace(s.Peers)
	a.core = core.New(a.cfg, &reporter{app: a})
	a.coreMu.Unlock()
	// Another work folder has another backup in use, or none.
	if !a.logFor(a.c().NodeDir()) {
		a.log.tool("Work dir: " + a.cfg.WorkDir)
	}
	if a.cfg.Peers != "" {
		a.log.tool("Bitcoin peers pinned: " + a.cfg.Peers)
	}
	return a.GetState(), nil
}

// ChooseWorkDir opens a folder picker and returns the chosen path, or ""
// when the user cancelled.
func (a *App) ChooseWorkDir() (string, error) {
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:                "Choose the folder that holds the restored app",
		DefaultDirectory:     filepath.Dir(a.c().Config().WorkDir),
		CanCreateDirectories: true,
	})
}

// ListGoogle signs in to Google if needed and lists the backups.
func (a *App) ListGoogle() ([]core.Snapshot, error) {
	var snaps []core.Snapshot
	err := a.run("list Google Drive backups", func(ctx context.Context) error {
		var err error
		snaps, err = a.c().GoogleSnapshots(ctx)
		return err
	})
	return snaps, err
}

// ListICloud signs in with the Apple ID if needed and lists the backups.
func (a *App) ListICloud() ([]core.Snapshot, error) {
	var snaps []core.Snapshot
	err := a.run("list iCloud backups", func(ctx context.Context) error {
		var err error
		snaps, err = a.c().ICloudSnapshots(ctx)
		return err
	})
	return snaps, err
}

// ForgetSignIns drops the cached Google and Apple sessions.
func (a *App) ForgetSignIns() {
	a.c().ForgetGoogle()
	a.c().ForgetICloud()
	a.log.tool("cached sign-ins removed")
}

// ZipInfo describes a chosen backup file.
type ZipInfo struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	NeedsPhrase bool   `json:"needsPhrase"`
}

// ChooseZip opens a file picker for a backup zip and inspects it. An empty
// path means the user cancelled.
func (a *App) ChooseZip() (ZipInfo, error) {
	path, err := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:   "Choose the Breez backup file",
		Filters: []wruntime.FileFilter{{DisplayName: "Backup zip (*.zip)", Pattern: "*.zip"}, {DisplayName: "All files", Pattern: "*"}},
	})
	if err != nil || path == "" {
		return ZipInfo{}, err
	}
	return a.InspectZip(path)
}

// InspectZip checks a backup file.
func (a *App) InspectZip(path string) (ZipInfo, error) {
	needs, err := core.ZipNeedsPhrase(path)
	if err != nil {
		return ZipInfo{}, err
	}
	a.log.tool(fmt.Sprintf("backup file %s, encrypted: %v", path, needs))
	return ZipInfo{Path: path, Name: filepath.Base(path), NeedsPhrase: needs}, nil
}

// CheckPhrase validates a backup phrase and returns "Mnemonics" (24 words)
// or "Mnemonics12" (12 words).
func (a *App) CheckPhrase(phrase string) (string, error) {
	return core.ValidateMnemonic(phrase)
}

// RestoreRequest says where the backup comes from.
type RestoreRequest struct {
	Source  string `json:"source"` // "google", "icloud", "zip"
	NodeID  string `json:"nodeId"`
	ZipPath string `json:"zipPath"`
	Phrase  string `json:"phrase"`
	Force   bool   `json:"force"`
}

// Restore downloads and places the backup in a folder of its own. A backup
// that is already restored on this computer is not downloaded again unless
// the user asks for it.
func (a *App) Restore(req RestoreRequest) error {
	return a.run("restore from "+req.Source, func(ctx context.Context) error {
		c := a.c()
		name, err := core.BackupName(req.Source, req.NodeID, req.ZipPath)
		if err != nil {
			return err
		}
		// Its folder may be the one restored again, and a restore is
		// refused while a node runs.
		a.stopHelper(false)
		if a.logDir != "" && filepath.Base(a.logDir) != name {
			a.logFor("")
		}
		if err := a.restore(ctx, c, name, req); err != nil {
			return err
		}
		a.logFor(c.NodeDir())
		return nil
	})
}

func (a *App) restore(ctx context.Context, c *core.Core, name string, req RestoreRequest) error {
	if c.IsRestored(name) && !req.Force {
		// Linux shows its own Yes/No buttons whatever labels are
		// passed, so the question is a yes/no one.
		answer, err := wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
			Type:          wruntime.QuestionDialog,
			Title:         "Already restored",
			Message:       "Already restored here. Continue with it?\n\nNo restores it again.",
			Buttons:       []string{"Yes", "No"},
			DefaultButton: "Yes",
		})
		if err != nil {
			return err
		}
		switch answer {
		case "Yes", "Ok":
			return c.UseBackup(name)
		case "No":
			req.Force = true
		default:
			return errors.New("restore cancelled")
		}
	}
	switch req.Source {
	case "google":
		return c.GoogleRestore(ctx, req.NodeID, req.Phrase, req.Force)
	case "icloud":
		return c.ICloudRestore(ctx, req.NodeID, req.Phrase, req.Force)
	case "zip":
		return c.ZipRestore(req.ZipPath, req.Phrase, req.Force)
	}
	return fmt.Errorf("unknown source %q", req.Source)
}

// RestoreOther prepares for restoring a different backup: a node running on
// the backup in use stops, and the log so far goes to that backup's folder.
// ask: money of this app is still on its way, which moves only while it
// runs; confirm first.
func (a *App) RestoreOther(ask bool) error {
	if ask {
		answer, err := wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
			Type:          wruntime.QuestionDialog,
			Title:         "Restore another backup?",
			Message:       "Funds of this app are still on their way and move only while it runs. Restore this backup again later to finish.\n\nRestore another backup now?",
			Buttons:       []string{"Yes", "No"},
			DefaultButton: "No",
			CancelButton:  "No",
		})
		if err != nil {
			return err
		}
		if answer != "Yes" && answer != "Ok" {
			return errors.New("restore another backup cancelled")
		}
	}
	// Wait out a status refresh or a history load instead of refusing.
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.stopHelper(true)
	a.logFor("")
	return nil
}

// RestoredApps lists the backups restored on this computer.
func (a *App) RestoredApps() []core.RestoredBackup { return a.c().RestoredBackups() }

// UseRestored continues with another backup restored on this computer. A
// node running on the backup in use stops first.
func (a *App) UseRestored(name string) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	c := a.c()
	if !c.IsRestored(name) {
		return fmt.Errorf("no restored backup %q", name)
	}
	if name != c.CurrentBackup() {
		a.stopHelper(true)
		a.logFor("")
		if err := c.UseBackup(name); err != nil {
			return err
		}
	}
	a.logFor(c.NodeDir())
	return nil
}

// StartAndSync starts the node, waits for chain sync (emitting "sync"
// events) and returns the wallet status. When the node has to start again
// for a change (see core.ErrRestartRequired) its helper exits and a new one
// carries on, up to maxNodeRestarts times in a row.
func (a *App) StartAndSync() (*core.Status, error) {
	var st *core.Status
	err := a.run("start node and sync", func(ctx context.Context) error {
		a.logFor(a.c().NodeDir())
		var err error
		st, err = a.syncOnHelper(ctx)
		// A failed start can leave the node half started, and its library
		// starts only once per process.
		a.syncFailed = err != nil && !errors.Is(err, context.Canceled)
		return err
	})
	return st, err
}

func (a *App) syncOnHelper(ctx context.Context) (*core.Status, error) {
	a.syncMu.Lock()
	a.syncing = true
	a.syncMu.Unlock()
	defer func() {
		a.syncMu.Lock()
		a.syncing = false
		a.syncMu.Unlock()
	}()
	for restarts := 0; ; restarts++ {
		p := a.runningHelper()
		if p != nil && (a.syncFailed || p.dir != a.c().NodeDir()) {
			a.syncStep("Stopping the node to start it again...")
			a.stopHelper(false)
			p = nil
		}
		if p == nil {
			var err error
			if p, err = a.startHelper(); err != nil {
				return nil, err
			}
		}
		m, err := p.call(ctx, helperRequest{Call: callStartAndSync}, a.cancelWait, a.log.tool)
		if !errors.Is(err, core.ErrRestartRequired) {
			return m.Status, err
		}
		// Core has stopped the node, and the helper exits by itself.
		p.await(a.stopWait, a.log.tool)
		if restarts == maxNodeRestarts {
			return nil, fmt.Errorf("the node asked to start again %d times in a row; Save log has the details", maxNodeRestarts+1)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a.log.tool("The node starts again.")
		a.syncStep("Starting the node again...")
	}
}

// syncStep shows msg on the sync screen, with the node at its start.
func (a *App) syncStep(msg string) {
	a.emit("sync", core.SyncProgress{Stage: "start", Percent: -1, Remaining: -1, Message: msg})
}

// ---- node helper ---------------------------------------------------------------

// runningHelper is the running helper, nil when none.
func (a *App) runningHelper() *nodeProc {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	return a.node
}

// startHelper starts a helper on the backup in use. Only with opMu held and
// no helper running.
func (a *App) startHelper() (*nodeProc, error) {
	c := a.c()
	if !c.HasRestoredNode() {
		return nil, fmt.Errorf("no restored backup in %s", c.Config().WorkDir)
	}
	dir := c.NodeDir()
	cmd, err := a.newHelper(filepath.Base(dir), c.Config())
	if err != nil {
		return nil, fmt.Errorf("start the node: %w", err)
	}
	// From here on a restore or a switch is refused, and the restored apps
	// list counts payments from before the node opened its breez.db.
	core.SetInUse(dir, core.PaymentCount(dir))
	a.syncMu.Lock()
	a.lastSync = nil
	a.syncMu.Unlock()
	// Held until a.node is set: a helper that dies at once is cleared by
	// nodeExit only after that.
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	p, err := startNodeProc(cmd, dir, nodeHandlers{event: a.nodeEvent, text: a.log.node, exit: a.nodeExit})
	if err != nil {
		core.SetInUse("", 0)
		return nil, fmt.Errorf("start the node: %w", err)
	}
	a.node = p
	a.syncFailed = false
	return p, nil
}

// stopHelper stops the helper, if one runs, and returns once it has exited.
// page: the page shows that the node is stopping meanwhile.
func (a *App) stopHelper(page bool) {
	p := a.runningHelper()
	if p == nil {
		return
	}
	if page {
		a.emit("stopping")
	}
	a.progress("Stopping the node...")
	p.stop(a.stopWait, a.log.tool)
}

// nodeCall sends a call to the running helper.
func (a *App) nodeCall(ctx context.Context, req helperRequest) (helperMessage, error) {
	p := a.runningHelper()
	if p == nil {
		return helperMessage{}, errNodeNotRunning
	}
	return p.call(ctx, req, a.cancelWait, a.log.tool)
}

// nodeEvent passes on what the helper reports.
func (a *App) nodeEvent(m helperMessage) {
	switch m.Event {
	case eventLog:
		a.log.add(m.Lines...)
	case eventProgress:
		// Already in the log.
		a.emit("progress", m.Text)
		// The sync screen shows the sync reports only. Between them, while
		// the node starts, waits or stops, its lines are what moves.
		a.syncMu.Lock()
		p := core.SyncProgress{Stage: "start", Remaining: -1}
		if a.lastSync != nil {
			p = *a.lastSync
		}
		syncing := a.syncing
		a.syncMu.Unlock()
		if syncing {
			p.Percent = -1
			p.Message = strings.TrimSpace(m.Text)
			a.emit("sync", p)
		}
	case eventSync:
		if m.Sync == nil {
			return
		}
		a.syncMu.Lock()
		p := *m.Sync
		a.lastSync = &p
		a.syncMu.Unlock()
		a.emit("sync", *m.Sync)
	}
}

// nodeExit runs once a helper's Wait returned.
func (a *App) nodeExit(p *nodeProc, err error, planned, waiting bool) {
	core.SetInUse("", 0)
	a.nodeMu.Lock()
	if a.node == p {
		a.node = nil
	}
	a.nodeMu.Unlock()
	how := ""
	if err != nil {
		how = " (" + err.Error() + ")"
	}
	if planned {
		a.log.tool("The node has stopped" + how + ".")
		return
	}
	a.log.tool("The node stopped unexpectedly" + how + ".")
	// A call waiting for the node fails with this; otherwise the page
	// learns it here. It does not start again by itself: a node that
	// crashes on start would crash again and again.
	if !waiting {
		a.emit("nodestopped", errNodeCrashed.Error())
	}
}

// withEnv returns env with the key=value pairs of set replacing any
// earlier values of those keys.
func withEnv(env []string, set ...string) []string {
	keys := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		keys[k] = true
	}
	var out []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// GetStatus refreshes the wallet status of the running node.
func (a *App) GetStatus() (*core.Status, error) {
	var st *core.Status
	err := a.run("refresh status", func(ctx context.Context) error {
		m, err := a.nodeCall(ctx, helperRequest{Call: callStatus})
		st = m.Status
		return err
	})
	return st, err
}

// GetHistory lists the app's money movements, newest first, with totals.
func (a *App) GetHistory() (*core.History, error) {
	var h *core.History
	err := a.run("history", func(ctx context.Context) error {
		m, err := a.nodeCall(ctx, helperRequest{Call: callHistory})
		h = m.History
		return err
	})
	if err == nil {
		a.historyMu.Lock()
		a.lastHistory = h
		a.historyMu.Unlock()
	}
	return h, err
}

// SaveHistory asks where to save the history shown on screen and writes it
// as CSV. Returns the path, or "" when the user cancelled.
func (a *App) SaveHistory() (string, error) {
	a.historyMu.Lock()
	h := a.lastHistory
	a.historyMu.Unlock()
	if h == nil {
		return "", errors.New("open the history first")
	}
	path, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{
		Title:           "Export the history",
		DefaultFilename: "breez-history-" + time.Now().Format("2006-01-02") + ".csv",
		Filters:         []wruntime.FileFilter{{DisplayName: "Spreadsheet (*.csv)", Pattern: "*.csv"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	if err := core.WriteHistoryCSV(f, h); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	a.log.tool("history exported to " + path)
	return path, nil
}

// ValidateAddress checks a bitcoin address.
func (a *App) ValidateAddress(address string) error {
	return a.c().ValidateAddress(strings.TrimSpace(address))
}

// PrepareSweep builds the sweep transactions without broadcasting.
func (a *App) PrepareSweep(address string) (*core.SweepPlan, error) {
	var plan *core.SweepPlan
	err := a.run("prepare sweep", func(ctx context.Context) error {
		m, err := a.nodeCall(ctx, helperRequest{Call: callPrepareSweep, Address: strings.TrimSpace(address)})
		plan = m.Plan
		return err
	})
	return plan, err
}

// BroadcastSweep publishes the prepared sweep at the chosen fee target.
// The helper refreshes the status after it, for the restored apps list.
func (a *App) BroadcastSweep(confTarget int) (string, error) {
	var txid string
	err := a.run("broadcast sweep", func(ctx context.Context) error {
		m, err := a.nodeCall(ctx, helperRequest{Call: callBroadcastSweep, ConfTarget: confTarget})
		txid = m.TxID
		return err
	})
	return txid, err
}

// ---- log ------------------------------------------------------------------------

// GetLog returns every log line kept in memory.
func (a *App) GetLog() []string { return a.log.snapshot() }

// SaveLog asks where to save the log and writes it. Returns the path, or
// "" when the user cancelled.
func (a *App) SaveLog() (string, error) {
	name := "breez-recovery-" + time.Now().Format("2006-01-02-1504") + ".log"
	path, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{
		Title:           "Save the recovery log",
		DefaultFilename: name,
		Filters:         []wruntime.FileFilter{{DisplayName: "Log (*.log)", Pattern: "*.log"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	header := fmt.Sprintf("Breez Recovery %s, %s/%s, saved %s\nwork dir: %s\n\n", version, runtime.GOOS, runtime.GOARCH, time.Now().Format(time.RFC3339), a.c().Config().WorkDir)
	body := header + a.log.text()
	if lnd, err := os.ReadFile(a.c().LogPath()); err == nil {
		body += "\n===== lnd.log (last 2000 lines) =====\n" + tail(string(lnd), 2000)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return "", err
	}
	a.log.tool("log saved to " + path)
	return path, nil
}

// CopyLog puts the log on the clipboard.
func (a *App) CopyLog() error {
	return wruntime.ClipboardSetText(a.ctx, a.log.text())
}

// CopyText puts arbitrary text on the clipboard.
func (a *App) CopyText(text string) error {
	return wruntime.ClipboardSetText(a.ctx, text)
}

// OpenURL opens a link in the system browser.
func (a *App) OpenURL(url string) { wruntime.BrowserOpenURL(a.ctx, url) }

// OpenWorkDir reveals the work folder in the file manager.
func (a *App) OpenWorkDir() {
	dir := a.c().Config().WorkDir
	os.MkdirAll(dir, 0700)
	// Not through Wails' BrowserOpenURL: since 2.16 it refuses file URLs.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		a.log.tool("open " + dir + ": " + err.Error())
	}
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
