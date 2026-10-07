package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInstallWritesAndStartsUserService(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	config := filepath.Join(root, "config")
	source := filepath.Join(root, "source-herdr-mcp")
	if err := os.WriteFile(source, []byte("test executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	var checkedURL string
	installer := &Installer{
		GOOS:        "linux",
		HomeDir:     home,
		ConfigDir:   config,
		Executable:  source,
		HerdrBinary: "/opt/herdr/bin/herdr",
		Systemctl:   "/usr/bin/systemctl",
		Run: func(_ context.Context, name string, arguments ...string) error {
			commands = append(commands, append([]string{name}, arguments...))
			return nil
		},
		CheckHealth: func(_ context.Context, healthURL string, timeout time.Duration) error {
			checkedURL = healthURL
			if timeout != 4*time.Second {
				return fmt.Errorf("timeout = %s", timeout)
			}
			return nil
		},
	}
	result, err := installer.Install(context.Background(), Options{
		Listen:        "127.0.0.1:19091",
		HealthTimeout: 4 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	binary, err := os.ReadFile(result.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(binary) != "test executable" {
		t.Fatalf("installed binary = %q", binary)
	}
	info, err := os.Stat(result.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary mode = %v", info.Mode().Perm())
	}
	unit, err := os.ReadFile(result.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		result.BinaryPath,
		"WantedBy=default.target",
		result.EnvPath,
		"127.0.0.1:19091",
		"/opt/herdr/bin/herdr",
		"ProtectHome=read-only",
		// ProtectHome=read-only would otherwise make the schema cache
		// unwritable, and the bridge needs that cache to register tools while
		// the Herdr binary is mid-upgrade.
		"CacheDirectory=herdr-mcp",
		// The bridge is built to outlive Herdr, so a crash should bring it back
		// even while Herdr itself is down.
		"Restart=always",
	} {
		if !strings.Contains(string(unit), expected) {
			t.Errorf("unit does not contain %q:\n%s", expected, unit)
		}
	}
	wantCommands := [][]string{
		{"/usr/bin/systemctl", "--user", "daemon-reload"},
		{"/usr/bin/systemctl", "--user", "enable", UnitName},
		{"/usr/bin/systemctl", "--user", "restart", UnitName},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
	if checkedURL != "http://127.0.0.1:19091/healthz" || checkedURL != result.HealthURL {
		t.Fatalf("health URL = %q", checkedURL)
	}
	if _, err := os.Stat(result.EnvPath); !os.IsNotExist(err) {
		t.Fatalf("installer should preserve the optional env file for user configuration, stat err = %v", err)
	}
}

func TestInstallRejectsUnsupportedPlatform(t *testing.T) {
	installer := &Installer{GOOS: "darwin"}
	if _, err := installer.Install(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveListenPrefersExplicitThenExistingServiceEnvironment(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "herdr-mcp", "env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("# existing service configuration\nHERDR_MCP_LISTEN=127.0.0.1:18091\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listen, err := resolveListen("", envPath)
	if err != nil || listen != "127.0.0.1:18091" {
		t.Fatalf("existing service listen = %q, err = %v", listen, err)
	}
	listen, err = resolveListen("127.0.0.1:19091", envPath)
	if err != nil || listen != "127.0.0.1:19091" {
		t.Fatalf("explicit listen = %q, err = %v", listen, err)
	}
	if _, err := resolveListen("0.0.0.0:8091", envPath); err == nil {
		t.Fatal("non-loopback listen was accepted")
	}
}

func TestUnitBodyEscapesSystemdPaths(t *testing.T) {
	body := unitBody(Result{
		BinaryPath: "/home/test user/.local/bin/herdr-mcp",
		EnvPath:    "/home/test user/.config/herdr-mcp/env",
	}, "/home/test user/.local/bin/herdr", "127.0.0.1:8091")
	if !strings.Contains(body, `EnvironmentFile=-/home/test\x20user/.config/herdr-mcp/env`) {
		t.Fatalf("environment path is not escaped for systemd:\n%s", body)
	}
	if !strings.Contains(body, `ExecStart="/home/test user/.local/bin/herdr-mcp"`) {
		t.Fatalf("executable path is not quoted for systemd:\n%s", body)
	}
}

func TestWaitForHealthRequiresHealthyJSON(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	if err := waitForHealth(context.Background(), server.URL, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("health checks = %d, want a retry", calls)
	}
}

func TestNativeServiceConfigurationAndCredentialIsolation(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env")
	// This credential is synthetic and must remain solely in the existing env.
	secret := "test_only_0123456789abcdefghijklmnop"
	env := "HERDR_MCP_LISTEN=192.168.1.2:8091\nHERDR_MCP_ALLOW_PRIVATE=true\nHERDR_MCP_BEARER_TOKEN=" + secret + "\nHERDR_MCP_TLS_CERT_FILE=/private/cert.pem\nHERDR_MCP_TLS_KEY_FILE=/private/key.pem\nHERDR_MCP_ALLOWED_HOSTS=mcp.private.test:8091\n"
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	listen, err := resolveListen("", envPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := resolveHTTP(Options{Listen: listen}, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowPrivate || c.TLSCertFile != "/private/cert.pem" || c.TLSKeyFile != "/private/key.pem" || len(c.AllowedHosts) != 1 {
		t.Fatal("service options lost")
	}
	value, configured := c.LookupEnv("HERDR_MCP_BEARER_TOKEN")
	if !configured || value != secret {
		t.Fatal("existing service credential not available")
	}
	c.Explicit = nil // source-path flags explicitly supplied to this unit renderer
	body := unitBody(Result{BinaryPath: "/bin/herdr-mcp", EnvPath: envPath}, "/bin/herdr", listen, c)
	for _, expected := range []string{"--allow-private", "--tls-cert-file", "--tls-key-file", "--allowed-hosts", "192.168.1.2:8091"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %s", expected)
		}
	}
	if strings.Contains(body, secret) || strings.Contains(body, "--bearer-token ") {
		t.Fatal("credential in generated unit")
	}
	c.BearerTokenFile = "/private/bearer-secret"
	body = unitBody(Result{}, "/bin/herdr", listen, c)
	if !strings.Contains(body, "--bearer-token-file") || strings.Contains(body, secret) {
		t.Fatal("secret source not isolated")
	}
}

func TestInstallRejectsUnsafeConfigurationBeforeMutation(t *testing.T) {
	for _, env := range []string{
		"HERDR_MCP_LISTEN=192.168.1.2:8091\n",
		"HERDR_MCP_BEARER_TOKEN=\n",
		"HERDR_MCP_BEARER_TOKEN_FILE=\n",
		"HERDR_MCP_TLS_CERT_FILE=/missing/cert\n",
		"HERDR_MCP_TLS_CERT_FILE=/missing/cert\nHERDR_MCP_TLS_KEY_FILE=/missing/key\n",
		"HERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\nCF_ACCESS_AUD=ambiguous\n",
	} {
		root := t.TempDir()
		envPath := filepath.Join(root, "herdr-mcp", "env")
		if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		mutated := false
		installer := &Installer{GOOS: "linux", ConfigDir: root, HomeDir: root, Executable: "/does/not/exist", Run: func(context.Context, string, ...string) error { mutated = true; return nil }, CheckHealth: func(context.Context, string, time.Duration) error { mutated = true; return nil }}
		if _, err := installer.Install(context.Background(), Options{}); err == nil || strings.Contains(err.Error(), "open current executable") {
			t.Fatalf("configuration not validated first: %v", err)
		}
		if mutated {
			t.Fatal("installer mutated services before validation")
		}
		if _, err := os.Stat(filepath.Join(root, ".local", "bin", "herdr-mcp")); !os.IsNotExist(err) {
			t.Fatal("installer wrote binary")
		}
		after, err := os.ReadFile(envPath)
		if err != nil || string(after) != env {
			t.Fatal("installer rewrote credential environment")
		}
	}
}

func TestHealthTLSVerificationIsRequired(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"ok":true}`) }))
	defer server.Close()
	if err := waitForHealth(context.Background(), server.URL, 200*time.Millisecond); err == nil {
		t.Fatal("untrusted service certificate accepted")
	}
}

func TestServiceEnvironmentFailsClosed(t *testing.T) {
	for _, env := range []string{
		"export HERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\n",
		"export\tHERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\n",
		"export \t HERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\n",
		"HERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\nHERDR_MCP_BEARER_TOKEN=\n",
		"HERDR_MCP_BEARER_TOKEN=\"unterminated\n",
		"HERDR_MCP_BEARER_TOKEN='first'last\n",
		"HERDR_MCP_TLS_CERT_FILE=/private/\\cert\n",
	} {
		path := filepath.Join(t.TempDir(), "env")
		os.WriteFile(path, []byte(env), 0o600)
		if _, err := resolveHTTP(Options{Listen: "127.0.0.1:8091"}, path); err == nil {
			t.Fatal("ambiguous systemd environment accepted")
		}
	}
}

func TestServiceExplicitFalseOverridesEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte("HERDR_MCP_ALLOW_PRIVATE=true\n"), 0o600)
	config, err := resolveHTTP(Options{Listen: "127.0.0.1:8091", HTTPFlags: map[string]bool{"allow-private": true}}, path)
	if err != nil {
		t.Fatal(err)
	}
	if config.AllowPrivate {
		t.Fatal("explicit false ignored")
	}
	body := unitBody(Result{}, "/bin/herdr", "127.0.0.1:8091", config)
	if !strings.Contains(body, "--allow-private=false") {
		t.Fatal("false override not persisted")
	}
}

func TestServiceEnvironmentOwnerPermissionsAndUnsetManagerDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	token := "test_only_0123456789abcdefghijklmnop"
	os.WriteFile(path, []byte("HERDR_MCP_BEARER_TOKEN="+token+"\n"), 0o644)
	if _, err := resolveHTTP(Options{Listen: "127.0.0.1:8091"}, path); err == nil {
		t.Fatal("readable credential environment accepted")
	}
	os.Chmod(path, 0o600)
	cfg, err := resolveHTTP(Options{Listen: "127.0.0.1:8091"}, path)
	if err != nil {
		t.Fatal(err)
	}
	body := unitBody(Result{EnvPath: path}, "/bin/herdr", cfg.Listen, cfg)
	if !strings.Contains(body, "EnvironmentFile="+escapeEnvironmentFilePath(path)+"\n") || !strings.Contains(body, " --require-auth") {
		t.Fatal("authenticated unit lost its mandatory credential source or startup guard")
	}
	var unset string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "UnsetEnvironment=") {
			unset = line
		}
	}
	if !strings.Contains(unset, "CF_ACCESS_TEAM_DOMAIN") || !strings.Contains(unset, "HERDR_MCP_BEARER_TOKEN_FILE") {
		t.Fatal("unvalidated manager environment not removed")
	}
	for _, key := range strings.Fields(strings.TrimPrefix(unset, "UnsetEnvironment=")) {
		if key == "HERDR_MCP_BEARER_TOKEN" {
			t.Fatal("persisted credential removed")
		}
	}
	if strings.Contains(body, token) {
		t.Fatal("credential serialized")
	}
	os.WriteFile(path, []byte("HERDR_MCP_BEARER_TOKEN='"+token+"'\n"), 0o600)
	cfg, err = resolveHTTP(Options{Listen: "127.0.0.1:8091"}, path)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := cfg.LookupEnv("HERDR_MCP_BEARER_TOKEN")
	if value != token {
		t.Fatal("single quoted environment differs from systemd")
	}
}

func TestInstallNativeHTTPSValidatesThenGeneratesSourceOnlyUnit(t *testing.T) {
	root := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "native-service-test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("192.168.1.2")}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	os.WriteFile(certPath, certPEM, 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	source := filepath.Join(root, "source")
	os.WriteFile(source, []byte("test executable"), 0o700)
	configDir := filepath.Join(root, "config")
	envPath := filepath.Join(configDir, "herdr-mcp", "env")
	os.MkdirAll(filepath.Dir(envPath), 0o700)
	env := "HERDR_MCP_LISTEN=192.168.1.2:8091\nHERDR_MCP_ALLOW_PRIVATE=true\nHERDR_MCP_BEARER_TOKEN=test_only_0123456789abcdefghijklmnop\nHERDR_MCP_TLS_CERT_FILE=" + certPath + "\nHERDR_MCP_TLS_KEY_FILE=" + keyPath + "\n"
	os.WriteFile(envPath, []byte(env), 0o600)
	starts := 0
	installer := &Installer{GOOS: "linux", HomeDir: filepath.Join(root, "home"), ConfigDir: configDir, Executable: source, HerdrBinary: "/bin/herdr", Systemctl: "systemctl", TrustRoots: roots, Run: func(context.Context, string, ...string) error { starts++; return nil }, CheckHealth: func(_ context.Context, url string, _ time.Duration) error {
		if url != "https://192.168.1.2:8091/healthz" {
			t.Fatal(url)
		}
		return nil
	}}
	result, err := installer.Install(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 3 {
		t.Fatal("service commands missing")
	}
	body, err := os.ReadFile(result.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "test_only_") || !strings.Contains(string(body), "--tls-key-file") {
		t.Fatal("source paths or credential isolation lost")
	}
	after, _ := os.ReadFile(envPath)
	if string(after) != env {
		t.Fatal("credential environment changed")
	}
	// A wrong bind SAN must fail before even a fake restart or binary copy.
	installer.HomeDir = filepath.Join(root, "bad-home")
	starts = 0
	_, err = installer.Install(context.Background(), Options{Listen: "192.168.1.3:8091"})
	if err == nil || starts != 0 {
		t.Fatal("SAN not verified before mutation")
	}
	if _, err := os.Stat(filepath.Join(installer.HomeDir, ".local", "bin", "herdr-mcp")); !os.IsNotExist(err) {
		t.Fatal("invalid TLS replaced binary")
	}
}
