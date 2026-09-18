package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xoba/ccodex/internal/codex"
)

func TestWatchSoundsOnceEachLowIterationWithManyLowQuotas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	reads, sounds := 0, 0
	cmd := newCommandWithAlarm(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		reads++
		snapshot := quotaSnapshot(codex.RateLimitSnapshot{})
		snapshot.RateLimits.RateLimitsByLimitID = map[string]codex.RateLimitSnapshot{
			"codex": {Primary: quotaWindow(99), Secondary: quotaWindow(100)},
			"other": {Primary: quotaWindow(98)},
		}
		return snapshot, nil
	}, nil, func(ctx context.Context, w io.Writer) error {
		sounds++
		if sounds != reads {
			t.Fatalf("played %d times during %d iterations", sounds, reads)
		}
		fmt.Fprint(w, "\a")
		if sounds == 2 {
			cancel()
		}
		return nil
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--interval", "1s", "--json"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	// Each sound is followed by the reminder that nothing will be spent; the
	// second sound cancels the watch before its reminder is written.
	if reads != 2 || sounds != 2 || strings.Count(stderr.String(), "\a") != 2 || !strings.HasPrefix(stderr.String(), "\accodex: alarm: quota is at or below 2% remaining") {
		t.Fatalf("reads=%d sounds=%d stderr=%q", reads, sounds, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two snapshots, got %s", stdout.String())
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("alarm corrupted JSON stdout: %q", line)
		}
	}
}

func TestWatchDoesNotSoundWhenMutedOrQuotaIsNotLow(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot *codex.Snapshot
		muted    bool
	}{
		{"muted", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(99)}), true},
		{"just above threshold", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(97.99)}), false},
		{"unknown", quotaSnapshot(codex.RateLimitSnapshot{}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			cmd := newCommandWithAlarm(func(context.Context, codex.Options) (*codex.Snapshot, error) {
				reads++
				if reads == 2 {
					cancel()
				}
				return test.snapshot, nil
			}, nil, func(context.Context, io.Writer) error {
				t.Fatal("unexpected alarm")
				return nil
			})
			cmd.SetOut(io.Discard)
			args := []string{"watch", "--interval", "1s"}
			if test.muted {
				args = append(args, "--no-alarm")
			}
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestWatchContinuesAfterSoundFailureWithoutReplayingOnFetchFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	reads, sounds := 0, 0
	cmd := newCommandWithAlarm(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		reads++
		switch reads {
		case 1:
			return quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(99)}), nil
		case 2:
			return nil, errors.New("fetch failed")
		default:
			cancel()
			return nil, context.Canceled
		}
	}, nil, func(context.Context, io.Writer) error {
		sounds++
		return errors.New("audio unavailable")
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--interval", "1s"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if sounds != 1 || reads != 3 || !strings.Contains(stderr.String(), "audio unavailable") || !strings.Contains(stderr.String(), "refresh failed") {
		t.Fatalf("unexpected retries: sounds=%d reads=%d stderr=%s", sounds, reads, stderr.String())
	}
}

func TestStatusDoesNotSoundForLowQuota(t *testing.T) {
	cmd := newCommandWithAlarm(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		return quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(100)}), nil
	}, nil, func(context.Context, io.Writer) error {
		t.Fatal("status sounded the watch alarm")
		return nil
	})
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchCustomAlarmThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sounds := 0
	cmd := newCommandWithAlarm(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		return quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(93)}), nil
	}, nil, func(context.Context, io.Writer) error {
		sounds++
		cancel()
		return nil
	})
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"watch", "--alarm-threshold", "10"})
	// Bound the test in case a regression keeps the default threshold of 2.
	testCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(testCtx); !errors.Is(err, context.Canceled) || sounds != 1 {
		t.Fatalf("custom threshold was not applied: sounds=%d err=%v", sounds, err)
	}
}

func TestWatchRejectsInvalidAlarmThreshold(t *testing.T) {
	for _, value := range []string{"-1", "100.1", "NaN", "+Inf", "-Inf", "bad"} {
		t.Run(value, func(t *testing.T) {
			cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
				t.Fatal("invalid threshold started a fetch")
				return nil, nil
			})
			cmd.SetArgs([]string{"watch", "--alarm-threshold=" + value})
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected threshold validation error")
			}
		})
	}
}

func TestWatchAlarmRemindsThatAutoResetIsOff(t *testing.T) {
	reminder := "ccodex: alarm: quota is at or below 10% remaining; automatic resets are off, so nothing will be spent (add --auto-reset to redeem an available earned reset)\n"
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{"monitoring only", nil, 2},
		{"auto-reset enabled", []string{"--auto-reset"}, 0},
		{"muted", []string{"--no-alarm"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			reads := 0
			cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
				if reads++; reads == 3 {
					cancel()
				}
				// 7% remaining with no reset available: low, but nothing to spend.
				return autoSnapshot(93, 0), nil
			}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
				t.Fatal("no reset was available")
				return nil, nil
			}, func(_ context.Context, w io.Writer) error {
				fmt.Fprint(w, "\a")
				return nil
			})
			cmd.SetOut(io.Discard)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"watch", "--interval", "1s", "--alarm-threshold", "10"}, test.args...))
			bounded, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if got := strings.Count(stderr.String(), reminder); got != test.want {
				t.Fatalf("reminders=%d, want %d: stderr=%q", got, test.want, stderr.String())
			}
			if test.want > 0 && !strings.HasPrefix(stderr.String(), "\a"+reminder) {
				t.Fatalf("reminder should follow the sound: stderr=%q", stderr.String())
			}
		})
	}
}
