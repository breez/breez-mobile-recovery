package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

// TestBuiltHelper checks the helper mode of a built app, the way the window
// uses it: started with the window's command, over pipes, a ping answered,
// and an exit of its own once its stdin ends. The build workflow runs it on
// each platform's release build (a Windows GUI program, a macOS universal
// binary), with BREEZ_RECOVERY_SMOKE_EXE naming the executable. Nothing
// starts a node, and the work folder is a new, empty one.
func TestBuiltHelper(t *testing.T) {
	exe := os.Getenv("BREEZ_RECOVERY_SMOKE_EXE")
	if exe == "" {
		t.Skip("BREEZ_RECOVERY_SMOKE_EXE is not set")
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	exited := make(chan error, 1)
	p, err := startNodeProc(helperCommandOf(exe, "smoke", core.Config{WorkDir: work}), filepath.Join(work, "backups", "smoke"), nodeHandlers{
		event: func(m helperMessage) {
			for _, l := range m.Lines {
				t.Log(l)
			}
		},
		// Logged, not failed: the window logs such lines too.
		text: func(s string) { t.Log("output that is no frame: " + s) },
		exit: func(_ *nodeProc, err error, _, _ bool) { exited <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.kill()
		<-p.done
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := p.call(ctx, helperRequest{Call: callPing}, 5*time.Second, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if m.Text == "" {
		t.Error("the ping's reply has no version")
	}
	t.Log("helper version " + m.Text)
	// The window is gone.
	p.in.Close()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the helper did not exit after its stdin ended")
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Errorf("the helper wrote to its work folder: %v %v", entries, err)
	}
}
