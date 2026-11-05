# First-version verification

Verified locally on 2026-10-29. These results describe this implementation, not live provider compatibility.

- Go 1.27.3; SQLite driver `modernc.org/sqlite` v1.61.0.
- Integration suite with Go race detector: passed. Coverage of `internal/gate`: 70.5% at the measured run. The command entry point is also exercised by the container smoke test.
- `go vet ./...`: passed.
- Linux tests and vet in the Docker `test` stage: passed.
- Runtime image built and exercised on Linux ARM64, as UID 65532, with read-only root and a persistent SQLite volume.
- Container smoke: model filtering, soft token threshold, SSE, hard request cap, revocation, connection deletion, readiness and persistence across container restart passed. Its isolated container and data volume were removed after the run.
- `docker compose config --quiet`: passed.
- `govulncheck`: no vulnerable symbols or packages used by this application. The scan reports GO-2026-5932 for the unused `golang.org/x/crypto/openpgp` package in the broader crypto module. Tabs Gate imports Argon2id, not OpenPGP; there is no applicable fixed version for that advisory.

No real provider credentials were supplied, no paid AI requests were made, and no public deployment was performed. Protocol behavior is covered by local fake providers; model-specific compatibility still requires an authorized live check.

Reproduce with the commands in the README and `python3 scripts/container_smoke.py` after building the runtime image.

## Built-in management UI

Verified locally on 2026-10-29 after adding the UI:

- UI assets, browser state and HTTP adapter are in `internal/webui`; the command
  entry point composes its handler with `internal/gate`. API implementation and
  quota/storage behavior were not changed.
- `go test -race ./...`, `go vet ./...`, and native static Go build: passed.
- Linux Docker test stage and Linux ARM64 runtime image build: passed. An
  isolated, read-only, non-root runtime container served the embedded page,
  JavaScript and CSS with the expected headers; API management still required
  key and pass. The container and its temporary data were removed.
- `scripts/ui_smoke.mjs`: passed in isolated Chrome against the real gateway
  API and a local fake provider. Checks covered opening a connection, provider
  model discovery, creation, actual usage after a simulated completion,
  edits preserving usage and expiry, multiple model allowlists, revocation,
  incorrect credentials, and a UTF-8 pass.
- Browser checks also covered manual model IDs after discovery failure, an
  interrupted creation response without automatic retry, key visibility only
  at creation, clearing on connection change/reload, and empty cookies,
  localStorage and sessionStorage.
- Desktop and 320px mobile layouts, light/dark appearance, and safe rendering
  of HTML-like key names: passed. No page errors or CSP violations were observed.
- UI uses embedded local assets only and needs no frontend build or Node
  runtime. Node and Playwright are optional development test tools.

No real provider credentials, paid model calls or public deployment were used.
The browser smoke fixture removes its database and executable after the run.

## English UI, connection management and additional providers

Verified locally on 2026-10-30:

- All UI labels, messages and formatting use English. Sign out uses a native
  confirmation dialog; Cancel and Escape preserve the open form and its draft.
- The existing connection PATCH endpoint changes the provider key and pass.
  Browser checks verified both changes separately, rejected the old credentials,
  and kept existing virtual keys working with their usage preserved.
- Connection deletion uses a separate destructive confirmation and the existing
  cascading DELETE endpoint. The browser fixture verified that the deleted
  connection and virtual key stop working while another connection remains
  accessible. Cancellation keeps the settings form open.
- Groq, Mistral, Together AI and xAI profiles were exercised through registration,
  model discovery, normal chat and streaming against local simulated providers.
  Checks covered credential isolation, endpoint paths, output limits,
  provider-specific stream options and actual token accounting.
- `go test -race ./...`, `go vet ./...`, JavaScript syntax checks and the Linux
  Docker test stage passed. The Linux ARM64 runtime image built successfully.
- The extended `scripts/ui_smoke.mjs` passed, including the existing key creation,
  edit, revoke, expiry, model discovery failure, ambiguous response, one-time
  secret, no browser persistence and HTML escaping checks. Desktop, 320px mobile
  and light/dark checks passed with no browser page errors.
- The local Compose service at `http://127.0.0.1:8080/` was rebuilt and restarted
  using the existing `tabsgate_gate_data` volume and `secrets/master_key` file.
  Readiness and container health passed. The live page loaded in English at
  390px, served the confirmation module and listed all seven providers.

Connection changes and deletion were tested only in isolated fixtures, never
against the user's local connections. No live provider credentials or paid AI
requests were used; real provider/model compatibility remains unverified.

## TabsGate brand

Verified locally on 2026-11-04:

- The header serves the owner's original PNG logo unchanged; its served bytes
  match the local asset. The embedded asset route and PNG content type passed
  the Go UI tests. The browser favicon uses the matching gradient brand symbol.
- Midnight surfaces and cyan, blue and violet accents are applied to both
  themes, primary buttons, keyboard focus, dialogs and usage meters. Default
  white button labels have at least 5.4:1 contrast at the gradient stops;
  secondary text colors were checked on the light and dark surfaces.
- Browser layout checks passed at 320, 390, 480, 600, 640 and 960px in both
  themes, with no header overlap or horizontal page overflow. Fresh touch
  contexts at 320 and 390px also passed; the 320px list keeps its title on one
  line and provides a full-width New key button with a 44px minimum target.
- The existing isolated UI smoke suite passed, including key management,
  connection rotation/deletion, sign-out cancellation and no browser storage.
  No page errors or CSP violations were observed.
- Linux Docker tests and vet passed. The runtime image was rebuilt and the
  existing local Compose service updated; readiness and container health
  passed. Existing data and encryption key mounts were retained.

Visual checks of the running service used intercepted example responses for
connection screens. They did not create or change any live connections.
