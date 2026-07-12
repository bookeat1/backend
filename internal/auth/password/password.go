// Package password wraps bcrypt. It verifies both $2a$ (Supabase/GoTrue) and
// $2b$ hashes, so passwords imported from Supabase keep working.
package password

import "golang.org/x/crypto/bcrypt"

// Hash returns a bcrypt hash of plain at the default cost.
func Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// Verify reports whether plain matches the bcrypt hash.
func Verify(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
