package remoteclient

import (
	"context"
	"net/http"
	"strings"
)

// legacyProtocolVersion is a pre-2026-07-28 revision that servers still
// commonly accept for the initialize handshake.
const legacyProtocolVersion = "2025-06-18"

const legacyInitializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"` + legacyProtocolVersion + `","capabilities":{},` +
	`"clientInfo":{"name":"` + clientName + `","version":"1.0.0"}}}`

// requiresAuth reports whether an unauthenticated legacy initialize request to
// url is rejected with HTTP 401.
//
// The mcpcompat client records only the first non-2xx response of a connect
// attempt. When a server rejects the client's first, newest-revision request
// with 400 (unsupported protocol version) and then rejects the fallback
// initialize with 401, the client reports a legacy SSE server instead of an
// auth failure. This probe recovers the 401.
func requiresAuth(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(legacyInitializeBody))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", legacyProtocolVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusUnauthorized
}
