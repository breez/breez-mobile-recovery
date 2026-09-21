package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func TestKnownUpTo(t *testing.T) {
	headerAt := func(h uint32) (*wire.BlockHeader, error) {
		return &wire.BlockHeader{Nonce: h, Timestamp: time.Unix(1600000000+int64(h)*600, 0)}, nil
	}
	known, err := knownUpTo(headerAt, 900000, 687000)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(known.Blocks); n != knownHashes {
		t.Fatalf("%d blocks, want %d", n, knownHashes)
	}
	if top := known.Blocks[len(known.Blocks)-1].Height; top != 900000-knownBelowTip {
		t.Errorf("stands on %d, want %d", top, 900000-knownBelowTip)
	}
	for i := 1; i < len(known.Blocks); i++ {
		if known.Blocks[i].Height != known.Blocks[i-1].Height+1 {
			t.Fatal("heights are not consecutive")
		}
	}
	// A short chain: never at or below the node's first block.
	known, err = knownUpTo(headerAt, 687050, 687000)
	if err != nil {
		t.Fatal(err)
	}
	if known.Blocks[0].Height != 687001 {
		t.Errorf("starts at %d, want 687001", known.Blocks[0].Height)
	}
	if _, err := knownUpTo(headerAt, 687005, 687000); err == nil {
		t.Error("a walk of five blocks was accepted")
	}
}

// TestKnownHistoryOnARealWallet writes the sync state into a COPY of a real
// wallet file and reads it back the way btcwallet will. Gated:
//
//	BREEZ_LIVE_WALLETDB_COPY  a wallet.db lnd has synced once (birthday block
//	                          located and verified); it is copied, not touched
func TestKnownHistoryOnARealWallet(t *testing.T) {
	src := os.Getenv("BREEZ_LIVE_WALLETDB_COPY")
	if src == "" {
		t.Skip("BREEZ_LIVE_WALLETDB_COPY not set")
	}
	path := filepath.Join(t.TempDir(), "wallet.db")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.Create(path)
	io.Copy(out, in)
	in.Close()
	out.Close()

	scripts, err := walletScripts(path, "mainnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the wallet's history check watches %d scripts", len(scripts))

	var known knownHistory
	for h := uint32(960000); h < 960000+knownHashes; h++ {
		hash := chainhash.DoubleHashH([]byte(fmt.Sprint(h)))
		known.Blocks = append(known.Blocks, knownBlock{Height: h, Hash: hash.String(), Time: 1780000000 + int64(h)})
	}
	applied, err := applyKnownHistory(path, "mainnet", &known)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("not applied: the wallet has no verified birthday block below 960000")
	}
	// The integrity of the file after the write.
	if err := checkBolt(path); err != nil {
		t.Fatal(err)
	}
	// A second write on top must work too (hashes below are in place now).
	more := knownHistory{Blocks: []knownBlock{{Height: 960000 + knownHashes, Hash: chainhash.DoubleHashH([]byte("next")).String(), Time: 1790000000}}}
	if applied, err := applyKnownHistory(path, "mainnet", &more); err != nil || !applied {
		t.Fatalf("second write: applied %v, %v", applied, err)
	}
	// Blocks at or below the birthday block are refused, nothing written.
	low := knownHistory{Blocks: []knownBlock{{Height: 1, Hash: chainhash.DoubleHashH([]byte("low")).String(), Time: 1}}}
	if applied, err := applyKnownHistory(path, "mainnet", &low); err != nil || applied {
		t.Fatalf("a block below the birthday: applied %v, %v", applied, err)
	}
}
