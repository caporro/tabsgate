# Management UI

The UI lives entirely in this package. `handler.go` embeds the static assets;
the command entry point composes this handler with the independent
`internal/gate` API. The API package does not import or know about the UI.

- `assets/index.html`: page shell, no inline scripts or styles.
- `assets/app.css`: responsive layout, light/dark appearance and controls.
- `assets/app.js`: views, form validation and interaction state.
- `assets/api.js`: the only HTTP adapter; uses the existing gateway endpoints.
- `assets/confirm.js`: native modal confirmations that preserve the current view.
- `assets/logo.png`: the original, unmodified TabsGate logo supplied by the owner.
- `assets/icon.svg`: the compact brand symbol for the browser favicon.

The header keeps the logo on its original midnight background in both themes.
Cyan, blue and violet accents, primary buttons, keyboard focus and usage meters
follow the brand. Primary button colors are darker than the logo highlights to
keep white labels readable. Theme colors are defined in `assets/app.css`.

No Node runtime, frontend build, external fonts, CDN, cookies, account system,
browser storage or third-party service is required. Provider credentials and a
newly issued virtual key stay in page memory only. Signing out,
reloading or leaving the page clears them. A new key is copied from its creation
response; key lists contain only metadata and prefixes.

All UI copy and formatting is English. Sign out asks for confirmation; cancelling
or pressing Escape keeps the current screen and unsaved form values. Connection
settings use the existing PATCH endpoint to replace the provider API key and/or
pass without resetting virtual keys, limits, expiry or usage. After a successful
update the HTTP adapter uses the new credentials. Deletion requires a separate
confirmation and uses the existing cascading DELETE endpoint. It removes only
that connection and its virtual keys and usage, not the provider's original key
or the instance encryption key.

Models are discovered from the selected connection on first opening the key
form. An ID can also be entered manually if discovery is unavailable. Extra
models and the output cap are under Options. Edits keep the original expiry
unless explicitly changed and do not reset usage. Refresh reads current usage
from the API. Mutations are never automatically retried; an ambiguous create
requires checking the list before creating another key.

The page and assets are served at `/` and `/assets/*`. Only these exact static
paths are handled here; all other requests go to the original API handler.
The Docker binary includes the UI assets through Go embed.

Run the Go suite with `go test -race ./...`. The optional browser smoke test is
`node scripts/ui_smoke.mjs` from the project root. It builds an isolated fixture
against the real gateway API and a local fake provider; it never uses live
credentials or paid requests. It needs Go, Playwright and Chromium. Environment
overrides: `TABSGATE_GO_BIN`, `TABSGATE_PLAYWRIGHT_MODULE`,
`TABSGATE_CHROME_EXECUTABLE`, `TABSGATE_UI_SCREENSHOTS`.

The browser checks include sign-out cancellation with an unsaved draft,
credential rotation, continued virtual-key usage, confirmed connection deletion,
isolation of another connection, and the 320px mobile layout.
