package cli

import (
	"errors"
	"math"
	"testing"

	"xoba.com/ccodex/internal/codex"
)

func autoResetSnapshot(used float64, credits int64) *codex.Snapshot {
	snapshot := quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(used)})
	snapshot.RateLimits.RateLimitResetCredits = &codex.ResetCreditsSummary{AvailableCount: credits}
	return snapshot
}

func TestAutoResetStartsOnlyBelowThresholdWithKnownCredit(t *testing.T) {
	unknownCredit := autoResetSnapshot(100, 1)
	unknownCredit.RateLimits.RateLimitResetCredits = nil
	for _, test := range []struct {
		name      string
		snapshot  *codex.Snapshot
		threshold float64
		want      bool
	}{
		{"nil snapshot", nil, 5, false},
		{"missing limits", &codex.Snapshot{}, 5, false},
		{"unknown quota", quotaSnapshot(codex.RateLimitSnapshot{}), 5, false},
		{"unknown credits", unknownCredit, 5, false},
		{"zero credits", autoResetSnapshot(100, 0), 5, false},
		{"negative credits", autoResetSnapshot(100, -1), 5, false},
		{"healthy", autoResetSnapshot(20, 2), 5, false},
		{"exact threshold", autoResetSnapshot(95, 2), 5, false},
		{"low", autoResetSnapshot(95.01, 2), 5, true},
		{"custom threshold", autoResetSnapshot(93, 2), 10, true},
		{"zero threshold", autoResetSnapshot(100, 2), 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state autoResetState
			params, attempt := state.next(test.snapshot, test.threshold)
			if attempt != test.want || (params.IdempotencyKey != "") != test.want {
				t.Fatalf("next = (%+v, %v), want attempt=%v", params, attempt, test.want)
			}
		})
	}
}

func TestAutoResetConsumesOneRequestUntilVerifiedRecovery(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed"} {
		t.Run(outcome, func(t *testing.T) {
			low := autoResetSnapshot(99, 2)
			low.RateLimits.RateLimits.Secondary = quotaWindow(100)
			low.RateLimits.RateLimits.IndividualLimit = &codex.SpendControlLimitSnapshot{RemainingPercent: 1}
			var state autoResetState
			first, attempt := state.next(low, 5)
			if !attempt {
				t.Fatal("expected first reset")
			}
			healthy := autoResetSnapshot(95, 1)
			healthy.RateLimits.RateLimits.Secondary = quotaWindow(80)
			healthy.RateLimits.RateLimits.IndividualLimit = &codex.SpendControlLimitSnapshot{RemainingPercent: 5}
			// The follow-up read is already healthy, but only an ordinary
			// watch fetch may confirm recovery and permit another request.
			state.record(&codex.ResetResult{Outcome: outcome, RateLimits: healthy.RateLimits}, nil)
			for range 3 {
				if _, attempt := state.next(low, 5); attempt {
					t.Fatal("persistent low quota consumed another request")
				}
			}
			partial := autoResetSnapshot(0, 1)
			partial.RateLimits.RateLimits.Secondary = quotaWindow(96)
			partial.RateLimits.RateLimits.IndividualLimit = &codex.SpendControlLimitSnapshot{RemainingPercent: 100}
			if _, attempt := state.next(partial, 5); attempt {
				t.Fatal("partial recovery allowed another request")
			}
			if _, attempt := state.next(healthy, 5); attempt {
				t.Fatal("healthy fetch should not reset")
			}
			second, attempt := state.next(low, 5)
			if !attempt || second.IdempotencyKey == first.IdempotencyKey || second.IdempotencyKey == "" {
				t.Fatalf("new low episode did not get a new key: first=%+v second=%+v attempt=%v", first, second, attempt)
			}
		})
	}
}

func TestAutoResetMissingTriggerDoesNotRearm(t *testing.T) {
	past := int64(0)
	for _, test := range []struct {
		name string
		read *codex.Snapshot
	}{
		{"nil snapshot", nil},
		{"missing limits", &codex.Snapshot{}},
		{"missing window", quotaSnapshot(codex.RateLimitSnapshot{Secondary: quotaWindow(0)})},
		{"missing percentage", quotaSnapshot(codex.RateLimitSnapshot{Primary: &codex.RateLimitWindow{}})},
		{"past reset without percentage", quotaSnapshot(codex.RateLimitSnapshot{Primary: &codex.RateLimitWindow{ResetsAt: &past}})},
		{"nan percentage", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(math.NaN())})},
		{"infinite percentage", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(math.Inf(1))})},
		{"different bucket", &codex.Snapshot{RateLimits: &codex.RateLimitsResponse{RateLimitsByLimitID: map[string]codex.RateLimitSnapshot{"other": {Primary: quotaWindow(0)}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state autoResetState
			low := autoResetSnapshot(99, 2)
			state.next(low, 5)
			state.record(&codex.ResetResult{Outcome: "reset"}, nil)
			if _, attempt := state.next(test.read, 5); attempt {
				t.Fatal("missing trigger caused a reset")
			}
			if _, attempt := state.next(low, 5); attempt {
				t.Fatal("missing data rearmed the request")
			}
		})
	}
}

func TestAutoResetRecoveryRequiresAllTriggersAndNoOtherLowQuota(t *testing.T) {
	low := autoResetSnapshot(0, 2)
	low.RateLimits.RateLimitsByLimitID = map[string]codex.RateLimitSnapshot{
		"codex": {Primary: quotaWindow(99), Secondary: quotaWindow(98)},
		"other": {IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 1}},
	}
	for _, test := range []struct {
		name    string
		buckets map[string]codex.RateLimitSnapshot
	}{
		{"omitted low bucket", map[string]codex.RateLimitSnapshot{
			"codex": {Primary: quotaWindow(0), Secondary: quotaWindow(0)},
		}},
		{"omitted low secondary", map[string]codex.RateLimitSnapshot{
			"codex": {Primary: quotaWindow(0)},
			"other": {IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 100}},
		}},
		{"omitted spend limit", map[string]codex.RateLimitSnapshot{
			"codex": {Primary: quotaWindow(0), Secondary: quotaWindow(0)},
			"other": {Primary: quotaWindow(0)},
		}},
		{"new low bucket", map[string]codex.RateLimitSnapshot{
			"codex": {Primary: quotaWindow(0), Secondary: quotaWindow(0)},
			"other": {IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 100}},
			"new":   {Primary: quotaWindow(99)},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state autoResetState
			state.next(low, 5)
			state.record(&codex.ResetResult{Outcome: "reset"}, nil)
			read := autoResetSnapshot(0, 1)
			read.RateLimits.RateLimitsByLimitID = test.buckets
			if _, attempt := state.next(read, 5); attempt {
				t.Fatal("unverified recovery caused a reset")
			}
			if _, attempt := state.next(low, 5); attempt {
				t.Fatal("unverified recovery rearmed the request")
			}
		})
	}
}

func TestAutoResetUnknownOutcomePreservesKeyAcrossHealthyOrMissingReads(t *testing.T) {
	for _, test := range []struct {
		name   string
		result *codex.ResetResult
		err    error
	}{
		{"transport error", nil, errors.New("connection closed")},
		{"nil result", nil, nil},
		{"unknown outcome", &codex.ResetResult{Outcome: "futureOutcome"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state autoResetState
			first, _ := state.next(autoResetSnapshot(99, 1), 5)
			state.record(test.result, test.err)
			for _, read := range []*codex.Snapshot{nil, autoResetSnapshot(0, 0), &codex.Snapshot{}} {
				if _, attempt := state.next(read, 5); attempt {
					t.Fatal("unknown or healthy quota should not trigger a reset")
				}
			}
			for _, knownCredits := range []bool{true, false} {
				low := autoResetSnapshot(99, 0)
				if !knownCredits {
					low.RateLimits.RateLimitResetCredits = nil
				}
				retry, attempt := state.next(low, 5)
				if !attempt || retry != first {
					t.Fatalf("unknown outcome lost its key: first=%+v retry=%+v attempt=%v", first, retry, attempt)
				}
				state.record(nil, errors.New("still unresolved"))
			}
			state.record(&codex.ResetResult{Outcome: "alreadyRedeemed"}, nil)
			if _, attempt := state.next(autoResetSnapshot(99, 1), 5); attempt {
				t.Fatal("confirmed retry allowed another reset")
			}
		})
	}
}

func TestAutoResetNoOpRetriesSameKeyWhileEligible(t *testing.T) {
	for _, outcome := range []string{"nothingToReset", "noCredit"} {
		t.Run(outcome, func(t *testing.T) {
			var state autoResetState
			low := autoResetSnapshot(99, 2)
			first, _ := state.next(low, 5)
			state.record(&codex.ResetResult{Outcome: outcome}, nil)
			if _, attempt := state.next(autoResetSnapshot(99, 0), 5); attempt {
				t.Fatal("no-op should wait for an available credit")
			}
			if _, attempt := state.next(nil, 5); attempt {
				t.Fatal("unknown quota should not trigger a reset")
			}
			retry, attempt := state.next(low, 5)
			if !attempt || retry != first {
				t.Fatalf("no-op did not retry the same request: first=%+v retry=%+v attempt=%v", first, retry, attempt)
			}
			state.record(&codex.ResetResult{Outcome: outcome}, nil)
			if _, attempt := state.next(autoResetSnapshot(95, 1), 5); attempt {
				t.Fatal("healthy quota should not trigger a reset")
			}
			second, attempt := state.next(low, 5)
			if !attempt || second.IdempotencyKey == first.IdempotencyKey {
				t.Fatal("a future low episode should get a new request")
			}
		})
	}
}
