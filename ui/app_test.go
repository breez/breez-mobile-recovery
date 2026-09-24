package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// The helper gets the current settings, not the ones the window was
// started with, and keeps the rest of the environment.
func TestWithEnvReplaces(t *testing.T) {
	got := withEnv([]string{"HOME=/h", "BREEZ_RECOVERY_WORKDIR=/old", "BREEZ_RECOVERY_NODE_HELPER=a", "PATH=/p"},
		"BREEZ_RECOVERY_NODE_HELPER=b", "BREEZ_RECOVERY_WORKDIR=/new", "BREEZ_RECOVERY_PEERS=")
	want := []string{"HOME=/h", "PATH=/p", "BREEZ_RECOVERY_NODE_HELPER=b", "BREEZ_RECOVERY_WORKDIR=/new", "BREEZ_RECOVERY_PEERS="}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A backup's log is appended to its recovery.log, and the page is told
// to empty its panel; lines waiting to go out are not sent after that.
func TestLogMovesToItsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), backupLogFile)
	var sent []string
	l := newLogBuffer(10)
	l.emit = func(name string, data ...interface{}) { sent = append(sent, name) }
	for _, session := range []string{"first", "second"} {
		l.add(session + " restore")
		if err := l.moveTo(path); err != nil {
			t.Fatal(err)
		}
		l.flush()
		if got := l.text(); got != "\n" {
			t.Errorf("log after the move: %q", got)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "=====") != 2 || strings.Index(string(data), "first restore") > strings.Index(string(data), "second restore") {
		t.Errorf("recovery.log:\n%s", data)
	}
	if strings.Join(sent, ",") != "logreset,logreset" {
		t.Errorf("events %q", sent)
	}
	// A failed write keeps the lines.
	l.add("kept")
	if err := l.moveTo(filepath.Join(t.TempDir(), "missing", backupLogFile)); err == nil {
		t.Fatal("write to a missing folder succeeded")
	}
	if !strings.Contains(l.text(), "kept") {
		t.Error("lines dropped on a failed write")
	}
}

// The sync screen moves with the node's lines while no report comes: at
// the start, and when the node stops to start again, where the bar turns
// into one with no measure. A line in the middle of a stage whose reports
// keep coming leaves the bar where it is.
func TestSyncScreenLines(t *testing.T) {
	var mu sync.Mutex
	var sent []core.SyncProgress
	a := &App{log: newLogBuffer(100), syncQuiet: 100 * time.Millisecond}
	a.emitFn = func(name string, data ...interface{}) {
		if name == "sync" {
			mu.Lock()
			sent = append(sent, data[0].(core.SyncProgress))
			mu.Unlock()
		}
	}
	last := func() core.SyncProgress {
		mu.Lock()
		defer mu.Unlock()
		if len(sent) == 0 {
			return core.SyncProgress{}
		}
		return sent[len(sent)-1]
	}
	check := func(what, stage, msg string, percent float64, remaining int64) {
		t.Helper()
		p := last()
		if p.Stage != stage || p.Message != msg || p.Percent != percent || p.Remaining != remaining {
			t.Errorf("%s: %+v", what, p)
		}
	}
	line := func(s string) { a.nodeEvent(helperMessage{Event: eventProgress, Text: s}) }
	report := func(stage string, percent float64) {
		a.nodeEvent(helperMessage{Event: eventSync, Sync: &core.SyncProgress{Stage: stage, Percent: percent, Remaining: 60, Message: "Making sure your channels are still open"}})
	}

	line("Refreshing the status...")
	if len(sent) != 0 {
		t.Fatalf("a line outside a sync reached the sync screen: %+v", sent)
	}
	a.syncMu.Lock()
	a.syncing = true
	a.syncMu.Unlock()
	line("Starting the node...")
	check("start", "start", "Starting the node...", -1, -1)

	report("channels", 40)
	line("  abcd:1 closed on chain, tx ef01.")
	check("line mid-stage", "channels", "abcd:1 closed on chain, tx ef01.", 40, 60)
	report("channels", 41)
	time.Sleep(300 * time.Millisecond)
	check("report after the line", "channels", "Making sure your channels are still open", 41, 60)

	line("Stopping the node to start it again...")
	time.Sleep(300 * time.Millisecond)
	check("stop", "channels", "Stopping the node to start it again...", -1, -1)
	a.syncStep("Starting the node again...")
	check("again", "start", "Starting the node again...", -1, -1)
	line("Starting the node...")
	check("new start", "start", "Starting the node...", -1, -1)
}
