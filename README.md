# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API is an OpenAI-compatible proxy server for [Freebuff](https://freebuff.com). It translates standard OpenAI API requests into Freebuff's backend format, allowing you to use Freebuff's free models with any OpenAI-compatible client, SDK, or CLI tool.

## Features

- **OpenAI Compatible API** — Standard OpenAI endpoints; works with any compatible client out of the box.
- **Freebuff Session Compatibility** — Preserves the current Freebuff waiting-room/session contract, including model-bound session selection and OpenAI-compatible request metadata.
- **Multi-Token Rotation** — Cycle through multiple auth tokens with automatic periodic rotation.
- **HTTP Proxy Support** — Route all outbound traffic through a configurable upstream proxy.

## Getting Auth Tokens

Freebuff2API requires one or more Freebuff **auth tokens**. There are two ways to obtain one:

### Method 1 — Web (Recommended)

Visit **[https://freebuff.llm.pm](https://freebuff.llm.pm)**, log in with your Freebuff account, and your auth token will be displayed directly on the page. Copy it as your **AUTH_TOKENS** — no local installation required.

### Method 2 — Freebuff CLI

Install the Freebuff CLI:

```bash
npm i -g freebuff
```

Run `freebuff` in your terminal — on first launch it will guide you through login.

After logging in, your token is saved to a local credentials file:

| OS | Credentials Path |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

The file looks like:

```json
{
  "default": {
    "id": "user_10293847",
    "name": "Zhang San",
    "email": "zhangsan@example.com",
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7",
    ...
  }
}
```

Only the `authToken` value is needed — copy it as your **AUTH_TOKENS**.

> **Tip:** Log in with multiple accounts and configure all their tokens for higher throughput.

## Configuration

Configuration is managed via a YAML/JSON file and/or environment variables. The file keys and environment variable names are identical. By default the app looks for `config.yaml`, `config.yml`, then `config.json` in the working directory; use `-config` to specify another path.

```json
{
  "LISTEN_ADDR": ":8080",
  "UPSTREAM_BASE_URL": "https://codebuff.com",
  "AUTH_TOKENS": ["eyJhb..."],
  "AUTH_TOKEN_DIR": "tokens.d",
  "ROTATION_INTERVAL": "6h",
  "REQUEST_TIMEOUT": "15m",
  "API_KEYS": [],
  "HTTP_PROXY": ""
}
```

### Reference

| Key / Env Var | Description |
|---|---|
| `LISTEN_ADDR` | Proxy listen address (default `:8080`) |
| `UPSTREAM_BASE_URL` | Freebuff backend URL (default `https://codebuff.com`) |
| `AUTH_TOKENS` | Freebuff auth tokens (JSON array or comma-separated env var) |
| `AUTH_TOKEN_DIR` | Optional directory of token files; plain text, JSON, and YAML token blobs are supported |
| `ROTATION_INTERVAL` | Run rotation interval (default `6h`) |
| `REQUEST_TIMEOUT` | Upstream request timeout (default `15m`) |
| `API_KEYS` | Client API keys for proxy auth (empty = open access) |
| `HTTP_PROXY` | HTTP proxy for outbound requests |

Environment variables provide startup defaults. When a config file is present, its values win on reload. `LISTEN_ADDR` still requires a process restart because the HTTP listener is already bound.

### Runtime Status

- `GET /healthz`: lightweight readiness summary
- `GET /status`: full token/session snapshot, config summary, and available models

## Deployment

### Docker

Pre-built multi-arch images are available on GHCR:

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  ghcr.io/quorinex/freebuff2api:latest
```

Build from source:

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 -e AUTH_TOKENS="token1,token2" Freebuff2API
```

### Build from Source

**Requirements:** Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.json
```

## Links

- [linux.do](https://linux.do)

## Disclaimer

This project has no official affiliation with OpenAI, Codebuff, or Freebuff. All related trademarks and copyrights belong to their respective owners.

All contents within this repository are provided solely for communication, experimentation, and learning, and do not constitute production-ready services or professional advice. This project is provided on an "As-Is" basis, and users must use it at their own risk. The author assumes no liability for any direct or indirect damages resulting from the use, modification, or distribution of this project, nor provides any warranties of any kind, express or implied.

## License

MIT
