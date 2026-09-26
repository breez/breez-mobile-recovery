package core

import (
	"context"
	"errors"
	"testing"
)

// Stop pressed during the peer check is a cancel, not "none of the bitcoin
// peers answers" with its advice to check the internet connection.
func TestPeerCheckStopped(t *testing.T) {
	c := testCore(t, t.TempDir())
	c.cfg.Peers = "127.0.0.1:1, 127.0.0.1:2"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.checkPeers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("peer check after a stop: %v", err)
	}
}
