package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const stdioTestSchema = `{"protocol":20,"schema_version":1,"schemas":{"request":{"oneOf":[{"properties":{"method":{"const":"pane.read"},"params":{"$ref":"#/schemas/request/$defs/Params"}}}],"$defs":{"Params":{"type":"object","properties":{"pane_id":{"type":"string"}},"required":["pane_id"]}}}}}`

func TestStdioIgnoresHTTPConfiguration(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "fake-herdr")
	if err := os.WriteFile(fakeHerdr, []byte("#!/bin/sh\ncat <<'SCHEMA'\n"+stdioTestSchema+"\nSCHEMA\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioProcess$")
	// Deliberately invalid HTTP configuration proves stdio doesn't load auth,
	// certificates or bind a listener. It uses a synthetic binary and no sessions.
	command.Env = append(os.Environ(), "HERDR_MCP_STDIO_TEST_HELPER=1", "HERDR_MCP_STDIO_TEST_BINARY="+fakeHerdr, "HERDR_MCP_STDIO_TEST_SOCKET="+filepath.Join(root, "missing.sock"), "HERDR_MCP_BEARER_TOKEN=", "HERDR_MCP_TLS_CERT_FILE=/missing/cert", "HERDR_MCP_LISTEN=0.0.0.0:8091", "XDG_CACHE_HOME="+root)
	command.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "pane_read" {
		t.Fatalf("tools=%+v", result.Tools)
	}
}

func TestStdioProcess(t *testing.T) {
	if os.Getenv("HERDR_MCP_STDIO_TEST_HELPER") != "1" {
		return
	}
	err := runStdio([]string{"--herdr-bin", os.Getenv("HERDR_MCP_STDIO_TEST_BINARY"), "--socket", os.Getenv("HERDR_MCP_STDIO_TEST_SOCKET"), "--machines=false", "--schema-refresh=0"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestServeRejectsConfigurationBeforeHerdr(t *testing.T) {
	t.Setenv("HERDR_MCP_BEARER_TOKEN", "")
	for _, args := range [][]string{
		{"--herdr-bin", "/does/not/exist"},
		{"--tls-cert-file", "/does/not/exist", "--herdr-bin", "/does/not/exist"},
		{"--listen", "0.0.0.0:8091", "--herdr-bin", "/does/not/exist"},
	} {
		if err := runServe(args); err == nil || !strings.Contains(err.Error(), "bearer secret") {
			t.Fatalf("wrong failure boundary: %v", err)
		}
	}
}

func TestHTTPFlagsDoNotAcceptCredentialValues(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg, hosts := addHTTPFlags(flags, "127.0.0.1:8091")
	if flags.Lookup("bearer-token") != nil {
		t.Fatal("credential argv flag exists")
	}
	if err := flags.Parse([]string{"--allow-private", "--listen", "192.168.1.2:8091", "--bearer-token-file", "/private/secret", "--tls-cert-file", "/private/cert", "--tls-key-file", "/private/key", "--allowed-hosts", "mcp.test:8091"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowPrivate || cfg.Listen != "192.168.1.2:8091" || cfg.BearerTokenFile != "/private/secret" || cfg.TLSCertFile != "/private/cert" || cfg.TLSKeyFile != "/private/key" || *hosts != "mcp.test:8091" {
		t.Fatal("serve flags inconsistent")
	}
}
