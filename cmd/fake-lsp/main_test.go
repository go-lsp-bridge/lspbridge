package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// frame wraps a payload in LSP Content-Length framing.
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

// TestServeAllMethods drives serve through every request kind, both
// notification branches, the invalid-JSON skip, and the exit return.
func TestServeAllMethods(t *testing.T) {
	var in strings.Builder
	in.WriteString(frame("{not json"))                                            // invalid JSON → continue
	in.WriteString(frame(`{"jsonrpc":"2.0","method":"initialized","params":{}}`)) // notification, not didOpen
	in.WriteString(frame(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a"}}}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":2,"method":"textDocument/completion","params":{"textDocument":{"uri":"file:///a"}}}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///a"}}}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":4,"method":"textDocument/definition","params":{"textDocument":{"uri":"file:///a"}}}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":5,"method":"shutdown","params":null}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","id":6,"method":"nope","params":null}`))
	in.WriteString(frame(`{"jsonrpc":"2.0","method":"exit"}`)) // → return

	var out bytes.Buffer
	serve(bufio.NewReader(strings.NewReader(in.String())), &out)

	got := out.String()
	for _, want := range []string{
		"publishDiagnostics", "fake warning",
		`"serverInfo"`, "fakeItem", "anotherFake",
		"fake hover @ file:///a", `"definitionProvider"`,
		"method not found: nope",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("serve output missing %q\n---\n%s", want, got)
		}
	}
	// shutdown replies with a null result (id 5); definition replies an array.
	if !strings.Contains(got, `"id":5,"result":null`) {
		t.Fatalf("shutdown reply missing: %s", got)
	}
}

// TestServeEOF covers the io.EOF return branch (stream ends without exit).
func TestServeEOF(t *testing.T) {
	var out bytes.Buffer
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	serve(bufio.NewReader(strings.NewReader(in)), &out)
	if !strings.Contains(out.String(), "serverInfo") {
		t.Fatalf("initialize not answered: %s", out.String())
	}
}

// TestServeReadError covers the non-EOF read-error branch (bad framing).
func TestServeReadError(t *testing.T) {
	// Silence the stderr diagnostic the branch emits.
	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	var out bytes.Buffer
	serve(bufio.NewReader(strings.NewReader("Content-Length: notanumber\r\n\r\n")), &out)
	_ = w.Close()
	buf, _ := readAll(r)
	if !strings.Contains(string(buf), "fake-lsp: read:") {
		t.Fatalf("expected stderr diagnostic, got %q", buf)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output on read error, got %q", out.String())
	}
}

func TestReadMsg(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"basic", frame("hello"), "hello", false},
		{"lowercase-and-lf-blank", "content-length: 5\n\nhello", "hello", false},
		{"extra-header", "Content-Type: x\r\nContent-Length: 2\r\n\r\nhi", "hi", false},
		{"short-header-line", "X\r\n\r\n", "", true}, // missing Content-Length
		{"blank-first", "\r\n", "", true},            // missing Content-Length
		{"bad-atoi", "Content-Length: nope\r\n\r\n", "", true},
		{"truncated-body", "Content-Length: 10\r\n\r\nshort", "", true}, // io.ReadFull err
		{"eof-mid-header", "Content-Length: 5", "", true},               // ReadString err
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readMsg(bufio.NewReader(strings.NewReader(tc.input)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMustMarshal(t *testing.T) {
	if got := string(mustMarshal(map[string]int{"a": 1})); got != `{"a":1}` {
		t.Fatalf("mustMarshal ok path: %s", got)
	}
	// math.Inf is not representable in JSON → marshal fails → "null".
	if got := string(mustMarshal(math.Inf(1))); got != "null" {
		t.Fatalf("mustMarshal error path: got %s want null", got)
	}
	// Sanity: our null is valid JSON.
	var v any
	if err := json.Unmarshal(mustMarshal(math.Inf(1)), &v); err != nil {
		t.Fatalf("null not valid json: %v", err)
	}
}

// TestMainWiring executes main() itself (an empty stdin → immediate EOF)
// so the one-line wiring is covered in-process.
func TestMainWiring(t *testing.T) {
	origIn, origOut := os.Stdin, os.Stdout
	rIn, wIn, _ := os.Pipe()
	rOut, wOut, _ := os.Pipe()
	os.Stdin, os.Stdout = rIn, wOut
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()

	_ = wIn.Close() // EOF immediately
	done := make(chan struct{})
	go func() { main(); close(done) }()
	<-done
	_ = wOut.Close()
	_, _ = readAll(rOut)
}

func readAll(r *os.File) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 512)
	for {
		n, err := r.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			return buf.Bytes(), nil
		}
	}
}
