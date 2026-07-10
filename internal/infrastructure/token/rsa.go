// Package token issues and verifies RS256 access JWTs and exposes the public
// key as a JWKS document.
package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// GenerateTestKeyPEM returns a fresh RSA private key in PKCS#8 PEM for tests.
// Exported (and kept in a non-test file) so other packages' tests can reuse it.
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

// RSAIssuer signs access tokens with an RSA private key.
type RSAIssuer struct {
	key *rsa.PrivateKey
	kid string
	ttl time.Duration
}

// NewRSAIssuer parses a PKCS#8 (or PKCS#1) RSA private key PEM.
func NewRSAIssuer(privatePEM, kid string, ttl time.Duration) (*RSAIssuer, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("token: invalid private key PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("token: PKCS8 key is not RSA")
		}
		key = rk
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, fmt.Errorf("token: parse private key: %w", err)
	}
	return &RSAIssuer{key: key, kid: kid, ttl: ttl}, nil
}

// IssueAccess returns a signed token, its expiry, and any error.
func (i *RSAIssuer) IssueAccess(userID uuid.UUID, role string) (string, time.Time, error) {
	exp := time.Now().Add(i.ttl)
	claims := jwt.MapClaims{
		"sub":  userID.String(),
		"role": role,
		"iat":  time.Now().Unix(),
		"exp":  exp.Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = i.kid
	signed, err := t.SignedString(i.key)
	return signed, exp, err
}

// ParseAccess verifies signature + expiry and returns the subject and role.
func (i *RSAIssuer) ParseAccess(tok string) (uuid.UUID, string, error) {
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return &i.key.PublicKey, nil
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return uuid.Nil, "", errors.New("token: invalid claims")
	}
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, "", errors.New("token: invalid sub")
	}
	role, _ := claims["role"].(string)
	return id, role, nil
}

// JWKS returns the public key as a JWKS document for /.well-known/jwks.json.
func (i *RSAIssuer) JWKS() map[string]any {
	pub := i.key.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": i.kid, "n": n, "e": e,
		}},
	}
}
