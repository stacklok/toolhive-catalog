package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	mcp "github.com/stacklok/toolhive-core/mcpcompat/mcp"
	toolhiveRegistry "github.com/stacklok/toolhive-core/registry/types"

	"github.com/stacklok/toolhive-catalog/internal/remoteclient"
	"github.com/stacklok/toolhive-catalog/internal/serverjson"
	"github.com/stacklok/toolhive-catalog/internal/thvclient"
)

var (
	dryRunTools bool
	thvPath     string
)

// remoteTimeout bounds how long update-tools waits on a remote MCP server.
const remoteTimeout = 60 * time.Second

// skipError reports that a server was deliberately not updated, for example
// because it requires credentials that CI doesn't have. main exits with
// exitSkipped instead of the usual failure code so callers can tell the
// difference.
type skipError struct {
	reason error
}

func (e *skipError) Error() string { return e.reason.Error() }
func (e *skipError) Unwrap() error { return e.reason }

var updateToolsCmd = &cobra.Command{
	Use:   "update-tools <server.json>",
	Short: "Update tools list by querying the running MCP server",
	Long: `Queries an MCP server for its available tools and updates the tools list
in the _meta extensions.

For package servers, starts a temporary server with thv. For remote servers,
connects directly to the remote URL without credentials. Remote servers that
require authentication are skipped (exit code 3).`,
	Args:          cobra.ExactArgs(1),
	RunE:          runUpdateTools,
	SilenceErrors: true, // main prints the error, and distinguishes skips
	SilenceUsage:  true,
}

func init() {
	updateToolsCmd.Flags().BoolVarP(
		&dryRunTools, "dry-run", "d", false,
		"Show changes without writing",
	)
	updateToolsCmd.Flags().StringVar(
		&thvPath, "thv-path", "",
		"Path to thv binary (searches PATH if empty)",
	)
}

func runUpdateTools(_ *cobra.Command, args []string) error {
	path := args[0]

	sf, err := serverjson.LoadServerFile(path)
	if err != nil {
		return err
	}

	ext, err := sf.GetExtensions()
	if err != nil {
		return err
	}

	currentTools := ext.Tools
	currentDefs := ext.ToolDefinitions
	serverName := filepath.Base(filepath.Dir(path))

	if verbose {
		fmt.Printf("Processing server: %s\n", serverName)
		fmt.Printf("Current tools count: %d\n", len(currentTools))
		fmt.Printf("Current tool definitions count: %d\n", len(currentDefs))
	}

	var newTools []string
	var newDefs []mcp.Tool
	if sf.IsRemoteServer() {
		newTools, newDefs, err = fetchToolsFromRemote(sf)
		if errors.Is(err, remoteclient.ErrAuthRequired) {
			if verbose {
				fmt.Printf("Remote connection error: %v\n", err)
			}
			return &skipError{reason: remoteclient.ErrAuthRequired}
		}
	} else {
		newTools, newDefs, err = fetchToolsFromServer(sf, serverName)
	}
	if err != nil {
		return err
	}

	return applyToolsUpdate(sf, ext, currentTools, newTools, currentDefs, newDefs)
}

// fetchToolsFromRemote connects directly to the server's first remote and
// lists its tools without credentials.
func fetchToolsFromRemote(sf *serverjson.ServerFile) ([]string, []mcp.Tool, error) {
	remote := sf.ServerJSON.Remotes[0]
	fmt.Printf("Connecting to remote MCP server: %s\n", remote.URL)

	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()

	remoteDefs, err := remoteclient.ListTools(ctx, remote.URL, remote.Type)
	if err != nil {
		return nil, nil, err
	}
	defs, err := catalogToolDefinitions(remoteDefs)
	if err != nil {
		return nil, nil, err
	}

	tools := make([]string, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, d.Name)
	}
	sort.Strings(tools)
	fmt.Printf("Discovered %d tools (with definitions)\n", len(tools))
	return tools, defs, nil
}

// catalogToolDefinition holds the tool fields the catalog stores. It matches
// what the thv path records, so entries have the same shape whichever way
// their tools were fetched. Fields such as outputSchema are dropped; some
// servers embed very large output schemas.
type catalogToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// catalogToolDefinitions reduces tools to the fields the catalog stores and
// sorts them by name.
func catalogToolDefinitions(tools []mcp.Tool) ([]mcp.Tool, error) {
	defs := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		full, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal tool %s: %w", t.Name, err)
		}
		var stored catalogToolDefinition
		if err := json.Unmarshal(full, &stored); err != nil {
			return nil, fmt.Errorf("failed to read tool %s: %w", t.Name, err)
		}
		reduced, err := json.Marshal(stored)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal tool %s: %w", t.Name, err)
		}
		var def mcp.Tool
		if err := json.Unmarshal(reduced, &def); err != nil {
			return nil, fmt.Errorf("failed to convert tool %s: %w", t.Name, err)
		}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, nil
}

// fetchToolsFromServer starts a temporary MCP server and queries its tools.
func fetchToolsFromServer(
	sf *serverjson.ServerFile, serverName string,
) ([]string, []mcp.Tool, error) {
	client, err := thvclient.NewClient(thvPath, verbose)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create thv client: %w", err)
	}

	fmt.Printf("Starting temporary MCP server: %s\n", serverName)
	tempName, err := client.RunServer(sf, serverName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to run server: %w", err)
	}
	defer func() {
		if stopErr := client.StopServer(tempName); stopErr != nil {
			fmt.Printf(
				"Warning: failed to stop server %s: %v\n",
				tempName, stopErr,
			)
		}
	}()

	// Fetch full tool definitions first; extract names from them.
	defs, err := client.ListToolDefinitions(tempName)
	if err != nil {
		if logs, logErr := client.Logs(tempName); logErr == nil && logs != "" {
			fmt.Printf("Server logs:\n%s\n", logs)
		}
		return nil, nil, fmt.Errorf("failed to fetch tools: %w", err)
	}

	// If we got definitions, extract tool names from them.
	if defs != nil {
		var tools []string
		for _, d := range defs {
			tools = append(tools, d.Name)
		}
		sort.Strings(tools)
		fmt.Printf("Discovered %d tools (with definitions)\n", len(tools))
		return tools, defs, nil
	}

	// Text fallback: no definitions available, get names only.
	tools, err := client.ListTools(tempName)
	if err != nil {
		if logs, logErr := client.Logs(tempName); logErr == nil && logs != "" {
			fmt.Printf("Server logs:\n%s\n", logs)
		}
		return nil, nil, fmt.Errorf("failed to fetch tools: %w", err)
	}

	fmt.Printf("Discovered %d tools (names only)\n", len(tools))
	return tools, nil, nil
}

// applyToolsUpdate compares and writes the updated tools list and tool definitions.
func applyToolsUpdate(
	sf *serverjson.ServerFile,
	ext *toolhiveRegistry.ServerExtensions,
	currentTools, newTools []string,
	currentDefs, newDefs []mcp.Tool,
) error {
	if len(newTools) == 0 && len(currentTools) > 0 {
		fmt.Printf(
			"Warning: no tools detected but had %d previously. Keeping existing.\n",
			len(currentTools),
		)
		return fmt.Errorf("empty tools list detected")
	}

	sort.Strings(currentTools)
	sort.Strings(newTools)

	toolsChanged := !slices.Equal(currentTools, newTools)
	defsChanged := !toolDefinitionsEqual(currentDefs, newDefs)

	if !toolsChanged && !defsChanged {
		fmt.Printf("Tools list is already up to date (_meta size: %s)\n", formatBytes(sf.MetaSize()))
		return nil
	}

	if toolsChanged {
		printToolsDiff(currentTools, newTools)
	}
	if defsChanged {
		printToolDefsDiff(currentDefs, newDefs)
	}

	if dryRunTools {
		fmt.Println("[DRY RUN] Would update tools list")
		return nil
	}

	ext.Tools = newTools
	ext.ToolDefinitions = newDefs
	if ext.Metadata == nil {
		ext.Metadata = &toolhiveRegistry.Metadata{}
	}
	ext.Metadata.LastUpdated = time.Now().UTC().Format(time.RFC3339)

	if err := sf.UpdateExtensions(ext); err != nil {
		return fmt.Errorf("failed to write server.json: %w", err)
	}

	fmt.Printf("Successfully updated tools list (_meta size: %s)\n", formatBytes(sf.MetaSize()))
	return nil
}

func printToolsDiff(currentTools, newTools []string) {
	if verbose {
		diff := cmp.Diff(currentTools, newTools)
		if diff != "" {
			fmt.Printf("Diff:\n%s\n", diff)
		}
	} else {
		showToolsSummary(currentTools, newTools)
	}
}

func showToolsSummary(current, newTools []string) {
	added := diffSlices(newTools, current)
	removed := diffSlices(current, newTools)

	if len(added) > 0 {
		fmt.Printf("  Added (%d): %v\n", len(added), added)
	}
	if len(removed) > 0 {
		fmt.Printf("  Removed (%d): %v\n", len(removed), removed)
	}
}

// diffSlices returns items in a that are not in b.
func diffSlices(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	var result []string
	for _, v := range a {
		if _, ok := set[v]; !ok {
			result = append(result, v)
		}
	}
	return result
}

// toolDefinitionsEqual deeply compares two slices of tool definitions,
// including inputSchema and annotations, so that compact-to-full transitions
// are detected.
func toolDefinitionsEqual(a, b []mcp.Tool) bool {
	return cmp.Equal(a, b)
}

func printToolDefsDiff(currentDefs, newDefs []mcp.Tool) {
	if verbose {
		currentNames := toolDefNames(currentDefs)
		newNames := toolDefNames(newDefs)
		diff := cmp.Diff(currentNames, newNames)
		if diff != "" {
			fmt.Printf("Tool definitions diff:\n%s\n", diff)
		}
	} else {
		fmt.Printf("  Tool definitions: %d -> %d\n", len(currentDefs), len(newDefs))
	}
}

func toolDefNames(defs []mcp.Tool) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

func formatBytes(b int) string {
	const kb = 1024
	if b < kb {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
}
