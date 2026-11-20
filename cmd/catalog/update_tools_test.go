package main

import (
	"encoding/json"
	"testing"

	mcp "github.com/stacklok/toolhive-core/mcpcompat/mcp"
)

func TestCatalogToolDefinitions(t *testing.T) {
	t.Parallel()

	var tools []mcp.Tool
	if err := json.Unmarshal([]byte(`[
		{
			"name": "zeta",
			"description": "Last tool",
			"inputSchema": {"type": "object", "properties": {"q": {"type": "string"}}},
			"outputSchema": {"type": "object", "properties": {"huge": {"type": "string"}}},
			"annotations": {"readOnlyHint": true},
			"_meta": {"vendor/key": "value"}
		},
		{"name": "alpha", "description": "First tool", "inputSchema": {"type": "object"}}
	]`), &tools); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	defs, err := catalogToolDefinitions(tools)
	if err != nil {
		t.Fatalf("catalogToolDefinitions: %v", err)
	}
	if len(defs) != 2 || defs[0].Name != "alpha" || defs[1].Name != "zeta" {
		t.Fatalf("got %d defs, want [alpha zeta] in order", len(defs))
	}

	out, err := json.Marshal(defs[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, dropped := range []string{"outputSchema", "_meta"} {
		if _, ok := fields[dropped]; ok {
			t.Errorf("%s should be dropped, got %s", dropped, out)
		}
	}
	for _, kept := range []string{"name", "description", "inputSchema", "annotations"} {
		if _, ok := fields[kept]; !ok {
			t.Errorf("%s should be kept, got %s", kept, out)
		}
	}
}
