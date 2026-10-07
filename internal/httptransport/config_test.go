package httptransport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsafeListeners(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8091", "[::]:8091", "8.8.8.8:8091", "example.com:8091", "192.168.1.8:8091", "127.0.0.1:bad"} {
		t.Run(address, func(t *testing.T) {
			if _, err := New(Config{Listen: address}); err == nil {
				t.Fatal("unsafe listener accepted")
			}
		})
	}
}

// Arbitrary header strings must never authenticate unless they contain the
// exact owner-provisioned credential with a valid case-insensitive scheme.
func FuzzBearerHeader(f *testing.F) {
	for _, seed := range []string{"", "Bearer ", "Bearer abc", "Basic abc", "Bearer abc,def", "Bearer abc\n", "Bearer " + testSecret, "bearer " + testSecret} {
		f.Add(seed)
	}
	transport, err := New(bearerConfig("127.0.0.1:8091"))
	if err != nil {
		f.Fatal(err)
	}
	handler := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	f.Fuzz(func(t *testing.T, header string) {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := 401
		if len(header) == len("Bearer ")+len(testSecret) && strings.EqualFold(header[:len("Bearer")], "Bearer") && header[len("Bearer"):] == " "+testSecret {
			want = 204
		}
		if rec.Code != want {
			t.Fatalf("arbitrary credential status=%d want=%d", rec.Code, want)
		}
	})
}

const testSecret = "test_only_0123456789abcdefghijklmnop"
const wrongSecret = "wrong_only_0123456789abcdefghijklmn"

func noEnv(string) (string, bool) { return "", false }
func TestRequiredAuthenticationSurvivesMissingCredentialSource(t *testing.T) {
	config := bearerConfig("127.0.0.1:8091")
	config.RequireAuth = true
	if _, err := New(config); err != nil {
		t.Fatal(err)
	}
	config.LookupEnv = noEnv
	if _, err := New(config); err == nil {
		t.Fatal("authenticated service started after its credential source disappeared")
	}
}

func TestAuthenticationMode(t *testing.T) {
	for _, tc := range []struct {
		config Config
		want   string
	}{
		{Config{Listen: "127.0.0.1:8091", LookupEnv: noEnv}, "anonymous loopback"},
		{bearerConfig("127.0.0.1:8091"), "bearer required"},
		{Config{Listen: "127.0.0.1:8091", AccessTeam: "https://test.cloudflareaccess.com", AccessAudience: "test", AllowedHosts: []string{"mcp.tunnel.test"}, LookupEnv: noEnv}, "Cloudflare Access JWT required"},
	} {
		transport, err := New(tc.config)
		if err != nil {
			t.Fatal(err)
		}
		if got := transport.AuthMode(); got != tc.want {
			t.Fatalf("mode=%q want=%q", got, tc.want)
		}
	}
}

func bearerConfig(listen string) Config {
	return Config{Listen: listen, LookupEnv: func(name string) (string, bool) { return testSecret, name == "HERDR_MCP_BEARER_TOKEN" }}
}

func TestBearerEveryMethodAndPath(t *testing.T) {
	transport, err := New(bearerConfig("127.0.0.1:8091"))
	if err != nil {
		t.Fatal(err)
	}
	reached := 0
	handler := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(204) }), nil)
	for _, method := range []string{"GET", "POST", "DELETE", "PUT", "PATCH", "OPTIONS", "HEAD"} {
		for _, path := range []string{"/mcp", "/mcp/", "/mcp/../mcp", "/.well-known/oauth-authorization-server", "/version", "/healthz"} {
			for _, header := range []string{"", "Bearer " + wrongSecret, "Basic " + testSecret, "Bearer ", "Bearer " + testSecret + ",junk", "Bearer  " + testSecret, "Bearer " + testSecret + "\t", "Bearer " + testSecret, "bearer " + testSecret} {
				req := httptest.NewRequest(method, "http://127.0.0.1:8091"+path, nil)
				req.Header.Set("Authorization", header)
				req.Header.Set("Mcp-Session-Id", "previously-authorized-session")
				req.Header.Set("Last-Event-ID", "stream:42")
				req.Header.Set("Forwarded", "for=127.0.0.1;proto=https;host=localhost")
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				req.Header.Set("Cf-Access-Jwt-Assertion", "not-a-bearer-substitute")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				want := 401
				if strings.EqualFold(header, "Bearer "+testSecret) {
					want = 204
				}
				if rec.Code != want {
					t.Fatalf("%s %s status=%d want=%d", method, path, rec.Code, want)
				}
				if want == 401 && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer ") {
					t.Fatal("missing challenge")
				}
				if strings.Contains(rec.Body.String(), testSecret) || strings.Contains(rec.Body.String(), wrongSecret) {
					t.Fatal("credential leaked")
				}
			}
		}
	}
	if reached != 7*6*2 {
		t.Fatalf("downstream reached %d times", reached)
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
	req.Header.Add("Authorization", "Bearer "+testSecret)
	req.Header.Add("Authorization", "Bearer "+testSecret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("duplicate Authorization accepted")
	}
}

func TestHostAndOriginDefenses(t *testing.T) {
	config := bearerConfig("127.0.0.1:8091")
	config.AllowedHosts = []string{"mcp.private.test:8091"}
	transport, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	for _, tc := range []struct {
		host, origin string
		status       int
	}{
		{"127.0.0.1:8091", "", 204}, {"localhost:8091", "http://localhost:8091", 204},
		{"[::1]:8091", "http://[::1]:8091", 204}, {"mcp.private.test:8091", "http://mcp.private.test:8091", 204},
		{"attacker.test:8091", "", 403}, {"localhost:9999", "", 403},
		{"127.0.0.1:8091", "http://attacker.test:8091", 403}, {"127.0.0.1:8091", "http://localhost:8091", 403},
		{"127.0.0.1:8091", "https://127.0.0.1:8091", 403}, {"127.0.0.1:8091", "null", 403},
		{"127.0.0.1:8091", "http://127.0.0.1:8091/path", 403}, {"127.0.0.1:8091", "http://user@127.0.0.1:8091", 403},
		{"127.0.0.1:8091", "http://127.0.0.1:8091?x=1", 403}, {"127.0.0.1:8091", "http://127.0.0.1:8091#x", 403},
	} {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
		req.Host = tc.host
		req.Header.Set("Authorization", "Bearer "+testSecret)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		req.Header.Set("X-Forwarded-Host", "localhost:8091")
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("host=%s origin=%s status=%d want=%d", tc.host, tc.origin, rec.Code, tc.status)
		}
		req.Header.Del("Authorization")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatal("browser validation bypassed authentication")
		}
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Add("Origin", "http://127.0.0.1:8091")
	req.Header.Add("Origin", "http://127.0.0.1:8091")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("duplicate Origin accepted")
	}
	for _, host := range []string{"*", "https://mcp.test", "user@mcp.test", "mcp.test/path", "mcp.test:bad", " mcp.test"} {
		config.AllowedHosts = []string{host}
		if _, err := New(config); err == nil {
			t.Fatalf("bad allowlist entry accepted %q", host)
		}
	}
}

func TestHealthPolicy(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		config := Config{Listen: "127.0.0.1:8091", LookupEnv: noEnv, AllowedHosts: []string{"tunnel.test"}}
		if authenticated {
			config.LookupEnv = bearerConfig("").LookupEnv
		} else {
			config.AllowedHosts = nil
		}
		transport, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		handler := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"ok":true,"details":"local"}`) }), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"ok":true}`) }))
		for _, host := range []string{"127.0.0.1:8091", "tunnel.test"} {
			req := httptest.NewRequest("GET", "http://127.0.0.1:8091/healthz", nil)
			req.Host = host
			req.RemoteAddr = "127.0.0.1:4321"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if !authenticated && host == "tunnel.test" {
				if rec.Code != 403 {
					t.Fatal("unauthenticated tunnel Host accepted")
				}
				continue
			}
			if rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			detailed := !authenticated && host == "127.0.0.1:8091"
			if strings.Contains(rec.Body.String(), "details") != detailed {
				t.Fatal("health details exposed")
			}
		}
		if authenticated {
			for _, method := range []string{"POST", "HEAD", "OPTIONS", "DELETE"} {
				req := httptest.NewRequest(method, "http://127.0.0.1:8091/healthz", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 401 {
					t.Fatal("health method bypass")
				}
			}
		}
	}
}

func TestSecretSourcesAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	for _, token := range []string{"", " ", "short", strings.Repeat("a", 4097), testSecret + "\n", testSecret + "\t", testSecret + "=a", strings.Repeat("=", 32)} {
		c := Config{Listen: "127.0.0.1:8091", LookupEnv: func(key string) (string, bool) { return token, key == "HERDR_MCP_BEARER_TOKEN" }}
		if _, err := New(c); err == nil {
			t.Fatal("invalid environment secret accepted")
		}
	}
	c := Config{Listen: "127.0.0.1:8091", LookupEnv: noEnv, BearerTokenFile: path}
	if _, err := New(c); err == nil {
		t.Fatal("missing file accepted")
	}
	for _, token := range []string{"", "\n", testSecret + "\n\n", " " + testSecret, testSecret + "\r", strings.Repeat("a", 4100)} {
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(c); err == nil {
			t.Fatal("invalid file secret accepted")
		}
	}
	for _, token := range []string{testSecret, testSecret + "\n", testSecret + "\r\n"} {
		os.WriteFile(path, []byte(token), 0o600)
		if _, err := New(c); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(path, 0o644)
	if _, err := New(c); err == nil {
		t.Fatal("readable secret accepted")
	}
	os.Chmod(path, 0o600)
	link := path + "-link"
	os.Symlink(path, link)
	c.BearerTokenFile = link
	if _, err := New(c); err == nil {
		t.Fatal("symlink accepted")
	}
	c.BearerTokenFile = filepath.Dir(path)
	if _, err := New(c); err == nil {
		t.Fatal("directory accepted")
	}
	c.BearerTokenFile = path
	old, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(wrongSecret), 0o600)
	fresh, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for i, transport := range []*Transport{old, fresh} {
		h := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
		for _, secret := range []string{testSecret, wrongSecret} {
			req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			expected := 401
			if (i == 0 && secret == testSecret) || (i == 1 && secret == wrongSecret) {
				expected = 204
			}
			if rec.Code != expected {
				t.Fatal("restart rotation semantics changed")
			}
		}
	}
	c.LookupEnv = bearerConfig("").LookupEnv
	if _, err := New(c); err == nil {
		t.Fatal("ambiguous secret sources accepted")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.AccessTeam = "https://team.cloudflareaccess.com" },
		func(c *Config) { c.AccessAudience = "audience" },
	} {
		c = bearerConfig("127.0.0.1:8091")
		edit(&c)
		if _, err := New(c); err == nil {
			t.Fatal("ambiguous auth accepted")
		}
	}
	for _, c := range []Config{
		{Listen: "127.0.0.1:8091", AccessTeam: "https://team.cloudflareaccess.com", LookupEnv: noEnv},
		{Listen: "127.0.0.1:8091", AccessAudience: "audience", LookupEnv: noEnv},
		{Listen: "127.0.0.1:8091", AccessTeam: "https://team.cloudflareaccess.com", AccessAudience: "audience", LookupEnv: noEnv},
		{Listen: "127.0.0.1:8091", AccessTeam: "https://team.cloudflareaccess.com", AccessAudience: "audience", AllowedHosts: []string{"localhost:8091"}, LookupEnv: noEnv},
		{Listen: "127.0.0.1:8091", LookupEnv: func(key string) (string, bool) { return "", key == "HERDR_MCP_BEARER_TOKEN_FILE" }},
	} {
		if _, err := New(c); err == nil {
			t.Fatal("incomplete auth accepted")
		}
	}
}

func TestDefaultPortAuthorities(t *testing.T) {
	for _, tc := range []struct{ listen, host, origin string }{
		{"127.0.0.1:80", "127.0.0.1", "http://127.0.0.1"},
		{"127.0.0.1:80", "localhost", "http://localhost:80"},
		{"[::1]:80", "[::1]", "http://[::1]"},
	} {
		transport, err := New(bearerConfig(tc.listen))
		if err != nil {
			t.Fatal(err)
		}
		h := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
		req := httptest.NewRequest("POST", "http://127.0.0.1/mcp", nil)
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Errorf("default authority %s status=%d", tc.host, rec.Code)
		}
	}
}

func TestOriginRejectsEmptyQueryMarker(t *testing.T) {
	transport, err := New(bearerConfig("127.0.0.1:8091"))
	if err != nil {
		t.Fatal(err)
	}
	h := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Origin", "http://127.0.0.1:8091?")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("Origin with query marker accepted")
	}
}
