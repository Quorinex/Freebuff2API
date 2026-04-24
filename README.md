# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API is a compatibility-focused proxy for [Freebuff](https://freebuff.com). It translates client requests into the current Freebuff backend contract so you can expose a stable API to OpenAI-compatible clients, Claude-compatible clients, and tools that expect the OpenAI Responses API.

## Features

- OpenAI-compatible `POST /v1/chat/completions`
- OpenAI-compatible `POST /v1/responses`
- Claude-compatible `POST /v1/messages`
- Claude-compatible `POST /v1/messages/count_tokens`
- `GET /v1/models` model discovery
- Freebuff waiting-room and model-bound session handling
- Stable retryable proxy errors such as `waiting_room_queued`, `session_switch_in_progress`, and `token_pool_unavailable`
- Automatic token disabling when upstream reports a banned token
- YAML/JSON config loading with runtime hot reload
- Token directory loading via `AUTH_TOKEN_DIR`
- Runtime diagnostics via `GET /healthz` and `GET /status`
- Optional outbound HTTP proxy support

## Auth Tokens

Freebuff2API needs one or more Freebuff auth tokens.

### Method 1: Web

Visit **[https://freebuff.llm.pm](https://freebuff.llm.pm)**, sign in with your Freebuff account, and copy the displayed auth token.

### Method 2: Freebuff CLI

Install the CLI:

```bash
npm i -g freebuff
```

Run `freebuff` and finish the login flow. The token is then stored locally:

| OS | Credentials Path |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

Example:

```json
{
  "default": {
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7"
  }
}
```

Only the `authToken` value is required.

## Configuration

The server accepts YAML or JSON config files. By default it looks for `config.yaml`, then `config.yml`, then `config.json` in the working directory. You can also pass a path with `-config`.

Example:

```yaml
LISTEN_ADDR: ":8080"
UPSTREAM_BASE_URL: "https://www.codebuff.com"
AUTH_TOKENS:
  - "token-1"
  - "token-2"
AUTH_TOKEN_DIR: "tokens.d"
ROTATION_INTERVAL: "6h"
REQUEST_TIMEOUT: "15m"
API_KEYS: []
HTTP_PROXY: ""
```

### Reference

| Key / Env Var | Description |
|---|---|
| `LISTEN_ADDR` | Proxy listen address. Default: `:8080` |
| `UPSTREAM_BASE_URL` | Upstream Freebuff backend URL. Default: `https://www.codebuff.com` |
| `AUTH_TOKENS` | Inline auth tokens. JSON array in files, comma-separated in env |
| `AUTH_TOKEN_DIR` | Optional directory of token files. Plain text, JSON, and YAML token blobs are supported |
| `ROTATION_INTERVAL` | Run rotation interval. Default: `6h` |
| `REQUEST_TIMEOUT` | Upstream request timeout. Default: `15m` |
| `API_KEYS` | Optional client-facing API keys. Empty means open access |
| `HTTP_PROXY` | Optional outbound HTTP proxy |

Notes:

- Environment variables provide startup defaults.
- If a config file is loaded, runtime reloads use the file as the source of truth.
- `LISTEN_ADDR` still requires a process restart because the HTTP listener is already bound.

## Runtime Status

- `GET /healthz`: lightweight readiness summary
- `GET /status`: full token/session snapshot, active config summary, and available models

## Deployment

### Docker

Simple env-based run:

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  ghcr.io/quorinex/freebuff2api:latest
```

Recommended hot-reload setup:

```bash
mkdir -p runtime/tokens.d
cat > runtime/config.yaml <<'EOF'
LISTEN_ADDR: ":8080"
UPSTREAM_BASE_URL: "https://www.codebuff.com"
AUTH_TOKEN_DIR: "/runtime/tokens.d"
ROTATION_INTERVAL: "6h"
REQUEST_TIMEOUT: "15m"
API_KEYS: []
HTTP_PROXY: ""
EOF

printf '%s\n' 'token-1' > runtime/tokens.d/token-1.txt
printf '%s\n' 'token-2' > runtime/tokens.d/token-2.txt

docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -v "$(pwd)/runtime:/runtime" \
  ghcr.io/quorinex/freebuff2api:latest \
  -config /runtime/config.yaml
```

Build from source:

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 -e AUTH_TOKENS="token1,token2" Freebuff2API
```

### Build from Source

Requirements: Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.yaml
```

## Codex CLI

Freebuff2API can be used as a custom provider for Codex CLI via the OpenAI `Responses API`.

Add a dedicated profile to `~/.codex/config.toml`:

```toml
[profiles.freebuff]
model = "your-model-id"
model_provider = "freebuff"
model_reasoning_effort = "high"
model_reasoning_summary = "none"
model_verbosity = "medium"
model_catalog_json = "C:\\Users\\<username>\\.codex\\freebuff-model-catalog.json"

[model_providers.freebuff]
name = "Freebuff"
base_url = "https://your-gateway.example/v1"
wire_api = "responses"
experimental_bearer_token = "your-client-api-key"
```

Create `~/.codex/freebuff-model-catalog.json` and register the models exposed by your gateway. At minimum, include the same model id you set in the profile.

Codex CLI currently expects full model metadata for custom providers, not just a list of model ids. The most reliable approach is:

1. Run `codex debug models`
2. Copy a model entry with similar capabilities
3. Replace fields such as `slug`, `display_name`, and any capability metadata that should differ for your gateway model
4. Save the resulting `models` array to `freebuff-model-catalog.json`

Notes:

- `base_url` should point to your gateway's `/v1` root.
- `wire_api` must be `responses`.
- A custom `model_catalog_json` avoids Codex CLI fallback metadata warnings for non-OpenAI model ids.
- If your server enforces `API_KEYS`, replace `experimental_bearer_token` with a real client key.
- Keep the profile `model` and the catalog entry `slug` in sync with whatever model ids your gateway currently exposes.

Then launch Codex with:

```bash
codex -p freebuff
```

## Claude Code

Freebuff2API can also be used as a Claude Code gateway through the Anthropic-compatible endpoints.

Example `~/.claude/settings.json`:

```json
{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "ANTHROPIC_API_KEY": "your-client-api-key",
    "ANTHROPIC_BASE_URL": "https://your-gateway.example",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "your-sonnet-model-id",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "Sonnet via gateway",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "your-opus-model-id",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME": "Opus via gateway",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "your-haiku-model-id",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": "Haiku via gateway",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "ENABLE_TOOL_SEARCH": "true",
    "NO_PROXY": "localhost"
  },
  "permissions": {
    "defaultMode": "bypassPermissions",
    "skipDangerousModePermissionPrompt": true
  },
  "effortLevel": "high"
}
```

Notes:

- `ANTHROPIC_BASE_URL` should be the gateway root and should not include `/v1`.
- Map the `ANTHROPIC_DEFAULT_*_MODEL` variables to whatever model ids your gateway currently exposes.
- Keep `skipDangerousModePermissionPrompt` inside `permissions`; the top-level key is unnecessary.
- If your gateway requires client auth, use a real key instead of the placeholder value.

## Disclaimer

This project is not affiliated with OpenAI, Codebuff, or Freebuff. All related trademarks belong to their respective owners.

This repository is provided for communication, experimentation, and learning. It is not production advice and is provided on an "as-is" basis. Use it at your own risk.

## License

MIT
