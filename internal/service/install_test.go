package service

import (
	"context"
	"fmt"
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
		result.EnvPath,
		"127.0.0.1:19091",
		"/opt/herdr/bin/herdr",
		"ProtectHome=read-only",
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
