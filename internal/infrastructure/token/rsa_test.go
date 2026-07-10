package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/google/uuid"
)

// GenerateTestKeyPEM returns a fresh RSA private key in PKCS#8 PEM for tests.
func GenerateTestKeyPEM(t *testing.T) string {
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

func TestIssueAndParseRoundTrip(t *testing.T) {
	iss, err := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", 15*time.Minute)
	if err != nil {
		t.Fatalf("NewRSAIssuer: %v", err)
	}
	id := uuid.New()
	tok, exp, err := iss.IssueAccess(id, "user")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Error("expiry should be in the future")
	}
	gotID, gotRole, err := iss.ParseAccess(tok)
	if err != nil {
		t.Fatalf("ParseAccess: %v", err)
	}
	if gotID != id || gotRole != "user" {
		t.Errorf("round trip = %v/%q, want %v/user", gotID, gotRole, id)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	iss, _ := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", time.Minute)
	if _, _, err := iss.ParseAccess("not.a.jwt"); err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestJWKSExposesKey(t *testing.T) {
	iss, _ := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", time.Minute)
	jwks := iss.JWKS()
	keys, ok := jwks["keys"].([]map[string]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("JWKS keys malformed: %#v", jwks)
	}
	if keys[0]["kid"] != "kid-1" || keys[0]["kty"] != "RSA" {
		t.Errorf("unexpected jwk: %#v", keys[0])
	}
}
