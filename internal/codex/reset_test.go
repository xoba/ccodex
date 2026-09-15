package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResetOutcomesConsumeOnceThenRefresh(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed", "nothingToReset", "noCredit"} {
		t.Run(outcome, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, "reset-outcome-"+outcome)
			params := ResetParams{IdempotencyKey: "03c7915a-bc31-4010-a4dd-8072b339c2a9"}
			before := time.Now()
			result, err := Reset(context.Background(), opts, params)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != outcome || result.IdempotencyKey != params.IdempotencyKey || result.Account == nil || result.Account.Type != "chatgpt" {
				t.Fatalf("unexpected reset result: %+v", result)
			}
			if result.RateLimits == nil || len(result.Warnings) != 0 || result.FetchedAt.Before(before) || result.FetchedAt.After(time.Now()) {
				t.Fatalf("missing follow-up limits or invalid timestamp: %+v", result)
			}
			requests := readRequests(t, logPath)
			assertResetMethods(t, requests, true)
			if string(requests[3].Params) != `{"idempotencyKey":"03c7915a-bc31-4010-a4dd-8072b339c2a9"}` {
				t.Fatalf("unexpected consume payload: %s", requests[3].Params)
			}
			if string(requests[2].Params) != `{"refreshToken":false}` {
				t.Fatalf("unexpected account read: %s", requests[2].Params)
			}
			credits := result.RateLimits.RateLimitResetCredits
			if credits == nil || len(credits.Credits) != 1 || credits.Credits[0].ID != "credit-123" || credits.Credits[0].ResetType != "codexRateLimits" || credits.Credits[0].Status != "available" {
				t.Fatalf("reset-credit details were lost: %+v", credits)
			}
			credit := credits.Credits[0]
			if credit.GrantedAt != 1789000000 || credit.ExpiresAt == nil || *credit.ExpiresAt != 1791000000 || credit.Title == nil || *credit.Title != "Earned reset" || credit.Description != nil {
				t.Fatalf("unexpected reset-credit metadata: %+v", credit)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestResetPassesSelectedCreditAndOpaqueKeyUnchanged(t *testing.T) {
	opts, logPath, pidPath := setupHelper(t, "reset-outcome-reset")
	params := ResetParams{IdempotencyKey: "chosen-key:abc/123", CreditID: "credit-123"}
	result, err := Reset(context.Background(), opts, params)
	if err != nil {
		t.Fatal(err)
	}
	requests := readRequests(t, logPath)
	assertResetMethods(t, requests, true)
	var actual ResetParams
	if err := json.Unmarshal(requests[3].Params, &actual); err != nil {
		t.Fatal(err)
	}
	if actual != params || result.IdempotencyKey != params.IdempotencyKey {
		t.Fatalf("consume parameters changed: %+v, want %+v", actual, params)
	}
	assertHelperStopped(t, pidPath)
}

func TestResetValidatesBeforeStartingAppServer(t *testing.T) {
	for _, params := range []ResetParams{
		{},
		{IdempotencyKey: " \n\t"},
		{IdempotencyKey: "valid-key", CreditID: " \t"},
	} {
		t.Run(params.IdempotencyKey+params.CreditID, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, "reset-outcome-reset")
			if _, err := Reset(context.Background(), opts, params); err == nil {
				t.Fatal("expected parameter validation error")
			}
			for _, path := range []string{logPath, pidPath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("app-server started with invalid parameters: %v", err)
				}
			}
		})
	}
}

func TestResetRequiresChatGPTAuthenticationWithoutConsuming(t *testing.T) {
	for _, scenario := range []string{"logged-out", "api-key"} {
		t.Run(scenario, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, scenario)
			_, err := Reset(context.Background(), opts, ResetParams{IdempotencyKey: "test-key"})
			if err == nil || !strings.Contains(err.Error(), "codex login") {
				t.Fatalf("expected login guidance, got %v", err)
			}
			requests := readRequests(t, logPath)
			if len(requests) != 3 || requests[2].Method != "account/read" {
				t.Fatalf("made requests after unavailable authentication: %+v", requests)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestResetPreservesConfirmedOutcomeWhenRefreshFails(t *testing.T) {
	for _, scenario := range []string{"reset-refresh-error", "reset-refresh-missing", "reset-refresh-exit", "reset-refresh-hang"} {
		t.Run(scenario, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, scenario)
			if scenario == "reset-refresh-hang" {
				opts.Timeout = 300 * time.Millisecond
			}
			result, err := Reset(context.Background(), opts, ResetParams{IdempotencyKey: "retry-key"})
			if err != nil {
				t.Fatalf("lost confirmed reset outcome: %v", err)
			}
			if result.Outcome != "reset" || result.IdempotencyKey != "retry-key" || result.RateLimits != nil || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "outcome is confirmed") {
				t.Fatalf("unexpected partial result: %+v", result)
			}
			if strings.Contains(result.Warnings[0], "private-marker") {
				t.Fatalf("remote error text leaked into warning: %s", result.Warnings[0])
			}
			assertResetMethods(t, readRequests(t, logPath), true)
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestResetPreservesConfirmedOutcomeWhenRefreshIsCanceled(t *testing.T) {
	opts, logPath, pidPath := setupHelper(t, "reset-refresh-hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type response struct {
		result *ResetResult
		err    error
	}
	completed := make(chan response, 1)
	go func() {
		result, err := Reset(ctx, opts, ResetParams{IdempotencyKey: "retry-key"})
		completed <- response{result: result, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "account/rateLimits/read") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not receive follow-up rate-limit request")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-completed:
		if got.err != nil || got.result == nil || got.result.Outcome != "reset" || len(got.result.Warnings) != 1 {
			t.Fatalf("cancellation lost confirmed reset outcome: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop follow-up request")
	}
	assertHelperStopped(t, pidPath)
}

func TestResetAmbiguousFailuresNeverRetry(t *testing.T) {
	for _, scenario := range []string{
		"reset-consume-error", "reset-consume-exit", "reset-consume-hang",
		"reset-consume-unknown", "reset-consume-missing", "reset-consume-malformed",
	} {
		t.Run(scenario, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, scenario)
			if scenario == "reset-consume-hang" {
				opts.Timeout = 300 * time.Millisecond
			}
			result, err := Reset(context.Background(), opts, ResetParams{IdempotencyKey: "retry-key"})
			if result != nil || err == nil || !strings.Contains(err.Error(), "outcome may be unknown") || !strings.Contains(err.Error(), "same idempotency key") {
				t.Fatalf("result = %+v, error = %v; want explicit uncertainty and safe retry guidance", result, err)
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("remote text leaked into error: %v", err)
			}
			if scenario == "reset-consume-hang" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error lost context deadline: %v", err)
			}
			assertResetMethods(t, readRequests(t, logPath), false)
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestResetCreditsPreserveUnknownAndEmptyDetails(t *testing.T) {
	for _, tc := range []struct {
		json       string
		nilCredits bool
	}{
		{`{"availableCount":2,"credits":null}`, true},
		{`{"availableCount":0,"credits":[]}`, false},
	} {
		var credits ResetCreditsSummary
		if err := json.Unmarshal([]byte(tc.json), &credits); err != nil {
			t.Fatal(err)
		}
		if (credits.Credits == nil) != tc.nilCredits {
			t.Fatalf("lost unknown-vs-empty distinction: %+v", credits)
		}
		encoded, err := json.Marshal(credits)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tc.json {
			t.Fatalf("JSON = %s, want %s", encoded, tc.json)
		}
	}
}

func assertResetMethods(t *testing.T, requests []helperRequest, refresh bool) {
	t.Helper()
	var methods []string
	for _, req := range requests {
		methods = append(methods, req.Method)
	}
	want := []string{"initialize", "initialized", "account/read", "account/rateLimitResetCredit/consume"}
	if refresh {
		want = append(want, "account/rateLimits/read")
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}
