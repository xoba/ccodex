package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/xoba/ccodex/internal/codex"
	"github.com/xoba/ccodex/internal/resetbudget"
)

func newWatchResetTestCommand(t *testing.T, fetch fetchFunc, reset resetFunc, alarm alarmFunc) *cobra.Command {
	t.Helper()
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	return newCommandWithBudget(fetch, reset, alarm, func() (autoResetBudget, error) { return store, nil })
}

func autoSnapshot(used float64, credits int64) *codex.Snapshot {
	snapshot := quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(used)})
	snapshot.Account = &codex.Account{Type: "chatgpt", PlanType: "pro"}
	accountID := "test-account"
	snapshot.RateLimits.AccountID = &accountID
	snapshot.FetchedAt = time.Now().UTC()
	snapshot.RateLimits.RateLimitResetCredits = &codex.ResetCreditsSummary{AvailableCount: credits}
	return snapshot
}

func recoveredReset(params codex.ResetParams, credits int64) *codex.ResetResult {
	healthy := autoSnapshot(0, credits)
	return &codex.ResetResult{
		Outcome: "reset", IdempotencyKey: params.IdempotencyKey,
		Account: healthy.Account, RateLimits: healthy.RateLimits, FetchedAt: healthy.FetchedAt,
	}
}

func TestWatchAutoResetOptInRecoversAndRearms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	reads, sounds := 0, 0
	var keys []string
	cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
		// Reads 1 and 4 are low polls, each confirmed by the next read before
		// its reset is sent; read 3 is the healthy poll that rearms watch.
		reads++
		if reads == 3 {
			return autoSnapshot(0, 1), nil
		}
		return autoSnapshot(97, 2), nil
	}, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		if !strings.Contains(stderr.String(), params.IdempotencyKey) {
			t.Fatal("automatic reset key was not recorded before request")
		}
		keys = append(keys, params.IdempotencyKey)
		if len(keys) == 2 {
			cancel()
		}
		return recoveredReset(params, int64(2-len(keys))), nil
	}, func(context.Context, io.Writer) error {
		sounds++
		return nil
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--auto-reset", "--json", "--interval", "1s", "--max-resets-per-day", "2"})
	bounded, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
		t.Fatalf("watch failed: %v", err)
	}
	if reads != 5 || sounds != 2 || len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("reads=%d sounds=%d keys=%v", reads, sounds, keys)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 3 polls and 2 refreshed snapshots, got %d", len(lines))
	}
	for i, line := range lines {
		var snapshot codex.Snapshot
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil {
			t.Fatalf("non-snapshot JSON output: %s", line)
		}
		if (i == 1 || i == 4) && hasLowQuota(&snapshot, 5) {
			t.Fatal("post-reset JSON did not show the refreshed quota")
		}
	}
}

func TestOnlyOneAutoResetWatcherRunsAtATime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "auto-resets.json")
	run := func(ctx context.Context, fetch fetchFunc, args ...string) error {
		cmd := newCommandWithBudget(fetch, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
			t.Error("healthy quota was reset")
			return nil, errors.New("unexpected reset")
		}, nil, func() (autoResetBudget, error) { return resetbudget.Store{Path: path}, nil })
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"watch", "--interval", "1s"}, args...))
		return cmd.ExecuteContext(ctx)
	}
	// runUntilFirstPoll reports how a watcher ends once it has been admitted.
	runUntilFirstPoll := func(args ...string) error {
		watchCtx, stop := context.WithCancel(ctx)
		defer stop()
		return run(watchCtx, func(context.Context, codex.Options) (*codex.Snapshot, error) {
			stop()
			return autoSnapshot(10, 1), nil
		}, args...)
	}

	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	polled, firstDone := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		firstDone <- run(firstCtx, func(context.Context, codex.Options) (*codex.Snapshot, error) {
			select {
			case polled <- struct{}{}:
			default:
			}
			return autoSnapshot(10, 1), nil
		}, "--auto-reset")
	}()
	select {
	case <-polled:
	case <-ctx.Done():
		t.Fatal("first watcher never polled")
	}

	err := run(ctx, func(context.Context, codex.Options) (*codex.Snapshot, error) {
		t.Error("refused watcher polled anyway")
		return nil, errors.New("unexpected poll")
	}, "--auto-reset")
	if err == nil || !strings.Contains(err.Error(), "another ccodex watch --auto-reset is already running") {
		t.Fatalf("second automatic-reset watcher: %v", err)
	}
	// Watchers that cannot spend a reset are not affected.
	for _, args := range [][]string{nil, {"--auto-reset=false"}, {"--auto-reset", "--max-resets-per-day", "0"}} {
		if err := runUntilFirstPoll(args...); !errors.Is(err, context.Canceled) {
			t.Fatalf("watch %v beside an automatic-reset watcher: %v", args, err)
		}
	}
	stopFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first watcher: %v", err)
	}
	if err := runUntilFirstPoll("--auto-reset"); !errors.Is(err, context.Canceled) {
		t.Fatalf("automatic-reset watcher after the first one exited: %v", err)
	}
}

func TestWatchReadOnlyKeepsAlarmWithoutOpeningResetBudget(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"default", []string{"watch", "--interval", "1s"}},
		{"explicit opt-out", []string{"watch", "--auto-reset=false", "--interval", "1s"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads, sounds := 0, 0
			cmd := newCommandWithBudget(func(context.Context, codex.Options) (*codex.Snapshot, error) {
				reads++
				if reads == 2 {
					cancel()
				}
				return autoSnapshot(99, 2), nil
			}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
				t.Fatal("read-only watch redeemed a credit")
				return nil, nil
			}, func(context.Context, io.Writer) error {
				sounds++
				return nil
			}, func() (autoResetBudget, error) {
				t.Fatal("read-only watch opened automatic reset accounting")
				return nil, nil
			})
			cmd.SetOut(io.Discard)
			cmd.SetArgs(test.args)
			bounded, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) || reads != 2 || sounds != 1 {
				t.Fatalf("unexpected monitoring behavior: reads=%d sounds=%d err=%v", reads, sounds, err)
			}
		})
	}
}

func TestWatchAutoResetWorksWhenSoundMutedOrFails(t *testing.T) {
	for _, muted := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		resets, sounds := 0, 0
		var stderr bytes.Buffer
		cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
			// 7% remaining: reset must honor the custom threshold of 10.
			return autoSnapshot(93, 1), nil
		}, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
			resets++
			cancel()
			return recoveredReset(params, 0), nil
		}, func(context.Context, io.Writer) error {
			sounds++
			return errors.New("sound unavailable")
		})
		cmd.SetOut(io.Discard)
		cmd.SetErr(&stderr)
		args := []string{"watch", "--auto-reset", "--alarm-threshold", "10"}
		if muted {
			args = append(args, "--no-alarm")
		}
		cmd.SetArgs(args)
		bounded, stop := context.WithTimeout(ctx, time.Second)
		err := cmd.ExecuteContext(bounded)
		stop()
		cancel()
		if !errors.Is(err, context.Canceled) || resets != 1 || (muted && sounds != 0) || (!muted && sounds != 1) {
			t.Fatalf("muted=%v resets=%d sounds=%d err=%v", muted, resets, sounds, err)
		}
		if !muted && !strings.Contains(stderr.String(), "sound unavailable") {
			t.Fatal("sound failure was not reported")
		}
	}
}

func TestWatchAutoResetRetriesAmbiguousAttemptWithSameKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	reads := 0
	var keys []string
	cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
		reads++
		return autoSnapshot(99, int64(2-reads)), nil
	}, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		keys = append(keys, params.IdempotencyKey)
		if len(keys) == 1 {
			return nil, errors.New("response lost")
		}
		cancel()
		result := recoveredReset(params, 0)
		result.Outcome = "alreadyRedeemed"
		return result, nil
	}, nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--auto-reset", "--interval", "1s"})
	bounded, stop := context.WithTimeout(ctx, 4*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
		t.Fatalf("watch failed: %v", err)
	}
	if len(keys) != 2 || keys[0] != keys[1] || !strings.Contains(stderr.String(), "response lost") || !strings.Contains(stderr.String(), "No additional reset") {
		t.Fatalf("ambiguous attempt was not safely retried: keys=%v stderr=%s", keys, stderr.String())
	}
}

func TestInterruptedAutomaticResetReportsUnknownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	cmd := newWatchResetTestCommand(t, func(context.Context, codex.Options) (*codex.Snapshot, error) {
		return autoSnapshot(99, 1), nil
	}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		cancel()
		return nil, context.Canceled
	}, nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--auto-reset"})
	err := cmd.ExecuteContext(ctx)
	if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "outcome may be unknown") || !strings.Contains(err.Error(), "--idempotency-key") {
		t.Fatalf("interrupted reset lost retry guidance: %v", err)
	}
}

func TestAutomaticResetRequiresRecordingRequestID(t *testing.T) {
	var state autoResetState
	_, err := applyAutoReset(context.Background(), &state, autoSnapshot(99, 1), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("automatic reset consumed without recording request ID")
		return nil, nil
	}, resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}, 1, failingWriter{}, nil, nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected output error, got %v", err)
	}
}

func TestWatchDailyLimitDefaultsToOneAcrossRestarts(t *testing.T) {
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	resets := 0
	for run := 0; run < 2; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		var stderr bytes.Buffer
		cmd := newCommandWithBudget(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			return autoSnapshot(99, 2), nil
		}, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
			resets++
			cancel()
			return recoveredReset(params, 1), nil
		}, nil, func() (autoResetBudget, error) { return store, nil })
		cmd.SetOut(io.Discard)
		cmd.SetErr(writeFunc(func(p []byte) (int, error) {
			n, err := stderr.Write(p)
			if strings.Contains(string(p), "daily limit of 1 reached") {
				cancel()
			}
			return n, err
		}))
		cmd.SetArgs([]string{"watch", "--auto-reset"})
		bounded, stop := context.WithTimeout(ctx, 2*time.Second)
		err := cmd.ExecuteContext(bounded)
		stop()
		cancel()
		if !errors.Is(err, context.Canceled) || resets != 1 {
			t.Fatalf("run=%d resets=%d err=%v stderr=%s", run, resets, err, stderr.String())
		}
		if run == 1 && !strings.Contains(stderr.String(), "daily limit of 1 reached") {
			t.Fatal("restart did not enforce the saved default daily limit")
		}
	}
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

func TestWatchZeroDailyLimitDisablesResetsButKeepsAlarm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sounds := 0
	cmd := newCommandWithBudget(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		return autoSnapshot(99, 2), nil
	}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("zero daily limit sent a reset")
		return nil, nil
	}, func(context.Context, io.Writer) error {
		sounds++
		cancel()
		return nil
	}, func() (autoResetBudget, error) {
		t.Fatal("zero daily limit opened automatic reset accounting")
		return nil, nil
	})
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"watch", "--auto-reset", "--max-resets-per-day", "0"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) || sounds != 1 {
		t.Fatalf("sounds=%d err=%v", sounds, err)
	}
}

func TestWatchRejectsNegativeDailyLimit(t *testing.T) {
	cmd := newCommand(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		t.Fatal("invalid daily limit fetched account data")
		return nil, nil
	})
	cmd.SetArgs([]string{"watch", "--max-resets-per-day=-1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--max-resets-per-day") {
		t.Fatalf("expected daily limit validation, got %v", err)
	}
}

func TestAutomaticResetAccountingFailureFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-resets.json")
	if err := os.WriteFile(path, []byte("corrupt accounting"), 0600); err != nil {
		t.Fatal(err)
	}
	var state autoResetState
	var stderr bytes.Buffer
	updated, err := applyAutoReset(context.Background(), &state, autoSnapshot(99, 1), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("corrupt accounting allowed automatic reset")
		return nil, nil
	}, resetbudget.Store{Path: path}, 1, &stderr, nil, nil)
	if err != nil || updated != nil || state.params.IdempotencyKey != "" || !strings.Contains(stderr.String(), "could not read daily limit") {
		t.Fatalf("accounting failure did not preserve monitoring: updated=%v err=%v stderr=%s", updated, err, stderr.String())
	}
}

func TestAutoResetRetryOutputFailureKeepsPendingReservation(t *testing.T) {
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	var state autoResetState
	var stderr bytes.Buffer
	calls := 0
	reset := func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		calls++
		return nil, errors.New("lost response")
	}
	if _, err := applyAutoReset(context.Background(), &state, autoSnapshot(99, 1), 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := applyAutoReset(context.Background(), &state, autoSnapshot(99, 0), 5, codex.Options{}, reset, store, 1, failingWriter{}, nil, nil)
	if !errors.Is(err, io.ErrClosedPipe) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	allowed, err := store.Reserve(context.Background(), "another-attempt", 1, time.Now())
	if err != nil || allowed {
		t.Fatalf("uncertain request's daily slot was released: allowed=%v err=%v", allowed, err)
	}
}

func TestAutoResetResumesSavedPendingForSameAccount(t *testing.T) {
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	var original, restarted autoResetState
	var stderr bytes.Buffer
	var keys []string
	reset := func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		if params.ExpectedAccountID != "test-account" || params.ExpectedAccount == nil {
			t.Fatal("automatic request is not bound to the observed account")
		}
		keys = append(keys, params.IdempotencyKey)
		if len(keys) == 1 {
			return nil, errors.New("lost response")
		}
		result := recoveredReset(params, 0)
		result.Outcome = "alreadyRedeemed"
		return result, nil
	}
	if _, err := applyAutoReset(context.Background(), &original, autoSnapshot(99, 1), 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAutoReset(context.Background(), &restarted, autoSnapshot(99, 0), 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != keys[1] || restarted.outcome != autoResetConsumed {
		t.Fatalf("pending request not resumed: keys=%v state=%+v", keys, restarted)
	}
}

func TestResumedPendingOutputFailureCannotReleaseEarlierReservation(t *testing.T) {
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	var original, restarted autoResetState
	var stderr bytes.Buffer
	if _, err := applyAutoReset(context.Background(), &original, autoSnapshot(99, 1), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		return nil, errors.New("lost response")
	}, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := applyAutoReset(context.Background(), &restarted, autoSnapshot(99, 0), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("resumed request dispatched despite output failure")
		return nil, nil
	}, store, 1, failingWriter{}, nil, nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected output failure, got %v", err)
	}
	allowed, err := store.Reserve(context.Background(), "another-attempt", 1, time.Now())
	if err != nil || allowed {
		t.Fatalf("earlier uncertain redemption lost its reservation: allowed=%v err=%v", allowed, err)
	}
}

func TestAutoResetAccountChangeCannotReuseAnotherAccountsKey(t *testing.T) {
	store := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	var state autoResetState
	var stderr bytes.Buffer
	calls := 0
	reset := func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		calls++
		return nil, errors.New("lost response")
	}
	if _, err := applyAutoReset(context.Background(), &state, autoSnapshot(99, 1), 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	other := autoSnapshot(99, 1)
	otherID := "different-account"
	other.RateLimits.AccountID = &otherID
	if _, err := applyAutoReset(context.Background(), &state, other, 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	var restarted autoResetState
	if _, err := applyAutoReset(context.Background(), &restarted, other, 5, codex.Options{}, reset, store, 1, &stderr, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(stderr.String(), "account changed") || !strings.Contains(stderr.String(), "daily limit of 1 reached") {
		t.Fatalf("account-scoped key bypassed cap: calls=%d stderr=%s", calls, stderr.String())
	}
}
