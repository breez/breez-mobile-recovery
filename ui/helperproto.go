package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"github.com/breez/breez-mobile-recovery/core"
)

// The window and its node helper (helper.go) talk in JSON, one object per
// line: requests on the helper's stdin, events and replies on the stdout it
// had before core took os.Stdout for the library's logs. A line that is not
// a frame (a stray write to the helper's stdout) is handed on as text and
// never breaks the stream.

// The calls a helper serves.
const (
	callStartAndSync   = "startAndSync"
	callStatus         = "status"
	callHistory        = "history"
	callPrepareSweep   = "prepareSweep"
	callBroadcastSweep = "broadcastSweep"
	// callCancel cancels the call that runs; callStop ends the helper. Neither
	// is answered, and neither takes an id.
	callCancel = "cancel"
	callStop   = "stop"
	// callPing is answered at once with the program's version in Text,
	// without the node or the backup folder: the release build's check
	// that the helper mode works (TestBuiltHelper).
	callPing = "ping"
)

// helperRequest is a line the window writes to the helper.
type helperRequest struct {
	// ID comes back in the reply.
	ID         int64  `json:"id,omitempty"`
	Call       string `json:"call"`
	Address    string `json:"address,omitempty"`    // prepareSweep
	ConfTarget int    `json:"confTarget,omitempty"` // broadcastSweep
}

// The events a helper sends.
const (
	eventLog      = "log"      // Lines: log lines, formatted as the window's own
	eventProgress = "progress" // Text: a progress line for the page (already in the log)
	eventSync     = "sync"     // Sync: a sync report for the page
)

// helperMessage is a line the helper writes to the window: an event (Event
// set) or the reply to a call (ID set).
type helperMessage struct {
	Event string             `json:"event,omitempty"`
	Lines []string           `json:"lines,omitempty"`
	Text  string             `json:"text,omitempty"`
	Sync  *core.SyncProgress `json:"sync,omitempty"`

	ID    int64  `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
	// Restart is core.ErrRestartRequired: the node has stopped for a change
	// that needs a new start, and the helper exits after this reply.
	Restart bool `json:"restart,omitempty"`
	// Canceled says Error is the call's cancellation.
	Canceled bool            `json:"canceled,omitempty"`
	Status   *core.Status    `json:"status,omitempty"`
	History  *core.History   `json:"history,omitempty"`
	Plan     *core.SweepPlan `json:"plan,omitempty"`
	TxID     string          `json:"txid,omitempty"`
}

// writeFrame writes v as one line. json.Marshal escapes every newline inside
// a string, so a frame is always exactly one line.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readFrames reads r line by line until it ends and returns nil at EOF.
// decode is given each line and reports whether it was a frame; any other
// line goes to other. Lines have no length limit: a History reply is large.
func readFrames(r io.Reader, decode func([]byte) bool, other func(string)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 && !decode(t) {
			// A stray write without its own line end runs into the frame
			// written after it.
			if i := bytes.Index(t, []byte(`{"`)); i > 0 && decode(t[i:]) {
				other(string(t[:i]))
			} else {
				other(string(t))
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// decodeRequest reads a request line; ok is false for anything else.
func decodeRequest(line []byte) (req helperRequest, ok bool) {
	if json.Unmarshal(line, &req) != nil || req.Call == "" {
		return helperRequest{}, false
	}
	return req, true
}

// decodeMessage reads a line from the helper; ok is false for anything
// else.
func decodeMessage(line []byte) (msg helperMessage, ok bool) {
	if json.Unmarshal(line, &msg) != nil || (msg.Event == "" && msg.ID == 0) {
		return helperMessage{}, false
	}
	return msg, true
}
