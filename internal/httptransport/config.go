// Package httptransport owns listener, TLS, authentication and browser guards.
// It is independent of Herdr so configuration fails before session startup.
package httptransport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/Orange-County-AI/herdr-mcp/internal/access"
)

// Config contains source locations, never a bearer credential. LookupEnv is
// injectable so service validation can use its environment file without
// changing the parent process's environment.
type Config struct {
	Listen          string
	AllowPrivate    bool
	BearerTokenFile string
	TLSCertFile     string
	TLSKeyFile      string
	AllowedHosts    []string
	AccessTeam      string
	AccessAudience  string
	LookupEnv       func(string) (string, bool)
	BearerFileSet   bool
	TLSSet          bool
	Explicit        map[string]bool
	RequireAuth     bool
}

// Transport is immutable after startup. Credential and certificate rotation
// requires a restart, terminating old HTTP sessions and streams.
type Transport struct {
	TLSConfig *tls.Config
	private   bool
	bearer    *[sha256.Size]byte
	access    *access.Validator
	hosts     map[string]bool
}

func New(c Config) (*Transport, error) {
	t := &Transport{hosts: make(map[string]bool)}
	if c.LookupEnv == nil {
		c.LookupEnv = os.LookupEnv
	}
	if _, exists := c.LookupEnv("HERDR_MCP_BEARER_TOKEN_FILE"); (exists || c.BearerFileSet) && c.BearerTokenFile == "" {
		return nil, fmt.Errorf("configured bearer secret file path is empty")
	}
	token, configured := c.LookupEnv("HERDR_MCP_BEARER_TOKEN")
	if c.BearerTokenFile != "" && configured {
		return nil, fmt.Errorf("choose one bearer source: secret file or environment")
	}
	bearerConfigured := c.BearerTokenFile != "" || configured
	if bearerConfigured && (c.AccessTeam != "" || c.AccessAudience != "") {
		return nil, fmt.Errorf("bearer and Cloudflare Access configuration are mutually exclusive")
	}
	if c.BearerTokenFile != "" {
		var err error
		token, err = readSecret(c.BearerTokenFile)
		if err != nil {
			return nil, err
		}
	}
	if bearerConfigured {
		if !validToken(token) {
			return nil, fmt.Errorf("bearer secret must be 32-4096 ASCII token characters without whitespace")
		}
		hash := sha256.Sum256([]byte(token))
		t.bearer = &hash
	}
	if c.AccessTeam != "" || c.AccessAudience != "" {
		var err error
		t.access, err = access.NewValidator(c.AccessTeam, c.AccessAudience)
		if err != nil {
			return nil, err
		}
	}
	if c.RequireAuth && !bearerConfigured && t.access == nil {
		return nil, fmt.Errorf("this service requires configured authentication")
	}
	_, envCert := c.LookupEnv("HERDR_MCP_TLS_CERT_FILE")
	_, envKey := c.LookupEnv("HERDR_MCP_TLS_KEY_FILE")
	if ((c.TLSSet || envCert || envKey) && (c.TLSCertFile == "" || c.TLSKeyFile == "")) || (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, fmt.Errorf("TLS certificate and private-key files are both required")
	}
	if c.TLSCertFile != "" {
		certPEM, err := os.ReadFile(c.TLSCertFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS certificate: %w", err)
		}
		keyPEM, err := ReadOwnerFile(c.TLSKeyFile, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("read TLS private key: %w", err)
		}
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate/key: %w", err)
		}
		t.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("listen port must be numeric in 1-65535")
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback {
		if ip == nil || !ip.IsPrivate() {
			return nil, fmt.Errorf("listen must use loopback or an explicit private IP; wildcard and public addresses are refused")
		}
		if !c.AllowPrivate {
			return nil, fmt.Errorf("private listener requires --allow-private")
		}
		if t.bearer == nil {
			return nil, fmt.Errorf("private listener requires native bearer authentication")
		}
		if t.TLSConfig == nil {
			return nil, fmt.Errorf("private bearer listener requires native HTTPS certificate and key")
		}
		t.private = true
	}
	bindAuthority, err := normalizeAuthority(c.Listen, t.requestScheme())
	if err != nil {
		return nil, err
	}
	t.hosts[bindAuthority] = true
	if loopback {
		for _, alias := range []string{"localhost", "127.0.0.1", "::1"} {
			authority, _ := normalizeAuthority(net.JoinHostPort(alias, port), t.requestScheme())
			t.hosts[authority] = true
		}
	}
	for _, authority := range c.AllowedHosts {
		normalized, err := normalizeAuthority(authority, t.requestScheme())
		if err != nil {
			return nil, fmt.Errorf("invalid allowed Host authority: %w", err)
		}
		if t.bearer == nil && t.access == nil && !loopbackAuthority(normalized) {
			return nil, fmt.Errorf("non-loopback allowed Host requires authentication")
		}
		t.hosts[normalized] = true
	}
	return t, nil
}

// ReadOwnerFile opens without following symlinks and checks/reads the same
// inode. It is shared with installer credential-file preflight.
func ReadOwnerFile(path string, maxBytes int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open owner-only file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat owner-only file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("file must be regular, owned by the service user, and inaccessible to group/others")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read owner-only file: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("owner-only file exceeds size limit")
	}
	return data, nil
}

func readSecret(path string) (string, error) {
	bytes, err := ReadOwnerFile(path, 4098)
	if err != nil {
		return "", fmt.Errorf("bearer secret file: %w", err)
	}
	token := string(bytes)
	if strings.HasSuffix(token, "\n") {
		token = strings.TrimSuffix(token, "\n")
		token = strings.TrimSuffix(token, "\r")
	}
	return token, nil
}

func validToken(token string) bool {
	if len(token) < 32 || len(token) > 4096 {
		return false
	}
	padding := false
	for _, ch := range token {
		if ch == '=' {
			padding = true
			continue
		}
		if padding {
			return false
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._~+/", ch) {
			continue
		}
		return false
	}
	// Padding alone is not a token.
	return token[0] != '='
}

func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	return token, found && strings.EqualFold(scheme, "Bearer") && validToken(token)
}

func normalizeAuthority(authority, scheme string) (string, error) {
	if strings.TrimSpace(authority) != authority || strings.ContainsAny(authority, "/\\?#@%*\r\n\t ") || strings.HasSuffix(authority, ":") {
		return "", fmt.Errorf("expected hostname or IP with optional port")
	}
	parsed, err := url.Parse("http://" + authority)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return "", fmt.Errorf("expected hostname or IP with optional port")
	}
	if strings.Contains(parsed.Hostname(), ":") && net.ParseIP(parsed.Hostname()) == nil {
		return "", fmt.Errorf("invalid IP")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 || value > 65535 {
			return "", fmt.Errorf("invalid port")
		}
	}
	if (scheme == "http" && parsed.Port() == "80") || (scheme == "https" && parsed.Port() == "443") {
		hostname := parsed.Hostname()
		if strings.Contains(hostname, ":") {
			hostname = "[" + hostname + "]"
		}
		return strings.ToLower(hostname), nil
	}
	return strings.ToLower(authority), nil
}

// Guard must wrap the entire mux, not only POST or session creation. A session
// ID never substitutes for credentials. GET /healthz is the only auth exemption;
// private or tunnel probes receive only {ok}, and never session diagnostics.
func (t *Transport) Guard(next http.Handler, minimalHealth http.Handler) http.Handler {
	guarded := t.browserGuard(next)
	if t.access != nil {
		guarded = t.access.Middleware(guarded)
	}
	if t.bearer != nil {
		afterAuth := guarded
		guarded = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers := r.Header.Values("Authorization")
			var token string
			var valid bool
			if len(headers) == 1 {
				token, valid = bearerToken(headers[0])
			}
			candidate := sha256.Sum256([]byte(token))
			matches := subtle.ConstantTimeCompare(candidate[:], t.bearer[:])
			if !valid || matches != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="herdr-mcp"`)
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			afterAuth.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet && minimalHealth != nil {
			health := minimalHealth
			// Keep legacy local diagnostics only for direct loopback health probes.
			peer, _, err := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(peer)
			if t.bearer == nil && t.access == nil && !t.private && err == nil && ip != nil && ip.IsLoopback() && loopbackAuthority(r.Host) {
				health = next
			}
			t.browserGuard(health).ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

func loopbackAuthority(authority string) bool {
	parsed, err := url.Parse("http://" + authority)
	if err != nil {
		return false
	}
	ip := net.ParseIP(parsed.Hostname())
	return parsed.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func (t *Transport) browserGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, err := normalizeAuthority(r.Host, t.requestScheme())
		if err != nil || !t.hosts[host] {
			http.Error(w, "invalid Host", http.StatusForbidden)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) != 0 {
			if len(origins) != 1 {
				http.Error(w, "invalid Origin", http.StatusForbidden)
				return
			}
			origin, err := url.Parse(origins[0])
			scheme := t.requestScheme()
			originHost := ""
			if err == nil {
				originHost, _ = normalizeAuthority(origin.Host, scheme)
			}
			if err != nil || origin.Scheme != scheme || originHost != host || origin.User != nil || origin.Path != "" || strings.ContainsAny(origins[0], "?#") {
				http.Error(w, "invalid Origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (t *Transport) Scheme() string {
	if t.TLSConfig != nil {
		return "https"
	}
	return "http"
}

// AuthMode reports the resolved policy for startup diagnostics, without source
// locations or credentials.
func (t *Transport) AuthMode() string {
	if t.bearer != nil {
		return "bearer required"
	}
	if t.access != nil {
		return "Cloudflare Access JWT required"
	}
	return "anonymous loopback"
}

// Serve uses Go's standard TLS server with the prevalidated certificate. There
// is no fallback to plaintext if TLS startup or a handshake fails.
func (t *Transport) Serve(server *http.Server, listener net.Listener) error {
	if t.TLSConfig != nil {
		server.TLSConfig = t.TLSConfig.Clone()
		return server.ServeTLS(listener, "", "")
	}
	return server.Serve(listener)
}

// Access's trusted tunnel terminates HTTPS externally. Other modes use only
// their configured native scheme; arbitrary forwarded headers never decide it.
func (t *Transport) requestScheme() string {
	if t.access != nil {
		return "https"
	}
	return t.Scheme()
}

// VerifyCertificate validates service usability before the installer changes
// anything. Nil roots means the system trust store, never a verification bypass.
func (t *Transport) VerifyCertificate(host string, roots *x509.CertPool) error {
	if t.TLSConfig == nil {
		return nil
	}
	chain := t.TLSConfig.Certificates[0].Certificate
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return fmt.Errorf("parse server certificate: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, der := range chain[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("parse certificate chain: %w", err)
		}
		intermediates.AddCert(cert)
	}
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: intermediates})
	if err != nil {
		return fmt.Errorf("verify service certificate for bind address: %w", err)
	}
	return nil
}
