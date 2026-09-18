package cli

import (
	"math"

	"github.com/xoba/ccodex/internal/codex"
)

// quotaBuckets selects the same authoritative view for display and alarms.
// The legacy single bucket is only a fallback when the multi-bucket view is absent.
func quotaBuckets(limits *codex.RateLimitsResponse) map[string]codex.RateLimitSnapshot {
	if limits == nil {
		return nil
	}
	if len(limits.RateLimitsByLimitID) > 0 {
		return limits.RateLimitsByLimitID
	}
	if limits.RateLimits == nil {
		return nil
	}
	id := "codex"
	if limits.RateLimits.LimitID != nil && *limits.RateLimits.LimitID != "" {
		id = *limits.RateLimits.LimitID
	}
	return map[string]codex.RateLimitSnapshot{id: *limits.RateLimits}
}

// quotaLow is the one rule behind alarms and automatic resets, so both act
// on the same reading: remaining quota at or below the threshold. A zero
// threshold disables both.
func quotaLow(remaining, threshold float64) bool {
	return threshold > 0 && remaining <= threshold
}

func hasLowQuota(snapshot *codex.Snapshot, threshold float64) bool {
	if snapshot == nil {
		return false
	}
	for _, bucket := range quotaBuckets(snapshot.RateLimits) {
		for kind := uint8(0); kind < 3; kind++ {
			if remaining, known := quotaDimensionRemaining(bucket, kind); known && quotaLow(remaining, threshold) {
				return true
			}
		}
	}
	return false
}

// quotaDimensionRemaining reports a dimension's remaining percentage, clamped
// to 0–100, or false when the server did not supply a usable value.
func quotaDimensionRemaining(bucket codex.RateLimitSnapshot, kind uint8) (float64, bool) {
	if kind == 2 {
		if bucket.IndividualLimit == nil {
			return 0, false
		}
		return math.Max(0, math.Min(100, float64(bucket.IndividualLimit.RemainingPercent))), true
	}
	window := bucket.Primary
	if kind == 1 {
		window = bucket.Secondary
	}
	if window == nil || window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		return 0, false
	}
	return math.Max(0, math.Min(100, 100-*window.UsedPercent)), true
}
