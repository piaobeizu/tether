package server

import (
	"crypto/rand"
	"encoding/hex"
)

// newID generates a random 8-byte hex string used as a unique client/session ID.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
