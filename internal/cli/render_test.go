package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"xoba.com/ccodex/internal/codex"
)

func exampleSnapshot(t *testing.T) *codex.Snapshot {
	t.Helper()
	var snapshot codex.Snapshot
	const fixture = `{
  "fetchedAt": "2026-09-15T12:00:00Z",
  "account": {"type": "chatgpt", "email": "example@example.com", "planType": "plus"},
  "rateLimits": {
    "ordinaryUsageAllowed": false,
    "rateLimits": {"limitId": "codex", "primary": {"usedPercent": 99}},
    "rateLimitsByLimitId": {
      "a_other": {"limitName": "Other", "primary": {"usedPercent": null, "resetsAt": null}},
      "codex": {
        "primary": {"usedPercent": 25, "windowDurationMins": 300, "resetsAt": 1789477200},
        "secondary": {"usedPercent": 100, "windowDurationMins": 10080, "resetsAt": 0},
        "credits": {"hasCredits": true, "unlimited": false, "balance": "12.5"},
        "rateLimitReachedType": "workspace_member_usage_limit_reached"
      }
    }
  },
  "usage": {
    "summary": {"lifetimeTokens": 1234567, "peakDailyTokens": null},
    "dailyUsageBuckets": [
      {"startDate": "2026-09-14", "tokens": 1234},
      {"startDate": "2026-09-12", "tokens": 9999}
    ]
  }
}`
	if err := json.Unmarshal([]byte(fixture), &snapshot); err != nil {
		t.Fatal(err)
	}
	return &snapshot
}

func TestTextShowsMultipleBucketsWithoutInferringRecovery(t *testing.T) {
	var out bytes.Buffer
	if err := renderText(&out, exampleSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"example@example.com (plus)", "Primary (5h)", "Secondary (7d)", "25%", "75%",
		"Other [a_other]", "Included usage: blocked", "due; awaiting refresh",
		"workspace member usage limit reached", "Credits: 12.5", "Lifetime tokens: 1,234,567",
		"Peak daily tokens: unavailable", "Latest reported day (2026-09-14): 1,234 tokens",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "99%") {
		t.Fatal("rendered legacy bucket in addition to multi-bucket response")
	}
	if strings.Index(text, "\ncodex\n") > strings.Index(text, "Other [a_other]") {
		t.Fatal("main codex bucket should appear first")
	}
	other := text[strings.Index(text, "Other [a_other]"):strings.Index(text, "Token activity")]
	if strings.Contains(other, "0%") || !strings.Contains(other, "unavailable") {
		t.Fatalf("missing values were represented as real percentages: %s", other)
	}
}

func TestLegacyAndMissingOptionalData(t *testing.T) {
	var snapshot codex.Snapshot
	if err := json.Unmarshal([]byte(`{"rateLimits":{"rateLimits":{"primary":{"usedPercent":0}}},"warnings":["Token statistics are unavailable."]}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := renderText(&out, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex", "0%", "100%", "unavailable", "Token statistics are unavailable."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}

func TestResetCountdown(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 500000000, time.UTC)
	for _, test := range []struct {
		seconds int64
		want    string
	}{
		{1, "in 1s"}, {61, "in 1m 1s"}, {90061, "in 1d 1h 1m 1s"}, {0, "due; awaiting refresh"},
	} {
		timestamp := now.Unix() + test.seconds
		if got := resetText(&timestamp, now); !strings.Contains(got, test.want) {
			t.Errorf("reset %+v: got %s", test, got)
		}
	}
	if resetText(nil, now) != "unavailable" {
		t.Fatal("null reset must be unavailable")
	}
}

func TestTerminalLabelsCannotInjectControlSequences(t *testing.T) {
	snapshot := exampleSnapshot(t)
	email := "test\x1b[2J\nuser"
	snapshot.Account.Email = &email
	var out bytes.Buffer
	if err := renderText(&out, snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "\nuser") {
		t.Fatalf("terminal controls survived: %q", out.String())
	}
}
