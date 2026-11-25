// Command fakemcp is a minimal stdio MCP server for the client's tests:
// newline-delimited JSON-RPC, no SDK, deterministic behavior. It offers
// three tools:
//
//	echo {"text"}          → "echo: <text>"
//	slow {"ms", "text"}    → sleeps ms, then returns text
//	big  {"chars"}         → returns chars characters
//
// Test scripts talk to it through the tools.Registry like any real
// server. The directory name (testdata) keeps go build ./... and vet
// from traversing it; the tests build it explicitly.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Name            string          `json:"name"`
		Arguments       json.RawMessage `json:"arguments"`
	} `json:"params"`
}

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func respond(w *bufio.Writer, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Fprintf(w, "%s\n", b)
	w.Flush()
}

func call(name string, args json.RawMessage) (string, bool) {
	var in map[string]any
	_ = json.Unmarshal(args, &in)
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	num := func(k string) int {
		f, _ := in[k].(float64)
		return int(f)
	}
	switch name {
	case "echo":
		return "echo: " + str("text"), false
	case "slow":
		time.Sleep(time.Duration(num("ms")) * time.Millisecond)
		return str("text"), false
	case "big":
		return strings.Repeat("x", num("chars")), false
	}
	return "no such tool " + name, true
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	w := bufio.NewWriter(os.Stdout)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req request
		if json.Unmarshal([]byte(line), &req) != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // a notification: nothing to answer
		}
		switch req.Method {
		case "initialize":
			respond(w, req.ID, map[string]any{
				"protocolVersion": req.Params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fakemcp", "version": "1.0.0"},
			})
		case "tools/list":
			respond(w, req.ID, map[string]any{"tools": []tool{
				{Name: "echo", Description: "Echo the text back", InputSchema: map[string]any{
					"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required": []string{"text"},
				}},
				{Name: "slow", Description: "Sleep, then return the text", InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ms":   map[string]any{"type": "integer"},
						"text": map[string]any{"type": "string"},
					},
				}},
				{Name: "big", Description: "Return a big result", InputSchema: map[string]any{
					"type": "object", "properties": map[string]any{"chars": map[string]any{"type": "integer"}},
				}},
			}})
		case "tools/call":
			text, isErr := call(req.Params.Name, req.Params.Arguments)
			respond(w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			})
		default:
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found: " + req.Method},
			})
			fmt.Fprintf(w, "%s\n", b)
			w.Flush()
		}
	}
}
