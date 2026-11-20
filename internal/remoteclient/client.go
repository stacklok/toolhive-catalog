// Package remoteclient lists tools from remote MCP servers by connecting to
// them directly, without starting a ToolHive workload.
//
// It deliberately sends no credentials. Servers that require authentication
// are reported via ErrAuthRequired so callers can skip them rather than treat
// them as failures.
package remoteclient

import (
	"context"
	"errors"
	"fmt"

	mcpclient "github.com/stacklok/toolhive-core/mcpcompat/client"
	"github.com/stacklok/toolhive-core/mcpcompat/client/transport"
	mcp "github.com/stacklok/toolhive-core/mcpcompat/mcp"
)

// Transport types as they appear in server.json remotes[].type.
const (
	TransportStreamableHTTP = "streamable-http"
	TransportSSE            = "sse"
)

// clientName identifies this tool to remote servers in clientInfo.
const clientName = "toolhive-catalog"

// maxPages bounds tools/list pagination so a server that keeps returning a
// cursor can't loop forever.
const maxPages = 100

// ErrAuthRequired indicates the remote server rejected the unauthenticated
// request (HTTP 401 or an OAuth challenge).
var ErrAuthRequired = errors.New("remote server requires authentication")

// ListTools connects to the remote MCP server at url using the given
// transport type and returns all of its tools, following pagination.
//
// The caller controls the overall timeout through ctx.
func ListTools(ctx context.Context, url, transportType string) ([]mcp.Tool, error) {
	client, err := newClient(url, transportType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	if err := client.Start(ctx); err != nil {
		return nil, classify(fmt.Errorf("failed to start client: %w", err))
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: clientName, Version: "1.0.0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		err = classify(fmt.Errorf("failed to connect: %w", err))
		if transportType == TransportStreamableHTTP && errors.Is(err, transport.ErrLegacySSEServer) &&
			requiresAuth(ctx, url) {
			err = fmt.Errorf("%w: %w", ErrAuthRequired, err)
		}
		return nil, err
	}

	var tools []mcp.Tool
	var cursor mcp.Cursor
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("tools/list did not finish after %d pages", maxPages)
		}
		req := mcp.ListToolsRequest{}
		req.Params.Cursor = cursor
		res, err := client.ListTools(ctx, req)
		if err != nil {
			return nil, classify(fmt.Errorf("failed to list tools: %w", err))
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			return tools, nil
		}
		cursor = res.NextCursor
	}
}

func newClient(url, transportType string) (*mcpclient.Client, error) {
	switch transportType {
	case TransportStreamableHTTP:
		return mcpclient.NewStreamableHttpClient(url)
	case TransportSSE:
		return mcpclient.NewSSEMCPClient(url)
	default:
		return nil, fmt.Errorf("unsupported remote transport type %q", transportType)
	}
}

// classify wraps err with ErrAuthRequired when it represents an
// authentication failure, so callers can test for it with errors.Is.
func classify(err error) error {
	if isAuthError(err) {
		return fmt.Errorf("%w: %w", ErrAuthRequired, err)
	}
	return err
}

func isAuthError(err error) bool {
	var authErr *transport.AuthorizationRequiredError
	return errors.As(err, &authErr) ||
		errors.Is(err, transport.ErrUnauthorized) ||
		errors.Is(err, transport.ErrAuthorizationRequired) ||
		errors.Is(err, transport.ErrOAuthAuthorizationRequired)
}
