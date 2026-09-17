package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/xoba/ccodex/internal/history"
)

const historyTimeLayout = "2006-01-02 15:04:05"

func newHistoryCommand(createHistory func() (historyStore, error), jsonOutput *bool) *cobra.Command {
	open := func() (historyStore, error) {
		if createHistory == nil {
			return nil, fmt.Errorf("history is not configured")
		}
		return createHistory()
	}
	// list builds the RunE shared by a listing command and its flags.
	list := func(cmd *cobra.Command, defaultSince string, write func(*cobra.Command, historyStore, time.Time, string) error) {
		var since string
		var csvOutput bool
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			if csvOutput && *jsonOutput {
				return fmt.Errorf("--csv and --json cannot be combined")
			}
			from, err := parseSince(since, time.Now())
			if err != nil {
				return err
			}
			store, err := open()
			if err != nil {
				return err
			}
			format := "text"
			if csvOutput {
				format = "csv"
			} else if *jsonOutput {
				format = "json"
			}
			return write(cmd, store, from, format)
		}
		cmd.Flags().StringVar(&since, "since", defaultSince, "How far back to look: a duration such as 12h or 7d, or all")
		cmd.Flags().BoolVar(&csvOutput, "csv", false, "Print CSV with a header row")
	}

	root := &cobra.Command{
		Use:   "history",
		Short: "Show saved reset events and quota usage",
		Long:  "Show what ccodex has saved in its local history database.\nWithout a subcommand, list reset events. With --json, print one JSON object\nper line. Times are local in tables and UTC in CSV and JSON.",
	}
	list(root, "30d", writeResetHistory)
	resets := &cobra.Command{
		Use:   "resets",
		Short: "List reset requests, outcomes, and reasons automatic resets were skipped",
	}
	list(resets, "30d", writeResetHistory)
	usage := &cobra.Command{
		Use:   "usage",
		Short: "List saved quota readings, suitable for graphing",
		Long:  "List saved quota readings, oldest first. A reading is saved when a value\nchanges and at least every 15 minutes while ccodex is polling, so plot the\nvalues as steps.",
	}
	list(usage, "7d", writeUsageHistory)
	root.AddCommand(resets, usage, &cobra.Command{
		Use:   "path",
		Short: "Print the location of the history database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := open()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), store.Path())
			return err
		},
	})
	return root
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

func sinceText(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return " since " + since.Local().Format(historyTimeLayout)
}

func writeResetHistory(cmd *cobra.Command, store historyStore, since time.Time, format string) error {
	var buf bytes.Buffer
	table := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	records := csv.NewWriter(&buf)
	encoder := json.NewEncoder(&buf)
	switch format {
	case "csv":
		records.Write([]string{"time", "mode", "event", "outcome", "key", "account", "detail", "reason", "version"})
	case "text":
		fmt.Fprintln(table, "TIME\tMODE\tEVENT\tOUTCOME\tREQUEST\tDETAIL")
	}
	count := 0
	err := store.ResetEvents(cmd.Context(), since, func(event history.ResetEvent) error {
		count++
		switch format {
		case "json":
			return encoder.Encode(event)
		case "csv":
			reason := ""
			if event.Reason != nil {
				data, err := json.Marshal(event.Reason)
				if err != nil {
					return err
				}
				reason = string(data)
			}
			return records.Write([]string{
				event.Time.UTC().Format(time.RFC3339), csvText(event.Mode), csvText(event.Event), csvText(event.Outcome),
				csvText(event.Key), csvText(event.Account), csvText(event.Detail), reason, csvText(event.Version),
			})
		}
		key := clean(event.Key)
		if runes := []rune(key); len(runes) > 12 {
			key = "…" + string(runes[len(runes)-12:])
		}
		_, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", event.Time.Local().Format(historyTimeLayout),
			clean(event.Mode), clean(event.Event), clean(event.Outcome), key, resetEventDetail(event))
		return err
	})
	if err != nil {
		return err
	}
	records.Flush()
	if err := records.Error(); err != nil {
		return err
	}
	table.Flush()
	if format == "text" && count == 0 {
		buf.Reset()
		fmt.Fprintf(&buf, "No reset events recorded%s.\n", sinceText(since))
	}
	_, err = io.Copy(cmd.OutOrStdout(), &buf)
	return err
}

func resetEventDetail(event history.ResetEvent) string {
	if event.Event == "skipped" {
		if text, ok := map[string]string{
			"noAccountID":            "no stable account ID",
			"accountChanged":         "signed-in account changed",
			"budgetUnavailable":      "daily accounting unavailable",
			"dailyLimitReached":      "daily limit reached",
			"alreadyResetThisPeriod": "already reset during this low-quota period",
			"noResetAvailable":       "no earned reset available",
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
		parts = append(parts, fmt.Sprintf("%d resets available", *reason.ResetCredits))
	}
	if reason.Retry {
		parts = append(parts, "retry")
	}
	return clean(strings.Join(parts, "; "))
}

func writeUsageHistory(cmd *cobra.Command, store historyStore, since time.Time, format string) error {
	var buf bytes.Buffer
	table := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	records := csv.NewWriter(&buf)
	encoder := json.NewEncoder(&buf)
	switch format {
	case "csv":
		records.Write([]string{"time", "account", "plan", "limit_id", "dimension", "window_mins", "used_percent", "remaining_percent", "resets_at", "reset_credits"})
	case "text":
		fmt.Fprintln(table, "TIME\tACCOUNT\tLIMIT\tWINDOW\tUSED\tREMAINING\tRESETS AT\tRESETS AVAILABLE")
	}
	count := 0
	err := store.Samples(cmd.Context(), since, func(sample history.Sample) error {
		count++
		if format == "json" {
			return encoder.Encode(sample)
		}
		mins, resetsAt, credits := "", "", ""
		if sample.WindowMins != nil {
			mins = strconv.FormatInt(*sample.WindowMins, 10)
		}
		if sample.ResetCredits != nil {
			credits = strconv.FormatInt(*sample.ResetCredits, 10)
		}
		if format == "csv" {
			if sample.ResetsAt != nil {
				resetsAt = sample.ResetsAt.UTC().Format(time.RFC3339)
			}
			return records.Write([]string{
				sample.Time.UTC().Format(time.RFC3339), csvText(sample.Account), csvText(sample.Plan), csvText(sample.LimitID),
				csvText(sample.Dimension), mins, strconv.FormatFloat(sample.UsedPercent, 'g', -1, 64),
				strconv.FormatFloat(sample.RemainingPercent, 'g', -1, 64), resetsAt, credits,
			})
		}
		window := clean(sample.Dimension)
		if sample.WindowMins != nil && *sample.WindowMins > 0 {
			window += " (" + windowLength(*sample.WindowMins) + ")"
		}
		if sample.ResetsAt != nil {
			resetsAt = sample.ResetsAt.Local().Format("2006-01-02 15:04")
		}
		_, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%g%%\t%g%%\t%s\t%s\n", sample.Time.Local().Format(historyTimeLayout),
			clean(sample.Account), clean(sample.LimitID), window, sample.UsedPercent, sample.RemainingPercent, resetsAt, credits)
		return err
	})
	if err != nil {
		return err
	}
	records.Flush()
	if err := records.Error(); err != nil {
		return err
	}
	table.Flush()
	if format == "text" && count == 0 {
		buf.Reset()
		fmt.Fprintf(&buf, "No usage recorded%s.\n", sinceText(since))
	}
	_, err = io.Copy(cmd.OutOrStdout(), &buf)
	return err
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
