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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/access"
	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/Orange-County-AI/herdr-mcp/internal/mcpserver"
	"github.com/Orange-County-AI/herdr-mcp/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.3.0"

// schemaLoadBudget caps how long a degraded start waits on the Herdr binary
// before falling back to the cached schema.
const schemaLoadBudget = 20 * time.Second

const defaultDenyMethods = "events.subscribe,pane.report_agent,pane.report_agent_session,pane.report_metadata,workspace.report_metadata,pane.clear_agent_authority,pane.release_agent,pane.graphics.*"

type commonFlags struct {
	socket          string
	herdrBinary     string
	allowMethods    string
	denyMethods     string
	concurrency     int
	longConcurrency int
	queueDepth      int
	outageGrace     time.Duration
}

// runtimeBundle is what a successful startup produced, including the parts a
// degraded startup could not verify.
type runtimeBundle struct {
	Server       *mcpserver.Server
	Client       *herdr.Client
	Queue        *herdr.Queue
	HerdrVersion string
	Protocol     int
	Notes        []string
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
	bundle, err := buildRuntime(ctx, *common, false)
	if err != nil {
		return err
	}
	for _, note := range bundle.Notes {
		log.Printf("startup: %s", note)
	}

	var mcpHandler http.Handler = bundle.Server.HTTPHandler()
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
	mux.HandleFunc("/healthz", healthHandler(bundle))

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

	log.Printf("herdr: version=%s protocol=%d socket=%s", describeVersion(bundle.HerdrVersion), bundle.Protocol, bundle.Client.SocketPath)
	log.Printf("queue: %d concurrent, %d long-poll, %d queued per lane, %s outage grace",
		common.concurrency, common.longConcurrency, common.queueDepth, common.outageGrace)
	log.Printf("mcp: %d tools at http://%s/mcp", len(bundle.Server.Methods), *listen)
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
	bundle, err := buildRuntime(ctx, *common, false)
	if err != nil {
		return err
	}
	for _, note := range bundle.Notes {
		log.Printf("startup: %s", note)
	}
	log.Printf("herdr: version=%s protocol=%d socket=%s tools=%d", describeVersion(bundle.HerdrVersion), bundle.Protocol, bundle.Client.SocketPath, len(bundle.Server.Methods))
	return bundle.Server.MCP.Run(ctx, &mcp.StdioTransport{})
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
	bundle, err := buildRuntime(ctx, *common, true)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"herdr_version":     bundle.HerdrVersion,
		"protocol":          bundle.Server.Schema.Protocol,
		"schema_version":    bundle.Server.Schema.SchemaVersion,
		"socket":            bundle.Client.SocketPath,
		"tools":             len(bundle.Server.Methods),
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
	flags.IntVar(&common.concurrency, "max-concurrent", envInt("HERDR_MCP_MAX_CONCURRENT", 8), "simultaneous Herdr calls; further calls queue")
	flags.IntVar(&common.longConcurrency, "max-long-concurrent", envInt("HERDR_MCP_MAX_LONG_CONCURRENT", 64), "simultaneous long-poll calls (agent_wait, events_wait, pane_wait_for_output, agent_prompt)")
	flags.IntVar(&common.queueDepth, "queue-depth", envInt("HERDR_MCP_QUEUE_DEPTH", 256), "calls allowed to wait per lane before new ones are shed")
	flags.DurationVar(&common.outageGrace, "outage-grace", envDuration("HERDR_MCP_OUTAGE_GRACE", 2*time.Minute), "how long a call waits for an unreachable Herdr before failing")
	return common
}

// buildRuntime prepares the MCP tool surface. In strict mode (doctor) every
// dependency must be present and agreeing. Otherwise the bridge starts anyway:
// a missing Herdr binary falls back to the cached schema, an unreachable socket
// becomes a parked queue rather than a failed boot, and a protocol that has
// moved on is reported per call instead of crashing the process into a systemd
// restart loop. The whole point is that the MCP endpoint stays answerable while
// Herdr is not.
func buildRuntime(ctx context.Context, flags commonFlags, strict bool) (*runtimeBundle, error) {
	bundle := &runtimeBundle{}

	var schema *herdr.Schema
	var err error
	if strict {
		schema, err = herdr.LoadSchema(ctx, flags.herdrBinary)
		if err != nil {
			return nil, err
		}
	} else {
		cache, cacheErr := herdr.DefaultSchemaCache()
		if cacheErr != nil {
			bundle.Notes = append(bundle.Notes, cacheErr.Error())
		}
		// A wedged `herdr api schema` must not hold the bridge down forever;
		// past this budget the cached schema is the better answer.
		schemaCtx, cancelSchema := context.WithTimeout(ctx, schemaLoadBudget)
		var note string
		schema, note, err = herdr.LoadSchemaCached(schemaCtx, flags.herdrBinary, cache)
		cancelSchema()
		if err != nil {
			return nil, err
		}
		if note != "" {
			bundle.Notes = append(bundle.Notes, note)
		}
	}
	bundle.Protocol = schema.Protocol

	client := &herdr.Client{SocketPath: flags.socket}
	bundle.Client = client
	queue := herdr.NewQueue(ctx, client, schema.Protocol)
	queue.Concurrency = flags.concurrency
	queue.LongConcurrency = flags.longConcurrency
	queue.Backlog = flags.queueDepth
	queue.OutageGrace = flags.outageGrace
	bundle.Queue = queue

	herdrVersion, protocol, pingErr := client.Ping(ctx)
	switch {
	case pingErr != nil && strict:
		return nil, pingErr
	case pingErr != nil:
		queue.MarkUnreachable(pingErr)
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("Herdr socket %s is not answering yet (%v); tools are registered and calls will wait for it", flags.socket, pingErr))
	case protocol != schema.Protocol && strict:
		return nil, fmt.Errorf("protocol mismatch: %s reports schema protocol %d, socket reports %d", flags.herdrBinary, schema.Protocol, protocol)
	default:
		queue.MarkObserved(protocol)
		bundle.HerdrVersion = herdrVersion
		if protocol != schema.Protocol {
			bundle.Notes = append(bundle.Notes, fmt.Sprintf("protocol mismatch: schema protocol %d, socket protocol %d; tool calls will report this until herdr-mcp restarts", schema.Protocol, protocol))
		}
	}

	server, err := mcpserver.New(schema, queue, version, splitCSV(flags.allowMethods), splitCSV(flags.denyMethods))
	if err != nil {
		return nil, err
	}
	bundle.Server = server
	return bundle, nil
}

// healthHandler reports the bridge's own health, with Herdr's reachability
// nested underneath.
//
// The split matters: "ok" now means the MCP endpoint is serving tools, not that
// Herdr is up, because those became different facts the moment the bridge was
// allowed to outlive an outage. A monitor that used to alert on Herdr being
// down must watch herdr.available instead. Only a bridge that cannot serve
// correctly -- today, a protocol that moved out from under its tools -- still
// answers 503.
func healthHandler(bundle *runtimeBundle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		status := bundle.Queue.Availability()
		if !status.Available {
			// A live probe costs nothing here and shortens the window between
			// Herdr returning and /healthz admitting it.
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			if _, protocol, err := bundle.Client.Ping(ctx); err == nil {
				bundle.Queue.MarkObserved(protocol)
				status = bundle.Queue.Availability()
			}
			cancel()
		}
		w.Header().Set("Content-Type", "application/json")
		if status.ProtocolMismatch {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       !status.ProtocolMismatch,
			"version":  version,
			"protocol": bundle.Protocol,
			"tools":    len(bundle.Server.Methods),
			"herdr":    status,
		})
	}
}

func describeVersion(herdrVersion string) string {
	if herdrVersion == "" {
		return "unreachable"
	}
	return herdrVersion
}

func envInt(name string, fallback int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
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
