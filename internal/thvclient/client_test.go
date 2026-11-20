package thvclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// statusSequence returns a getStatus func that yields each result in order,
// repeating the last one once the sequence is exhausted.
func statusSequence(
	results ...func() (WorkloadStatus, error),
) (func(context.Context, string) (WorkloadStatus, error), *int) {
	calls := 0
	return func(context.Context, string) (WorkloadStatus, error) {
		i := calls
		if i >= len(results) {
			i = len(results) - 1
		}
		calls++
		return results[i]()
	}, &calls
}

func status(s, health string) func() (WorkloadStatus, error) {
	return func() (WorkloadStatus, error) { return WorkloadStatus{Status: s, Health: health}, nil }
}

func statusErr(msg string) func() (WorkloadStatus, error) {
	return func() (WorkloadStatus, error) { return WorkloadStatus{}, errors.New(msg) }
}

// withTimeout returns a context with the given timeout and the same
// descriptive cause that waitForReady uses.
func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeoutCause(context.Background(), d, errors.New("timed out after "+d.String()))
	t.Cleanup(cancel)
	return ctx
}

func TestWaitForStatus_RunningAfterStarting(t *testing.T) {
	t.Parallel()
	get, calls := statusSequence(
		statusErr("workload not found"),
		status(statusStarting, ""),
		status(statusStarting, ""),
		status(statusRunning, ""),
	)

	if err := waitForStatus(withTimeout(t, time.Second), "srv", get, time.Millisecond); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if *calls != 4 {
		t.Errorf("expected 4 status calls, got %d", *calls)
	}
}

func TestWaitForStatus_FailsFastOnTerminalStatus(t *testing.T) {
	t.Parallel()
	for _, s := range []string{statusError, statusStopped, statusStopping, statusUnhealthy, statusRemoving} {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			get, calls := statusSequence(
				status(statusStarting, ""),
				status(s, "container exited"),
			)

			err := waitForStatus(withTimeout(t, time.Minute), "srv", get, time.Millisecond)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "status: "+s) || !strings.Contains(err.Error(), "container exited") {
				t.Errorf("error should include status and health, got %q", err)
			}
			if *calls != 2 {
				t.Errorf("expected to stop after 2 status calls, got %d", *calls)
			}
		})
	}
}

func TestWaitForStatus_TimesOutWhileStarting(t *testing.T) {
	t.Parallel()
	get, _ := statusSequence(status(statusStarting, ""))

	err := waitForStatus(withTimeout(t, 20*time.Millisecond), "srv", get, time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after") || !strings.Contains(err.Error(), "status: starting") {
		t.Errorf("unexpected error: %q", err)
	}
}

func TestWaitForStatus_TimesOutWithStatusError(t *testing.T) {
	t.Parallel()
	get, _ := statusSequence(statusErr("workload not found"))

	err := waitForStatus(withTimeout(t, 20*time.Millisecond), "srv", get, time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "workload not found") {
		t.Errorf("error should wrap the last status error, got %q", err)
	}
}

func TestWaitForStatus_CancelsHungStatusCheck(t *testing.T) {
	t.Parallel()
	// A status check that only returns when its context is cancelled, like
	// a thv subprocess killed by exec.CommandContext.
	get := func(ctx context.Context, _ string) (WorkloadStatus, error) {
		<-ctx.Done()
		return WorkloadStatus{}, ctx.Err()
	}

	start := time.Now()
	err := waitForStatus(withTimeout(t, 50*time.Millisecond), "srv", get, time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitForStatus took %s, want it bounded by the timeout", elapsed)
	}
}

func TestStatus_KillsHungSubprocess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake thv")
	}
	fakeThv := filepath.Join(t.TempDir(), "thv")
	if err := os.WriteFile(fakeThv, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil { // #nosec G306 - test executable
		t.Fatal(err)
	}
	c := &Client{thvPath: fakeThv}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Status(ctx, "srv"); err == nil {
		t.Fatal("expected error from killed subprocess")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Status took %s, want the subprocess killed at the deadline", elapsed)
	}
}

func TestParseStatusJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		output     string
		wantStatus string
		wantHealth string
		wantErr    bool
	}{
		{
			name:       "running",
			output:     `{"name": "temp-fetch", "status": "running", "url": "http://127.0.0.1:1234/mcp"}`,
			wantStatus: statusRunning,
		},
		{
			name:       "error with health",
			output:     `{"name": "temp-bad", "status": "error", "health": "failed to create container"}`,
			wantStatus: statusError,
			wantHealth: "failed to create container",
		},
		{
			name:       "log lines before JSON",
			output:     "some warning\n{\"status\": \"starting\"}",
			wantStatus: statusStarting,
		},
		{name: "not JSON", output: "Error: workload not found", wantErr: true},
		{name: "missing status", output: `{"name": "x"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := ParseStatusJSON(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.Status != tt.wantStatus || st.Health != tt.wantHealth {
				t.Errorf("got %+v, want status=%q health=%q", st, tt.wantStatus, tt.wantHealth)
			}
		})
	}
}
