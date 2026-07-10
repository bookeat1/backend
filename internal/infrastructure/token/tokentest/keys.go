// Package tokentest provides RSA key material for tests of the token package
// and its consumers. It lives in its own package (not a _test.go file) so that
// tests in other packages can import it without the production token package
// depending on the testing package.
package tokentest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// GenerateKeyPEM returns a fresh RSA private key in PKCS#8 PEM for tests.
func GenerateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
