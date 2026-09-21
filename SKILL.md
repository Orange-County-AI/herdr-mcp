---
name: herdr-mcp
description: "Install, configure, run, expose, and troubleshoot herdr-mcp, the MCP bridge for Herdr's socket API. Use when connecting Herdr to Claude, ChatGPT, or another MCP client; installing the loopback systemd service; configuring Cloudflare Tunnel, Access Managed OAuth, or Access JWT validation; restricting exposed Herdr methods; or diagnosing herdr-mcp health, schema, socket, and protocol errors."
---

# Herdr MCP

`herdr-mcp` turns the active Herdr session's local socket methods into MCP tools. It supports stdio for local clients and Streamable HTTP for remote clients.

## Install through Herdr by default

Prefer Herdr's plugin manager. It owns an isolated checkout and build, avoiding
collisions with a different `herdr-mcp` already installed through `GOBIN`:

```bash
herdr plugin install Orange-County-AI/herdr-mcp --yes
herdr plugin action invoke ocai.herdr-mcp.doctor
```

On Linux, install or update the always-on service from the managed plugin:

```bash
herdr plugin action invoke ocai.herdr-mcp.install-service
```

Do not mix plugin and standalone installation unless the user deliberately
chooses which binary owns the service and which appears first on `PATH`.

Use standalone Go installation only when the plugin is unsuitable or a local
stdio client needs a directly addressable executable:

```bash
go install github.com/Orange-County-AI/herdr-mcp/cmd/herdr-mcp@latest
herdr-mcp doctor
```

Herdr must be running. `HERDR_SOCKET_PATH` selects a non-default session socket.
`HERDR_BIN` selects the Herdr binary whose schema should be exposed.

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

With the recommended plugin installation:

```bash
herdr plugin action invoke ocai.herdr-mcp.install-service
```

With a standalone binary:

```bash
herdr-mcp install-service
```

Both copy the selected executable to `~/.local/bin/herdr-mcp`, resolve the
absolute Herdr binary, write `~/.config/systemd/user/herdr-mcp.service`, reload
systemd, enable and restart the unit, and wait for the health endpoint. The
installer uses explicit `--listen` first, then `HERDR_MCP_LISTEN` from the
existing `~/.config/herdr-mcp/env`, then `127.0.0.1:8091`. Set the environment
value before invoking the plugin action if the default port is occupied:

```dotenv
HERDR_MCP_LISTEN=127.0.0.1:18091
```

The standalone command also accepts custom flags:

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
HERDR_MCP_DENY_METHODS=events.subscribe,pane.report_agent,pane.report_agent_session,pane.report_metadata,workspace.report_metadata,pane.clear_agent_authority,pane.release_agent,pane.graphics.*
CF_ACCESS_TEAM_DOMAIN=https://your-team.cloudflareaccess.com
CF_ACCESS_AUD=your-access-application-audience
```

After changing the file:

```bash
systemctl --user restart herdr-mcp.service
curl --fail http://127.0.0.1:8091/healthz
```

An allow list is evaluated first; the deny list always wins. By default, `events.subscribe`, harness-internal lifecycle reporting, and `pane.graphics.*` are hidden so client discovery focuses on agent and pane control. Use `events_wait` or `pane_wait_for_output` instead of the unsupported persistent subscription. Pass `--deny-methods 'events.subscribe'` only when a specialized client genuinely needs the internal protocol methods.

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

## Expect it to outlive Herdr

`serve` and `stdio` start whether or not Herdr is running, and stay up when it
stops. Do not read a failed `herdr-mcp` start as "Herdr is down" any more.

- **Tools** come from `herdr api schema --json` and are cached at
  `$XDG_CACHE_HOME/herdr-mcp/schema.json`, so a start during an upgrade that
  replaced the binary falls back to that cache and says so in the log. Startup
  fails only when neither the binary nor the cache can supply a schema.
- **Calls park** while the socket is unreachable and resume when it answers, so
  a Herdr restart costs latency rather than failed tool calls. Past
  `--outage-grace` (default `2m`) a call fails with an error naming the outage
  and how long it has lasted.
- **A call that already reached Herdr is never resent.** Only a failed dial is
  retried, because only a failed dial is known not to have applied anything.
- **Bursts queue.** `--max-concurrent` (8) bounds simultaneous requests;
  long polls get a separate `--max-long-concurrent` (64) lane so they cannot
  starve ordinary calls. Past `--queue-depth` (256) waiting calls, new ones are
  shed with a saturation error rather than joining a queue that will only time
  out.
- **`/healthz` `ok` means the bridge is serving, not that Herdr is up.** Read
  `herdr.available`, `herdr.down_for_seconds`, `herdr.waiting`, and
  `herdr.in_flight`. `ok:false` with HTTP 503 means the bridge itself cannot
  serve correctly -- currently only a Herdr protocol that no longer matches its
  registered tools. The next schema refresh usually clears it by re-registering
  the tools; restarting `herdr-mcp` forces it immediately.

`doctor` is unchanged and stays strict: it fails when the binary is missing, the
socket is unreachable, or the protocols disagree. Use it, not `/healthz`, to
answer "is Herdr actually reachable right now".

## Drive a saved SSH machine

Herdr 0.9 made saved SSH machines first class, and most tools take an optional
`machine` argument naming one by label or profile id. Omit it for the local
session.

```jsonc
{"name": "machine_list", "arguments": {}}
{"name": "pane_list",    "arguments": {"machine": "minime"}}
{"name": "agent_prompt", "arguments": {"machine": "gigachad", "target": "reviewer",
                                       "text": "Summarize the failing test."}}
```

- **`machine_list` is the only discovery path.** Machine profiles are Herdr
  client configuration; the socket protocol has no `machine.*` method. The
  result also shows which machines the bridge currently holds a connection to.
- **IDs are scoped to one machine.** Two machines can both have `w1:p1` or an
  agent named `reviewer`. Never reuse a locally discovered id remotely: list on
  the machine you intend to drive. Labels are case-sensitive.
- **The far end must already be running Herdr 0.9+.** The bridge probes the
  host for its socket path, forwards that socket over SSH, and refuses a machine
  whose protocol differs from the one its tools were registered from. It never
  starts a remote server.
- **A failed remote call never falls back to local**, and a connection error
  does not prove a mutation was not applied. Inspect remote state before
  retrying anything that mutates.
- **Client-scoped tools are local-only** (`client_window_title_*`,
  `client_shell_surface_set`, `popup_close`, the dismiss tools,
  `server_live_handoff`) and reject a `machine` argument, because there is no
  attached client on the far end.

Connections are lazy, reuse one SSH control master per machine, and are dropped
after `--machine-idle` (default `15m`). `--machines=false` disables routing.

## Expect the tool list to change under you

The bridge re-reads Herdr's schema every `--schema-refresh` (default `5m`) and
re-registers its tools when the document changed, emitting the MCP
`tools/list_changed` notification. Re-list tools when you see it rather than
caching the surface for the life of a session.

This exists because the protocol number is not a staleness signal: Herdr 0.9.1
added `pane.link.resolve` inside protocol 22, so a bridge started against 0.9.0
reported a matching protocol while serving a tool list that was one short.
`/healthz` and `doctor` both report `schema_digest`, which is what actually
moves.

## Treat the tool surface as privileged

The generated tools directly control Herdr. The full schema includes destructive methods such as `server_stop`, `worktree_remove`, `pane_close`, plugin unlinking, and integration uninstalling.

Before exposing a server to another user or agent, decide whether it needs the full schema. Prefer a narrow `HERDR_MCP_ALLOW_METHODS` list for constrained deployments. Require explicit user intent before invoking destructive tools.

## Diagnose failures

- **Socket connection failure:** confirm Herdr is running and inspect `HERDR_SOCKET_PATH`. Tool calls report this only after `--outage-grace` elapses; `herdr-mcp doctor` and `/healthz`'s `herdr.available` answer immediately.
- **Protocol mismatch:** ensure `HERDR_BIN` and the running Herdr server are the same release, then rerun `doctor`. If Herdr changed protocol while the bridge was running, every call reports it and `/healthz` returns 503 until the next `--schema-refresh` re-registers the tools, or until `herdr-mcp` restarts.
- **Calls fail with "queue is saturated":** more than `--queue-depth` calls are already waiting per lane. Check `/healthz` `herdr.waiting`; if Herdr is up, raise `--max-concurrent`, and if it is down, fix that first.
- **Tools are registered but stale:** the log says `using cached Herdr schema`. The `herdr` binary named by `--herdr-bin` could not answer; restart the service once it can.
- **Service starts in a shell but not systemd:** inspect the unit's resolved `--herdr-bin` and the environment file; do not rely on shell-only PATH setup.
- **Remote client receives redirects or cannot register:** enable Cloudflare Access Managed OAuth on the protected application.
- **Origin returns `missing Cloudflare Access assertion`:** verify the request traversed the Access application and that its audience matches `CF_ACCESS_AUD`.
- **Tool is absent:** inspect `HERDR_MCP_ALLOW_METHODS` and `HERDR_MCP_DENY_METHODS`, then restart the service.
- **`events_subscribe` is absent:** this is intentional; use a one-shot wait tool.
- **A `machine` argument is rejected as "not accepted":** either the tool is client-scoped and local-only, or the bridge runs with `--machines=false`. Check `machine_list`; if it is missing too, routing is off.
- **`no saved Herdr machine matches ...`:** the error lists the known labels. They are case-sensitive, and a disabled profile is reported as disabled rather than missing. `herdr machine list --json` is the source of truth.
- **`Herdr is not running on <host>`:** start Herdr there; forwarding never starts a remote server. `ssh <host> herdr status server --json` reproduces the probe.
- **A remote machine is refused on protocol:** the two Herdr installs are different releases. Update both, then let the bridge reload or restart it.
- **A remote call fails to connect:** it was **not** rolled back for you. Inspect remote state with a list tool before retrying a mutation.
