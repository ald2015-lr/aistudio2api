<div align="center">

# AI Studio to OpenAI, Anthropic & Gemini Compatible API

<p align="center">
  <a href="README.md">中文</a>
  &nbsp;|&nbsp;
  <a href="README_en.md"><b>English</b></a>
</p>

<p>
  <b>A High-Performance Go Proxy Server</b><br>
  Converts the Google AI Studio web protocol into OpenAI, Responses, Anthropic, and Gemini compatible APIs
</p>

<p>
  Playground + Build Dual Quota Channels &nbsp;•&nbsp;
  High-Concurrency Multi-Account &nbsp;•&nbsp;
  Camoufox and Pure Go WAA Backends<br>
  Claude Code, Codex, and Other Agent Clients &nbsp;•&nbsp;
  Nano Banana, Veo, TTS, and Omni
</p>

</div>

---

## Core Capabilities

- **Dual Quota Channels**: Every account has separate Playground and Build app proxy quotas, and `UPSTREAM_CHANNELS` enables either or both; when one channel hits its limit, the same account continues on the other
- **High-Concurrency Multi-Account**: Detects Free, Pro, Ultra, and Plus benefits and routes across accounts by the live model catalog with round-robin or fill-first
- **Separate Ultra Pool**: Call Ultra accounts on their own through the `/ultra` prefix (for example `http://127.0.0.1:2048/ultra/v1`), exclusive by default, with their own resident and peak worker counts; Pro accounts can optionally join it (`ULTRA_INCLUDE_PRO`) to form a paid pool
- **Two WAA Backends**: Camoufox holds the official WAA lifecycle by default; with `WAA_BACKEND=go`, pure Go generates the official proof and no browser is downloaded or launched at runtime
- **Four API Protocols**: OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, and Gemini GenerateContent, plus Gemini Interactions
- **Mainstream Agent Clients**: Works with Claude Code, Codex, OpenCode, pi, omp, OpenClaw, and Hermes, including file read and write tool calls; native web search works in Claude Code, Codex, and omp
- **Tool Choice Across Four Protocols**: Required, named, and single function calls, `strict` argument validation, Gemini `VALIDATED`, and Anthropic `thinking.disabled`; tool schemas Playground cannot encode (`$ref`, `uniqueItems`, and similar) are sent as a simplified shape with the full schema handed to the model

## Features

- **Native Streaming**: Text, reasoning summaries, function calls, Google tools, media, and usage
- **TTS Speech Generation**: Gemini TTS models for single-speaker and multi-speaker audio
- **Image Generation**: Nano Banana image generation
- **Video Generation**: Veo video generation and image-to-video; Gemini Omni accepts text, image, and video input and returns text and MP4 video through the four generation APIs
- **YouTube Input**: Paste a video URL to attach and read the external video
- **Smart Model Switching**: Discover models from AI Studio and route through the `model` field
- **Google Tools**: Search, Image Search, URL Context, Code Execution, and Maps
- **Files and Transcribe**: File upload, metadata, content, deletion, and audio transcription
- **Live and Robotics**: WebSocket text, audio, JPEG images, media end, tool calls, resumption, and interruption
- **Anti-Fingerprinting**: Camoufox holds the official WAA lifecycle with a stable browser fingerprint and network exit per account
- **GUI Launcher**: Manage accounts, service controls, live logs, models, requests, and configuration in the web UI
- **Usage Dashboard**: A local SQLite ledger tracks request counts, success rate, tokens, latency percentiles, downgrade rejection rate and duplicate reply rate, with filters, breakdowns, request records and CSV export
- **Modular Architecture**: Go handles protocols, scheduling, APIs, and management; Camoufox hosts WAA and isolated login

## System Requirements

- **Windows Release Runtime**: Windows 10 or later, `aistudio2api.exe`, and `start.bat`
- **Linux Release Runtime**: Extract `linux-amd64.tar.gz` and run `./aistudio2api`; Camoufox needs the Firefox system libraries, on Debian/Ubuntu run `sudo apt install libgtk-3-0 libasound2 libnss3 libdbus-glib-1-2 libxtst6 libxrandr2 libgbm1 libxkbcommon0 libpango-1.0-0 libcairo2 libxcomposite1 libxdamage1 libxfixes3 fonts-liberation`
- **Source Build**: Go 1.25.0+, Node.js 22.13+ or 24+, and its bundled npm
- **Operating System**: Windows, macOS, Linux
- **Memory**: 2GB+ available memory for one account; each resident prewarmed account adds about 0.6GB
- **Network**: Stable internet connection to Google AI Studio

## Installation

### Method 1: Windows One-Click Start (Recommended)

Download the `windows-amd64.zip` package from [Releases](https://github.com/ald2015-lr/aistudio2api/releases), extract it, and run `start.bat`. The package includes the management interface and is ready to run.

To start from source:

```powershell
git clone https://github.com/Mag1cFall/AIStudio2API.git
cd AIStudio2API
copy .env.example .env
```

Then double-click `start.bat`. You can also run it from PowerShell:

```powershell
.\start.bat
```

The script runs an existing `aistudio2api.exe` immediately. In a source checkout without the executable, it installs frontend dependencies and builds the frontend and Go program.

The first launch downloads Camoufox for the current platform to `runtime/camoufox/`. Set `CAMOUFOX_PATH` to use an existing executable instead.

### Method 2: Linux and macOS Source Build

#### 1. Install Dependencies

- Go 1.25.0 or later
- Node.js 22.13+ or 24+, with its bundled npm

#### 2. Clone the Project

```bash
git clone https://github.com/Mag1cFall/AIStudio2API.git
cd AIStudio2API
cp .env.example .env
```

#### 3. Build and Run

```bash
cd web
npm ci
npm run build
cd ..
go build -o aistudio2api ./cmd/aistudio2api
chmod +x ./aistudio2api
./aistudio2api
```

The first Linux or macOS launch also prepares the matching Camoufox build automatically.

## Quick Start

### First-Time Use (Authentication Required)

1. **Prepare the first account**:

   Windows can import a local Chrome account:

   ```powershell
   start.bat setup
   ```

   Linux and macOS use an isolated Camoufox login:

   ```bash
   ./aistudio2api setup --login
   ```

   The Google email is read from AI Studio after login, and Google Drive is authorized for the account. The account is saved under the path configured by `AISTUDIO_AUTH_STATES` in `.env`. Locale and timezone default to the current computer and can be set with `--locale` and `--timezone`.

2. **Start the management UI**:
   - Double-click `start.bat` on Windows
   - Run `./aistudio2api` on Linux or macOS
   - The browser opens `http://127.0.0.1:2048`
   - The initial state is `STOPPED`, with Logs open by default

3. **Add another account**:
   - Open Accounts
   - "Import Chrome accounts" supports selecting multiple local Chrome accounts
   - "Browser login" opens an isolated Camoufox window, detects the email after login, authorizes Google Drive, and saves the account; when Google asks to verify your identity, confirm it in that window or on your phone

4. **Start the API**:
   - Click "Start service" to start the data plane
   - The state advances through `LAUNCHING` to `RUNNING`; "Stop service" cancels an in-progress launch
   - Use Logs to confirm account, model, and request status
   - The API listens on `http://127.0.0.1:2048` by default

Account actions depend on state:

| Account state | Available actions |
| --- | --- |
| `ready` | Edit, disable, verify, delete |
| `disabled` | Edit, enable, delete |
| `auth_required` | Edit, disable, log in again, verify, delete |

"Log in again" appears only when the account state is `auth_required`.

### Daily Use (With Existing Authentication)

1. Double-click `start.bat` on Windows; run `./aistudio2api` on Linux or macOS
2. Click "Start service" to enable the APIs
3. "Stop service" cancels an in-progress launch or active requests and closes WAA workers while the management UI and Logs remain available
4. Click "Start service" again to resume the APIs

Starting again loads the latest data-plane settings from `.env`. Changes to `LISTEN_ADDR` or `PROXY_API_KEY` require restarting the management process.

Press `Ctrl+C` in the launch window or close that window to exit the manager. Closing the browser tab does not stop the manager.

### Quick Start

`start.bat`: Starts the manager and opens the web UI.

`start.bat -open-ui=false`: Starts the manager without opening the web UI.

`start.bat setup`: Scans local Chrome accounts. Use `--email` or `--profile` to select a Chrome account. Run `start.bat setup --login` for an isolated login, or `start.bat setup --storage-state <file>` to import a file.

## API Usage

### OpenAI Compatible Interface

After starting the service, call OpenAI Chat Completions directly:

```bash
curl http://127.0.0.1:2048/v1/chat/completions \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.7-flash",
    "messages": [{"role": "user", "content": "Hello, world!"}],
    "stream": true
  }'
```

### Client Configuration Example

| Protocol | Base URL | API key |
| --- | --- | --- |
| OpenAI Chat / Responses | `http://127.0.0.1:2048/v1` | `PROXY_API_KEY` from `.env` |
| Anthropic Messages | `http://127.0.0.1:2048` | `PROXY_API_KEY` from `.env` |
| Gemini | `http://127.0.0.1:2048` | `PROXY_API_KEY` from `.env` |

Read model names from `GET /v1/models` or `GET /v1beta/models`.

Text chat models accept name suffixes, combinable in any order (for example `gemini-3.1-pro-preview-128-nothinking-online` or `gemini-4-argon-low-nothinking`); the model lists include these aliases:

| Suffix | Effect |
| --- | --- |
| none | Highest thinking level; an explicit `reasoning_effort`, `thinking` or similar client setting wins |
| `-low`, `-medium`, `-high`, `-minimal` | Thinking level chosen by name, overriding client thinking settings, e.g. `gemini-4-argon-low`, `gemini-4-argon-medium`; an unsupported level falls back to the closest supported one, and budget-only models use 1024, 8192, the maximum or 128 tokens |
| `-128` | Lowest thinking, same as `-minimal` |
| `-nothinking` | The model still thinks but the thought text is not returned (thought signatures are kept) |
| `-online` | Enables Google Search when the model supports it |

A catalog model ID that already ends in a level (for example `xxx-low`) uses that level instead of being forced to the highest.

The model name in responses (`model` for OpenAI, Anthropic, Responses and Interactions; `modelVersion` for Gemini) is the requested name with these suffixes removed: requests for `gemini-3.1-pro-preview`, `gemini-3.1-pro-preview-128` and `gemini-3.1-pro-preview-low` all return `gemini-3.1-pro-preview`; names not in the catalog are returned as requested. Names with a `models/` prefix (such as `models/gemini-3.1-pro-preview-low`) are resolved after removing the prefix, the same in every protocol. The upstream-reported serving model version is not returned there; it only appears in the `provider_model` field of OpenAI, Responses and non-streaming Anthropic responses, and the admin log records `served_model` when the serving model belongs to a different family than the requested one.

Every API call must send `PROXY_API_KEY` (default `sk-onechat-fun-fun`, editable in the Settings page). With the default key, only pages on this machine can call the API from a browser; web clients need a custom key. The key can go in the `Authorization: Bearer`, `x-api-key`, `x-goog-api-key` or `api-key` header or the `?key=` query parameter, and the request passes if any of them matches the configured key. Relays such as new-api often put a downstream token in `?key=` and this service's key in a header; those requests pass too, so a 401 does not get the whole channel disabled. Only one key is supported.

Protocol compatibility (details in the [Google AI Studio protocol specification](docs/protocol.md)):

- **Parameters upstream does not have are ignored**: OpenAI Chat `frequency_penalty`, `presence_penalty`, `logprobs` and `logit_bias`, and Gemini `frequencyPenalty`, `presencePenalty`, `responseLogprobs` and `logprobs`, are ignored instead of returning 400 (SillyTavern, LobeChat, Open WebUI and Dify often send a non-zero penalty). Non-streaming Chat responses carry `"logprobs": null`. `n` and `candidateCount` still only accept 1.
- **Tolerant tool call history**: when the `arguments` of a Chat `tool_calls` entry or a Responses `function_call` item is not a JSON object (for example a truncated call), it is sent as `{"_raw":"<original>"}`, so a session replayed by Cline, Roo, OpenCode and similar clients no longer fails every turn with 400.
- **Anthropic server tools**: `web_search_*`, `web_fetch_*` and `code_execution_*` are matched by prefix, so any dated version works; `tool_search_tool_*` and MCP toolset definitions are ignored. `web_fetch` `allowed_domains` and `blocked_domains` still return 400.
- **Anthropic history**: after the API key changes, or when a conversation comes from another Claude channel, web_search history keeps titles and URLs as text (snippets are only restored for sources this service issued). Calls or results missing their other half, failed searches, and history from other server tools such as web_fetch, code_execution and tool_search also become text instead of returning 400. `document` accepts `text`, `content` and `file` (`file_id`) sources, and `image` accepts a `file` source.
- **Anthropic structured output**: `json_schema` in `output_config.format`, or the beta `output_format`, produces JSON that follows the schema (both were silently ignored before, so clients got free text).
- **Tool parameter schemas**: properties listed in `required` but not defined get an empty `{}` definition, so upstream no longer rejects the whole request with `property is not defined` (the same on Playground, Build and `strict` validation). On Playground, integers written as `"5"` or `5.0` and `TYPE_UNSPECIFIED` no longer make the schema fall back to the simplified encoding.
- **Small Gemini fixes**: `MODALITY_UNSPECIFIED` and `MODE_UNSPECIFIED` count as unset; `googleSearchRetrieval` with `dynamicRetrievalConfig` enables Google Search as usual; `fileData` may omit `mimeType` (it is inferred from the `displayName` or `fileUri` extension).
- **Upstream finish message**: when Google returns a `finishMessage` (for example the details of a `MALFORMED_FUNCTION_CALL`), Gemini responses pass it through as `finishMessage` and the admin log records an extra `上游结束说明` (upstream finish message) line. Google's explanation for a blocked prompt only goes to the admin log; the error clients receive is unchanged.
- **Stream heartbeats**: after 10 seconds without new content, Gemini streams write a blank line (the google-genai Python SDK parses `: ping` as JSON and fails), Anthropic sends the official `event: ping`, and OpenAI Chat, Responses and Interactions still send `: ping`.

For Cherry Studio:

1. Open Cherry Studio settings
2. Add an OpenAI-compatible provider
3. Set the API host to `http://127.0.0.1:2048/v1`
4. Set the API key to `PROXY_API_KEY` from `.env`
5. Load models from `/v1/models`, or add `gemini-3.6-flash` and `gemini-3.7-flash` manually

[Claude Code](https://github.com/anthropics/claude-code) uses the Anthropic endpoint. Subagents pick models by the opus, sonnet, and haiku tiers; the variables below map them to AI Studio models. WebSearch runs on Google Search:

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:2048"
$env:ANTHROPIC_API_KEY = "<PROXY_API_KEY>"
$env:ANTHROPIC_MODEL = "gemini-3.8-flash"
$env:ANTHROPIC_DEFAULT_OPUS_MODEL = "gemini-3.1-pro-preview"
$env:ANTHROPIC_DEFAULT_SONNET_MODEL = "gemini-3.8-flash"
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = "gemini-3.5-flash-lite"
```

[Codex](https://github.com/openai/codex) uses the Responses endpoint. Add a provider to `~/.codex/config.toml` and put `PROXY_API_KEY` in the `AISTUDIO2API_KEY` environment variable. Codex's `web_search` tool runs on Google Search:

```toml
model = "gemini-3.8-flash"
model_provider = "aistudio"

[model_providers.aistudio]
name = "AIStudio2API"
base_url = "http://127.0.0.1:2048/v1"
env_key = "AISTUDIO2API_KEY"
wire_api = "responses"
```

[omp](https://github.com/can1357/oh-my-pi) runs its `web_search` tool through its own provider order. Set `GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:2048` and `GEMINI_API_KEY=<PROXY_API_KEY>`, and put the Gemini provider first in the omp config:

```yaml
providers:
  webSearchOrder:
    - gemini
  webSearchGeminiModel: gemini-3.8-flash
```

Main endpoints:

| Capability | Endpoint |
| --- | --- |
| Models | `GET /v1/models`, `GET /v1/models/{model}`, `GET /v1beta/models`, `GET /v1beta/models/{model}` |
| OpenAI Chat | `POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Gemini Interactions | `POST /v1beta/interactions`, `POST /v1/interactions` |
| Files | `POST /v1/files`, `GET /v1/files/{id}`, `GET /v1/files/{id}/content`, `DELETE /v1/files/{id}` |
| Anthropic | `POST /v1/messages`, `POST /v1/messages/count_tokens` |
| Gemini | `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent`, `:countTokens` |
| Images | `POST /v1/images/generations` |
| Speech | `POST /v1/audio/speech` |
| Transcription | `POST /v1/audio/transcriptions` |
| Music | Gemini `generateContent` with `responseModalities: ["AUDIO"]` |
| Video | `POST /v1/videos`, `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content` |
| Gemini Video | `POST /v1beta/models/{model}:predictLongRunning`, `GET /v1beta/operations/{id}` |
| Live (including live translation and live transcription) / Robotics | `GET /v1/live`, `GET /v1/robotics/stream` |

All four generation APIs can enable Search, Image Search, URL Context, Code Execution, and Maps through their protocol fields. Request and event formats for Files, Transcribe, Live, and Robotics are documented in the [Google AI Studio protocol specification](docs/protocol.md).

Inline attachments in generation requests are preferentially uploaded as temporary Drive files and cleaned up when the request ends. If the account has not granted Drive access, the original inline data is sent instead. Images, audio, video, PDFs, and other inputs must be supported by the selected model. Upload reusable attachments once through the Files API and reuse their file IDs.

Gemini attachments and video image inputs accept `inlineData` / `inline_data`, `fileData` / `file_data`, `mimeType` / `mime_type`, and `fileUri` / `file_uri`. Base64 media data supports standard and URL-safe alphabets, padded and unpadded forms, and the `data:<MIME>;base64,` prefix. Markdown images in OpenAI assistant history also support URL-safe Base64 and CR/LF line breaks. Inline GIFs and GIFs uploaded through video multipart requests are converted to PNG using the first frame, preserving the logical canvas, frame position, and transparency.

### Ultra Pool (/ultra prefix)

Accounts with the Ultra tier form a separate Ultra pool (with `ULTRA_INCLUDE_PRO=true`, accounts with the Pro tier join it as well and it becomes a paid pool, see "Pro accounts in the Ultra pool" below); every other account (including accounts whose tier has not been read yet) belongs to the normal pool. Point a client's base URL at the `/ultra` prefix and its requests are served only by Ultra-pool accounts; endpoints, authentication (the same `PROXY_API_KEY`), request bodies and response formats are the same as on the normal API:

| Protocol | Normal pool base URL | Ultra pool base URL |
| --- | --- | --- |
| OpenAI Chat / Responses | `http://127.0.0.1:2048/v1` | `http://127.0.0.1:2048/ultra/v1` |
| Anthropic Messages | `http://127.0.0.1:2048` | `http://127.0.0.1:2048/ultra` |
| Gemini | `http://127.0.0.1:2048` | `http://127.0.0.1:2048/ultra` (requests go to `/ultra/v1beta/...`) |

```bash
curl http://127.0.0.1:2048/ultra/v1/chat/completions \
  -H "Authorization: Bearer <PROXY_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model": "<MODEL>", "messages": [{"role": "user", "content": "Hello"}]}'
```

- **Exclusive switch**: with `ULTRA_EXCLUSIVE=true` (default), Ultra-pool accounts only serve `/ultra` requests and normal paths use the other accounts only; with `false`, normal paths may also use Ultra-pool accounts, while `/ultra` still uses Ultra-pool accounts only.
- **Pro accounts in the Ultra pool (paid pool)**: with `ULTRA_INCLUDE_PRO=true`, accounts with the Pro tier join Ultra accounts in the Ultra pool and follow every Ultra-pool setting: they serve `/ultra` and appear in `/ultra/v1/models`, follow `ULTRA_EXCLUSIVE`, their workers count toward the Ultra partition and its two limits `ULTRA_WARM_WORKER_LIMIT` and `ULTRA_MAX_ACTIVE_WORKERS`, the `ULTRA_PRIMARY_MODELS` fast fail applies to them, and they count toward the Ultra lines in the sidebar, the Ultra primary model supply and `ultra_accounts` in `/api/status`. The default is `false`, so nothing changes on upgrade; saving it in the management UI applies it immediately without a service restart, and running workers are recounted in their new partition instead of being restarted; older management pages that do not send the field keep the current value.
  - **Normal paths stop using Pro (with `ULTRA_EXCLUSIVE=true`)**: this keeps Pro accounts for `/ultra` only, so they no longer take part in the model calls of the normal pool (Free, Plus and the other accounts). It costs three things: `gemini-4-argon` (offered on the Build channel only; on normal paths it was already served by Pro accounts) and every model that needs a subscription disappear from `/v1/models` on normal paths, so clients have to switch to `/ultra`; the normal pool shrinks accordingly; and on normal paths a Free or Plus account that exceeds its input limit can no longer retry on a Pro account. With `ULTRA_EXCLUSIVE=false` normal paths keep using Pro accounts as usual (just like Ultra accounts), and the management UI points out that this combination does not keep Pro for `/ultra` only.
  - **The real tier does not change**: the tier column on the Accounts page still shows Pro (with an extra "Ultra pool" tag, and the tier filter still filters by tier), the tier header is still `TIER1`, and input limits, model eligibility and the higher-tier retry all follow Pro: models that only an Ultra subscription can use are still served by Ultra accounts only, and on `/ultra` a Pro account that exceeds its input limit can retry once on an Ultra account. The learned daily-quota channel sharing is still grouped by real tier: Pro evidence is never mixed into the Ultra verdicts, and toggling the switch does not change what has been learned. Plus, Free and accounts whose tier has not been read yet stay in the normal pool.
  - **Whether Pro can serve `gemini-4-argon` depends on the upstream catalog**: the Pro account's Build catalog has to list it as generatable; if upstream ever marks it as Ultra-subscription-only in some account's Playground catalog, Pro accounts are excluded. After turning the switch on, check whether the standby count of "Ultra primary gemini-4-argon" in the sidebar includes Pro accounts, or tick "Use the Ultra pool" in the API playground and try the model directly.
  - **Capacity**: Pro and Ultra share the two limits of the Ultra partition, whose defaults of 2 and 5 are small; raise them to match your Pro accounts when turning the switch on, and lower the normal-partition `WARM_WORKER_LIMIT` and `MAX_ACTIVE_WORKERS` if needed. Prewarming picks accounts tier by tier in turn with Pro before Ultra, so with `ULTRA_WARM_WORKER_LIMIT=1` the only resident worker may be a Pro account and models that only an Ultra subscription can use wait for a cold start every time; keep it at 2 or more. Right after a switch the total number of browsers can briefly exceed the sum of both peaks (the extra ones are the Pro workers that changed partition); they are reaped after 5 minutes idle.
  - **When switching**: resources do not move, so files, videos and Veo operations that already live on a Pro account may need a different entry point after a switch: after turning it on, referencing them through normal paths returns 400 with `ULTRA_EXCLUSIVE=true`, so use `/ultra` instead (nothing changes with `ULTRA_EXCLUSIVE=false`); after turning it off, referencing them through `/ultra` returns 400, so use normal paths instead. Switch while no video job is in progress. Model names with a suffix (such as `gemini-4-argon-low`) do not pick up the new catalog right away: each pool's alias snapshot expires after 30 seconds, but an expired snapshot is still returned while a suffixed request starts a refresh in the background, so suffixed requests until that refresh finishes (those before the snapshot expires, the one that starts the refresh, and any that arrive while it runs) still resolve against the catalog from before the switch and may fail because the model is not found. When suffixed requests are rare this can happen long after the switch; retrying shortly after works. Names without a suffix are not affected. If `ULTRA_INCLUDE_PRO` is set in the process environment, that value wins and saving it in the management UI has no effect (as with every other setting).
- **Models and errors**: `/ultra/v1/models` and `/ultra/v1beta/models` list only the models available on Ultra-pool accounts. With no usable Ultra-pool account the API returns 503; when all Ultra-pool accounts are cooling down it returns 429 with `Retry-After`; Ultra primary models (`ULTRA_PRIMARY_MODELS`, default `gemini-4-argon`) get a 429 right away when quota runs out instead of switching accounts inside the service over and over, see [Primary models](#primary-models). Files, videos and other resources are checked against the pool their owning account belongs to now: an `/ultra` request cannot use a resource on a normal-pool account, and with `ULTRA_EXCLUSIVE=true` a normal-path request cannot use a resource on an Ultra-pool account either; referencing a resource outside the request's pool returns 400 with a message naming the pool it belongs to (with `ULTRA_EXCLUSIVE=false` normal paths do not check the pool). Resources do not move when `ULTRA_INCLUDE_PRO` is switched, see "When switching" above.
- **Separate worker counts**: workers of Ultra-pool accounts are counted separately from the normal pool. `ULTRA_WARM_WORKER_LIMIT` (resident, default 2; 0 means start on demand only) and `ULTRA_MAX_ACTIVE_WORKERS` (peak, default 5) can be raised to 10, 20 or whatever you need; `WARM_WORKER_LIMIT` and `MAX_ACTIVE_WORKERS` apply to the normal pool only, so the total number of browsers can reach the sum of both limits. Ultra requests never take workers from the normal pool and vice versa. With `ULTRA_INCLUDE_PRO` on, Pro and Ultra accounts share these two limits, whose defaults are small, so raise them as needed.
- **Management UI**: the "Ultra pool" section of Settings edits these four settings and the "Primary models" group edits the Ultra primary model list and the Ultra queue limit, all applying immediately on save; the Accounts page shows how many Ultra-pool accounts you have (ready / total, including Pro accounts when `ULTRA_INCLUDE_PRO` is on) and filters by tier, the tier column shows the real tier and Pro accounts in the Ultra pool carry an extra "Ultra pool" tag; the sidebar shows Ultra worker usage; requests under `/ultra` carry an Ultra tag in the request log, the request list and usage records, and the Usage page filters and groups by pool; in the API playground, tick "Use the Ultra pool" to send through `/ultra`: the model list then shows only the models of Ultra-pool accounts, while models offered on the Build channel only (such as `gemini-4-argon`) are still listed.
- Other unknown paths under `/ultra` return 404; the `/trace/` debugging route picks accounts by the normal-path rules, and `/trace/ultra/v1/...` is the debugging route of the Ultra pool.

### Gemini Interactions

The Google Gen AI SDK's `client.interactions`, TalkifyTTS, and similar clients connect to `http://127.0.0.1:2048/v1beta/interactions`; the stable endpoint is `/v1/interactions`. Like every other API call it requires `PROXY_API_KEY`, and errors use the Gemini format:

```bash
curl http://127.0.0.1:2048/v1beta/interactions \
  -H "x-goog-api-key: sk-onechat-fun-fun" \
  -H "Content-Type: application/json" \
  -d '{"model": "<model>", "input": "Hello, world!", "stream": true}'
```

Speech generation (single or multi-speaker):

```python
from google import genai

client = genai.Client(api_key="sk-onechat-fun-fun", http_options={"base_url": "http://127.0.0.1:2048"})
stream = client.interactions.create(
    model="<TTS model>",
    input="Hello, this is a test.",
    response_format={"type": "audio"},
    generation_config={"speech_config": [{"voice": "Kore"}]},
    stream=True,
)
for event in stream:
    if event.event_type == "step.delta" and event.delta.type == "audio":
        print(event.delta.data)
```

Streaming audio defaults to Base64-encoded 24 kHz, 16-bit little-endian mono PCM; non-streaming defaults to a complete WAV file. Set `response_format.mime_type` to `audio/l16` or `audio/wav` to choose explicitly. Model name suffixes, function calls (the `id` is always non-empty; send it back unchanged to match results), and `previous_interaction_id` all work. Continuation shares the in-process response nodes with Responses, rejects `resp_` Responses IDs, and does not store speech output. See the [Interactions protocol](docs/protocol.md#gemini-interactions) for fields and events.

### TTS Speech Generation

```bash
curl http://127.0.0.1:2048/v1/audio/speech \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-tts-preview",
    "input": "Hello, this is a test.",
    "voice": "Kore",
    "response_format": "wav"
  }' \
  --output speech.wav
```

Multi-speaker speech is available through Gemini `generateContent` with `multiSpeakerVoiceConfig`.

```bash
curl http://127.0.0.1:2048/v1beta/models/gemini-2.5-flash-preview-tts:generateContent \
  -H "x-goog-api-key: 123" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{"parts": [{"text": "Joe: How are you?\nJane: I am fine, thanks!"}]}],
    "generationConfig": {
      "responseModalities": ["AUDIO"],
      "speechConfig": {
        "multiSpeakerVoiceConfig": {
          "speakerVoiceConfigs": [
            {"speaker": "Joe", "voiceConfig": {"prebuiltVoiceConfig": {"voiceName": "Kore"}}},
            {"speaker": "Jane", "voiceConfig": {"prebuiltVoiceConfig": {"voiceName": "Puck"}}}
          ]
        }
      }
    }
  }' --output speech.json
```

Available voices are returned by `capability_options.voices` in the live model catalog. Models with the `speech_metadata` capability, such as `gemini-3.8-flash-tts`, accept the same `Speaker: line` script, and each text part can also set `speechMetadata.speaker` and `speechMetadata.style`, with `multiSpeakerVoiceConfig.mode` selecting `VERBATIM` or `CONVERSATIONAL`; OpenAI `instructions` become the speech style on these models.

### Image Generation (Nano Banana)

```bash
curl http://127.0.0.1:2048/v1/images/generations \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-image",
    "prompt": "A cute cat wearing a tiny hat",
    "n": 1,
    "size": "1024x1024"
  }'
```

### Video Generation (Veo)

```bash
curl http://127.0.0.1:2048/v1/videos \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "veo-3.1-fast-generate-preview",
    "prompt": "A drone flying over a forest"
  }'
```

After creating the operation, poll `GET /v1/videos/{id}` and download the result from `GET /v1/videos/{id}/content`.

## Models

The model catalog follows AI Studio updates; clients read the current values from `/v1/models` or `/v1beta/models`. The table below is a catalog-shape example; runtime results are authoritative for model IDs, limits, and methods:

| Model ID | Display name | Input | Output | Methods |
| --- | --- | ---: | ---: | --- |
| `antigravity-preview-05-2026` | Antigravity Agent Preview | 131072 | 65536 | `countTokens, generateContent` |
| `gemini-2.5-flash` | Gemini 2.5 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-flash-image` | Nano Banana | 32768 | 32768 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-2.5-flash-lite` | Gemini 2.5 Flash-Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-flash-preview-tts` | Gemini 2.5 Flash Preview TTS | 8192 | 16384 | `countTokens, generateContent` |
| `gemini-2.5-pro` | Gemini 2.5 Pro | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-pro-preview-tts` | Gemini 2.5 Pro Preview TTS | 8192 | 16384 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3-flash-preview` | Gemini 3 Flash Preview | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3-pro-image` | Nano Banana Pro | 131072 | 32768 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-image` | Nano Banana 2 | 65536 | 65536 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-lite` | Gemini 3.1 Flash Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.1-flash-lite-image` | Nano Banana 2 Lite | 65536 | 65536 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-tts-preview` | Gemini 3.1 Flash TTS Preview | 8192 | 16384 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-pro-preview` | Gemini 3.1 Pro Preview | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.5-flash` | Gemini 3.5 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.5-flash-lite` | Gemini 3.5 Flash Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.6-flash` | Gemini 3.6 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.7-flash` | Gemini 3.7 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-flash-latest` | Gemini Flash Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-flash-lite-latest` | Gemini Flash-Lite Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-omni-flash-preview` | Gemini Omni Flash Preview | 131072 | 65536 | `countTokens, generateContent` |
| `gemini-pro-latest` | Gemini Pro Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-robotics-er-1.6-preview` | Gemini Robotics-ER 1.6 Preview | 131072 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-robotics-er-2-preview` | Gemini Robotics-ER 2 Preview | 131072 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemma-4-26b-a4b-it` | Gemma 4 26B A4B IT | 262144 | 32768 | `countTokens, generateContent` |
| `gemma-4-31b-it` | Gemma 4 31B IT | 262144 | 32768 | `countTokens, generateContent` |
| `lyria-3-clip-preview` | Lyria 3 Clip Preview | 1048576 | 65536 | `countTokens, generateContent` |
| `lyria-3-pro-preview` | Lyria 3 Pro Preview | 1048576 | 65536 | `countTokens, generateContent` |
| `veo-3.1-fast-generate-preview` | Veo 3.1 fast | 480 | 8192 | `predictLongRunning` |
| `veo-3.1-generate-preview` | Veo 3.1 | 480 | 8192 | `predictLongRunning` |
| `veo-3.1-lite-generate-preview` | Veo 3.1 lite | 480 | 8192 | `predictLongRunning` |

Public endpoints implement `generateContent`, `countTokens`, and `predictLongRunning`. `/v1/models` and `/v1beta/models` preserve the live upstream catalogs across accounts; scheduling uses explicit model ID, method, and capability fields plus current account runtime state.

## Project Architecture

```text
AIStudio2API/
├── cmd/aistudio2api/        # Thin entry point
├── internal/app/            # Commands, management listener, data-plane lifecycle, and scheduling
├── internal/setup/          # Account import and isolated-login CLI
├── internal/aistudio/       # AI Studio protocol, authentication, models, and media
├── internal/api/            # OpenAI, Responses, Anthropic, and Gemini adapters
├── internal/chromeauth/     # Windows Chrome and DBSC import
├── internal/camoufoxnative/ # Camoufox BiDi, login, and WAA workers
├── internal/webui/          # Embedded frontend build
├── web/                     # Vue 3 and TypeScript management UI
├── docs/                    # Development guide and protocol specification
└── start.bat                # Windows one-click launcher
```

## Configuration

### Environment Variables

Copy and edit the environment file:

```bash
cp .env.example .env
```

| Variable | Default | Purpose |
| --- | --- | --- |
| `AISTUDIO_AUTH_STATES` | `auth` | Account file, directory, or comma-separated paths |
| `LISTEN_ADDR` | `127.0.0.1:2048` | Management UI and API listen address |
| `PROXY_API_KEY` | `sk-onechat-fun-fun` | Public API key; every API call must send it, and an empty value means the default. The default key is public in the source code, so set your own key before listening on a public address |
| `PROXY` | empty | Proxy used by Chrome import, login, and accounts without an override: http, https, or socks5, optionally with credentials (`http://user:pass@IP:port`, `IP:port:user:pass`, and more); saved in URL form, see [Proxy Configuration](#proxy-configuration) |
| `INIT_TIMEOUT` | `2m` | Per-account WAA initialization timeout |
| `REQUEST_TIMEOUT` | `5m` | Deadline before the first upstream event of a generation request: queuing, cold starts, WAA proof, attachment uploads, waiting for the first token and account switches before it all count, and it returns 504 when it expires. Once output starts it becomes an idle limit: the request ends with 504 only when upstream sends no event and no bytes for this long; a whole generation request is capped at 1 hour (or this value if longer). Token counting, video and transcription requests are limited by it as a whole. Changes apply to new requests only; see [Long replies cut off or regenerated](#long-replies-cut-off-or-regenerated) |
| `FIRST_EVENT_TIMEOUT` | `0` (disabled) | How long each attempt waits for its first upstream event after sending upstream; on timeout only that attempt is abandoned and the request retries on another account, returning 504 when no retry is possible. Must be shorter than `REQUEST_TIMEOUT`; models that think for a long time may take a while to emit their first event, so leave plenty of headroom |
| `WARM_WORKER_LIMIT` | `5` | Number of resident prewarmed accounts in the normal pool (Ultra-pool accounts excluded). Set it to at least the number of workers you want resident: workers above it are reaped after 5 minutes idle, and prewarming does not refill slots freed above it, so only cold starts on the request path fill them |
| `MAX_ACTIVE_WORKERS` | `10` | Maximum normal-pool workers active during peak load; the total number of browsers can reach this plus `ULTRA_MAX_ACTIVE_WORKERS`. When a cooled-down worker that is still serving requests is retired, its replacement starts first, so while it drains a partition can run up to `min(WARM_STARTUP_CONCURRENCY×2, peak/4)` more (at least 1) |
| `ULTRA_EXCLUSIVE` | `true` | `true` keeps Ultra-pool accounts for requests under the `/ultra` prefix and normal paths use the other accounts only; `false` lets normal paths use Ultra-pool accounts too, while `/ultra` still uses Ultra-pool accounts only |
| `ULTRA_INCLUDE_PRO` | `false` | `true` puts accounts with the Pro tier in the Ultra pool (the paid pool), where they follow every Ultra-pool setting (`/ultra`, `ULTRA_EXCLUSIVE`, the Ultra partition and its two `ULTRA_*` worker limits, `ULTRA_PRIMARY_MODELS`); with `ULTRA_EXCLUSIVE=true` Pro accounts no longer serve normal paths. The real tier does not change (the `TIER1` tier header, input limits, model eligibility, the tier shown on the Accounts page and daily-quota channel sharing still follow Pro). Applies immediately on save; see [Ultra Pool](#ultra-pool-ultra-prefix) for the effects and caveats |
| `ULTRA_WARM_WORKER_LIMIT` | `2` | Resident prewarmed workers of the Ultra pool; `0` means start on demand only. With `ULTRA_INCLUDE_PRO` on it is shared by Ultra and Pro accounts, and with `1` the only resident worker may be a Pro account |
| `ULTRA_MAX_ACTIVE_WORKERS` | `5` | Maximum Ultra-pool workers active during peak load; at least 1 and not lower than `ULTRA_WARM_WORKER_LIMIT`. With `ULTRA_INCLUDE_PRO` on it is shared by Ultra and Pro accounts |
| `WARM_STARTUP_CONCURRENCY` | `2` | Concurrent Camoufox worker cold starts: prewarming uses at most this many, and cold starts for waiting requests get one extra reserved slot and go first; `WAA_BACKEND=go` is not limited |
| `PER_ACCOUNT_CONCURRENCY` | `2` | Concurrent requests allowed per account; bound by the per-minute request limit (`ACCOUNT_RPM_LIMIT`), so do not raise it |
| `ACCOUNT_RPM_LIMIT` | `10` | The most generation requests one account sends in the last 60 seconds (upstream allows 12 per account per minute, shared by all models and both channels). An account that reaches it is skipped for another account, and new workers start once every running worker is full; only this service's requests are counted and 429 cooldowns remain the fallback; `0` turns it off; applies immediately on save |
| `ROUTING_STRATEGY` | `round-robin` | `round-robin` rotates accounts; `fill-first` reuses the first available account |
| `UPSTREAM_CHANNELS` | `playground,build` | Upstream channels for generation requests; either one can be used alone |
| `STREAM_PLAYGROUND_MODELS` | empty | Comma-separated models whose streaming requests go to Playground first. Models that need a subscription return the whole reply at once on the Build channel; streaming requests for the listed models use Playground first and stream chunk by chunk, switching to Build immediately when every Playground account is busy instead of waiting for Playground. Empty lets Playground and Build rotate as usual; non-streaming requests are not affected. Service Configuration in the management UI lists the catalog models that need a subscription and are available on Playground; changes apply immediately |
| `PRIMARY_MODELS` | empty | Comma-separated primary models of the normal pool; they apply to normal paths only (without the `/ultra` prefix). Requests for the listed models wait at most `PRIMARY_QUEUE_WAIT` for a free account or worker, then return 429 with `Retry-After` so the client retries later instead of queuing until `REQUEST_TIMEOUT`; empty keeps every model queuing as usual. When every candidate is cooling down the usual rules still apply. Editable in Service Configuration of the management UI; changes apply immediately (see [Primary models](#primary-models)) |
| `PRIMARY_QUEUE_WAIT` | `10s` | Queue limit for primary models, shared by the whole request: it is wall-clock time from the first account pick, so retries on another account after a failure (such as a 403 or a quota 429) and the retry backoff count too, and switching accounts does not restart it; the browser start-up of a cold start the request runs for itself does not count (up to `INIT_TIMEOUT`); once the limit is reached only an account that is free right away is used, otherwise 429; `0` never queues and returns 429 when nothing is free right away; cannot exceed `REQUEST_TIMEOUT` (when unset it is the smaller of 10s and `REQUEST_TIMEOUT`) |
| `ULTRA_PRIMARY_MODELS` | `gemini-4-argon` | Comma-separated primary models of the Ultra pool; they apply to `/ultra` requests (and, with `ULTRA_EXCLUSIVE=false`, to these models on normal paths when they are not in `PRIMARY_MODELS`). Queuing follows the same rules as `PRIMARY_MODELS`, and on top of that a 429 comes right away when quota runs out: at once when every candidate is cooling down; after a quota 429, at most 2 more account switches when the daily quota is used up and at most 5 more for per-minute limits and rate limits, where the pick right after each quota failure uses only workers that are already running (see [Primary models](#primary-models)). Set it to an empty value in `.env` or clear it in the management UI to turn it off; an empty process environment variable has no effect. Changes apply immediately |
| `ULTRA_PRIMARY_QUEUE_WAIT` | `10s` | Queue limit for Ultra primary models, with the same rules as `PRIMARY_QUEUE_WAIT`: shared by the whole request, `0` never queues, cannot exceed `REQUEST_TIMEOUT` (when unset it is the smaller of 10s and `REQUEST_TIMEOUT`) |
| `WAA_BACKEND` | `camoufox` | `camoufox` runs WAA in a Camoufox page; `go` runs WAA inside the service process and neither downloads nor starts Camoufox |
| `TEMPORARY_CHAT` | `false` | Use Temporary Chat for the WAA prewarm page |
| `AUTO_START` | `true` | Start the generation service automatically when the management process starts, retrying transient failures from 5 seconds up to 1 minute apart; `false` keeps it `STOPPED` until started from the management page |
| `REQUEST_BODY_LOG` | `false` | Also store public API POST request and response bodies in the usage ledger (each truncated to 64 KiB, newest 1000 kept), viewable in the request details of the Usage page; bodies are stored in plain text on this machine. Takes effect after restarting the program |
| `ADMIN_PASSWORD` | empty | When set, the management page can also be opened with HTTP Basic authentication (any username, this value as password); 10 failures within 10 minutes block the IP for 15 minutes. Without it, only the admin token works. The management page and `/api/` always require the admin token stored in `.admin-token`: open the `http://127.0.0.1:2048/?admin_token=<token>` address printed in the startup log once and the browser remembers the login; scripts send it in the `X-Admin-Token` header. Use HTTPS, since plain HTTP sends the token and password in clear text |

The Usage page of the management UI reads the local ledger `runtime/requests.db`: every POST request that passes the API key check adds one row when it finishes (token counting requests are not recorded), giving request counts, success rate, tokens, latency percentiles, downgrade rejection rate, duplicate reply rate, breakdowns, request records and CSV export; records and rollups are kept for 90 days. If the ledger cannot be opened the service logs a WARN and keeps running.

The service loads every account from `AISTUDIO_AUTH_STATES`. `WARM_WORKER_LIMIT` sets the resident warm pool of the normal pool, `MAX_ACTIVE_WORKERS` caps its peak worker count, the Ultra pool has its own `ULTRA_WARM_WORKER_LIMIT` and `ULTRA_MAX_ACTIVE_WORKERS` (see [Ultra Pool](#ultra-pool-ultra-prefix)), and these worker settings, `ULTRA_EXCLUSIVE` and `ULTRA_INCLUDE_PRO` apply immediately when saved from the management UI; `WARM_STARTUP_CONCURRENCY` limits concurrent Camoufox cold starts (prewarming can fill it, cold starts for waiting requests keep one extra slot), `PER_ACCOUNT_CONCURRENCY` controls request slots per account, and `ACCOUNT_RPM_LIMIT` caps the generation requests each account sends per minute.

The pool can run roughly `MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY` generations at once (`ULTRA_MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY` for the Ultra pool, shared by Ultra and Pro accounts when `ULTRA_INCLUDE_PRO` is on), and at most the number of workers × `ACCOUNT_RPM_LIMIT` generations per minute: upstream allows each account at most 12 generation requests per minute, so a higher per-account concurrency cannot send more than that within a minute. Beyond that, the extra requests queue for a free account or worker while picking an account, time to first token grows, and they end with 504 when `REQUEST_TIMEOUT` expires; accounts that hit their daily quota but still hold a worker reduce the usable capacity further. Such dead slots (accounts cooled down globally, or on a primary model or a model that many workers are cooled down on) are drained even while requests for other models keep using them: they stop taking new requests, a replacement account starts first, and the worker closes when its requests finish. Requests for other models go to dead slots first, which leaves the workers that can still serve the primary models to them. The `PRIMARY_MODELS` fast fail only treats the symptom: requests for the listed models that queue longer than `PRIMARY_QUEUE_WAIT` get a 429 so the client retries later, which adds no capacity. When load stays above capacity, scale out by adding workers (`MAX_ACTIVE_WORKERS`, `ULTRA_MAX_ACTIVE_WORKERS`); per-account concurrency is bound by the per-minute request limit (`ACCOUNT_RPM_LIMIT`), so do not raise it; or reduce concurrency.

Account 403 cooldown: when a generation request for a model returns HTTP 403 permission denied (common when a free account calls a paid model), that model on that channel of the account is cooled down. The account is not picked for that model while it cools, and switching to another account adds no backoff. The cooldown grows with repeats within 24 hours: 30 minutes the first time, 6 hours the second, and from the third on until the next midnight Pacific time (at least 6 hours). The two channels are counted separately: another 403 on the same channel while it is still cooling (a request sent before the cooldown was written) does not raise the level, it only restarts the cooldown, and one success on that channel starts over. These 403s add no backoff but still count toward each request's limit of 5 consecutive failures, so when every account returns 403 for a model one request does not walk through the whole pool. When at least 5 different accounts in one pool return 403 for the same model within 10 minutes and that model has had no success in the meantime, the 403s are treated as a pool-wide problem (such as a broken WAA proof or request shape): no account is cooled and no level is raised, so everything recovers as soon as the fix is deployed. If the account's other channel is still free, the request tries it once more there; once every channel is cooled, a resident worker of the account is registered for retirement: it closes when idle, and while busy it is drained only if the model is a primary model or one that many workers are cooled down on; otherwise it closes once idle. When one account returns 403 three times in a row within 10 minutes across at least 2 models (with no success in between), it is additionally paused globally for 30 minutes. Cooldowns and pauses are shown on the "Cooldowns & requests" page and survive restarts.

403 permission denied list: accounts whose generation requests return HTTP 403 PERMISSION_DENIED (`The caller does not have permission`, protocol error code 7) are recorded separately and listed at the top of the "Accounts" page: "403 permission denied accounts (usually the account has not accepted Google's terms; fetch a new auth file and upload it) N", with each account's email, the models and channels that returned 403, the last time and the count; more than 20 accounts are collapsed. Such accounts usually have not accepted Google's terms yet and keep returning 403 after the cooldown ends; only a new auth file helps. To fix them, use "Copy all emails" (one per line) or "Download list" (a txt file, also one email per line), sign in to each account and accept the terms, fetch a new auth file and upload it over `auth/<Google email>/storage-state.json` (or replace the whole account directory). Within about 10 to 20 seconds the service picks up the new auth file (including one re-exported from a browser that was already signed in, after accepting the terms, where the login cookies did not change; the service's own cookie write-backs do not count), removes the account from the list, lifts its 403 cooldowns and automatic pause and restarts its cooldown levels (quota and other cooldowns stay), so the account is scheduled again at once instead of waiting for the cooldown; an auth file from a new login put in place while the service was stopped is picked up at startup the same way, and records of accounts whose directories were removed are dropped; a browser login of the account from the management page clears it too. If the new auth file still gets a 403 the account is listed again. A later success of the account on a model and channel also clears that model's entry for that channel. Pool-wide 403s (see the previous paragraph) are not recorded: once the pool-wide verdict is reached, every entry for that model added in that pool since the model's last success there is withdrawn (when an outage starts right after a success, each 403 in the roughly 10 minutes before the verdict is first recorded per account). "Clear records" only empties the list and does not lift cooldowns. To get rid of these accounts, use "Delete all": after confirming, every listed account (including collapsed ones) is removed from the pool and its account directory (auth file) is deleted, the same as "Delete" for a single account in the account list, and its 403 record goes with it; this cannot be undone, and an account can be used again only after its auth file is uploaded again. After confirming, the page reads the list again, and checks it again before each batch, so it deletes only accounts that were listed when you confirmed and are still listed when submitted: an account whose auth file was uploaded again while the dialog was open, and that has already left the list, is not deleted, and accounts listed only after confirming are not deleted either (the service needs about 10 to 20 seconds to pick up a new auth file, so a just-uploaded account is still listed until then and is deleted). The page calls `POST /api/accounts/bulk-delete` in batches of 500, shows progress and failures in the bulk progress panel, can stop between batches, and reports how many accounts were deleted, no longer existed (already gone from the pool; only the record is cleared) and failed. An account that is serving a request first stops taking new requests and is deleted once it becomes idle, waiting at most about 10 seconds for the whole batch; an account that does not become idle in time is reported as failed ("Account kept serving requests") and is scheduled normally again, so click again later. The records are kept in `.permission-denied.json` in the account directory and survive restarts; the management endpoints are `GET /api/accounts/permission-denied` and `POST /api/accounts/permission-denied/clear` (body `{"account_ids":[...]}` or `{"all":true}`; account IDs are matched case-insensitively).

Capacity lines in the management sidebar: "N dead slots · retiring" counts resident workers whose account is in a long cooldown (global, or on a primary model or a model that many workers are cooled down on); while some of them drain, "N draining" is appended (they take no new requests and close once their in-flight requests finish; their replacements have already been started when a slot was free, otherwise they start once the old worker closes). With `PRIMARY_MODELS` set, each primary model gets a line "Primary <model>: N warm · C cooling · M standby": warm is the number of resident workers that can serve it, cooling the number of resident workers cooling down for it (including short cooldowns ending within 2 minutes; they cannot serve it until the cooldown ends), standby the number of idle accounts that still can (counted up to 1000, shown as 1000+), refreshed about every 30 seconds and shown in yellow when standby is 0 (see [Troubleshooting](#primary-model-returns-429-or-first-token-is-slow)); each model of `ULTRA_PRIMARY_MODELS` gets its own line "Ultra primary <model>: N warm · C cooling · M standby" below the Ultra worker line, counting only resident workers of the Ultra partition and idle accounts of the Ultra pool (including Pro accounts when `ULTRA_INCLUDE_PRO` is on; after a switch it is recounted for the new pools within about 5 seconds). In `/api/status` these are `dead`, `draining` and `retiring` (pending retirements) of `workers` and `ultra_workers`, and `primary_supply` (entries for Ultra primary models carry `"pool": "ultra"`); each `primary_supply` entry also has `cooling`: the number of resident workers that are cooling down for that model (not draining and a candidate for it, but every supported enabled channel is cooling down, including short cooldowns that end within 2 minutes); these workers count neither as warm nor as standby.

### Primary models

There are two primary model lists, each applying to the pool a request goes to. Both are edited in the "Primary models" group of Service Configuration in the management UI and apply as soon as they are saved, without a restart:

| List | Default | Applies to | When quota runs out |
| --- | --- | --- | --- |
| `PRIMARY_MODELS`, `PRIMARY_QUEUE_WAIT` | empty, `10s` | Requests on normal paths (without the `/ultra` prefix) | Same as other models: when every candidate is cooling down and one recovers within 1 minute the request waits for it, and a quota 429 switches to another account |
| `ULTRA_PRIMARY_MODELS`, `ULTRA_PRIMARY_QUEUE_WAIT` | `gemini-4-argon`, `10s` | `/ultra` requests; with `ULTRA_EXCLUSIVE=false` also models on normal paths that are in this list but not in `PRIMARY_MODELS` (usually models only Ultra-pool accounts can serve) | 429 right away instead of switching accounts inside the service over and over (see below) |

Both lists limit queuing: requests for the listed models wait at most the queue limit for a free account or worker, then get a 429 with `Retry-After` (the queue limit clamped to 1 to 30 seconds, or 5 seconds when the limit is 0; the header adds random jitter, see `Retry-After` below); the limit is shared by the whole request, with the rules described for `PRIMARY_QUEUE_WAIT` above. A model can be in both lists: normal paths use the normal list, `/ultra` looks only at the Ultra list, and a model that is only in the normal list queues as usual on `/ultra`. Dead slots and replacements follow each partition's own list too: workers of the Ultra partition are judged by the Ultra primary models only and the normal partition by the normal list only (they are not merged even with `ULTRA_EXCLUSIVE=false`, so primary models of the normal pool never retire the scarcer Ultra workers).

Ultra primary models return a 429 right away when quota runs out:

- **Every candidate cooling down**: a 429 at once, with `Retry-After` set to the seconds until the earliest recovery (for a daily quota that is the real recovery time); the request no longer waits in place for a cooldown that ends within 1 minute. When an uncooled standby account exists but the Ultra partition has no free worker slot, that is a capacity shortage and the queue limit applies. When the Playground-first pick of the downgrade guard finds everything cooling down, the request still falls back to a pick on any channel as before.
- **Quota 429 during a request**: quota failures are counted in two classes, each with its own switch limit, and switching never backs off. The daily class (a daily quota, or any cooldown with at least 15 minutes left, such as a per-minute limit with a very long upstream retry delay) allows at most 2 more account switches (at most 3 accounts per request that fail on the daily quota; the probe on an account's other channel is not counted again); the rate class (per-minute limits, and the rate limit of a 429 without quota details: 30 seconds the first time, longer each time it repeats, see [Pool busy 429 and escalating rate-limit cooldowns](#pool-busy-429-and-escalating-rate-limit-cooldowns); once it reaches 30 minutes or more it counts as the daily class by its remaining time) allows at most 5 more, because the per-minute window is counted per account (12 per account per minute, shared by all models and both channels) and one account running out says nothing about the others. When the leased account entered a cooldown before the request went out (usually because another request on the same account just got a 429, so this request never reached upstream), it counts as one daily-class failure only if the cooldown that blocked it has at least 15 minutes left; shorter ones just switch accounts without counting. The pick right after each quota failure uses only workers that are already running: an idle one is used right away, busy ones are waited for within the queue limit, and no standby account is cold-started for this request, nor does it wait for background expansion. When some of those busy workers have used up their per-minute requests (which can take up to a minute to free up), a worker for one standby account is also started in the background (if its partition still has room) for later requests. When no running worker can serve it, the request gets a 429 at once while (if its partition still has room) a worker for one standby account is started in the background for later requests. If the worker it switched to then fails for a reason other than quota (a 5xx or an overload, for example), the next pick follows the normal rules and may cold-start a standby account. If the same account's other channel is free, the request tries it there once more first (not an account switch, and the request progress says `同账号换通道重试`; a rate-class failure counts again, a daily-class failure counts once per account); a request with files still copies them to another account and tries there first.
- **Not counted as quota failures**: a 429 for input above the account's input limit, which is still returned as a 400. The first attempt may still cold-start a standby account as usual (at that point nobody knows whether it has quota left).
- **`Retry-After`**: when every candidate is cooling down, the time until the earliest recovery; when the daily-class switch limit is reached or no other account is left, the earlier of 5 seconds and the earliest quota recovery this request has seen; when the rate-class switch limit is reached ("限流失败已达换号上限"), the earliest recovery among the rate-class cooldowns this request has seen, at most 1 minute, or 5 seconds when unknown; when no running worker is left after a quota 429, 5 seconds; never less than 1 second. The response header then adds a random 0 to min(suggested wait ÷ 5 seconds, 3) seconds (nothing when the suggested wait is under 5 seconds), so clients do not all retry the moment a cooldown ends; the "建议重试" value in the management log does not include it.
- **Fixed public message**: the same as when the pool is cooling down, `Resource has been exhausted (e.g. check quota).` (OpenAI `rate_limit_exceeded`, Anthropic `rate_limit_error`, Gemini `RESOURCE_EXHAUSTED`), never the upstream text; the reason, quota and rate failure counts and total time go to the management log only ("Ultra 主用模型额度不足，立即返回 429", see [Runtime logging](docs/logging.md)).

Before writing response headers, a streaming request waits for the first event for the queue limit of the list that matches its pool plus 5 seconds (at least 10 seconds). When an Ultra primary model request cold-starts a standby account for itself (for example on the first attempt when no worker is running), that wait is extended by the start-up time (the headers are held during the start-up): if the freshly started account then returns a quota 429 and the request ends out of quota, the client still gets an HTTP 429 with `Retry-After`. The extension has two caps: it adds up to at most `INIT_TIMEOUT` (once that is used up, cold-starting another account no longer extends it), and the headers are held for at most 50 seconds from the moment the wait starts (or the queue limit plus 5 seconds, if that is longer); at the cap the response starts with a 200 as usual (with heartbeats after that), and later errors arrive in the stream. The client receives no bytes while the headers are held, so a reverse proxy or relay in front needs a read timeout longer than this wait (at most 50 seconds by default; nginx's default `proxy_read_timeout` of 60 seconds and Cloudflare's 100 seconds are enough). With a shorter timeout the proxy disconnects during the cold start, and the disconnect also cancels the start-up in progress. So these 429s usually come back as the HTTP status rather than as an in-stream error after a 200. `PRIMARY_MODELS` of the normal pool and models in neither list behave as before (for a primary model of the normal pool, a queue overrun after the request's own cold start can still be an in-stream error).

> **Upgrade note**: `ULTRA_PRIMARY_MODELS` is not empty by default, so after the upgrade `gemini-4-argon` on `/ultra` becomes an Ultra primary model automatically. If you do not want that, write `ULTRA_PRIMARY_MODELS=` (an empty value) in `.env` or clear the Ultra primary model list in the management UI; an empty process environment variable has no effect (an empty value counts as unset, so the value in `.env` or the default is used). Also, `PRIMARY_MODELS` now applies to normal paths only: if you relied on it to make `/ultra` requests fail fast, add those models to `ULTRA_PRIMARY_MODELS`.

### Port Configuration

- **Management UI and APIs**: Default port `2048`
- **Camoufox**: Local ports are allocated dynamically

## Advanced Features

### Proxy Configuration

HTTP, HTTPS, and SOCKS5 proxies are supported, with or without credentials (static and rotating IP accounts from proxy providers can be entered as they are):

1. Set the default proxy under Service Configuration (`PROXY` in `.env`)
2. Set an account-specific proxy when editing an account, logging in with a browser, or importing from Chrome; leave it empty to use the default proxy
3. The account proxy is used for login, WAA, and business requests

Five formats are accepted; http is assumed when no scheme is given, and `socks5h://` is treated as `socks5://`:

- `http://host:port` (no credentials; likewise `https://` and `socks5://`)
- `http://user:pass@host:port` (likewise `https://`)
- `socks5://user:pass@host:port`
- `host:port:user:pass` (the most common format in provider dashboards; everything after the third colon is the password, and a scheme may be added, for example `socks5://1.2.3.4:1080:user:pass`)
- `user:pass@host:port`

The `user:pass:host:port` and `host:port@user:pass` orders, which put the credentials and host:port the wrong way round, are not supported: with a numeric password they cannot be told apart from `host:port:user:pass` and `user:pass@host:port` (for example `myuser:54321:1.2.3.4:8080` and `1.2.3.4:8080@myuser:54321`), so saving fails with a hint to use `http://user:pass@host:port` instead. The check fires when the host part is not an IP and has no dot while `user:pass` looks like an IP or dotted domain plus a port; it also fires for a dotless proxy host name (for example `localhost` or a LAN host name) with a domain-like username and a numeric password, so write the proxy host as an IP in that case. IPv6 addresses need brackets, for example `http://user:pass@[2001:db8::1]:8080`.

Notes:

- **Saved in URL form**: for example `1.2.3.4:8080:user:pass` is saved as `http://user:pass@1.2.3.4:8080`. Every character of the username and password other than letters, digits, and `- . _ ~` is percent-encoded, so `.env` needs no quotes; a valid URL without credentials is saved unchanged. The only exception is the colon form with a scheme and a numeric or empty password (for example `http://1.2.3.4:8080:user:123`): earlier versions read it as a URL without credentials whose host name contains colons (which never worked), and it is now treated as credentials and saved as `http://user:123@1.2.3.4:8080`; such a value that fails the rule above (for example `http://myuser:54321:1.2.3.4:8080`) stops the service from starting when it is in `.env`, and keeps that account from loading when it is in `account.json`, so rewrite it as the error message says. In the URL and `user:pass@` forms, `%XX` sequences are percent-decoded; to keep a literal sequence such as `%41` in the password, use the colon form. With a scheme and credentials but no port, http gets 80 and https gets 443; without a scheme a port is required; SOCKS5 always requires a port; the username of an HTTP/HTTPS proxy cannot contain a colon (Basic authentication splits the username and password at the first colon), and the SOCKS5 username and password are each limited to 255 bytes.
- **Two egress paths**: with credentials, the browser (the Camoufox WAA worker, browser login, and login verification) reaches the upstream proxy through a relay on a random `127.0.0.1` port (Firefox proxy settings have no credentials; the relay adds the authentication and forwards the tunnel bytes unchanged), while background requests (business requests, WAA of the Go backend, Chrome import, and auth renewal) connect to the upstream proxy with the credentials directly. Both paths use the same upstream, so the exit IP is the same with a static IP or a sticky session. Proxies without credentials do not go through the relay and behave exactly as before.
- **Rotating IPs**: enable a sticky session at your provider and set a long session duration. A proxy that rotates per connection makes browser pages and background requests leave from different IPs, and the IP also changes when a session expires.
- **Test proxy**: the default proxy and account proxy inputs have a "Test proxy" button that queries the third-party exit IP service `api.ipify.org` through the proxy and shows the exit IP and time for the browser path and the background request path, warning when the two IPs differ (the proxy rotates per connection). Only api.ipify.org is contacted, not Google, and api.ipify.org sees your exit IP. In the account dialogs an empty proxy tests the default proxy; the test also works while the service is stopped. The button tests the value currently in the input without saving it first; on the settings page the auto-apply countdown does not start while a test is running and does not run after a failed test, so click "Apply now" yourself if needed.
- **`https://` without credentials**: the browser (Camoufox) connects to the proxy's TLS port as a plain HTTP proxy, so login and WAA will likely fail (current behavior); use the `http://` form or a credentialed URL (the local relay handles TLS to the upstream).
- **Storage and display of credentials**: `.env` and `account.json` store the proxy with its credentials in plain text, with file mode 0600; the management API returns the full proxy (at the same level as `PROXY_API_KEY`; the management page itself requires the admin token), and the page shows only `scheme://host:port` without credentials plus an "auth" tag until you click into the input. Credentials passed with `--proxy` on the command line are visible to other users of the machine in `ps`, so prefer `.env`. Accounts imported with `setup` get the default proxy of that moment written into their own `account.json`; after the proxy password changes, update those accounts one by one (or clear their account proxy to use the default proxy). Opening the management page directly by server IP and port (for example `http://<server IP>:2048`) uses plain HTTP, so the proxy credentials in the configuration, the account list, and the live event stream travel in clear text, just like the admin token; put the page behind an HTTPS reverse proxy, or use an SSH tunnel `ssh -L 2048:127.0.0.1:2048 <server>` and open `http://127.0.0.1:2048`.
- **Relay security**: the relay listens only on `127.0.0.1` on a port chosen by the system and has no authentication of its own: while a browser is running, other local processes could use it to go out through your proxy (the same level of exposure as the debugging port Camoufox opens on the loopback address). Credentials are not written into the Camoufox temporary profile.

### Authentication File Management

Authentication files are stored in `auth/` by default:

| Path | Contents |
| --- | --- |
| `auth/<Google email>/account.json` | Account email, proxy, locale, timezone, and enabled state (a missing `enabled` field means enabled; `false` without a manual disable record is switched to enabled and written back at startup or import); the proxy is stored with its credentials in plain text, with file mode 0600 |
| `auth/<Google email>/storage-state.json` | Google cookies and authentication renewal material |
| `auth/<Google email>/runtime-state.json` | Benefit tier, model eligibility, cooldowns, and resource ownership |
| `auth/.permission-denied.json` | List of 403 permission denied accounts (account, model, channel, count, and time); an account is removed once its auth file is updated |
| `auth/<Google email>/camoufox-cache/` | Web cache of that account's browser; can be deleted while the service is stopped |
| `auth/.leases/<Google email>.lock` | Cross-process lease for the account directory |
| `[user cache]/AIStudio2API/runtime-leases/<Google email>.lock` | WAA Worker lease for that email on the current computer |

The lowercase Google email is the account directory, management UI identity, and log source. `.leases` coordinates account-directory access, while the runtime lease in the user cache allows one WAA Worker per email on the current computer.

The Accounts page supports Chrome batch import and isolated Camoufox login. `ready` accounts can be edited, disabled, verified, and deleted; `auth_required` accounts can log in again. Verification reports its conclusion (valid, or why the login expired), and bulk verification counts "valid / login expired / failed" separately (see [Egress rate limiting, catalog cache and "login required"](#egress-rate-limiting-catalog-cache-and-login-required)).

Accounts are enabled by default: at startup and when the directory watcher imports a folder, an account whose `account.json` says disabled (or has no `enabled` field) and that has no manual disable record is switched to enabled and written back in parallel, with a single summary log line. Only accounts disabled from the management page or API stay disabled; disabling an account that is already disabled also records it as disabled by hand. That record is written before the API call returns, survives the folder being moved away and put back, and is cleared only when an administrator enables the account (including bulk enable) or deletes it. New-account onboarding never enables an account disabled by hand, except accounts queued with "Process disabled accounts". If `.disable-reasons.json` exists but cannot be read, a warning is logged, disabled accounts without a record stay disabled and are recorded as disabled by hand, and the old file is kept as `.disable-reasons.json.unreadable-<time>`. "Enable all" and enabling a selection call `POST /api/accounts/bulk-enable` in batches of 500 (at most 1000 per request): each account only gets its enabled flag rewritten, and the account and model snapshot is merged and pushed once per batch instead of once per account. Accounts that are verifying, logging in, or serving a request are reported as busy after 3 seconds; retry later. Enabled accounts without a model catalog sync it in the background (16 at a time) and become usable once it arrives; accounts whose login has expired then show as needing login.

## Documentation

- [Development and contribution](docs/development.md)
- [Google AI Studio protocol specification](docs/protocol.md)
- [WAA implementation](docs/waa.md)
- [Build channel](docs/build.md)
- [Runtime logging](docs/logging.md)
- [Reusable reverse-engineering development guide](docs/reverse-engineering.md)

## Important Notes

### About Camoufox

This project uses [Camoufox](https://camoufox.com/) to reduce automation detection. Camoufox is based on Firefox and changes lower-level browser behavior to retain a realistic device fingerprint.

Go handles encoding, scheduling, streaming decode, and public protocols. WAA-protected `GenerateContent` requests are sent by the account's fingerprinted Camoufox page, preserving the native Firefox TLS/HTTP2 stack, headers, cookies, and page fingerprint.

With `WAA_BACKEND=go`, WAA runs inside the service process, emulates the Firefox page environment from the account fingerprint, and sends requests with Firefox request headers; Camoufox is neither downloaded nor started at runtime. Browser login on the Accounts page still uses Camoufox and prepares it on first login.

### Limitations

- **Client-Managed History**: Clients submit complete conversation context for Chat, Anthropic, and Gemini requests
- **AI Studio History**: API requests are not saved to website history; `TEMPORARY_CHAT=true` also disables autosave for the WAA prewarm page
- **Responses and Interactions Sessions**: `previous_response_id` and `previous_interaction_id` are stored only in the current process and are cleared on restart
- **Authentication Expiry**: Chrome imports retain DBSC renewal material; isolated-login accounts must log in again after authentication expires
- **Input Length Cap**: Some accounts have a separate input cap on Playground and Build (for example 131072 tokens). A Playground cap error retries once on Build, and once more on a higher-tier account when the pool has one; both retries only take accounts available right now. If the input still exceeds the cap the request returns 400 with the same body as a safety-blocked prompt (`Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)`, `content_policy_violation` for OpenAI) so the per-account cap is not exposed to clients, even if upstream reported it as 429, and no account is cooled down; the admin log still records the real reason. A model hard limit (`(2000000) exceeds … (1048576)`) returns 400 with Google's text (`context_length_exceeded` for OpenAI, `prompt is too long` for Anthropic) immediately without retrying

## Troubleshooting

### Windows Port Reserved by System

If startup reports that the port configured by `LISTEN_ADDR` is unavailable while Task Manager shows no owning process, Hyper-V, WSL2, or Docker NAT may have reserved the port range.

Run the following commands from an elevated PowerShell or CMD window.

#### 1. Inspect Reserved Port Ranges

```powershell
netsh interface ipv4 show excludedportrange protocol=tcp
```

If `2048` falls inside a reserved range, change `LISTEN_ADDR`, or restart WinNAT and inspect the range again:

```powershell
net stop winnat
net start winnat
```

When the port is free, it can be reserved persistently:

```powershell
netsh int ipv4 add excludedportrange protocol=tcp startport=2048 numberofports=1 store=persistent
```

Common runtime states:

| State | Resolution |
| --- | --- |
| The page does not open automatically | Open the address configured by `LISTEN_ADDR` in `.env` |
| `service_stopped` | Click "Start service" in the management UI |
| No account is available | Add, enable, or log in to an account from Accounts |
| Camoufox preparation fails | Check access to GitHub Releases or set `CAMOUFOX_PATH` |
| Linux account warmup `exit status 255` | Install the Camoufox runtime libraries, see the apt command in “System Requirements” |

### Primary model returns 429 or first token is slow

Look at the model's line "Primary <model>: N warm · C cooling · M standby" in the management sidebar (or `primary_supply` in `/api/status`):

- **0 standby (yellow)**: no idle account in the normal pool can serve this model any more, most likely because the usable accounts have used up the day's quota for it. Dead slots that are retired cannot be replaced with an account that serves it, so requests wait for a resident worker that still can, and get a 429 once they queue longer than `PRIMARY_QUEUE_WAIT`. The quota resets at midnight Pacific time (15:00 Beijing time during PDT, 16:00 during PST) and supply recovers by itself; until then the only options are adding accounts or sending fewer requests for this model.
- **Standby above 0 but few warm**: check `WARM_WORKER_LIMIT`; it should be at least the number of workers you want resident. Below the resident count, slots freed by retirement or reaping are not refilled by prewarming and extra idle workers are reaped after 5 minutes, so primary-model requests wait for cold starts.
- **Dead slots or draining that stay high**: search the runtime log for "替补跳过" (no standby account in the partition can serve those models without a cooldown) and "排空超时" (a pinned or file-bound request keeps using that worker). Two more cases are normal and write no log line: while a draining worker has a long stream that has started output, the drain lasts until the stream ends, at most the total cap (1 hour by default), and "排空超时" only appears after the total cap plus 30 seconds; once "draining" reaches 2 × the drain allowance (`drainLimit`: `max(WARM_STARTUP_CONCURRENCY, 1) × 2`, at most a quarter of the partition peak and at least 1), the remaining dead slots wait for one of those drains to finish before draining, and keep serving other models meanwhile. When the slots given up by draining are already used up, the replacement of a new drain is not started because the slots are full (log line "替补未启动 | 原因=排空" with a slots-full error) and comes up once the old worker closes.
- **429 for an Ultra primary model**: look at the "Ultra primary <model>" line below the Ultra workers; it is counted the same way, for the Ultra pool only. These models get a 429 right away when quota runs out (see [Primary models](#primary-models)); the runtime log shows "Ultra 主用模型候选全部冷却，立即返回 429" or "Ultra 主用模型额度不足，立即返回 429", the latter with the reason and the number of quota failures. The client only needs to retry after `Retry-After`.
- **Many cooling, few warm**: most resident workers are cooling down for this model, usually because of escalating rate-limit cooldowns or the day's quota running out; for pool busy 429s see the candidate snapshot and the escalating rate-limit cooldowns in the next section, or run `bash tools/diag_busy.sh`.
- **"Egress limited" in the sidebar, or many accounts cooling at once**: usually one egress IP is throttled by the upstream as a whole, and neither switching nor adding accounts helps; see [Egress rate limiting, catalog cache and "login required"](#egress-rate-limiting-catalog-cache-and-login-required). When accounts needing login suddenly increase, see the login recheck in the same section.

### Pool busy 429 and escalating rate-limit cooldowns

**What a pool busy 429 is**: a request for a primary model (`PRIMARY_MODELS` on normal paths, `ULTRA_PRIMARY_MODELS` on `/ultra`) did not get a free account or worker slot within the queue limit (`PRIMARY_QUEUE_WAIT`, `ULTRA_PRIMARY_QUEUE_WAIT`), so it gets a 429 with `Retry-After` right away for the client to retry later, instead of queuing until `REQUEST_TIMEOUT`. It means there is not enough capacity for that model at this moment; it is not an upstream quota error. The client sees the same message as for exhausted quota (`Resource has been exhausted (e.g. check quota).`); the difference shows only in the management log: `request.error` (and the usage ledger) starts with "号池繁忙：模型 … 暂时没有空闲的账号或 Worker" and gives the queued time, the limit, the total time, the attempt number and the last wait reason, and the request timeline line "主用模型排队超过 10s，立即返回 429" carries a snapshot of the candidates at that moment (fields in [Runtime logs](docs/logging.md#排队等待)):

- `热空闲`, `热忙碌`, `备用`, `备用忙碌`: idle and busy accounts that already have a worker, and idle and busy standby accounts; accounts in a cooldown are not counted. `每分钟已满` is how many of them count as busy because of `ACCOUNT_RPM_LIMIT`, `启动中=是` means a candidate's worker is starting, and `最早恢复` is how long until the earliest candidate cooldown ends.
- `驻留冷却`: the number of resident workers cooling down for that model, i.e. `cooling` of `primary_supply` in `/api/status` (counted about every 30 seconds).

How to read it: when `驻留冷却` is at least `热空闲` plus `热忙碌`, most resident workers are cooling down for the model (usually the escalating rate limit below, or the day's quota running out); when most of `热忙碌` is `每分钟已满`, the per-minute limit is the bottleneck; when `备用` and `备用忙碌` are both 0, the pool has no idle account left that can serve it; otherwise concurrency usually exceeds workers × per-account concurrency (see the previous section).

**Escalating rate-limit cooldowns**: for accounts that have used up their quota, upstream often returns a bare `Quota exceeded` (no quota period and no retry delay). Such a 429 used to cool the account down for only 30 seconds, so the account came back, got picked and failed again every 30 seconds, and its resident worker never counted as cooling down nor became a dead slot, so it was never replaced by another account. Now, when the same account gets this 429 for the same model again after its cooldown has ended, the cooldown goes up one level:

| Level | Cooldown | Resident worker |
| --- | --- | --- |
| 1 | 30 seconds | Less than 2 minutes left: not counted as cooling down, not retired |
| 2 | 5 minutes | Counts as cooling down while more than 2 minutes are left and every supported enabled channel is cooling down; can be replaced by a standby account when the partition is full |
| 3 | 30 minutes | A dead slot while at least 15 minutes are left and every enabled channel is cooling down; drained, retired and replaced |
| 4 | 2 hours | Same as above |
| 5 and up | Until the next midnight Pacific time | Same as above |

- No level runs past the next midnight Pacific time. One success on that channel resets the account to level 1 (the runtime log says "限流档位清零" when it had reached level 2 or higher), and a new quota day starts again at level 1.
- Unless the Build and Playground channels have been judged to have separate quotas (the "每日额度通道判定" verdict in the runtime log is shared or still observing, or there is no record yet), both channels share one level, and the other enabled channel is cooled down at the same time; when they are judged separate, each channel has its own level.
- The level does not go up for a 429 that arrives after output has already been sent to the client (it is still cooled down), nor for concurrent requests that were sent before the cooldown was written, or the same request retrying on the other channel.
- Pool-level guard: when at least 5 accounts in the same pool get this 429 for the same model within 10 minutes and nothing succeeds in that time, it is treated as an upstream-wide throttle or outage: cooldowns stay at level 1 and do not escalate, and the runtime log says "号池级限流" (at most once every 10 minutes), so the whole pool is not cooled down for hours. Switching accounts does not help then; send fewer requests or wait for upstream to recover.
- Levels live in memory only and start again at level 1 after a restart; the cooldowns themselves are still saved in each account's `runtime-state.json` and restored after a restart.
- A 429 with a retry delay (RetryInfo, `retry in <duration>` in the error message, `Retry-After`), a per-minute or daily period, or an account-wide quota does not escalate and is cooled down as before; a 429 returned while copying files or uploading attachments has nothing to do with the model quota, does not escalate either, and cools down only that channel for 30 seconds.

The runtime log line for such a cooldown carries `连续限流=第 N 档`, plus `同步=两个通道` when both channels were cooled down (see [Runtime logs](docs/logging.md#额度冷却日志)).

**How the three quota 429s are recognized**: the metric, limit value and model in the upstream's private GeminiApiErrorDetails, and `Please retry in <duration>` in the error message, are parsed:

- Daily limit of a free account (message `You exceeded your current quota …`, 10 per day): a daily-limit cooldown of that model on the account, until the later of the next Pacific midnight and the `retry in` time, logged with `上限=10 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro`.
- Daily limit of a pro/ultra account (message `You've reached your usage limit …`, 50 per day): a daily-limit cooldown until the next Pacific midnight, logged with `上限=50`; it is no longer treated as a bare 30-second rate limit that escalates step by step (previously the same account was retried several times, for 5 minutes or more, before being retired).
- Per-minute limit of a pro/ultra account (12 per minute, shared by all models and both channels): a minute-limit cooldown of the whole account until that minute ends, logged with `上限=12 | 指标=generate_requests_global`; `ACCOUNT_RPM_LIMIT` throttles ahead of it.

If the runtime log shows `额度 429 详情未识别` (quota 429 details not recognized), please send the whole line to the developers: such a 429 carries GeminiApiErrorDetails in a layout different from the ones above, so no quota metric could be parsed (the cooldown line carries `详情类型=` but no `指标=`) and the cooldown can only go by the message (a bare `Quota exceeded` falls back to the escalating rate-limit cooldown); the line carries the raw details used to extend the parser (at most one line per kind every 10 minutes, kept even when ordinary lines fill the console log queue, see [Runtime logs](docs/logging.md#额度冷却日志); section 3 of `bash tools/diag_busy.sh` lists these lines too).

When the two channels are already judged to share the daily quota (the runtime log's `每日额度通道判定` reads shared), a daily limit also cools down the other channel, the resident worker is registered for retirement right away (when at least 15 minutes of cooldown remain), and the request switches accounts at once without backoff; while still observing, the other channel of the same account is tried once first. `模型=` is the model the upstream counts the quota against, which may differ from the requested model name (for example a request for `gemini-3.1-pro-preview` logs `gemini-3.1-pro`). A 429 without a metric additionally carries `详情类型=` (the detail types the upstream attached), to check whether a bare `Quota exceeded` came with quota details that were not parsed.

**One-shot diagnosis**: `bash tools/diag_busy.sh` (run it in the program directory, or point it there with `APP_DIR=/path/to/app`) summarizes why requests got pool busy 429s and ends with automatic conclusions:

```bash
bash tools/diag_busy.sh                         # last 60 minutes, focusing on the primary model with the most pool busy 429s
MINUTES=120 MODEL=<model> bash tools/diag_busy.sh  # wider window, a specific model
QE_MINUTES=720 bash tools/diag_busy.sh          # look back 12 hours (default 6) for the per-account bare Quota exceeded analysis
OFFLINE=1 LOG_FILE=<log> LEDGER_DB=<ledger> DIAG_NOW=2026-10-22T08:00:00Z bash tools/diag_busy.sh  # analyze copies of the log and ledger
```

- Contents: configuration and current capacity (including `cooling` of the primary-model supply), request results per model, the last wait reason of pool busy 429s and the upstream failures before them, a breakdown of the candidate snapshots at queue overflow, upstream 429s by message, how often each rate-limit level was written and reset, pool-level throttles, the levels of current cooldowns, a per-account verdict on bare Quota exceeded (the day's quota used up, or a short-term rate limit), cooldowns and changes of resident workers, and the per-minute limit. `ACCOUNTS=0` skips `/api/accounts` (large with tens of thousands of accounts; without it there is no grouping by benefit tier).
- Read-only: it sends only GET requests to the local management API (reading `.admin-token` by itself), opens the usage ledger read-only, changes no configuration, cooldown, account or worker and creates no file in the program directory, and keeps temporary files in the system temp directory, removing them on exit; when the management API cannot be reached it analyzes only the runtime log and the usage ledger, and when python3 lacks the sqlite3 module it analyzes only the runtime log. Accounts are masked, so the output can be sent to the developers as is.

### Egress rate limiting, catalog cache and "login required"

When the pool has many accounts but still returns frequent 429s, the cause is usually not exhausted quota: either too many requests leave through the same egress IP and the upstream throttles it as a whole, or restarts, catalog syncs and false "login required" verdicts push usable accounts out. None of the following needs configuration; the thresholds are fixed.

- **Why a 429 happened**: the 429s this service decides on (queue limit, all candidates cooling, switches used up, egress rate limiting) carry `Retry-After` (clients just retry after it), and 429s also carry an `X-AIStudio2API-Reason` response header with its source: `local_queue` (queued past the local limit; the request usually never reached the upstream), `local_cooling` (all candidate accounts are cooling down), `upstream_quota` (an upstream quota or rate-limit 429, including running out of account switches) and `egress_limited` (the egress is rate limited). The message and status code are unchanged; streaming and non-streaming 429s of all four protocols carry the header.
- **Account switch limits in the normal pool**: requests that are not pinned to an account switch at most 4 more accounts on daily-quota 429s and at most 2 more times on rate-limit 429s (bare `Quota exceeded`, per-minute limits), and try at most 8 accounts in total; switches caused by an expired login stop after 2. At the limit the request returns 429 with `Retry-After` right away instead of walking the pool (a single request used to switch through a dozen accounts and pull more of them into cooldown). Ultra primary models keep their own rules (see [Primary models](#primary-models)).
- **Egress rate limiting**: when, within 2 minutes, enough accounts behind one egress (the account proxy after normalisation, or direct without a proxy) get bare 429s at the same time and freshly switched-in accounts are rejected on their first request, the egress is judged to be throttled by the upstream. Switching accounts does not help then, so escalating cooldowns and on-demand cold starts pause and requests are queued and sent at the egress rate limit with primary models first (non-primary models keep about a quarter); everything returns to normal once the limit lifts. The sidebar shows "Egress limited <egress> · upstream rejected R% · local 429 N/min · queued Q · L/min" ("Ultra L/min" when only the Ultra pool's lane is throttled, "normal L1/min · Ultra L2/min" when both are): "upstream rejected" is the share of requests sent upstream that were rejected, "local 429" counts requests that queued past the limit and got a 429 locally (header `egress_limited`). If the verdict is wrong (for example the accounts really ran out of quota for the day), press "Withdraw" at the end of the line: escalating cooldowns take over again and the egress is not judged for 30 minutes; "Resume" undoes it. The service also withdraws by itself when the rejected accounts keep failing on retries.
- **Throughput limit of an egress**: how many requests per minute one egress can get through is decided upstream; more accounts do not raise it. The only way to raise it is to give accounts different proxies so requests spread over several egresses (set a per-account proxy when editing the account on the Accounts page, see [Proxy Configuration](#proxy-configuration)). One proxy address (password removed, user name kept) is one egress and is treated as one egress IP: a rotating proxy with the same address is still one egress, while proxies that pick the exit by user name become different egresses with different user names.
- **Restarts no longer sync every catalog**: model catalogs are saved in `runtime/model-catalogs/` and loaded directly on restart; the background only refreshes missing, older-than-24-hours and suspected accounts slowly (idle accounts only, pausing on upstream 429 or while the egress is rate limited). Prewarming starts right after a restart and the egress is no longer flooded by thousands of catalog requests at startup. As a result a restart no longer clears global account cooldowns as a side effect (they expire on their own), and neither does a background catalog refresh; "login required" is not persisted, so it resets to zero on restart and climbs back with use (false verdicts are recovered by the recheck below). The sidebar lines "Catalog sync paused until …" and "N catalogs pending · M suspected logins to probe" only show while paused, when catalogs are missing or when suspected logins are waiting to be probed.
- **Unschedulable resident workers are released right away**: a resident worker whose account needs login, is disabled or has no model catalog cannot serve any model but holds a slot; it is now closed when idle (busy ones after their requests end) and prewarming refills the slot. The sidebar worker line shows "N unschedulable".
- **Fewer false "login required" verdicts, with automatic recovery**: a single upstream 401 no longer marks the account as needing login. It first gets a 5-minute cooldown and a "suspected expired login" entry, then one recheck with the cookies from the auth file; only a second 401 marks it, and a passing recheck lifts the cooldown. When an image request fails to obtain attachment authorization (`GenerateAccessToken`), only that attachment attempt fails and the account cools down for 2 minutes; it is no longer marked as needing login. Accounts already marked as needing login are rechecked slowly every 15 minutes and return to ready once API authentication works again; those still failing are rechecked less and less often (up to every 6 hours). Accounts the browser itself found logged out (verification redirected to the sign-in page, or a Worker start landed on it) are not recovered by the API recheck; they need a new login or an updated auth file. No rechecks run while the egress is rate limited. The sidebar line "Login recheck · N suspected · M new login-required in 10 min · K bad accounts" shows when there are suspects, recent marks or bad accounts; many new marks within 10 minutes usually point to an egress or upstream problem rather than accounts that really expired.
- **Bad accounts**: an account with 2 bare rate-limit 429s in a row and no success in between is left out of prewarming, scale-out and replacements for 2 hours (while other candidates exist), and is restored by one success; prewarming and replacements prefer accounts that recently succeeded. The record lives in `runtime/model-catalogs/bad-accounts.json` and survives restarts.
- **Pace of on-demand replacement**: workers that became ready less than 2 minutes ago are not replaced on demand, and each partition replaces at most 10 workers per minute on demand, so workers no longer keep opening and closing during request peaks.

"Verify" on the Accounts page now reports its conclusion: when the browser is logged in, the API is checked once more. A single verification shows "Account verified" or "Login expired: <reason>" (when the browser is not logged in, the reason includes the login page it was redirected to, such as `accounts.google.com/v3/signin/identifier`; when the browser is logged in but API authentication fails, the cookies have probably been revoked by Google and the account must log in again). Bulk verification counts "valid / login expired / failed" separately and lists the expired accounts with their reasons, instead of counting everything that did not throw as a success.

Section 0 of `bash tools/diag_busy.sh` shows the current egress verdicts, catalog sync and login recheck; section 9 summarises their log lines and the conclusions point out egress rate limiting, paused catalog syncs or many confirmed suspects. Log fields are listed in [Runtime logs](docs/logging.md#429-治理).

### Slow first token for gemini-4-argon on Ultra

`gemini-4-argon` needs a subscription and is only offered on the Build channel. A slow first token usually comes from a mix of the following:

- **Build single call**: models that need a subscription are generated through a single call on the Build channel, which returns the whole reply at once when it is finished, so even a streaming request receives its first token only then and the time to first token is about the time of the whole generation. Every model with `build_unary: true` in the management catalog (`GET /api/models`) works this way; this model has no Playground channel, so `STREAM_PLAYGROUND_MODELS` does not help.
- **Highest thinking by default**: a model name without a suffix sets the thinking level to the highest (HIGH) unless the client explicitly passes `reasoning_effort` or another thinking parameter; the longer it thinks, the later the single call returns.
- **Per-minute quota cooldowns**: a per-model minute limit cools down only that model on the account and recovers quickly, but with few Ultra-pool accounts all of them cool down easily. As an Ultra primary model (the default) the request gets a 429 with `Retry-After` right away; once removed from `ULTRA_PRIMARY_MODELS`, requests wait in place for a cooldown that ends within 1 minute. There is also a per-account minute limit (`quota_limit=GeneratePerMinutePerProjectPerUser`, `quota_limit_value=12`: at most 12 generation requests per account per minute, shared by all models and both channels), and hitting it cools the whole account until that minute ends; `ACCOUNT_RPM_LIMIT` (10 by default) switches away from an account once it has sent that many in the last 60 seconds, and starts new workers once every running worker is full, so the limit is avoided up front, with the 429 cooldown still as the fallback. Search the runtime log for `类型=分钟限额` to see these cooldowns, with `上限=` and `指标=` (see [Runtime logging](docs/logging.md)).
- **Quota 429 account switches and cold starts**: when it is not an Ultra primary model, a quota 429 makes the request walk through the Ultra pool one account at a time, and switching to a standby account without a worker means a browser cold start on the spot (10 to 30 seconds or more each time); as an Ultra primary model it switches at most 2 more times when the daily quota is used up and at most 5 more times for per-minute limits and rate limits, and the switch after a quota failure only goes to workers that are already running.

To diagnose:

```bash
./tools/slow_requests.sh 24 30 gemini-4-argon
```

This lists the 30 requests for the model with the slowest first token in the last 24 hours from the usage ledger, splits the time to first token into queuing (the time before the final account was obtained: queuing, account switches, waiting for workers and cold starts) and upstream (first event minus queuing), and lists every attempt with its account, channel, failure reason and duration: long queuing with many attempts means quota switches and cold starts; short queuing with a long upstream part is the generation time of the single call itself (thinking plus reply).

What to do:

- Keep `FIRST_EVENT_TIMEOUT=0` (the default): the first event of a single call is the whole reply, so a first-event timeout would abandon generations that are long but normal and switch accounts, wasting quota.
- Shorten thinking: use the `-low` or `-medium` suffix (such as `gemini-4-argon-low`; the suffix wins over client parameters), or pass `reasoning_effort` (`low`, `medium`) from the client.
- Make sure it is in `ULTRA_PRIMARY_MODELS` (it is by default), so that when quota runs out the client retries after `Retry-After` instead of the service switching accounts and cold-starting workers.
- With few Ultra accounts that often all cool down, turn on `ULTRA_INCLUDE_PRO` so Pro accounts serve `/ultra` too, provided their Build catalogs list the model (after turning it on, check whether the standby count of "Ultra primary gemini-4-argon" in the sidebar includes Pro accounts), and raise `ULTRA_WARM_WORKER_LIMIT` and `ULTRA_MAX_ACTIVE_WORKERS` at the same time; see [Ultra Pool](#ultra-pool-ultra-prefix).

### Long replies cut off or regenerated

`REQUEST_TIMEOUT` only bounds the part before the first upstream event (queuing, cold starts, WAA proof, waiting for the first token). Once output starts, the stream is not cut while upstream keeps sending events or bytes: the request ends with 504 only when upstream sends no event and no bytes for `REQUEST_TIMEOUT` in a row, and a whole request is capped at 1 hour (or `REQUEST_TIMEOUT` if longer). A cut after output has started still ends with an error; it is never turned into a normal finish. The client always sees 504 `Deadline expired before operation could complete.` (inside the stream for streaming requests); the source shows in `request.error` of the management log or `error` of the usage ledger:

| `error` | Source | What to do |
| --- | --- | --- |
| `本服务首事件前期限 <limit> 到期[（停在 <where>）]: …` | Expired before the first token: queuing, a cold start, or long thinking with thoughts hidden (Anthropic `thinking.type=disabled`, Gemini `includeThoughts=false`: on the Build channel upstream returns no thoughts; on the Playground channel upstream returns them and this service drops them). `（停在 …）` is present when it expired during an attempt, and `（停在 文件资源刷新）` when it expired while refreshing file resources; when it expired while queuing or waiting on the original account the text is `…到期: 排队 … 后请求超时（…）` or `…到期: 回到原账号等待 … 后请求超时（…）`; other expiries outside an attempt (for example during the retry backoff after a timeout-type error) read `…到期: <previous error>` | Raise `REQUEST_TIMEOUT`, or look into queuing (`bash tools/diag_busy.sh`) |
| `本服务首事件后空闲期限 <limit> 到期（已输出 推理=N 正文=M）: …` | Upstream stalled after output started: no event and no bytes for `REQUEST_TIMEOUT` | Check the last `事件流停顿` in the request timeline |
| `本服务总时长上限 1h0m0s 到期（…）: …` | The request ran past the total cap | Raise `REQUEST_TIMEOUT` if you need longer |
| `… 返回 HTTP 504、协议错误码 4: …` | Google's own deadline | Shorten thinking or output |
| `AI Studio stream closed before finish` | Upstream closed the stream before its finish frame | Check warnings for the same account at that moment |
| `context deadline exceeded` on its own (no `本服务` prefix) | Whole-request deadline of an older version, or a token counting, video or transcription request. With `AI Studio … 协议位置` or `BiDi … 等待回复超时` in front it is an upstream or browser link timeout, not this service's deadline | Upgrade; for link timeouts check warnings for the same account at that moment |

On this service, "cut off and then regenerated" shows up as a new request with the same input and a different request ID: after the first upstream event the service never switches accounts or starts over, so the regeneration comes from the client or a relay. The most common cause is a new-api channel of the Claude type with failure retries on: it rewrites the in-stream error as 500 and retries on the same connection, so the client sees the cut first half and then a second answer generated from scratch in one reply. Set new-api's retry count to 0, or switch the channel that points at this service to the OpenAI type. Non-streaming requests send no byte before generation finishes, so behind Cloudflare they get a 524 after about 100 seconds (logged here as a cancelled 499 after about 100 seconds) and a client retry regenerates the whole reply; use streaming for models that think for a long time. The full write-up (including what to do because new-api does not forward `: ping`, and the impact on workers) is in [Long generations and relay retries](docs/development.md#长生成与中转重试) (Chinese).

**One-shot diagnosis**: `bash tools/diag_regen.sh` (run it in the program directory, or point it there with `APP_DIR=/path/to/app`) classifies cut-offs as in the table above, finds resends of the same input and ends with automatic conclusions:

```bash
bash tools/diag_regen.sh                          # last 24 hours, focus on the model with the most cut-offs
MINUTES=4320 MODEL=<model> bash tools/diag_regen.sh  # wider window, a given model (MODEL=all for every model)
RETRY_GAP=600 bash tools/diag_regen.sh            # count the same input within 600 s after the previous request as a resend (default 300)
OFFLINE=1 LOG_FILE=<log> LEDGER_DB=<ledger> DIAG_NOW=2026-10-22T08:00:00Z bash tools/diag_regen.sh  # analyze copies of the log and ledger
```

- Contents: related settings and nginx timeouts, end reasons and durations (with clusters), the stage and output counts of this service's own deadline cuts, resends of the same input (client or relay retries) and account switches before the first event inside the service, log keywords (`本服务期限截断`, `事件流停顿`, `协议错误码 4`, `stream closed before finish` and more), and requests interrupted around a stop or a configuration restart.
- Read-only: it only sends GET requests to the local management API (reading `.admin-token` automatically), opens the usage ledger read-only and only reads nginx configuration files; it changes no settings, cooldowns, accounts or workers and sends no generation request. Temporary files go to the system temporary directory and are removed on exit; without the management API it analyzes the runtime log and the usage ledger only, and without the python3 sqlite3 module the runtime log only. The output contains no prompt text and masks accounts.

## Contributing

Issues and Pull Requests are welcome!

## Development Roadmap

- ✅ **TTS Support**: Adapted `gemini-2.5-flash/pro-preview-tts` speech generation models
- ✅ **Media Generation**: Supports Imagen 3, Veo 2, Nano Banana image/video generation
- ✅ **Documentation**: Update and optimize documentation in `docs/` directory
- **One-Click Deployment**: Provide fully automated install and launch scripts for Windows/Linux/macOS
- ✅ **Go Refactoring**: Migrate core proxy service to Go for improved concurrency and reduced resource usage
- ✅ **Multi-Worker Load Balancing**: Support multi-Google account rotation pool for higher concurrency limits

### Pure Go WAA Runtime

- ✅ **Pure Go backend**: `WAA_BACKEND=go` runs the official interpreter and program inside the service process, emulating the Firefox page environment from each account fingerprint; it neither downloads nor starts Camoufox at runtime, while account login still uses Camoufox
- **Firefox engine details**: implement `Intl` formatting, the global resolution timing of regular-expression literals, and the Symbol key order of `RegExp.prototype`
