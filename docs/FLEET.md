# `cpm add` + Fleet

Two features for running the same set of Claude accounts across multiple machines
(e.g. a Windows desktop + a macOS laptop) with one command and no per-OS drift.

## `cpm add <email> <alias>`

Adds a new Claude account as an isolated profile in one step:

1. Validates `<alias>` (safe name) and `<email>`.
2. Clones `args` + `env` from a **Max template** — `--from <profile>`, else
   `fleet.default_template`, else the first profile with no custom base URL/token.
   (glm-class profiles are never used as a template.)
3. Appends a `[profiles.<alias>]` block to `config.toml` (text-append — existing
   comments/formatting are preserved; backslash paths are written as single-quoted
   TOML literals).
4. Materializes the profile dir (seed `settings.json` etc. from `source_dir`, link
   the shared `commands/skills/agents/plugins/projects`), optionally syncs MCP
   (see `manage_mcp`), and installs the `claude-<alias>` launcher.
5. Prints the one manual step: `claude-<alias>` then `/login`. **Credentials are
   never created or copied.**

```
cpm add claude1@digrum.com digrum1
cpm add someone@example.com work --from bdaya
cpm add someone@example.com work --fleet     # also add on every peer (see below)
```

## `manage_mcp` — who owns each profile's MCP servers

```toml
manage_mcp = false   # top-level; default true
```

- `true` (default): `cpm install` / `cpm add` copy the `mcpServers` block from
  `~/.claude.json` into every profile's `.claude.json`.
- `false`: cpm **never touches** any profile's `mcpServers`. Use this when an
  external MCP proxy/gateway is the sole owner of each profile's MCP roster, so
  cpm can't revert the gateway's wiring. After `cpm add` under `manage_mcp=false`,
  run your gateway's provisioning so the new profile gets its MCP servers.

## Fleet — same accounts across machines

Declare peers in `config.toml`. Each machine keeps its **own** `source_dir`,
`bin_dir`, and OS-appropriate profile env; only the *set of accounts* is shared.

```toml
[fleet]
id = "windows-desktop"       # this machine (informational)
default_template = "digrum"  # template for cpm add

[fleet.peers.macbook]
host = "macos"               # SSH host/alias (from ~/.ssh/config)
os   = "darwin"              # windows | darwin | linux
cpm  = "~/dev/bin/cpm"       # the peer's cpm binary (default: "cpm" on PATH)
# config_path = "~/.claude-profiles/config.toml"   # default shown
```

### `cpm fleet status`
Shows this machine's profiles and, for each peer: SSH reachability and a
profile-set diff (which aliases are missing where). Read-only.

### `cpm fleet sync`
Reconciles the **union** of account aliases across the local machine and every
reachable peer. Any alias present somewhere but missing on a machine is added
there — locally via `cpm add`, remotely by invoking the **peer's own `cpm add`
over SSH**. Because each machine materializes the profile from *its own* template,
the result is correct for that machine's OS (Windows `${USERPROFILE}\…` + the
PowerShell knob; macOS `${HOME}/…`) — no cross-OS path rewriting. `cpm fleet sync`
itself never transports credentials — it prints a login matrix (account × machine)
and each newly-added profile needs its own `/login`, **or** a subsequent
`cpm fleet creds push` to hand off an existing login instead (see below).

### `cpm add … --fleet`
Adds the account locally, then runs the peer's `cpm add` on every reachable peer.

## `cpm fleet creds` — moving an existing login between machines

Three verbs under `fleet` (not a top-level `cpm creds` — that would sit
confusingly beside the existing top-level `cpm credentials`, which is a
**local-only** status view). `cpm fleet creds` with no subcommand behaves as
`status`.

**This is a one-shot handoff, not replication.** Each account has a *home*
machine at any moment; `push`/`pull` moves it. Two machines actively sharing one
refresh-token chain WILL race: access tokens live ~8h, refresh tokens are
single-use and rotate, and whichever machine refreshes first silently kills the
other's copy (`invalid_grant` → Claude Code wipes that machine's
`.credentials.json`). There is no daemon, no cron, no auto-push-on-refresh, and
no bidirectional reconcile — all three would multiply the number of refresh
callers, which is exactly the failure mechanism. Recovery from a lost race is a
**re-push/pull** from whichever machine still holds a live chain, not a browser
login — `cpm fleet creds status` names the divergence (different lineage
fingerprint) before it bites.

### `cpm fleet creds status [alias...]`
Read-only matrix: local vs every peer, per profile. `--peer <name>` (repeatable)
narrows to specific peers. Never writes anything, local or remote. Shows, per
machine: token expiry (`claudeAiOauth.expiresAt`, not mtime — server-issued,
immune to clock skew), refresh-token horizon, and a lineage fingerprint `fp` —
`sha256(refreshToken)[:12]` hex. It is **not** the secret (a 48-bit prefix of a
SHA-256 hash over a high-entropy token is not invertible): same `fp` on two
machines means the same token chain (safe to push/pull between); different `fp`
means two independent logins racing (`! DIVERGENT LINEAGE`).

### `cpm fleet creds push <alias>... | --all`
Sends **local → peer(s)** over an SSH **stdin pipe** — never argv, never `scp`,
never base64 on the wire. The remote write is `umask 077; cat > tmp && chmod 600
&& mv -f` — 0600 from the moment the file exists (no world-readable window),
atomic same-filesystem rename (a killed transfer leaves only the `.cpm-tmp`
file). Only `claudeAiOauth` is overwritten on the destination; the peer's own
`mcpOAuth` grants (third-party integrations — Cloudflare, Figma, Sentry — keyed
per-machine by a config-hash suffix) and any unknown top-level keys are
preserved unless `--include-mcp` is passed.

Flags: `--peer <name>` (repeatable; default every reachable peer), `--all`
(every locally-authenticated profile that passes the guards — mutually
exclusive with alias args, exactly one of the two is required), `--dry-run`
(print the plan, write nothing), `--yes`/`-y` (assume yes to the routine
overwrite prompt; **required** when stdin is not a TTY, e.g. dispatch/background
— otherwise the command errors instead of hanging), `--force` (override a
*hard* refusal — see Gate B below; deliberately distinct from `--yes`),
`--include-mcp` (also transport `mcpOAuth`; default off).

Pushing **to a Windows peer is refused outright** — piping data on stdin while
also passing a PowerShell script through two shells' quoting layers has a
silent-corruption failure mode on a credentials file. Use `pull` on the Windows
machine instead.

### `cpm fleet creds pull <alias>... | --all --peer <name>`
**Peer → local**, SSH stdout capture (stdout/stderr kept in separate buffers —
never `CombinedOutput`, so a stderr banner line can never splice into the JSON
payload), written locally via an atomic temp+rename at `0600`. `--peer` is
**required** whenever more than one peer is configured — a pull has no sensible
"merge from all" semantics. This is the supported route onto a Windows
destination: push FROM Windows and pull ON Windows are the two ways credentials
move into a Windows box.

### Confirmation UX — two distinct gates
- **Gate A (routine overwrite, `--yes` clears it):** destination already has
  this profile, same or older token. Prompted unless `--yes`/`--dry-run`.
- **Gate B (hard refusal, needs `--force` even with `--yes`):** any of —
  destination is **strictly newer** than the source (overwriting would kill a
  working session); source's `refreshTokenExpiresAt` is already in the past
  (pushing/pulling a dead token over a possibly-live copy); `--include-mcp`
  would delete an `mcpOAuth` grant the destination has and the source doesn't.
  The message always names the safe alternative (`pull` instead of a refused
  `push`, or `--force` to proceed anyway).

Additional, non-overridable refusals: peer profile directory absent (hints at
`cpm fleet sync`); source has no credentials (skipped, so `--all` is safe); the
shared `default`/`~/.claude` identity (`cpm fleet creds` only ever touches
`<profilesBase>/<alias>/`, never the shared keychain-only identity).

### `cpm fleet creds verify <alias> --peer <name>`
Runs a real headless auth check on the peer — `claude-<alias> -p "reply with
the single word OK" --output-format text` over non-interactive SSH, no GUI, no
keychain unlock available. A response is direct proof that the pushed file
alone is authenticating that session.

### The macOS Keychain caveat
**v1 is file-store only — cpm never reads, writes, or unlocks the macOS
Keychain.** Every darwin-targeting push prints a note to that effect and points
at `cpm fleet creds verify` to confirm live. This is deliberate, not an
oversight: under `CLAUDE_CONFIG_DIR=<profile dir>`, token refresh has been
observed to persist to the profile's `.credentials.json` file and not back to
the per-profile Keychain item, so the file is believed to be the
read-authoritative store for these profiles — `verify` is the concrete way to
confirm that for any given profile without decrypting anything.

### OS-convention translation (fallback)
The primary path needs no translation (each machine uses its own template). If a
peer has no Max template to clone from, `TranslateEnvForOS` converts path env
values between `${USERPROFILE}\` and `${HOME}/` conventions and drops
Windows-only keys (e.g. `CLAUDE_CODE_USE_POWERSHELL_TOOL`) on unix peers.

## Requirements & invariants

- SSH to each peer must be non-interactive (key-based; usable with `BatchMode`).
- The new cpm binary must be deployed on every machine you drive fleet ops from
  or to (peers run *their own* `cpm add`).
- No secrets ever land in `config.toml`, launchers, or SSH command lines.
  Credentials move only over an SSH **stdin pipe** (`cpm fleet creds`), never in
  argv, never through the git-backed `cpm cloud` channel (which allowlists what
  it syncs and never enumerates `.credentials.json`).
- Fleet ops only **add** profiles; removal is never automated (avoids the
  junction-follow data-loss trap).
