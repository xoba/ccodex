package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"github.com/xoba/ccodex/internal/codex"
	"github.com/xoba/ccodex/internal/resetbudget"
)

type autoResetBudget interface {
	LockOperation(context.Context) (func() error, error)
	Pending(context.Context, string) (string, error)
	Reserve(context.Context, string, int, time.Time) (bool, error)
	Complete(context.Context, string, string) error
}

func defaultAutoResetBudget() (autoResetBudget, error) {
	path, err := resetbudget.DefaultPath()
	if err != nil {
		return nil, err
	}
	return resetbudget.Store{Path: path}, nil
}

// applyAutoReset considers one ordinary watch snapshot. A returned snapshot is
// a fresh quota read after redemption; it must not rearm this state machine.
func applyAutoReset(ctx context.Context, state *autoResetState, snapshot *codex.Snapshot, threshold float64, opts codex.Options, reset resetFunc, budget autoResetBudget, limit int, output io.Writer) (*codex.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit == 0 {
		return nil, nil
	}
	if snapshot == nil || snapshot.RateLimits == nil || snapshot.RateLimits.AccountID == nil || *snapshot.RateLimits.AccountID == "" {
		if hasLowQuota(snapshot, threshold) {
			_, err := fmt.Fprintln(output, "ccodex: automatic reset skipped: a stable account ID is unavailable")
			return nil, err
		}
		return nil, nil
	}
	accountID := *snapshot.RateLimits.AccountID
	if state.params.ExpectedAccountID != "" && state.params.ExpectedAccountID != accountID {
		_, err := fmt.Fprintln(output, "ccodex: automatic resets paused because the signed-in account changed; resolve any pending request on its original account before restarting automatic mode")
		return nil, err
	}
	if !hasLowQuota(snapshot, threshold) {
		state.next(snapshot, threshold)
		return nil, nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// Serialize the full automatic operation across watch processes. In
	// particular, a no-op must not release a slot while another client is
	// still redeeming the same saved request.
	lockCtx, lockCancel := context.WithTimeout(ctx, timeout)
	release, lockErr := budget.LockOperation(lockCtx)
	lockCancel()
	if lockErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_, err := fmt.Fprintf(output, "ccodex: automatic reset skipped: could not lock daily accounting: %v\n", lockErr)
		return nil, err
	}
	defer release()
	before := *state
	resumedPending := false
	prefix := fmt.Sprintf("auto-%x-", sha256.Sum256([]byte(accountID)))
	if state.params.IdempotencyKey == "" && hasLowQuota(snapshot, threshold) {
		pendingCtx, pendingCancel := context.WithTimeout(ctx, timeout)
		key, err := budget.Pending(pendingCtx, prefix)
		pendingCancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_, writeErr := fmt.Fprintf(output, "ccodex: automatic reset skipped: could not read daily limit: %v\n", err)
			return nil, writeErr
		}
		if key != "" {
			resumedPending = true
			state.resume(snapshot, threshold, codex.ResetParams{IdempotencyKey: key, ExpectedAccount: snapshot.Account, ExpectedAccountID: accountID})
		}
	}
	params, attempt := state.next(snapshot, threshold)
	if !attempt {
		return nil, nil
	}
	if params.ExpectedAccountID == "" {
		params.IdempotencyKey = prefix + params.IdempotencyKey
		params.ExpectedAccount = snapshot.Account
		params.ExpectedAccountID = accountID
		state.params = params
	}
	budgetCtx, cancel := context.WithTimeout(ctx, timeout)
	allowed, budgetErr := budget.Reserve(budgetCtx, params.IdempotencyKey, limit, time.Now())
	cancel()
	if budgetErr != nil || !allowed {
		// No request was sent. A budget denial must not turn a new attempt
		// into an unresolved redemption or bypass the next credit check.
		*state = before
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if budgetErr != nil {
			_, err := fmt.Fprintf(output, "ccodex: automatic reset skipped: could not reserve daily limit: %v\n", budgetErr)
			return nil, err
		}
		_, err := fmt.Fprintf(output, "ccodex: automatic reset skipped: daily limit of %d reached\n", limit)
		return nil, err
	}
	// Save the request ID in the command's output before dispatching. Retries
	// use this ID even if the first call spent the last available reset.
	if _, err := fmt.Fprintf(output, "ccodex: automatic reset request ID: %s\n", params.IdempotencyKey); err != nil {
		// Output failed before dispatch, so the unused reservation can be
		// released unless it covers an earlier unresolved request.
		if !resumedPending && (before.params.IdempotencyKey != params.IdempotencyKey || before.outcome != autoResetUnresolved) {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			_ = budget.Complete(cleanupCtx, params.IdempotencyKey, "nothingToReset")
			cleanupCancel()
		}
		return nil, err
	}
	result, resetErr := reset(ctx, opts, params)
	message := ""
	if resetErr == nil {
		valid := false
		if result != nil {
			message, valid = resetOutcomeMessage(result.Outcome)
		}
		if !valid {
			resetErr = fmt.Errorf("Codex returned an unrecognized automatic reset outcome")
		}
	}
	state.record(result, resetErr)
	if resetErr != nil {
		if ctx.Err() != nil {
			// Do not wrap context.Canceled: main suppresses it for a clean
			// watch exit, but an uncertain redemption must remain visible.
			return nil, fmt.Errorf("automatic reset interrupted; outcome may be unknown; resolve this attempt with ccodex reset --idempotency-key %q before restarting automatic mode", params.IdempotencyKey)
		}
		_, err := fmt.Fprintf(output, "ccodex: automatic reset failed: %v; will retry request %s while quota remains low\n", resetErr, params.IdempotencyKey)
		return nil, err
	}
	// A known outcome must be recorded even if cancellation happened during
	// Codex's follow-up quota read. The already-persisted reservation protects
	// the cap when this update fails.
	completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	completeErr := budget.Complete(completeCtx, params.IdempotencyKey, result.Outcome)
	completeCancel()
	if _, err := fmt.Fprintf(output, "ccodex: automatic reset: %s (request %s)\n", message, params.IdempotencyKey); err != nil {
		return nil, err
	}
	if completeErr != nil {
		if _, err := fmt.Fprintf(output, "ccodex: reset outcome is confirmed, but the daily reset record could not be updated: %v\n", completeErr); err != nil {
			return nil, err
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(output, "ccodex: %s\n", clean(warning)); err != nil {
			return nil, err
		}
	}
	if result.RateLimits == nil {
		return nil, nil
	}
	updated := *snapshot
	updated.FetchedAt = result.FetchedAt
	updated.Account = result.Account
	updated.RateLimits = result.RateLimits
	updated.Warnings = append(append([]string(nil), snapshot.Warnings...), result.Warnings...)
	return &updated, nil
}
