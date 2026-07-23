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
PowerShell knob; macOS `${HOME}/…`) — no cross-OS path rewriting. Credentials are
never transported; the command prints a login matrix (account × machine).

### `cpm add … --fleet`
Adds the account locally, then runs the peer's `cpm add` on every reachable peer.

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
- Fleet ops only **add** profiles; removal is never automated (avoids the
  junction-follow data-loss trap).
