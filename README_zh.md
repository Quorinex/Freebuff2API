# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API 是一个以兼容性和使用体验为重点的 [Freebuff](https://freebuff.com) 代理服务。它会把客户端请求转换成当前 Freebuff 后端要求的协议格式，对外提供稳定的 OpenAI 兼容接口、Claude 兼容接口，以及 OpenAI Responses API 兼容接口。

## 功能特性

- OpenAI 兼容 `POST /v1/chat/completions`
- OpenAI 兼容 `POST /v1/responses`
- Claude 兼容 `POST /v1/messages`
- Claude 兼容 `POST /v1/messages/count_tokens`
- `GET /v1/models` 模型发现接口
- 兼容 Freebuff 当前 waiting-room 和按模型绑定的 session 协议
- 返回稳定的可重试错误码，例如 `waiting_room_queued`、`session_switch_in_progress`、`token_pool_unavailable`
- 上游返回 banned token 时自动禁用对应 token
- 支持 YAML / JSON 配置文件和运行时热加载
- 支持通过 `AUTH_TOKEN_DIR` 从目录加载 token
- 提供 `GET /healthz` 和 `GET /status` 运行状态接口
- 支持上游 HTTP 代理

## 获取 Auth Token

Freebuff2API 需要一个或多个 Freebuff auth token。

### 方式一：网页获取

访问 **[https://freebuff.llm.pm](https://freebuff.llm.pm)**，登录你的 Freebuff 账号后，复制页面展示的 auth token。

### 方式二：Freebuff CLI

安装 CLI：

```bash
npm i -g freebuff
```

运行 `freebuff` 并完成登录流程。登录后 token 会保存在本地凭证文件中：

| 系统 | 凭证路径 |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

示例：

```json
{
  "default": {
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7"
  }
}
```

只需要取出 `authToken` 的值即可。

## 配置说明

程序支持 YAML 或 JSON 配置文件。默认会按顺序查找当前目录下的 `config.yaml`、`config.yml`、`config.json`。也可以通过 `-config` 显式指定路径。

示例：

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

### 配置项

| 配置项 / 环境变量 | 说明 |
|---|---|
| `LISTEN_ADDR` | 服务监听地址，默认 `:8080` |
| `UPSTREAM_BASE_URL` | 上游 Freebuff 地址，默认 `https://www.codebuff.com` |
| `AUTH_TOKENS` | 直接写在配置中的 token；文件中是数组，环境变量中用逗号分隔 |
| `AUTH_TOKEN_DIR` | 可选 token 目录，支持纯文本、JSON、YAML 三种文件格式 |
| `ROTATION_INTERVAL` | run 轮换间隔，默认 `6h` |
| `REQUEST_TIMEOUT` | 上游请求超时时间，默认 `15m` |
| `API_KEYS` | 对外暴露给客户端的 API Key；留空表示不鉴权 |
| `HTTP_PROXY` | 可选的上游 HTTP 代理 |

补充说明：

- 环境变量用于提供启动时默认值。
- 如果加载了配置文件，运行时热更新会以配置文件内容为准。
- `LISTEN_ADDR` 修改后仍然需要重启进程，因为监听端口已经绑定。

## 运行状态接口

- `GET /healthz`：轻量级健康摘要
- `GET /status`：完整 token / session 状态、当前配置摘要、可用模型列表

## 部署方式

### Docker

最简单的环境变量启动方式：

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  ghcr.io/quorinex/freebuff2api:latest
```

推荐的热加载目录挂载方式：

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

从源码构建镜像：

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 -e AUTH_TOKENS="token1,token2" Freebuff2API
```

### 源码编译

要求：Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.yaml
```

## Codex CLI 配置

Freebuff2API 可以通过 OpenAI `Responses API` 作为 Codex CLI 的自定义 provider 使用。

在 `~/.codex/config.toml` 中增加一个独立 profile：

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

同时创建 `~/.codex/freebuff-model-catalog.json`，把网关当前暴露出来的模型写进去。至少要包含 profile 里设置的同一个模型 id。

目前 Codex CLI 对自定义 provider 的 model catalog 要求是完整 metadata，不只是模型 id 列表。更稳的做法是：

1. 运行 `codex debug models`
2. 复制一个能力相近的模型条目
3. 替换其中的 `slug`、`display_name` 以及需要调整的能力字段
4. 把生成后的 `models` 数组保存为 `freebuff-model-catalog.json`

说明：

- `base_url` 要写到网关的 `/v1`
- `wire_api` 必须是 `responses`
- `model_catalog_json` 用来给非 OpenAI 官方模型补 metadata
- 如果服务端启用了 `API_KEYS`，把 `experimental_bearer_token` 换成真实的客户端 key
- profile 里的 `model` 和 catalog 里的 `slug` 必须始终与网关当前实际暴露的模型 id 保持一致

启动方式：

```bash
codex -p freebuff
```

## Claude Code 配置

Freebuff2API 也可以作为 Claude Code 的网关，通过 Anthropic 兼容接口提供服务。

`~/.claude/settings.json` 示例：

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

说明：

- `ANTHROPIC_BASE_URL` 要写网关根地址，不要带 `/v1`
- `ANTHROPIC_DEFAULT_*_MODEL` 要映射到网关当前实际暴露的模型 id
- `skipDangerousModePermissionPrompt` 只需要保留在 `permissions` 里，不要再写一份顶层字段
- 如果网关启用了客户端鉴权，请把占位值换成真实的 key

## 免责声明

本项目与 OpenAI、Codebuff、Freebuff 没有任何官方关联，相关商标归各自所有者所有。

本仓库仅用于交流、实验和学习，不构成生产建议。本项目按 “as-is” 方式提供，使用风险由使用者自行承担。

## License

MIT
