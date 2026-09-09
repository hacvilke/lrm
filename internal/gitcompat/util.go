package gitcompat

import (
	"crypto/sha256"
	"encoding/json"
)

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	out := make([]byte, 32)
	copy(out, sum[:])
	return out
}
