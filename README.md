# herdr-mcp

`herdr-mcp` exposes the active [Herdr](https://github.com/herdrdev/herdr) session's local socket API as Model Context Protocol tools.

At startup it:

1. reads the selected Herdr binary's versioned schema with `herdr api schema --json`;
2. pings the selected socket and refuses a binary/socket protocol mismatch;
3. registers one named MCP tool per selected socket method; and
4. forwards tool calls over Herdr's newline-delimited local socket protocol.

The tool surface follows the socket method names: `agent.read` becomes `agent_read`, `pane.wait_for_output` becomes `pane_wait_for_output`, and so on. Results are returned as both MCP structured content and JSON text.

## Install

```sh
go install github.com/Orange-County-AI/herdr-mcp/cmd/herdr-mcp@latest
```

Herdr must be running. `herdr-mcp` uses `HERDR_SOCKET_PATH` when set and otherwise targets the default session at the platform's Herdr config directory.

Verify the binary and socket agree:

```sh
herdr-mcp doctor
```

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

## Herdr plugin

The repository is also a Herdr plugin. Installation builds the Go binary in the managed plugin checkout:

```sh
herdr plugin install Orange-County-AI/herdr-mcp --yes
herdr plugin action invoke ocai.herdr-mcp.doctor
```

For an interactive/local HTTP server in a Herdr-managed tab:

```sh
herdr plugin pane open --plugin ocai.herdr-mcp --entrypoint server
```

That pane is not a process supervisor. Use systemd for an always-on Cloudflare Tunnel origin.

## Remote MCP through Cloudflare Tunnel

The HTTP transport binds only to loopback. Point a Cloudflare Tunnel hostname at it, then protect that hostname with a Cloudflare Access **MCP server application** and enable **Managed OAuth**. Cloudflare owns the OAuth 2.0 authorization-code + PKCE flow; `herdr-mcp` remains the resource origin.

This split is deliberate. Claude and ChatGPT need interactive OAuth, and current MCP/OpenAI guidance recommends an established identity provider rather than a bespoke authorization server. Cloudflare Managed OAuth publishes the required discovery metadata, performs dynamic client registration, applies the Access policy, rotates tokens, and forwards the authenticated identity to the origin.

### 1. Run the loopback origin

On Linux with systemd, one command copies the current binary to
`~/.local/bin`, writes and enables the user unit, restarts it, and waits for the
health endpoint:

```sh
herdr-mcp install-service
```

The command resolves the absolute Herdr binary before writing the unit, so the
service does not depend on an interactive shell's `PATH`. It is idempotent and
can also update an existing installation. To select another loopback port:

```sh
herdr-mcp install-service --listen 127.0.0.1:18091
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

## Method policy

By default every non-streaming method in the selected Herdr schema is exposed except `events.subscribe`. A one-shot MCP tool call cannot preserve that subscription's streaming socket lifetime; use `events_wait` or `pane_wait_for_output` instead.

The full surface includes destructive operations such as `server_stop`, `worktree_remove`, `pane_close`, plugin unlinking, and integration uninstalling. Restrict a deployment with exact names or shell-style globs:

```sh
herdr-mcp serve \
  --allow-methods 'ping,session.snapshot,agent.*,pane.read,pane.wait_for_output' \
  --deny-methods 'agent.view.*,agent.rename'
```

Equivalent environment variables:

```dotenv
HERDR_MCP_ALLOW_METHODS=ping,session.snapshot,agent.*,pane.read,pane.wait_for_output
HERDR_MCP_DENY_METHODS=events.subscribe,server.stop,server.live_handoff
```

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
| `--deny-methods` | `HERDR_MCP_DENY_METHODS` | `events.subscribe` |
| `--listen` | `HERDR_MCP_LISTEN` | `127.0.0.1:8091` |
| `--access-team-domain` | `CF_ACCESS_TEAM_DOMAIN` | unset |
| `--access-aud` | `CF_ACCESS_AUD` | unset |

`serve` refuses non-loopback listeners. Remote access belongs behind a tunnel and an authorization policy, not on a public origin port.

## Development

```sh
mise run check
mise run build
```

The bridge has no generated copy of Herdr's API types. Tests cover schema extraction and reference closure, socket request/response behavior, MCP tool forwarding, systemd service installation, and Cloudflare Access JWT validation.

## License

Apache-2.0.
