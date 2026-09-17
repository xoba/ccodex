package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/xoba/ccodex/internal/history"
)

const historyTimeLayout = "2006-01-02 15:04:05"

// historyEntry is one line of the history: a check, with one sample per quota
// window it reported, or a reset event.
type historyEntry struct {
	time    time.Time
	samples []history.Sample
	event   *history.ResetEvent
}

func newHistoryCommand(createHistory func() (historyStore, error), jsonOutput *bool) *cobra.Command {
	var since string
	var checksOnly, resetsOnly, csvOutput, showPath bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show every saved check and reset event",
		Long:  "Show everything ccodex has saved, oldest first: one line for every successful\ncheck made by status, watch, or reset, and one line for every reset request,\noutcome, error, and skipped automatic reset.\n\n  ccodex history                  Everything\n  ccodex history --since 24h      Only the last day\n  ccodex history --resets         Only reset events\n  ccodex history --checks --csv   Every reading as CSV, ready to graph\n\n--csv and --json print the same records for other tools, with UTC times; CSV\nhas one row per quota window of each check. The table uses local times.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if csvOutput && *jsonOutput {
				return fmt.Errorf("--csv and --json cannot be combined")
			}
			from, err := parseSince(since, time.Now())
			if err != nil {
				return err
			}
			if createHistory == nil {
				return fmt.Errorf("history is not configured")
			}
			store, err := createHistory()
			if err != nil {
				return err
			}
			if showPath {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), store.Path())
				return err
			}
			// Asking for both kinds is the same as asking for neither.
			entries, err := loadHistory(cmd, store, from, !resetsOnly || checksOnly, !checksOnly || resetsOnly)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			switch {
			case csvOutput:
				err = writeHistoryCSV(&buf, entries)
			case *jsonOutput:
				err = writeHistoryJSON(&buf, entries)
			case len(entries) == 0:
				window := ""
				if !from.IsZero() {
					window = " since " + from.Local().Format(historyTimeLayout)
				}
				fmt.Fprintf(&buf, "No history recorded%s.\n", window)
			default:
				writeHistoryTable(&buf, entries)
			}
			if err != nil {
				return err
			}
			_, err = io.Copy(cmd.OutOrStdout(), &buf)
			return err
		},
	}
	cmd.Flags().StringVar(&since, "since", "all", "How far back to look: a duration such as 90m, 12h, or 7d, or all")
	cmd.Flags().BoolVar(&checksOnly, "checks", false, "Show only checks")
	cmd.Flags().BoolVar(&resetsOnly, "resets", false, "Show only reset events")
	cmd.Flags().BoolVar(&csvOutput, "csv", false, "Print CSV with a header row")
	cmd.Flags().BoolVar(&showPath, "path", false, "Print the location of the history database and exit")
	return cmd
}

// parseSince returns the zero time for "all".
func parseSince(value string, now time.Time) (time.Time, error) {
	if value == "all" {
		return time.Time{}, nil
	}
	var age time.Duration
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.ParseFloat(days, 64)
		if err != nil || math.IsNaN(count) || count < 0 || count > 36500 {
			return time.Time{}, fmt.Errorf("--since must be a duration such as 12h or 7d, or all")
		}
		age = time.Duration(count * 24 * float64(time.Hour))
	} else {
		var err error
		if age, err = time.ParseDuration(value); err != nil || age < 0 {
			return time.Time{}, fmt.Errorf("--since must be a duration such as 12h or 7d, or all")
		}
	}
	return now.Add(-age), nil
}

// loadHistory merges checks and reset events by time. A reset event sorts
// before a check saved in the same second.
func loadHistory(cmd *cobra.Command, store historyStore, since time.Time, checks, resets bool) ([]historyEntry, error) {
	var entries []historyEntry
	if resets {
		if err := store.ResetEvents(cmd.Context(), since, func(event history.ResetEvent) error {
			entries = append(entries, historyEntry{time: event.Time, event: &event})
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if checks {
		first := len(entries)
		if err := store.Samples(cmd.Context(), since, func(sample history.Sample) error {
			if last := len(entries) - 1; last >= first && sameCheck(entries[last].samples, sample) {
				entries[last].samples = append(entries[last].samples, sample)
			} else {
				entries = append(entries, historyEntry{time: sample.Time, samples: []history.Sample{sample}})
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].time.Before(entries[j].time) })
	return entries, nil
}

// sameCheck reports whether next continues the check that saved samples. A
// check reports each window once, so a repeat starts another check even when
// two were saved within the same second.
func sameCheck(samples []history.Sample, next history.Sample) bool {
	if first := samples[0]; !first.Time.Equal(next.Time) || first.Source != next.Source || first.Account != next.Account {
		return false
	}
	for _, sample := range samples {
		if sample.LimitID == next.LimitID && sample.Dimension == next.Dimension {
			return false
		}
	}
	return true
}

// resetSource names the command behind a reset event, matching check sources.
func resetSource(event *history.ResetEvent) string {
	switch event.Mode {
	case "auto":
		return "watch"
	case "manual":
		return "reset"
	}
	return event.Mode
}

func writeHistoryTable(w io.Writer, entries []historyEntry) {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "TIME\tSOURCE\tACCOUNT\tEVENT\tDETAIL")
	for _, entry := range entries {
		source, account, event, detail := "", "", "check", ""
		if entry.event != nil {
			source, account, event = resetSource(entry.event), entry.event.Account, "reset "+entry.event.Event
			detail = resetEventDetail(*entry.event)
			if key := []rune(clean(entry.event.Key)); len(key) > 12 {
				detail = strings.TrimSpace(detail + " (request …" + string(key[len(key)-12:]) + ")")
			} else if len(key) > 0 {
				detail = strings.TrimSpace(detail + " (request " + string(key) + ")")
			}
		} else {
			source, account, detail = entry.samples[0].Source, entry.samples[0].Account, checkDetail(entry.samples)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", entry.time.Local().Format(historyTimeLayout), clean(source), clean(account), clean(event), detail)
	}
	table.Flush()
}

// checkDetail summarizes a check as remaining quota per limit and window.
func checkDetail(samples []history.Sample) string {
	var parts []string
	for i := 0; i < len(samples); {
		limit := samples[i].LimitID
		var windows []string
		for ; i < len(samples) && samples[i].LimitID == limit; i++ {
			label := samples[i].Dimension
			if samples[i].WindowMins != nil && *samples[i].WindowMins > 0 {
				label = windowLength(*samples[i].WindowMins)
			} else if label == "individual" {
				label = "spend limit"
			}
			windows = append(windows, fmt.Sprintf("%s %g%%", label, samples[i].RemainingPercent))
		}
		parts = append(parts, limit+" "+strings.Join(windows, ", ")+" left")
	}
	if credits := samples[0].ResetCredits; credits != nil {
		parts = append(parts, fmt.Sprintf("earned resets: %d", *credits))
	}
	return clean(strings.Join(parts, "; "))
}

func resetEventDetail(event history.ResetEvent) string {
	switch event.Event {
	case "outcome":
		return clean(event.Outcome)
	case "skipped":
		if text, ok := map[string]string{
			"noAccountID":            "no stable account ID",
			"accountChanged":         "signed-in account changed",
			"budgetUnavailable":      "daily accounting unavailable",
			"dailyLimitReached":      "daily limit reached",
			"alreadyResetThisPeriod": "already reset during this low-quota period",
			"noResetAvailable":       "no earned reset available",
			"quotaRecovered":         "quota recovered before the request was sent",
			"quotaUnconfirmed":       "could not confirm that quota was still low",
		}[event.Detail]; ok {
			return text
		}
	}
	reason := event.Reason
	if event.Event != "requested" || reason == nil {
		return clean(event.Detail)
	}
	if reason.Trigger != "lowQuota" {
		if reason.CreditID != "" {
			return clean(reason.Trigger + ", credit " + reason.CreditID)
		}
		return clean(reason.Trigger)
	}
	parts := make([]string, 0, len(reason.Low)+3)
	for _, low := range reason.Low {
		parts = append(parts, fmt.Sprintf("%s %s %g%% left", low.LimitID, low.Dimension, low.RemainingPercent))
	}
	if reason.Threshold != nil {
		parts = append(parts, fmt.Sprintf("threshold %g%%", *reason.Threshold))
	}
	if reason.ResetCredits != nil {
		parts = append(parts, fmt.Sprintf("earned resets: %d", *reason.ResetCredits))
	}
	if reason.Retry {
		parts = append(parts, "retry")
	}
	return clean(strings.Join(parts, "; "))
}

// writeHistoryCSV uses one set of columns for both kinds of record so the
// header never depends on the flags; type says which columns a row fills.
func writeHistoryCSV(w io.Writer, entries []historyEntry) error {
	records := csv.NewWriter(w)
	records.Write([]string{
		"time", "type", "source", "account", "plan", "limit_id", "dimension", "window_mins", "used_percent",
		"remaining_percent", "resets_at", "reset_credits", "event", "outcome", "key", "detail", "reason", "version",
	})
	for _, entry := range entries {
		at := entry.time.UTC().Format(time.RFC3339)
		if event := entry.event; event != nil {
			reason := ""
			if event.Reason != nil {
				data, err := json.Marshal(event.Reason)
				if err != nil {
					return err
				}
				reason = string(data)
			}
			records.Write([]string{
				at, "reset", csvText(resetSource(event)), csvText(event.Account), "", "", "", "", "", "", "", "",
				csvText(event.Event), csvText(event.Outcome), csvText(event.Key), csvText(event.Detail), reason, csvText(event.Version),
			})
			continue
		}
		for _, sample := range entry.samples {
			mins, resetsAt, credits := "", "", ""
			if sample.WindowMins != nil {
				mins = strconv.FormatInt(*sample.WindowMins, 10)
			}
			if sample.ResetsAt != nil {
				resetsAt = sample.ResetsAt.UTC().Format(time.RFC3339)
			}
			if sample.ResetCredits != nil {
				credits = strconv.FormatInt(*sample.ResetCredits, 10)
			}
			records.Write([]string{
				at, "check", csvText(sample.Source), csvText(sample.Account), csvText(sample.Plan), csvText(sample.LimitID),
				csvText(sample.Dimension), mins, strconv.FormatFloat(sample.UsedPercent, 'g', -1, 64),
				strconv.FormatFloat(sample.RemainingPercent, 'g', -1, 64), resetsAt, credits, "", "", "", "", "", "",
			})
		}
	}
	records.Flush()
	return records.Error()
}

type historyWindowJSON struct {
	LimitID          string     `json:"limitId"`
	Dimension        string     `json:"dimension"`
	WindowMins       *int64     `json:"windowMins,omitempty"`
	UsedPercent      float64    `json:"usedPercent"`
	RemainingPercent float64    `json:"remainingPercent"`
	ResetsAt         *time.Time `json:"resetsAt,omitempty"`
}

type historyCheckJSON struct {
	Type         string              `json:"type"`
	Time         time.Time           `json:"time"`
	Source       string              `json:"source,omitempty"`
	Account      string              `json:"account,omitempty"`
	Plan         string              `json:"plan,omitempty"`
	ResetCredits *int64              `json:"resetCredits,omitempty"`
	Windows      []historyWindowJSON `json:"windows"`
}

type historyResetJSON struct {
	Type    string          `json:"type"`
	Time    time.Time       `json:"time"`
	Source  string          `json:"source,omitempty"`
	Account string          `json:"account,omitempty"`
	Event   string          `json:"event"`
	Outcome string          `json:"outcome,omitempty"`
	Key     string          `json:"key,omitempty"`
	Detail  string          `json:"detail,omitempty"`
	Reason  *history.Reason `json:"reason,omitempty"`
	Version string          `json:"version,omitempty"`
}

func writeHistoryJSON(w io.Writer, entries []historyEntry) error {
	encoder := json.NewEncoder(w)
	for _, entry := range entries {
		var record any
		if event := entry.event; event != nil {
			record = historyResetJSON{
				Type: "reset", Time: event.Time.UTC(), Source: resetSource(event), Account: event.Account, Event: event.Event,
				Outcome: event.Outcome, Key: event.Key, Detail: event.Detail, Reason: event.Reason, Version: event.Version,
			}
		} else {
			first := entry.samples[0]
			check := historyCheckJSON{
				Type: "check", Time: first.Time.UTC(), Source: first.Source, Account: first.Account, Plan: first.Plan,
				ResetCredits: first.ResetCredits, Windows: make([]historyWindowJSON, 0, len(entry.samples)),
			}
			for _, sample := range entry.samples {
				check.Windows = append(check.Windows, historyWindowJSON{
					LimitID: sample.LimitID, Dimension: sample.Dimension, WindowMins: sample.WindowMins,
					UsedPercent: sample.UsedPercent, RemainingPercent: sample.RemainingPercent, ResetsAt: sample.ResetsAt,
				})
			}
			record = check
		}
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

// csvText keeps server-supplied text on one line and stops spreadsheets from
// treating it as a formula.
func csvText(value string) string {
	value = clean(value)
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}
