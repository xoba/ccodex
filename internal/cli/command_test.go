package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xoba/ccodex/internal/codex"
)

func TestStatusJSON(t *testing.T) {
	var stdout bytes.Buffer
	cmd := newCommand(func(ctx context.Context, opts codex.Options) (*codex.Snapshot, error) {
		if opts.Binary != "/custom/codex" || opts.Timeout != 2*time.Second {
			t.Fatalf("options not forwarded: %+v", opts)
		}
		return exampleSnapshot(t), nil
	})
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"status", "--json", "--codex-bin", "/custom/codex", "--timeout", "2s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		FetchedAt  string `json:"fetchedAt"`
		RateLimits struct {
			Buckets map[string]struct {
				Primary struct {
					UsedPercent *float64 `json:"usedPercent"`
					ResetsAt    *int64   `json:"resetsAt"`
				} `json:"primary"`
			} `json:"rateLimitsByLimitId"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.FetchedAt != "2026-09-15T12:00:00Z" {
		t.Fatalf("wrong refresh timestamp: %s", result.FetchedAt)
	}
	if bucket := result.RateLimits.Buckets["codex"]; bucket.Primary.UsedPercent == nil || *bucket.Primary.UsedPercent != 25 || bucket.Primary.ResetsAt == nil {
		t.Fatalf("quota data lost: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"usedPercent": null`) {
		t.Fatalf("unknown percentage should remain null: %s", stdout.String())
	}
}

func TestCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--timeout", "0s"}, {"--timeout", "-1s"}, {"--codex-bin="},
		{"watch", "--interval", "0s"}, {"watch", "--interval", "500ms"},
		{"status", "extra"}, {"typo"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
				t.Fatal("invalid arguments triggered a fetch")
				return nil, nil
			})
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected argument error")
			}
		})
	}
}

func TestStatusFailureHasNoSuccessOutput(t *testing.T) {
	want := errors.New("not signed in")
	var stdout bytes.Buffer
	cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) { return nil, want })
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"status", "--json"})
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed status emitted success output: %s", stdout.String())
	}
}

func TestWatchRetriesAndWritesJSONLines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	calls := 0
	cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary connection failure")
		}
		return exampleSnapshot(t), nil
	})
	cmd.SetOut(cancelWriter{w: &stdout, cancel: cancel})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--json", "--interval", "1s"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if calls != 2 || !strings.Contains(stderr.String(), "refresh failed") {
		t.Fatalf("failed poll did not retry: calls=%d stderr=%s", calls, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
		t.Fatalf("expected one successful JSON line, got: %s", stdout.String())
	}
}

type cancelWriter struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (w cancelWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.cancel()
	return n, err
}

func TestOutputErrorStopsCommand(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"watch", "--json"}} {
		cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			return exampleSnapshot(t), nil
		})
		cmd.SetOut(failingWriter{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("output error not propagated: %v", err)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
