package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"xoba.com/ccodex/internal/codex"
)

func resetPreviewSnapshot(t *testing.T) *codex.Snapshot {
	t.Helper()
	snapshot := exampleSnapshot(t)
	var credits codex.ResetCreditsSummary
	if err := json.Unmarshal([]byte(`{"availableCount":2,"credits":[{"id":"reset_1","status":"available","resetType":"codexRateLimits","grantedAt":0,"expiresAt":1790000000,"title":"Earned reset"}]}`), &credits); err != nil {
		t.Fatal(err)
	}
	snapshot.RateLimits.RateLimitResetCredits = &credits
	return snapshot
}

func TestResetDryRunNeverConsumes(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var out, stderr bytes.Buffer
		reads := 0
		cmd := newCommandWithReset(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			reads++
			return resetPreviewSnapshot(t), nil
		}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
			t.Fatal("dry-run consumed a reset")
			return nil, nil
		})
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		args := []string{"reset", "--dry-run", "--credit-id", "reset_1"}
		if asJSON {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if reads != 1 || stderr.Len() != 0 {
			t.Fatalf("unexpected operations reads=%d stderr=%s", reads, stderr.String())
		}
		if asJSON {
			var result struct {
				DryRun   bool           `json:"dryRun"`
				CreditID string         `json:"creditId"`
				Snapshot codex.Snapshot `json:"snapshot"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.DryRun || result.CreditID != "reset_1" || result.Snapshot.RateLimits.RateLimitResetCredits.AvailableCount != 2 {
				t.Fatalf("invalid preview: %s", out.String())
			}
		} else {
			for _, want := range []string{"Dry run: no reset requested.", "Available rate-limit resets: 2", "reset_1", "Earned reset", "Expires:", "count above is authoritative"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("preview missing %q: %s", want, out.String())
				}
			}
		}
	}
}

func TestResetRecordsGeneratedKeyBeforeSending(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	var gotParams codex.ResetParams
	cmd := newCommandWithReset(nil, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		calls++
		gotParams = params
		if opts.Binary != "/custom/codex" || opts.Timeout != 30*time.Second {
			t.Fatalf("options not forwarded: %+v", opts)
		}
		if !strings.Contains(stderr.String(), params.IdempotencyKey) {
			t.Fatal("key not recorded before consuming")
		}
		return &codex.ResetResult{Outcome: "reset", IdempotencyKey: params.IdempotencyKey}, nil
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"reset", "--json", "--codex-bin", "/custom/codex", "--timeout", "30s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(gotParams.IdempotencyKey) {
		t.Fatalf("bad request count/key: calls=%d params=%+v", calls, gotParams)
	}
	var result codex.ResetResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "reset" || result.IdempotencyKey != gotParams.IdempotencyKey {
		t.Fatalf("missing outcome/key: %s", stdout.String())
	}
}

func TestResetOutcomesAndExplicitRetryKey(t *testing.T) {
	for outcome, message := range map[string]string{
		"reset":           "One earned reset was consumed",
		"alreadyRedeemed": "No additional reset was consumed",
		"nothingToReset":  "no quota window is currently eligible",
		"noCredit":        "no eligible earned reset credit is available for this request",
	} {
		t.Run(outcome, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := newCommandWithReset(nil, func(ctx context.Context, opts codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
				if params.IdempotencyKey != "retry-123" || params.CreditID != "reset_1" {
					t.Fatalf("request params lost: %+v", params)
				}
				return &codex.ResetResult{Outcome: outcome, IdempotencyKey: params.IdempotencyKey, Warnings: []string{"Could not refresh quota; run ccodex status."}}, nil
			})
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"reset", "--idempotency-key", "retry-123", "--credit-id", "reset_1"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), message) || !strings.Contains(stdout.String(), "Could not refresh quota") {
				t.Fatalf("outcome/refresh warning lost: %s", stdout.String())
			}
		})
	}
}

func TestResetErrorsPreserveKeyAndDoNotRetry(t *testing.T) {
	for _, failure := range []error{errors.New("response lost"), context.Canceled} {
		var stdout, stderr bytes.Buffer
		calls := 0
		cmd := newCommandWithReset(nil, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
			calls++
			return nil, failure
		})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"reset", "--idempotency-key", "retry-123", "--json"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `--idempotency-key "retry-123"`) {
			t.Fatalf("error lost retry key: %v", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatal("ambiguous cancellation would be swallowed by main")
		}
		if calls != 1 || stdout.Len() != 0 {
			t.Fatalf("unexpected retry or successful output: calls=%d stdout=%s", calls, stdout.String())
		}
	}
}

func TestResetInvalidFlagsCannotConsume(t *testing.T) {
	for _, args := range [][]string{
		{"reset", "extra"}, {"reset", "--idempotency-key="},
		{"reset", "--credit-id="}, {"reset", "--idempotency-key", "  "},
		{"reset", "--idempotency-key", "bad\nkey"}, {"reset", "--timeout", "0s"},
	} {
		cmd := newCommandWithReset(nil, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
			t.Fatal("invalid flags consumed a reset")
			return nil, nil
		})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}

func TestResetCannotProceedWhenKeyCannotBeRecorded(t *testing.T) {
	cmd := newCommandWithReset(nil, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("consumed before recording request ID")
		return nil, nil
	})
	cmd.SetErr(failingWriter{})
	cmd.SetArgs([]string{"reset"})
	if err := cmd.Execute(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected output error, got %v", err)
	}
}

func TestResetResultRendersRefreshedQuotaWithoutTokenPlaceholder(t *testing.T) {
	snapshot := resetPreviewSnapshot(t)
	var out bytes.Buffer
	result := &codex.ResetResult{
		Outcome: "reset", IdempotencyKey: "retry-123", FetchedAt: snapshot.FetchedAt,
		Account: snapshot.Account, RateLimits: snapshot.RateLimits,
	}
	if err := renderResetResult(&out, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Primary (5h)") || strings.Contains(out.String(), "Token activity") {
		t.Fatalf("unexpected reset report: %s", out.String())
	}
}
