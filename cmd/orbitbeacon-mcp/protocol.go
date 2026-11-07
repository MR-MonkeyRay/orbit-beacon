package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"orbitbeacon/internal/buildinfo"
)

const maxLineBytes = 1 << 20
const preferredVersion = "2025-06-18"

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func rpcFailure(id json.RawMessage, code int, message string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{code, message}}
}

type session struct {
	initialized bool
	client      *apiClient
}

func (s *session) handle(ctx context.Context, line []byte) *response {
	var fields map[string]json.RawMessage
	if !utf8.Valid(line) || !json.Valid(line) {
		return rpcFailure(nil, -32700, "Parse error")
	}
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return rpcFailure(nil, -32600, "Invalid Request")
	}
	id, hasID := fields["id"]
	if hasID {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(id))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			return rpcFailure(nil, -32600, "Invalid Request")
		}
		switch value.(type) {
		case nil, string, json.Number:
		default:
			return rpcFailure(nil, -32600, "Invalid Request")
		}
	}
	var version, method string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || json.Unmarshal(fields["method"], &method) != nil || method == "" {
		return rpcFailure(nil, -32600, "Invalid Request")
	}
	if !hasID {
		return nil
	}
	success := func(value any) *response { return &response{JSONRPC: "2.0", ID: id, Result: value} }
	params := fields["params"]
	switch method {
	case "ping":
		return success(struct{}{})
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) != nil || p.ProtocolVersion == "" {
			return rpcFailure(id, -32602, "Invalid params")
		}
		switch p.ProtocolVersion {
		case "2025-06-18", "2025-03-26", "2024-11-05":
		default:
			p.ProtocolVersion = preferredVersion
		}
		s.initialized = true
		return success(map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": struct{}{}}, "serverInfo": map[string]string{"name": "orbitbeacon", "version": buildinfo.Current().Version}})
	}
	if !s.initialized {
		return rpcFailure(id, -32002, "Not initialized")
	}
	switch method {
	case "tools/list":
		if len(params) != 0 {
			var p map[string]json.RawMessage
			if json.Unmarshal(params, &p) != nil || p == nil || len(p) != 0 {
				return rpcFailure(id, -32602, "Invalid params")
			}
		}
		return success(map[string]any{"tools": toolTable()})
	case "tools/call":
		var p map[string]json.RawMessage
		if json.Unmarshal(params, &p) != nil || p == nil {
			return rpcFailure(id, -32602, "Invalid params")
		}
		for key := range p {
			if key != "name" && key != "arguments" {
				return rpcFailure(id, -32602, "Invalid params")
			}
		}
		var name string
		if json.Unmarshal(p["name"], &name) != nil {
			return rpcFailure(id, -32602, "Invalid params")
		}
		for _, t := range toolTable() {
			if t.Name != name {
				continue
			}
			target, ok := t.requestTarget(p["arguments"])
			if !ok {
				return success(result("Invalid tool arguments", true))
			}
			return success(s.client.get(ctx, target))
		}
		return rpcFailure(id, -32602, "Unknown tool")
	default:
		return rpcFailure(id, -32601, "Method not found")
	}
}

// ReadSlice bounds buffering even for arbitrarily long lines. Discard the rest
// of an oversized line, report one error, then resume at the next newline.
func serve(ctx context.Context, in io.Reader, out io.Writer, client *apiClient) error {
	reader := bufio.NewReaderSize(in, 64<<10)
	s := session{client: client}
	line := make([]byte, 0, 64<<10)
	oversized := false
	for {
		if ctx.Err() != nil {
			return nil
		}
		fragment, err := reader.ReadSlice('\n')
		complete := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if complete {
			fragment = fragment[:len(fragment)-1]
		}
		if !oversized {
			if len(line)+len(fragment) > maxLineBytes {
				oversized = true
				line = line[:0]
			} else {
				line = append(line, fragment...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if complete || len(line) != 0 || oversized {
			var reply *response
			if oversized {
				reply = rpcFailure(nil, -32700, "Parse error")
			} else {
				reply = s.handle(ctx, line)
			}
			if reply != nil {
				encoded, marshalErr := json.Marshal(reply)
				if marshalErr != nil {
					return marshalErr
				}
				if _, writeErr := out.Write(append(encoded, '\n')); writeErr != nil {
					return writeErr
				}
			}
			line = line[:0]
			oversized = false
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
