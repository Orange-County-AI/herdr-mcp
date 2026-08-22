package access

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	assertionHeader = "Cf-Access-Jwt-Assertion"
	maxJWKSBytes    = 1 << 20
	keyCacheTTL     = time.Hour
)

// Validator verifies the signed identity assertion Cloudflare Access forwards.
type Validator struct {
	issuer   string
	audience string
	certsURL string
	client   *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyType string `json:"kty"`
	KeyID   string `json:"kid"`
	Use     string `json:"use"`
	N       string `json:"n"`
	E       string `json:"e"`
}

// NewValidator creates a validator for one Access application audience.
func NewValidator(teamDomain, audience string) (*Validator, error) {
	teamDomain = strings.TrimSpace(teamDomain)
	audience = strings.TrimSpace(audience)
	if teamDomain == "" || audience == "" {
		return nil, fmt.Errorf("Cloudflare Access team domain and audience are both required")
	}
	if !strings.Contains(teamDomain, "://") {
		teamDomain = "https://" + teamDomain
	}
	parsed, err := url.Parse(teamDomain)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid Cloudflare Access team domain %q", teamDomain)
	}
	if parsed.Scheme != "https" && !isLoopback(parsed.Hostname()) {
		return nil, fmt.Errorf("Cloudflare Access team domain must use https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	issuer := strings.TrimRight(parsed.String(), "/")
	return &Validator{
		issuer:   issuer,
		audience: audience,
		certsURL: issuer + "/cdn-cgi/access/certs",
		client:   &http.Client{Timeout: 5 * time.Second},
		keys:     make(map[string]*rsa.PublicKey),
	}, nil
}

// Middleware requires a valid Access assertion on every wrapped request.
func (v *Validator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertion := strings.TrimSpace(r.Header.Get(assertionHeader))
		if assertion == "" {
			http.Error(w, "missing Cloudflare Access assertion", http.StatusUnauthorized)
			return
		}
		if err := v.Validate(r.Context(), assertion); err != nil {
			http.Error(w, "invalid Cloudflare Access assertion", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Validate checks signature, issuer, audience, and registered time claims.
func (v *Validator) Validate(ctx context.Context, assertion string) error {
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(assertion, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected signing algorithm %q", token.Method.Alg())
		}
		keyID, ok := token.Header["kid"].(string)
		if !ok || keyID == "" {
			return nil, fmt.Errorf("token has no key id")
		}
		return v.key(ctx, keyID)
	},
		jwt.WithAudience(v.audience),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
	)
	if err != nil {
		return fmt.Errorf("verify Access JWT: %w", err)
	}
	if !token.Valid {
		return fmt.Errorf("verify Access JWT: token is invalid")
	}
	return nil
}

func (v *Validator) key(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if time.Now().Before(v.expires) {
		if key := v.keys[keyID]; key != nil {
			return key, nil
		}
	}
	if err := v.refreshLocked(ctx); err != nil {
		return nil, err
	}
	if key := v.keys[keyID]; key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("Access signing key %q is not published", keyID)
}

func (v *Validator) refreshLocked(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return fmt.Errorf("build Access certs request: %w", err)
	}
	response, err := v.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch Access certs: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("fetch Access certs: HTTP %d", response.StatusCode)
	}
	var document jwksDocument
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode Access certs: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, encoded := range document.Keys {
		if encoded.KeyType != "RSA" || encoded.KeyID == "" {
			continue
		}
		key, err := rsaKey(encoded)
		if err != nil {
			return fmt.Errorf("decode Access signing key %q: %w", encoded.KeyID, err)
		}
		keys[encoded.KeyID] = key
	}
	if len(keys) == 0 {
		return fmt.Errorf("Access certs document contains no RSA signing keys")
	}
	v.keys = keys
	v.expires = time.Now().Add(keyCacheTTL)
	return nil
}

func rsaKey(encoded jwk) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(encoded.N)
	if err != nil || len(modulus) == 0 {
		return nil, fmt.Errorf("invalid modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(encoded.E)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return nil, fmt.Errorf("invalid exponent")
	}
	exponent := 0
	for _, value := range exponentBytes {
		exponent = exponent<<8 | int(value)
	}
	if exponent < 3 || exponent%2 == 0 {
		return nil, fmt.Errorf("invalid exponent %d", exponent)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
