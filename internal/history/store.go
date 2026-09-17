// Package history keeps a local record of quota samples and reset events in a
// SQLite database. It drives the sqlite3 command-line tool instead of linking a
// driver, which keeps ccodex free of cgo; on macOS the tool is part of the OS.
package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	schemaVersion = 1
	// Heartbeat bounds how long an unchanged quota dimension goes unrecorded.
	Heartbeat = 15 * time.Minute
	// Apple's sqlite3 is preferred over a newer one on PATH so every macOS
	// user gets the same, known feature set.
	systemBinary = "/usr/bin/sqlite3"
)

// migrations[v] upgrades a database from schema version v to v+1. Two processes
// may apply the same migration, so each one must be idempotent.
var migrations = [schemaVersion]string{`PRAGMA journal_mode=WAL;
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS samples (
  id            INTEGER PRIMARY KEY,
  ts            INTEGER NOT NULL, -- Unix seconds
  account       TEXT NOT NULL,    -- short hash of the account ID; '' if unknown
  plan          TEXT,
  limit_id      TEXT NOT NULL,
  dimension     TEXT NOT NULL,    -- primary | secondary | individual
  window_mins   INTEGER,
  used_pct      REAL NOT NULL,
  resets_at     INTEGER,          -- Unix seconds
  reset_credits INTEGER
);
CREATE INDEX IF NOT EXISTS samples_series ON samples(account, limit_id, dimension, ts);
CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts);
CREATE TABLE IF NOT EXISTS reset_events (
  id      INTEGER PRIMARY KEY,
  ts      INTEGER NOT NULL, -- Unix seconds
  key     TEXT,             -- idempotency key; NULL if no request was made
  event   TEXT NOT NULL,    -- requested | outcome | error | skipped
  mode    TEXT NOT NULL,    -- auto | manual
  account TEXT,
  outcome TEXT,
  detail  TEXT,
  reason  TEXT,             -- JSON
  version TEXT              -- ccodex version that wrote the row
);
CREATE INDEX IF NOT EXISTS reset_events_key ON reset_events(key);
CREATE INDEX IF NOT EXISTS reset_events_ts ON reset_events(ts);
-- One row per request key: its first request and its latest known outcome.
CREATE VIEW IF NOT EXISTS resets AS
SELECT r.key, r.mode, r.account, r.ts AS requested_at, r.reason,
       o.ts AS resolved_at, o.outcome
FROM reset_events r
LEFT JOIN reset_events o ON o.id = (
  SELECT MAX(id) FROM reset_events WHERE key = r.key AND event = 'outcome')
WHERE r.event = 'requested'
  AND r.id = (SELECT MIN(id) FROM reset_events WHERE key = r.key AND event = 'requested');
PRAGMA user_version=1;
COMMIT;
`}

// Sample is one quota dimension as reported by one successful refresh.
type Sample struct {
	Time             time.Time  `json:"time"`
	Account          string     `json:"account"`
	Plan             string     `json:"plan,omitempty"`
	LimitID          string     `json:"limitId"`
	Dimension        string     `json:"dimension"`
	WindowMins       *int64     `json:"windowMins,omitempty"`
	UsedPercent      float64    `json:"usedPercent"`
	RemainingPercent float64    `json:"remainingPercent"`
	ResetsAt         *time.Time `json:"resetsAt,omitempty"`
	ResetCredits     *int64     `json:"resetCredits,omitempty"`
}

// ResetEvent is one step of a reset attempt, or a reason none was made. Rows
// are only ever appended: a request whose outcome was never learned stays
// visible as a "requested" event without a matching "outcome".
type ResetEvent struct {
	Time    time.Time `json:"time"`
	Key     string    `json:"key,omitempty"`
	Event   string    `json:"event"`
	Mode    string    `json:"mode"`
	Account string    `json:"account,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Reason  *Reason   `json:"reason,omitempty"`
	Version string    `json:"version,omitempty"`
}

// Reason explains why a reset was requested.
type Reason struct {
	Trigger         string     `json:"trigger"` // lowQuota | manual
	Threshold       *float64   `json:"threshold,omitempty"`
	Low             []LowQuota `json:"low,omitempty"`
	ResetCredits    *int64     `json:"resetCredits,omitempty"`
	MaxResetsPerDay int        `json:"maxResetsPerDay,omitempty"`
	Retry           bool       `json:"retry,omitempty"`
	CreditID        string     `json:"creditId,omitempty"`
}

// LowQuota is a quota dimension that was below the threshold.
type LowQuota struct {
	LimitID          string  `json:"limitId"`
	Dimension        string  `json:"dimension"`
	RemainingPercent float64 `json:"remainingPercent"`
}

// Rows cross the sqlite3 boundary as JSON in both directions, with times as
// Unix seconds.
type sampleRow struct {
	TS           int64   `json:"ts"`
	Account      string  `json:"account"`
	Plan         string  `json:"plan"`
	LimitID      string  `json:"limitId"`
	Dimension    string  `json:"dimension"`
	WindowMins   *int64  `json:"windowMins"`
	UsedPercent  float64 `json:"usedPercent"`
	ResetsAt     *int64  `json:"resetsAt"`
	ResetCredits *int64  `json:"resetCredits"`
}

type eventRow struct {
	TS      int64   `json:"ts"`
	Key     string  `json:"key"`
	Event   string  `json:"event"`
	Mode    string  `json:"mode"`
	Account string  `json:"account"`
	Outcome string  `json:"outcome"`
	Detail  string  `json:"detail"`
	Reason  *Reason `json:"reason"`
	Version string  `json:"version"`
}

// Store is safe for concurrent use, and several processes may share one path.
type Store struct {
	path   string
	binary string // empty selects the system sqlite3

	mu    sync.Mutex
	ready bool
}

// New returns a store for the database at path. Nothing is created until the
// first write.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns the current user's history database, which sits beside
// the automatic-reset budget.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find history directory: %w", err)
	}
	return filepath.Join(dir, "ccodex", "history.db"), nil
}

func (s *Store) Path() string { return s.path }

// RecordSamples appends the samples of one refresh. Unless force is set, a
// sample that repeats the latest saved values for its account, limit, and
// dimension is dropped until Heartbeat has passed, even if another process
// saved them.
func (s *Store) RecordSamples(ctx context.Context, samples []Sample, force bool) error {
	if len(samples) == 0 {
		return nil
	}
	rows := make([]sampleRow, 0, len(samples))
	for _, sample := range samples {
		row := sampleRow{
			TS: sample.Time.Unix(), Account: sample.Account, Plan: sample.Plan,
			LimitID: sample.LimitID, Dimension: sample.Dimension, WindowMins: sample.WindowMins,
			UsedPercent: sample.UsedPercent, ResetCredits: sample.ResetCredits,
		}
		if sample.ResetsAt != nil {
			resetsAt := sample.ResetsAt.Unix()
			row.ResetsAt = &resetsAt
		}
		rows = append(rows, row)
	}
	data, err := literal(rows)
	if err != nil {
		return err
	}
	if err := s.prepare(ctx); err != nil {
		return err
	}
	keep := "1"
	if !force {
		keep = fmt.Sprintf(`NOT EXISTS (
  SELECT 1 FROM samples p
  WHERE p.id = (SELECT s.id FROM samples s
                WHERE s.account = n.account AND s.limit_id = n.limit_id AND s.dimension = n.dimension
                ORDER BY s.ts DESC, s.id DESC LIMIT 1)
    AND p.used_pct = n.used_pct AND p.window_mins IS n.window_mins
    AND p.reset_credits IS n.reset_credits AND p.plan IS n.plan
    AND n.ts - p.ts < %d)`, int64(Heartbeat/time.Second))
	}
	_, err = s.run(ctx, false, fmt.Sprintf(`INSERT INTO samples(ts, account, plan, limit_id, dimension, window_mins, used_pct, resets_at, reset_credits)
SELECT * FROM (
  SELECT json_extract(value, '$.ts') AS ts, json_extract(value, '$.account') AS account,
         NULLIF(json_extract(value, '$.plan'), '') AS plan, json_extract(value, '$.limitId') AS limit_id,
         json_extract(value, '$.dimension') AS dimension, json_extract(value, '$.windowMins') AS window_mins,
         json_extract(value, '$.usedPercent') AS used_pct, json_extract(value, '$.resetsAt') AS resets_at,
         json_extract(value, '$.resetCredits') AS reset_credits
  FROM json_each(%s)) n
WHERE %s;
`, data, keep))
	return err
}

// RecordResetEvent appends one event and returns only after sqlite3 commits it.
func (s *Store) RecordResetEvent(ctx context.Context, event ResetEvent) error {
	data, err := literal(eventRow{
		TS: event.Time.Unix(), Key: event.Key, Event: event.Event, Mode: event.Mode, Account: event.Account,
		Outcome: event.Outcome, Detail: event.Detail, Reason: event.Reason, Version: event.Version,
	})
	if err != nil {
		return err
	}
	if err := s.prepare(ctx); err != nil {
		return err
	}
	_, err = s.run(ctx, false, fmt.Sprintf(`INSERT INTO reset_events(ts, key, event, mode, account, outcome, detail, reason, version)
SELECT json_extract(j, '$.ts'), NULLIF(json_extract(j, '$.key'), ''), json_extract(j, '$.event'),
       json_extract(j, '$.mode'), NULLIF(json_extract(j, '$.account'), ''), NULLIF(json_extract(j, '$.outcome'), ''),
       NULLIF(json_extract(j, '$.detail'), ''), json_extract(j, '$.reason'), NULLIF(json_extract(j, '$.version'), '')
FROM (SELECT json(%s) AS j);
`, data))
	return err
}

// Prune deletes samples older than before. Reset events are never deleted.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	if err := s.prepare(ctx); err != nil {
		return err
	}
	_, err := s.run(ctx, false, fmt.Sprintf("DELETE FROM samples WHERE ts < %d;\n", before.Unix()))
	return err
}

// Samples calls fn for each sample at or after since, oldest first. A missing
// database has no samples; reading never creates one.
func (s *Store) Samples(ctx context.Context, since time.Time, fn func(Sample) error) error {
	return s.query(ctx, fmt.Sprintf(`SELECT json_object('ts', ts, 'account', account, 'plan', plan, 'limitId', limit_id,
  'dimension', dimension, 'windowMins', window_mins, 'usedPercent', used_pct, 'resetsAt', resets_at,
  'resetCredits', reset_credits)
FROM samples WHERE ts >= %d ORDER BY ts, id;
`, since.Unix()), func(line []byte) error {
		var row sampleRow
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode history sample: %w", err)
		}
		sample := Sample{
			Time: time.Unix(row.TS, 0).UTC(), Account: row.Account, Plan: row.Plan, LimitID: row.LimitID,
			Dimension: row.Dimension, WindowMins: row.WindowMins, UsedPercent: row.UsedPercent,
			RemainingPercent: 100 - row.UsedPercent, ResetCredits: row.ResetCredits,
		}
		if row.ResetsAt != nil {
			resetsAt := time.Unix(*row.ResetsAt, 0).UTC()
			sample.ResetsAt = &resetsAt
		}
		return fn(sample)
	})
}

// ResetEvents calls fn for each reset event at or after since, oldest first.
func (s *Store) ResetEvents(ctx context.Context, since time.Time, fn func(ResetEvent) error) error {
	return s.query(ctx, fmt.Sprintf(`SELECT json_object('ts', ts, 'key', key, 'event', event, 'mode', mode, 'account', account,
  'outcome', outcome, 'detail', detail, 'reason', CASE WHEN json_valid(reason) THEN json(reason) END,
  'version', version)
FROM reset_events WHERE ts >= %d ORDER BY ts, id;
`, since.Unix()), func(line []byte) error {
		var row eventRow
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode history reset event: %w", err)
		}
		return fn(ResetEvent{
			Time: time.Unix(row.TS, 0).UTC(), Key: row.Key, Event: row.Event, Mode: row.Mode, Account: row.Account,
			Outcome: row.Outcome, Detail: row.Detail, Reason: row.Reason, Version: row.Version,
		})
	})
}

// prepare creates the private directory and database file, then brings the
// schema up to date. sqlite3 would otherwise create the file with the umask's
// permissions; its journal files copy the mode set here.
func (s *Store) prepare(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if strings.TrimSpace(s.path) == "" {
		return errors.New("history path must not be empty")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect history directory: %w", err)
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create history database: %w", err)
	}
	file.Close()
	if err := s.regular(); err != nil {
		return err
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("protect history database: %w", err)
	}
	version, err := s.version(ctx)
	if err != nil {
		return err
	}
	for ; version < schemaVersion; version++ {
		if _, err := s.run(ctx, false, migrations[version]); err != nil {
			return err
		}
	}
	s.ready = true
	return nil
}

func (s *Store) regular() error {
	info, err := os.Lstat(s.path)
	if err != nil {
		return fmt.Errorf("inspect history database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("history database must be a regular file")
	}
	return nil
}

// version refuses a database written by a newer ccodex, whose schema this
// build could misread or damage.
func (s *Store) version(ctx context.Context) (int, error) {
	output, err := s.run(ctx, true, "PRAGMA user_version;\n")
	if err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("read history schema version: %w", err)
	}
	if version > schemaVersion {
		return 0, fmt.Errorf("history database has schema version %d, but this ccodex supports up to %d; upgrade ccodex", version, schemaVersion)
	}
	return version, nil
}

func (s *Store) query(ctx context.Context, script string, row func([]byte) error) error {
	if _, err := os.Lstat(s.path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := s.regular(); err != nil {
		return err
	}
	version, err := s.version(ctx)
	if err != nil || version == 0 {
		return err
	}
	output, err := s.run(ctx, true, script)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(nil, len(output)+1)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := row(scanner.Bytes()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// run feeds script to sqlite3 on stdin. -init keeps a user's ~/.sqliterc from
// changing output or error handling, and -safe disables dot-commands that
// reach outside the database, such as .shell.
func (s *Store) run(ctx context.Context, readonly bool, script string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binary := s.binary
	if binary == "" {
		binary = systemBinary
		if _, err := os.Stat(binary); runtime.GOOS != "darwin" || err != nil {
			found, err := exec.LookPath("sqlite3")
			if err != nil {
				return nil, errors.New("sqlite3 was not found; install SQLite 3.37 or newer")
			}
			binary = found
		}
	}
	args := []string{"-init", os.DevNull, "-batch", "-bail", "-safe"}
	if readonly {
		args = append(args, "-readonly", "-list", "-noheader")
	}
	cmd := exec.CommandContext(ctx, binary, append(args, s.path)...)
	detach(cmd)
	cmd.Stdin = strings.NewReader(".timeout 5000\n" + script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if message, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); message != "" {
			return nil, fmt.Errorf("sqlite3: %s", message)
		}
		return nil, fmt.Errorf("run sqlite3: %w", err)
	}
	return stdout.Bytes(), nil
}

// literal renders v as a SQL string literal holding JSON. encoding/json escapes
// every control character, so the text is a single line without NUL bytes and
// an apostrophe is the only character that could end the literal early.
func literal(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode history record: %w", err)
	}
	for _, b := range data {
		if b < 0x20 {
			return "", errors.New("encoded history record contains a control character")
		}
	}
	return "'" + strings.ReplaceAll(string(replaceNUL(data)), "'", "''") + "'", nil
}

// replaceNUL substitutes U+FFFD for escaped NUL characters in encoded JSON.
// SQLite's text functions stop at a NUL, which would silently drop the rest of
// the string. Escapes are skipped pairwise so an escaped backslash followed by
// the text "u0000" is left alone.
func replaceNUL(data []byte) []byte {
	const nul = `\u0000`
	if !bytes.Contains(data, []byte(nul)) {
		return data
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		switch {
		case data[i] != '\\':
			out = append(out, data[i])
			i++
		case bytes.HasPrefix(data[i:], []byte(nul)):
			out = append(out, `\ufffd`...)
			i += len(nul)
		default:
			out = append(out, data[i:min(i+2, len(data))]...)
			i += 2
		}
	}
	return out
}
