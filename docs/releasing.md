# Releasing ccodex

The primary repository is [xoba/ccodex](https://github.com/xoba/ccodex).
Homebrew installs tagged source archives using
[Formula/ccodex.rb](https://github.com/xoba/homebrew-tap/blob/main/Formula/ccodex.rb)
in `xoba/homebrew-tap`. There are no prebuilt release binaries or cross-repository
automation credentials to maintain.

Release tags use `vMAJOR.MINOR.PATCH`, starting with `v0.1.0`. The binary's version
is injected at build time through `github.com/xoba/ccodex/internal/buildinfo.Version`,
without the leading `v`. An ordinary development build reports `dev`.

## 1. Prepare and verify

Start from an up-to-date `main` checkout with a clean working tree. Use the Go
version declared in `go.mod`, Homebrew, and an authenticated `gh` installation
with write access to both repositories.

```sh
git switch main
git pull --ff-only origin main
git status --short
go mod verify
go vet ./...
go test -race ./...
```

Update the README when commands or defaults change. In particular, monitoring
must remain read-only by default: automatic redemption requires `watch
--auto-reset`, and manual redemption requires the explicit reset command.

Choose an unused release version and check the production build. The temporary
directory also holds the release notes and source archive for later steps.

```sh
ccodex_version=0.1.0
ccodex_tag="v${ccodex_version}"
ccodex_release_dir="$(mktemp -d)"
go build -trimpath \
  -ldflags "-s -w -X github.com/xoba/ccodex/internal/buildinfo.Version=${ccodex_version}" \
  -o "$ccodex_release_dir/ccodex" .
"$ccodex_release_dir/ccodex" --version
"$ccodex_release_dir/ccodex" --help
"$ccodex_release_dir/ccodex" watch --help
```

These commands need no Codex installation or account. Tests use simulated Codex
responses; successful CI does not establish compatibility with every Codex CLI
version. For changes to the Codex protocol, separately check `status` and a short
read-only `watch` session with a signed-in account and record the tested Codex
version in the README. Reset operations spend earned resets and should only be
used deliberately during manual verification.

Commit and push any release preparation before tagging. The
[CI workflow](../.github/workflows/ci.yml) runs vet, race tests, and version/help
smoke tests on Linux x86-64 and macOS Apple Silicon and Intel. Review the latest
run for the exact release commit and wait for all jobs to pass:

```sh
gh run list --repo xoba/ccodex --workflow ci.yml --commit "$(git rev-parse HEAD)"
gh run watch RUN_ID --repo xoba/ccodex --exit-status
```

Replace `RUN_ID` with the appropriate run ID. Runner labels are documented in
[GitHub's runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
The workflow pins official checkout and setup-go actions to commit SHAs; update
both the SHA and its version comment when upgrading either action.

## 2. Tag and publish the release

```sh
git tag -a "$ccodex_tag" -m "Release $ccodex_tag"
git push origin "$ccodex_tag"
gh run list --repo xoba/ccodex --workflow ci.yml --branch "$ccodex_tag"
gh run watch RUN_ID --repo xoba/ccodex --exit-status
```

Wait for the tag's CI run to appear, then watch that run. Write release notes to
`$ccodex_release_dir/notes.md`, covering user-visible changes, compatibility, and
the installation command. Publish only after the tag's checks pass:

```sh
gh release create "$ccodex_tag" --repo xoba/ccodex --verify-tag \
  --title "$ccodex_tag" --notes-file "$ccodex_release_dir/notes.md"
```

Published tags are permanent. If a published release needs a fix, issue a new
patch release rather than moving the tag or replacing its contents.

## 3. Update the Homebrew tap

Download exactly the source archive that the formula will use and calculate its
SHA-256 checksum:

```sh
curl --fail --location --retry 3 \
  "https://github.com/xoba/ccodex/archive/refs/tags/${ccodex_tag}.tar.gz" \
  --output "$ccodex_release_dir/ccodex.tar.gz"
shasum -a 256 "$ccodex_release_dir/ccodex.tar.gz"
brew tap xoba/tap
cd "$(brew --repository xoba/tap)"
git pull --ff-only
```

Edit `Formula/ccodex.rb` to update the `url` tag and `sha256` value. Its build
command must continue to inject the formula's version:

```ruby
system "go", "build", *std_go_args(
  ldflags: "-s -w -X github.com/xoba/ccodex/internal/buildinfo.Version=#{version}",
)
```

The formula must also declare `uses_from_macos "sqlite"`, because `ccodex`
records its history through the `sqlite3` tool. On macOS this uses the system
copy and installs nothing; on Linux it installs Homebrew's. `ccodex history path`
needs neither Codex nor `sqlite3`, which makes it a suitable formula test.

Check that Homebrew's Go dependency meets the minimum version in `go.mod`. Test
the edited formula before pushing it:

```sh
brew style xoba/tap/ccodex
brew audit --strict xoba/tap/ccodex
brew install --build-from-source xoba/tap/ccodex
brew test xoba/tap/ccodex
"$(brew --prefix ccodex)/bin/ccodex" --version
"$(brew --prefix ccodex)/bin/ccodex" watch --help
```

If `ccodex` is already installed, use `brew reinstall --build-from-source
xoba/tap/ccodex` in place of `brew install`. Use the explicit Homebrew executable
path above to avoid accidentally checking an older development binary earlier
on `PATH`. Formula tests must work without Codex or a signed-in account.

```sh
git diff -- Formula/ccodex.rb
git add Formula/ccodex.rb
git commit -m "ccodex ${ccodex_version}"
git push
```

## 4. Check the public installation

After the tap update is available, verify from a fresh Homebrew installation or
another machine:

```sh
brew update
brew install xoba/tap/ccodex
ccodex --version
ccodex watch --help
```

Existing users update with `brew upgrade ccodex`. Actual usage checks also need
the Codex CLI and a signed-in account; the README's quick start covers that
setup. `ccodex watch` must show usage without automatically spending resets.
