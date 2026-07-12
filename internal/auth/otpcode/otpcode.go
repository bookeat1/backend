// Package otpcode generates 6-digit numeric OTPs and hashes them with sha256
// (codes are never stored in the clear), mirroring the Supabase edge function.
package otpcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const length = 6

// Generate returns a cryptographically random 6-digit code (zero-padded).
func Generate() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(b[:]) % 1_000_000
	return fmt.Sprintf("%0*d", length, n), nil
}

// Hash returns the hex sha256 of code.
func Hash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
