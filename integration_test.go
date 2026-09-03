package lspbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestBridgeEndToEnd spawns the fake-lsp binary + drives it through the
// WS handler the way a CodeMirror SPA would. Confirms each method shape
// (initialize / completion / hover / definition / didOpen → publish-
// Diagnostics) survives the WS→stdio→WS round-trip. Skipped on the qemu
// cross-arch CI lanes (see requireLocalExec).
func TestBridgeEndToEnd(t *testing.T) {
	requireLocalExec(t)
	bin := buildFakeLSP(t)
	t.Setenv("LSPBRIDGE_FAKE", bin)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleWS(w, r, "fake", testLogger(t), nil)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	send := func(id int, method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("ws write %s: %v", method, err)
		}
	}
	notify := func(method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("ws notify %s: %v", method, err)
		}
	}
	read := func() map[string]any {
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("ws decode: %v ; raw=%s", err, raw)
		}
		return m
	}

	// 1) initialize
	send(1, "initialize", map[string]any{"processId": nil, "rootUri": "file:///proj", "capabilities": map[string]any{}})
	got := read()
	if got["id"] != float64(1) || got["result"] == nil {
		t.Fatalf("initialize: unexpected response: %v", got)
	}
	if res, ok := got["result"].(map[string]any); !ok || res["capabilities"] == nil {
		t.Fatalf("initialize: missing capabilities: %v", got)
	}
	notify("initialized", map[string]any{})

	// 2) didOpen → publishDiagnostics notification
	notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": "file:///proj/main.tex", "languageId": "latex", "version": 1, "text": "Hello"},
	})
	got = read()
	if got["method"] != "textDocument/publishDiagnostics" {
		t.Fatalf("didOpen: expected publishDiagnostics, got %v", got)
	}

	// 3) completion
	send(2, "textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": "file:///proj/main.tex"},
		"position":     map[string]int{"line": 0, "character": 3},
	})
	got = read()
	res, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("completion: missing result: %v", got)
	}
	if items, ok := res["items"].([]any); !ok || len(items) != 2 {
		t.Fatalf("completion: expected 2 items, got %v", res["items"])
	}

	// 4) hover
	send(3, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": "file:///proj/main.tex"},
		"position":     map[string]int{"line": 0, "character": 0},
	})
	got = read()
	hres, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("hover: missing result: %v", got)
	}
	contents, ok := hres["contents"].(map[string]any)
	if !ok || !strings.Contains(toString(contents["value"]), "fake hover") {
		t.Fatalf("hover: missing expected text: %v", hres)
	}

	// 5) definition
	send(4, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": "file:///proj/main.tex"},
		"position":     map[string]int{"line": 0, "character": 0},
	})
	got = read()
	if dres, ok := got["result"].([]any); !ok || len(dres) != 1 {
		t.Fatalf("definition: expected 1 location, got %v", got)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// buildFakeLSP builds cmd/fake-lsp into the test tempdir and verifies it
// actually runs on this host (a foreign-arch build under qemu can't
// exec). If it can't build or can't start, the caller's test is skipped
// rather than failed — the pure logic tests still exercise the framing.
func buildFakeLSP(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("LSPBRIDGE_FAKE"); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v
		}
	}
	// The name needs the platform's executable suffix. `go build` adds ".exe"
	// on Windows only in its DEFAULT naming -- "go build example/sam writes sam
	// or sam.exe" -- and -o "forces build to write the resulting executable to
	// the named output file", suffix and all. Without this the binary is
	// written as "fake-lsp", Windows will not exec a file with no recognised
	// extension, the pre-flight below skips the test, and the suite goes green
	// while the whole spawn path goes unrun: 92.3% coverage instead of 100%.
	out := filepath.Join(t.TempDir(), "fake-lsp")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	build := exec.Command("go", "build", "-o", out, "./cmd/fake-lsp")
	if buf, err := build.CombinedOutput(); err != nil {
		t.Skipf("fake-lsp build failed (skipping) : %v\n%s", err, buf)
	}
	// Pre-flight: confirm the freshly-built binary is executable on this
	// host before the handler tries to spawn it.
	probe := exec.Command(out)
	stdin, _ := probe.StdinPipe()
	if err := probe.Start(); err != nil {
		t.Skipf("fake-lsp not runnable on this host (skipping) : %v", err)
	}
	_ = stdin.Close()
	_ = probe.Wait()
	return out
}
