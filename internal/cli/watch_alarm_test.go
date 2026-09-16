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

	"xoba.com/ccodex/internal/codex"
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
			"codex": {Primary: quotaWindow(96), Secondary: quotaWindow(100)},
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
	if reads != 2 || sounds != 2 || stderr.String() != "\a\a" {
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
		{"exact threshold", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(95)}), false},
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
			return quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(96)}), nil
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
	// Bound the test in case a regression keeps the default threshold of 5.
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
