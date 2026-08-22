package service

import (
	"context"
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
)

const UnitName = "herdr-mcp.service"

// Options controls the installed loopback service.
type Options struct {
	Listen        string
	HealthTimeout time.Duration
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
		HealthURL:  "http://" + options.Listen + "/healthz",
	}
	if err := installExecutable(installer.Executable, result.BinaryPath); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(result.EnvPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create config directory: %w", err)
	}
	if err := writeFileAtomic(result.UnitPath, []byte(unitBody(result, installer.HerdrBinary, options.Listen)), 0o644); err != nil {
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
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("listen address %q is not loopback", listen)
		}
	}
	return listen, nil
}

func environmentValue(path, key string) (string, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read service environment %s: %w", path, err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(name) == key {
			return strings.Trim(strings.TrimSpace(value), `"`), nil
		}
	}
	return "", nil
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

func unitBody(result Result, herdrBinary, listen string) string {
	return fmt.Sprintf(`[Unit]
Description=Herdr socket API MCP bridge
After=network-online.target herdr.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-%s
ExecStart=%s serve --listen %s --herdr-bin %s
Restart=on-failure
RestartSec=3
ProtectSystem=strict
ProtectHome=read-only
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=default.target
`, escapeEnvironmentFilePath(result.EnvPath), quote(result.BinaryPath), quote(listen), quote(herdrBinary))
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
