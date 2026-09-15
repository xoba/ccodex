# ccodex

**Check Codex** — a Go CLI for viewing your Codex subscription limits and usage,
and redeeming earned rate-limit resets.

`ccodex` uses your existing Codex ChatGPT sign-in to show:

- Account and plan information.
- Available quota buckets and windows, with percentages used and remaining.
- Reset times in your local time zone and reset countdowns.
- Credits and limit state, when returned by Codex.
- The number of available earned resets, with details when returned by Codex.
- Available token usage summaries and the latest daily usage bucket.
- The time the information was refreshed.

Window lengths come from Codex; they are not assumed to be hourly or weekly.
Missing optional data is shown as unavailable. A past reset time does not cause
`ccodex` to assume that quota has recovered.

## Setup

You need Go 1.27 or newer and the OpenAI `codex` CLI on your `PATH`. Sign in to
Codex with your ChatGPT account:

```sh
codex login
go build -o bin/ccodex ./cmd/ccodex
./bin/ccodex status
```

API key authentication alone does not provide the subscription information this
version needs. No separate API key, Keep enrollment, or credential setup is
required for `ccodex`.

To install the executable into your Go binary directory:

```sh
go install ./cmd/ccodex
```

The examples below assume that directory is on your `PATH`.

## Usage

```sh
ccodex                         # Show current status
ccodex status                  # Same as the default command
ccodex status --json            # Structured snapshot, including daily history
ccodex watch                   # Refresh continuously
ccodex watch --interval 30s     # Wait 30 seconds between refreshes
ccodex watch --json             # One JSON object per successful refresh
ccodex watch --no-alarm         # Monitor without sound
ccodex watch --alarm-threshold 10 # Sound below 10% remaining
ccodex reset --dry-run          # Inspect earned resets without consuming one
```

`watch` refreshes immediately, then waits between completed refreshes. The default
interval is `60s`, with a minimum of `1s`. Text mode appends snapshots to the
terminal; JSON mode writes one object per line. Failed refreshes are reported on
stderr and retried. Press Ctrl-C to stop.

`watch` sounds at most once per successful refresh when any reported quota window
or individual spend limit has remaining quota **strictly below the alarm
threshold**, repeating while the quota stays low. Set `--alarm-threshold PERCENT`
to a finite number from `0` through `100`, including decimals; the default is `5`.
Values exactly at the threshold, unknown values, and failed refreshes do not
trigger an alarm. A threshold of `0` disables threshold alarms; `--no-alarm`
explicitly mutes watch sound. Other commands never sound.
macOS plays the built-in Sosumi sound; other platforms use a terminal bell, whose
audibility depends on terminal settings. Sound errors produce a warning without
stopping monitoring, and `--json` keeps stdout clean for JSON consumers.

Global flags are available on every command:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--codex-bin PATH` | `codex` | Codex executable to use |
| `--timeout DURATION` | `15s` | Timeout for each refresh, or the whole reset operation |

For example:

```sh
ccodex status --codex-bin /path/to/codex --timeout 30s
ccodex --help
```

## Redeem an earned reset

`ccodex reset` **immediately requests one earned reset, without a confirmation
prompt**. Codex decides whether any quota windows are eligible. Normal status,
watch, and dry-run commands never consume resets.

Inspect available resets first:

```sh
ccodex reset --dry-run
ccodex reset --dry-run --json
```

The preview shows the available count and any returned credit IDs, titles,
statuses, and expiration times. Detail rows may be capped; the available count is
authoritative. Dry-run JSON contains `dryRun: true`, an optional requested
`creditId`, and the full `snapshot`. Reset details are also included in status
JSON. A preview does not guarantee eligibility when a reset is requested.

To redeem one reset:

```sh
ccodex reset                         # Codex selects the credit
ccodex reset --credit-id CREDIT_ID    # Select a specific earned reset
```

Every redemption attempt has an **idempotency key**, which identifies that one
attempt if it needs to be retried. By default, `ccodex` generates a UUID and
writes it to stderr before sending the request. You can supply your own key:

```sh
ccodex reset --idempotency-key my-reset-attempt-001 --json
```

If the command fails or is interrupted, retry with the **exact same key** using
`--idempotency-key`. Preserve `--credit-id` too if you supplied one. A fresh key
starts a new attempt and could consume another reset. Reset requests are never
retried automatically.

The backend reports one of these outcomes:

| Outcome | Meaning |
| --- | --- |
| `reset` | The reset was applied. |
| `alreadyRedeemed` | This attempt already redeemed a reset; no additional reset was consumed. |
| `nothingToReset` | There was nothing eligible to reset; no reset was consumed. |
| `noCredit` | No eligible reset credit was available for this request; no reset was consumed. |

All four outcomes exit successfully. Network, protocol, and authentication
failures exit with an error. Reset JSON includes `outcome`, `idempotencyKey`,
`account`, `rateLimits`, `fetchedAt`, and any `warnings`.

After a known outcome, `ccodex` reads the actual quota again. If that refresh
fails, it preserves the successful outcome and reports a warning. Run
`ccodex status` to check the quota; do not request a fresh reset just to refresh
the display. `--timeout` covers the entire reset operation, including the
follow-up quota read.

See the [official earned-reset documentation](https://learn.chatgpt.com/docs/app-server#8-earned-rate-limit-resets-chatgpt).

## Scope and compatibility

This first version monitors **Codex subscription usage**. Access to general
ChatGPT conversation message quotas has not been verified. OpenAI API usage and
cost reporting would be a separate provider, potentially using the OpenAI Go SDK
and Keep for API credentials later.

`ccodex` uses the documented Codex app-server account, rate-limit, usage, and
earned-reset endpoints. That interface is evolving; the initial implementation
targets the schema available in Codex CLI `0.154.0`. If the usage endpoint is
unsupported, `ccodex` reports a warning and still displays available account and
limit data.
See the [official app-server documentation](https://learn.chatgpt.com/docs/app-server#auth-endpoints).

## Development

The CLI uses Cobra. Each operation launches `codex app-server` and exchanges JSON
messages over stdin/stdout using Go's standard library. Codex manages
authentication; `ccodex` does not maintain a separate credential store. Read
commands are read-only; `reset` is the explicit write operation.

Reset tests use helper processes. Development checks against a live account use
`reset --dry-run` and never redeem a reset.

```sh
go test ./...
go test -race ./...
go vet ./...
```
