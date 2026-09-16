// Package resetbudget limits automatic reset spending across local processes
// and restarts. Pending requests count against the budget until their outcome
// is known, because an interrupted request may already have consumed a reset.
package resetbudget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	stateVersion = 1
	dateLayout   = "2006-01-02"
)

// Store is shared by every watch process using Path. The adjacent .lock file
// must remain at a stable path while the JSON state is atomically replaced.
type Store struct {
	Path string
	// Now supplies the local completion time; nil uses time.Now.
	Now func() time.Time
}

type entry struct {
	Days   []string `json:"days"`
	Status string   `json:"status"`
}

type ledger struct {
	Version int              `json:"version"`
	Entries map[string]entry `json:"entries"`
}

// DefaultPath returns the current user's shared automatic-reset budget path.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find automatic-reset budget directory: %w", err)
	}
	return filepath.Join(dir, "ccodex", "auto-resets.json"), nil
}

// LockOperation serializes the complete automatic reset operation across
// processes. Hold it from pending-key lookup through recording the outcome.
// Ledger methods use a separate lock and remain usable while this lock is held.
func (s Store) LockOperation(ctx context.Context) (release func() error, err error) {
	lock, err := s.acquireLock(ctx, ".operation.lock")
	if err != nil {
		return nil, err
	}
	return lock.Unlock, nil
}

// Reserve durably charges key to now's local calendar date before returning
// true. Retries with the same key do not consume another slot on that date.
// Pending requests count on every date until resolved. An unresolved retry
// crossing midnight records the new date without counting itself twice.
func (s Store) Reserve(ctx context.Context, key string, limit int, now time.Time) (bool, error) {
	if limit < 0 {
		return false, errors.New("daily automatic-reset limit must not be negative")
	}
	if strings.TrimSpace(key) == "" {
		return false, errors.New("automatic-reset request key must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if limit == 0 {
		return false, nil
	}
	day := now.Format(dateLayout)
	allowed := false
	err := s.withLedger(ctx, func(state *ledger) (bool, error) {
		current, exists := state.Entries[key]
		if containsDay(current.Days, day) {
			allowed = true
			return false, nil
		}
		used := 0
		for savedKey, saved := range state.Entries {
			if savedKey != key && (saved.Status == "pending" || containsDay(saved.Days, day)) {
				used++
			}
		}
		if used >= limit {
			return false, nil
		}
		if !exists {
			current.Status = "pending"
		}
		current.Days = append(current.Days, day)
		state.Entries[key] = current
		allowed = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// Pending returns the lexically first unresolved request with prefix, or an
// empty string if none exists. This permits safe same-key retries after restart.
func (s Store) Pending(ctx context.Context, prefix string) (string, error) {
	key := ""
	err := s.withLedger(ctx, func(state *ledger) (bool, error) {
		for candidate, saved := range state.Entries {
			if saved.Status == "pending" && strings.HasPrefix(candidate, prefix) && (key == "" || candidate < key) {
				key = candidate
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

// Complete keeps charges for a confirmed redemption and releases reservations
// only when the server confirms no reset was consumed. Unknown outcomes keep
// their reservations. Completion is idempotent for an already-recorded result.
func (s Store) Complete(ctx context.Context, key string, outcome string) error {
	switch outcome {
	case "reset", "alreadyRedeemed", "nothingToReset", "noCredit":
	default:
		return nil
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("automatic-reset request key must not be empty")
	}
	return s.withLedger(ctx, func(state *ledger) (bool, error) {
		current, exists := state.Entries[key]
		if outcome == "nothingToReset" || outcome == "noCredit" {
			if current.Status == "success" {
				return false, errors.New("automatic-reset no-op contradicts a saved successful redemption; budget charge preserved")
			}
			delete(state.Entries, key)
			return exists, nil
		}
		if !exists {
			return false, errors.New("automatic-reset request has no saved budget reservation")
		}
		changed := current.Status != "success"
		clock := s.Now
		if clock == nil {
			clock = time.Now
		}
		day := clock().Format(dateLayout)
		if !containsDay(current.Days, day) {
			current.Days = append(current.Days, day)
			changed = true
		}
		current.Status = "success"
		state.Entries[key] = current
		return changed, nil
	})
}

func (s Store) withLedger(ctx context.Context, update func(*ledger) (bool, error)) (err error) {
	lock, err := s.acquireLock(ctx, ".lock")
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock automatic-reset budget: %w", unlockErr))
		}
	}()
	state, err := s.read()
	if err != nil {
		return err
	}
	changed, err := update(state)
	if err != nil || !changed {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.write(state)
}

func (s Store) acquireLock(ctx context.Context, suffix string) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.Path) == "" {
		return nil, errors.New("automatic-reset budget path must not be empty")
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create automatic-reset budget directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect automatic-reset budget directory: %w", err)
	}
	lock := flock.New(s.Path+suffix, flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("lock automatic-reset budget%s: %w", suffix, err)
	}
	if !locked {
		return nil, errors.New("automatic-reset budget lock was not acquired")
	}
	if err := ctx.Err(); err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	if err := os.Chmod(lock.Path(), 0o600); err != nil {
		_ = lock.Unlock()
		return nil, fmt.Errorf("protect automatic-reset budget lock: %w", err)
	}
	return lock, nil
}

func (s Store) read() (*ledger, error) {
	info, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &ledger{Version: stateVersion, Entries: make(map[string]entry)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect automatic-reset budget: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("automatic-reset budget must be a regular file")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read automatic-reset budget: %w", err)
	}
	var state ledger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode automatic-reset budget: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("automatic-reset budget has trailing data")
	}
	if state.Version != stateVersion || state.Entries == nil {
		return nil, errors.New("automatic-reset budget has an unsupported version or missing entries")
	}
	for key, saved := range state.Entries {
		if strings.TrimSpace(key) == "" || len(saved.Days) == 0 || (saved.Status != "pending" && saved.Status != "success") {
			return nil, errors.New("automatic-reset budget contains an invalid entry")
		}
		seen := make(map[string]bool, len(saved.Days))
		for _, day := range saved.Days {
			parsed, err := time.Parse(dateLayout, day)
			if err != nil || parsed.Format(dateLayout) != day || seen[day] {
				return nil, errors.New("automatic-reset budget contains an invalid or duplicate date")
			}
			seen[day] = true
		}
	}
	return &state, nil
}

func (s Store) write(state *ledger) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode automatic-reset budget: %w", err)
	}
	dir := filepath.Dir(s.Path)
	file, err := os.CreateTemp(dir, ".auto-resets-*")
	if err != nil {
		return fmt.Errorf("create automatic-reset budget replacement: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect automatic-reset budget replacement: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write automatic-reset budget replacement: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync automatic-reset budget replacement: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close automatic-reset budget replacement: %w", err)
	}
	if err := os.Rename(file.Name(), s.Path); err != nil {
		return fmt.Errorf("replace automatic-reset budget: %w", err)
	}
	// Windows does not support syncing directory handles through os.File.
	if runtime.GOOS != "windows" {
		parent, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("open automatic-reset budget directory for sync: %w", err)
		}
		defer parent.Close()
		if err := parent.Sync(); err != nil {
			return fmt.Errorf("sync automatic-reset budget directory: %w", err)
		}
	}
	return nil
}

func containsDay(days []string, day string) bool {
	for _, saved := range days {
		if saved == day {
			return true
		}
	}
	return false
}
