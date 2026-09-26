package main

import (
	"strings"
	"testing"
)

// The restarted copy gets the Advanced settings, replacing any values the
// environment already had.
func TestWithEnvReplaces(t *testing.T) {
	env := []string{"HOME=/h", "BREEZ_RECOVERY_WORKDIR=/old", "BREEZ_RECOVERY_RELAUNCH=x"}
	got := withEnv(env, "BREEZ_RECOVERY_RELAUNCH=use", "BREEZ_RECOVERY_WORKDIR=/new", "BREEZ_RECOVERY_PEERS=")
	want := "HOME=/h BREEZ_RECOVERY_RELAUNCH=use BREEZ_RECOVERY_WORKDIR=/new BREEZ_RECOVERY_PEERS="
	if strings.Join(got, " ") != want {
		t.Fatalf("got %q, want %q", strings.Join(got, " "), want)
	}
}
