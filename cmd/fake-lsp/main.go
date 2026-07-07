// fake-lsp is a deterministic LSP-like subprocess used by the
// lspbridge integration test. It reads Content-Length-framed JSON-RPC
// messages on stdin + writes canned responses on stdout so the bridge
// can be exercised without depending on texlab / gopls / etc. being
// installed on the dev box.
//
// Supported methods :
//
//	initialize     → { capabilities: { ... } }
//	initialized    → no reply (notification)
//	textDocument/didOpen → no reply ; sends a publishDiagnostics
//	                       notification with one fake warning on line 0
//	                       for any uri
//	textDocument/completion → { items: [{ label: "fakeItem", ... }] }
//	textDocument/hover      → { contents: { kind: "plaintext",
//	                            value: "fake hover @ ${uri}" } }
//	textDocument/definition → [{ uri, range: ... }]
//	shutdown                → null
//	exit                    → process exits
//
// Anything else gets a method-not-found error reply.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() { serve(bufio.NewReader(os.Stdin), os.Stdout) }

// serve runs the canned-response loop against an arbitrary reader/writer
// pair so it can be driven from tests without a real process.
func serve(in *bufio.Reader, out io.Writer) {
	for {
		body, err := readMsg(in)
		if err != nil {
			if err == io.EOF {
				return
			}
			fmt.Fprintf(os.Stderr, "fake-lsp: read: %v\n", err)
			return
		}
		var m rpc
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}
		if m.Method == "exit" {
			return
		}
		if m.ID == nil {
			// Notification — react to didOpen with a synthetic
			// publishDiagnostics so the editor's lint extension has
			// something to render.
			if m.Method == "textDocument/didOpen" {
				var p struct {
					TextDocument struct {
						URI string `json:"uri"`
					} `json:"textDocument"`
				}
				_ = json.Unmarshal(m.Params, &p)
				sendNotification(out, "textDocument/publishDiagnostics", map[string]any{
					"uri": p.TextDocument.URI,
					"diagnostics": []map[string]any{{
						"range": map[string]any{
							"start": map[string]int{"line": 0, "character": 0},
							"end":   map[string]int{"line": 0, "character": 5},
						},
						"severity": 2, // warning
						"message":  "fake warning",
						"source":   "fake-lsp",
					}},
				})
			}
			continue
		}
		// Request — send a response keyed on the method.
		switch m.Method {
		case "initialize":
			sendResult(out, m.ID, map[string]any{
				"capabilities": map[string]any{
					"textDocumentSync":   1,
					"completionProvider": map[string]any{"triggerCharacters": []string{"."}},
					"hoverProvider":      true,
					"definitionProvider": true,
				},
				"serverInfo": map[string]any{"name": "fake-lsp", "version": "0.1"},
			})
		case "textDocument/completion":
			sendResult(out, m.ID, map[string]any{
				"isIncomplete": false,
				"items": []map[string]any{{
					"label":      "fakeItem",
					"detail":     "fake completion",
					"insertText": "fakeItem",
					"kind":       1,
				}, {
					"label":      "anotherFake",
					"detail":     "second item",
					"insertText": "anotherFake",
					"kind":       3,
				}},
			})
		case "textDocument/hover":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(m.Params, &p)
			sendResult(out, m.ID, map[string]any{
				"contents": map[string]any{
					"kind":  "plaintext",
					"value": "fake hover @ " + p.TextDocument.URI,
				},
			})
		case "textDocument/definition":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(m.Params, &p)
			sendResult(out, m.ID, []map[string]any{{
				"uri": p.TextDocument.URI,
				"range": map[string]any{
					"start": map[string]int{"line": 0, "character": 0},
					"end":   map[string]int{"line": 0, "character": 1},
				},
			}})
		case "shutdown":
			sendResult(out, m.ID, nil)
		default:
			sendError(out, m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

func readMsg(in *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			if length == 0 {
				return nil, fmt.Errorf("missing Content-Length")
			}
			break
		}
		if len(line) > len("Content-Length:") &&
			(line[:len("Content-Length:")] == "Content-Length:" ||
				line[:len("Content-Length:")] == "content-length:") {
			v := line[len("Content-Length:"):]
			s := 0
			for s < len(v) && (v[s] == ' ' || v[s] == '\t') {
				s++
			}
			e := len(v)
			for e > s && (v[e-1] == ' ' || v[e-1] == '\t' || v[e-1] == '\r' || v[e-1] == '\n') {
				e--
			}
			n, err := strconv.Atoi(v[s:e])
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(in, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeMsg(out io.Writer, payload []byte) {
	fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(payload))
	_, _ = out.Write(payload)
}

func sendResult(out io.Writer, id json.RawMessage, result any) {
	b, _ := json.Marshal(rpc{JSONRPC: "2.0", ID: id, Result: mustMarshal(result)})
	writeMsg(out, b)
}

func sendError(out io.Writer, id json.RawMessage, code int, msg string) {
	b, _ := json.Marshal(rpc{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	writeMsg(out, b)
}

func sendNotification(out io.Writer, method string, params any) {
	b, _ := json.Marshal(rpc{JSONRPC: "2.0", Method: method, Params: mustMarshal(params)})
	writeMsg(out, b)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
