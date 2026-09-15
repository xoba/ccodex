// Package codex reads account information and redeems earned rate-limit resets
// through the local Codex app-server.
package codex

import "time"

// Snapshot contains the account and quota information from one refresh.
// A nil optional value means the server did not supply that information.
type Snapshot struct {
	FetchedAt  time.Time           `json:"fetchedAt"`
	Account    *Account            `json:"account"`
	RateLimits *RateLimitsResponse `json:"rateLimits"`
	Usage      *UsageResponse      `json:"usage"`
	Warnings   []string            `json:"warnings,omitempty"`
}

type Account struct {
	Type     string  `json:"type"`
	Email    *string `json:"email"`
	PlanType string  `json:"planType"`
}

type RateLimitsResponse struct {
	RateLimits            *RateLimitSnapshot           `json:"rateLimits"`
	RateLimitsByLimitID   map[string]RateLimitSnapshot `json:"rateLimitsByLimitId"`
	OrdinaryUsageAllowed  *bool                        `json:"ordinaryUsageAllowed"`
	RateLimitResetCredits *ResetCreditsSummary         `json:"rateLimitResetCredits"`
}

type RateLimitSnapshot struct {
	LimitID              *string                    `json:"limitId"`
	LimitName            *string                    `json:"limitName"`
	Primary              *RateLimitWindow           `json:"primary"`
	Secondary            *RateLimitWindow           `json:"secondary"`
	Credits              *CreditsSnapshot           `json:"credits"`
	PlanType             *string                    `json:"planType"`
	RateLimitReachedType *string                    `json:"rateLimitReachedType"`
	IndividualLimit      *SpendControlLimitSnapshot `json:"individualLimit"`
	SpendControlReached  *bool                      `json:"spendControlReached"`
}

type RateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int64   `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

type CreditsSnapshot struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type ResetCreditsSummary struct {
	AvailableCount int64         `json:"availableCount"`
	Credits        []ResetCredit `json:"credits"`
}

// ResetCredit describes an earned reset when the backend supplies details.
type ResetCredit struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"resetType"`
	Status      string  `json:"status"`
	GrantedAt   int64   `json:"grantedAt"`
	ExpiresAt   *int64  `json:"expiresAt"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// ResetParams identifies one logical reset attempt. Reuse IdempotencyKey when
// retrying an attempt whose outcome is unknown; a UUID is recommended.
type ResetParams struct {
	IdempotencyKey string `json:"idempotencyKey"`
	CreditID       string `json:"creditId,omitempty"`
}

// ResetResult preserves a confirmed outcome even if the follow-up quota read
// fails. RateLimits is nil in that case and Warnings explains the missing data.
type ResetResult struct {
	Outcome        string              `json:"outcome"`
	IdempotencyKey string              `json:"idempotencyKey"`
	Account        *Account            `json:"account"`
	RateLimits     *RateLimitsResponse `json:"rateLimits"`
	FetchedAt      time.Time           `json:"fetchedAt"`
	Warnings       []string            `json:"warnings,omitempty"`
}

type SpendControlLimitSnapshot struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int    `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
}

type UsageResponse struct {
	Summary           UsageSummary       `json:"summary"`
	DailyUsageBuckets []DailyUsageBucket `json:"dailyUsageBuckets"`
}

type UsageSummary struct {
	LifetimeTokens        *int64 `json:"lifetimeTokens"`
	PeakDailyTokens       *int64 `json:"peakDailyTokens"`
	CurrentStreakDays     *int64 `json:"currentStreakDays"`
	LongestStreakDays     *int64 `json:"longestStreakDays"`
	LongestRunningTurnSec *int64 `json:"longestRunningTurnSec"`
}

type DailyUsageBucket struct {
	StartDate string `json:"startDate"`
	Tokens    int64  `json:"tokens"`
}
