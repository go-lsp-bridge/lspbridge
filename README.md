<p align="center"><img src="https://raw.githubusercontent.com/go-lsp-bridge/brand/main/social/go-lsp-bridge-lspbridge.png" alt="go-lsp-bridge/lspbridge" width="720"></p>

# lspbridge — go-lsp-bridge

[![Docs](https://img.shields.io/badge/docs-mkdocs--material-4F46E5)](https://go-lsp-bridge.github.io/docs/)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26.4%2B-00ADD8)](https://go.dev/dl/)
[![Coverage](https://img.shields.io/badge/coverage-100%25-1a7f37)](#tests--coverage)

**A pure-Go (no cgo) bridge that relays JSON-RPC LSP traffic between a
WebSocket and a language-server subprocess's stdio.** It adds and strips the
`Content-Length` framing on the stdio side so the WebSocket payload is the bare
JSON object, exactly what browser LSP clients such as
[`codemirror-languageserver`](https://github.com/FurqanSoftware/codemirror-languageserver)
and `@open-rpc/codemirror-lsp` expect — dropping a real language server
(texlab, gopls, pyright, typescript-language-server, rust-analyzer, …) behind a
web editor with a single HTTP handler.

The only dependency is [`github.com/coder/websocket`](https://github.com/coder/websocket).

## Wire shape

```
server → client : one JSON-RPC object per WS text frame
client → server : one JSON-RPC object per WS text frame
```

The bridge owns the stdio `Content-Length` framing in both directions; the
WebSocket carries bare JSON, so a CodeMirror LSP client connects with no
adapter.

## Features

- **Per-connection subprocess.** Each WebSocket spawns its own language server,
  killed when the socket closes.
- **Two-layer concurrency caps.** A process-wide ceiling keeps the host from
  being saturated; a per-subject ceiling stops one runaway editor (reconnect
  loop) from starving everyone else. Tag the request with `WithSubject` and the
  per-user counter buckets by authenticated identity rather than raw token.
- **Configurable registry.** `DefaultServers()` ships launchers for latex / go /
  python / typescript / javascript / rust, each overridable per-deployment via a
  neutral `LSPBRIDGE_*` environment variable. Bring your own map, or extend the
  defaults in place — adding a language is one line.
- **Visible failures.** A missing binary or a failed spawn sends a JSON-RPC
  error frame back to the editor instead of silently dropping completions.
- **Progressive enablement.** `AvailableLanguages()` reports which servers are
  actually resolvable on the host so a client can light up LSP only where it
  works.

CGO-free, **100% test coverage** (including every error branch), `gofmt` +
`go vet` clean, and green across the six 64-bit Go targets (amd64, arm64,
riscv64, loong64, ppc64le, s390x).

## Install

```sh
go get github.com/go-lsp-bridge/lspbridge
```

## Usage

Wire `HandleWS` into any `net/http` mux. The `langKey` is whatever your route
extracts (path segment, query param, …):

```go
package main

import (
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-lsp-bridge/lspbridge"
)

func main() {
	logger := slog.Default()
	accept := &websocket.AcceptOptions{OriginPatterns: []string{"editor.example.com"}}

	http.HandleFunc("/lsp/", func(w http.ResponseWriter, r *http.Request) {
		lang := r.PathValue("lang") // e.g. registered via /lsp/{lang}
		// Tag the request so the per-user concurrency cap keys by identity.
		r = r.WithContext(lspbridge.WithSubject(r.Context(), subjectOf(r)))
		lspbridge.HandleWS(w, r, lang, logger, accept)
	})

	// Advertise only the servers actually installed on this host.
	_ = lspbridge.AvailableLanguages() // ["go", "latex", ...]

	http.ListenAndServe(":8080", nil)
}

func subjectOf(r *http.Request) string { return "anon" }
```

Override a server binary without rebuilding:

```sh
LSPBRIDGE_GOPLS=/opt/toolchains/go/bin/gopls \
LSPBRIDGE_TEXLAB=/usr/local/bin/texlab \
  ./your-server
```

Or supply an entirely custom registry:

```go
lspbridge.Servers = map[string]lspbridge.LanguageServer{
	"zig": {Lang: "zig", Binary: "zls", EnvOverride: "LSPBRIDGE_ZLS"},
}
```

## API

```go
// HandleWS upgrades r to a WebSocket, spawns the language server for langKey,
// and relays JSON-RPC in both directions until either side closes.
func HandleWS(w http.ResponseWriter, r *http.Request, langKey string,
	logger *slog.Logger, acceptOpts *websocket.AcceptOptions)

// WithSubject tags ctx so the per-user concurrency cap buckets by identity.
func WithSubject(ctx context.Context, subject string) context.Context

// DefaultServers returns a fresh registry with neutral LSPBRIDGE_* overrides.
func DefaultServers() map[string]LanguageServer

// Servers is the registry HandleWS/AvailableLanguages dispatch through;
// initialised to DefaultServers(), replaceable or extensible by the caller.
var Servers map[string]LanguageServer

// AvailableLanguages returns the language ids whose binary resolves on $PATH.
func AvailableLanguages() []string

// EncodeError marshals a JSON-RPC error response.
func EncodeError(id any, code int, msg string) []byte

type LanguageServer struct {
	Lang        string   // canonical key ("latex", "go", …)
	Binary      string   // default executable name
	Args        []string // extra CLI arguments
	EnvOverride string   // env var that, if set to an existing file, wins
}
```

## Tests & coverage

The suite exercises the full WS → subprocess → WS round-trip against a bundled
deterministic stub (`cmd/fake-lsp`), plus unit tests for the framing, registry
resolution, concurrency caps, and every pipe/spawn error branch — reaching
**100% statement coverage including error paths**.

```sh
COVERPKG=$(go list ./... | paste -sd, -)
go test -race -coverpkg="$COVERPKG" -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1   # 100.0%
```

The exec/WebSocket tests can't run under `qemu-user` (a host-built stub can't be
exec'd for a foreign arch, and there's no real loopback net), so the qemu
cross-arch CI lanes set `LSPBRIDGE_NO_EXEC=1` to skip them while the pure
framing / registry / concurrency logic still runs and validates the code on all
six arches. The native ubuntu/macos lane runs the whole thing at 100%.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright the go-lsp-bridge/lspbridge authors.
