package core

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// A walk over the chain downloads every block's filter, gigabytes for a few
// years, and the node keeps none of them. What the walk learnt about the
// channels is therefore kept in a small file, so the next walk, after a
// restart or on a later day, only covers the blocks since.
//
// The file is used only when all of it fits the node: its format, every
// channel's scripts and start height, and the hash of the block it stands
// on. Anything else discards it and the walk is done again from the start.
// It stands a few blocks below the tip it reached, so an ordinary
// reorganisation of the tip does not cost a whole walk; those blocks are
// simply looked at again, which changes nothing that was already known.
const (
	walkStateFile    = "chain-walk.json"
	walkStateVersion = 2
	walkStateBack    = 6 // blocks below the reached tip the file stands on
)

type savedWalk struct {
	Version int `json:"version"`
	// Next is the first block the next walk looks at, LastHash the hash of
	// the block before it.
	Next     uint32         `json:"next"`
	LastHash string         `json:"lastHash"`
	Channels []savedChannel `json:"channels"`
}

type savedChannel struct {
	ChanPoint  string `json:"chanPoint"`
	PkScript   string `json:"pkScript"`
	ToUsScript string `json:"toUsScript"`
	HeightHint uint32 `json:"heightHint"`
	// Unusable is the reason the channel cannot be looked for, "" when it can.
	Unusable string `json:"unusable,omitempty"`

	Found       bool   `json:"found"`
	ScriptOK    bool   `json:"scriptOk"`
	ClosingTxID string `json:"closingTxid,omitempty"`
	CloseHeight uint32 `json:"closeHeight,omitempty"`
	ToUs        string `json:"toUs,omitempty"` // txid:index
	ToUsValue   int64  `json:"toUsValue,omitempty"`
	ToUsSpent   bool   `json:"toUsSpent,omitempty"`
}

// save writes what the walk knows. fundings are all channels it was made
// for, the unusable ones included.
func (w *chainWalk) save(dir string, chain filterSource, fundings []channelFunding) error {
	if w.next <= walkStateBack+1 {
		return nil
	}
	next := w.next - walkStateBack
	hash, err := chain.GetBlockHash(int64(next - 1))
	if err != nil {
		return fmt.Errorf("block %d: %w", next-1, err)
	}
	out := savedWalk{Version: walkStateVersion, Next: next, LastHash: hash.String()}
	states := map[string]*fundingState{}
	for _, st := range w.states {
		states[st.f.chanPoint] = st
	}
	for _, f := range fundings {
		sc := savedChannel{ChanPoint: f.chanPoint, PkScript: hex.EncodeToString(f.pkScript),
			ToUsScript: hex.EncodeToString(f.toUsScript), HeightHint: f.heightHint}
		if v, ok := w.unusable[f.chanPoint]; ok {
			sc.Unusable = v.reason
		} else if st := states[f.chanPoint]; st != nil {
			sc.Found, sc.ScriptOK, sc.ToUsSpent, sc.ToUsValue = st.found, st.scriptOK, st.toUsSpent, st.toUsValue
			if st.spent != nil {
				sc.ClosingTxID, sc.CloseHeight = st.spent.ClosingTxID, st.spent.Height
			}
			if st.toUs != nil {
				sc.ToUs = st.toUs.String()
			}
		} else {
			return fmt.Errorf("channel %s is not part of the walk", f.chanPoint)
		}
		out.Channels = append(out.Channels, sc)
	}
	raw, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, walkStateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, walkStateFile))
}

// loadWalk returns the saved walk when it fits the node and these channels
// exactly, and otherwise nil with the reason.
func loadWalk(dir string, chain filterSource, fundings []channelFunding) (*chainWalk, string) {
	raw, err := os.ReadFile(filepath.Join(dir, walkStateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, err.Error()
	}
	var in savedWalk
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, "unreadable: " + err.Error()
	}
	if in.Version != walkStateVersion {
		return nil, fmt.Sprintf("format %d, want %d", in.Version, walkStateVersion)
	}
	best, err := chain.BestBlock()
	if err != nil {
		return nil, err.Error()
	}
	if in.Next < 2 || in.Next > uint32(best.Height)+1 {
		return nil, fmt.Sprintf("stands on block %d, the chain tip is %d", in.Next-1, best.Height)
	}
	hash, err := chain.GetBlockHash(int64(in.Next - 1))
	if err != nil {
		return nil, err.Error()
	}
	if hash.String() != in.LastHash {
		return nil, fmt.Sprintf("block %d changed", in.Next-1)
	}
	saved := map[string]savedChannel{}
	for _, sc := range in.Channels {
		saved[sc.ChanPoint] = sc
	}
	w := &chainWalk{unusable: map[string]channelVerdict{}, next: in.Next, all: fundings}
	for _, f := range fundings {
		sc, ok := saved[f.chanPoint]
		if !ok {
			return nil, "channel " + f.chanPoint + " is not in it"
		}
		pk, err1 := hex.DecodeString(sc.PkScript)
		toUs, err2 := hex.DecodeString(sc.ToUsScript)
		if err1 != nil || err2 != nil || !bytes.Equal(pk, f.pkScript) || !bytes.Equal(toUs, f.toUsScript) || sc.HeightHint != f.heightHint {
			return nil, "channel " + f.chanPoint + " differs"
		}
		if sc.Unusable != "" {
			w.unusable[f.chanPoint] = channelVerdict{verdict: verdictUnverified, reason: sc.Unusable}
			continue
		}
		st := &fundingState{f: f, found: sc.Found, scriptOK: sc.ScriptOK, toUsValue: sc.ToUsValue, toUsSpent: sc.ToUsSpent}
		if sc.ClosingTxID != "" {
			st.spent = &SpentChannel{ChannelPoint: f.chanPoint, ClosingTxID: sc.ClosingTxID, Height: sc.CloseHeight}
		}
		if sc.ToUs != "" {
			op, err := parseOutPoint(sc.ToUs)
			if err != nil {
				return nil, "channel " + f.chanPoint + ": " + err.Error()
			}
			st.toUs = op
		}
		if st.toUs != nil && st.spent == nil {
			return nil, "channel " + f.chanPoint + " has a close output without a close"
		}
		w.states = append(w.states, st)
	}
	return w, ""
}

func parseOutPoint(s string) (*wire.OutPoint, error) {
	var txid string
	var index uint32
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			txid = s[:i]
			if _, err := fmt.Sscanf(s[i+1:], "%d", &index); err != nil {
				return nil, fmt.Errorf("bad outpoint %q", s)
			}
			hash, err := chainhash.NewHashFromStr(txid)
			if err != nil {
				return nil, fmt.Errorf("bad outpoint %q", s)
			}
			return wire.NewOutPoint(hash, index), nil
		}
	}
	return nil, fmt.Errorf("bad outpoint %q", s)
}

// savedFundings are the channels the saved walk covers, nil when there is
// no usable file. The walk itself is loaded, and checked, by loadWalk.
func savedFundings(dir string) []channelFunding {
	raw, err := os.ReadFile(filepath.Join(dir, walkStateFile))
	if err != nil {
		return nil
	}
	var in savedWalk
	if json.Unmarshal(raw, &in) != nil || in.Version != walkStateVersion {
		return nil
	}
	var out []channelFunding
	for _, sc := range in.Channels {
		op, err1 := parseOutPoint(sc.ChanPoint)
		pk, err2 := hex.DecodeString(sc.PkScript)
		toUs, err3 := hex.DecodeString(sc.ToUsScript)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil
		}
		f := channelFunding{chanPoint: sc.ChanPoint, outpoint: *op, pkScript: pk, heightHint: sc.HeightHint}
		if len(toUs) > 0 {
			f.toUsScript = toUs
		}
		out = append(out, f)
	}
	return out
}
