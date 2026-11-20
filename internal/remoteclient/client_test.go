package remoteclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	mcp "github.com/stacklok/toolhive-core/mcpcompat/mcp"
	"github.com/stacklok/toolhive-core/mcpcompat/server"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newMCPServer(t *testing.T, pageSize int, toolNames ...string) *httptest.Server {
	t.Helper()
	s := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false), server.WithPageSize(pageSize))
	for _, name := range toolNames {
		s.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)
	return ts
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestListTools(t *testing.T) {
	t.Parallel()
	ts := newMCPServer(t, 0, "beta", "alpha")

	tools, err := ListTools(testContext(t), ts.URL+"/mcp", TransportStreamableHTTP)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := toolNames(tools); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("got tools %v, want [alpha beta]", got)
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	t.Parallel()
	ts := newMCPServer(t, 2, "a", "b", "c", "d", "e")

	tools, err := ListTools(testContext(t), ts.URL+"/mcp", TransportStreamableHTTP)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := toolNames(tools); len(got) != 5 {
		t.Fatalf("got %d tools %v, want 5", len(got), got)
	}
}

func TestListToolsUnauthorized(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)

	_, err := ListTools(testContext(t), ts.URL, TransportStreamableHTTP)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("got error %v, want ErrAuthRequired", err)
	}
}

// A server that rejects the newest protocol revision with 400 and then the
// legacy initialize with 401 must still be reported as requiring auth.
func TestListToolsUnauthorizedAfterVersionRejection(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Method string `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)

		if msg.Method == "initialize" && msg.Params.ProtocolVersion != "" &&
			r.Header.Get("MCP-Protocol-Version") != "2026-07-28" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,` +
			`"message":"Unsupported protocol version","data":{"supported":["2025-06-18"]}}}`))
	}))
	t.Cleanup(ts.Close)

	_, err := ListTools(testContext(t), ts.URL, TransportStreamableHTTP)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("got error %v, want ErrAuthRequired", err)
	}
}

func TestListToolsServerErrorIsNotAuth(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	_, err := ListTools(testContext(t), ts.URL, TransportStreamableHTTP)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrAuthRequired) {
		t.Fatalf("got ErrAuthRequired for a 500 response: %v", err)
	}
}

func TestListToolsUnsupportedTransport(t *testing.T) {
	t.Parallel()
	_, err := ListTools(testContext(t), "https://example.com", "stdio")
	if err == nil {
		t.Fatal("expected an error for stdio transport")
	}
}
