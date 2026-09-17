package resetbudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

var testDate = time.Date(2026, 9, 15, 23, 59, 0, 0, time.FixedZone("local", -4*60*60))

func newTestStore(t *testing.T) Store {
	t.Helper()
	return Store{Path: filepath.Join(t.TempDir(), "ccodex", "auto-resets.json"), Now: func() time.Time { return testDate }}
}

func reserve(t *testing.T, store Store, key string, limit int, now time.Time, want bool) {
	t.Helper()
	got, err := store.Reserve(context.Background(), key, limit, now)
	if err != nil || got != want {
		t.Fatalf("Reserve(%q, %d, %s) = %v, %v; want %v", key, limit, now, got, err, want)
	}
}

func readTestLedger(t *testing.T, store Store) ledger {
	t.Helper()
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	var state ledger
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestReservationsSurviveRestartAndRetriesChargeOnce(t *testing.T) {
	store := newTestStore(t)
	reserve(t, store, "first", 1, testDate, true)
	// A fresh Store models a restarted process with no in-memory accounting.
	restarted := Store{Path: store.Path, Now: store.Now}
	reserve(t, restarted, "second", 1, testDate, false)
	for range 3 {
		reserve(t, restarted, "first", 1, testDate, true)
	}
	state := readTestLedger(t, restarted)
	if len(state.Entries) != 1 || len(state.Entries["first"].Days) != 1 || state.Entries["first"].Status != "pending" {
		t.Fatalf("unexpected pending accounting: %+v", state)
	}
	if err := restarted.Complete(context.Background(), "first", "reset"); err != nil {
		t.Fatal(err)
	}
	reserve(t, Store{Path: store.Path}, "second", 1, testDate, false)
	if state := readTestLedger(t, store); state.Entries["first"].Status != "success" {
		t.Fatalf("confirmed charge was not saved: %+v", state)
	}
}

func TestConcurrentStoresEnforceOneSharedSlot(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const contenders = 16
	start := make(chan struct{})
	results := make(chan bool, contenders)
	errorsSeen := make(chan error, contenders)
	var workers sync.WaitGroup
	for i := range contenders {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			accepted, err := (Store{Path: store.Path}).Reserve(ctx, fmt.Sprintf("request-%d", i), 1, testDate)
			results <- accepted
			errorsSeen <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted := 0
	for result := range results {
		if result {
			accepted++
		}
	}
	if accepted != 1 || len(readTestLedger(t, store).Entries) != 1 {
		t.Fatalf("%d concurrent requests reserved a budget of one", accepted)
	}
}

func TestDayRolloverAndPendingRetryReserveEachLocalDate(t *testing.T) {
	store := newTestStore(t)
	nextDay := testDate.Add(2 * time.Minute)
	reserve(t, store, "pending", 1, testDate, true)
	reserve(t, store, "pending", 1, nextDay, true)
	reserve(t, store, "other", 1, nextDay, false)
	state := readTestLedger(t, store)
	days := state.Entries["pending"].Days
	if len(days) != 2 || days[0] != "2026-09-15" || days[1] != "2026-09-16" {
		t.Fatalf("pending retry was not charged on each local date: %+v", days)
	}
	if err := store.Complete(context.Background(), "pending", "alreadyRedeemed"); err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "other", 1, testDate, false)
	reserve(t, store, "other", 1, nextDay, false)
	reserve(t, store, "other", 1, nextDay.Add(24*time.Hour), true)
}

func TestOldPendingRequestBlocksNewSpendingUntilResolved(t *testing.T) {
	store := newTestStore(t)
	reserve(t, store, "pending", 1, testDate, true)
	nextDay := testDate.Add(2 * time.Minute)
	reserve(t, store, "new", 1, nextDay, false)
	reserve(t, store, "new", 1, testDate.Add(10*24*time.Hour), false)
	// Retrying the outstanding request must not count it twice.
	reserve(t, store, "pending", 1, nextDay, true)
	reserve(t, store, "new", 1, nextDay, false)
	if err := store.Complete(context.Background(), "pending", "nothingToReset"); err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "new", 1, nextDay, true)
}

func TestCompletionAfterMidnightChargesCompletionDate(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed"} {
		t.Run(outcome, func(t *testing.T) {
			store := newTestStore(t)
			nextDay := testDate.Add(2 * time.Minute)
			store.Now = func() time.Time { return nextDay }
			reserve(t, store, "pending", 1, testDate, true)
			// The first attempt returns after midnight, without another Reserve.
			if err := store.Complete(context.Background(), "pending", outcome); err != nil {
				t.Fatal(err)
			}
			state := readTestLedger(t, store)
			days := state.Entries["pending"].Days
			if len(days) != 2 || days[0] != "2026-09-15" || days[1] != "2026-09-16" || state.Entries["pending"].Status != "success" {
				t.Fatalf("completion date was not saved: %+v", state)
			}
			reserve(t, store, "new", 1, testDate, false)
			reserve(t, store, "new", 1, nextDay, false)
			reserve(t, store, "new", 1, nextDay.Add(24*time.Hour), true)
		})
	}
}

func TestSameDayRetrySurvivesCapReduction(t *testing.T) {
	store := newTestStore(t)
	reserve(t, store, "first", 2, testDate, true)
	reserve(t, store, "second", 2, testDate, true)
	reserve(t, store, "first", 1, testDate, true)
	reserve(t, store, "third", 1, testDate, false)
	reserve(t, store, "first", 0, testDate, false)
}

func TestPendingFiltersByPrefixAndReturnsFirstSortedKey(t *testing.T) {
	store := newTestStore(t)
	for _, key := range []string{"auto-account-z", "auto-account-a", "auto-other-0", "auto-account-0"} {
		reserve(t, store, key, 4, testDate, true)
	}
	if err := store.Complete(context.Background(), "auto-account-0", "reset"); err != nil {
		t.Fatal(err)
	}
	// A restarted process discovers the original request key deterministically.
	restarted := Store{Path: store.Path, Now: store.Now}
	for range 5 {
		if key, err := restarted.Pending(context.Background(), "auto-account-"); err != nil || key != "auto-account-a" {
			t.Fatalf("Pending selected %q, %v; want auto-account-a", key, err)
		}
	}
	if err := restarted.Complete(context.Background(), "auto-account-a", "noCredit"); err != nil {
		t.Fatal(err)
	}
	if key, err := restarted.Pending(context.Background(), "auto-account-"); err != nil || key != "auto-account-z" {
		t.Fatalf("Pending selected %q, %v; want auto-account-z", key, err)
	}
	if err := restarted.Complete(context.Background(), "auto-account-z", "alreadyRedeemed"); err != nil {
		t.Fatal(err)
	}
	if key, err := restarted.Pending(context.Background(), "auto-account-"); err != nil || key != "" {
		t.Fatalf("resolved prefix still has pending request %q, %v", key, err)
	}
	if key, err := restarted.Pending(context.Background(), ""); err != nil || key != "auto-other-0" {
		t.Fatalf("Pending with empty prefix selected %q, %v; want auto-other-0", key, err)
	}
}

func TestPendingMidnightRetryWaitsWhenNewDayIsFull(t *testing.T) {
	store := newTestStore(t)
	nextDay := testDate.Add(2 * time.Minute)
	reserve(t, store, "pending", 2, testDate, true)
	reserve(t, store, "today", 2, nextDay, true)
	reserve(t, store, "pending", 1, nextDay, false)
	if err := store.Complete(context.Background(), "today", "nothingToReset"); err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "pending", 1, nextDay, true)
}

func TestNoOpReleasesAllDatesAndUnknownOutcomeKeepsThem(t *testing.T) {
	for _, outcome := range []string{"nothingToReset", "noCredit"} {
		t.Run(outcome, func(t *testing.T) {
			store := newTestStore(t)
			nextDay := testDate.Add(2 * time.Minute)
			reserve(t, store, "first", 1, testDate, true)
			reserve(t, store, "first", 1, nextDay, true)
			for _, unknown := range []string{"", "unknown"} {
				if err := store.Complete(context.Background(), "first", unknown); err != nil {
					t.Fatal(err)
				}
				reserve(t, store, "second", 1, testDate, false)
				reserve(t, store, "second", 1, nextDay, false)
			}
			for range 2 {
				if err := store.Complete(context.Background(), "first", outcome); err != nil {
					t.Fatal(err)
				}
			}
			if len(readTestLedger(t, store).Entries) != 0 {
				t.Fatal("known no-op left a reservation")
			}
			reserve(t, store, "second", 1, testDate, true)
			if err := store.Complete(context.Background(), "second", "reset"); err != nil {
				t.Fatal(err)
			}
			reserve(t, store, "third", 1, nextDay, true)
		})
	}
}

func TestCompleteSuccessIsIdempotent(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed"} {
		t.Run(outcome, func(t *testing.T) {
			store := newTestStore(t)
			reserve(t, store, "first", 1, testDate, true)
			for range 2 {
				if err := store.Complete(context.Background(), "first", outcome); err != nil {
					t.Fatal(err)
				}
			}
			reserve(t, store, "second", 1, testDate, false)
			if state := readTestLedger(t, store); state.Entries["first"].Status != "success" {
				t.Fatalf("confirmed charge was not saved: %+v", state)
			}
		})
	}
}

func TestContradictoryNoOpCannotReleaseConfirmedSpending(t *testing.T) {
	for _, outcome := range []string{"nothingToReset", "noCredit"} {
		t.Run(outcome, func(t *testing.T) {
			store := newTestStore(t)
			reserve(t, store, "first", 1, testDate, true)
			if err := store.Complete(context.Background(), "first", "reset"); err != nil {
				t.Fatal(err)
			}
			if err := store.Complete(context.Background(), "first", outcome); err == nil {
				t.Fatal("contradictory no-op was accepted")
			}
			reserve(t, store, "second", 1, testDate, false)
			if state := readTestLedger(t, store); state.Entries["first"].Status != "success" {
				t.Fatalf("contradictory no-op altered confirmed spending: %+v", state)
			}
		})
	}
}

func TestInvalidStateFailsClosedWithoutReplacement(t *testing.T) {
	for _, data := range []string{
		"", "not json", "null", "{}",
		`{"version":2,"entries":{}}`,
		`{"version":1,"entries":null}`,
		`{"version":1,"entries":{}} {}`,
		`{"version":1,"entries":{},"extra":true}`,
		`{"version":1,"entries":{"key":{"days":[],"status":"pending"}}}`,
		`{"version":1,"entries":{"key":{"days":["2026-09-15"],"status":"other"}}}`,
		`{"version":1,"entries":{"key":{"days":["bad"],"status":"pending"}}}`,
		`{"version":1,"entries":{"key":{"days":["2026-09-15","2026-09-15"],"status":"pending"}}}`,
	} {
		t.Run(data, func(t *testing.T) {
			store := newTestStore(t)
			if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if accepted, err := store.Reserve(context.Background(), "new", 1, testDate); err == nil || accepted {
				t.Fatalf("invalid accounting permitted a reservation: accepted=%v err=%v", accepted, err)
			}
			if err := store.Complete(context.Background(), "key", "noCredit"); err == nil {
				t.Fatal("completion accepted invalid accounting")
			}
			if key, err := store.Pending(context.Background(), ""); err == nil || key != "" {
				t.Fatalf("pending lookup accepted invalid accounting: %q, %v", key, err)
			}
			if saved, err := os.ReadFile(store.Path); err != nil || string(saved) != data {
				t.Fatalf("invalid accounting was overwritten: %q, %v", saved, err)
			}
		})
	}
}

func TestNonRegularAndUnreadableStateFailClosed(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(store.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.Reserve(context.Background(), "new", 1, testDate); err == nil || accepted {
		t.Fatalf("directory accepted as accounting: accepted=%v err=%v", accepted, err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	unreadable := newTestStore(t)
	reserve(t, unreadable, "first", 1, testDate, true)
	if err := os.Chmod(unreadable.Path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable.Path, 0o600) })
	if accepted, err := unreadable.Reserve(context.Background(), "new", 1, testDate); err == nil || accepted {
		t.Fatalf("unreadable accounting accepted: accepted=%v err=%v", accepted, err)
	}
}

func TestLockHonorsContextDeadline(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(store.Path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if accepted, err := store.Reserve(ctx, "first", 1, testDate); !errors.Is(err, context.DeadlineExceeded) || accepted {
		t.Fatalf("lock did not honor deadline: accepted=%v err=%v", accepted, err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked reservation wrote state: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "first", 1, testDate, true)
}

func TestOperationLockSerializesStoresWithoutBlockingLedger(t *testing.T) {
	store := newTestStore(t)
	release, err := store.LockOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// The operation guard must not hold the ledger lock across backend I/O.
	ledgerContext, cancelLedger := context.WithTimeout(context.Background(), time.Second)
	defer cancelLedger()
	if accepted, err := store.Reserve(ledgerContext, "first", 1, testDate); err != nil || !accepted {
		t.Fatalf("operation lock blocked its ledger reservation: %v, %v", accepted, err)
	}
	if err := store.Complete(ledgerContext, "first", "nothingToReset"); err != nil {
		t.Fatal(err)
	}
	// Releasing a reservation must not let another process start a reset
	// until the enclosing operation has also released its separate guard.
	other := Store{Path: store.Path, Now: store.Now}
	waitContext, cancelWait := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancelWait()
	if nextRelease, err := other.LockOperation(waitContext); !errors.Is(err, context.DeadlineExceeded) || nextRelease != nil {
		if nextRelease != nil {
			_ = nextRelease()
		}
		t.Fatalf("concurrent operation was not blocked by the guard: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	acquireContext, cancelAcquire := context.WithTimeout(context.Background(), time.Second)
	defer cancelAcquire()
	nextRelease, err := other.LockOperation(acquireContext)
	if err != nil {
		t.Fatalf("released operation guard could not be reacquired: %v", err)
	}
	if err := nextRelease(); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherLockAdmitsOneHolderWithoutWaiting(t *testing.T) {
	store := newTestStore(t)
	release, err := store.LockWatcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Another process is refused at once rather than queued behind the holder.
	other := Store{Path: store.Path, Now: store.Now}
	started := time.Now()
	if nextRelease, err := other.LockWatcher(context.Background()); !errors.Is(err, ErrWatcherActive) || nextRelease != nil {
		t.Fatalf("second watcher was admitted: %v", err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("refusal took %v", waited)
	}
	// The slot is separate from the locks that guard each reset and the ledger.
	operationContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseOperation, err := other.LockOperation(operationContext)
	if err != nil {
		t.Fatalf("watcher lock blocked a reset operation: %v", err)
	}
	reserve(t, other, "first", 1, testDate, true)
	if err := releaseOperation(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	nextRelease, err := other.LockWatcher(context.Background())
	if err != nil {
		t.Fatalf("released watcher slot could not be claimed: %v", err)
	}
	if err := nextRelease(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{}).LockWatcher(context.Background()); err == nil || errors.Is(err, ErrWatcherActive) {
		t.Fatalf("empty path: %v", err)
	}
}

func TestZeroLimitAndInvalidInput(t *testing.T) {
	store := newTestStore(t)
	reserve(t, store, "first", 0, testDate, false)
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled budget created state: %v", err)
	}
	reserve(t, store, "first", 1, testDate, true)
	reserve(t, store, "first", 0, testDate, false)
	for _, test := range []struct {
		key   string
		limit int
	}{{"key", -1}, {"", 1}, {"  ", 1}} {
		if accepted, err := store.Reserve(context.Background(), test.key, test.limit, testDate); err == nil || accepted {
			t.Fatalf("invalid input accepted: %+v: %v, %v", test, accepted, err)
		}
	}
	if accepted, err := (Store{}).Reserve(context.Background(), "key", 1, testDate); err == nil || accepted {
		t.Fatalf("empty path accepted: %v, %v", accepted, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if accepted, err := store.Reserve(ctx, "key", 1, testDate); !errors.Is(err, context.Canceled) || accepted {
		t.Fatalf("canceled reservation accepted: %v, %v", accepted, err)
	}
}

func TestCreatedStateAndDirectoryArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses ACLs instead of Unix mode bits")
	}
	store := newTestStore(t)
	for _, lock := range []func(context.Context) (func() error, error){store.LockOperation, store.LockWatcher} {
		release, err := lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
	reserve(t, store, "first", 1, testDate, true)
	for path, want := range map[string]os.FileMode{store.Path: 0o600, store.Path + ".lock": 0o600, store.Path + ".operation.lock": 0o600, store.Path + ".watcher.lock": 0o600, filepath.Dir(store.Path): 0o700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s should have mode %o: info=%v err=%v", path, want, info, err)
		}
	}
}
