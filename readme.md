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

**Watch automatically redeems an available earned reset when quota is low, up to
one per local calendar day by default.** Use `watch --auto-reset=false` for
monitoring without automatic redemption.

## Setup

You need Go 1.27 or newer and the OpenAI `codex` CLI on your `PATH`. From the
project root, sign in to Codex with your ChatGPT account and build:

```sh
codex login
go build .
./ccodex status
```

To run directly from the project root, showing status by default:

```sh
go run .
```

API key authentication alone does not provide the subscription information this
version needs. No separate API key, Keep enrollment, or credential setup is
required for `ccodex`.

To install the executable into your Go binary directory:

```sh
go install .
```

The examples below assume that directory is on your `PATH`.

## Usage

```sh
ccodex                                  # Show current status
ccodex status                           # Same as the default command
ccodex status --json                     # Snapshot, including daily history
ccodex watch                            # Monitor, sound alarms, and auto-reset
ccodex watch --interval 30s              # Wait 30 seconds between refreshes
ccodex watch --json                      # Stream snapshots as JSON lines
ccodex watch --auto-reset=false          # Monitor and sound alarms only
ccodex watch --max-resets-per-day 2       # Allow up to two automatic resets per day
ccodex watch --max-resets-per-day 0       # Disable automatic resets; keep alarms
ccodex watch --no-alarm                  # Mute sound; auto-reset stays enabled
ccodex watch --alarm-threshold 10        # Alarm and auto-reset below 10% remaining
ccodex reset --dry-run                   # Inspect resets without consuming one
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
trigger an alarm. A threshold of `0` disables alarms and new automatic reset
attempts. `--no-alarm` mutes watch sound but **does not disable automatic resets**.
Other commands never sound.
macOS plays the built-in Sosumi sound; other platforms use a terminal bell, whose
audibility depends on terminal settings. Sound errors produce a warning without
stopping monitoring, and `--json` keeps stdout clean for JSON consumers.

### Automatic resets in watch

`--auto-reset` defaults to `true`. On a fresh successful poll below the alarm
threshold, watch sounds once unless muted, then starts a single reset attempt
for that continuous low-quota period if the reported available reset count is
greater than zero and the daily cap permits it. Codex decides which quota windows
are eligible. Use `--auto-reset=false` to keep alarm monitoring without consuming
resets, or this command for silent, read-only monitoring:

```sh
ccodex watch --auto-reset=false --no-alarm
```

`--max-resets-per-day COUNT` sets the daily cap, with a default of `1`. It accepts
a nonnegative integer; `0` disables automatic resets while leaving alarms
enabled. The cap follows the local calendar day and is shared by the same local
user across accounts, watch processes, and restarts. It applies only to automatic
resets; manual `ccodex reset` is unaffected.

An automatic attempt counts toward the cap before it is sent. A `reset` or
`alreadyRedeemed` outcome retains that count; `noCredit` and `nothingToReset`
release it. Pending attempts count against each day's cap until resolved;
confirmed redemption also counts on the day its success is recorded. An attempt
that crosses midnight can therefore count on both days. Retrying the same key
does not add another count within the same day. If daily accounting is
unavailable or corrupt, watch reports an error and continues monitoring and
alarms without automatic resets.

Automatic resets require Codex to supply a stable account identity. If it is
unavailable, or the signed-in account changes during a tracked attempt,
automatic resets pause while monitoring and alarms continue. A pending attempt
is resumed only for its original account; watch checks the account again before
requesting a reset.

After `reset` or `alreadyRedeemed`, watch reads and displays actual quota
immediately. Healthy subsequent polls stop the alarm. If quota remains low, the
alarm continues, but watch does not spend another reset until a later ordinary
watch poll confirms that the quotas which triggered the attempt have recovered.
Missing quota data does not count as recovery. Any new low-quota period remains
subject to the daily cap.

Watch prints the attempt's request key to stderr before consumption. A
`noCredit` or `nothingToReset` outcome is retried on later low-quota polls with
available resets, using that same key. If the outcome is unknown, watch retains
the key and retries while quota is low, even if the reported reset count becomes
zero, to confirm any earlier redemption. It never creates a new key from an
unresolved attempt. Reset outcomes and warnings go to stderr; `--json` stdout
contains only snapshot objects, including an extra updated snapshot after a
reset when the quota refresh succeeds.

The daily count and pending request key persist, so restarting watch does not
bypass the cap. When restarted on the same account, watch automatically resumes
a pending attempt with its saved key once a fresh poll shows low quota. Tracking
of a completed low-quota period lasts only for the current process. You can also
resolve an unknown outcome manually on the original account with
`ccodex reset --idempotency-key KEY`, using the printed key. The manual reset
command below explains the possible outcomes and retry behavior.

### Global flags

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
prompt**. Codex decides whether any quota windows are eligible. `status` and
`reset --dry-run` never consume resets.

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
starts a new attempt and could consume another reset. Manual `reset` requests
are never retried automatically.

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

## Local state

Daily automatic-reset accounting is stored in
`ccodex/auto-resets.json` under the platform's user configuration directory
(`os.UserConfigDir()` in Go). On macOS, the path is
`~/Library/Application Support/ccodex/auto-resets.json`.

## Development

The CLI uses Cobra. Each operation launches `codex app-server` and exchanges JSON
messages over stdin/stdout using Go's standard library. Codex manages
authentication; `ccodex` does not maintain a separate credential store. `status`,
`reset --dry-run`, and `watch --auto-reset=false` are read-only. Manual `reset`
and watch with automatic resets enabled can consume earned resets.

Reset tests use helper processes. Development checks against a live account use
`status`, `reset --dry-run`, or `watch --auto-reset=false --no-alarm` and never
redeem a reset.

```sh
go test ./...
go test -race ./...
go vet ./...
```
