package service

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/httptransport"
)

const UnitName = "herdr-mcp.service"

// Options controls the installed service. HTTP contains only source paths;
// bearer values stay in the owner's secret file or service environment.
type Options struct {
	Listen        string
	HealthTimeout time.Duration
	HTTP          httptransport.Config
	AllowedHosts  string
	HTTPFlags     map[string]bool
}

// Result names the files and endpoint installed for the user.
type Result struct {
	BinaryPath string
	UnitPath   string
	EnvPath    string
	HealthURL  string
}

// Installer owns the host dependencies used by Install. Exported fields make
// the filesystem and process boundary deterministic in tests.
type Installer struct {
	GOOS        string
	HomeDir     string
	ConfigDir   string
	Executable  string
	HerdrBinary string
	Systemctl   string
	Run         func(context.Context, string, ...string) error
	CheckHealth func(context.Context, string, time.Duration) error
	TrustRoots  *x509.CertPool // nil uses system roots; injectable for synthetic test CAs
}

// NewInstaller resolves the current executable, Herdr binary, and systemctl.
func NewInstaller() (*Installer, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve current executable: %w", err)
	}
	herdrName := strings.TrimSpace(os.Getenv("HERDR_BIN"))
	if herdrName == "" {
		herdrName = "herdr"
	}
	herdrBinary, err := exec.LookPath(herdrName)
	if err != nil {
		return nil, fmt.Errorf("find Herdr binary %q: %w", herdrName, err)
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, fmt.Errorf("find systemctl: %w", err)
	}
	return &Installer{
		GOOS:        runtime.GOOS,
		HomeDir:     homeDir,
		ConfigDir:   configDir,
		Executable:  executable,
		HerdrBinary: herdrBinary,
		Systemctl:   systemctl,
		Run:         runCommand,
		CheckHealth: waitForHealth,
	}, nil
}

// Install copies the current binary, writes a user unit, enables and restarts
// it, then waits for the loopback health endpoint.
func (installer *Installer) Install(ctx context.Context, options Options) (Result, error) {
	if installer.GOOS != "linux" {
		return Result{}, fmt.Errorf("install-service requires Linux with systemd user services")
	}
	envPath := filepath.Join(installer.ConfigDir, "herdr-mcp", "env")
	listen, err := resolveListen(options.Listen, envPath)
	if err != nil {
		return Result{}, err
	}
	options.Listen = listen
	httpConfig, err := resolveHTTP(options, envPath)
	if err != nil {
		return Result{}, err
	}
	transport, err := httptransport.New(httpConfig)
	if err != nil {
		return Result{}, err
	}
	host, _, _ := net.SplitHostPort(listen)
	if err := transport.VerifyCertificate(host, installer.TrustRoots); err != nil {
		return Result{}, err
	}
	if options.HealthTimeout <= 0 {
		options.HealthTimeout = 15 * time.Second
	}
	if installer.Run == nil || installer.CheckHealth == nil {
		return Result{}, fmt.Errorf("service installer dependencies are incomplete")
	}

	result := Result{
		BinaryPath: filepath.Join(installer.HomeDir, ".local", "bin", "herdr-mcp"),
		UnitPath:   filepath.Join(installer.ConfigDir, "systemd", "user", UnitName),
		EnvPath:    envPath,
		HealthURL:  transport.Scheme() + "://" + options.Listen + "/healthz",
	}
	if err := installExecutable(installer.Executable, result.BinaryPath); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(result.EnvPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create config directory: %w", err)
	}
	unitConfig := httpConfig
	if err := writeFileAtomic(result.UnitPath, []byte(unitBody(result, installer.HerdrBinary, options.Listen, unitConfig)), 0o644); err != nil {
		return Result{}, fmt.Errorf("install systemd unit: %w", err)
	}

	for _, command := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", UnitName},
		{"--user", "restart", UnitName},
	} {
		if err := installer.Run(ctx, installer.Systemctl, command...); err != nil {
			return Result{}, err
		}
	}
	if err := installer.CheckHealth(ctx, result.HealthURL, options.HealthTimeout); err != nil {
		return Result{}, fmt.Errorf("service did not become healthy: %w; inspect with `systemctl --user status %s`", err, UnitName)
	}
	return result, nil
}

func resolveListen(explicit, envPath string) (string, error) {
	listen := strings.TrimSpace(explicit)
	if listen == "" {
		value, err := environmentValue(envPath, "HERDR_MCP_LISTEN")
		if err != nil {
			return "", err
		}
		listen = value
	}
	if listen == "" {
		listen = "127.0.0.1:8091"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid loopback listen address %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate())) {
		return "", fmt.Errorf("listen must use loopback or a private IP")
	}
	return listen, nil
}

var httpEnvironmentKeys = []string{
	"HERDR_MCP_LISTEN", "HERDR_MCP_ALLOW_PRIVATE", "HERDR_MCP_BEARER_TOKEN_FILE",
	"HERDR_MCP_BEARER_TOKEN", "HERDR_MCP_TLS_CERT_FILE", "HERDR_MCP_TLS_KEY_FILE",
	"HERDR_MCP_ALLOWED_HOSTS", "CF_ACCESS_TEAM_DOMAIN", "CF_ACCESS_AUD",
}

func serviceEnvironment(path string) (map[string]string, error) {
	values := make(map[string]string)
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return values, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read service environment: %w", err)
	}
	relevant := make(map[string]bool)
	for _, key := range httpEnvironmentKeys {
		relevant[key] = true
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.Trim(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		candidate := line
		if strings.HasPrefix(candidate, "export") && len(candidate) > len("export") && strings.ContainsRune(" \t", rune(candidate[len("export")])) {
			candidate = strings.TrimLeft(candidate[len("export"):], " \t")
		}
		rawName, value, found := strings.Cut(candidate, "=")
		name := strings.Trim(rawName, " \t\r")
		if !relevant[name] {
			continue
		} // preserve unrelated existing settings
		if !found || candidate != line || rawName != name {
			return nil, fmt.Errorf("unsupported service environment syntax for %s", name)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("duplicate service environment setting %s", name)
		}
		value = strings.Trim(value, " \t\r")
		if strings.ContainsAny(value, "\\\r\x00") {
			return nil, fmt.Errorf("unsupported escapes/control characters in service setting %s", name)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			quote := value[0]
			if len(value) < 2 || value[len(value)-1] != quote {
				return nil, fmt.Errorf("unsupported quoted service setting %s", name)
			}
			value = value[1 : len(value)-1]
			if strings.ContainsRune(value, rune(quote)) {
				return nil, fmt.Errorf("unsupported embedded quotes in service setting %s", name)
			}
		} else if strings.ContainsAny(value, "\"'") {
			return nil, fmt.Errorf("unsupported quotes in service setting %s", name)
		}
		values[name] = value
	}
	if _, bearer := values["HERDR_MCP_BEARER_TOKEN"]; bearer {
		// Read/check the exact owner-controlled inode and parse it again below if
		// the path changed. A path replacement cannot substitute a world-readable
		// credential file between the permission check and read.
		checked, err := httptransport.ReadOwnerFile(path, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("service bearer environment: %w", err)
		}
		if string(checked) != string(content) {
			return nil, fmt.Errorf("service environment changed during validation; retry installation")
		}
	}
	return values, nil
}

func resolveHTTP(options Options, envPath string) (httptransport.Config, error) {
	values, err := serviceEnvironment(envPath)
	if err != nil {
		return httptransport.Config{}, err
	}
	if _, processToken := os.LookupEnv("HERDR_MCP_BEARER_TOKEN"); processToken {
		if _, persistedToken := values["HERDR_MCP_BEARER_TOKEN"]; !persistedToken {
			return httptransport.Config{}, fmt.Errorf("installer bearer credential must be in the existing owner-only service environment or a secret file, not only the installer process")
		}
	}
	c := options.HTTP
	c.Listen = options.Listen
	c.Explicit = options.HTTPFlags
	if c.Explicit == nil {
		c.Explicit = make(map[string]bool)
	}
	for _, item := range []struct {
		key, flag string
		target    *string
	}{
		{"HERDR_MCP_BEARER_TOKEN_FILE", "bearer-token-file", &c.BearerTokenFile},
		{"HERDR_MCP_TLS_CERT_FILE", "tls-cert-file", &c.TLSCertFile},
		{"HERDR_MCP_TLS_KEY_FILE", "tls-key-file", &c.TLSKeyFile},
		{"CF_ACCESS_TEAM_DOMAIN", "access-team-domain", &c.AccessTeam},
		{"CF_ACCESS_AUD", "access-aud", &c.AccessAudience},
		{"HERDR_MCP_ALLOWED_HOSTS", "allowed-hosts", &options.AllowedHosts},
	} {
		if !c.Explicit[item.flag] && *item.target == "" {
			*item.target = values[item.key]
		}
	}
	if !c.Explicit["allow-private"] && !c.AllowPrivate {
		if value, exists := values["HERDR_MCP_ALLOW_PRIVATE"]; exists {
			c.AllowPrivate, err = strconv.ParseBool(value)
			if err != nil {
				return c, fmt.Errorf("invalid service HERDR_MCP_ALLOW_PRIVATE")
			}
		}
	}
	_, rawBearer := values["HERDR_MCP_BEARER_TOKEN"]
	c.RequireAuth = c.RequireAuth || rawBearer || c.BearerTokenFile != "" || c.AccessTeam != "" || c.AccessAudience != ""
	c.BearerFileSet = c.Explicit["bearer-token-file"]
	c.TLSSet = c.Explicit["tls-cert-file"] || c.Explicit["tls-key-file"]
	c.AllowedHosts = nil
	if options.AllowedHosts != "" {
		c.AllowedHosts = strings.Split(options.AllowedHosts, ",")
	}
	c.LookupEnv = func(key string) (string, bool) { value, exists := values[key]; return value, exists }
	return c, nil
}

func environmentValue(path, key string) (string, error) {
	values, err := serviceEnvironment(path)
	return values[key], err
}

func installExecutable(source, destination string) error {
	if source == "" {
		return fmt.Errorf("current executable path is empty")
	}
	if sourceInfo, err := os.Stat(source); err == nil {
		if destinationInfo, destinationErr := os.Stat(destination); destinationErr == nil && os.SameFile(sourceInfo, destinationInfo) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create binary directory: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open current executable: %w", err)
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".herdr-mcp-*")
	if err != nil {
		return fmt.Errorf("create temporary executable: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return fmt.Errorf("copy executable: %w", err)
	}
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return fmt.Errorf("mark executable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close executable: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("install executable: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".herdr-mcp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func unitBody(result Result, herdrBinary, listen string, configs ...httptransport.Config) string {
	var arguments string
	var unsetEnvironment string
	environmentPrefix := "-"
	if len(configs) != 0 {
		c := configs[0]
		if c.LookupEnv != nil {
			if _, rawBearer := c.LookupEnv("HERDR_MCP_BEARER_TOKEN"); rawBearer {
				environmentPrefix = ""
			}
			for _, key := range httpEnvironmentKeys {
				if _, present := c.LookupEnv(key); !present {
					unsetEnvironment += " " + key
				}
			}
		}
		arguments += " --allow-private=" + strconv.FormatBool(c.AllowPrivate)
		if c.RequireAuth {
			arguments += " --require-auth"
		}
		for _, item := range []struct{ flag, value string }{
			{"--bearer-token-file", c.BearerTokenFile},
			{"--tls-cert-file", c.TLSCertFile},
			{"--tls-key-file", c.TLSKeyFile},
			{"--allowed-hosts", strings.Join(c.AllowedHosts, ",")},
			{"--access-team-domain", c.AccessTeam},
			{"--access-aud", c.AccessAudience},
		} {
			if c.Explicit[strings.TrimPrefix(item.flag, "--")] || item.value != "" {
				arguments += " " + item.flag + " " + quote(item.value)
			}
		}
	}
	// The bridge deliberately outlives Herdr, so nothing here binds its
	// lifetime to herdr.service beyond start ordering, and Restart=always
	// covers a crash even while Herdr is down.
	//
	// CacheDirectory is load-bearing, not hygiene: ProtectHome=read-only makes
	// the schema cache unwritable, and without that cache the bridge cannot
	// register tools during an upgrade that replaces the Herdr binary -- which
	// is one of the outages it exists to cover.
	//
	// RuntimeDirectory holds the forwarded sockets for saved SSH machines, and
	// systemd removes it on stop, so a crash cannot leave stale sockets that
	// block the next ssh from binding. ProtectHome stays read-only: ssh needs to
	// READ ~/.ssh, and a host it has never seen simply fails to be recorded in
	// known_hosts rather than failing to connect.
	return fmt.Sprintf(`[Unit]
Description=Herdr socket API MCP bridge
After=network-online.target herdr.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%s%s
UnsetEnvironment=%s
ExecStart=%s serve --listen %s --herdr-bin %s%s
Restart=always
RestartSec=3
CacheDirectory=herdr-mcp
RuntimeDirectory=herdr-mcp
ProtectSystem=strict
ProtectHome=read-only
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=default.target
`, environmentPrefix, escapeEnvironmentFilePath(result.EnvPath), strings.TrimSpace(unsetEnvironment), quote(result.BinaryPath), quote(listen), quote(herdrBinary), arguments)
}

func quote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "$", "$$")
	return strconv.Quote(value)
}

func escapeEnvironmentFilePath(value string) string {
	return strings.NewReplacer(
		"\\", "\\x5c",
		" ", "\\x20",
		"\t", "\\x09",
		"%", "%%",
	).Replace(value)
}

func runCommand(ctx context.Context, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func waitForHealth(ctx context.Context, healthURL string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var lastError error
	for {
		request, err := http.NewRequestWithContext(deadline, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var status struct {
				OK bool `json:"ok"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&status)
			response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil && status.OK {
				return nil
			}
			lastError = fmt.Errorf("HTTP %d", response.StatusCode)
		} else {
			lastError = err
		}
		select {
		case <-deadline.Done():
			if lastError != nil {
				return lastError
			}
			return deadline.Err()
		case <-ticker.C:
		}
	}
}
