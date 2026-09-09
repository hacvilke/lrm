package transport

import "crypto/ed25519"

func ed25519Verify(pub, msg, sig []byte) bool {
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}
