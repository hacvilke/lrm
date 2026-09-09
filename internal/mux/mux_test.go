package mux

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
)

func TestConcurrentStreams(t *testing.T) {
	a, b := net.Pipe()
	sa := NewSession(a, true)
	sb := NewSession(b, false)
	defer sa.Close()
	defer sb.Close()

	const n = 8
	var wg sync.WaitGroup
	// Receiver: accept n streams and echo lengths.
	got := make([][]byte, n)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			st, err := sb.AcceptStream()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			data, _ := io.ReadAll(st)
			got[i] = data
			_ = st.Close()
		}
	}()
	payloads := make([][]byte, n)
	for i := 0; i < n; i++ {
		payloads[i] = bytes.Repeat([]byte{byte(i + 1)}, 1000+i*100)
	}
	var wwg sync.WaitGroup
	for i := 0; i < n; i++ {
		wwg.Add(1)
		go func(i int) {
			defer wwg.Done()
			st, err := sa.OpenStream()
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			if _, err := st.Write(payloads[i]); err != nil {
				t.Errorf("write: %v", err)
			}
			_ = st.Close()
		}(i)
	}
	wwg.Wait()
	wg.Wait()
	total := 0
	for _, g := range got {
		total += len(g)
	}
	want := 0
	for _, p := range payloads {
		want += len(p)
	}
	if total != want {
		t.Fatalf("total=%d want=%d", total, want)
	}
}
