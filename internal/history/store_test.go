package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store := New(filepath.Join(t.TempDir(), "state", "history.db"))
	if _, err := store.run(context.Background(), true, "SELECT 1;\n"); err != nil && strings.Contains(err.Error(), "was not found") {
		t.Skip(err)
	}
	return store
}

func sample(at time.Time, dimension string, used float64) Sample {
	mins, credits := int64(300), int64(2)
	resetsAt := at.Add(time.Hour)
	return Sample{
		Time: at, Account: "abc123", Plan: "pro", LimitID: "codex", Dimension: dimension,
		WindowMins: &mins, UsedPercent: used, ResetsAt: &resetsAt, ResetCredits: &credits,
	}
}

func readSamples(t *testing.T, store *Store) []Sample {
	t.Helper()
	var samples []Sample
	if err := store.Samples(context.Background(), time.Time{}, func(s Sample) error {
		samples = append(samples, s)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return samples
}

func readEvents(t *testing.T, store *Store) []ResetEvent {
	t.Helper()
	var events []ResetEvent
	if err := store.ResetEvents(context.Background(), time.Time{}, func(e ResetEvent) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestResetEventsRoundTripHostileText(t *testing.T) {
	store := testStore(t)
	threshold, credits := 5.0, int64(2)
	at := time.Unix(1_800_000_000, 0).UTC()
	hostile := "it's \"quoted\"'); DROP TABLE reset_events;--\n.shell touch pwned\r\n\u2028 caf\u00e9 \\u0000 \\\\ end"
	want := []ResetEvent{{
		Time: at, Key: "auto-" + hostile, Event: "requested", Mode: "auto", Account: "abc123", Version: "1.2.3",
		Reason: &Reason{
			Trigger: "lowQuota", Threshold: &threshold, ResetCredits: &credits, MaxResetsPerDay: 1, Retry: true,
			Low: []LowQuota{{LimitID: hostile, Dimension: "primary", RemainingPercent: 3.5}},
		},
	}, {
		Time: at.Add(2 * time.Second), Key: "auto-" + hostile, Event: "error", Mode: "auto", Detail: hostile,
	}, {
		Time: at.Add(3 * time.Second), Event: "skipped", Mode: "auto", Detail: "dailyLimitReached",
	}, {
		Time: at.Add(4 * time.Second), Key: "manual-key", Event: "outcome", Mode: "manual", Outcome: "reset",
		Reason: &Reason{Trigger: "manual", CreditID: "credit-1"},
	}}
	for _, event := range want {
		if err := store.RecordResetEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if got := readEvents(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("events changed in storage:\n got %+v\nwant %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path()), "pwned")); err == nil {
		t.Fatal("stored text was executed as a sqlite3 command")
	}
	var since []ResetEvent
	if err := store.ResetEvents(context.Background(), at.Add(3*time.Second), func(e ResetEvent) error {
		since = append(since, e)
		return nil
	}); err != nil || len(since) != 2 {
		t.Fatalf("since filter returned %d events, err=%v", len(since), err)
	}
}

func TestNULCharactersDoNotTruncateText(t *testing.T) {
	store := testStore(t)
	event := ResetEvent{Time: time.Unix(1_800_000_000, 0).UTC(), Event: "error", Mode: "manual", Detail: "before\x00after"}
	if err := store.RecordResetEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if got := readEvents(t, store); len(got) != 1 || got[0].Detail != "before\uFFFDafter" {
		t.Fatalf("got %+v", got)
	}
}

func TestResetsViewPairsRequestWithLatestOutcome(t *testing.T) {
	store := testStore(t)
	at := time.Unix(1_800_000_000, 0)
	for i, event := range []ResetEvent{
		{Key: "k1", Event: "requested", Mode: "auto"},
		{Key: "k1", Event: "outcome", Mode: "auto", Outcome: "noCredit"},
		{Key: "k1", Event: "requested", Mode: "auto"},
		{Key: "k1", Event: "outcome", Mode: "auto", Outcome: "reset"},
		{Key: "k2", Event: "requested", Mode: "manual"},
		{Event: "skipped", Mode: "auto", Detail: "dailyLimitReached"},
	} {
		event.Time = at.Add(time.Duration(i) * time.Second)
		if err := store.RecordResetEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	output, err := store.run(context.Background(), true, "SELECT key, mode, requested_at, resolved_at, outcome FROM resets ORDER BY key;\n")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("k1|auto|%d|%d|reset\nk2|manual|%d||\n", at.Unix(), at.Unix()+3, at.Unix()+4)
	if string(output) != want {
		t.Fatalf("resets view:\n got %q\nwant %q", output, want)
	}
}

func TestSamplesAreSavedOnChangeOrHeartbeat(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0).UTC()
	record := func(s *Store, force bool, samples ...Sample) {
		t.Helper()
		if err := s.RecordSamples(ctx, samples, force); err != nil {
			t.Fatal(err)
		}
	}
	record(store, false, sample(at, "primary", 40), sample(at, "secondary", 10))
	// Another process polling the same account must not duplicate the rows.
	record(New(store.Path()), false, sample(at.Add(time.Minute), "primary", 40), sample(at.Add(time.Minute), "secondary", 10))
	record(store, false, sample(at.Add(2*time.Minute), "primary", 41), sample(at.Add(2*time.Minute), "secondary", 10))
	record(store, false, sample(at.Add(2*time.Minute+Heartbeat-time.Second), "primary", 41))
	record(store, false, sample(at.Add(2*time.Minute+Heartbeat), "primary", 41))
	record(store, true, sample(at.Add(3*time.Minute+Heartbeat), "primary", 41))
	other := sample(at.Add(4*time.Minute+Heartbeat), "primary", 41)
	other.Account = "other"
	record(store, false, other)

	var got []string
	for _, s := range readSamples(t, store) {
		got = append(got, fmt.Sprintf("%s %s %s %g", s.Time.Sub(at), s.Account, s.Dimension, s.UsedPercent))
	}
	want := []string{
		"0s abc123 primary 40", "0s abc123 secondary 10", "2m0s abc123 primary 41",
		"17m0s abc123 primary 41", "18m0s abc123 primary 41", "19m0s other primary 41",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved samples:\n got %q\nwant %q", got, want)
	}
	first := readSamples(t, store)[0]
	if want := sample(at, "primary", 40); first.RemainingPercent != 60 || first.Plan != "pro" || *first.WindowMins != 300 ||
		*first.ResetCredits != 2 || !first.ResetsAt.Equal(*want.ResetsAt) {
		t.Fatalf("sample fields changed in storage: %+v", first)
	}
}

func TestSamplesKeepMissingValuesMissing(t *testing.T) {
	store := testStore(t)
	at := time.Unix(1_800_000_000, 0).UTC()
	want := Sample{Time: at, LimitID: "codex", Dimension: "individual", UsedPercent: 100, RemainingPercent: 0}
	for i := 0; i < 2; i++ {
		if err := store.RecordSamples(context.Background(), []Sample{want}, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := readSamples(t, store); !reflect.DeepEqual(got, []Sample{want}) {
		t.Fatalf("got %+v", got)
	}
}

func TestPruneDeletesOldSamplesButNeverResetEvents(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)
	if err := store.RecordSamples(ctx, []Sample{sample(at, "primary", 1)}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSamples(ctx, []Sample{sample(at.Add(time.Hour), "primary", 2)}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResetEvent(ctx, ResetEvent{Time: at, Key: "k", Event: "requested", Mode: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(ctx, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if samples, events := readSamples(t, store), readEvents(t, store); len(samples) != 1 || samples[0].UsedPercent != 2 || len(events) != 1 {
		t.Fatalf("samples=%+v events=%+v", samples, events)
	}
}

func TestDatabaseIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	store := testStore(t)
	if err := store.RecordResetEvent(context.Background(), ResetEvent{Time: time.Now(), Event: "skipped", Mode: "auto"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", entry.Name(), info.Mode().Perm())
		}
	}
	if info, err := os.Stat(filepath.Dir(store.Path())); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode: %v err=%v", info.Mode().Perm(), err)
	}
}

func TestReadingCreatesNothing(t *testing.T) {
	store := testStore(t)
	if samples, events := readSamples(t, store), readEvents(t, store); samples != nil || events != nil {
		t.Fatalf("samples=%v events=%v", samples, events)
	}
	if _, err := os.Stat(filepath.Dir(store.Path())); !os.IsNotExist(err) {
		t.Fatalf("reading created the history directory: %v", err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.RecordResetEvent(ctx, ResetEvent{Time: time.Now(), Event: "skipped", Mode: "auto"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.run(ctx, false, fmt.Sprintf("PRAGMA user_version=%d;\n", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	upgraded := New(store.Path())
	err := upgraded.RecordSamples(ctx, []Sample{sample(time.Now(), "primary", 1)}, true)
	if err == nil || !strings.Contains(err.Error(), "upgrade ccodex") {
		t.Fatalf("write to a newer schema: %v", err)
	}
	if err := upgraded.ResetEvents(ctx, time.Time{}, func(ResetEvent) error { return nil }); err == nil {
		t.Fatal("read of a newer schema succeeded")
	}
}

func TestProcessesShareOneDatabase(t *testing.T) {
	store := testStore(t)
	const writers, each = 4, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			own := New(store.Path())
			for i := 0; i < each; i++ {
				errs <- own.RecordResetEvent(context.Background(), ResetEvent{
					Time: time.Now(), Key: fmt.Sprintf("w%d-%d", w, i), Event: "requested", Mode: "manual",
				})
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if events := readEvents(t, store); len(events) != writers*each {
		t.Fatalf("saved %d of %d events", len(events), writers*each)
	}
}

func TestSymlinkedDatabaseIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	store := testStore(t)
	dir := filepath.Dir(store.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.db"), store.Path()); err != nil {
		t.Fatal(err)
	}
	err := store.RecordResetEvent(context.Background(), ResetEvent{Time: time.Now(), Event: "skipped", Mode: "auto"})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlinked database: %v", err)
	}
}
