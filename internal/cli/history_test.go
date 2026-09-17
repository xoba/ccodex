package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xoba/ccodex/internal/codex"
	"github.com/xoba/ccodex/internal/history"
	"github.com/xoba/ccodex/internal/resetbudget"
)

// fakeHistory keeps writes in memory. writeErr makes every write fail.
type fakeHistory struct {
	writeErr error
	samples  []history.Sample
	events   []history.ResetEvent
	pruned   []time.Time
}

func (h *fakeHistory) Path() string { return "/history.db" }
func (h *fakeHistory) RecordSamples(_ context.Context, samples []history.Sample) error {
	if h.writeErr != nil {
		return h.writeErr
	}
	h.samples = append(h.samples, samples...)
	return nil
}
func (h *fakeHistory) RecordResetEvent(_ context.Context, event history.ResetEvent) error {
	if h.writeErr != nil {
		return h.writeErr
	}
	h.events = append(h.events, event)
	return nil
}
func (h *fakeHistory) Prune(_ context.Context, before time.Time) error {
	h.pruned = append(h.pruned, before)
	return h.writeErr
}
func (h *fakeHistory) Samples(_ context.Context, since time.Time, fn func(history.Sample) error) error {
	for _, sample := range h.samples {
		if !sample.Time.Before(since) {
			if err := fn(sample); err != nil {
				return err
			}
		}
	}
	return nil
}
func (h *fakeHistory) ResetEvents(_ context.Context, since time.Time, fn func(history.ResetEvent) error) error {
	for _, event := range h.events {
		if !event.Time.Before(since) {
			if err := fn(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *fakeHistory) eventNames() []string {
	names := make([]string, 0, len(h.events))
	for _, event := range h.events {
		names = append(names, strings.TrimSuffix(event.Event+" "+event.Outcome+event.Detail, " "))
	}
	return names
}

func testRecorder(store historyStore, stderr *bytes.Buffer) *historyRecorder {
	return &historyRecorder{store: store, output: stderr, timeout: time.Second}
}

func TestStatusSavesSamplesUnlessHistoryIsOff(t *testing.T) {
	for _, test := range []struct {
		args []string
		want int
	}{{[]string{"status"}, 1}, {[]string{}, 1}, {[]string{"status", "--no-history"}, 0}} {
		store := &fakeHistory{}
		cmd := newCommandWithHistory(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			return autoSnapshot(40, 2), nil
		}, nil, nil, nil, func() (historyStore, error) { return store, nil })
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(test.args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if len(store.samples) != test.want || stderr.Len() != 0 || len(store.pruned) != 0 {
			t.Fatalf("%v: samples=%d pruned=%d stderr=%q", test.args, len(store.samples), len(store.pruned), stderr.String())
		}
		if test.want == 0 {
			continue
		}
		got := store.samples[0]
		if got.Account != "3001f754dd60" || got.Plan != "pro" || got.LimitID != "codex" || got.Dimension != "primary" ||
			got.UsedPercent != 40 || got.RemainingPercent != 60 || *got.ResetCredits != 2 || got.Source != "status" {
			t.Fatalf("unexpected sample: %+v", got)
		}
	}
}

func TestSnapshotSamplesCoverEveryKnownDimension(t *testing.T) {
	used, mins, resetsAt := 150.0, int64(10080), int64(1_800_000_000)
	plan := "team"
	snapshot := quotaSnapshot(codex.RateLimitSnapshot{
		Primary: quotaWindow(12.5), Secondary: &codex.RateLimitWindow{UsedPercent: &used, WindowDurationMins: &mins, ResetsAt: &resetsAt},
		IndividualLimit: &codex.SpendControlLimitSnapshot{RemainingPercent: 30, ResetsAt: resetsAt}, PlanType: &plan,
	})
	snapshot.RateLimits.RateLimitsByLimitID = map[string]codex.RateLimitSnapshot{
		"zeta": {Primary: quotaWindow(1)}, "alpha": *snapshot.RateLimits.RateLimits, "empty": {Primary: &codex.RateLimitWindow{}},
	}
	var got []string
	for _, s := range snapshotSamples(snapshot) {
		line := fmt.Sprintf("%s/%s used=%g plan=%s account=%q", s.LimitID, s.Dimension, s.UsedPercent, s.Plan, s.Account)
		if s.WindowMins != nil {
			line += fmt.Sprintf(" mins=%d", *s.WindowMins)
		}
		if s.ResetsAt != nil {
			line += fmt.Sprintf(" resets=%d", s.ResetsAt.Unix())
		}
		if s.ResetCredits != nil {
			line += " credits"
		}
		got = append(got, line)
	}
	want := []string{
		`alpha/primary used=12.5 plan=team account=""`,
		`alpha/secondary used=100 plan=team account="" mins=10080 resets=1800000000`,
		`alpha/individual used=70 plan=team account="" resets=1800000000`,
		`zeta/primary used=1 plan= account=""`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("samples:\n got %q\nwant %q", got, want)
	}
	if snapshotSamples(nil) != nil || snapshotSamples(&codex.Snapshot{}) != nil {
		t.Fatal("missing quota data produced samples")
	}
}

func TestSampleFailureIsReportedOnceAndNeverStopsWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeHistory{writeErr: errors.New("disk full")}
	reads := 0
	cmd := newCommandWithHistory(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		if reads++; reads == 3 {
			cancel()
		}
		return autoSnapshot(40, 2), nil
	}, nil, nil, nil, func() (historyStore, error) { return store, nil })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--interval", "1s", "--json"})
	bounded, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if reads != 3 || strings.Count(stderr.String(), "history was not saved: disk full") != 1 {
		t.Fatalf("reads=%d stderr=%q", reads, stderr.String())
	}
}

func TestWatchPrunesOldSamplesOncePerDay(t *testing.T) {
	var stderr bytes.Buffer
	store := &fakeHistory{}
	recorder := testRecorder(store, &stderr)
	recorder.retention = 48 * time.Hour
	for i := 0; i < 3; i++ {
		recorder.samples(context.Background(), autoSnapshot(40, 2), "watch")
	}
	if len(store.pruned) != 1 || time.Since(store.pruned[0]) < 47*time.Hour || time.Since(store.pruned[0]) > 49*time.Hour {
		t.Fatalf("pruned=%v", store.pruned)
	}
}

func TestAutoResetHistoryExplainsTheRequest(t *testing.T) {
	var state autoResetState
	var stderr bytes.Buffer
	store := &fakeHistory{}
	recorder := testRecorder(store, &stderr)
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	reset := func(_ context.Context, _ codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		if len(store.events) != 1 || store.events[0].Event != "requested" || store.events[0].Key != params.IdempotencyKey {
			t.Fatalf("request was sent before history saved it: %+v", store.events)
		}
		return recoveredReset(params, 1), nil
	}
	if _, err := applyAutoReset(context.Background(), &state, autoSnapshot(97, 2), 5, codex.Options{}, reset, budget, 1, &stderr, recorder); err != nil {
		t.Fatal(err)
	}
	if got := store.eventNames(); !reflect.DeepEqual(got, []string{"requested", "outcome reset"}) {
		t.Fatalf("events=%q", got)
	}
	requested, outcome := store.events[0], store.events[1]
	threshold, credits := 5.0, int64(2)
	want := &history.Reason{
		Trigger: "lowQuota", Threshold: &threshold, ResetCredits: &credits, MaxResetsPerDay: 1,
		Low: []history.LowQuota{{LimitID: "codex", Dimension: "primary", RemainingPercent: 3}},
	}
	if !reflect.DeepEqual(requested.Reason, want) {
		t.Fatalf("reason=%+v", requested.Reason)
	}
	if requested.Mode != "auto" || requested.Account != "3001f754dd60" || requested.Time.IsZero() || requested.Version == "" ||
		outcome.Key != requested.Key || outcome.Account != requested.Account || !strings.Contains(requested.Key, requested.Account) {
		t.Fatalf("requested=%+v outcome=%+v", requested, outcome)
	}

	// Still low after the reset: say why nothing more is spent, but only once.
	for i := 0; i < 2; i++ {
		if _, err := applyAutoReset(context.Background(), &state, autoSnapshot(97, 1), 5, codex.Options{}, reset, budget, 1, &stderr, recorder); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.eventNames(); !reflect.DeepEqual(got, []string{"requested", "outcome reset", "skipped alreadyResetThisPeriod"}) {
		t.Fatalf("events=%q", got)
	}
}

func TestAutoResetIsNotSentWhenHistoryCannotRecordIt(t *testing.T) {
	var state autoResetState
	var stderr bytes.Buffer
	store := &fakeHistory{writeErr: errors.New("sqlite3 was not found")}
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	updated, err := applyAutoReset(context.Background(), &state, autoSnapshot(97, 2), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("unrecorded automatic reset was sent")
		return nil, nil
	}, budget, 1, &stderr, testRecorder(store, &stderr))
	if err != nil || updated != nil || state.params.IdempotencyKey != "" {
		t.Fatalf("updated=%v err=%v state=%+v", updated, err, state)
	}
	if !strings.Contains(stderr.String(), "was not sent because history could not record it: sqlite3 was not found") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if allowed, err := budget.Reserve(context.Background(), "another-attempt", 1, time.Now()); err != nil || !allowed {
		t.Fatalf("unsent request kept its daily slot: allowed=%v err=%v", allowed, err)
	}
}

func TestUnrecordedRetryKeepsItsPendingReservation(t *testing.T) {
	var original, restarted autoResetState
	var stderr bytes.Buffer
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	if _, err := applyAutoReset(context.Background(), &original, autoSnapshot(97, 1), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		return nil, errors.New("lost response")
	}, budget, 1, &stderr, nil); err != nil {
		t.Fatal(err)
	}
	store := &fakeHistory{writeErr: errors.New("disk full")}
	if _, err := applyAutoReset(context.Background(), &restarted, autoSnapshot(97, 0), 5, codex.Options{}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("unrecorded retry was sent")
		return nil, nil
	}, budget, 1, &stderr, testRecorder(store, &stderr)); err != nil {
		t.Fatal(err)
	}
	if allowed, err := budget.Reserve(context.Background(), "another-attempt", 1, time.Now()); err != nil || allowed {
		t.Fatalf("uncertain redemption lost its reservation: allowed=%v err=%v", allowed, err)
	}
}

func TestAutoResetHistoryRecordsFailuresAndSkips(t *testing.T) {
	var state autoResetState
	var stderr bytes.Buffer
	store := &fakeHistory{}
	recorder := testRecorder(store, &stderr)
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	fail := func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		return nil, errors.New("lost response")
	}
	apply := func(state *autoResetState, snapshot *codex.Snapshot, reset resetFunc) {
		t.Helper()
		if _, err := applyAutoReset(context.Background(), state, snapshot, 5, codex.Options{}, reset, budget, 1, &stderr, recorder); err != nil {
			t.Fatal(err)
		}
	}
	apply(&state, autoSnapshot(97, 1), fail)
	apply(&state, autoSnapshot(97, 1), fail)
	if got := store.eventNames(); !reflect.DeepEqual(got, []string{"requested", "error lost response", "requested", "error lost response"}) {
		t.Fatalf("events=%q", got)
	}
	if store.events[0].Reason.Retry || !store.events[2].Reason.Retry || store.events[0].Key != store.events[2].Key {
		t.Fatalf("retry was not marked: %+v then %+v", store.events[0], store.events[2])
	}

	// The unresolved request holds today's only slot, so another account is
	// refused: once per low period, and again after it recovers and drops.
	store.events = nil
	other := func(used float64) *codex.Snapshot {
		snapshot := autoSnapshot(used, 1)
		id := "other-account"
		snapshot.RateLimits.AccountID = &id
		return snapshot
	}
	var second autoResetState
	apply(&second, other(97), fail)
	apply(&second, other(97), fail)
	apply(&second, other(10), fail)
	apply(&second, other(97), fail)
	noCredit := other(97)
	noCredit.RateLimits.RateLimitResetCredits.AvailableCount = 0
	apply(&second, noCredit, fail)
	noAccount := other(97)
	noAccount.RateLimits.AccountID = nil
	apply(&second, noAccount, fail)
	want := []string{"skipped dailyLimitReached", "skipped dailyLimitReached", "skipped noResetAvailable", "skipped noAccountID"}
	if got := store.eventNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%q", got)
	}
}

func TestManualResetIsRecordedButNeverBlockedByHistory(t *testing.T) {
	for _, writeErr := range []error{nil, errors.New("disk full")} {
		store := &fakeHistory{writeErr: writeErr}
		cmd := newCommandWithHistory(nil, func(_ context.Context, _ codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
			if writeErr == nil && (len(store.events) != 1 || store.events[0].Key != params.IdempotencyKey) {
				t.Fatalf("request was sent before history saved it: %+v", store.events)
			}
			return recoveredReset(params, 0), nil
		}, nil, nil, func() (historyStore, error) { return store, nil })
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"reset", "--idempotency-key", "my-key", "--credit-id", "credit-1"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), "Reset applied") {
			t.Fatalf("stdout=%q", stdout.String())
		}
		if writeErr != nil {
			if strings.Count(stderr.String(), "history was not saved: disk full") != 1 {
				t.Fatalf("stderr=%q", stderr.String())
			}
			continue
		}
		if got := store.eventNames(); !reflect.DeepEqual(got, []string{"requested", "outcome reset"}) {
			t.Fatalf("events=%q", got)
		}
		want := &history.Reason{Trigger: "manual", CreditID: "credit-1"}
		if requested, outcome := store.events[0], store.events[1]; requested.Mode != "manual" || requested.Key != "my-key" ||
			!reflect.DeepEqual(requested.Reason, want) || outcome.Account != "3001f754dd60" {
			t.Fatalf("requested=%+v outcome=%+v", requested, outcome)
		}
		if len(store.samples) != 1 || store.samples[0].Source != "reset" || store.samples[0].UsedPercent != 0 {
			t.Fatalf("reading after the reset was not kept: %+v", store.samples)
		}
	}
}

func TestManualResetFailureIsRecorded(t *testing.T) {
	store := &fakeHistory{}
	cmd := newCommandWithHistory(nil, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		return nil, errors.New("backend unavailable")
	}, nil, nil, func() (historyStore, error) { return store, nil })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"reset"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("reset failure was not returned")
	}
	if got := store.eventNames(); !reflect.DeepEqual(got, []string{"requested", "error backend unavailable"}) {
		t.Fatalf("events=%q", got)
	}
}

func TestUnavailableHistoryPausesAutomaticResetsOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	reads := 0
	cmd := newCommandWithHistory(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		if reads++; reads == 3 {
			cancel()
		}
		return autoSnapshot(97, 2), nil
	}, func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error) {
		t.Fatal("unrecorded automatic reset was sent")
		return nil, nil
	}, nil, func() (autoResetBudget, error) { return budget, nil }, func() (historyStore, error) {
		return nil, errors.New("no home directory")
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"watch", "--auto-reset", "--interval", "1s", "--json"})
	bounded, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if reads != 3 || strings.Count(stdout.String(), "\n") != 2 || strings.Count(stderr.String(), "was not sent because history could not record it: no home directory") != 2 {
		t.Fatalf("reads=%d stdout=%q stderr=%q", reads, stdout.String(), stderr.String())
	}
}

func TestWatchSavesEveryCheckWithItsSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeHistory{}
	budget := resetbudget.Store{Path: filepath.Join(t.TempDir(), "auto-resets.json")}
	reads := 0
	cmd := newCommandWithHistory(func(context.Context, codex.Options) (*codex.Snapshot, error) {
		if reads++; reads == 4 {
			cancel()
		}
		if reads == 3 {
			return autoSnapshot(97, 1), nil
		}
		return autoSnapshot(40, 1), nil
	}, func(_ context.Context, _ codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
		return recoveredReset(params, 0), nil
	}, nil, func() (autoResetBudget, error) { return budget, nil }, func() (historyStore, error) { return store, nil })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"watch", "--auto-reset", "--no-alarm", "--interval", "1s", "--json"})
	bounded, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	if err := cmd.ExecuteContext(bounded); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var got []string
	for _, sample := range store.samples {
		got = append(got, fmt.Sprintf("%s %g", sample.Source, sample.UsedPercent))
	}
	// The unchanged second reading is kept too, as is the one after the reset.
	if want := []string{"watch 40", "watch 40", "watch 97", "auto-reset 0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("saved checks: %q", got)
	}
}

func historyFixture() *fakeHistory {
	at := time.Date(2026, 3, 17, 14, 3, 12, 0, time.UTC)
	threshold, credits, fiveHours, week := 5.0, int64(2), int64(300), int64(10080)
	resetsAt := at.Add(2 * time.Hour)
	check := func(at time.Time, source string, remaining float64) []history.Sample {
		return []history.Sample{{
			Time: at, Source: source, Account: "3001f754dd60", Plan: "pro", LimitID: "codex", Dimension: "primary", WindowMins: &fiveHours,
			UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: &resetsAt, ResetCredits: &credits,
		}, {
			Time: at, Source: source, Account: "3001f754dd60", Plan: "pro", LimitID: "codex", Dimension: "secondary", WindowMins: &week,
			UsedPercent: 41, RemainingPercent: 59, ResetCredits: &credits,
		}, {
			Time: at, Source: source, Account: "3001f754dd60", Plan: "pro", LimitID: "spark", Dimension: "individual",
			UsedPercent: 70, RemainingPercent: 30, ResetCredits: &credits,
		}}
	}
	store := &fakeHistory{
		events: []history.ResetEvent{{
			Time: at, Key: "auto-3001f754dd60-1111-2222-333344445555", Event: "requested", Mode: "auto", Account: "3001f754dd60", Version: "1.2.3",
			Reason: &history.Reason{
				Trigger: "lowQuota", Threshold: &threshold, ResetCredits: &credits, MaxResetsPerDay: 1,
				Low: []history.LowQuota{{LimitID: "codex", Dimension: "primary", RemainingPercent: 3.5}},
			},
		}, {
			Time: at.Add(2 * time.Second), Key: "auto-3001f754dd60-1111-2222-333344445555", Event: "outcome", Mode: "auto", Outcome: "reset",
		}, {
			Time: at.Add(time.Hour), Event: "skipped", Mode: "auto", Detail: "dailyLimitReached",
		}, {
			Time: at.Add(2 * time.Hour), Key: "k", Event: "error", Mode: "manual", Detail: "=cmd|' /C calc'!A0\x1b[31m\nnext",
		}},
	}
	// Two identical status checks in the same second, the check that triggered
	// the reset, and the reading after it.
	store.samples = append(store.samples, check(at.Add(-time.Minute), "status", 3.5)...)
	store.samples = append(store.samples, check(at.Add(-time.Minute), "status", 3.5)...)
	store.samples = append(store.samples, check(at, "watch", 3.5)...)
	store.samples = append(store.samples, check(at.Add(2*time.Second), "auto-reset", 100)[:1]...)
	return store
}

func runHistory(t *testing.T, store historyStore, args ...string) string {
	t.Helper()
	cmd := newCommandWithHistory(nil, nil, nil, nil, func() (historyStore, error) { return store, nil })
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return stdout.String()
}

func TestHistoryShowsEveryCheckAndResetEventInOrder(t *testing.T) {
	store := historyFixture()
	at := store.events[0].Time
	var want bytes.Buffer
	for _, line := range []struct {
		at   time.Time
		rest string
	}{
		{at.Add(-time.Minute), "status  3001f754dd60  check            codex 5h 3.5%, 7d 59% left; spark spend limit 30% left; earned resets: 2"},
		{at.Add(-time.Minute), "status  3001f754dd60  check            codex 5h 3.5%, 7d 59% left; spark spend limit 30% left; earned resets: 2"},
		{at, "watch   3001f754dd60  reset requested  codex primary 3.5% left; threshold 5%; earned resets: 2 (request …333344445555)"},
		{at, "watch   3001f754dd60  check            codex 5h 3.5%, 7d 59% left; spark spend limit 30% left; earned resets: 2"},
		{at.Add(2 * time.Second), "watch                 reset outcome    reset (request …333344445555)"},
		{at.Add(2 * time.Second), "auto-reset  3001f754dd60  check        codex 5h 100% left; earned resets: 2"},
		{at.Add(time.Hour), "watch                 reset skipped    daily limit reached"},
		{at.Add(2 * time.Hour), "reset                 reset error      =cmd|' /C calc'!A0 [31m next (request k)"},
	} {
		fmt.Fprintf(&want, "%s  %s\n", line.at.Local().Format(historyTimeLayout), line.rest)
	}
	got := runHistory(t, store, "history")
	header, rows, _ := strings.Cut(got, "\n")
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if squeeze(header) != "TIME SOURCE ACCOUNT EVENT DETAIL" {
		t.Errorf("header: %q", header)
	}
	gotRows, wantRows := strings.Split(rows, "\n"), strings.Split(want.String(), "\n")
	if len(gotRows) != len(wantRows) {
		t.Fatalf("history:\n%s", got)
	}
	for i := range wantRows {
		if squeeze(gotRows[i]) != squeeze(wantRows[i]) {
			t.Errorf("line %d:\n got %s\nwant %s", i+1, squeeze(gotRows[i]), squeeze(wantRows[i]))
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Error("control characters reached the terminal")
	}

	count := func(args ...string) (checks, resets int) {
		for _, line := range strings.Split(strings.TrimSpace(runHistory(t, store, args...)), "\n")[1:] {
			if strings.Contains(line, " check ") {
				checks++
			} else {
				resets++
			}
		}
		return checks, resets
	}
	for _, test := range []struct {
		args           []string
		checks, resets int
	}{
		{[]string{"history", "--since", "all"}, 4, 4}, {[]string{"history", "--checks"}, 4, 0}, {[]string{"history", "--resets"}, 0, 4},
		{[]string{"history", "--checks", "--resets"}, 4, 4},
	} {
		if checks, resets := count(test.args...); checks != test.checks || resets != test.resets {
			t.Errorf("%v: %d checks and %d reset events", test.args, checks, resets)
		}
	}
	// The fixture is far older than an hour.
	if got := runHistory(t, store, "history", "--since", "1h"); !strings.HasPrefix(got, "No history recorded since ") {
		t.Errorf("recent history: %q", got)
	}
	if got := runHistory(t, &fakeHistory{}, "history"); got != "No history recorded.\n" {
		t.Errorf("empty history: %q", got)
	}
	if got := runHistory(t, store, "history", "--path"); got != "/history.db\n" {
		t.Errorf("path: %q", got)
	}
}

func TestHistoryCSVAndJSON(t *testing.T) {
	store := historyFixture()
	store.samples = store.samples[6:]
	wantCSV := `time,type,source,account,plan,limit_id,dimension,window_mins,used_percent,remaining_percent,resets_at,reset_credits,event,outcome,key,detail,reason,version
2026-03-17T14:03:12Z,reset,watch,3001f754dd60,,,,,,,,,requested,,auto-3001f754dd60-1111-2222-333344445555,,"{""trigger"":""lowQuota"",""threshold"":5,""low"":[{""limitId"":""codex"",""dimension"":""primary"",""remainingPercent"":3.5}],""resetCredits"":2,""maxResetsPerDay"":1}",1.2.3
2026-03-17T14:03:12Z,check,watch,3001f754dd60,pro,codex,primary,300,96.5,3.5,2026-03-17T16:03:12Z,2,,,,,,
2026-03-17T14:03:12Z,check,watch,3001f754dd60,pro,codex,secondary,10080,41,59,,2,,,,,,
2026-03-17T14:03:12Z,check,watch,3001f754dd60,pro,spark,individual,,70,30,,2,,,,,,
2026-03-17T14:03:14Z,reset,watch,,,,,,,,,,outcome,reset,auto-3001f754dd60-1111-2222-333344445555,,,
2026-03-17T14:03:14Z,check,auto-reset,3001f754dd60,pro,codex,primary,300,0,100,2026-03-17T16:03:12Z,2,,,,,,
2026-03-17T15:03:12Z,reset,watch,,,,,,,,,,skipped,,,dailyLimitReached,,
2026-03-17T16:03:12Z,reset,reset,,,,,,,,,,error,,k,'=cmd|' /C calc'!A0 [31m next,,
`
	if got := runHistory(t, store, "history", "--csv"); got != wantCSV {
		t.Errorf("CSV:\n got %s\nwant %s", got, wantCSV)
	}
	header, _, _ := strings.Cut(wantCSV, "\n")
	if got := runHistory(t, store, "history", "--checks", "--csv"); !strings.HasPrefix(got, header+"\n") || strings.Count(got, "\n") != 5 || strings.Contains(got, ",reset,") {
		t.Errorf("checks CSV:\n%s", got)
	}

	wantJSON := `{"type":"check","time":"2026-03-17T14:03:12Z","source":"watch","account":"3001f754dd60","plan":"pro","resetCredits":2,"windows":[{"limitId":"codex","dimension":"primary","windowMins":300,"usedPercent":96.5,"remainingPercent":3.5,"resetsAt":"2026-03-17T16:03:12Z"},{"limitId":"codex","dimension":"secondary","windowMins":10080,"usedPercent":41,"remainingPercent":59},{"limitId":"spark","dimension":"individual","usedPercent":70,"remainingPercent":30}]}
{"type":"check","time":"2026-03-17T14:03:14Z","source":"auto-reset","account":"3001f754dd60","plan":"pro","resetCredits":2,"windows":[{"limitId":"codex","dimension":"primary","windowMins":300,"usedPercent":0,"remainingPercent":100,"resetsAt":"2026-03-17T16:03:12Z"}]}
`
	if got := runHistory(t, store, "history", "--checks", "--json"); got != wantJSON {
		t.Errorf("JSON:\n got %s\nwant %s", got, wantJSON)
	}
	got := runHistory(t, store, "history", "--resets", "--json")
	if !strings.HasPrefix(got, `{"type":"reset","time":"2026-03-17T14:03:12Z","source":"watch","account":"3001f754dd60","event":"requested","key":"auto-`) ||
		!strings.Contains(got, `"reason":{"trigger":"lowQuota","threshold":5,`) || strings.Count(got, "\n") != 4 {
		t.Errorf("reset JSON:\n%s", got)
	}
	if got := runHistory(t, &fakeHistory{}, "history", "--json"); got != "" {
		t.Errorf("empty JSON: %q", got)
	}
}

func TestHistoryCommandRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"history", "--csv", "--json"}, {"history", "--since", "yesterday"}, {"history", "--since", "-1h"},
		{"history", "--since", "-2d"}, {"history", "--since", "NaNd"}, {"history", "extra"}, {"history", "usage"},
	} {
		cmd := newCommandWithHistory(nil, nil, nil, nil, func() (historyStore, error) { return &fakeHistory{}, nil })
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Time{
		"all": {}, "0": now, "90m": now.Add(-90 * time.Minute), "7d": now.AddDate(0, 0, -7), "0.5d": now.Add(-12 * time.Hour),
	} {
		if got, err := parseSince(value, now); err != nil || !got.Equal(want) {
			t.Errorf("%s: got %v err=%v", value, got, err)
		}
	}
}

// TestHistoryEndToEnd runs the real store when sqlite3 is installed.
func TestHistoryEndToEnd(t *testing.T) {
	store := history.New(filepath.Join(t.TempDir(), "ccodex", "history.db"))
	create := func() (historyStore, error) { return store, nil }
	run := func(args ...string) (string, string) {
		t.Helper()
		cmd := newCommandWithHistory(func(context.Context, codex.Options) (*codex.Snapshot, error) {
			return autoSnapshot(40, 2), nil
		}, func(_ context.Context, _ codex.Options, params codex.ResetParams) (*codex.ResetResult, error) {
			return recoveredReset(params, 1), nil
		}, nil, nil, create)
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout.String(), stderr.String()
	}
	if _, stderr := run("status"); strings.Contains(stderr, "sqlite3 was not found") {
		t.Skip(stderr)
	} else if stderr != "" {
		t.Fatalf("status: %s", stderr)
	}
	run("status")
	run("reset", "--idempotency-key", "end-to-end")
	lines := strings.Split(strings.TrimSpace(func() string { out, _ := run("history", "--csv"); return out }()), "\n")[1:]
	var got []string
	for _, line := range lines {
		fields := strings.Split(line, ",")
		got = append(got, strings.Join([]string{fields[1], fields[2], fields[8], fields[12], fields[13]}, " "))
	}
	// Both status checks are kept although nothing changed between them.
	want := []string{"check status 40  ", "check status 40  ", "reset reset  requested ", "reset reset  outcome reset", "check reset 0  "}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history:\n got %q\nwant %q", got, want)
	}
	if table, _ := run("history"); strings.Count(table, " check ") != 3 || strings.Count(table, "(request end-to-end)") != 2 {
		t.Fatalf("history table:\n%s", table)
	}
}
