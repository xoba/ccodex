package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/xoba/ccodex/internal/buildinfo"
	"github.com/xoba/ccodex/internal/codex"
	"github.com/xoba/ccodex/internal/history"
)

type historyStore interface {
	Path() string
	RecordSamples(context.Context, []history.Sample) error
	RecordResetEvent(context.Context, history.ResetEvent) error
	Prune(context.Context, time.Time) error
	Samples(context.Context, time.Time, func(history.Sample) error) error
	ResetEvents(context.Context, time.Time, func(history.ResetEvent) error) error
}

func defaultHistory() (historyStore, error) {
	path, err := history.DefaultPath()
	if err != nil {
		return nil, err
	}
	return history.New(path), nil
}

// unavailableHistory stands in when the store cannot be located. It fails every
// write, so automatic resets stay off instead of running unrecorded, which a
// nil recorder (history switched off by the user) would allow.
type unavailableHistory struct{ err error }

func (h unavailableHistory) Path() string { return "" }
func (h unavailableHistory) RecordSamples(context.Context, []history.Sample) error {
	return h.err
}
func (h unavailableHistory) RecordResetEvent(context.Context, history.ResetEvent) error {
	return h.err
}
func (h unavailableHistory) Prune(context.Context, time.Time) error { return h.err }
func (h unavailableHistory) Samples(context.Context, time.Time, func(history.Sample) error) error {
	return h.err
}
func (h unavailableHistory) ResetEvents(context.Context, time.Time, func(history.ResetEvent) error) error {
	return h.err
}

// historyRecorder is one command's view of the history. A nil recorder means
// the user switched history off; every method is then a no-op.
type historyRecorder struct {
	store   historyStore
	output  io.Writer
	timeout time.Duration
	// retention is how long watch keeps samples; zero keeps them forever and
	// is what one-shot commands use, since they never prune.
	retention time.Duration
	lastPrune time.Time
	lastError string
	lastSkip  string
}

// samples saves one check, naming the command that made it. History is a
// convenience here, so a failure is reported once and never interrupts
// monitoring.
func (r *historyRecorder) samples(ctx context.Context, snapshot *codex.Snapshot, source string) {
	if r == nil {
		return
	}
	samples := snapshotSamples(snapshot)
	if len(samples) == 0 {
		return
	}
	for i := range samples {
		samples[i].Source = source
	}
	writeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	err := r.store.RecordSamples(writeCtx, samples)
	if err == nil && r.retention > 0 && time.Since(r.lastPrune) >= 24*time.Hour {
		r.lastPrune = time.Now()
		err = r.store.Prune(writeCtx, time.Now().Add(-r.retention))
	}
	if ctx.Err() == nil {
		r.report(err)
	}
}

// event saves one reset event, finishing even if ctx is canceled: a known
// outcome must not be lost to Ctrl-C. The caller decides whether an error
// blocks the reset.
func (r *historyRecorder) event(ctx context.Context, event history.ResetEvent) error {
	if r == nil {
		return nil
	}
	event.Time = time.Now()
	event.Version = buildinfo.Version
	if len(event.Detail) > 2000 {
		event.Detail = event.Detail[:2000]
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
	defer cancel()
	return r.store.RecordResetEvent(writeCtx, event)
}

// log saves an event whose failure must not change what happens next.
func (r *historyRecorder) log(ctx context.Context, event history.ResetEvent) {
	if r != nil {
		r.report(r.event(ctx, event))
	}
}

// skipped notes why watch made no automatic reset while quota was low. Watch
// reaches the same conclusion on every poll, so only a change is saved.
func (r *historyRecorder) skipped(ctx context.Context, account, detail string) {
	if r == nil || r.lastSkip == detail {
		return
	}
	r.lastSkip = detail
	r.log(ctx, history.ResetEvent{Event: "skipped", Mode: "auto", Account: account, Detail: detail})
}

// recovered ends the low-quota period that skipped was deduplicating.
func (r *historyRecorder) recovered() {
	if r != nil {
		r.lastSkip = ""
	}
}

func (r *historyRecorder) report(err error) {
	if err == nil {
		r.lastError = ""
		return
	}
	if message := err.Error(); message != r.lastError {
		r.lastError = message
		fmt.Fprintf(r.output, "ccodex: history was not saved: %v (--no-history turns history off)\n", err)
	}
}

// accountHash names an account in the history without storing its ID. It is a
// prefix of the hash inside automatic request keys, so the two can be matched.
func accountHash(limits *codex.RateLimitsResponse) string {
	if limits == nil || limits.AccountID == nil || *limits.AccountID == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(*limits.AccountID)))[:12]
}

func dimensionName(kind uint8) string {
	return [...]string{"primary", "secondary", "individual"}[kind]
}

func sortedBucketIDs(buckets map[string]codex.RateLimitSnapshot) []string {
	ids := make([]string, 0, len(buckets))
	for id := range buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// snapshotSamples flattens a snapshot into one sample per known quota
// dimension, using the same clamped percentages as alarms and automatic resets.
func snapshotSamples(snapshot *codex.Snapshot) []history.Sample {
	if snapshot == nil || snapshot.RateLimits == nil {
		return nil
	}
	at := snapshot.FetchedAt
	if at.IsZero() {
		at = time.Now()
	}
	var credits *int64
	if summary := snapshot.RateLimits.RateLimitResetCredits; summary != nil {
		credits = &summary.AvailableCount
	}
	buckets := quotaBuckets(snapshot.RateLimits)
	var samples []history.Sample
	for _, id := range sortedBucketIDs(buckets) {
		bucket := buckets[id]
		plan := ""
		if snapshot.Account != nil {
			plan = snapshot.Account.PlanType
		}
		if plan == "" && bucket.PlanType != nil {
			plan = *bucket.PlanType
		}
		for kind := uint8(0); kind < 3; kind++ {
			remaining, known := quotaDimensionRemaining(bucket, kind)
			if !known {
				continue
			}
			sample := history.Sample{
				Time: at, Account: accountHash(snapshot.RateLimits), Plan: plan, LimitID: id,
				Dimension: dimensionName(kind), UsedPercent: 100 - remaining, RemainingPercent: remaining,
				ResetCredits: credits,
			}
			resetsAt := int64(0)
			if kind == 2 {
				resetsAt = bucket.IndividualLimit.ResetsAt
			} else if window := [...]*codex.RateLimitWindow{bucket.Primary, bucket.Secondary}[kind]; window != nil {
				sample.WindowMins = window.WindowDurationMins
				if window.ResetsAt != nil {
					resetsAt = *window.ResetsAt
				}
			}
			if resetsAt > 0 {
				reset := time.Unix(resetsAt, 0).UTC()
				sample.ResetsAt = &reset
			}
			samples = append(samples, sample)
		}
	}
	return samples
}

// lowQuotaReason records what watch saw when it decided to request a reset.
func lowQuotaReason(snapshot *codex.Snapshot, threshold float64, limit int, retry bool) *history.Reason {
	reason := &history.Reason{Trigger: "lowQuota", Threshold: &threshold, MaxResetsPerDay: limit, Retry: retry}
	if summary := snapshot.RateLimits.RateLimitResetCredits; summary != nil {
		reason.ResetCredits = &summary.AvailableCount
	}
	buckets := quotaBuckets(snapshot.RateLimits)
	for _, id := range sortedBucketIDs(buckets) {
		for kind := uint8(0); kind < 3; kind++ {
			if remaining, known := quotaDimensionRemaining(buckets[id], kind); known && remaining < threshold {
				reason.Low = append(reason.Low, history.LowQuota{LimitID: id, Dimension: dimensionName(kind), RemainingPercent: remaining})
			}
		}
	}
	return reason
}
