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
	historyQueue   = 256 // records waiting for the history writer
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

	// flushed marks a request instead of a record: the writer closes it when
	// everything queued before it is on disk.
	flushed chan struct{}
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

// recordHistoryL queues one message for the history writer when history is
// on. Disk work never happens on the engine goroutine; if the disk falls a
// whole queue behind, the engine waits for it rather than lose a record.
func (e *Engine) recordHistoryL(peer PeerID, id MsgID, mine bool, text string, at time.Time) {
	if e.histCh == nil {
		return
	}
	rec := histRec{Peer: hex.EncodeToString(peer[:]), ID: uint64(id), Mine: mine, Text: text, At: at.Unix()}
	select {
	case e.histCh <- rec:
	case <-e.sessCtx.Done(): // shutting down: the writer has stopped
	}
}

// historyWriter is the one goroutine that writes history. It stops once the
// sessions have ended and what was queued is written.
func (e *Engine) historyWriter() {
	defer e.wg.Done()
	for {
		select {
		case rec := <-e.histCh:
			e.writeHistory(rec)
		case <-e.sessCtx.Done():
			for {
				select {
				case rec := <-e.histCh:
					e.writeHistory(rec)
				default:
					return
				}
			}
		}
	}
}

// writeHistory appends first and whatever else is already queued, one
// segment write per segment touched.
func (e *Engine) writeHistory(first histRec) {
	batch := []histRec{first}
	for len(batch) < historySegment {
		select {
		case rec := <-e.histCh:
			batch = append(batch, rec)
			continue
		default:
		}
		break
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
	save := func() bool {
		b, err := json.Marshal(recs)
		if err == nil {
			err = e.st.WriteBlob(segName(idx.Next), b)
		}
		if err != nil {
			e.log.Warn("history write failed", "err", err.Error())
			return false
		}
		for len(idx.Count) <= idx.Next {
			idx.Count = append(idx.Count, 0)
		}
		idx.Count[idx.Next] = len(recs)
		return true
	}
	dirty := false
	for _, rec := range batch {
		if rec.flushed != nil {
			defer close(rec.flushed) // after the writes below
			continue
		}
		recs, dirty = append(recs, rec), true
		if len(recs) == historySegment {
			if !save() {
				return
			}
			idx.Next++
			recs, dirty = nil, false
		}
	}
	if dirty && !save() {
		return
	}
	if ib, err := json.Marshal(idx); err == nil {
		_ = e.st.WriteBlob(historyIndex, ib)
	}
}

// flushHistory waits until every message recorded so far is on disk.
func (e *Engine) flushHistory() {
	if e.histCh == nil {
		return
	}
	flushed := make(chan struct{})
	select {
	case e.histCh <- histRec{flushed: flushed}:
		select {
		case <-flushed:
		case <-e.done:
		}
	case <-e.sessCtx.Done(): // the writer empties the queue before it stops
		e.wg.Wait()
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
	e.flushHistory()
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

// Burn deletes all stored history and every partial transfer that is not
// being written right now. It returns the number of files removed.
func (e *Engine) Burn() (int, error) {
	e.flushHistory()
	busy := map[string]bool{} // names of partials and sidecars of running downloads
	e.do(func() {
		for _, t := range e.transfers {
			if t.partPath != "" {
				busy[t.partPath], busy[e.metaRel(t.partID)] = true, true
			}
		}
	})
	e.histMu.Lock()
	defer e.histMu.Unlock()
	n := 0
	for _, sub := range []string{historyDir, partialsDir} {
		blobs, err := e.st.ListBlobs(sub)
		if err != nil {
			return n, err
		}
		for _, b := range blobs {
			if busy[b] {
				continue
			}
			if err := e.st.DeleteBlob(b); err != nil {
				return n, err
			}
			n++
		}
	}
	parts, _ := listParts(e.partialsPath())
	for _, p := range parts {
		if !busy[p] && removeFile(p) == nil {
			n++
		}
	}
	return n, nil
}
