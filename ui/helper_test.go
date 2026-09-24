package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// fakeHelperEnv makes the test binary a node helper over a fake node, the
// usual way to test a program that starts itself: TestMain checks it.
const fakeHelperEnv = "BREEZ_RECOVERY_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeHelperEnv); mode != "" {
		helperMain(os.Stdin, os.Stdout, func(rep core.Reporter) (nodeCore, func() error) {
			return newFakeNode(rep, mode), func() error { return nil }
		})
		os.Exit(1) // the helper exits itself
	}
	os.Exit(m.Run())
}

// newFakeNode is the node of a fake helper process in mode: "sync" works,
// "restart" asks for a restart on every start, "crash" panics while it
// syncs, "crashidle" panics just after a sync, "hang" never returns from
// its stop, "stuck" ignores a cancel while it syncs, "startcancel" waits in
// its first start until cancelled and cannot start again in the process,
// "waitcancel" waits in its first sync until cancelled, "failstart" fails
// its start and takes 1.5 s to stop, "slowstop" takes 500 ms to stop.
func newFakeNode(rep core.Reporter, mode string) *fakeNode {
	f := &fakeNode{rep: rep, restart: mode == "restart", name: os.Getenv(helperEnv)}
	// Its calls run one at a time.
	calls := 0
	untilCancelledOnce := func(ctx context.Context) error {
		if calls++; calls > 1 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	switch mode {
	case "crash":
		f.wait = func(context.Context) error { panic("fake crash") }
	case "crashidle":
		f.afterStatus = func() { time.AfterFunc(300*time.Millisecond, func() { panic("fake crash while idle") }) }
	case "hang":
		f.stopHook = func() { select {} }
	case "stuck":
		f.wait = func(context.Context) error { select {} }
	case "startcancel":
		// As if the library's app had started and lnd's RPC was awaited:
		// the app starts only once per process (breez app.go Start).
		f.start = func(ctx context.Context) error {
			if calls > 0 {
				return errors.New("start node: Breez already started")
			}
			return untilCancelledOnce(ctx)
		}
	case "waitcancel":
		f.wait = untilCancelledOnce
	case "failstart":
		f.start = func(context.Context) error { return errors.New("start failed") }
		f.stopHook = func() { time.Sleep(1500 * time.Millisecond) }
	case "slowstop":
		f.stopHook = func() { time.Sleep(500 * time.Millisecond) }
	}
	return f
}

// fakeNode stands in for core.Core. Its hooks, when set, run inside the
// calls; it notes what happened, in order.
type fakeNode struct {
	rep     core.Reporter
	restart bool   // StartNode asks for a restart
	name    string // the backup it runs, logged on start

	start       func(ctx context.Context) error // inside StartNode
	wait        func(ctx context.Context) error // inside WaitSynced
	broadcast   func()                          // inside BroadcastSweep
	stopHook    func()                          // inside StopWithin
	afterStatus func()                          // at the end of Status
	history     *core.History

	mu    sync.Mutex
	notes []string
}

func (f *fakeNode) note(s string) {
	f.mu.Lock()
	f.notes = append(f.notes, s)
	f.mu.Unlock()
}

func (f *fakeNode) noted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

func (f *fakeNode) StartNode(ctx context.Context) error {
	f.rep.Progress("Starting the node...")
	if f.name != "" {
		f.rep.NodeLog("fake node on " + f.name)
	}
	if f.start != nil {
		if err := f.start(ctx); err != nil {
			return err
		}
	}
	f.rep.NodeLog("lnd: node up")
	if f.restart {
		f.rep.NodeLog("lnd: stopped for the restart")
		return fmt.Errorf("caught up: %w", core.ErrRestartRequired)
	}
	return nil
}

func (f *fakeNode) WaitSynced(ctx context.Context, report func(core.SyncProgress)) error {
	report(core.SyncProgress{Stage: "headers", Message: "Catching up", Percent: 50})
	report(core.SyncProgress{Stage: "addresses", Message: "Looking for funds"})
	if f.wait != nil {
		err := f.wait(ctx)
		f.note("wait returned")
		return err
	}
	return nil
}

func (f *fakeNode) CheckChannelsOnChain(ctx context.Context, report func(core.SyncProgress)) ([]core.SpentChannel, error) {
	report(core.SyncProgress{Stage: "channels", Message: "Checking channels"})
	return nil, nil
}

func (f *fakeNode) Status(ctx context.Context) (*core.Status, error) {
	f.note("status")
	if f.afterStatus != nil {
		f.afterStatus()
	}
	return &core.Status{NodeID: "fake", OnchainConfirmed: 1000}, nil
}

func (f *fakeNode) History(ctx context.Context) (*core.History, error) {
	return f.history, nil
}

func (f *fakeNode) PrepareSweep(ctx context.Context, address string) (*core.SweepPlan, error) {
	return &core.SweepPlan{Address: address, Amount: 1000}, nil
}

func (f *fakeNode) BroadcastSweep(confTarget int) (string, error) {
	if f.broadcast != nil {
		f.broadcast()
	}
	f.note("broadcast")
	return fmt.Sprintf("tx%d", confTarget), nil
}

func (f *fakeNode) StopWithin(d time.Duration) bool {
	f.note(fmt.Sprintf("stop %v", d))
	f.rep.NodeLog("LTND: Shutdown complete")
	if f.stopHook != nil {
		f.stopHook()
	}
	return true
}

// rig runs a helper over pipes: stdin is written by the test, stdout read
// into msgs, lines that are no frame into stray.
type rig struct {
	t     *testing.T
	h     *helper
	fake  *fakeNode
	in    *io.PipeWriter
	out   *io.PipeReader
	msgs  chan helperMessage
	stray chan string
	exits chan int
	done  chan struct{}
	id    int64
}

func newRig(t *testing.T, fake *fakeNode, setup func() error) *rig {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &rig{t: t, fake: fake, in: inW, out: outR,
		msgs: make(chan helperMessage, 10000), stray: make(chan string, 100),
		exits: make(chan int, 10), done: make(chan struct{})}
	r.h = newHelper(outW, func(code int) { r.exits <- code })
	r.h.callGrace, r.h.stopWait, r.h.goneWait, r.h.hardExit = 200*time.Millisecond, 20*time.Millisecond, 15*time.Millisecond, 3*time.Second
	if setup == nil {
		setup = func() error { return nil }
	}
	r.h.core, r.h.setup, fake.rep = fake, setup, r.h
	go func() {
		r.h.serve(inR)
		close(r.done)
	}()
	go readFrames(outR, func(line []byte) bool {
		m, ok := decodeMessage(line)
		if ok {
			r.msgs <- m
		}
		return ok
	}, func(s string) { r.stray <- s })
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the helper did not stop after stdin ended")
		}
		outR.Close()
	})
	return r
}

func (r *rig) send(req helperRequest) {
	r.t.Helper()
	if err := writeFrame(r.in, req); err != nil {
		r.t.Fatal(err)
	}
}

// call sends a call and returns its id.
func (r *rig) call(name string) int64 {
	r.id++
	r.send(helperRequest{ID: r.id, Call: name})
	return r.id
}

// until reads messages until want accepts one, and returns it with every
// log line and event seen before it.
func (r *rig) until(want func(helperMessage) bool) (helperMessage, []string, []helperMessage) {
	r.t.Helper()
	var lines []string
	var events []helperMessage
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if want(m) {
				return m, lines, events
			}
			if m.Event == eventLog {
				lines = append(lines, m.Lines...)
			} else {
				events = append(events, m)
			}
		case <-deadline:
			r.t.Fatal("no such message from the helper")
		}
	}
}

func (r *rig) reply(id int64) (helperMessage, []string, []helperMessage) {
	r.t.Helper()
	return r.until(func(m helperMessage) bool { return m.ID == id })
}

func (r *rig) exit() int {
	r.t.Helper()
	select {
	case code := <-r.exits:
		return code
	case <-time.After(10 * time.Second):
		r.t.Fatal("the helper did not exit")
		return -1
	}
}

func contains(lines []string, s string) int {
	for i, l := range lines {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

// A sync reaches the window as the page's events, with every log line
// before the reply, in the order it was written.
func TestHelperStartAndSync(t *testing.T) {
	r := newRig(t, &fakeNode{}, nil)
	id := r.call(callStartAndSync)
	m, lines, events := r.reply(id)
	if m.Error != "" || m.Restart || m.Status == nil || m.Status.NodeID != "fake" {
		t.Fatalf("reply %+v", m)
	}
	var stages []string
	for _, e := range events {
		switch e.Event {
		case eventProgress:
			stages = append(stages, "progress:"+e.Text)
		case eventSync:
			stages = append(stages, e.Sync.Stage)
		}
	}
	if got := strings.Join(stages, ","); got != "progress:Starting the node...,headers,addresses,channels" {
		t.Errorf("events %s", got)
	}
	started, up, caught := contains(lines, "[recovery] Starting the node..."), contains(lines, "lnd: node up"), contains(lines, "[recovery] Catching up")
	if started < 0 || up < started || caught < up {
		t.Errorf("log lines out of order or missing: %q", lines)
	}
	if contains(lines, "Looking for funds") >= 0 {
		t.Error("the search's twice-a-second report went to the log")
	}
}

// A planned restart is a flag, not an error text, and the helper exits:
// its library does not start again in the same process. Core stopped the
// node; a stop after that leaves the library alone.
func TestHelperPlannedRestart(t *testing.T) {
	fake := &fakeNode{restart: true}
	r := newRig(t, fake, nil)
	id := r.call(callStartAndSync)
	m, lines, _ := r.reply(id)
	if !m.Restart || m.Error != "" {
		t.Fatalf("reply %+v", m)
	}
	if contains(lines, "stopped for the restart") < 0 {
		t.Errorf("the node's last lines came after the reply: %q", lines)
	}
	if code := r.exit(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if m, _, _ := r.reply(r.call(callStatus)); m.Error != errHelperStopping.Error() {
		t.Errorf("a call after the restart: %+v", m)
	}
	r.send(helperRequest{Call: callStop})
	r.exit()
	if notes := fake.noted(); len(notes) != 0 {
		t.Errorf("the node was used after the restart: %q", notes)
	}
}

// Cancel ends the running call; the next call runs.
func TestHelperCancel(t *testing.T) {
	fake := &fakeNode{wait: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newRig(t, fake, nil)
	id := r.call(callStartAndSync)
	r.until(func(m helperMessage) bool { return m.Event == eventSync && m.Sync.Stage == "addresses" })
	if m, _, _ := r.reply(r.call(callStatus)); m.Error != errBusy.Error() {
		t.Errorf("a second call while one runs: %+v", m)
	}
	r.send(helperRequest{Call: callCancel})
	m, _, _ := r.reply(id)
	if !m.Canceled || !strings.Contains(m.Error, "canceled") || !m.NodeUp {
		t.Fatalf("reply %+v", m)
	}
	if m, _, _ := r.reply(r.call(callStatus)); m.Error != "" || m.Status == nil {
		t.Errorf("the call after a cancel: %+v", m)
	}
}

// A stop request stops the node and exits, with the node's last lines
// sent first.
func TestHelperStop(t *testing.T) {
	fake := &fakeNode{}
	r := newRig(t, fake, nil)
	r.send(helperRequest{Call: callStop})
	if code := r.exit(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if got := strings.Join(fake.noted(), ","); got != "stop 20ms" {
		t.Errorf("notes %s", got)
	}
	r.until(func(m helperMessage) bool { return m.Event == eventLog && contains(m.Lines, "Shutdown complete") >= 0 })
}

// When the window is gone (stdin ends) the call is cancelled and has
// returned before the node is stopped.
func TestHelperWindowGone(t *testing.T) {
	fake := &fakeNode{wait: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newRig(t, fake, nil)
	r.call(callStartAndSync)
	r.until(func(m helperMessage) bool { return m.Event == eventSync && m.Sync.Stage == "addresses" })
	r.in.Close()
	if code := r.exit(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if got := strings.Join(fake.noted(), ","); got != "wait returned,stop 15ms" {
		t.Errorf("notes %s", got)
	}
}

// A write the window no longer reads means it is gone: the helper stops.
func TestHelperStopsWhenTheWindowStopsReading(t *testing.T) {
	fake := &fakeNode{}
	r := newRig(t, fake, nil)
	r.out.Close()
	r.call(callStatus)
	if code := r.exit(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if got := strings.Join(fake.noted(), ","); got != "status,stop 15ms" {
		t.Errorf("notes %s", got)
	}
}

// A stop that does not return still ends the process.
func TestHelperHardExit(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := newRig(t, &fakeNode{stopHook: func() { <-release }}, nil)
	r.h.hardExit = 300 * time.Millisecond
	start := time.Now()
	r.send(helperRequest{Call: callStop})
	if code := r.exit(); code != 2 {
		t.Errorf("exit %d", code)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("hard exit after %v", took)
	}
}

// A broadcast under way is not cut short by a stop.
func TestHelperStopWaitsForABroadcast(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeNode{broadcast: func() {
		close(started)
		time.Sleep(600 * time.Millisecond) // past the call grace of 200 ms
	}}
	r := newRig(t, fake, nil)
	r.id++
	r.send(helperRequest{ID: r.id, Call: callBroadcastSweep, ConfTarget: 6})
	<-started
	r.send(helperRequest{Call: callStop})
	if code := r.exit(); code != 0 {
		t.Errorf("exit %d", code)
	}
	if got := strings.Join(fake.noted(), ","); got != "broadcast,status,stop 20ms" {
		t.Errorf("notes %s", got)
	}
	if m, _, _ := r.reply(r.id); m.TxID != "tx6" {
		t.Errorf("reply %+v", m)
	}
}

// The helper's setup (the backup's lock) is tried again by the next call
// until it succeeds, and then never again.
func TestHelperSetupRetried(t *testing.T) {
	tries := 0
	r := newRig(t, &fakeNode{}, func() error {
		tries++
		if tries == 1 {
			return core.ErrBackupInUse
		}
		return nil
	})
	if m, _, _ := r.reply(r.call(callStatus)); m.Error != core.ErrBackupInUse.Error() {
		t.Errorf("first call: %+v", m)
	}
	for i := 0; i < 2; i++ {
		if m, _, _ := r.reply(r.call(callStatus)); m.Error != "" {
			t.Errorf("call %d: %+v", i+2, m)
		}
	}
	if tries != 2 {
		t.Errorf("setup ran %d times", tries)
	}
}

// Every call answers with its result; stray input is logged, not fatal.
func TestHelperCalls(t *testing.T) {
	big := &core.History{Totals: core.Totals{In: 7}}
	for i := 0; i < 20000; i++ {
		big.Entries = append(big.Entries, core.Entry{Kind: core.KindReceived, Detail: strings.Repeat("d", 250)})
	}
	r := newRig(t, &fakeNode{history: big}, nil)
	if _, err := io.WriteString(r.in, "hello\n"); err != nil {
		t.Fatal(err)
	}
	m, lines, _ := r.reply(r.call(callHistory))
	if m.History == nil || len(m.History.Entries) != 20000 || m.History.Totals.In != 7 {
		t.Fatalf("history %+v", m.Error)
	}
	if contains(lines, "not a request: hello") < 0 {
		t.Errorf("stray input not logged: %q", lines)
	}
	r.id++
	r.send(helperRequest{ID: r.id, Call: callPrepareSweep, Address: " bc1qaddress "})
	if m, _, _ := r.reply(r.id); m.Plan == nil || m.Plan.Address != "bc1qaddress" {
		t.Errorf("prepare: %+v", m)
	}
	if m, _, _ := r.reply(r.call("lncli")); !strings.Contains(m.Error, "unknown call") {
		t.Errorf("unknown call: %+v", m)
	}
	// A report that cannot be encoded is left out; the stream goes on.
	r.h.send(&helperMessage{Event: eventSync, Sync: &core.SyncProgress{Percent: math.NaN()}})
	if m, _, _ := r.reply(r.call(callStatus)); m.Status == nil {
		t.Errorf("status: %+v", m)
	}
}

// A ping is answered with the version, without the backup's lock or the
// node.
func TestHelperPing(t *testing.T) {
	fake := &fakeNode{}
	r := newRig(t, fake, func() error {
		t.Error("the ping ran the setup")
		return nil
	})
	if m, _, _ := r.reply(r.call(callPing)); m.Text != version || m.Error != "" {
		t.Errorf("ping: %+v", m)
	}
	if notes := fake.noted(); len(notes) != 0 {
		t.Errorf("the ping reached the node: %q", notes)
	}
}

// Stray output never breaks the stream: a line that is no frame is handed
// on as text, a frame written right after a stray write without a line end
// still arrives, and lines have no length limit.
func TestReadFrames(t *testing.T) {
	big := strings.Repeat("x", 5<<20)
	var in bytes.Buffer
	in.WriteString("panic: something\n\n")
	writeFrame(&in, helperMessage{Event: eventProgress, Text: "one"})
	in.WriteString(`{"foo":1}` + "\n")
	in.WriteString("stray without an end")
	writeFrame(&in, helperMessage{ID: 3, TxID: big})
	in.WriteString(`{"event":"progress","text":"last, no line end"}`)
	var got []helperMessage
	var other []string
	err := readFrames(&in, func(line []byte) bool {
		m, ok := decodeMessage(line)
		if ok {
			got = append(got, m)
		}
		return ok
	}, func(s string) { other = append(other, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Text != "one" || got[1].ID != 3 || len(got[1].TxID) != len(big) || got[2].Text != "last, no line end" {
		t.Errorf("frames: %d", len(got))
	}
	if strings.Join(other, "|") != `panic: something|{"foo":1}|stray without an end` {
		t.Errorf("other lines %q", other)
	}
}

// helperProcess starts the test binary as a helper over a fake node.
func helperProcess(t *testing.T, mode string) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), fakeHelperEnv+"="+mode)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, in, out
}

// waitExit waits for the helper process and fails the test after limit.
func waitExit(t *testing.T, cmd *exec.Cmd, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		cmd.Process.Kill()
		t.Fatalf("the helper did not exit within %v", limit)
		return nil
	}
}

// readAll reads the helper's stdout to its end, as the window must before
// Wait, and returns the frames. onReply sees each reply as it arrives.
func readAll(t *testing.T, out io.Reader, onReply func(helperMessage)) []helperMessage {
	t.Helper()
	var got []helperMessage
	err := readFrames(out, func(line []byte) bool {
		m, ok := decodeMessage(line)
		if ok {
			got = append(got, m)
			if m.ID != 0 {
				onReply(m)
			}
		}
		return ok
	}, func(s string) { t.Errorf("stray output: %q", s) })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func logLines(msgs []helperMessage) []string {
	var lines []string
	for _, m := range msgs {
		lines = append(lines, m.Lines...)
	}
	return lines
}

// The real process: a call is answered, and the helper stops the node and
// exits when its stdin ends.
func TestHelperProcess(t *testing.T) {
	cmd, in, out := helperProcess(t, "sync")
	if err := writeFrame(in, helperRequest{ID: 1, Call: callStartAndSync}); err != nil {
		t.Fatal(err)
	}
	msgs := readAll(t, out, func(helperMessage) { in.Close() })
	if err := waitExit(t, cmd, 30*time.Second); err != nil {
		t.Fatalf("exit: %v", err)
	}
	var reply *helperMessage
	for i := range msgs {
		if msgs[i].ID == 1 {
			reply = &msgs[i]
		}
	}
	if reply == nil || reply.Status == nil || reply.Status.NodeID != "fake" {
		t.Fatalf("reply %+v", reply)
	}
	lines := logLines(msgs)
	if contains(lines, "The node runs in a helper process") < 0 || contains(lines, "Shutdown complete") < 0 {
		t.Errorf("log %q", lines)
	}
}

// After a planned restart the process exits by itself, stdin still open.
func TestHelperProcessExitsForARestart(t *testing.T) {
	cmd, in, out := helperProcess(t, "restart")
	defer in.Close()
	if err := writeFrame(in, helperRequest{ID: 1, Call: callStartAndSync}); err != nil {
		t.Fatal(err)
	}
	restart := false
	readAll(t, out, func(m helperMessage) { restart = m.Restart })
	if err := waitExit(t, cmd, 10*time.Second); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if !restart {
		t.Error("the reply did not ask for a restart")
	}
}

// With the window gone, a write to it fails: SIGPIPE on macOS and Linux
// would kill the helper at once, with the node running. The helper stops
// the node and exits instead.
func TestHelperProcessSurvivesABrokenPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGPIPE on Windows")
	}
	cmd, in, out := helperProcess(t, "sync")
	defer in.Close() // open: the stop must come from the failed write
	out.Close()
	err := waitExit(t, cmd, 30*time.Second)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("the helper died: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
}
