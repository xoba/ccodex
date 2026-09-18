package cli

import "github.com/xoba/ccodex/internal/codex"

type autoResetOutcome uint8

const (
	autoResetUnresolved autoResetOutcome = iota
	autoResetUnused
	autoResetConsumed
)

type quotaDimension struct {
	bucket string
	kind   uint8 // 0: primary window, 1: secondary window, 2: individual spend limit
}

// autoResetState keeps one logical request for a continuous low-quota episode.
// Its zero value is ready to use. Call next once per ordinary successful fetch,
// then record after an attempted reset; a reset's follow-up read cannot rearm it.
type autoResetState struct {
	params   codex.ResetParams
	outcome  autoResetOutcome
	triggers []quotaDimension
}

func (s *autoResetState) next(snapshot *codex.Snapshot, threshold float64) (codex.ResetParams, bool) {
	low := hasLowQuota(snapshot, threshold)
	if s.params.IdempotencyKey != "" {
		// An unknown outcome may already have spent the last credit. Preserve
		// this request across healthy or missing readings, and retry its key
		// when low again even if the credit count has since fallen to zero.
		if s.outcome == autoResetUnresolved {
			return s.params, low
		}
		if !low && s.recovered(snapshot, threshold) {
			*s = autoResetState{}
			return codex.ResetParams{}, false
		}
		if !low || s.outcome == autoResetConsumed || !hasResetCredit(snapshot) {
			return codex.ResetParams{}, false
		}
		// A no-op can become eligible later. It is still the same request.
		s.outcome = autoResetUnresolved
		return s.params, true
	}
	if !low || !hasResetCredit(snapshot) {
		return codex.ResetParams{}, false
	}
	s.params = codex.ResetParams{IdempotencyKey: newRequestID()}
	s.resume(snapshot, threshold, s.params)
	return s.params, true
}

// resume adopts an account-bound request saved before an earlier watch exit.
// Its outcome is unresolved until the same idempotency key is confirmed.
func (s *autoResetState) resume(snapshot *codex.Snapshot, threshold float64, params codex.ResetParams) {
	s.params = params
	s.outcome = autoResetUnresolved
	s.triggers = nil
	for id, bucket := range quotaBuckets(snapshot.RateLimits) {
		for kind := uint8(0); kind < 3; kind++ {
			if remaining, known := quotaDimensionRemaining(bucket, kind); known && quotaLow(remaining, threshold) {
				s.triggers = append(s.triggers, quotaDimension{bucket: id, kind: kind})
			}
		}
	}
}

func (s *autoResetState) record(result *codex.ResetResult, err error) {
	if s.params.IdempotencyKey == "" {
		return
	}
	s.outcome = autoResetUnresolved
	if err != nil || result == nil {
		return
	}
	switch result.Outcome {
	case "reset", "alreadyRedeemed":
		s.outcome = autoResetConsumed
	case "nothingToReset", "noCredit":
		s.outcome = autoResetUnused
	}
}

func (s *autoResetState) recovered(snapshot *codex.Snapshot, threshold float64) bool {
	if snapshot == nil || len(s.triggers) == 0 {
		return false
	}
	buckets := quotaBuckets(snapshot.RateLimits)
	for _, trigger := range s.triggers {
		bucket, present := buckets[trigger.bucket]
		if !present {
			return false
		}
		remaining, known := quotaDimensionRemaining(bucket, trigger.kind)
		if !known || quotaLow(remaining, threshold) {
			return false
		}
	}
	return true
}

func hasResetCredit(snapshot *codex.Snapshot) bool {
	return snapshot != nil && snapshot.RateLimits != nil &&
		snapshot.RateLimits.RateLimitResetCredits != nil &&
		snapshot.RateLimits.RateLimitResetCredits.AvailableCount > 0
}
