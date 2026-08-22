package access

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestValidatorChecksAccessAssertion(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jwksDocument{Keys: []jwk{{
			KeyType: "RSA",
			KeyID:   "key-1",
			Use:     "sig",
			N:       base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			E:       base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.E)).Bytes()),
		}}})
	}))
	defer server.Close()
	issuer = server.URL

	validator, err := NewValidator(issuer, "access-audience")
	if err != nil {
		t.Fatal(err)
	}
	assertion := signedAssertion(t, privateKey, "key-1", issuer, "access-audience")
	if err := validator.Validate(context.Background(), assertion); err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(context.Background(), assertion); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("JWKS fetches = %d, want 1", fetches.Load())
	}

	wrongAudience := signedAssertion(t, privateKey, "key-1", issuer, "other-audience")
	if err := validator.Validate(context.Background(), wrongAudience); err == nil {
		t.Fatal("wrong audience was accepted")
	}
}

func TestMiddlewareRequiresAssertion(t *testing.T) {
	validator := &Validator{}
	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestNewValidatorRejectsIncompleteOrInsecureConfig(t *testing.T) {
	for _, test := range []struct {
		domain   string
		audience string
	}{
		{"", "aud"},
		{"team.cloudflareaccess.com", ""},
		{"http://example.com", "aud"},
	} {
		if _, err := NewValidator(test.domain, test.audience); err == nil {
			t.Fatalf("NewValidator(%q, %q) succeeded", test.domain, test.audience)
		}
	}
}

func signedAssertion(t *testing.T, key *rsa.PrivateKey, keyID, issuer, audience string) string {
	t.Helper()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
	})
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
