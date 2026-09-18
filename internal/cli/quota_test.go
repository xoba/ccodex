package cli

import (
	"math"
	"testing"

	"github.com/xoba/ccodex/internal/codex"
)

func quotaWindow(used float64) *codex.RateLimitWindow {
	return &codex.RateLimitWindow{UsedPercent: &used}
}

func quotaSnapshot(bucket codex.RateLimitSnapshot) *codex.Snapshot {
	return &codex.Snapshot{RateLimits: &codex.RateLimitsResponse{RateLimits: &bucket}}
}

func TestLowQuotaThresholdAndMissingValues(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot *codex.Snapshot
		want     bool
	}{
		{"no snapshot", nil, false},
		{"no limits", &codex.Snapshot{}, false},
		{"unknown windows", quotaSnapshot(codex.RateLimitSnapshot{}), false},
		{"unknown percentage", quotaSnapshot(codex.RateLimitSnapshot{Primary: &codex.RateLimitWindow{}}), false},
		{"exactly five percent", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(95)}), true},
		{"just below five percent", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(95.01)}), true},
		{"just above five percent", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(94.99)}), false},
		{"exhausted", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(100)}), true},
		{"over quota", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(101)}), true},
		{"secondary low", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(0), Secondary: quotaWindow(96)}), true},
		{"not a number", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(math.NaN())}), false},
		{"infinity", quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(math.Inf(1))}), false},
		{"spend limit low", quotaSnapshot(codex.RateLimitSnapshot{IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 4}}), true},
		{"spend limit exactly five", quotaSnapshot(codex.RateLimitSnapshot{IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 5}}), true},
		{"spend limit just above five", quotaSnapshot(codex.RateLimitSnapshot{IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 6}}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hasLowQuota(test.snapshot, 5); got != test.want {
				t.Fatalf("hasLowQuota = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLowQuotaUsesEveryAuthoritativeBucket(t *testing.T) {
	snapshot := quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(100)})
	snapshot.RateLimits.RateLimitsByLimitID = map[string]codex.RateLimitSnapshot{
		"codex": {Primary: quotaWindow(25)},
		"other": {Secondary: quotaWindow(94)},
	}
	if hasLowQuota(snapshot, 5) {
		t.Fatal("legacy mirror should not override the multi-bucket view")
	}
	pastReset := int64(0)
	window := quotaWindow(96)
	window.ResetsAt = &pastReset
	snapshot.RateLimits.RateLimitsByLimitID["other"] = codex.RateLimitSnapshot{Secondary: window}
	if !hasLowQuota(snapshot, 5) {
		t.Fatal("low quota in another bucket should trigger, even if its reset timestamp is past")
	}
}

func TestCustomQuotaThreshold(t *testing.T) {
	for _, test := range []struct {
		used, threshold float64
		want            bool
	}{
		{93, 10, true}, {90, 10, true}, {89.99, 10, false},
		{97.5, 2.5, true}, {97.49, 2.5, false},
		{100, 0, false}, {101, 0, false},
		{0, 100, true}, {1, 100, true},
	} {
		snapshot := quotaSnapshot(codex.RateLimitSnapshot{Primary: quotaWindow(test.used)})
		if got := hasLowQuota(snapshot, test.threshold); got != test.want {
			t.Errorf("used=%g threshold=%g: got %v, want %v", test.used, test.threshold, got, test.want)
		}
	}
	snapshot := quotaSnapshot(codex.RateLimitSnapshot{IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 7}})
	if !hasLowQuota(snapshot, 10) || !hasLowQuota(snapshot, 7) || hasLowQuota(snapshot, 6.5) {
		t.Fatal("individual spend limit did not use the custom threshold")
	}
}
