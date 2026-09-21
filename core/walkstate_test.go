package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// closedChannelChain is a chain with one channel the peer closed, paying
// this app 999 sat that nobody collected yet, and some blocks on top.
func closedChannelChain(t *testing.T) (*memChain, []channelFunding, *wire.MsgTx) {
	t.Helper()
	chain := &memChain{first: 700000, prev: map[wire.OutPoint][]byte{}}
	fundingScript := []byte{0x00, 0x20, 0xaa}
	toUs := []byte{0x00, 0x14, 0xbb}
	funding := pay(fundingScript, 100000)
	point := wire.OutPoint{Hash: funding.TxHash()}
	fundedAt := chain.add(funding)
	closing := pay(toUs, 999, point)
	chain.add(closing)
	for i := 0; i < 20; i++ {
		chain.add(pay([]byte{0x51}, int64(i+1)))
	}
	fundings := []channelFunding{
		{chanPoint: point.String(), outpoint: point, pkScript: fundingScript, heightHint: fundedAt, toUsScript: toUs},
		{chanPoint: "alias:0", heightHint: 16000000},
	}
	return chain, fundings, closing
}

func TestSavedWalk(t *testing.T) {
	chain, fundings, closing := closedChannelChain(t)
	dir := t.TempDir()
	best, _ := chain.BestBlock()
	walk := newChainWalk(fundings, nil, 0)
	if err := walk.run(context.Background(), chain, nil); err != nil {
		t.Fatal(err)
	}
	if err := walk.save(dir, chain, fundings); err != nil {
		t.Fatal(err)
	}

	loaded, why := loadWalk(dir, chain, fundings)
	if loaded == nil {
		t.Fatalf("saved walk not used: %s", why)
	}
	got, want := loaded.verdicts(), walk.verdicts()
	for _, f := range fundings {
		if got[f.chanPoint] != want[f.chanPoint] {
			t.Errorf("%s: loaded %+v, walked %+v", f.chanPoint, got[f.chanPoint], want[f.chanPoint])
		}
	}
	if v := got[fundings[0].chanPoint]; v.verdict != verdictSpent || v.spent.Collect != 999 {
		t.Errorf("verdict %q collect %d, want spent with 999 to collect", v.verdict, v.spent.Collect)
	}

	// The sweep arrives; the loaded walk only looks at the blocks it stands
	// below and the new one, and sees it.
	sweptAt := chain.add(pay([]byte{0x51}, 864, wire.OutPoint{Hash: closing.TxHash()}))
	var walked []uint32
	if err := loaded.run(context.Background(), chain, func(h, _, _ uint32) { walked = append(walked, h) }); err != nil {
		t.Fatal(err)
	}
	if len(walked) == 0 || walked[0] != uint32(best.Height)+1-walkStateBack || walked[len(walked)-1] != sweptAt {
		t.Errorf("walked %v, want from block %d to %d", walked, uint32(best.Height)+1-walkStateBack, sweptAt)
	}
	if v := loaded.verdicts()[fundings[0].chanPoint]; v.spent.Collect != 0 {
		t.Errorf("collect %d after the sweep, want 0", v.spent.Collect)
	}
}

func TestSavedWalkIsRejected(t *testing.T) {
	chain, fundings, _ := closedChannelChain(t)
	save := func(t *testing.T) string {
		dir := t.TempDir()
		walk := newChainWalk(fundings, nil, 0)
		if err := walk.run(context.Background(), chain, nil); err != nil {
			t.Fatal(err)
		}
		if err := walk.save(dir, chain, fundings); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	edit := func(t *testing.T, dir, old, new string) {
		path := filepath.Join(dir, walkStateFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), old) {
			t.Fatalf("%q not in the file", old)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, new, 1)), 0600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no file", func(t *testing.T) {
		if w, why := loadWalk(t.TempDir(), chain, fundings); w != nil || why != "" {
			t.Errorf("walk %v, reason %q", w, why)
		}
	})
	t.Run("the chain changed below it", func(t *testing.T) {
		dir := save(t)
		reorged := *chain
		reorged.blocks = append([]*wire.MsgBlock{}, chain.blocks...)
		i := len(reorged.blocks) - 1 - walkStateBack
		other := *reorged.blocks[i]
		other.Header.Nonce = 999999
		reorged.blocks[i] = &other
		if w, why := loadWalk(dir, &reorged, fundings); w != nil || why == "" {
			t.Errorf("used although block %d changed", reorged.first+uint32(i))
		}
	})
	t.Run("another channel", func(t *testing.T) {
		dir := save(t)
		more := append(append([]channelFunding{}, fundings...), channelFunding{chanPoint: "bb:1", heightHint: 700001})
		if w, why := loadWalk(dir, chain, more); w != nil || why == "" {
			t.Error("used although a channel is missing from it")
		}
	})
	t.Run("a channel's script differs", func(t *testing.T) {
		dir := save(t)
		changed := append([]channelFunding{}, fundings...)
		changed[0].pkScript = []byte{0x00, 0x20, 0xab}
		if w, why := loadWalk(dir, chain, changed); w != nil || why == "" {
			t.Error("used although the funding script differs")
		}
	})
	t.Run("other format", func(t *testing.T) {
		dir := save(t)
		edit(t, dir, `"version": 2`, `"version": 3`)
		if w, why := loadWalk(dir, chain, fundings); w != nil || why == "" {
			t.Error("used although the format differs")
		}
	})
	t.Run("damaged", func(t *testing.T) {
		dir := save(t)
		edit(t, dir, `{`, `[`)
		if w, why := loadWalk(dir, chain, fundings); w != nil || why == "" {
			t.Error("used although unreadable")
		}
	})
	t.Run("past the tip", func(t *testing.T) {
		dir := save(t)
		short := *chain
		short.blocks = chain.blocks[:5]
		if w, why := loadWalk(dir, &short, fundings); w != nil || why == "" {
			t.Error("used although it stands past the chain tip")
		}
	})
}
