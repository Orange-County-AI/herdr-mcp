---
name: herdr-mcp
description: "Install, configure, run, expose, and troubleshoot herdr-mcp, the MCP bridge for Herdr's socket API. Use when connecting Herdr to Claude, ChatGPT, or another MCP client; installing the loopback systemd service; configuring Cloudflare Tunnel, Access Managed OAuth, or Access JWT validation; restricting exposed Herdr methods; or diagnosing herdr-mcp health, schema, socket, and protocol errors."
---

# Herdr MCP

`herdr-mcp` turns the active Herdr session's local socket methods into MCP tools. It supports stdio for local clients and Streamable HTTP for remote clients.

## Establish the installed state

Check the binaries and live socket before changing configuration:

```bash
command -v herdr
command -v herdr-mcp
herdr-mcp version
herdr-mcp doctor
```

`doctor` is the authority for compatibility. It loads `herdr api schema --json`, pings the selected socket, and refuses a protocol mismatch.

If `herdr-mcp` is missing and Go is available:

```bash
go install github.com/Orange-County-AI/herdr-mcp/cmd/herdr-mcp@latest
```

Herdr must be running. `HERDR_SOCKET_PATH` selects a non-default or named session socket. `HERDR_BIN` selects the Herdr binary whose schema should be exposed.

## Choose the transport

Use stdio when the MCP client runs on the same machine:

```bash
herdr-mcp stdio
```

For Claude Code:

```bash
claude mcp add --transport stdio --scope user herdr -- herdr-mcp stdio
```

Use Streamable HTTP only for a loopback origin that a private network or Cloudflare Tunnel reaches:

```bash
herdr-mcp serve --listen 127.0.0.1:8091
```

Never bind the origin to a public interface. The CLI refuses non-loopback listeners.

## Install the Linux user service

On Linux with systemd, prefer the idempotent installer over manually copying the binary and unit:

```bash
herdr-mcp install-service
```

It copies the current executable to `~/.local/bin/herdr-mcp`, resolves the absolute Herdr binary, writes `~/.config/systemd/user/herdr-mcp.service`, reloads systemd, enables and restarts the unit, and waits for `http://127.0.0.1:8091/healthz`.

Use a different loopback port when required:

```bash
herdr-mcp install-service --listen 127.0.0.1:18091
```

Inspect failures with:

```bash
systemctl --user status herdr-mcp.service
journalctl --user -u herdr-mcp.service -n 100
herdr-mcp doctor
```

## Configure the environment

The service reads the optional file `~/.config/herdr-mcp/env`. Do not overwrite unrelated existing values. Common entries are:

```dotenv
HERDR_SOCKET_PATH=/home/you/.config/herdr/herdr.sock
HERDR_MCP_ALLOW_METHODS=ping,session.snapshot,agent.*,pane.read,pane.wait_for_output
HERDR_MCP_DENY_METHODS=events.subscribe,server.stop,server.live_handoff
CF_ACCESS_TEAM_DOMAIN=https://your-team.cloudflareaccess.com
CF_ACCESS_AUD=your-access-application-audience
```

After changing the file:

```bash
systemctl --user restart herdr-mcp.service
curl --fail http://127.0.0.1:8091/healthz
```

An allow list is evaluated first; the deny list always wins. By default all schema methods except `events.subscribe` are exposed. Use `events_wait` or `pane_wait_for_output` instead of the unsupported persistent subscription.

## Expose it through Cloudflare

For remote Claude or ChatGPT access:

1. Route a Cloudflare Tunnel hostname to `http://127.0.0.1:8091`.
2. Create a Cloudflare Access MCP server application for that hostname.
3. Add an allow policy for the intended users.
4. Enable Access Managed OAuth.
5. Configure the redirect URI classes required by the clients.
6. Put `CF_ACCESS_TEAM_DOMAIN` and `CF_ACCESS_AUD` in the service environment file so the origin validates `Cf-Access-Jwt-Assertion`.
7. Give the client `https://<hostname>/mcp`.

Cloudflare owns the authorization-code + PKCE flow. Do not put a second origin OAuth server behind Access Managed OAuth. Keep the tunnel origin loopback-only and do not open a firewall port.

## Treat the tool surface as privileged

The generated tools directly control Herdr. The full schema includes destructive methods such as `server_stop`, `worktree_remove`, `pane_close`, plugin unlinking, and integration uninstalling.

Before exposing a server to another user or agent, decide whether it needs the full schema. Prefer a narrow `HERDR_MCP_ALLOW_METHODS` list for constrained deployments. Require explicit user intent before invoking destructive tools.

## Diagnose failures

- **Socket connection failure:** confirm Herdr is running and inspect `HERDR_SOCKET_PATH`.
- **Protocol mismatch:** ensure `HERDR_BIN` and the running Herdr server are the same release, then rerun `doctor`.
- **Service starts in a shell but not systemd:** inspect the unit's resolved `--herdr-bin` and the environment file; do not rely on shell-only PATH setup.
- **Remote client receives redirects or cannot register:** enable Cloudflare Access Managed OAuth on the protected application.
- **Origin returns `missing Cloudflare Access assertion`:** verify the request traversed the Access application and that its audience matches `CF_ACCESS_AUD`.
- **Tool is absent:** inspect `HERDR_MCP_ALLOW_METHODS` and `HERDR_MCP_DENY_METHODS`, then restart the service.
- **`events_subscribe` is absent:** this is intentional; use a one-shot wait tool.
