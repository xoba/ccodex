package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"xoba.com/codex/internal/codex"
)

type resetFunc func(context.Context, codex.Options, codex.ResetParams) (*codex.ResetResult, error)

func newResetCommand(fetch fetchFunc, reset resetFunc, opts *codex.Options, jsonOutput *bool, validate func() error) *cobra.Command {
	var dryRun bool
	var params codex.ResetParams
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Spend one earned reset on eligible Codex quota windows",
		Long:  "Spend one earned rate-limit reset using your signed-in Codex account.\nCodex decides which windows are eligible. This command takes effect immediately.\nUse --dry-run to inspect available resets without consuming one.\n\nA request ID is written to stderr before redemption. If a request is interrupted\nor fails, reuse it with --idempotency-key to avoid consuming a second reset.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validate(); err != nil {
				return err
			}
			for _, flag := range []struct {
				name, value string
			}{{"credit-id", params.CreditID}, {"idempotency-key", params.IdempotencyKey}} {
				if cmd.Flags().Changed(flag.name) && (strings.TrimSpace(flag.value) == "" || strings.IndexFunc(flag.value, unicode.IsControl) >= 0) {
					return fmt.Errorf("--%s must be non-empty and contain no control characters", flag.name)
				}
			}
			if dryRun {
				snapshot, err := fetch(cmd.Context(), *opts)
				if err != nil {
					return err
				}
				if *jsonOutput {
					return writeJSON(cmd.OutOrStdout(), struct {
						DryRun   bool            `json:"dryRun"`
						CreditID string          `json:"creditId,omitempty"`
						Snapshot *codex.Snapshot `json:"snapshot"`
					}{true, params.CreditID, snapshot})
				}
				return renderResetPreview(cmd.OutOrStdout(), snapshot, params.CreditID)
			}
			if reset == nil {
				return fmt.Errorf("reset operation is not configured")
			}
			if params.IdempotencyKey == "" {
				params.IdempotencyKey = newRequestID()
			}
			// Record the key before sending anything. If this write fails, do not
			// redeem: the user must be able to identify an interrupted attempt.
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Reset request ID: %s\nIf interrupted, retry with this same --idempotency-key.\n", params.IdempotencyKey); err != nil {
				return err
			}
			result, err := reset(cmd.Context(), *opts, params)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					// Main treats read-only cancellation as a clean exit. A reset
					// interruption needs its retry guidance and a nonzero exit.
					return fmt.Errorf("reset interrupted; outcome may be unknown; retry this attempt with --idempotency-key %q", params.IdempotencyKey)
				}
				return fmt.Errorf("%w; retry this attempt with --idempotency-key %q", err, params.IdempotencyKey)
			}
			if *jsonOutput {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			return renderResetResult(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Read available resets without consuming one")
	cmd.Flags().StringVar(&params.CreditID, "credit-id", "", "Specific reset credit to redeem (default: Codex selects one)")
	cmd.Flags().StringVar(&params.IdempotencyKey, "idempotency-key", "", "Request ID; reuse the same value when retrying an attempt (default: new UUID)")
	return cmd
}

func newRequestID() string {
	var id [16]byte
	rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func renderResetPreview(w io.Writer, snapshot *codex.Snapshot, creditID string) error {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "Dry run: no reset requested.")
	fmt.Fprintf(&buf, "Refreshed: %s\n", snapshot.FetchedAt.Local().Format(time.RFC3339))
	if creditID != "" {
		fmt.Fprintf(&buf, "Requested credit: %s\n", clean(creditID))
	}
	if snapshot.RateLimits == nil || snapshot.RateLimits.RateLimitResetCredits == nil {
		fmt.Fprintln(&buf, "Available rate-limit resets: unavailable")
	} else {
		credits := snapshot.RateLimits.RateLimitResetCredits
		fmt.Fprintf(&buf, "Available rate-limit resets: %d\n", credits.AvailableCount)
		if credits.Credits == nil {
			fmt.Fprintln(&buf, "Reset details: unavailable")
		}
		for _, credit := range credits.Credits {
			fmt.Fprintf(&buf, "\n%s\n", clean(credit.ID))
			if credit.Title != nil {
				fmt.Fprintf(&buf, "  %s\n", clean(*credit.Title))
			}
			fmt.Fprintf(&buf, "  Status: %s\n", clean(credit.Status))
			if credit.ExpiresAt != nil {
				fmt.Fprintf(&buf, "  Expires: %s\n", time.Unix(*credit.ExpiresAt, 0).Local().Format(time.RFC3339))
			}
		}
		if credits.Credits != nil && int64(len(credits.Credits)) < credits.AvailableCount {
			fmt.Fprintln(&buf, "\nDetails are not available for every reset; the count above is authoritative.")
		}
	}
	fmt.Fprintln(&buf, "\nCodex determines eligibility when a reset is requested.")
	_, err := io.Copy(w, &buf)
	return err
}

func renderResetResult(w io.Writer, result *codex.ResetResult) error {
	messages := map[string]string{
		"reset":           "Reset applied. One earned reset was consumed.",
		"alreadyRedeemed": "This request already applied a reset. No additional reset was consumed.",
		"nothingToReset":  "No reset used: no quota window is currently eligible.",
		"noCredit":        "No reset used: no eligible earned reset credit is available for this request.",
	}
	message, ok := messages[result.Outcome]
	if !ok {
		return fmt.Errorf("unrecognized reset outcome; check status and retry with the same --idempotency-key")
	}
	if _, err := fmt.Fprintf(w, "%s\nRequest ID: %s\n\n", message, clean(result.IdempotencyKey)); err != nil {
		return err
	}
	if result.RateLimits == nil {
		for _, warning := range result.Warnings {
			if _, err := fmt.Fprintf(w, "Note: %s\n", clean(warning)); err != nil {
				return err
			}
		}
		return nil
	}
	return renderSnapshot(w, &codex.Snapshot{
		FetchedAt: result.FetchedAt, Account: result.Account,
		RateLimits: result.RateLimits, Warnings: result.Warnings,
	}, false)
}
