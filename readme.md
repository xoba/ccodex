# ccodex

**Check Codex** — see your Codex subscription usage, remaining quota, and reset
times from the terminal.

`ccodex` uses your existing Codex ChatGPT sign-in. Run it once for a snapshot or
leave `ccodex watch` running for updates and low-quota alarms. **Monitoring is
read-only by default.** Redeeming earned resets always requires an explicit
command or flag.

## Quick start

On macOS, with [Homebrew](https://brew.sh) installed:

```sh
# Skip this if the Codex CLI is already installed and on your PATH.
brew install --cask codex

# Sign in with your ChatGPT account; skip if already signed in.
codex login

brew install xoba/tap/ccodex
ccodex watch
```

`ccodex` is a companion for people already using the Codex CLI. If `codex` is on
your `PATH` and you are signed in, skip the Codex installation and login steps
above; `ccodex` reuses your existing setup.

Codex is distributed as a Homebrew **cask**, while `ccodex` uses a source-build
**formula**. Homebrew [does not support formulas depending on casks](https://github.com/orgs/Homebrew/discussions/5015),
so the current package cannot install Codex automatically as a dependency.

Watch refreshes every **60 seconds** and sounds an alarm when remaining quota is
**below 5%**. It never consumes an earned reset unless you pass `--auto-reset`.
Press Ctrl-C to stop. For a single snapshot, run `ccodex`.

You need a ChatGPT sign-in that provides Codex subscription limits. API key
authentication alone does not provide this information. `ccodex` needs no
separate API key or credential setup. Homebrew builds this release from source
and installs Go as a build dependency.

## Everyday commands

```sh
ccodex                              # Current status
ccodex status --json                 # Full snapshot as JSON
ccodex play                          # Play the alarm once and exit
ccodex watch                         # Read-only monitoring with alarms
ccodex watch --no-alarm              # Silent, read-only monitoring
ccodex watch --interval 30s          # Refresh every 30 seconds
ccodex watch --alarm-threshold 10    # Alarm below 10% remaining
ccodex watch --json                  # Stream snapshots as JSON lines
ccodex reset --dry-run               # Inspect earned resets without using one
ccodex --help
```

Status includes your account and plan, available quota windows, percentages used
and remaining, reset times and countdowns, earned resets, and available token
activity. Missing optional information is shown as unavailable. Window lengths
come from Codex, and reset times use your local time zone.

Watch appends each snapshot to the terminal. Failed refreshes are reported on
stderr and retried; alarms sound at most once per successful refresh while quota
remains low. macOS uses the built-in Sosumi sound; other platforms use the
terminal bell. JSON snapshots, including any data-availability warnings, stay on
stdout; refresh errors, reset outcomes, and alarms go to stderr.

## Opt in to earned resets

To let watch redeem an available earned reset when quota falls below the alarm
threshold:

```sh
ccodex watch --auto-reset
```

The default cap is **one automatic reset per local calendar day**, shared across
accounts, watch processes, and restarts for the same local user. Set a different
cap explicitly:

```sh
ccodex watch --auto-reset --max-resets-per-day 2
```

Setting a cap alone does not enable automatic resets. `--no-alarm` only mutes
sound; it does not disable resets when `--auto-reset` is present. Codex decides
which quota windows are eligible. Pending attempts retain their request IDs so
retries do not start new redemptions.

To request a reset yourself:

```sh
ccodex reset --dry-run               # Preview first
ccodex reset                         # Immediately request one earned reset
```

`ccodex reset` takes effect without a confirmation prompt and is outside the
watch daily cap. If a request fails or is interrupted, retry with the **same
`--idempotency-key` printed by the command**. A new key starts a new attempt.
See the [usage reference](docs/usage.md) for reset outcomes, retry behavior,
daily accounting, local state, and all flags.

## Upgrade

```sh
brew update
brew upgrade ccodex
ccodex --version
```

If you installed Codex through Homebrew, update it with
`brew upgrade --cask codex`.

## Compatibility and troubleshooting

Developed on macOS against Codex CLI **0.154.0**. `ccodex` communicates with the
Codex app-server, whose interface can change between versions. If token activity
is unsupported or temporarily unavailable, account and quota information still
display with a warning. This tool reports Codex subscription usage; it does not
report general ChatGPT conversation limits or OpenAI API billing.

- **Codex executable not found:** install the Codex CLI and ensure `codex` is on
  your `PATH`, or use `ccodex --codex-bin /path/to/codex`.
- **Not signed in or wrong authentication type:** check `codex login status`, then
  run `codex login` and sign in with ChatGPT.
- **Refresh times out:** try `ccodex status --timeout 30s`.
- **Missing data or protocol errors:** update Codex and `ccodex`, then retry.
  Optional fields depend on what your account and Codex version return.
- **No audible alarm:** run `ccodex play` to test your sound or terminal-bell
  settings. Watch alarms require known remaining quota strictly below the threshold.

Report reproducible problems in [GitHub issues](https://github.com/xoba/ccodex/issues).
Remove account details and other private data from any output you share.

## Development

Requires Go **1.27.1 or newer** and the Codex CLI for live use:

```sh
git clone https://github.com/xoba/ccodex.git
cd ccodex
go build -o ccodex .
./ccodex status

go test ./...
go test -race ./...
go vet ./...
```

Alternatively, install with Go and add your Go binary directory to `PATH`:

```sh
go install github.com/xoba/ccodex@latest
```

Status and reset operations launch `codex app-server` and exchange JSON over
stdin/stdout. Codex manages authentication; `ccodex` has no separate credential
store and does not start a Codex turn to check usage. Tests use helper processes
and do not require a signed-in account. Use `status`, `reset --dry-run`, or
`watch --no-alarm` for read-only checks against a live account.

Release instructions are in [docs/releasing.md](docs/releasing.md).

## License

[MIT](LICENSE).
