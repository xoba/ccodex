package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xoba/ccodex/internal/codex"
	"github.com/xoba/ccodex/internal/resetbudget"
)

func TestWatchSettingsDescribeEffectiveConfiguration(t *testing.T) {
	for _, test := range []struct {
		name              string
		args              []string
		budgetUnavailable bool
		want              []string
	}{
		{
			name: "default read-only watch",
			want: []string{"Alarm/auto-reset threshold: at or below 2% remaining", "Alarm: enabled", "Auto-reset: disabled (enable with --auto-reset)"},
		},
		{
			name: "custom threshold and daily cap",
			args: []string{"--auto-reset", "--alarm-threshold", "3.5", "--max-resets-per-day", "4"},
			want: []string{"Alarm/auto-reset threshold: at or below 3.5% remaining", "Auto-reset: enabled (daily cap: 4 per local calendar day"},
		},
		{
			name: "muting preserves automatic resets",
			args: []string{"--auto-reset", "--no-alarm"},
			want: []string{"Alarm: muted (--no-alarm)", "Auto-reset: enabled (daily cap: 1 per local calendar day"},
		},
		{
			name: "zero threshold overrides reset opt-in",
			args: []string{"--auto-reset", "--alarm-threshold", "0"},
			want: []string{"Alarm: disabled (--alarm-threshold=0)", "Auto-reset: disabled (--auto-reset is set, but --alarm-threshold=0)"},
		},
		{
			name: "zero cap overrides reset opt-in",
			args: []string{"--auto-reset", "--max-resets-per-day", "0"},
			want: []string{"Alarm: enabled", "Auto-reset: disabled (--auto-reset is set, but --max-resets-per-day=0)"},
		},
		{
			name:              "unavailable accounting overrides reset opt-in",
			args:              []string{"--auto-reset"},
			budgetUnavailable: true,
			want:              []string{"Auto-reset: unavailable (--auto-reset is set; daily accounting unavailable)"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
			cmd := newCommandWithBudget(func(context.Context, codex.Options) (*codex.Snapshot, error) {
				return autoSnapshot(25, 1), nil
			}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
				t.Fatal("healthy quota should not reset")
				return nil, nil
			}, func(context.Context, io.Writer) error {
				t.Fatal("healthy quota should not sound an alarm")
				return nil
			}, func() (autoResetBudget, error) {
				if test.budgetUnavailable {
					return nil, errors.New("cannot open reset accounting")
				}
				return store, nil
			})
			cmd.SetOut(cancelWriter{w: &stdout, cancel: cancel})
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"watch"}, test.args...))
			if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("watch failed: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("watch did not report %q:\n%s", want, stdout.String())
				}
			}
			if strings.Count(stdout.String(), "Auto-reset:") != 1 {
				t.Fatalf("watch emitted conflicting reset settings:\n%s", stdout.String())
			}
			if test.budgetUnavailable && !strings.Contains(stderr.String(), "cannot open reset accounting") {
				t.Fatalf("accounting failure explanation lost: %s", stderr.String())
			}
		})
	}
}

func TestWatchSettingsRepeatOnEachSuccessfulPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	reads := 0
	cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		reads++
		return autoSnapshot(25, 1), nil
	})
	cmd.SetOut(writeFunc(func(p []byte) (int, error) {
		n, err := stdout.Write(p)
		if strings.Count(stdout.String(), "Codex usage\n") == 2 {
			cancel()
		}
		return n, err
	}))
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"watch", "--interval", "1s", "--alarm-threshold", "4.5"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("watch failed: %v", err)
	}
	if reads != 2 || strings.Count(stdout.String(), "Alarm/auto-reset threshold: at or below 4.5% remaining") != 2 || strings.Count(stdout.String(), "Auto-reset: disabled") != 2 {
		t.Fatalf("settings were not repeated for both polls: reads=%d\n%s", reads, stdout.String())
	}
}

func TestWatchSettingsPersistAfterResetAndKeepJSONClean(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		name := "text"
		if jsonOutput {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout bytes.Buffer
			low := autoSnapshot(99, 1)
			healthy := autoSnapshot(0, 0)
			resets := 0
			cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
				return low, nil
			}, func(_ context.Context, _ codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
				resets++
				return &codex.ResetResult{
					Outcome: "reset", IdempotencyKey: params.IdempotencyKey,
					Account: healthy.Account, RateLimits: healthy.RateLimits, FetchedAt: healthy.FetchedAt,
				}, nil
			}, nil)
			cmd.SetOut(writeFunc(func(p []byte) (int, error) {
				n, err := stdout.Write(p)
				if resets == 1 {
					cancel()
				}
				return n, err
			}))
			cmd.SetErr(io.Discard)
			args := []string{"watch", "--auto-reset", "--no-alarm"}
			if jsonOutput {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("watch failed: %v", err)
			}
			if resets != 1 {
				t.Fatalf("expected one automatic reset, got %d", resets)
			}
			if !jsonOutput {
				if strings.Count(stdout.String(), "Codex usage\n") != 2 || strings.Count(stdout.String(), "Auto-reset: enabled") != 2 || strings.Count(stdout.String(), "Alarm: muted") != 2 {
					t.Fatalf("post-reset snapshot lost settings:\n%s", stdout.String())
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected two JSON snapshots, got:\n%s", stdout.String())
			}
			for i, want := range []*codex.Snapshot{low, healthy} {
				var got codex.Snapshot
				decoder := json.NewDecoder(strings.NewReader(lines[i]))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&got); err != nil {
					t.Fatalf("watch settings corrupted snapshot JSON: %v\n%s", err, lines[i])
				}
				if !reflect.DeepEqual(got, *want) {
					t.Fatalf("snapshot %d changed in JSON output: got %+v, want %+v", i, got, *want)
				}
			}
		})
	}
}

func TestStatusDoesNotDescribeWatchSettings(t *testing.T) {
	for _, args := range [][]string{nil, {"status"}} {
		var stdout bytes.Buffer
		cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			return autoSnapshot(25, 1), nil
		})
		cmd.SetOut(&stdout)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, watchLabel := range []string{"Alarm/auto-reset threshold:", "Alarm:", "Auto-reset:"} {
			if strings.Contains(stdout.String(), watchLabel) {
				t.Fatalf("status %v claimed watch configuration:\n%s", args, stdout.String())
			}
		}
	}
}
