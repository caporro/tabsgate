# TabsGate

An accountless, containerized AI API gateway, usable with [Tabs Chat](https://tabs.chat) and other OpenAI-compatible clients. Bring a provider key, choose a management keyword, and issue revocable virtual keys with request, token, model and time limits. Go + SQLite, with a built-in minimal management UI; no accounts, cookies or application login.

## Quick start with Docker

Clone the repository and enter its directory:

```sh
git clone https://github.com/caporro/tabsgate.git
cd tabsgate
```

```sh
docker compose build
mkdir -p secrets
chmod 700 secrets
(set -C; docker run --rm tabsgate:local keygen > secrets/master_key)
chmod 644 secrets/master_key
docker compose up -d
curl http://localhost:8080/readyz
```

Run `keygen` **once for a new database**. Retain the master key securely: changing or losing it makes the stored provider credentials unusable. The service refuses to open an existing database with a different key. `secrets/` is excluded from Git and the Docker build context.

Compose binds to `127.0.0.1:8080`, uses a persistent named volume, a non-root process, a read-only root filesystem and a mounted secret. The image contains a static binary and CA certificates, with no shell. The secret file is readable by the container's UID 65532; the enclosing host directory stays private with mode `700`. This is intentional: Compose file secrets retain host file permissions, so a host-owned `600` file is not readable by the non-root service. A production secret manager can supply the file directly with appropriate ownership and permissions; do not put it in the image.

Expose the service through an HTTPS reverse proxy for remote clients. Keep response buffering off for SSE. Configure an upstream timeout longer than the gateway's request timeout. If the proxy runs in another container, attach it to the same network and route to `tabsgate:8080`. Change `TABSGATE_PORT` to select a different loopback host port.

## Management UI

Open `http://localhost:8080/` after starting the service. Select a provider, enter
its original API key and a pass, then choose **Open**. On first use the pass is
chosen by you; reopening the same connection requires the same pass. There is
no pass recovery.

The page lists your virtual keys. **New key** creates one with a name, model,
request limit, token threshold and expiry. Initial limits are 30 model calls,
1,000,000 tokens and 7 days; change them before creating. The model is chosen
explicitly from the provider catalog, or entered as an ID. Additional allowed
models and an optional output cap are under **Options**. The server supplies
its configured default output cap when the field is left blank.

Click a key to read its usage, state and expiry, modify its limits or revoke it.
**Refresh** reads current usage from the API. Edits preserve recorded usage
and keep the exact original expiry unless explicitly changed. A revoked key
cannot be restored. The complete virtual key is shown only on successful
creation: copy it and the displayed base URL for your client.

**Connection** opens settings for replacing the provider API key and/or changing
the pass. Blank fields keep the current values. Virtual keys, limits, expiry and
usage are preserved, and the browser switches to the new credentials after a
successful update. The provider itself stays fixed for that connection.

**Delete connection** asks for confirmation, then removes the saved provider
credential, pass, all virtual keys and their usage records from TabsGate. It
does not revoke the original key at the provider or remove unrelated
connections. This action does not affect the instance encryption key in
`secrets/master_key`.

**Sign out** asks for confirmation before clearing the current page. Cancelling
or pressing Escape keeps the view and unsaved form values. Signing out leaves
the connection and virtual keys active.

The UI is in English, including errors, labels and date/number formatting.
Original credentials and newly issued keys stay in page memory only. Signing
out, refreshing or leaving the page clears them; there are no cookies,
localStorage or sessionStorage entries. A lost creation response is never
retried automatically: check the list and revoke any key whose value was lost
before issuing another.

The UI is embedded in the Go binary and works in the existing container, with
no frontend build, Node runtime, CDN or external assets. Its source, HTTP
adapter and static handler are logically separate in
[`internal/webui`](internal/webui/README.md); `internal/gate` remains the API
implementation. The page works on mobile and follows the device's light/dark
appearance. Management integration inside Tabs Chat is a separate future change.

If `TABSGATE_CORS_ORIGINS` is restricted, include the UI's own origin as well as
the origins of external clients. The built-in page calls its gateway on the
same origin.

## A complete API flow

Examples use `curl` and `jq`. The values below are placeholders; send real credentials only over HTTPS outside localhost. Original keys and keywords belong only in management calls, never in shared links or chat messages.

```sh
export GATE=http://localhost:8080
export PROVIDER_KEY='your-provider-key'
export KEYWORD='your-management-word'

CONNECTION=$(curl --fail-with-body -sS "$GATE/api/v1/connections" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg key "$PROVIDER_KEY" --arg word "$KEYWORD" \
    '{provider:"openai",api_key:$key,keyword:$word}')")
CONNECTION_ID=$(printf '%s' "$CONNECTION" | jq -r .id)

# Discover models available with your original credential.
curl --fail-with-body -sS "$GATE/api/v1/connections/$CONNECTION_ID/models" \
  -H "Authorization: Bearer $PROVIDER_KEY" \
  -H "X-TabsGate-Keyword: $KEYWORD"

export MODEL='a-model-id-from-the-list'
VIRTUAL=$(curl --fail-with-body -sS "$GATE/api/v1/connections/$CONNECTION_ID/keys" \
  -H "Authorization: Bearer $PROVIDER_KEY" \
  -H "X-TabsGate-Keyword: $KEYWORD" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg model "$MODEL" \
    '{name:"Information collection",max_requests:30,token_budget:1000000,
      ttl_seconds:604800,allowed_models:[$model],max_output_tokens:4096}')")
VIRTUAL_KEY=$(printf '%s' "$VIRTUAL" | jq -r .key)
VIRTUAL_ID=$(printf '%s' "$VIRTUAL" | jq -r .id)

curl --fail-with-body -sS "$GATE/api/v1/key" \
  -H "Authorization: Bearer $VIRTUAL_KEY"

curl --fail-with-body -N "$GATE/v1/chat/completions" \
  -H "Authorization: Bearer $VIRTUAL_KEY" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg model "$MODEL" \
    '{model:$model,stream:true,messages:[{role:"user",content:"Hello"}]}')"
```

The `key` value is returned **only at creation**. Store it before discarding the response. Listing keys returns their IDs and short prefixes, never their secrets. If a create response is lost, list and revoke the unwanted key, then create another; creation is not automatically retried.

In Tabs Chat, select its OpenAI-compatible connection, set the base URL to `https://your-gateway/v1` and use the **virtual key** as the API key. Select one of that key's allowed models. Share only this configuration; exclude any original-provider connection from the shared agent. Management UI integration is a separate future change.

## Endpoints

Machine-readable contract: [OpenAPI 3.1](docs/openapi.json).

API routes return JSON except successful streaming chat responses and `204` deletes. The UI is served at `/` with embedded assets under `/assets/`. There are no cookie sessions. Query parameters are rejected, including API keys in URLs.

| Method | Path | Access | Purpose |
| --- | --- | --- | --- |
| GET | `/healthz` | Public | Process liveness |
| GET | `/readyz` | Public | Database and accounting readiness |
| GET | `/api/v1/providers` | Public | Supported provider catalog |
| POST | `/api/v1/connections` | Credentials in body | Register or reopen a connection |
| GET | `/api/v1/connections/{id}` | Management | Connection metadata and aggregate usage |
| PATCH | `/api/v1/connections/{id}` | Management | Change upstream key and/or keyword |
| DELETE | `/api/v1/connections/{id}` | Management | Delete connection, credentials, keys and usage records |
| GET | `/api/v1/connections/{id}/models` | Management | Provider model discovery |
| GET | `/api/v1/connections/{id}/keys` | Management | List keys, limits and usage |
| POST | `/api/v1/connections/{id}/keys` | Management | Issue a virtual key |
| PATCH | `/api/v1/connections/{id}/keys/{keyId}` | Management | Change a key's configuration |
| DELETE | `/api/v1/connections/{id}/keys/{keyId}` | Management | Permanently revoke a key; retain counters |
| GET | `/api/v1/key` | Virtual key | Own status and usage, including expired/revoked states |
| GET | `/v1/models` | Active virtual key | Model list filtered by the key's allowlist |
| POST | `/v1/chat/completions` | Active virtual key | Text chat, function tool calling, optional SSE |

Management headers are `Authorization: Bearer <original-provider-key>` and `X-TabsGate-Keyword: <keyword>`. Virtual endpoints accept `Authorization: Bearer tg_...`. IDs are not authorization credentials. An existing provider/key pair can only be reopened with its existing keyword. There is no keyword recovery.

Registration body: `{ "provider": "openai", "api_key": "...", "keyword": "..." }`. A new connection returns `201`; reopening returns `200` and the same ID. Registration does not spend tokens or verify the key with the provider; model discovery or the first chat establishes provider access.

Connection PATCH accepts `api_key`, `keyword`, or both. Authenticate with the current credentials. Replacement keys already registered to another connection are rejected. Existing virtual keys and usage survive rotation. The old credentials stop authorizing new management requests.

### Virtual key configuration

| Field | Create | Meaning |
| --- | --- | --- |
| `name` | Optional | Up to 160 bytes |
| `max_requests` | Required | Integer, 1–1,000,000 |
| `token_budget` | Required | Integer, 1–1,000,000,000,000; input + output |
| `ttl_seconds` | One expiry field required | 1–31,536,000 seconds from this operation |
| `expires_at` | Alternative to TTL | RFC3339 timestamp, in the future and at most 365 days away |
| `allowed_models` | Required | 1–100 exact model IDs; no wildcards |
| `max_output_tokens` | Optional, default 4096 | Integer, 1–16384; enforced by the gateway |

PATCH accepts any of these fields. Supply either `ttl_seconds` or `expires_at`, never both. PATCH does not reset usage. Lowering a limit below consumption blocks subsequent calls. Increasing limits or extending expiry can restore an exhausted/expired key. A revoked key cannot be reactivated. Revoked keys count toward the limit of 1000 keys per connection.

Example: increase the total request allowance without resetting usage:

```sh
curl --fail-with-body -sS -X PATCH "$GATE/api/v1/connections/$CONNECTION_ID/keys/$VIRTUAL_ID" \
  -H "Authorization: Bearer $PROVIDER_KEY" -H "X-TabsGate-Keyword: $KEYWORD" \
  -H 'Content-Type: application/json' --data '{"max_requests":50}'

# Revoke the virtual key.
curl --fail-with-body -X DELETE "$GATE/api/v1/connections/$CONNECTION_ID/keys/$VIRTUAL_ID" \
  -H "Authorization: Bearer $PROVIDER_KEY" -H "X-TabsGate-Keyword: $KEYWORD"

# Delete the complete connection from TabsGate.
curl --fail-with-body -X DELETE "$GATE/api/v1/connections/$CONNECTION_ID" \
  -H "Authorization: Bearer $PROVIDER_KEY" -H "X-TabsGate-Keyword: $KEYWORD"
```

## Limit semantics

- **A use is one admitted attempt to call the model.** Admission increments the durable request counter immediately before dispatch. Upstream errors, transport failures, client cancellations and retries count. Local validation failures, exhausted-key rejections, model discovery and introspection do not count. A human turn can cause multiple model calls through tools or client retries. The gateway itself never retries generation.
- **Soft token budget:** a request is admitted only while recorded usage is below the threshold. Its entire response can finish, even if it exceeds the budget. Subsequent requests are blocked. Exactly one generation may run per virtual key. Different keys can run concurrently; there is a shared gateway capacity limit.
- **Token accounting:** provider `usage.total_tokens`, or prompt + completion tokens, is used when a complete response is available. Reasoning/cache breakdowns are not added again. Without reliable final usage, including interrupted SSE, charge the normalized request's UTF-8 byte length plus permitted output tokens, mark the request as estimated, and release the slot only after this is persisted. This deliberately overestimates typical text and is not a tokenizer or a billing guarantee.
- **Explicit upstream HTTP rejection:** consumes one attempt and zero generation tokens. Ambiguous transport failures use the conservative estimate.
- **TTL, revocation, changes:** affect new admissions. A request already admitted may finish with the settings and upstream credential it started with. They do not undo provider charges.
- **Crash recovery:** pending requests are settled using their stored estimate on startup. A file lock permits a single process per SQLite database. Failed settlement makes readiness fail and stops further generation until recovery/restart.
- **Caps are per virtual key.** There is no combined budget for all virtual keys on one connection in this version. A copied virtual key can be used for other prompts until its limits are reached. Goal enforcement and server-side prompts are not implemented.

### Errors

Errors have the form `{"error":{"type":"tabsgate_error","code":"...","message":"..."}}`.

| HTTP | Typical codes | Client behavior |
| --- | --- | --- |
| 400 / 413 / 415 | `invalid_request`, `invalid_json`, `body_too_large`, `invalid_content_type` | Correct the request |
| 401 | `invalid_credentials`, `invalid_key` | Supply valid credentials |
| 403 | `key_expired`, `key_revoked`, `request_limit_reached`, `token_budget_exhausted`, `model_not_allowed` | Stop; show the reason |
| 409 | `key_busy`, `configuration_changed`, `connection_exists` | Wait/refresh; never start parallel calls on the same key |
| 429 | `rate_limited` | Respect `Retry-After` |
| 502 | `upstream_error`, `upstream_unavailable`, `invalid_upstream_response` | Attempt may already be counted; no automatic gateway retry |
| 503 | `busy`, `not_ready`, `accounting_unavailable`, `capacity_reached` | Retry only after recovery |

After SSE headers are sent, failures arrive as a JSON `error` object in the stream. The HTTP status can no longer change. Treat them as errors even if a final `[DONE]` follows. Error text from providers is redacted rather than reflected to clients.

## Providers and configuration

Defaults: OpenAI, DeepSeek, OpenRouter, Groq, Mistral, Together AI and xAI,
using their Chat Completions endpoints. Provider compatibility is verified
against local protocol fixtures; it has **not** been tested using paid live
credentials. A provider/model must honor its configured output limit field and
report compatible usage. Models with other protocol requirements are outside
this version.

| Provider | Base URL | Output limit |
| --- | --- | --- |
| OpenAI | `https://api.openai.com/v1` | `max_completion_tokens` |
| DeepSeek | `https://api.deepseek.com/v1` | `max_tokens` |
| OpenRouter | `https://openrouter.ai/api/v1` | `max_tokens` |
| Groq | `https://api.groq.com/openai/v1` | `max_completion_tokens` |
| Mistral | `https://api.mistral.ai/v1` | `max_tokens` |
| Together AI | `https://api.together.ai/v1` | `max_tokens` |
| xAI | `https://api.x.ai/v1` | `max_tokens` |

The Mistral and Together AI profiles omit the optional `stream_options` field;
usage from their streaming events is still accounted for. Other profiles request
stream usage explicitly. Custom profiles can opt into the same behavior with
`omit_stream_options: true`. Missing final usage always uses the conservative
accounting fallback described above.

Provider references: [Groq compatibility](https://console.groq.com/docs/openai),
[Groq parameters](https://console.groq.com/docs/api-reference),
[Mistral Chat](https://docs.mistral.ai/api/endpoint/chat),
[Together compatibility](https://docs.together.ai/docs/inference/openai-compatibility),
[Together Chat](https://docs.together.ai/reference/chat-completions),
[xAI Chat](https://docs.x.ai/developers/rest-api-reference/inference/chat-completions).

`providers.example.json` documents the operator-owned catalog. Mount a file and set `TABSGATE_PROVIDERS_FILE` to replace the default list. These URLs are trusted deployment configuration, never user-submitted destinations. Redirects are not followed and private network addresses are blocked at dial time. No incoming headers are forwarded wholesale: upstream authorization is rebuilt from the encrypted credential.

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `TABSGATE_MASTER_KEY_FILE` | Required unless direct key supplied | File containing base64-encoded 32-byte master key |
| `TABSGATE_MASTER_KEY` | Alternative | Direct base64 key; mutually exclusive with the file |
| `TABSGATE_DB_PATH` | `data/tabsgate.db`, `/data/tabsgate.db` in image | SQLite database |
| `TABSGATE_ADDR` | `:8080` | Listen address |
| `TABSGATE_PROVIDERS_FILE` | Built-in catalog | Operator provider configuration |
| `TABSGATE_CORS_ORIGINS` | `*` | Comma-separated origins; wildcard supports hosted and standalone Tabs Chat |
| `TABSGATE_TRUSTED_PROXIES` | Empty | Explicit CIDRs allowed to supply `X-Forwarded-For` |
| `TABSGATE_REQUEST_TIMEOUT` | `2m` | Entire provider request deadline; 1s–10m |
| `TABSGATE_ALLOW_PRIVATE_UPSTREAMS` | `false` | Opt-in for operator-controlled local HTTP/private providers; intended for tests/self-hosting |

Rate limits are in-memory safeguards, not usage quotas: 5 requests/s per client IP with burst 60, management 0.5/s per IP with burst 10 plus a global 10/s burst 20, virtual key access 2/s with burst 30, model discovery 0.2/s per connection with burst 5. Do not trust forwarding headers from arbitrary clients. Behind a reverse proxy, configure its actual CIDR or clients will share its IP bucket. Usage quotas themselves are always persistent.

Request bodies are limited to 1 MiB, management bodies to 16/32 KiB, upstream responses to 4 MiB and individual SSE events to 1 MiB. At most 16 upstream operations run concurrently across the gateway. Only one completion (`n=1`), text content and client-executed function tools are supported. Images, audio, built-in paid tools, provider routing overrides, fallback model arrays, Responses API, embeddings and MCP proxying are not supported. Unknown top-level generation fields are rejected. The gateway requests stream usage where supported, accounts for automatically reported usage, and bounds both output-limit aliases.

## Storage and operation

Provider keys are encrypted with AES-256-GCM and connection-bound associated data. Key lookup uses a domain-separated HMAC. Virtual keys have 256 random bits and are stored as SHA-256 digests; keywords use salted Argon2id. The database stores counters and request metadata, never conversation contents. The service sees plaintext credentials and prompts in memory while forwarding, so it must be trusted by its users.

SQL migrations are embedded and applied transactionally. SQLite uses WAL, foreign keys, full synchronization and secure deletion. Keep the database on a local persistent volume; do not share it between hosts or replicas. Each network request runs outside database transactions.

Deleting a connection removes active records and prevents future use of its virtual keys. It does not revoke the credential with the provider, erase provider-side records, cancel already admitted calls or instantly scrub older WAL pages and backups. Retention and backup deletion remain deployment responsibilities. Use SQLite's backup facilities or stop the service before copying the complete database; copying only a live `.db` file can omit WAL data. Retain the matching master key separately. Never restore an old backup over a running service: it can restore revoked credentials and older quotas. Review/revoke restored keys before exposing a restored instance.

The `requests` metadata ledger is currently retained until connection deletion. Monitor database size and available disk space. Application logs contain operational events, not request bodies, credentials or provider error bodies. Configure the external reverse proxy with the same redaction policy. Logs on startup may include configuration/file paths, not secrets.

### Instance usage

The management UI shows usage for each virtual key. For an instance-wide
summary, an operator with SQLite access can run the following read-only query
against the instance database or a consistent backup:

```sql
SELECT COUNT(DISTINCT k.connection_id) AS active_connections,
       COUNT(DISTINCT r.key_id) AS used_virtual_keys,
       COUNT(*) AS admitted_calls,
       COUNT(CASE WHEN r.state = 'completed' THEN 1 END) AS completed_calls,
       COALESCE(SUM(CASE WHEN r.estimated = 0 THEN r.tokens ELSE 0 END), 0) AS reported_tokens,
       COALESCE(SUM(CASE WHEN r.estimated = 1 THEN r.tokens ELSE 0 END), 0) AS estimated_tokens
FROM requests AS r
JOIN virtual_keys AS k ON k.id = r.key_id
WHERE r.started_at >= unixepoch('now', '-7 days');
```

This counts connections that made at least one admitted call in the past seven
days. One person may manage several connections or share a virtual key. Deleting
a connection also removes its history from this report. Usage stays on the
installation that processed the requests.

## Development and verification

Go 1.27+, no CGO required:

```sh
go mod download
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/tabsgate ./cmd/tabsgate
docker build --target test -t tabsgate:test .
docker build -t tabsgate:local .
```

Tests use temporary databases and local HTTP provider fixtures. They cover credentials and ownership, request/token/TTL limits, concurrent admission, SSE delivery, missing usage, cancellations, restart recovery, model filtering, header isolation, redirects and CORS. No real provider key is needed. The container smoke test is `python3 scripts/container_smoke.py`; it builds no images, uses `tabsgate:local`, creates isolated Docker resources and removes them at completion.

Optional UI integration test: `node scripts/ui_smoke.mjs` (requires Go,
Playwright and Chromium; see [`internal/webui/README.md`](internal/webui/README.md)
for executable overrides). It starts an isolated real gateway with a local fake
provider and checks the complete browser flow, usage, editing, revocation,
one-time keys, interrupted responses, mobile layout and absence of browser
credential persistence. No live model calls are made.

Protocol references: [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [DeepSeek](https://api-docs.deepseek.com/), [OpenRouter Chat Completions](https://openrouter.ai/docs/api/api-reference/chat/send-chat-completion-request), [SQLite WAL](https://sqlite.org/wal.html).
