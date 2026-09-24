package main

import (
	"strings"
	"testing"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// The relaunched window goes back where the old one was, unless that spot
// is off the screen it now opens on (the old window was on another one).
func TestRelaunchWindowPlace(t *testing.T) {
	t.Setenv(windowEnv, "200,150,1100,760")
	x, y, w, h, ok := relaunchWindow()
	if !ok || x != 200 || y != 150 || w != 1100 || h != 760 {
		t.Fatalf("got %d,%d,%d,%d %v", x, y, w, h, ok)
	}
	for _, bad := range []string{"", "200,150", "200,150,0,760", "a,b,c,d"} {
		t.Setenv(windowEnv, bad)
		if _, _, _, _, ok := relaunchWindow(); ok {
			t.Errorf("%q read as a window place", bad)
		}
	}

	screen := func(current bool) wruntime.Screen {
		s := wruntime.Screen{IsCurrent: current}
		s.Size.Width, s.Size.Height = 1512, 945
		return s
	}
	screens := []wruntime.Screen{screen(false), screen(true)}
	for _, c := range []struct {
		x, y int
		fits bool
	}{{200, 150, true}, {412, 185, true}, {413, 150, false}, {-10, 150, false}, {200, 186, false}, {2000, 150, false}} {
		if got := fitsScreen(screens, c.x, c.y, 1100, 760); got != c.fits {
			t.Errorf("at %d,%d: fits %v, want %v", c.x, c.y, got, c.fits)
		}
	}
	if fitsScreen(nil, 0, 0, 1100, 760) {
		t.Error("fits with no screen known")
	}
}

// The restarted copy gets the current settings, not the ones it was
// started with, and keeps the rest of the environment.
func TestWithEnvReplaces(t *testing.T) {
	got := withEnv([]string{"HOME=/h", "BREEZ_RECOVERY_WORKDIR=/old", "BREEZ_RECOVERY_RELAUNCH=continue", "PATH=/p"},
		"BREEZ_RECOVERY_RELAUNCH=restore-other", "BREEZ_RECOVERY_WORKDIR=/new", "BREEZ_RECOVERY_PEERS=")
	want := []string{"HOME=/h", "PATH=/p", "BREEZ_RECOVERY_RELAUNCH=restore-other", "BREEZ_RECOVERY_WORKDIR=/new", "BREEZ_RECOVERY_PEERS="}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
