package lspbridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- readContentLength ------------------------------------------------

func TestReadContentLength(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"basic", "Content-Length: 42\r\n\r\n", 42, false},
		{"lowercase", "content-length: 7\r\n\r\n", 7, false},
		{"lf-blank", "Content-Length: 3\n\n", 3, false},
		{"with-extra-header", "Content-Type: application/json\r\nContent-Length: 9\r\n\r\n", 9, false},
		{"short-non-header-line", "X\r\n\r\n", 0, true},
		{"missing-length", "Content-Type: x\r\n\r\n", 0, true},
		{"bogus-length", "Content-Length: notanumber\r\n\r\n", 0, true},
		{"eof-mid-header", "Content-Length: 5", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.input))
			got, err := readContentLength(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// ---- registry / resolution -------------------------------------------

func TestDefaultServersCoversCoreLangs(t *testing.T) {
	d := DefaultServers()
	must := []string{"latex", "go", "python", "typescript", "javascript", "rust", "fake"}
	for _, lang := range must {
		if _, ok := d[lang]; !ok {
			t.Fatalf("DefaultServers missing %q", lang)
		}
	}
	// Neutral env prefix — no host-application leakage.
	for _, s := range d {
		if s.EnvOverride != "" && !strings.HasPrefix(s.EnvOverride, "LSPBRIDGE_") {
			t.Fatalf("non-neutral EnvOverride %q", s.EnvOverride)
		}
	}
	// Package-level Servers is initialised from DefaultServers.
	if _, ok := Servers["go"]; !ok {
		t.Fatalf("package Servers not initialised")
	}
}

func TestResolveBinary(t *testing.T) {
	// EnvOverride set + file exists → wins.
	f := writeExec(t, "server-a")
	t.Setenv("LSPBRIDGE_PROBE", f)
	got, err := LanguageServer{Binary: "no-such-xyz", EnvOverride: "LSPBRIDGE_PROBE"}.resolveBinary()
	if err != nil || got != f {
		t.Fatalf("env-override path: got %q err %v, want %q", got, err, f)
	}

	// EnvOverride set but file missing → fall through to LookPath (fails).
	t.Setenv("LSPBRIDGE_PROBE", tmpPath(t, "does-not-exist"))
	if _, err := (LanguageServer{Binary: "no-such-xyz-binary", EnvOverride: "LSPBRIDGE_PROBE"}).resolveBinary(); err == nil {
		t.Fatalf("expected LookPath failure for missing binary")
	}

	// EnvOverride empty + binary on PATH ("go" is present under setup-go).
	if _, err := (LanguageServer{Binary: "go"}).resolveBinary(); err != nil {
		t.Fatalf("expected to resolve go on PATH: %v", err)
	}

	// EnvOverride empty + binary absent → error.
	if _, err := (LanguageServer{Binary: "definitely-not-a-real-binary-xyz"}).resolveBinary(); err == nil {
		t.Fatalf("expected error for absent binary")
	}
}

func TestAvailableLanguages(t *testing.T) {
	// Make "fake" resolvable via its env override; the others aren't
	// installed on CI, so both the append + skip branches run.
	f := writeExec(t, "fake-bin")
	t.Setenv("LSPBRIDGE_FAKE", f)
	got := AvailableLanguages()
	found := false
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("AvailableLanguages not sorted: %v", got)
		}
	}
	for _, g := range got {
		if _, ok := Servers[g]; !ok {
			t.Fatalf("unknown lang %q", g)
		}
		if g == "fake" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected fake in resolvable set: %v", got)
	}
}

func TestSortStrings(t *testing.T) {
	in := []string{"c", "a", "b", "a"}
	sortStrings(in)
	if strings.Join(in, "") != "aabc" {
		t.Fatalf("sortStrings: %v", in)
	}
	sortStrings(nil) // no panic on empty
}

func TestEncodeError(t *testing.T) {
	s := string(EncodeError("init-1", -32000, "boom"))
	for _, want := range []string{`"id":"init-1"`, `"code":-32000`, `"message":"boom"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("payload missing %q: %s", want, s)
		}
	}
}

// ---- subject + concurrency caps --------------------------------------

func TestSubjectFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := lspSubjectFromRequest(r); got != "anon" {
		t.Fatalf("fallback: got %q", got)
	}
	r = r.WithContext(WithSubject(r.Context(), "alice"))
	if got := lspSubjectFromRequest(r); got != "alice" {
		t.Fatalf("subject: got %q", got)
	}
}

func TestAcquireCaps(t *testing.T) {
	// Per-user cap: acquire up to the ceiling, then reject; releasing
	// brings the count back down and deletes the map entry at zero.
	var releases []func()
	for i := 0; i < lspPerUserMax; i++ {
		rg, rp, err := lspAcquire("bob")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, rg, rp)
	}
	if _, _, err := lspAcquire("bob"); err == nil {
		t.Fatalf("expected per-user cap rejection")
	}
	for _, rel := range releases {
		rel()
	}
	lspPerUserMu.Lock()
	_, still := lspPerUserCnt["bob"]
	lspPerUserMu.Unlock()
	if still {
		t.Fatalf("expected bob's counter deleted at zero")
	}

	// Global cap: saturate lspSem, confirm rejection, then drain.
	for i := 0; i < lspGlobalMax; i++ {
		lspSem <- struct{}{}
	}
	if _, _, err := lspAcquire("carol"); err == nil {
		t.Fatalf("expected global cap rejection")
	}
	for i := 0; i < lspGlobalMax; i++ {
		<-lspSem
	}
}

// ---- pump functions (unit, no real socket) ---------------------------

type readResult struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

type fakeConn struct {
	reads    []readResult
	ri       int
	writeErr error
	written  [][]byte
}

func (f *fakeConn) Read(_ context.Context) (websocket.MessageType, []byte, error) {
	if f.ri >= len(f.reads) {
		return 0, nil, io.EOF
	}
	r := f.reads[f.ri]
	f.ri++
	return r.typ, r.data, r.err
}

func (f *fakeConn) Write(_ context.Context, _ websocket.MessageType, p []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, append([]byte(nil), p...))
	return nil
}

func frame(s string) string {
	return "Content-Length: " + itoa(len(s)) + "\r\n\r\n" + s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestPumpServerToClient(t *testing.T) {
	// Happy path: two framed messages forwarded, then EOF.
	c := &fakeConn{}
	pumpServerToClient(context.Background(), c, strings.NewReader(frame(`{"a":1}`)+frame(`{"b":2}`)), testLogger(t))
	if len(c.written) != 2 || string(c.written[0]) != `{"a":1}` {
		t.Fatalf("happy forward: %q", c.written)
	}

	// Framing error (non-EOF): bad Content-Length.
	pumpServerToClient(context.Background(), &fakeConn{}, strings.NewReader("Content-Length: bad\r\n\r\n"), testLogger(t))

	// Body read error: declared length exceeds available bytes.
	pumpServerToClient(context.Background(), &fakeConn{}, strings.NewReader("Content-Length: 10\r\n\r\nshort"), testLogger(t))

	// WS write error: aborts the loop.
	cw := &fakeConn{writeErr: errors.New("write boom")}
	pumpServerToClient(context.Background(), cw, strings.NewReader(frame(`{"a":1}`)), testLogger(t))
	if len(cw.written) != 0 {
		t.Fatalf("expected no writes recorded on error")
	}
}

// failWriter fails on its N-th Write call (1-indexed).
type failWriter struct {
	failAt int
	n      int
	buf    []byte
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n >= w.failAt {
		return 0, errors.New("stdin boom")
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func TestPumpClientToServer(t *testing.T) {
	// Happy path + non-text skip + empty skip + read-EOF return.
	stdin := &failWriter{failAt: 1 << 30}
	c := &fakeConn{reads: []readResult{
		{typ: websocket.MessageText, data: []byte(`{"x":1}`)},
		{typ: websocket.MessageBinary, data: []byte("ignored")},
		{typ: websocket.MessageText, data: []byte("")},
		// then implicit EOF
	}}
	pumpClientToServer(context.Background(), c, stdin, testLogger(t))
	if !strings.Contains(string(stdin.buf), `{"x":1}`) || !strings.Contains(string(stdin.buf), "Content-Length: 7") {
		t.Fatalf("stdin framing: %q", stdin.buf)
	}

	// Explicit read error (not EOF) also returns.
	cErr := &fakeConn{reads: []readResult{{err: errors.New("read boom")}}}
	pumpClientToServer(context.Background(), cErr, &failWriter{failAt: 1 << 30}, testLogger(t))

	// Header write error.
	c1 := &fakeConn{reads: []readResult{{typ: websocket.MessageText, data: []byte(`{"x":1}`)}}}
	pumpClientToServer(context.Background(), c1, &failWriter{failAt: 1}, testLogger(t))

	// Body write error (header ok, body fails).
	c2 := &fakeConn{reads: []readResult{{typ: websocket.MessageText, data: []byte(`{"x":1}`)}}}
	pumpClientToServer(context.Background(), c2, &failWriter{failAt: 2}, testLogger(t))
}

// ---- HandleWS error branches (no real socket) ------------------------

func TestHandleWSTooManyRequests(t *testing.T) {
	orig := acquireFn
	acquireFn = func(string) (func(), func(), error) {
		return func() {}, func() {}, errors.New("busy")
	}
	defer func() { acquireFn = orig }()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/lsp/fake", nil)
	HandleWS(w, r, "fake", testLogger(t), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", w.Code)
	}
}

func TestHandleWSUnknownLanguage(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/lsp/klingon", nil)
	HandleWS(w, r, "klingon-not-a-lang", testLogger(t), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleWSBinaryUnavailable(t *testing.T) {
	// Env override points nowhere and the default binary is absent →
	// resolveBinary fails → 503.
	t.Setenv("LSPBRIDGE_FAKE", tmpPath(t, "nope"))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/lsp/fake", nil)
	HandleWS(w, r, "fake", testLogger(t), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
}

func TestHandleWSAcceptFailure(t *testing.T) {
	// resolveBinary succeeds (env → real file) but the request isn't a
	// WebSocket upgrade, so websocket.Accept fails and HandleWS returns.
	f := writeExec(t, "accept-bin")
	t.Setenv("LSPBRIDGE_FAKE", f)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/lsp/fake", nil)
	HandleWS(w, r, "fake", testLogger(t), &websocket.AcceptOptions{InsecureSkipVerify: true})
	// Accept writes a 400-ish status; the key assertion is that we
	// returned without panicking and didn't spawn anything.
	if w.Code == http.StatusOK {
		t.Fatalf("expected a non-200 handshake failure, got %d", w.Code)
	}
}

// ---- HandleWS pipe/spawn failures (live WS, seam-injected) -----------

func TestHandleWSPipeAndSpawnFailures(t *testing.T) {
	requireLocalExec(t)
	f := writeExec(t, "seam-bin")

	cases := []struct {
		name    string
		set     func() func()
		wantMsg string
	}{
		{"stdin-pipe", func() func() {
			o := cmdStdinPipe
			cmdStdinPipe = func(*exec.Cmd) (io.WriteCloser, error) { return nil, errors.New("boom") }
			return func() { cmdStdinPipe = o }
		}, "lsp stdin pipe failed"},
		{"stdout-pipe", func() func() {
			o := cmdStdoutPipe
			cmdStdoutPipe = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errors.New("boom") }
			return func() { cmdStdoutPipe = o }
		}, "lsp stdout pipe failed"},
		{"spawn", func() func() {
			o := cmdStart
			cmdStart = func(*exec.Cmd) error { return errors.New("boom") }
			return func() { cmdStart = o }
		}, "lsp spawn failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LSPBRIDGE_FAKE", f)
			restore := tc.set()
			defer restore()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				HandleWS(w, r, "fake", testLogger(t), nil)
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close(websocket.StatusNormalClosure, "")
			_, raw, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !strings.Contains(string(raw), tc.wantMsg) {
				t.Fatalf("want %q in %s", tc.wantMsg, raw)
			}
		})
	}
}

// ---- test helpers -----------------------------------------------------

// writeExec writes an existing file whose path resolveBinary can stat.
func writeExec(t *testing.T, name string) string {
	t.Helper()
	p := tmpPath(t, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func tmpPath(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + string(os.PathSeparator) + name
}

// requireLocalExec skips tests that need a real subprocess / WebSocket.
// The qemu cross-arch CI lanes set LSPBRIDGE_NO_EXEC=1 because a
// host-built helper can't be exec'd under qemu-user and loopback net is
// unavailable; the framing/registry/concurrency logic still runs there.
func requireLocalExec(t *testing.T) {
	t.Helper()
	if os.Getenv("LSPBRIDGE_NO_EXEC") == "1" {
		t.Skip("exec/WebSocket tests disabled (LSPBRIDGE_NO_EXEC=1)")
	}
}
