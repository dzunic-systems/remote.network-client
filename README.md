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

Other commands:

| Command | What it does |
|---|---|
| `remote-client chat --model MODEL "prompt"` | One completion over the same path as `serve` |
| `remote-client doctor` | Can this machine reach a node directly? Sends no inference and costs nothing |
| `remote-client serve --no-direct` | Everything through the router |
| `remote-client version` | Print the version |

## How a request travels

- `serve` asks a router for candidate nodes. The router runs the same
  checks as its own API: credits, limits and the model.
- It dials the first candidate over QUIC, pinned to the node's identity
  key, and sends the request with a short-lived ticket the router signed.
- If a node cannot answer before its first token, the next candidate is
  tried, then the router.
- Requests that need router-side features always go through the router:
  web search, attachments, and `n > 1`.
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

The node agent people run to *serve* models is a separate program:
[dzunic-systems/remote.network](https://github.com/dzunic-systems/remote.network).

Licensed under the Apache License, Version 2.0. See `LICENSE`.
