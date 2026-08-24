package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/access"
	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/Orange-County-AI/herdr-mcp/internal/mcpserver"
	"github.com/Orange-County-AI/herdr-mcp/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.2.6"

const defaultDenyMethods = "events.subscribe,pane.report_agent,pane.report_agent_session,pane.report_metadata,workspace.report_metadata,pane.clear_agent_authority,pane.release_agent,pane.graphics.*"

type commonFlags struct {
	socket       string
	herdrBinary  string
	allowMethods string
	denyMethods  string
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return runServe(nil)
	}
	switch arguments[0] {
	case "serve":
		return runServe(arguments[1:])
	case "stdio":
		return runStdio(arguments[1:])
	case "doctor":
		return runDoctor(arguments[1:])
	case "install-service":
		return runInstallService(arguments[1:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		if strings.HasPrefix(arguments[0], "-") {
			return runServe(arguments)
		}
		printUsage()
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runServe(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags)
	listen := flags.String("listen", envDefault("HERDR_MCP_LISTEN", "127.0.0.1:8091"), "loopback address for Streamable HTTP")
	accessTeam := flags.String("access-team-domain", os.Getenv("CF_ACCESS_TEAM_DOMAIN"), "Cloudflare Access team domain")
	accessAudience := flags.String("access-aud", os.Getenv("CF_ACCESS_AUD"), "Cloudflare Access application audience")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	if err := validateListen(*listen); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runtime, herdrVersion, err := buildRuntime(ctx, *common)
	if err != nil {
		return err
	}

	var mcpHandler http.Handler = runtime.HTTPHandler()
	if *accessTeam != "" || *accessAudience != "" {
		validator, err := access.NewValidator(*accessTeam, *accessAudience)
		if err != nil {
			return err
		}
		mcpHandler = validator.Middleware(mcpHandler)
		log.Printf("auth: Cloudflare Access JWT required for /mcp")
	} else {
		log.Printf("auth: /mcp trusts the network edge; keep the listener on loopback and require Cloudflare Access externally")
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/healthz", healthHandler(runtime.Client, runtime.Schema.Protocol))

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}()

	log.Printf("herdr: version=%s protocol=%d socket=%s", herdrVersion, runtime.Schema.Protocol, runtime.Client.SocketPath)
	log.Printf("mcp: %d tools at http://%s/mcp", len(runtime.Methods), *listen)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func runStdio(arguments []string) error {
	flags := flag.NewFlagSet("stdio", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("stdio does not accept positional arguments")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runtime, herdrVersion, err := buildRuntime(ctx, *common)
	if err != nil {
		return err
	}
	log.Printf("herdr: version=%s protocol=%d socket=%s tools=%d", herdrVersion, runtime.Schema.Protocol, runtime.Client.SocketPath, len(runtime.Methods))
	return runtime.MCP.Run(ctx, &mcp.StdioTransport{})
}

func runDoctor(arguments []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	common := addCommonFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("doctor does not accept positional arguments")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runtime, herdrVersion, err := buildRuntime(ctx, *common)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"herdr_version":     herdrVersion,
		"protocol":          runtime.Schema.Protocol,
		"schema_version":    runtime.Schema.SchemaVersion,
		"socket":            runtime.Client.SocketPath,
		"tools":             len(runtime.Methods),
		"herdr_mcp_version": version,
	})
}

func runInstallService(arguments []string) error {
	flags := flag.NewFlagSet("install-service", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	listen := flags.String("listen", os.Getenv("HERDR_MCP_LISTEN"), "loopback address for the installed service (default: existing service environment or 127.0.0.1:8091)")
	timeout := flags.Duration("timeout", 15*time.Second, "maximum time to wait for service health")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("install-service does not accept positional arguments")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be greater than zero")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	installer, err := service.NewInstaller()
	if err != nil {
		return err
	}
	result, err := installer.Install(ctx, service.Options{
		Listen:        *listen,
		HealthTimeout: *timeout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s\n", result.BinaryPath)
	fmt.Printf("Enabled %s\n", result.UnitPath)
	fmt.Printf("Healthy %s\n", result.HealthURL)
	fmt.Printf("Optional environment: %s\n", result.EnvPath)
	return nil
}

func addCommonFlags(flags *flag.FlagSet) *commonFlags {
	defaultSocket, err := herdr.DefaultSocketPath()
	if err != nil {
		defaultSocket = os.Getenv("HERDR_SOCKET_PATH")
	}
	common := &commonFlags{}
	flags.StringVar(&common.socket, "socket", defaultSocket, "Herdr API socket path")
	flags.StringVar(&common.herdrBinary, "herdr-bin", envDefault("HERDR_BIN", "herdr"), "Herdr binary used to load the API schema")
	flags.StringVar(&common.denyMethods, "deny-methods", envDefault("HERDR_MCP_DENY_METHODS", defaultDenyMethods), "comma-separated method globs to omit")
	return common
}

func buildRuntime(ctx context.Context, flags commonFlags) (*mcpserver.Server, string, error) {
	schema, err := herdr.LoadSchema(ctx, flags.herdrBinary)
	if err != nil {
		return nil, "", err
	}
	client := &herdr.Client{SocketPath: flags.socket}
	herdrVersion, protocol, err := client.Ping(ctx)
	if err != nil {
		return nil, "", err
	}
	if protocol != schema.Protocol {
		return nil, "", fmt.Errorf("protocol mismatch: %s reports schema protocol %d, socket reports %d", flags.herdrBinary, schema.Protocol, protocol)
	}
	runtime, err := mcpserver.New(schema, client, version, splitCSV(flags.allowMethods), splitCSV(flags.denyMethods))
	if err != nil {
		return nil, "", err
	}
	return runtime, herdrVersion, nil
}

func healthHandler(client *herdr.Client, expectedProtocol int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		version, protocol, err := client.Ping(ctx)
		w.Header().Set("Content-Type", "application/json")
		if err != nil || protocol != expectedProtocol {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"version":  version,
			"protocol": protocol,
		})
	}
}

func validateListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid --listen address %q: %w", address, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen must use a loopback address; expose it through Cloudflare Tunnel instead")
	}
	return nil
}

func splitCSV(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func envDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `herdr-mcp exposes Herdr's socket API as MCP tools.

usage:
  herdr-mcp serve [flags]            serve Streamable HTTP on loopback (default)
  herdr-mcp stdio [flags]            serve MCP over stdin/stdout
  herdr-mcp doctor [flags]           verify schema/socket compatibility
  herdr-mcp install-service [flags]  install and start a systemd user service
  herdr-mcp version

Run "herdr-mcp <command> -h" for command flags.`)
}
