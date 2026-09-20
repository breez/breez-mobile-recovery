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
type App struct {
	ctx context.Context

	opMu     sync.Mutex // one long operation at a time
	cancelMu sync.Mutex
	cancel   context.CancelFunc

	coreMu sync.Mutex
	cfg    core.Config
	core   *core.Core

	log *logBuffer

	historyMu   sync.Mutex
	lastHistory *core.History // what the History screen shows, for export
}

func newApp() *App {
	a := &App{cfg: core.DefaultConfig(), log: newLogBuffer(20000)}
	a.core = core.New(a.cfg, &reporter{app: a})
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.log.start(ctx)
	a.log.tool(fmt.Sprintf("Breez Recovery %s on %s/%s", version, runtime.GOOS, runtime.GOARCH))
	a.log.tool("Work dir: " + a.cfg.WorkDir)
}

func (a *App) beforeClose(ctx context.Context) bool {
	a.coreMu.Lock()
	c := a.core
	a.coreMu.Unlock()
	// Closing mid-way stops the node. Ask first while something runs, so
	// a stray click on the window controls does not end a long sync.
	if !a.opMu.TryLock() || c.NodeRunning() {
		if a.opMu.TryLock() {
			a.opMu.Unlock()
		}
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
	} else {
		a.opMu.Unlock()
	}
	a.cancelCurrent()
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	return false
}

// ---- reporter --------------------------------------------------------------

type reporter struct{ app *App }

func (r *reporter) Progress(msg string) {
	r.app.log.tool(msg)
	// The core can report before the window exists (it tidies the work
	// folder when it is created); the log keeps the line.
	if r.app.ctx != nil {
		wruntime.EventsEmit(r.app.ctx, "progress", msg)
	}
}

func (r *reporter) NodeLog(line string) { r.app.log.node(line) }

func (r *reporter) SignIn(provider, url string) {
	wruntime.EventsEmit(r.app.ctx, "signin", map[string]string{"provider": provider, "url": url})
}

// ---- log buffer -------------------------------------------------------------

// logBuffer keeps the tool and node log lines for the log panel and the
// export, and pushes new lines to the frontend in batches.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	pending []string
	ctx     context.Context
}

func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

func (l *logBuffer) start(ctx context.Context) {
	l.mu.Lock()
	l.ctx = ctx
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

func (l *logBuffer) flush() {
	l.mu.Lock()
	batch := l.pending
	l.pending = nil
	ctx := l.ctx
	l.mu.Unlock()
	if len(batch) > 0 && ctx != nil {
		wruntime.EventsEmit(ctx, "log", batch)
	}
}

func (l *logBuffer) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.pending = append(l.pending, line)
}

func (l *logBuffer) tool(msg string) {
	l.add(time.Now().Format("15:04:05") + "  [recovery] " + msg)
}

func (l *logBuffer) node(line string) {
	l.add(line)
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
	RestoreOther     bool   `json:"restoreOther"` // set after a self-restart: go straight to choosing a backup
	AutoContinue     bool   `json:"autoContinue"` // set after a self-restart: go straight to sync
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
		RestoreOther:     os.Getenv(relaunchEnv) == "restore-other",
		AutoContinue:     os.Getenv(relaunchEnv) == "continue" && c.HasRestoredNode(),
		LogPath:          c.LogPath(),
		GoogleConfigured: cfg.GoogleClientID != "",
	}
}

// Settings are the advanced options a user can change before restoring.
type Settings struct {
	WorkDir string `json:"workDir"`
	Peers   string `json:"peers"`
}

// ApplySettings replaces the session configuration. It refuses while an
// operation runs or a node is started.
func (a *App) ApplySettings(s Settings) (State, error) {
	if !a.opMu.TryLock() {
		return State{}, errBusy
	}
	defer a.opMu.Unlock()
	if strings.TrimSpace(s.WorkDir) == "" {
		return State{}, errors.New("the work folder cannot be empty")
	}
	a.coreMu.Lock()
	a.core.Stop()
	a.cfg.WorkDir = strings.TrimSpace(s.WorkDir)
	a.cfg.Peers = strings.TrimSpace(s.Peers)
	a.core = core.New(a.cfg, &reporter{app: a})
	a.coreMu.Unlock()
	a.log.tool("Work dir: " + a.cfg.WorkDir)
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

// InspectZip checks a backup file (also used for dropped files).
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
	})
}

// RestoreOther prepares for restoring a different backup. The breez
// library stays bound to the first backup folder it ran on, so when it
// already runs in this process the app starts again and opens on the
// backup sources. It reports whether it is restarting.
func (a *App) RestoreOther() (bool, error) {
	c := a.c()
	if !c.LibraryBound() {
		return false, nil
	}
	if !a.opMu.TryLock() {
		return false, errBusy
	}
	defer a.opMu.Unlock()
	a.log.tool("stopping the node to restore a different backup")
	if !c.StopWithin(20 * time.Second) {
		a.log.tool("the node did not stop cleanly; the program exits and starts again")
	}
	a.relaunch("restore-other")
	return true, nil
}

// StartAndSync starts the node, waits for chain sync (emitting "sync"
// events) and returns the wallet status.
func (a *App) StartAndSync() (*core.Status, error) {
	var st *core.Status
	err := a.run("start node and sync", func(ctx context.Context) error {
		c := a.c()
		if err := c.StartNode(ctx); err != nil {
			if errors.Is(err, core.ErrRestartRequired) {
				a.relaunch("continue")
				return err
			}
			return err
		}
		err := c.WaitSynced(ctx, func(p core.SyncProgress) {
			a.log.tool(p.Message)
			wruntime.EventsEmit(a.ctx, "sync", p)
		})
		if err != nil {
			return err
		}
		// Before anything is shown as spendable, make sure the chain agrees
		// that the channels are open. A backup taken before a channel
		// closed still lists it.
		if _, err := c.CheckChannelsOnChain(ctx, func(p core.SyncProgress) {
			wruntime.EventsEmit(a.ctx, "sync", p)
		}); err != nil {
			return err
		}
		st, err = c.Status(ctx)
		return err
	})
	return st, err
}

// relaunchEnv tells a copy of the program started by relaunch what to do
// first: "continue" the sync, or open on the backup sources
// ("restore-other").
const relaunchEnv = "BREEZ_RECOVERY_RELAUNCH"

// relaunch starts a fresh copy of this program, then quits this one. Used
// when the node or the library needs a program restart.
func (a *App) relaunch(then string) {
	exe, err := os.Executable()
	if err != nil {
		a.log.tool("relaunch: " + err.Error())
		return
	}
	cmd := exec.Command(exe)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, relaunchEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, relaunchEnv+"="+then)
	if err := cmd.Start(); err != nil {
		a.log.tool("relaunch: " + err.Error())
		return
	}
	a.log.tool("restarting the app")
	// A plain exit rather than a window close: the library may still be
	// winding down and the new copy needs the work folder released.
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

// GetStatus refreshes the wallet status of the running node.
func (a *App) GetStatus() (*core.Status, error) {
	var st *core.Status
	err := a.run("refresh status", func(ctx context.Context) error {
		var err error
		st, err = a.c().Status(ctx)
		return err
	})
	return st, err
}

// GetHistory lists the app's money movements, newest first, with totals.
func (a *App) GetHistory() (*core.History, error) {
	var h *core.History
	err := a.run("history", func(ctx context.Context) error {
		var err error
		h, err = a.c().History(ctx)
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
	return core.ValidateAddress(strings.TrimSpace(address))
}

// PrepareSweep builds the sweep transactions without broadcasting.
func (a *App) PrepareSweep(address string) (*core.SweepPlan, error) {
	var plan *core.SweepPlan
	err := a.run("prepare sweep", func(ctx context.Context) error {
		var err error
		plan, err = a.c().PrepareSweep(ctx, strings.TrimSpace(address))
		return err
	})
	return plan, err
}

// BroadcastSweep publishes the prepared sweep at the chosen fee target.
func (a *App) BroadcastSweep(confTarget int) (string, error) {
	var txid string
	err := a.run("broadcast sweep", func(ctx context.Context) error {
		var err error
		txid, err = a.c().BroadcastSweep(confTarget)
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
	wruntime.BrowserOpenURL(a.ctx, "file://"+filepath.ToSlash(dir))
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
