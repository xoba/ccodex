package cli

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"xoba.com/codex/internal/codex"
)

func renderText(w io.Writer, snapshot *codex.Snapshot) error {
	return renderSnapshot(w, snapshot, true)
}

func renderSnapshot(w io.Writer, snapshot *codex.Snapshot, showUsage bool) error {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "Codex usage")
	if a := snapshot.Account; a != nil {
		name := "signed-in account"
		if a.Email != nil && *a.Email != "" {
			name = clean(*a.Email)
		}
		plan := a.PlanType
		if plan == "" {
			plan = "plan unavailable"
		}
		fmt.Fprintf(&buf, "Account: %s (%s)\n", name, clean(plan))
	}
	fmt.Fprintf(&buf, "Refreshed: %s\n", snapshot.FetchedAt.Local().Format(time.RFC3339))
	if limits := snapshot.RateLimits; limits != nil {
		if limits.OrdinaryUsageAllowed != nil {
			state := "allowed"
			if !*limits.OrdinaryUsageAllowed {
				state = "blocked"
			}
			fmt.Fprintf(&buf, "Included usage: %s\n", state)
		}
		buckets := quotaBuckets(limits)
		ids := make([]string, 0, len(buckets))
		for id := range buckets {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if (ids[i] == "codex") != (ids[j] == "codex") {
				return ids[i] == "codex"
			}
			return ids[i] < ids[j]
		})
		if len(ids) == 0 {
			fmt.Fprintln(&buf, "\nQuota windows: unavailable")
		}
		for _, id := range ids {
			bucket := buckets[id]
			name := clean(id)
			if bucket.LimitName != nil && *bucket.LimitName != "" && *bucket.LimitName != id {
				name = clean(*bucket.LimitName) + " [" + clean(id) + "]"
			}
			fmt.Fprintf(&buf, "\n%s\n", name)
			if bucket.Primary == nil && bucket.Secondary == nil {
				fmt.Fprintln(&buf, "Quota windows: unavailable")
			} else {
				table := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
				fmt.Fprintln(table, "WINDOW\tUSED\tREMAINING\tRESETS")
				writeWindow(table, "Primary", bucket.Primary, snapshot.FetchedAt)
				writeWindow(table, "Secondary", bucket.Secondary, snapshot.FetchedAt)
				table.Flush()
			}
			if bucket.RateLimitReachedType != nil && *bucket.RateLimitReachedType != "" {
				fmt.Fprintf(&buf, "Limit state: %s\n", clean(strings.ReplaceAll(*bucket.RateLimitReachedType, "_", " ")))
			}
			if bucket.SpendControlReached != nil {
				state := "not reached"
				if *bucket.SpendControlReached {
					state = "reached"
				}
				fmt.Fprintf(&buf, "Spend control: %s\n", state)
			}
			if limit := bucket.IndividualLimit; limit != nil {
				fmt.Fprintf(&buf, "Individual spend limit: %s of %s used; %d%% remaining; resets %s\n", clean(limit.Used), clean(limit.Limit), limit.RemainingPercent, resetText(&limit.ResetsAt, snapshot.FetchedAt))
			}
			if credits := bucket.Credits; credits != nil {
				switch {
				case credits.Unlimited:
					fmt.Fprintln(&buf, "Credits: unlimited")
				case credits.Balance != nil:
					fmt.Fprintf(&buf, "Credits: %s\n", clean(*credits.Balance))
				case credits.HasCredits:
					fmt.Fprintln(&buf, "Credits: available (balance unavailable)")
				default:
					fmt.Fprintln(&buf, "Credits: none")
				}
			}
		}
		if credits := limits.RateLimitResetCredits; credits != nil {
			fmt.Fprintf(&buf, "\nAvailable rate-limit resets: %d\n", credits.AvailableCount)
		}
	} else {
		fmt.Fprintln(&buf, "\nQuota windows: unavailable")
	}

	if showUsage {
		writeUsage(&buf, snapshot.Usage)
	}
	for _, warning := range snapshot.Warnings {
		fmt.Fprintf(&buf, "\nNote: %s\n", clean(warning))
	}
	fmt.Fprintln(&buf)
	_, err := io.Copy(w, &buf)
	return err
}

func writeUsage(buf *bytes.Buffer, usage *codex.UsageResponse) {
	fmt.Fprintln(buf, "\nToken activity")
	if usage != nil {
		fmt.Fprintf(buf, "Lifetime tokens: %s\n", optionalCount(usage.Summary.LifetimeTokens))
		fmt.Fprintf(buf, "Peak daily tokens: %s\n", optionalCount(usage.Summary.PeakDailyTokens))
		if v := usage.Summary.CurrentStreakDays; v != nil {
			fmt.Fprintf(buf, "Current streak: %s days\n", formatCount(*v))
		}
		if v := usage.Summary.LongestStreakDays; v != nil {
			fmt.Fprintf(buf, "Longest streak: %s days\n", formatCount(*v))
		}
		if v := usage.Summary.LongestRunningTurnSec; v != nil {
			fmt.Fprintf(buf, "Longest turn: %s seconds\n", formatCount(*v))
		}
		if len(usage.DailyUsageBuckets) > 0 {
			latest := usage.DailyUsageBuckets[0]
			for _, bucket := range usage.DailyUsageBuckets[1:] {
				if bucket.StartDate > latest.StartDate {
					latest = bucket
				}
			}
			fmt.Fprintf(buf, "Latest reported day (%s): %s tokens\n", clean(latest.StartDate), formatCount(latest.Tokens))
		}
	} else {
		fmt.Fprintln(buf, "Unavailable")
	}
}

func writeWindow(w io.Writer, name string, window *codex.RateLimitWindow, now time.Time) {
	if window == nil {
		return
	}
	if window.WindowDurationMins != nil && *window.WindowDurationMins > 0 {
		name += " (" + windowLength(*window.WindowDurationMins) + ")"
	}
	used, remaining := "unavailable", "unavailable"
	if window.UsedPercent != nil && !math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) {
		used = fmt.Sprintf("%g%%", *window.UsedPercent)
		remaining = fmt.Sprintf("%g%%", math.Max(0, math.Min(100, 100-*window.UsedPercent)))
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, used, remaining, resetText(window.ResetsAt, now))
}

func windowLength(minutes int64) string {
	if minutes%1440 == 0 {
		return fmt.Sprintf("%dd", minutes/1440)
	}
	if minutes%60 == 0 {
		return fmt.Sprintf("%dh", minutes/60)
	}
	if minutes > 60 {
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	return fmt.Sprintf("%dm", minutes)
}

func resetText(timestamp *int64, now time.Time) string {
	if timestamp == nil {
		return "unavailable"
	}
	reset := time.Unix(*timestamp, 0)
	formatted := reset.Local().Format("2006-01-02 15:04 MST")
	if !reset.After(now) {
		return formatted + " (due; awaiting refresh)"
	}
	seconds := int64(math.Ceil(reset.Sub(now).Seconds()))
	days := seconds / 86400
	seconds %= 86400
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60
	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return formatted + " (in " + strings.Join(parts, " ") + ")"
}

func optionalCount(v *int64) string {
	if v == nil {
		return "unavailable"
	}
	return formatCount(*v)
}

func formatCount(v int64) string {
	s := strconv.FormatInt(v, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

// Account and bucket labels come from the server. Keep control characters out
// of terminal output while preserving the original strings in JSON output.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
