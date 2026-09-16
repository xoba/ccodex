package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestBoundResetRejectsChangedOrUnavailableAccountBeforeConsume(t *testing.T) {
	email, otherEmail := "test@example.com", "other@example.com"
	for _, tc := range []struct {
		name     string
		scenario string
		expected *Account
	}{
		{"changed email", "reset-outcome-reset", &Account{Type: "chatgpt", Email: &otherEmail}},
		{"changed auth type", "reset-outcome-reset", &Account{Type: "apiKey", Email: &email}},
		{"expected nil email", "reset-outcome-reset", &Account{Type: "chatgpt"}},
		{"current nil email", "account-no-email", &Account{Type: "chatgpt", Email: &email}},
		{"logged out", "logged-out", &Account{Type: "chatgpt", Email: &email}},
		{"switched to API key", "api-key", &Account{Type: "chatgpt", Email: &email}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, tc.scenario)
			result, err := Reset(context.Background(), opts, ResetParams{
				IdempotencyKey: "bound-key", ExpectedAccount: tc.expected,
			})
			if result != nil || !errors.Is(err, ErrAccountChanged) || !strings.Contains(err.Error(), "reset was not attempted") {
				t.Fatalf("result = %+v, error = %v; want account identity error before consuming", result, err)
			}
			requests := readRequests(t, logPath)
			if len(requests) != 3 || requests[2].Method != "account/read" {
				t.Fatalf("unexpected request after account mismatch: %+v", requests)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestBoundResetRejectsChangedOrUnavailableAccountIDBeforeConsume(t *testing.T) {
	for _, tc := range []struct{ scenario, expectedID string }{
		{"reset-outcome-reset", "other-account"},
		{"reset-accountid-missing", "account-123"},
		{"reset-accountid-null", "account-123"},
		{"reset-accountid-blank", " "},
		{"limits-error", "account-123"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, tc.scenario)
			result, err := Reset(context.Background(), opts, ResetParams{
				IdempotencyKey: "bound-key", ExpectedAccountID: tc.expectedID,
			})
			if result != nil || !errors.Is(err, ErrAccountChanged) || !strings.Contains(err.Error(), "reset was not attempted") {
				t.Fatalf("result = %+v, error = %v; want account identity error before consuming", result, err)
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("remote error text leaked: %v", err)
			}
			requests := readRequests(t, logPath)
			if len(requests) != 4 || requests[3].Method != "account/rateLimits/read" {
				t.Fatalf("unexpected request after failed account-ID check: %+v", requests)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestBoundResetMatchingIdentityConsumesOnceAndOmitsExpectedFields(t *testing.T) {
	email := "test@example.com"
	for _, tc := range []struct {
		name     string
		scenario string
		expected *Account
	}{
		{"matches with changed plan", "reset-outcome-reset", &Account{Type: "chatgpt", Email: &email, PlanType: "plus"}},
		{"matches nil email using account ID", "account-no-email", &Account{Type: "chatgpt"}},
		{"matches using account ID alone", "reset-outcome-reset", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, tc.scenario)
			result, err := Reset(context.Background(), opts, ResetParams{
				IdempotencyKey: "bound-key", CreditID: "selected-credit",
				ExpectedAccount: tc.expected, ExpectedAccountID: "account-123",
			})
			if err != nil || result == nil || result.Outcome != "reset" {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
			requests := readRequests(t, logPath)
			var methods []string
			for _, request := range requests {
				methods = append(methods, request.Method)
			}
			want := []string{"initialize", "initialized", "account/read", "account/rateLimits/read", "account/rateLimitResetCredit/consume", "account/rateLimits/read"}
			if !reflect.DeepEqual(methods, want) {
				t.Fatalf("methods = %v, want %v", methods, want)
			}
			var payload map[string]any
			if err := json.Unmarshal(requests[4].Params, &payload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(payload, map[string]any{"idempotencyKey": "bound-key", "creditId": "selected-credit"}) {
				t.Fatalf("identity checks leaked into consume request: %s", requests[4].Params)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestBoundResetEmailOnlyDoesNotAddRateLimitPreflight(t *testing.T) {
	email := "test@example.com"
	opts, logPath, pidPath := setupHelper(t, "reset-outcome-reset")
	result, err := Reset(context.Background(), opts, ResetParams{
		IdempotencyKey: "bound-key", ExpectedAccount: &Account{Type: "chatgpt", Email: &email},
	})
	if err != nil || result == nil || result.Outcome != "reset" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	assertResetMethods(t, readRequests(t, logPath), true)
	assertHelperStopped(t, pidPath)
}
