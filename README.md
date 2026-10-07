# herdr-mcp

`herdr-mcp` exposes the active [Herdr](https://github.com/herdrdev/herdr) session's local socket API as Model Context Protocol tools.

At startup it:

1. reads the selected Herdr binary's versioned schema with `herdr api schema --json`;
2. pings the selected socket and refuses a binary/socket protocol mismatch;
3. registers one named MCP tool per selected socket method; and
4. forwards tool calls over Herdr's newline-delimited local socket protocol.

The tool surface follows the socket method names: `agent.read` becomes `agent_read`, `pane.wait_for_output` becomes `pane_wait_for_output`, and so on. Results are returned as both MCP structured content and JSON text.

It re-reads that schema on an interval and swaps the tools when it changed, and
most tools take an optional `machine` argument that runs them on one of Herdr's
local sessions or saved SSH machines. See [Saved SSH machines](#saved-ssh-machines) and
[Keeping up with Herdr](#keeping-up-with-herdr).

## Saved SSH machines

Most tools take an optional `machine` argument selecting a Herdr session.
Omitting it keeps using the bridge's startup socket, including an explicit
`--socket` or `HERDR_SOCKET_PATH`. Local routing needs no SSH or saved profile.

```jsonc
{"name": "machine_list", "arguments": {}}
{"name": "agent_list", "arguments": {}}                              // startup socket
{"name": "agent_list", "arguments": {"machine": "local:work"}}        // another local session
{"name": "pane_list", "arguments": {"machine": "minime"}}             // profile's configured session
{"name": "agent_list", "arguments": {"machine": "ssh:0fb972d6/work"}} // specific remote session
```

`machine_list` returns `local_sessions` and `machines`. Each session has its
name, running state, socket path and a stable `selector`; saved machines retain
their labels, IDs and configured session, and add a selector, connection state,
`sessions`, or `sessions_error` if that host could not be listed. Local discovery
uses `herdr session list --json`, so it follows Herdr's socket paths, including
`~/.config/herdr/sessions/<name>/herdr.sock`. Saved profiles come from
`herdr machine list --json`; remote sessions come from the same session-list
command over SSH. Disabled profiles are listed without probing them. `connections` lists every
forwarded session held for a profile; `connected` is true when any is held.

Discovery is live on every roster request and selector resolution: sessions
and machines added later need no bridge restart. Remote roster probes are
read-only, run at most four at once, and share a five-second budget. A sleeping
host gets its own discovery error while the rest of the roster remains usable;
listing never starts a Herdr server or opens a forwarded socket. Health reporting
keeps its existing no-SSH behavior and short discovery cache. During a Herdr
binary upgrade, the roster
can report cached local sessions or profiles with `local_sessions_error` or
`machines_error` identifying the failed source. Dispatch always requires fresh
discovery; cached reporting never authorizes a routed call.

Copy selectors from the roster. `local:<name>` always selects a local session;
`ssh:<profile-id>/<session>` always selects that remote session;
`ssh:<profile-id>` uses the profile's configured session. Components are
percent-encoded when necessary. Existing saved machine labels and IDs still
work, and an unambiguous bare local session name also works. If a local session
and a machine share a name, the bare selector fails and asks for an explicit
selector. Labels and session names are case-sensitive. An empty selector is an
error; omit the argument to use the startup socket.

**IDs are scoped to one session.** Two sessions can both have `w1:p1`, or an
agent named `reviewer`. List on the session you intend to drive and pass the
same selector on subsequent tools. Every routed session uses the bridge's
existing allow/deny method policy, short/long-poll admission queues and outage
settings, through the same authenticated HTTP or stdio transport.

**How it connects.** On first use the bridge asks that host for its Herdr socket
path (`herdr status server --json` over SSH -- the path is not assumable, it is
`~/.config/herdr/herdr.sock` on a normal host and `/dev/shm/herdr/herdr.sock` on
an agent box), then forwards that socket to a local one over an SSH control
master and talks to it exactly as it talks to the local session. That is why
every method works remotely and not just the subset Herdr's own
`herdr --machine` CLI covers. Remote connections are made lazily, verified to speak the
same protocol the tools were registered from, and dropped after `--machine-idle`
without use. The system `ssh` binary does the work, so `~/.ssh/config`
(ProxyJump, IdentityFile, agent, Tailscale aliases) applies unchanged.

**Requirements and limits.** Herdr 0.9+ on both ends, with the remote server
already running -- forwarding never starts one. Stopped sessions, missing sockets,
unknown or ambiguous selectors and protocol mismatches produce explicit errors.
Routed sockets are checked against the registered protocol before delivering
a method, including after a restart. A failed routed call never falls
back to the local session, and a connection error does **not** prove a mutation
was not applied: inspect remote state before retrying. Tools that act on the
attached client (`client_window_title_*`, `client_shell_surface_set`,
`popup_close`, the dismiss tools, `server_live_handoff`) are local-only and
carry no `machine` argument, because there is no attached client on the far end.

Pass `--machines=false` to drop routing entirely, leaving a local-only bridge.

## Keeping up with Herdr

Herdr updates itself, and a bridge whose tools only change on restart is
quietly wrong in between. **The protocol number is not a sufficient staleness
signal**: Herdr 0.9.1 added `pane.link.resolve` inside protocol 22, so a bridge
started against 0.9.0 kept reporting a matching protocol while serving one tool
fewer than the running Herdr had.

So the bridge digests the schema document, re-reads it every
`--schema-refresh` (default 5m), and when the digest moves it re-registers the
tool surface in place and emits the MCP `tools/list_changed` notification.
Removed methods are unregistered, added ones appear, the expected protocol is
re-pointed, and every routed session connection is dropped so the next call
re-verifies the far end against the new protocol. `/healthz` and `doctor` both
report `schema_digest` so a stale bridge is visible rather than inferred.

Set `--schema-refresh 0` to disable it and go back to restart-only updates.

## Herdr plugin

**Recommended.** Let Herdr own the checkout and build instead of placing a
second copy in `GOBIN`:

```sh
herdr plugin install Orange-County-AI/herdr-mcp --yes
herdr plugin action invoke ocai.herdr-mcp.doctor
```

On Linux, install or update the always-on user service from that managed
plugin binary:

```sh
herdr plugin action invoke ocai.herdr-mcp.install-service
```

This avoids collisions with a pre-existing `go install` binary and keeps the
plugin build isolated under Herdr's managed checkout. Do not mix the plugin and
standalone installation methods unless you intentionally manage which binary
is first on `PATH`.

For an interactive HTTP server in a Herdr-managed tab instead of systemd:

```sh
herdr plugin pane open --plugin ocai.herdr-mcp --entrypoint server
```

## Standalone Go binary

Use this only when you are not installing the Herdr plugin, or when you need a
standalone stdio binary:

```sh
go install github.com/Orange-County-AI/herdr-mcp/cmd/herdr-mcp@latest
herdr-mcp doctor
```

Herdr must be running. `herdr-mcp` uses `HERDR_SOCKET_PATH` when set and
otherwise targets the default session at the platform's Herdr config directory.

## Agent skill

Install the repository's root skill globally for supported coding agents:

```sh
npx skills add Orange-County-AI/herdr-mcp --skill herdr-mcp -g
```

Omit `-g` to install it only in the current project. The skill teaches agents
how to select stdio or HTTP, install the user service, configure method policy,
set up Cloudflare Tunnel and Access Managed OAuth, and diagnose socket,
protocol, health, and Access assertion failures.

## Local MCP

For a local client, use stdio:

```sh
herdr-mcp stdio
```

For example, Claude Code can launch it directly:

```sh
claude mcp add --transport stdio --scope user herdr -- herdr-mcp stdio
```


## Native private HTTPS with bearer authentication

The default remains `127.0.0.1:8091` over HTTP; stdio is unchanged. For a
private network, the bridge can serve HTTPS directly through Go's standard TLS
server. No proxy or automatic certificate issuance is involved.

Provision a random bearer secret through your secret manager into a regular
file owned by the service user, with mode `0600` (or `0400`). The file must
contain 32–4096 ASCII characters from `A-Z a-z 0-9 - . _ ~ + /`, optionally
followed by `=` padding and a single LF or CRLF. Symlinks, empty files,
whitespace, and group/other permissions are rejected. Use at least 32 random
bytes encoded as base64. Alternatively inject `HERDR_MCP_BEARER_TOKEN` into the
server environment; an explicitly empty value fails startup. Choose one source.
Never put the credential in command arguments, URLs, logs, unit files, or
client configuration committed to source control. Configure the client's
Authorization header through its secret store.

With owner-provisioned files, start a private listener:

```sh
herdr-mcp serve --listen 192.168.1.20:8091 --allow-private \
  --bearer-token-file /home/you/.config/herdr-mcp/bearer-secret \
  --tls-cert-file /home/you/.config/herdr-mcp/server-chain.pem \
  --tls-key-file /home/you/.config/herdr-mcp/server-key.pem \
  --allowed-hosts herdr.internal.example:8091
```

Private binding is explicit and limited to RFC1918 IPv4 and IPv6 ULA addresses.
Wildcards, public IPs, DNS bind names, and the CGNAT range `100.64.0.0/10` are
refused. Private listeners require both native bearer auth and native TLS.
Loopback HTTP remains available for local clients. Do not send bearer credentials
across a public plaintext link. This feature does not open firewall ports,
publish ports, provision certificates, or change production networking.

Private-key files must be regular, service-user-owned, and inaccessible to
group/others, like bearer files. Ports must be numeric in `1–65535`; zero is
refused. Standard HTTP/HTTPS ports may be omitted from Host and Origin.

The certificate chain must match the client's hostname or IP in its Subject
Alternative Names. Use a certificate trusted by the client, or install the
private CA's root certificate into the client machine's trust store and any
application-specific trust store. On Debian/Ubuntu, place the **CA certificate**
under `/usr/local/share/ca-certificates/` with a `.crt` extension and run
`update-ca-certificates`; macOS clients can trust it through Keychain Access.
Clients with a separate CA configuration must also trust that CA there. Keep the
server private key readable only by the service user. Never disable certificate
verification (`-k`, `InsecureSkipVerify`, or equivalent). TLS 1.2 is the minimum;
incomplete, unreadable, malformed, or mismatched certificate/key configuration
fails startup, with no HTTP fallback.

Bearer authentication covers every HTTP method and route except exact
`GET /healthz`: initialization, tool discovery/calls, SSE streaming,
reconnection, session deletion, and unknown/discovery routes all require the
header on every request. A session ID never authenticates a request. Missing,
wrong, malformed, or duplicate credentials receive `401` with a Bearer
challenge. Token digests are compared in constant time. There is no native
OAuth discovery or version route; use the local `version` command.

Host authorities are checked against the bind address and explicit
`--allowed-hosts` entries, including their ports. Loopback aliases at the bind
port are also accepted. Additional non-loopback hosts require authentication.
An Origin, if present, must exactly match the request's scheme and Host;
otherwise the request receives `403`. Forwarded headers do not grant trust or
bypass authentication. No cross-origin browser access is enabled.

Secrets and TLS files are read once at startup. Replace them atomically and
restart **herdr-mcp** to rotate credentials or renew certificates; a restart
ends old MCP sessions and streams, and clients must initialize again with the
new credential. File replacement alone leaves the old credential and certificate
active. This does not restart Herdr or alter its sessions.

For the Linux service, place source paths and the private-bind opt-in in the
existing `~/.config/herdr-mcp/env` (keep it owner-only):

```dotenv
HERDR_MCP_LISTEN=192.168.1.20:8091
HERDR_MCP_ALLOW_PRIVATE=true
HERDR_MCP_BEARER_TOKEN_FILE=/home/you/.config/herdr-mcp/bearer-secret
HERDR_MCP_TLS_CERT_FILE=/home/you/.config/herdr-mcp/server-chain.pem
HERDR_MCP_TLS_KEY_FILE=/home/you/.config/herdr-mcp/server-key.pem
HERDR_MCP_ALLOWED_HOSTS=herdr.internal.example:8091
```

`install-service` accepts the same HTTP/TLS flags, validates configuration and certificate validity/SAN/trust
before writing or restarting anything, and generates units containing only
source paths and nonsecret settings. It preserves the existing environment file. Installer precedence is explicit flags (including false) > existing service
environment > defaults. Transient shell HTTP defaults are ignored, and a raw
bearer value supplied only to the installer process is rejected. All validated
nonsecret HTTP settings are recorded in the unit; rerun `install-service` when
changing auth mode, source paths, TLS/Host settings or the bind address.
Generated authenticated units require auth at startup, so a missing credential
source cannot silently start an anonymous service. Unvalidated manager
environment settings are removed. The existing service environment supports
simple single-line `NAME=value` assignments, optionally wholly single/double
quoted; security settings with `export`, escapes, multiline values, duplicate
keys or mixed quoting fail installation. A file containing the raw bearer
credential must also be regular, service-user-owned and owner-only. Rotating
the value at an existing credential source requires only a bridge restart. The HTTPS health check uses system
trusted roots and the bind address, so the certificate must include that IP SAN (DNS SAN for
`localhost`) and
the installer host must trust its CA before installation. It never skips TLS
verification. Native HTTPS does not require TLS termination. The supported
external termination path is Cloudflare's authenticated tunnel to a loopback
origin, described below; arbitrary forwarded headers are not trusted.

## Remote MCP through Cloudflare Tunnel

For Cloudflare Access, keep the HTTP transport on loopback. Point a Cloudflare Tunnel hostname at it, then protect that hostname with a Cloudflare Access **MCP server application** and enable **Managed OAuth**. Cloudflare owns the OAuth 2.0 authorization-code + PKCE flow; `herdr-mcp` remains the resource origin.

This split is deliberate. Claude and ChatGPT need interactive OAuth, and current MCP/OpenAI guidance recommends an established identity provider rather than a bespoke authorization server. Cloudflare Managed OAuth publishes the required discovery metadata, performs dynamic client registration, applies the Access policy, rotates tokens, and forwards the authenticated identity to the origin.

### 1. Run the loopback origin

The recommended plugin flow installs the Linux systemd user service with:

```sh
herdr plugin action invoke ocai.herdr-mcp.install-service
```

The action copies the managed plugin binary to `~/.local/bin`, writes and
enables the user unit, restarts it, and waits for the health endpoint.

With the standalone Go installation, run the equivalent CLI command:

```sh
herdr-mcp install-service
```

Both forms resolve the absolute Herdr binary before writing the unit, are
idempotent, and can update an existing installation. `install-service` first
honors an explicit `--listen`, then `HERDR_MCP_LISTEN` in the existing service
environment file, and finally defaults to `127.0.0.1:8091`. Configure a
non-default port before invoking the plugin action:

```dotenv
# ~/.config/herdr-mcp/env
HERDR_MCP_LISTEN=127.0.0.1:18091
```

`deploy/herdr-mcp.service` remains available as a manual template.

If the service does not inherit the correct session socket, add it to `~/.config/herdr-mcp/env`:

```dotenv
HERDR_SOCKET_PATH=/home/you/.config/herdr/herdr.sock
```

### 2. Route the tunnel

Merge `deploy/cloudflared-ingress.yml` into the named tunnel's configuration, replacing `herdr-mcp.example.com`, then validate and restart the existing `cloudflared` service:

```yaml
- hostname: herdr-mcp.example.com
  service: http://127.0.0.1:8091
```

Keep the origin loopback-only. Cloudflare Tunnel is outbound-only; no firewall port should be opened.

### 3. Enable Access Managed OAuth

In **Zero Trust → Access controls → Applications**:

1. create an MCP server application for the public hostname;
2. add an allow policy for the intended users;
3. enable **Managed OAuth** under Advanced settings;
4. allow the redirect URI classes needed by Claude and ChatGPT; and
5. use a short access-token lifetime with a longer grant-session duration.

Cloudflare's MCP server application contract requires the origin to validate the signed Access assertion. Configure the application audience and team domain in `~/.config/herdr-mcp/env`:

```dotenv
CF_ACCESS_TEAM_DOMAIN=https://your-team.cloudflareaccess.com
CF_ACCESS_AUD=your-access-application-audience
HERDR_MCP_ALLOWED_HOSTS=herdr-mcp.example.com
```

`HERDR_MCP_ALLOWED_HOSTS` must name the tunnel hostname: cloudflared forwards
it as the Host header, and the bridge refuses to start under Access without it.

Bearer and Cloudflare settings are mutually exclusive: remove the bearer source
when selecting Access, and remove both Access settings when selecting bearer.
There is no OR fallback between them. After adding these settings, rerun
`herdr-mcp install-service` (or its plugin action) to validate and record them
before restarting the bridge.

When both Access values are present, `herdr-mcp` requires `Cf-Access-Jwt-Assertion` on `/mcp`, fetches Cloudflare's current RSA signing keys, and verifies the signature, issuer, audience, and expiry on every request. `GET /healthz` is an unprivileged health probe: tunnel requests receive only
`{"ok":true}` (or `false` with 503). Direct loopback probes retain existing
diagnostics only in the unauthenticated default mode. Access always returns
the minimal probe, including when the tunnel rewrites Host to loopback. Bearer mode always returns
only the minimal probe, including on loopback. All other health methods require
authentication first, then return 405. Access accepts an HTTPS Origin matching
the explicitly allowed tunnel Host even though its local connection is HTTP.

Do not put another origin OAuth server behind Access Managed OAuth. Managed OAuth replaces the protected application's `401` behavior by design.

### 4. Connect Claude or ChatGPT

Use the public MCP URL:

```text
https://herdr-mcp.example.com/mcp
```

Claude can add it as a remote HTTP MCP server. In ChatGPT, add the URL as a custom MCP-backed plugin/app. The first connection opens the Cloudflare Access login flow; no client secret is copied into either product.

## Availability and queueing

`serve` and `stdio` start whether or not Herdr is running, and stay up when it
goes away. That covers the two windows where the bridge used to be unreachable
at exactly the wrong moment: a Herdr restart, and an upgrade that swaps the
`herdr` binary out from under it.

**Starting without Herdr.** Tools come from `herdr api schema --json`, which
needs the binary but not a running session. The parsed document is cached at
`$XDG_CACHE_HOME/herdr-mcp/schema.json` on every successful read, so a start
during an upgrade falls back to the cached copy and logs that it did. With
neither a binary nor a cache there is no honest tool surface, and startup fails.

**Calls wait instead of failing.** A call that cannot reach the socket is parked
and released as soon as Herdr answers, so a restart shows up as latency rather
than a wall of errors. One shared prober does the reconnecting, so a hundred
parked callers are still one connection attempt every 500ms. Past
`--outage-grace` a caller gives up with an error naming the outage and its
duration. That grace is measured from each call's own arrival, so a call that
lands an hour into an outage still gets a full period rather than an instant
failure.

**Only failed dials are retried.** Once a request is on the wire, a failure is
ambiguous -- Herdr may have applied `pane_close` and died before answering --
so it is reported, never resent. A failed dial wrote nothing, which is what
makes it safe to retry.

**Bursts are metered, not dropped.** `--max-concurrent` bounds simultaneous
requests against Herdr; the rest queue. Long polls (`agent_wait`, `agent_prompt`,
`events_wait`, `pane_wait_for_output`) hold a connection for minutes while
consuming no Herdr capacity, so they get their own `--max-long-concurrent` lane
and cannot starve ordinary calls. `--queue-depth` bounds each lane's waiting
room; past it, new calls are shed immediately. That is deliberate: an unbounded
queue during an outage only guarantees that everyone waits and then fails.

**Health.** `GET /healthz` reports the bridge, with Herdr nested underneath:

```json
{"ok": true, "protocol": 22, "tools": 94, "schema_digest": "sha256:226d4ecb...",
 "herdr": {"available": false, "down_for_seconds": 12, "waiting": 3, "in_flight": 8,
           "detail": "Herdr socket unreachable; calls are parked until it returns"},
 "machines": [{"id": "0fb972d6...", "label": "minime", "target": "minime",
               "enabled": true, "connected": true, "herdr_version": "0.9.1",
               "protocol": 22, "idle_for_seconds": 41}]}
```

`machines` lists every saved profile and marks the ones this bridge holds a
connection to. Reporting never dials, so polling `/healthz` cannot cost an SSH
handshake to a sleeping laptop.

`ok` now means the MCP endpoint is serving tools, which stopped being the same
fact as "Herdr is up" the moment the bridge was allowed to outlive an outage.
**A monitor that alerted on Herdr being down must watch `herdr.available`.**
`ok` goes false, with HTTP 503, only when the bridge itself cannot serve
correctly -- today that means Herdr came back on a different protocol than the
one its registered tools were built from. Calls then return that mismatch rather
than sending well-formed requests with the wrong meaning. The next schema
refresh normally clears it on its own by re-registering the tools; restarting
`herdr-mcp` forces it immediately. `doctor` is unchanged and still strict: it fails if the binary,
the socket, or the protocols disagree.

## Method policy

By default every non-streaming client-facing method in the selected Herdr schema is exposed. `events.subscribe` is omitted because a one-shot MCP tool call cannot preserve that streaming socket lifetime; use `events_wait` or `pane_wait_for_output` instead. Harness-internal lifecycle reporting (`pane.report_*`, `pane.release_agent`, and `pane.clear_agent_authority`) and terminal graphics (`pane.graphics.*`, removed outright in Herdr 0.9.2) are also omitted to keep client tool discovery focused on agent and pane control. `server.ssh_agent.register` (Herdr 0.9.2) is omitted for the same reason as `events.subscribe`: a registration lasts only as long as the API connection that made it, and the bridge dials a fresh connection per call, so the tool could only register and immediately unregister.

To re-expose the internal methods for a specialized client while keeping the unsupported subscription excluded, pass `--deny-methods 'events.subscribe'`.
The full surface includes destructive operations such as `server_stop`, `worktree_remove`, `pane_close`, plugin unlinking, and integration uninstalling. Restrict a deployment with exact names or shell-style globs:

```sh
herdr-mcp serve \
  --allow-methods 'ping,session.snapshot,agent.*,pane.read,pane.wait_for_output' \
  --deny-methods 'agent.view.*,agent.rename'
```

Equivalent environment variables:

```dotenv
HERDR_MCP_ALLOW_METHODS=ping,session.snapshot,agent.*,pane.read,pane.wait_for_output
HERDR_MCP_DENY_METHODS=events.subscribe,pane.report_agent,pane.report_agent_session,pane.report_metadata,workspace.report_metadata,pane.clear_agent_authority,pane.release_agent,pane.graphics.*,server.ssh_agent.register

An allow list is evaluated first; the deny list always wins.

## Commands

```text
herdr-mcp serve [flags]            Streamable HTTP/HTTPS at /mcp plus GET /healthz
herdr-mcp stdio [flags]            MCP over stdin/stdout
herdr-mcp doctor [flags]           schema/socket compatibility check
herdr-mcp install-service [flags]  install and start a systemd user service
herdr-mcp version
```

Common configuration:

| Flag | Environment | Default |
| --- | --- | --- |
| `--socket` | `HERDR_SOCKET_PATH` | default Herdr session socket |
| `--herdr-bin` | `HERDR_BIN` | `herdr` |
| `--allow-methods` | `HERDR_MCP_ALLOW_METHODS` | all methods |
| `--deny-methods` | `HERDR_MCP_DENY_METHODS` | internal reporting, graphics, and `events.subscribe` |
| `--listen` | `HERDR_MCP_LISTEN` | `127.0.0.1:8091` |
| `--require-auth` | none (generated authenticated service invariant) | `false` |
| `--allow-private` | `HERDR_MCP_ALLOW_PRIVATE` | `false` |
| `--bearer-token-file` | `HERDR_MCP_BEARER_TOKEN_FILE` | unset |
| environment only | `HERDR_MCP_BEARER_TOKEN` | unset |
| `--tls-cert-file` | `HERDR_MCP_TLS_CERT_FILE` | unset |
| `--tls-key-file` | `HERDR_MCP_TLS_KEY_FILE` | unset |
| `--allowed-hosts` | `HERDR_MCP_ALLOWED_HOSTS` | bind authority and loopback aliases |
| `--access-team-domain` | `CF_ACCESS_TEAM_DOMAIN` | unset |
| `--access-aud` | `CF_ACCESS_AUD` | unset |
| `--max-concurrent` | `HERDR_MCP_MAX_CONCURRENT` | `8` |
| `--max-long-concurrent` | `HERDR_MCP_MAX_LONG_CONCURRENT` | `64` |
| `--queue-depth` | `HERDR_MCP_QUEUE_DEPTH` | `256` |
| `--outage-grace` | `HERDR_MCP_OUTAGE_GRACE` | `2m` |
| `--machines` | `HERDR_MCP_MACHINES` | `true` |
| `--machine-idle` | `HERDR_MCP_MACHINE_IDLE` | `15m` |
| `--schema-refresh` | `HERDR_MCP_SCHEMA_REFRESH` | `5m` |

`serve` refuses wildcard/public listeners and unauthenticated private listeners.
Use native authenticated private HTTPS, or Cloudflare Access with a loopback origin.

## Development

```sh
mise run check
mise run build
```

The bridge has no generated copy of Herdr's API types. Tests cover schema extraction and reference closure, socket request/response behavior, MCP tool forwarding, stdio, systemd service installation, Cloudflare Access JWT
validation, bearer/session authentication, Host/Origin checks, and native HTTPS
with trusted and untrusted test CAs. Run `go test ./...`, `go test -race ./...`,
`go vet ./...`, and `go build ./...` before handoff.

## License

Apache-2.0.
