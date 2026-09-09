package cas

import (
	"bytes"
	"io"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("lrm content addressed storage")
	h, n, err := s.PutReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) {
		t.Fatalf("n=%d want %d", n, len(data))
	}
	if !s.Exists(h) {
		t.Fatal("object missing after put")
	}
	rc, err := s.Get(h)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("roundtrip mismatch")
	}
	// Idempotent re-put.
	h2, err := s.PutBytes(data)
	if err != nil || h2 != h {
		t.Fatal("idempotent put failed")
	}
}

func TestStreamingLarge(t *testing.T) {
	s, _ := New(t.TempDir())
	// 5 MiB pseudo-random stream (never fully in memory on the store side).
	r := &patternReader{n: 5 << 20}
	h, n, err := s.PutReader(r)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5<<20 {
		t.Fatalf("n=%d", n)
	}
	sz, err := s.Stat(h)
	if err != nil || sz != n {
		t.Fatalf("stat=%d err=%v", sz, err)
	}
}

type patternReader struct {
	n int
	i int
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.i >= p.n {
		return 0, io.EOF
	}
	for i := range b {
		if p.i >= p.n {
			return i, nil
		}
		b[i] = byte(p.i * 31)
		p.i++
	}
	return len(b), nil
}

func TestParsePrefix(t *testing.T) {
	s, _ := New(t.TempDir())
	h, _ := s.PutBytes([]byte("prefix-resolution"))
	got, err := s.Parse(Hex(h)[:12])
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatal("prefix resolve mismatch")
	}
}
