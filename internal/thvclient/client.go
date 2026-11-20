package thvclient

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	mcp "github.com/stacklok/toolhive-core/mcpcompat/mcp"

	"github.com/stacklok/toolhive-catalog/internal/serverjson"
)

const (
	// defaultReadyTimeout bounds how long RunServer waits for a workload to
	// reach "running". It covers image pulls and slow container startup on
	// CI runners, where large images can take well over a minute.
	defaultReadyTimeout = 3 * time.Minute

	// defaultReadyPollInterval is how often RunServer checks workload status.
	defaultReadyPollInterval = 2 * time.Second
)

// Workload statuses reported by `thv status`. These mirror the
// WorkloadStatus values in ToolHive's pkg/container/runtime package.
const (
	statusRunning   = "running"
	statusStarting  = "starting"
	statusError     = "error"
	statusStopped   = "stopped"
	statusStopping  = "stopping"
	statusUnhealthy = "unhealthy"
	statusRemoving  = "removing"
)

// WorkloadStatus is the subset of `thv status --format json` output that
// RunServer needs.
type WorkloadStatus struct {
	Status string `json:"status"`
	Health string `json:"health,omitempty"`
}

// Client interacts with the ToolHive CLI (thv) to run MCP servers
// and discover their available tools.
type Client struct {
	thvPath      string
	verbose      bool
	readyTimeout time.Duration
	pollInterval time.Duration
}

// NewClient creates a new ToolHive client. If thvPath is empty, it searches PATH.
func NewClient(thvPath string, verbose bool) (*Client, error) {
	if thvPath == "" {
		var err error
		thvPath, err = exec.LookPath("thv")
		if err != nil {
			return nil, fmt.Errorf("thv binary not found in PATH: %w", err)
		}
	}

	return &Client{
		thvPath:      thvPath,
		verbose:      verbose,
		readyTimeout: defaultReadyTimeout,
		pollInterval: defaultReadyPollInterval,
	}, nil
}

// RunServer starts a temporary MCP server from a server.json file.
// Returns the temporary server name for subsequent operations.
func (c *Client) RunServer(sf *serverjson.ServerFile, serverName string) (string, error) {
	if sf.IsRemoteServer() {
		return "", fmt.Errorf("remote servers cannot be run locally")
	}
	if !sf.IsPackageServer() {
		return "", fmt.Errorf("no packages found in server.json")
	}

	image := sf.ServerJSON.Packages[0].Identifier
	if image == "" {
		return "", fmt.Errorf("empty image identifier in server.json")
	}

	ext, err := sf.GetExtensions()
	if err != nil {
		return "", fmt.Errorf("failed to get extensions: %w", err)
	}

	tempName := fmt.Sprintf("temp-%s-%d", serverName, time.Now().Unix())
	runArgs := BuildRunCommand(sf, ext, tempName, image)

	if c.verbose {
		fmt.Printf("Running command: thv %s\n", strings.Join(runArgs, " "))
	}

	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	runCmd := exec.Command(c.thvPath, runArgs...)
	runOutput, err := runCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"failed to start MCP server: %w\nOutput: %s",
			err, string(runOutput),
		)
	}

	if err := c.waitForReady(tempName); err != nil {
		if logs, logErr := c.Logs(tempName); logErr == nil && logs != "" {
			fmt.Printf("Server logs:\n%s\n", logs)
		}
		if stopErr := c.StopServer(tempName); stopErr != nil {
			fmt.Printf("Warning: %v\n", stopErr)
		}
		return "", err
	}

	return tempName, nil
}

// statusWaitDelay bounds how long a cancelled `thv status` call may take to
// release its output pipes before Wait returns anyway.
const statusWaitDelay = 5 * time.Second

// Status returns the current status of a workload. The thv subprocess is
// killed if ctx is cancelled or its deadline passes.
func (c *Client) Status(ctx context.Context, serverName string) (WorkloadStatus, error) {
	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	cmd := exec.CommandContext(ctx, c.thvPath, "status", serverName, "--format", "json")
	cmd.WaitDelay = statusWaitDelay
	output, err := cmd.CombinedOutput()
	if err != nil {
		return WorkloadStatus{}, fmt.Errorf(
			"thv status failed: %w\nOutput: %s",
			err, string(output),
		)
	}
	return ParseStatusJSON(string(output))
}

// waitForReady polls the workload status until the server is running.
func (c *Client) waitForReady(serverName string) error {
	if c.verbose {
		fmt.Printf("Waiting up to %s for %s to be ready\n", c.readyTimeout, serverName)
	}
	ctx, cancel := context.WithTimeoutCause(context.Background(), c.readyTimeout,
		fmt.Errorf("timed out after %s", c.readyTimeout))
	defer cancel()

	start := time.Now()
	err := waitForStatus(ctx, serverName, c.Status, c.pollInterval)
	if err == nil && c.verbose {
		fmt.Printf("Server %s ready after %s\n", serverName, time.Since(start).Round(time.Second))
	}
	return err
}

// waitForStatus polls getStatus until the workload is running, reaches a
// state it won't recover from, or ctx is done. getStatus receives ctx so an
// in-flight status check is cancelled when the deadline passes.
func waitForStatus(
	ctx context.Context,
	serverName string,
	getStatus func(context.Context, string) (WorkloadStatus, error),
	interval time.Duration,
) error {
	var last WorkloadStatus
	var lastErr error

	for {
		st, err := getStatus(ctx, serverName)
		if err != nil {
			// The workload may not be registered yet; keep polling.
			lastErr = err
		} else {
			last, lastErr = st, nil
			switch st.Status {
			case statusRunning:
				return nil
			case statusError, statusStopped, statusStopping, statusUnhealthy, statusRemoving:
				return fmt.Errorf("server %s failed to start (status: %s)%s",
					serverName, st.Status, healthSuffix(st.Health))
			}
		}

		if ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(interval):
				continue
			}
		}

		if lastErr != nil {
			return fmt.Errorf("server %s did not become ready: %w (last status check: %w)",
				serverName, context.Cause(ctx), lastErr)
		}
		status := last.Status
		if status == "" {
			status = statusStarting
		}
		return fmt.Errorf("server %s did not become ready: %w (status: %s)%s",
			serverName, context.Cause(ctx), status, healthSuffix(last.Health))
	}
}

func healthSuffix(health string) string {
	if health == "" {
		return ""
	}
	return ": " + health
}

// ListTools queries a running MCP server for its tools.
func (c *Client) ListTools(serverName string) ([]string, error) {
	listArgs := NewCommandBuilder("mcp").
		AddPositional("list").
		AddPositional("tools").
		AddFlag("--server", serverName).
		AddFlag("--format", "json").
		Build()

	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	listCmd := exec.Command(c.thvPath, listArgs...)
	output, err := listCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"thv mcp list failed: %w\nOutput: %s",
			err, string(output),
		)
	}

	return ParseToolsJSON(string(output))
}

// ListToolDefinitions queries a running MCP server for full tool definitions.
// Returns nil (not an error) if the output is text-only.
func (c *Client) ListToolDefinitions(serverName string) ([]mcp.Tool, error) {
	listArgs := NewCommandBuilder("mcp").
		AddPositional("list").
		AddPositional("tools").
		AddFlag("--server", serverName).
		AddFlag("--format", "json").
		Build()

	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	listCmd := exec.Command(c.thvPath, listArgs...)
	output, err := listCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"thv mcp list failed: %w\nOutput: %s",
			err, string(output),
		)
	}

	return ParseToolDefinitions(string(output))
}

// Logs retrieves logs from a running MCP server.
func (c *Client) Logs(serverName string) (string, error) {
	logsArgs := NewCommandBuilder("logs").
		AddFlag("--follow", "false").
		AddPositional(serverName).
		Build()

	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	logsCmd := exec.Command(c.thvPath, logsArgs...)
	output, err := logsCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"thv logs failed: %w\nOutput: %s",
			err, string(output),
		)
	}

	return string(output), nil
}

// StopServer stops a running MCP server.
func (c *Client) StopServer(serverName string) error {
	// #nosec G204 - thvPath is validated in NewClient via exec.LookPath
	stopCmd := exec.Command(c.thvPath, "stop", serverName)
	if err := stopCmd.Run(); err != nil {
		return fmt.Errorf("failed to stop server %s: %w", serverName, err)
	}
	return nil
}
