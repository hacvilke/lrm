// Package mux implements the Streaming Multiplexer: many concurrent
// logical binary streams over a single (secure) connection.
//
// Wire frame: [streamID:4 BE][flags:1][len:4 BE][payload]
// Flags: 0x01=DATA, 0x02=FIN (graceful close), 0x04=RST (abort).
//
// Example: stream file chunks on streams 1..N while simultaneously
// exchanging graph have/want state on stream 0.
package mux

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

const (
	flagDATA = 0x01
	flagFIN  = 0x02
	flagRST  = 0x04

	maxPayload = 32 * 1024
)

// Stream is one logical stream.
type Stream struct {
	id      uint32
	sess    *Session
	readCh  chan []byte
	readBuf []byte
	closed  atomic.Bool
	finSeen atomic.Bool
	closeMu sync.Mutex
	err     error
}

// ID returns the stream id.
func (s *Stream) ID() uint32 { return s.id }

// Read implements io.Reader.
func (s *Stream) Read(p []byte) (int, error) {
	for len(s.readBuf) == 0 {
		if s.finSeen.Load() {
			return 0, io.EOF
		}
		if s.err != nil {
			return 0, s.err
		}
		chunk, ok := <-s.readCh
		if !ok {
			return 0, io.EOF
		}
		if chunk == nil { // FIN marker
			s.finSeen.Store(true)
			return 0, io.EOF
		}
		s.readBuf = chunk
	}
	n := copy(p, s.readBuf)
	s.readBuf = s.readBuf[n:]
	return n, nil
}

// Write implements io.Writer (chunked to maxPayload).
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("write to closed stream %d", s.id)
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxPayload {
			n = maxPayload
		}
		if err := s.sess.writeFrame(s.id, flagDATA, p[:n]); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// Close gracefully closes (sends FIN).
func (s *Stream) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	_ = s.sess.writeFrame(s.id, flagFIN, nil)
	s.sess.forget(s.id)
	return nil
}

// Reset aborts the stream.
func (s *Stream) Reset() error {
	s.closed.Store(true)
	_ = s.sess.writeFrame(s.id, flagRST, nil)
	s.sess.forget(s.id)
	return nil
}

// Session multiplexes streams over one conn.
type Session struct {
	conn    net.Conn
	writeMu sync.Mutex

	streamsMu sync.Mutex
	streams   map[uint32]*Stream
	nextID    atomic.Uint32

	acceptCh chan *Stream
	closed   atomic.Bool
	err      error
}

// NewSession starts muxing over conn. Odd ids are locally-initiated when
// initiator=true, even otherwise (prevents collision).
func NewSession(conn net.Conn, initiator bool) *Session {
	s := &Session{
		conn: conn, streams: map[uint32]*Stream{},
		acceptCh: make(chan *Stream, 64),
	}
	if initiator {
		s.nextID.Store(1)
	} else {
		s.nextID.Store(2)
	}
	go s.readLoop()
	return s
}

// OpenStream creates a new outbound stream.
func (s *Session) OpenStream() (*Stream, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("session closed")
	}
	id := s.nextID.Add(2) - 2
	st := &Stream{id: id, sess: s, readCh: make(chan []byte, 16)}
	s.streamsMu.Lock()
	s.streams[id] = st
	s.streamsMu.Unlock()
	return st, nil
}

// AcceptStream blocks for the next inbound stream.
func (s *Session) AcceptStream() (*Stream, error) {
	st, ok := <-s.acceptCh
	if !ok {
		return nil, s.errOr(io.EOF)
	}
	return st, nil
}

// Close shuts the session and underlying conn.
func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.acceptCh)
	return s.conn.Close()
}

func (s *Session) errOr(def error) error {
	if s.err != nil {
		return s.err
	}
	return def
}

func (s *Session) forget(id uint32) {
	s.streamsMu.Lock()
	delete(s.streams, id)
	s.streamsMu.Unlock()
}

func (s *Session) writeFrame(id uint32, flags byte, payload []byte) error {
	hdr := make([]byte, 9)
	binary.BigEndian.PutUint32(hdr[0:4], id)
	hdr[4] = flags
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.conn.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := s.conn.Write(payload)
		return err
	}
	return nil
}

func (s *Session) readLoop() {
	defer s.Close()
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(s.conn, hdr); err != nil {
			s.err = err
			return
		}
		id := binary.BigEndian.Uint32(hdr[0:4])
		flags := hdr[4]
		ln := binary.BigEndian.Uint32(hdr[5:9])
		if ln > maxPayload {
			s.err = fmt.Errorf("mux payload too large (%d)", ln)
			return
		}
		var payload []byte
		if ln > 0 {
			payload = make([]byte, ln)
			if _, err := io.ReadFull(s.conn, payload); err != nil {
				s.err = err
				return
			}
		}
		s.dispatch(id, flags, payload)
	}
}

func (s *Session) dispatch(id uint32, flags byte, payload []byte) {
	s.streamsMu.Lock()
	st, ok := s.streams[id]
	if !ok {
		if flags&flagFIN != 0 || flags&flagRST != 0 {
			s.streamsMu.Unlock()
			return
		}
		st = &Stream{id: id, sess: s, readCh: make(chan []byte, 16)}
		s.streams[id] = st
		s.streamsMu.Unlock()
		select {
		case s.acceptCh <- st:
		default:
			// Slow acceptor: drop (peer will see missing FIN/timeout).
		}
	} else {
		s.streamsMu.Unlock()
	}
	switch {
	case flags&flagRST != 0:
		st.err = fmt.Errorf("stream %d reset by peer", id)
		close(st.readCh)
		s.forget(id)
	case flags&flagFIN != 0:
		select {
		case st.readCh <- nil:
		default:
		}
		if len(payload) > 0 {
			// Deliver final data first: requeue trick — prepend via extra send.
			// (FIN-with-data is rare; deliver data then FIN.)
			select {
			case st.readCh <- payload:
				select {
				case st.readCh <- nil:
				default:
				}
			default:
			}
		}
	default: // DATA
		if len(payload) > 0 {
			st.readCh <- payload
		}
	}
}
