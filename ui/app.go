package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
			Message:       "The app is still working. Closing stops the node; you can reopen the app later and it continues where it left off.",
			Buttons:       []string{"Keep running", "Close"},
			DefaultButton: "Keep running",
			CancelButton:  "Keep running",
		})
		if err != nil || answer != "Close" {
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
	wruntime.EventsEmit(r.app.ctx, "progress", msg)
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

func (l *logBuffer) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
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
		Peers:            cfg.Peers,
		HasNode:          c.HasRestoredNode(),
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

// Restore downloads and places the backup in the work dir.
func (a *App) Restore(req RestoreRequest) error {
	return a.run("restore from "+req.Source, func(ctx context.Context) error {
		c := a.c()
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

// StartAndSync starts the node, waits for chain sync (emitting "sync"
// events) and returns the wallet status.
func (a *App) StartAndSync() (*core.Status, error) {
	var st *core.Status
	err := a.run("start node and sync", func(ctx context.Context) error {
		c := a.c()
		if err := c.StartNode(ctx); err != nil {
			return err
		}
		err := c.WaitSynced(ctx, func(p core.SyncProgress) {
			a.log.tool(p.Message)
			wruntime.EventsEmit(a.ctx, "sync", p)
		})
		if err != nil {
			return err
		}
		wruntime.EventsEmit(a.ctx, "progress", "Connecting to channel peers...")
		c.WaitChannelsActive(ctx, 30*time.Second)
		st, err = c.Status(ctx)
		return err
	})
	return st, err
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

// ValidateAddress checks a bitcoin address.
func (a *App) ValidateAddress(address string) error {
	return core.ValidateAddress(strings.TrimSpace(address))
}

// CloseChannels closes every channel with the funds going to address.
func (a *App) CloseChannels(address string, force bool) (*core.CloseResult, error) {
	var res *core.CloseResult
	err := a.run("close channels", func(ctx context.Context) error {
		c := a.c()
		wruntime.EventsEmit(a.ctx, "progress", "Giving channel peers a moment to connect...")
		c.WaitChannelsActive(ctx, 60*time.Second)
		var err error
		res, err = c.CloseChannels(ctx, strings.TrimSpace(address), force)
		return err
	})
	return res, err
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
