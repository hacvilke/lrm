package sync

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestPacedReaderDirect(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 512*1024)
	start := time.Now()
	n, err := io.ReadFull(&pacedReader{r: bytes.NewReader(data), rate: 128 * 1024}, make([]byte, 512*1024))
	if err != nil || n != 512*1024 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	d := time.Since(start)
	t.Logf("512 KiB at 128 KiB/s took %s", d)
	if d < 2*time.Second {
		t.Fatalf("no pacing: %s", d)
	}
}
