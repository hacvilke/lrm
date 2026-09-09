package daemon

import "crypto/sha256"

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	out := make([]byte, 32)
	copy(out, sum[:])
	return out
}
