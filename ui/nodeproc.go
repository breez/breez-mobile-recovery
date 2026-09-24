package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// The window's side of the node helper (helper.go). The window starts a
// helper for the backup in use, sends it the calls that need the node,
// passes on what it reports to the log and the page, and stops it. One
// helper at a time, and a new one only once the last one's Wait returned:
// two cannot share a backup's databases (breez.db is opened without a
// timeout).

const (
	// helperStopTimeout is how long the window waits for a helper to exit
	// before it kills it. The helper stops its node within 20 s of a stop
	// request and ends itself 25 s after.
	helperStopTimeout = 25 * time.Second
	// helperCancelTimeout is how long a cancelled call may take to answer
	// before its helper is killed. A broadcast is never killed this way.
	helperCancelTimeout = 30 * time.Second
	// maxNodeRestarts caps the planned restarts in a row in one sync.
	maxNodeRestarts = 6
)

var (
	errNodeCrashed    = errors.New("the node stopped unexpectedly. Continue recovery starts it again; Save log has the details")
	errNodeNotRunning = errors.New("the node is not running. Continue recovery starts it")
	errNodeStopped    = errors.New("the node was stopped")
)

// helperCommand is the command that runs the node helper on the backup
// named name with cfg's work folder and peers: this program again.
func helperCommand(name string, cfg core.Config) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return helperCommandOf(exe, name, cfg), nil
}

// helperCommandOf is helperCommand with the program exe.
func helperCommandOf(exe, name string, cfg core.Config) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.Env = withEnv(os.Environ(),
		helperEnv+"="+name,
		"BREEZ_RECOVERY_WORKDIR="+cfg.WorkDir,
		"BREEZ_RECOVERY_PEERS="+cfg.Peers)
	return cmd
}

// nodeHandlers get what a helper sends besides replies.
type nodeHandlers struct {
	event func(helperMessage) // an event frame
	text  func(string)        // a line of its stderr, or of stdout that is no frame
	// exit runs once Wait returned, before the calls still waiting fail.
	// planned: the window stopped it, or it exited for a restart. waiting:
	// a call was waiting for a reply.
	exit func(p *nodeProc, err error, planned, waiting bool)
}

// nodeProc is one helper process.
type nodeProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	dir    string // the backup folder it runs
	stderr *lineWriter
	h      nodeHandlers

	sendMu sync.Mutex // one frame at a time on stdin

	mu      sync.Mutex
	lastID  int64
	pending map[int64]chan helperMessage
	planned bool
	exited  bool

	done chan struct{} // closed once Wait returned and the calls failed
}

// startNodeProc starts cmd as the helper of the backup folder dir.
func startNodeProc(cmd *exec.Cmd, dir string, h nodeHandlers) (*nodeProc, error) {
	p := &nodeProc{cmd: cmd, dir: dir, h: h, stderr: &lineWriter{line: h.text},
		pending: map[int64]chan helperMessage{}, done: make(chan struct{})}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	p.in = in
	// A panic trace ends up in the log. Wait waits for the copy; the delay
	// bounds it should anything else hold the pipe.
	cmd.Stderr = p.stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go p.read(out)
	return p, nil
}

// read reads the helper's stdout to its end, then waits for the process:
// Wait closes the pipe, and frames not read by then would be lost.
func (p *nodeProc) read(out io.Reader) {
	readFrames(out, func(line []byte) bool {
		m, ok := decodeMessage(line)
		if !ok {
			return false
		}
		if m.ID == 0 {
			p.h.event(m)
			return true
		}
		p.mu.Lock()
		ch := p.pending[m.ID]
		delete(p.pending, m.ID)
		// The helper exits after a restart reply.
		p.planned = p.planned || m.Restart
		p.mu.Unlock()
		if ch != nil {
			ch <- m
		}
		return true
	}, p.h.text)
	err := p.cmd.Wait()
	p.stderr.flush()
	p.mu.Lock()
	p.exited = true
	waiting := p.pending
	p.pending = nil
	planned := p.planned
	p.mu.Unlock()
	p.h.exit(p, err, planned, len(waiting) > 0)
	for _, ch := range waiting {
		close(ch)
	}
	close(p.done)
}

func (p *nodeProc) send(req helperRequest) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return writeFrame(p.in, req)
}

// exitError is what a call gets when the helper exited without answering.
func (p *nodeProc) exitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.planned {
		return errNodeStopped
	}
	return errNodeCrashed
}

// call sends req and waits for the reply. When ctx ends the call is
// cancelled, and a helper that has not answered cancelWait later is
// killed, unless the call is a broadcast. logf notes that kill.
func (p *nodeProc) call(ctx context.Context, req helperRequest, cancelWait time.Duration, logf func(string)) (helperMessage, error) {
	ch := make(chan helperMessage, 1)
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return helperMessage{}, p.exitError()
	}
	p.lastID++
	req.ID = p.lastID
	p.pending[req.ID] = ch
	p.mu.Unlock()
	// A write that fails means the helper is gone: its exit fails the call.
	_ = p.send(req)
	cancelled := ctx.Done()
	var timeout <-chan time.Time
	for {
		select {
		case m, ok := <-ch:
			switch {
			case !ok && ctx.Err() != nil:
				return helperMessage{}, ctx.Err()
			case !ok:
				return helperMessage{}, p.exitError()
			}
			return m, replyError(m)
		case <-cancelled:
			cancelled = nil
			_ = p.send(helperRequest{Call: callCancel})
			if req.Call != callBroadcastSweep {
				timeout = time.After(cancelWait)
			}
		case <-timeout:
			timeout = nil
			logf(fmt.Sprintf("The node did not stop the call within %v of the cancel; its process is ended.", cancelWait))
			p.kill()
		}
	}
}

// replyError is the error a reply carries.
func replyError(m helperMessage) error {
	switch {
	case m.Restart:
		return core.ErrRestartRequired
	case m.Canceled:
		return context.Canceled
	case m.Error != "":
		return errors.New(m.Error)
	}
	return nil
}

// stop asks the helper to stop and returns once it has exited; see await.
func (p *nodeProc) stop(wait time.Duration, logf func(string)) {
	p.mu.Lock()
	p.planned = true
	p.mu.Unlock()
	p.sendMu.Lock()
	_ = writeFrame(p.in, helperRequest{Call: callStop})
	p.in.Close() // stdin's end stops it too, should the frame not arrive
	p.sendMu.Unlock()
	p.await(wait, logf)
}

// await returns once the helper has exited, killing it when it has not
// within wait; logf notes the kill.
func (p *nodeProc) await(wait time.Duration, logf func(string)) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.done:
		return
	case <-t.C:
	}
	logf(fmt.Sprintf("The node did not stop within %v; its process is ended.", wait))
	p.kill()
	<-p.done
}

func (p *nodeProc) kill() {
	p.mu.Lock()
	p.planned = true
	p.mu.Unlock()
	_ = p.cmd.Process.Kill()
}

// lineWriter hands on what is written to it line by line.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		if l := bytes.TrimRight(w.buf[:i], "\r"); len(l) > 0 {
			w.line(string(l))
		}
		w.buf = w.buf[i+1:]
	}
}

// flush hands on a last line without its end. Only after Wait: the copy
// into Write has ended then.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}
