package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
