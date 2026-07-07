// Package lspbridge bridges a WebSocket carrying JSON-RPC LSP messages
// to a language-server subprocess's stdio. Each LSP server gets a
// per-connection process (texlab / gopls / pyright / typescript-
// language-server / rust-analyzer / …) keyed by the language id.
//
// Wire shape on the WS :
//
//	server → client : one JSON-RPC object per WS text frame
//	client → server : one JSON-RPC object per WS text frame
//
// The bridge strips + adds the Content-Length framing on the stdio
// side so the WS payload is the bare JSON, matching what the standard
// CodeMirror LSP clients expect (e.g. `codemirror-languageserver`,
// `@open-rpc/codemirror-lsp`).
//
// Binary resolution : a language id resolves to a binary via the
// [Servers] registry. Each entry is configurable via an environment
// variable so operators can override the binary path per-deployment
// without rebuilding the image. [DefaultServers] uses a neutral,
// project-agnostic prefix :
//
//	LSPBRIDGE_TEXLAB        — texlab binary path (default: "texlab")
//	LSPBRIDGE_GOPLS         — gopls binary path  (default: "gopls")
//	LSPBRIDGE_PYRIGHT       — pyright-langserver (default: "pyright-langserver")
//	LSPBRIDGE_TS            — typescript-language-server (default: "typescript-language-server")
//	LSPBRIDGE_RUSTANALYZER  — rust-analyzer      (default: "rust-analyzer")
//
// When the binary isn't on $PATH the bridge writes a JSON-RPC error
// response back to the client + closes the WS so the editor can
// surface a single visible failure instead of silently dropping
// completions.
//
// The registry is a plain map : callers may pass their own by
// replacing [Servers] (e.g. `lspbridge.Servers = myMap`) or extend the
// defaults in place (`lspbridge.Servers["zig"] = …`). Adding a new
// language is a one-line change ; the handler dispatches by the URL
// path's lang parameter.
package lspbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

// LanguageServer describes how to launch one LSP for a given language.
type LanguageServer struct {
	Lang   string   // canonical key passed via URL path : "latex" / "go" / …
	Binary string   // default executable name
	Args   []string // additional CLI arguments
	// EnvOverride : if non-empty + the env var is set (and points at an
	// existing file), use that path instead of Binary. Lets ops override
	// per-deployment without re-building the image.
	EnvOverride string
}

// Concurrency caps for LSP subprocess spawns. Two layers : a global
// process-wide ceiling (lspSem) so the host can't be saturated, and a
// per-subject ceiling (lspPerUser) so a single editor with a runaway
// reconnect loop can't starve everyone else. The numbers are
// deliberate compromises — moderate-sized teams stay under the global
// cap, individual users can run 4 LSPs (one per open language) before
// hitting the per-user ceiling.
const (
	lspGlobalMax  = 16
	lspPerUserMax = 4
)

var (
	lspSem        = make(chan struct{}, lspGlobalMax)
	lspPerUserMu  sync.Mutex
	lspPerUserCnt = map[string]int{}
)

// subjectCtxKey is the request-context key under which the caller
// stashes the authenticated subject before invoking HandleWS. Using an
// unexported empty struct as the key is type-system safe + carries zero
// collision risk with other packages' context values.
type subjectCtxKey struct{}

// WithSubject returns a child context tagged with the per-user counter
// key. Call this in the HTTP handler before HandleWS so the per-user
// concurrency cap buckets by the authenticated identity rather than by
// raw token (which would double the cap during rotation overlap or
// multi-device use).
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectCtxKey{}, subject)
}

// lspSubjectFromRequest extracts the per-user counter key. Reads from
// the context-stashed subject set by WithSubject ; falls back to "anon"
// so an unauthenticated / dev-mode request still bucketises.
func lspSubjectFromRequest(r *http.Request) string {
	if v, ok := r.Context().Value(subjectCtxKey{}).(string); ok && v != "" {
		return v
	}
	return "anon"
}

// lspAcquire reserves a slot in both the global and per-user
// semaphores. Returns release closures the caller defers. err =
// 429-friendly message when either ceiling is hit.
func lspAcquire(subject string) (func(), func(), error) {
	select {
	case lspSem <- struct{}{}:
	default:
		return func() {}, func() {}, errors.New("lspbridge: server is busy (too many concurrent sessions)")
	}
	releaseGlobal := func() { <-lspSem }

	lspPerUserMu.Lock()
	if lspPerUserCnt[subject] >= lspPerUserMax {
		lspPerUserMu.Unlock()
		releaseGlobal()
		return func() {}, func() {}, errors.New("lspbridge: too many concurrent sessions for this user")
	}
	lspPerUserCnt[subject]++
	lspPerUserMu.Unlock()
	releasePerUser := func() {
		lspPerUserMu.Lock()
		defer lspPerUserMu.Unlock()
		if lspPerUserCnt[subject] > 0 {
			lspPerUserCnt[subject]--
		}
		if lspPerUserCnt[subject] == 0 {
			delete(lspPerUserCnt, subject)
		}
	}
	return releaseGlobal, releasePerUser, nil
}

// DefaultServers returns a fresh registry of the common LSP launchers,
// keyed by canonical language id. Each entry's EnvOverride uses the
// neutral LSPBRIDGE_* prefix so nothing about this catalogue is tied to
// a particular host application. Callers can use it as-is, extend it,
// or ignore it and supply their own map via [Servers].
func DefaultServers() map[string]LanguageServer {
	return map[string]LanguageServer{
		"latex":      {Lang: "latex", Binary: "texlab", EnvOverride: "LSPBRIDGE_TEXLAB"},
		"go":         {Lang: "go", Binary: "gopls", EnvOverride: "LSPBRIDGE_GOPLS", Args: []string{"serve"}},
		"python":     {Lang: "python", Binary: "pyright-langserver", EnvOverride: "LSPBRIDGE_PYRIGHT", Args: []string{"--stdio"}},
		"typescript": {Lang: "typescript", Binary: "typescript-language-server", EnvOverride: "LSPBRIDGE_TS", Args: []string{"--stdio"}},
		"javascript": {Lang: "javascript", Binary: "typescript-language-server", EnvOverride: "LSPBRIDGE_TS", Args: []string{"--stdio"}},
		"rust":       {Lang: "rust", Binary: "rust-analyzer", EnvOverride: "LSPBRIDGE_RUSTANALYZER"},
		// "fake" : deterministic LSP stub bundled as cmd/fake-lsp ; only
		// wired through when LSPBRIDGE_FAKE points at a built binary.
		// Used by the integration test so the full WS→subprocess→WS
		// round-trip can run without depending on texlab / gopls being
		// installed on the host.
		"fake": {Lang: "fake", Binary: "fake-lsp-not-on-path", EnvOverride: "LSPBRIDGE_FAKE"},
	}
}

// Servers is the registry the exported handlers dispatch through.
// Initialised to [DefaultServers]; callers may replace it wholesale or
// mutate it before serving.
var Servers = DefaultServers()

// resolveBinary returns the resolved executable path for a language or
// "" + error if no binary is on $PATH. Env override wins.
func (s LanguageServer) resolveBinary() (string, error) {
	if s.EnvOverride != "" {
		if v := os.Getenv(s.EnvOverride); v != "" {
			if _, err := os.Stat(v); err == nil {
				return v, nil
			}
		}
	}
	bin, err := exec.LookPath(s.Binary)
	if err != nil {
		return "", err
	}
	return bin, nil
}

// Seams for fault injection in tests : default to the real syscalls,
// overridden to force the otherwise-unreachable error branches.
var (
	acquireFn     = lspAcquire
	cmdStdinPipe  = (*exec.Cmd).StdinPipe
	cmdStdoutPipe = (*exec.Cmd).StdoutPipe
	cmdStart      = (*exec.Cmd).Start
)

// wsConn is the slice of *websocket.Conn the bridge needs. Narrowing to
// an interface lets the framing pumps be unit-tested against an
// in-memory fake without a real socket.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
}

// HandleWS upgrades the HTTP request to a WebSocket + spawns the
// per-language LSP. Bidirectional pipe runs until either side closes ;
// the subprocess is killed on WS close.
//
// langKey : the URL path param ("latex", "go", …) extracted by the
// caller's mux. Returning quickly on unknown languages keeps the
// editor's error path clean.
//
// acceptOpts : the caller passes the same websocket.AcceptOptions it
// uses for the rest of its WS endpoints so the Origin allowlist stays
// uniform across endpoints. nil = use a permissive default (dev mode).
func HandleWS(w http.ResponseWriter, r *http.Request, langKey string, logger *slog.Logger, acceptOpts *websocket.AcceptOptions) {
	// Per-user + global concurrency caps : reject early so a runaway
	// editor / malicious client can't fork-bomb the host with LSP
	// subprocesses. Subject keys the per-user counter — empty fallback
	// covers dev mode where no identity is injected.
	subject := lspSubjectFromRequest(r)
	releaseGlobal, releasePerUser, err := acquireFn(subject)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer releaseGlobal()
	defer releasePerUser()

	server, ok := Servers[langKey]
	if !ok {
		http.Error(w, "lspbridge: unknown language "+langKey, http.StatusNotFound)
		return
	}
	bin, err := server.resolveBinary()
	if err != nil {
		http.Error(w, "lspbridge: server binary "+server.Binary+" not installed : "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	opts := acceptOpts
	if opts == nil {
		opts = &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	// LSP messages can be larger than the default ; gopls initialize
	// responses easily run past 100 KB. Force compression off while
	// preserving the caller-provided origin policy.
	optsCopy := *opts
	optsCopy.CompressionMode = websocket.CompressionDisabled
	c, err := websocket.Accept(w, r, &optsCopy)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusInternalError, "internal error")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, server.Args...)
	stdin, err := cmdStdinPipe(cmd)
	if err != nil {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"lsp stdin pipe failed"}}`))
		return
	}
	stdout, err := cmdStdoutPipe(cmd)
	if err != nil {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"lsp stdout pipe failed"}}`))
		return
	}
	cmd.Stderr = os.Stderr // surface server stderr in the host logs
	if err := cmdStart(cmd); err != nil {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"lsp spawn failed"}}`))
		return
	}
	logger.Info("lsp.spawn", "lang", langKey, "bin", bin, "pid", cmd.Process.Pid)
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logger.Info("lsp.exit", "lang", langKey, "pid", cmd.Process.Pid)
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// server → client : read Content-Length-framed JSON messages from
	// the subprocess + forward each as one WS text frame.
	go func() {
		defer wg.Done()
		defer cancel()
		pumpServerToClient(ctx, c, stdout, logger)
	}()

	// client → server : pull a WS frame, prepend Content-Length, write
	// to stdin.
	go func() {
		defer wg.Done()
		defer cancel()
		pumpClientToServer(ctx, c, stdin, logger)
	}()

	wg.Wait()
	c.Close(websocket.StatusNormalClosure, "")
}

// pumpServerToClient reads Content-Length-framed JSON messages from the
// subprocess stdout + writes each bare payload as one WS text frame. It
// returns on EOF, a framing/body read error, or a WS write error.
func pumpServerToClient(ctx context.Context, c wsConn, stdout io.Reader, logger *slog.Logger) {
	br := bufio.NewReaderSize(stdout, 65536)
	for {
		length, err := readContentLength(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Info("lsp.read.framing.err", "err", err)
			}
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			logger.Info("lsp.read.body.err", "err", err)
			return
		}
		if err := c.Write(ctx, websocket.MessageText, buf); err != nil {
			logger.Info("lsp.ws.write.err", "err", err)
			return
		}
	}
}

// pumpClientToServer pulls WS text frames + writes each as a
// Content-Length-framed message to the subprocess stdin. Non-text
// frames and empty payloads are skipped. It returns on a WS read error
// or a stdin write error.
func pumpClientToServer(ctx context.Context, c wsConn, stdin io.Writer, logger *slog.Logger) {
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if len(payload) == 0 {
			continue
		}
		frame := []byte("Content-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n")
		if _, err := stdin.Write(frame); err != nil {
			logger.Info("lsp.stdin.frame.err", "err", err)
			return
		}
		if _, err := stdin.Write(payload); err != nil {
			logger.Info("lsp.stdin.body.err", "err", err)
			return
		}
	}
}

// readContentLength scans the LSP framing header. Each message is :
//
//	Content-Length: <N>\r\n
//	(optional Content-Type: …\r\n)
//	\r\n
//	<JSON bytes>
//
// We accept any header line + only act on Content-Length.
func readContentLength(br *bufio.Reader) (int, error) {
	length := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		if line == "\r\n" || line == "\n" {
			if length == 0 {
				return 0, fmt.Errorf("lspbridge framing : missing Content-Length")
			}
			return length, nil
		}
		// case-insensitive header parse
		if len(line) > len("Content-Length:") {
			head := line[:len("Content-Length:")]
			if head == "Content-Length:" || head == "content-length:" {
				v := line[len("Content-Length:"):]
				// strip whitespace + CRLF
				start := 0
				for start < len(v) && (v[start] == ' ' || v[start] == '\t') {
					start++
				}
				end := len(v)
				for end > start && (v[end-1] == ' ' || v[end-1] == '\t' || v[end-1] == '\r' || v[end-1] == '\n') {
					end--
				}
				n, err := strconv.Atoi(v[start:end])
				if err != nil {
					return 0, fmt.Errorf("lspbridge framing : bad Content-Length %q", v[start:end])
				}
				length = n
			}
		}
	}
}

// AvailableLanguages returns the set of language ids whose binary is
// actually resolvable on the host. A client uses this for progressive
// enablement : connecting to the bridge when the host advertises
// support, and falling back to a simpler autocomplete otherwise.
func AvailableLanguages() []string {
	out := make([]string, 0, len(Servers))
	for k, s := range Servers {
		if _, err := s.resolveBinary(); err == nil {
			out = append(out, k)
		}
	}
	// caller doesn't care about ordering ; sort for stable output.
	sortStrings(out)
	return out
}

// sortStrings sorts in place with a dependency-free insertion sort
// (the slice is tiny : one entry per configured language).
func sortStrings(out []string) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
}

// EncodeError writes a JSON-RPC error response. Used by a handler when
// the bridge can't be set up before the subprocess even starts.
func EncodeError(id any, code int, msg string) []byte {
	type errObj struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	type resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   errObj `json:"error"`
	}
	b, _ := json.Marshal(resp{JSONRPC: "2.0", ID: id, Error: errObj{Code: code, Message: msg}})
	return b
}
