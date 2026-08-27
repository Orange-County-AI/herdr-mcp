# herdr-mcp

`herdr-mcp` exposes the active [Herdr](https://github.com/herdrdev/herdr) session's local socket API as Model Context Protocol tools.

At startup it:

1. reads the selected Herdr binary's versioned schema with `herdr api schema --json`;
2. pings the selected socket and refuses a binary/socket protocol mismatch;
3. registers one named MCP tool per selected socket method; and
4. forwards tool calls over Herdr's newline-delimited local socket protocol.

The tool surface follows the socket method names: `agent.read` becomes `agent_read`, `pane.wait_for_output` becomes `pane_wait_for_output`, and so on. Results are returned as both MCP structured content and JSON text.

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


## Remote MCP through Cloudflare Tunnel

The HTTP transport binds only to loopback. Point a Cloudflare Tunnel hostname at it, then protect that hostname with a Cloudflare Access **MCP server application** and enable **Managed OAuth**. Cloudflare owns the OAuth 2.0 authorization-code + PKCE flow; `herdr-mcp` remains the resource origin.

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
```

When both values are present, `herdr-mcp` requires `Cf-Access-Jwt-Assertion` on `/mcp`, fetches Cloudflare's current RSA signing keys, and verifies the signature, issuer, audience, and expiry on every request. `/healthz` remains an unprivileged origin health check.

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
{"ok": true, "protocol": 20, "tools": 84,
 "herdr": {"available": false, "down_for_seconds": 12, "waiting": 3, "in_flight": 8,
           "detail": "Herdr socket unreachable; calls are parked until it returns"}}
```

`ok` now means the MCP endpoint is serving tools, which stopped being the same
fact as "Herdr is up" the moment the bridge was allowed to outlive an outage.
**A monitor that alerted on Herdr being down must watch `herdr.available`.**
`ok` goes false, with HTTP 503, only when the bridge itself cannot serve
correctly -- today that means Herdr came back on a different protocol than the
one its registered tools were built from. Calls then return that mismatch with
the fix (restart `herdr-mcp`) rather than sending well-formed requests with the
wrong meaning. `doctor` is unchanged and still strict: it fails if the binary,
the socket, or the protocols disagree.

## Method policy

By default every non-streaming client-facing method in the selected Herdr schema is exposed. `events.subscribe` is omitted because a one-shot MCP tool call cannot preserve that streaming socket lifetime; use `events_wait` or `pane_wait_for_output` instead. Harness-internal lifecycle reporting (`pane.report_*`, `pane.release_agent`, and `pane.clear_agent_authority`) and terminal graphics (`pane.graphics.*`) are also omitted to keep client tool discovery focused on agent and pane control.

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
HERDR_MCP_DENY_METHODS=events.subscribe,pane.report_agent,pane.report_agent_session,pane.report_metadata,workspace.report_metadata,pane.clear_agent_authority,pane.release_agent,pane.graphics.*

An allow list is evaluated first; the deny list always wins.

## Commands

```text
herdr-mcp serve [flags]            Streamable HTTP at /mcp plus GET /healthz
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
| `--access-team-domain` | `CF_ACCESS_TEAM_DOMAIN` | unset |
| `--access-aud` | `CF_ACCESS_AUD` | unset |
| `--max-concurrent` | `HERDR_MCP_MAX_CONCURRENT` | `8` |
| `--max-long-concurrent` | `HERDR_MCP_MAX_LONG_CONCURRENT` | `64` |
| `--queue-depth` | `HERDR_MCP_QUEUE_DEPTH` | `256` |
| `--outage-grace` | `HERDR_MCP_OUTAGE_GRACE` | `2m` |

`serve` refuses non-loopback listeners. Remote access belongs behind a tunnel and an authorization policy, not on a public origin port.

## Development

```sh
mise run check
mise run build
```

The bridge has no generated copy of Herdr's API types. Tests cover schema extraction and reference closure, socket request/response behavior, MCP tool forwarding, systemd service installation, and Cloudflare Access JWT validation.

## License

Apache-2.0.
