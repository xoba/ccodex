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

func hasLowQuota(snapshot *codex.Snapshot, threshold float64) bool {
	if snapshot == nil {
		return false
	}
	for _, bucket := range quotaBuckets(snapshot.RateLimits) {
		for _, window := range []*codex.RateLimitWindow{bucket.Primary, bucket.Secondary} {
			if window == nil || window.UsedPercent == nil {
				continue
			}
			used := *window.UsedPercent
			if !math.IsNaN(used) && !math.IsInf(used, 0) && math.Max(0, math.Min(100, 100-used)) < threshold {
				return true
			}
		}
		if bucket.IndividualLimit != nil && math.Max(0, math.Min(100, float64(bucket.IndividualLimit.RemainingPercent))) < threshold {
			return true
		}
	}
	return false
}
