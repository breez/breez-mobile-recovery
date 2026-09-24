package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// The node helper. The window starts a copy of this program with helperEnv
// set to the name of a restored backup, and that copy runs the node: the
// breez library and lnd. The library's app object starts and stops only
// once per process, and a process stays bound to the first folder the
// library ran on, so a new start of the node, and another backup, need a
// new process; with the node in a helper the window stays. The helper
// serves the calls that need the node, plus cancel and stop, over its stdin
// and stdout (helperproto.go). It never touches Wails: the Wails runtime
// functions end a program that has no window.

// helperEnv selects helper mode; its value is the backup's folder name.
const helperEnv = "BREEZ_RECOVERY_NODE_HELPER"

const (
	// helperCallGrace is how long a stop waits for a cancelled call to
	// return before it stops the node under it. A broadcast is waited for
	// until the hard exit.
	helperCallGrace = 3 * time.Second
	// helperStopWait bounds the node's stop on a stop request. The window
	// kills a helper that has not exited 25 s after asking.
	helperStopWait = 20 * time.Second
	// helperGoneWait bounds the node's stop once the window is gone.
	helperGoneWait = 15 * time.Second
	// helperHardExit ends the process this long after a stop began,
	// whatever still runs.
	helperHardExit = 25 * time.Second
	// helperLogEvery is how often the log lines go to the window.
	helperLogEvery = 250 * time.Millisecond
)

var (
	errHelperStopping = errors.New("the node is stopping")
	errWindowGone     = errors.New("the window is gone")
)

// nodeCore is the part of core.Core the helper runs.
type nodeCore interface {
	StartNode(ctx context.Context) error
	WaitSynced(ctx context.Context, onProgress func(core.SyncProgress)) error
	CheckChannelsOnChain(ctx context.Context, onProgress func(core.SyncProgress)) ([]core.SpentChannel, error)
	Status(ctx context.Context) (*core.Status, error)
	History(ctx context.Context) (*core.History, error)
	PrepareSweep(ctx context.Context, address string) (*core.SweepPlan, error)
	BroadcastSweep(confTarget int) (string, error)
	StopWithin(d time.Duration) bool
}

// syncNode starts the node, waits for the chain sync and the channel check,
// and returns the funds. logf gets the sync lines worth a log line, onSync
// every sync report.
func syncNode(ctx context.Context, c nodeCore, logf func(string), onSync func(core.SyncProgress)) (*core.Status, error) {
	if err := c.StartNode(ctx); err != nil {
		return nil, err
	}
	err := c.WaitSynced(ctx, func(p core.SyncProgress) {
		// The search for later funds reports twice a second with the
		// same words; its log lines come from core.
		if p.Stage != "addresses" {
			logf(p.Message)
		}
		onSync(p)
	})
	if err != nil {
		return nil, err
	}
	// Before anything is shown as spendable, make sure the chain agrees
	// that the channels are open. A backup taken before a channel closed
	// still lists it.
	if _, err := c.CheckChannelsOnChain(ctx, onSync); err != nil {
		return nil, err
	}
	return c.Status(ctx)
}

// runHelper is the program in helper mode. The helper's exit ends it.
func runHelper() {
	// os.Stdout is read before core.New points it at the library's log
	// capture.
	helperMain(os.Stdin, os.Stdout, func(rep core.Reporter) (nodeCore, func() error) {
		cfg := core.DefaultConfig()
		cfg.SkipLock = true
		c := core.New(cfg, rep)
		name := os.Getenv(helperEnv)
		return c, func() error { return c.LockBackup(name) }
	})
}

// helperMain serves the window on in and out until the helper stops. open
// makes the session and the setup to run before the first call.
func helperMain(in io.Reader, out io.Writer, open func(core.Reporter) (nodeCore, func() error)) {
	// Without this a write to the window after it is gone would kill the
	// process on macOS and Linux before the node is stopped. Now the write
	// fails, and the failure stops the node.
	signal.Ignore(syscall.SIGPIPE)
	h := newHelper(out, os.Exit)
	h.core, h.setup = open(h)
	h.addLog(toolLine(fmt.Sprintf("The node runs in a helper process (pid %d).", os.Getpid())))
	h.serve(in)
}

type helper struct {
	core nodeCore
	// setup runs before a call until it has succeeded once (ready); only
	// the running call uses them.
	setup func() error
	ready bool

	out    io.Writer
	sendMu sync.Mutex // one write at a time, the log lines first
	gone   bool       // a write failed

	logMu   sync.Mutex
	pending []string

	mu       sync.Mutex
	running  *helperCall // nil while idle
	stopping bool
	// restarted: a call ended with a planned restart, for which core
	// stopped the node itself.
	restarted bool
	calls     sync.WaitGroup // calls not yet answered

	stopOnce sync.Once
	exit     func(code int)

	callGrace, stopWait, goneWait, hardExit time.Duration
}

type helperCall struct {
	name   string
	cancel context.CancelFunc
}

func newHelper(out io.Writer, exit func(int)) *helper {
	return &helper{
		out: out, exit: exit,
		callGrace: helperCallGrace, stopWait: helperStopWait,
		goneWait: helperGoneWait, hardExit: helperHardExit,
	}
}

// ---- core.Reporter -----------------------------------------------------------

func (h *helper) Progress(msg string) {
	h.addLog(toolLine(msg))
	h.send(&helperMessage{Event: eventProgress, Text: msg})
}

func (h *helper) NodeLog(line string) { h.addLog(line) }

// SignIn does not happen: the node signs in to nothing.
func (h *helper) SignIn(provider, url string) {
	h.addLog(toolLine("the node helper was asked to sign in to " + provider))
}

// ---- output ------------------------------------------------------------------

func (h *helper) addLog(line string) {
	h.logMu.Lock()
	h.pending = append(h.pending, line)
	h.logMu.Unlock()
}

// send writes the log lines so far, then m; nil sends only the log. A
// failed write means the window is gone: nothing more is written, and the
// helper stops.
func (h *helper) send(m *helperMessage) error {
	var frame []byte
	if m != nil {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		frame = append(b, '\n')
	}
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if h.gone {
		return errWindowGone
	}
	h.logMu.Lock()
	lines := h.pending
	h.pending = nil
	h.logMu.Unlock()
	var buf []byte
	if len(lines) > 0 {
		b, _ := json.Marshal(helperMessage{Event: eventLog, Lines: lines})
		buf = append(b, '\n')
	}
	buf = append(buf, frame...)
	if len(buf) == 0 {
		return nil
	}
	if _, err := h.out.Write(buf); err != nil {
		h.gone = true
		go h.stop(h.goneWait)
		return errWindowGone
	}
	return nil
}

// reply answers call id with m (nil: nothing but the outcome) and err.
func (h *helper) reply(id int64, m *helperMessage, err error) {
	if m == nil {
		m = &helperMessage{}
	}
	m.ID = id
	switch {
	case errors.Is(err, core.ErrRestartRequired):
		m.Restart = true
	case err != nil:
		m.Error = err.Error()
		m.Canceled = errors.Is(err, context.Canceled)
	}
	if err := h.send(m); err != nil && !errors.Is(err, errWindowGone) {
		h.send(&helperMessage{ID: id, Error: "the node's answer could not be sent: " + err.Error()})
	}
}

// ---- calls ---------------------------------------------------------------------

// serve reads the window's requests until stdin ends, then stops.
func (h *helper) serve(in io.Reader) {
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		t := time.NewTicker(helperLogEvery)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				h.send(nil)
			}
		}
	}()
	readFrames(in, func(line []byte) bool {
		req, ok := decodeRequest(line)
		if ok {
			h.dispatch(req)
		}
		return ok
	}, func(text string) {
		h.addLog(toolLine("node helper: not a request: " + text))
	})
	// The window closed stdin, or is gone.
	h.stop(h.goneWait)
}

func (h *helper) dispatch(req helperRequest) {
	switch req.Call {
	case callCancel:
		h.mu.Lock()
		if h.running != nil {
			h.running.cancel()
		}
		h.mu.Unlock()
	case callStop:
		h.stop(h.stopWait)
	case callPing:
		h.reply(req.ID, &helperMessage{Text: version}, nil)
	case callStartAndSync, callStatus, callHistory, callPrepareSweep, callBroadcastSweep:
		h.start(req)
	default:
		h.reply(req.ID, nil, fmt.Errorf("unknown call %q", req.Call))
	}
}

// start runs a call in the background, one at a time: the reader keeps
// reading for a cancel or a stop.
func (h *helper) start(req helperRequest) {
	h.mu.Lock()
	var refused error
	switch {
	case h.stopping:
		refused = errHelperStopping
	case h.running != nil:
		refused = errBusy
	}
	if refused != nil {
		h.mu.Unlock()
		h.reply(req.ID, nil, refused)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.running = &helperCall{name: req.Call, cancel: cancel}
	h.calls.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.calls.Done()
		m, err := h.run(ctx, req)
		cancel()
		restart := errors.Is(err, core.ErrRestartRequired)
		h.mu.Lock()
		h.running = nil // before the reply: the window may send the next call at once
		h.stopping = h.stopping || restart
		h.restarted = h.restarted || restart
		h.mu.Unlock()
		h.reply(req.ID, m, err)
		if restart {
			// Core has stopped the node, and its library does not start
			// again in this process.
			h.send(nil)
			h.exit(0)
		}
	}()
}

// run runs one call and returns its reply without the id.
func (h *helper) run(ctx context.Context, req helperRequest) (*helperMessage, error) {
	if !h.ready {
		if err := h.setup(); err != nil {
			return nil, err
		}
		h.ready = true
	}
	c := h.core
	switch req.Call {
	case callStartAndSync:
		st, err := syncNode(ctx, c,
			func(msg string) { h.addLog(toolLine(msg)) },
			func(p core.SyncProgress) { h.send(&helperMessage{Event: eventSync, Sync: &p}) })
		return &helperMessage{Status: st}, err
	case callStatus:
		st, err := c.Status(ctx)
		return &helperMessage{Status: st}, err
	case callHistory:
		hist, err := c.History(ctx)
		return &helperMessage{History: hist}, err
	case callPrepareSweep:
		plan, err := c.PrepareSweep(ctx, strings.TrimSpace(req.Address))
		return &helperMessage{Plan: plan}, err
	case callBroadcastSweep:
		txid, err := c.BroadcastSweep(req.ConfTarget)
		if err == nil {
			// The restored apps list shows what a backup still holds; the
			// sent funds have left it. Only the list depends on this.
			_, _ = c.Status(ctx)
		}
		return &helperMessage{TxID: txid}, err
	}
	return nil, fmt.Errorf("unknown call %q", req.Call)
}

// stop ends the helper: it cancels the call that runs, gives it a moment to
// return (a broadcast is waited for), stops the node within wait and exits.
// A timer ends the process should any of that not return.
func (h *helper) stop(wait time.Duration) {
	h.stopOnce.Do(func() {
		hard := time.AfterFunc(h.hardExit, func() { h.exit(2) })
		defer hard.Stop()
		h.mu.Lock()
		h.stopping = true
		grace := h.callGrace
		if h.running != nil {
			if h.running.name == callBroadcastSweep {
				grace = h.hardExit
			}
			h.running.cancel()
		}
		h.mu.Unlock()
		answered := make(chan struct{})
		go func() {
			h.calls.Wait()
			close(answered)
		}()
		select {
		case <-answered:
		case <-time.After(grace):
		}
		h.mu.Lock()
		restarted := h.restarted
		h.mu.Unlock()
		// After a planned restart core has stopped the node; a second Stop
		// could overlap the library's first one.
		if !restarted {
			h.core.StopWithin(wait)
		}
		h.send(nil)
		h.exit(0)
	})
}
