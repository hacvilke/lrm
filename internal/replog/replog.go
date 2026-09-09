// Package replog implements the replication log: an append-only,
// per-commit record of what each peer has seen/shared. It is the
// "Kafka/Raft-lite" audit trail that lets LRM identify exactly when
// history diverges.
package replog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/vectorclock"
)

// EntryType classifies log records.
type EntryType string

const (
	TypeCommit   EntryType = "commit"
	TypeFetch    EntryType = "fetch"
	TypePush     EntryType = "push"
	TypeMerge    EntryType = "merge"
	TypeConflict EntryType = "conflict"
	TypeBranch   EntryType = "branch"
)

// Entry is one replication event.
type Entry struct {
	Seq       uint64            `json:"seq"`
	Time      int64             `json:"time"`
	Type      EntryType         `json:"type"`
	PeerHex   string            `json:"peer"`
	Commit    string            `json:"commit"`
	Message   string            `json:"message,omitempty"`
	Clock     vectorclock.Clock `json:"clock,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// Log is an append-only JSONL log.
type Log struct {
	mu   sync.Mutex
	path string
	seq  uint64
	f    *os.File
}

// Open creates/opens the log at path (append mode).
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{path: path, f: f}
	// Recover seq by counting existing lines (streaming, bounded memory).
	l.seq = countLines(path)
	return l, nil
}

func countLines(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var n uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		n++
	}
	return n
}

// Append adds an entry (assigns Seq + Time).
func (l *Log) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if e.Time == 0 {
		e.Time = time.Now().UnixNano()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	raw = append(raw, '\n')
	if _, err := l.f.Write(raw); err != nil {
		return e, fmt.Errorf("append replog: %w", err)
	}
	return e, nil
}

// Close flushes and closes.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// ReadLast returns up to n most recent entries (streaming scan from head;
// memory bounded by n).
func (l *Log) ReadLast(n int) ([]Entry, error) {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	ring := make([]Entry, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if len(ring) < n {
			ring = append(ring, e)
		} else if n > 0 {
			copy(ring, ring[1:])
			ring[n-1] = e
		}
	}
	return ring, sc.Err()
}

// ScanAll streams every entry to fn (for sync / export).
func (l *Log) ScanAll(fn func(Entry) error) error {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}
