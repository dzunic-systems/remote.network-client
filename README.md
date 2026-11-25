# remote-client

The consumer client for the [remote.network](https://remote.network)
inference network. It runs a local OpenAI-compatible endpoint, so existing
tools point at `http://127.0.0.1:4141/v1` unchanged. Each chat request
goes straight to the serving node over QUIC when a direct path exists, and
through a router otherwise. The answer is the same either way.

**Status: early beta.** The direct path is switched on per account while
it is being tested. Until yours is, every request goes through the router.
This works the same, it just does not take the direct path.

## Install

macOS and Linux:

```
curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network-client/main/scripts/install.sh | sh
```

On Windows, download `remote-client-windows-amd64.exe` (or `-arm64`) from
the [releases page](https://github.com/dzunic-systems/remote.network-client/releases)
and check it against `checksums.txt`.

## Use

1. Create an API key at https://spine.remote.network (User Settings → API Keys).
2. Store it. The key is kept in `~/.remote/client-api-key` with
   owner-only permissions and is never printed:

   ```
   remote-client login
   ```

3. Run the local endpoint and point your tool at it:

   ```
   remote-client serve
   # OPENAI_BASE_URL=http://127.0.0.1:4141/v1, any API key value
   ```

   A small web UI comes with it: open http://127.0.0.1:4141/ui/ for
   chat with the network's models, every setting with the source it
   resolved from, a key replacement, the same check `doctor` runs, and
   the local tools (below). Turn it off with `--no-ui`.

## Local tools (MCP)

`serve` and `chat` can run tools on this machine and hand them to the
model. Configure servers in `~/.remote/mcp.json` — stdio MCP servers,
same shape as Claude Code's:

```json
{"mcpServers": {"fetch": {"command": "uvx", "args": ["mcp-server-fetch"]}}}
```

With servers configured, tools turn on by default (`tools: off` turns
them off). The web UI and `chat` then run a tool loop; each exposed
tool name is `<server>__<tool>`.

**Permission first.** Every tool starts at `ask`: the UI shows an
inline prompt (Allow once / Always allow this tool / Deny); `chat`
prompts on the terminal, and with no terminal a tool is denied. Modes
live in `~/.remote/permissions.json` (0600) and on the Settings page.

**The privacy boundary:** a tool's result is sent to the node that
serves the model — a third party's machine, unless it is your own node.
Local file contents leave this machine when a tool reads them. The UI
says so before a server's first run, and on the Settings page.

Bounds: at most 10 tool rounds per turn, a tool result is truncated at
64 KiB, a tool call times out after 60 s, and cancelling the turn kills
the in-flight tool call. Logs record the tool name, duration and size —
never arguments or results.

The OpenAI-compatible endpoint never injects tools: a request that
carries its own (Cursor, an SDK) passes through untouched.

Other commands:

| Command | What it does |
|---|---|
| `remote-client chat --model MODEL "prompt"` | One completion over the same path as `serve` |
| `remote-client doctor` | Can this machine reach a node directly? Sends no inference and costs nothing |
| `remote-client serve --no-direct` | Everything through the router |
| `remote-client config show` | Every setting with its effective value and its source |
| `remote-client config set KEY VALUE` | Write one setting to the config file |
| `remote-client version` | Print the version |

## Settings

Every setting resolves in the same order: **flag → environment → config
file (`~/.remote/client.yaml`) → default**, and `config show` tells you
which source won. A value from a flag or the environment is locked for
that run — change the flag or the variable, not the file.

| Setting | Flag | Environment | Config file key | Default |
|---|---|---|---|---|
| Router | `--router` | `REMOTE_ROUTER` | `router` | `https://spine.remote.network` |
| Second router (NAT cross-check) | `--router-alt` | `REMOTE_ROUTER_ALT` | `router_alt` | — |
| Listen address | `--listen` | `REMOTE_LISTEN` | `listen` | `127.0.0.1:4141` |
| Listen port | `--port` | `REMOTE_PORT` | `port` | `4141` (kept on the listen host; conflicts with a `listen` value naming a different port are an error) |
| API key | `--api-key` | `REMOTE_API_KEY` | — (never in the file) | the key file |
| Key file | `--api-key-file` | `REMOTE_API_KEY_FILE` | `api_key_file` | `~/.remote/client-api-key` |
| Direct path | `--no-direct` | `REMOTE_DIRECT` | `direct` | on |
| Local token | `--local-token` | `REMOTE_LOCAL_TOKEN` | — (never in the file) | — |
| Log level | `--verbose` (= debug) | `REMOTE_LOG_LEVEL` | `log_level` | `info` |
| Web UI | `--no-ui` | `REMOTE_UI` | `ui` | on |
| Default model | `--model` (`chat`) | `REMOTE_DEFAULT_MODEL` | `default_model` | — |
| Web search (client chat) | `--web-search` (`chat`) | `REMOTE_WEB_SEARCH` | `web_search` | off |
| MCP config | — | `REMOTE_MCP_CONFIG` | `mcp_config` | `~/.remote/mcp.json` |
| Local tools | — | `REMOTE_TOOLS` | `tools` | on when an MCP config exists |
| Everything's home | — | `REMOTE_HOME` | — | `~/.remote` |

Booleans accept `1/0/true/false/on/off/yes/no`; anything else is a
startup error that names the variable. The API key and the local token
are secrets: they are never written to the config file, never logged,
and shown only as "set" with their source (`config show`, the web UI).
`REMOTE_HOME` moves the key, the config file, the identity key and the
tool config together.

## How a request travels

- `serve` asks a router for candidate nodes. The router runs the same
  checks as its own API: credits, limits and the model.
- It dials the first candidate over QUIC, pinned to the node's identity
  key, and sends the request with a short-lived ticket the router signed.
- If a node cannot answer before its first token, the next candidate is
  tried, then the router.
- Requests that need router-side features always go through the router:
  web search, attachments, and `n > 1`. A turn with web search on always
  rides the router; only plain turns go direct. The web UI shows each
  turn's path ("via direct" / "via router") on the answer.
- With web search AND local tools on the same turn, the router runs its
  search loop around the client's tools: the client's tool calls come
  back to this machine and are executed here (permission and all), never
  by the router.
- Billing does not depend on the path. The node signs a receipt for what
  it served, the router bills it within the ticket's limits, and the
  client's own count is only a cross-check.

## Security

- `serve` listens on loopback only by default. It then accepts requests
  only when they name a loopback host and come from no other web origin,
  so a web page you visit cannot spend your credits through it.
- Listening on any other address requires `--local-token SECRET`. Every
  caller must then send that secret as `Authorization: Bearer SECRET` or
  `X-Local-Token`.
- The web UI under `/ui/` is covered by the same rules, and its
  state-changing calls additionally carry a per-process CSRF token the
  server bakes into the page. The API key never reaches the page: the
  proxy adds it to router-bound requests itself.

The node agent people run to *serve* models is a separate program:
[dzunic-systems/remote.network](https://github.com/dzunic-systems/remote.network).

Licensed under the Apache License, Version 2.0. See `LICENSE`.
