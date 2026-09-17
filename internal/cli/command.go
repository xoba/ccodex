// Package cli implements ccodex's commands and terminal output.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/spf13/cobra"
	"github.com/xoba/ccodex/internal/buildinfo"
	"github.com/xoba/ccodex/internal/codex"
)

type fetchFunc func(context.Context, codex.Options) (*codex.Snapshot, error)
type alarmFunc func(context.Context, io.Writer) error

// NewCommand creates a command tree. Configuration is scoped to this invocation.
func NewCommand() *cobra.Command {
	return newCommandWithHistory(codex.Fetch, codex.Reset, playAlarm, defaultAutoResetBudget, defaultHistory)
}

func newCommand(fetch fetchFunc) *cobra.Command {
	return newCommandWithReset(fetch, nil)
}

func newCommandWithReset(fetch fetchFunc, reset resetFunc) *cobra.Command {
	return newCommandWithAlarm(fetch, reset, nil)
}

func newCommandWithAlarm(fetch fetchFunc, reset resetFunc, alarm alarmFunc) *cobra.Command {
	return newCommandWithBudget(fetch, reset, alarm, defaultAutoResetBudget)
}

func newCommandWithBudget(fetch fetchFunc, reset resetFunc, alarm alarmFunc, createBudget func() (autoResetBudget, error)) *cobra.Command {
	return newCommandWithHistory(fetch, reset, alarm, createBudget, nil)
}

// newCommandWithHistory records history only when given createHistory, so
// commands built without one never touch the user's database.
func newCommandWithHistory(fetch fetchFunc, reset resetFunc, alarm alarmFunc, createBudget func() (autoResetBudget, error), createHistory func() (historyStore, error)) *cobra.Command {
	var jsonOutput bool
	var noHistory bool
	var historyDays int
	var noAlarm bool
	var autoReset bool
	var maxResetsPerDay int
	var alarmThreshold float64
	var opts codex.Options
	var interval time.Duration

	validate := func() error {
		if opts.Binary == "" {
			return fmt.Errorf("--codex-bin must not be empty")
		}
		if opts.Timeout <= 0 {
			return fmt.Errorf("--timeout must be greater than zero")
		}
		return nil
	}
	newRecorder := func(cmd *cobra.Command, retention time.Duration) *historyRecorder {
		if noHistory || createHistory == nil {
			return nil
		}
		store, err := createHistory()
		if err != nil {
			store = unavailableHistory{err}
		}
		return &historyRecorder{store: store, output: cmd.ErrOrStderr(), timeout: opts.Timeout, retention: retention}
	}
	writeSnapshot := func(cmd *cobra.Command, snapshot *codex.Snapshot, watching bool) error {
		if jsonOutput {
			enc := json.NewEncoder(cmd.OutOrStdout())
			if !watching {
				enc.SetIndent("", "  ")
			}
			return enc.Encode(snapshot)
		}
		return renderText(cmd.OutOrStdout(), snapshot)
	}
	status := func(cmd *cobra.Command, _ []string) error {
		if err := validate(); err != nil {
			return err
		}
		snapshot, err := fetch(cmd.Context(), opts)
		if err != nil {
			return err
		}
		if err := writeSnapshot(cmd, snapshot, false); err != nil {
			return err
		}
		newRecorder(cmd, 0).samples(cmd.Context(), snapshot, false)
		return nil
	}
	root := &cobra.Command{
		Use:           "ccodex",
		Short:         "Check Codex usage, limits, and reset times",
		Long:          "ccodex checks the subscription limits available through your signed-in Codex CLI.\nRunning ccodex without a subcommand shows the current status.",
		Version:       buildinfo.Version,
		Args:          cobra.NoArgs,
		RunE:          status,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Print JSON (one object per line in watch and history)")
	root.PersistentFlags().BoolVar(&noHistory, "no-history", false, "Do not record usage or reset events in the local history database")
	root.PersistentFlags().StringVar(&opts.Binary, "codex-bin", "codex", "Path to the Codex executable")
	root.PersistentFlags().DurationVar(&opts.Timeout, "timeout", 15*time.Second, "Maximum duration of each refresh or reset operation")
	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show current account, limits, and available token activity",
		Args:  cobra.NoArgs,
		RunE:  status,
	})
	root.AddCommand(&cobra.Command{
		Use:   "play",
		Short: "Play the alarm once and exit for testing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if alarm == nil {
				return fmt.Errorf("alarm playback is not configured")
			}
			return alarm(cmd.Context(), cmd.ErrOrStderr())
		},
	})
	watch := &cobra.Command{
		Use:   "watch",
		Short: "Refresh continuously until interrupted",
		Long:  "Fetch immediately, then wait the interval after each refresh.\nMonitoring is read-only by default. Below --alarm-threshold percent remaining,\nsound once per refresh. Use --auto-reset to automatically spend an available\nearned reset, subject to --max-resets-per-day (default 1).\nSuccessful snapshots go to stdout; failures and reset outcomes go to stderr.\nQuota changes and reset events are saved for ccodex history.\nPress Ctrl-C to stop.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validate(); err != nil {
				return err
			}
			if interval < time.Second {
				return fmt.Errorf("--interval must be at least 1s")
			}
			if math.IsNaN(alarmThreshold) || math.IsInf(alarmThreshold, 0) || alarmThreshold < 0 || alarmThreshold > 100 {
				return fmt.Errorf("--alarm-threshold must be a finite percentage between 0 and 100")
			}
			if maxResetsPerDay < 0 {
				return fmt.Errorf("--max-resets-per-day must be zero or greater")
			}
			if historyDays < 0 {
				return fmt.Errorf("--history-days must be zero or greater")
			}
			recorder := newRecorder(cmd, time.Duration(historyDays)*24*time.Hour)
			var resetState autoResetState
			var budget autoResetBudget
			if autoReset && reset != nil && maxResetsPerDay > 0 {
				var err error
				budget, err = createBudget()
				if err != nil {
					if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "ccodex: automatic resets unavailable: %v\n", err); writeErr != nil {
						return writeErr
					}
				}
			}
			for {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				snapshot, err := fetch(cmd.Context(), opts)
				if cmd.Context().Err() != nil {
					return cmd.Context().Err()
				}
				if err != nil {
					if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "ccodex: %s refresh failed: %v (retrying in %s)\n", time.Now().Format(time.RFC3339), err, interval); writeErr != nil {
						return writeErr
					}
				} else {
					if err := writeSnapshot(cmd, snapshot, true); err != nil {
						return err
					}
					if err := cmd.Context().Err(); err != nil {
						return err
					}
					recorder.samples(cmd.Context(), snapshot, false)
					if !noAlarm && alarm != nil && hasLowQuota(snapshot, alarmThreshold) {
						// Decide once over the whole snapshot, so multiple low
						// windows still produce only one sound this iteration.
						alarmErr := alarm(cmd.Context(), cmd.ErrOrStderr())
						if err := cmd.Context().Err(); err != nil {
							return err
						}
						if alarmErr != nil {
							if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "ccodex: could not play low-quota alarm: %v\n", alarmErr); err != nil {
								return err
							}
						}
					}
					if autoReset && reset != nil && budget != nil && maxResetsPerDay > 0 {
						updated, err := applyAutoReset(cmd.Context(), &resetState, snapshot, alarmThreshold, opts, reset, budget, maxResetsPerDay, cmd.ErrOrStderr(), recorder)
						if err != nil {
							return err
						}
						if updated != nil {
							// Always keep the reading that shows what the reset did.
							recorder.samples(cmd.Context(), updated, true)
							if err := writeSnapshot(cmd, updated, true); err != nil {
								return err
							}
						}
					}
				}
				timer := time.NewTimer(interval)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return cmd.Context().Err()
				case <-timer.C:
				}
			}
		},
	}
	watch.Flags().DurationVar(&interval, "interval", time.Minute, "Delay between refreshes (minimum 1s)")
	watch.Flags().Float64Var(&alarmThreshold, "alarm-threshold", 5, "Alarm below this remaining quota percentage; also applies to --auto-reset (0–100; 0 disables both)")
	watch.Flags().BoolVar(&noAlarm, "no-alarm", false, "Disable low-quota alarm sounds")
	watch.Flags().BoolVar(&autoReset, "auto-reset", false, "Opt in to automatically spending an available earned reset below the alarm threshold")
	watch.Flags().IntVar(&maxResetsPerDay, "max-resets-per-day", 1, "Maximum automatic resets per local calendar day, shared across watch restarts (0 disables)")
	watch.Flags().IntVar(&historyDays, "history-days", 90, "Delete usage history older than this many days (0 keeps it forever; reset events are always kept)")
	root.AddCommand(watch)
	root.AddCommand(newResetCommand(fetch, reset, &opts, &jsonOutput, validate, newRecorder))
	root.AddCommand(newHistoryCommand(createHistory, &jsonOutput))
	return root
}
