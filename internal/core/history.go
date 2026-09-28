package core

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Optional encrypted history (§7): off by default. Records live in opaque
// numbered segment blobs under history/ with an encrypted index; file names
// leak nothing about peers or days. `burrow burn` deletes the directory.

const (
	historyDir     = "history"
	historyIndex   = "history/index"
	historySegment = 256 // records per segment blob
)

// HistoryEntry is one stored message.
type HistoryEntry struct {
	Peer PeerID
	ID   MsgID
	Mine bool
	Text string
	At   time.Time
}

type histRec struct {
	Peer string `json:"p"`
	ID   uint64 `json:"i"`
	Mine bool   `json:"m"`
	Text string `json:"t"`
	At   int64  `json:"a"`
}

type histIndex struct {
	Next  int   `json:"next"`  // number of the segment currently being filled
	Count []int `json:"count"` // records per segment, by number
}

func (e *Engine) loadHistIndex() histIndex {
	var idx histIndex
	b, err := e.st.ReadBlob(historyIndex)
	if err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	return idx
}

func segName(n int) string { return historyDir + "/seg-" + strconv.Itoa(n) }

// recordHistory appends one message when history is enabled. Caller must not hold e.mu.
func (e *Engine) recordHistory(peer PeerID, id MsgID, mine bool, text string, at time.Time) {
	if !e.cfg.History {
		return
	}
	e.histMu.Lock()
	defer e.histMu.Unlock()
	idx := e.loadHistIndex()
	var recs []histRec
	if len(idx.Count) > idx.Next && idx.Count[idx.Next] > 0 {
		if b, err := e.st.ReadBlob(segName(idx.Next)); err == nil {
			_ = json.Unmarshal(b, &recs)
		}
	}
	recs = append(recs, histRec{Peer: hex.EncodeToString(peer[:]), ID: uint64(id), Mine: mine, Text: text, At: at.Unix()})
	b, err := json.Marshal(recs)
	if err != nil {
		return
	}
	if err := e.st.WriteBlob(segName(idx.Next), b); err != nil {
		e.log.Warn("history write failed", "err", err.Error())
		return
	}
	for len(idx.Count) <= idx.Next {
		idx.Count = append(idx.Count, 0)
	}
	idx.Count[idx.Next] = len(recs)
	if len(recs) >= historySegment {
		idx.Next++
		idx.Count = append(idx.Count, 0)
	}
	if ib, err := json.Marshal(idx); err == nil {
		_ = e.st.WriteBlob(historyIndex, ib)
	}
}

// History returns the last limit messages exchanged with peer, oldest first.
func (e *Engine) History(peer PeerID, limit int) ([]HistoryEntry, error) {
	if !e.cfg.History {
		return nil, ErrHistoryOff
	}
	if limit <= 0 {
		limit = 100
	}
	e.histMu.Lock()
	defer e.histMu.Unlock()
	idx := e.loadHistIndex()
	want := hex.EncodeToString(peer[:])
	var out []HistoryEntry
	for n := idx.Next; n >= 0 && len(out) < limit; n-- {
		if n >= len(idx.Count) || idx.Count[n] == 0 {
			continue
		}
		b, err := e.st.ReadBlob(segName(n))
		if err != nil {
			continue
		}
		var recs []histRec
		if json.Unmarshal(b, &recs) != nil {
			continue
		}
		for i := len(recs) - 1; i >= 0 && len(out) < limit; i-- {
			r := recs[i]
			if r.Peer != want {
				continue
			}
			out = append(out, HistoryEntry{Peer: peer, ID: MsgID(r.ID), Mine: r.Mine, Text: r.Text, At: time.Unix(r.At, 0)})
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // collected newest first → oldest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ErrHistoryOff is returned when history is disabled.
var ErrHistoryOff = errors.New("core: history is off (config: history = true)")

// Burn deletes all stored history and every partial transfer.
func (e *Engine) Burn() (int, error) {
	e.histMu.Lock()
	defer e.histMu.Unlock()
	n := 0
	for _, sub := range []string{historyDir, partialsDir} {
		blobs, err := e.st.ListBlobs(sub)
		if err != nil {
			return n, err
		}
		for _, b := range blobs {
			if err := e.st.DeleteBlob(b); err != nil {
				return n, err
			}
			n++
		}
	}
	parts, _ := listParts(e.partialsPath())
	for _, p := range parts {
		if removeFile(p) == nil {
			n++
		}
	}
	return n, nil
}
