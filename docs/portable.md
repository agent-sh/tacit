# Local and portable tacit

Choose a local store when your agents run on one machine. Choose the server when machines need to share facts. Both use the same Markdown facts, git history, retrieval and review methods.

## Local storage

`tacit init` creates a standalone repository at the configured store path. It refuses existing non-local repositories and non-empty directories, and is safe to rerun. Initialization is staged before the completed repository is moved into place.

The store records `tacit.local=true` in its local git configuration. This explicit marker selects local behavior; a failed server connection never changes the backend. Writes commit only their named files. Sync does no network work. `install follow` explains that a feed is unnecessary. `follow` returns its non-restarting configuration status (78, translated to 0 under launchd) so an existing service does not loop.

New local stores start with evidence-based ruling rules. An agent's `user_standard` label is not proof. The app records owner contributions as owner commits; imported quotes need backed evidence. Existing and server-created unaudited stores still need `tacit rulings audit` before relying on the new definition.

Local mode needs no enrollment keys. The OS account controls access. Agent notes still require scrump. Local commits reject the same obvious token shapes and private-key headers as the server's pre-receive check. A refused untracked inbox note is quarantined as `.rejected`; tracked files are restored and cached indexes invalidated. Rejected files are not facts and are excluded from verifier probes. This does not provide the server's authentication or multi-machine inbox boundary. Keep the store in your own account's private directory.

To use a server later, choose a separate store path and use `tacit clone` with your own server and enrolled keys. Back up or keep the local repository before choosing another store. `init` does not convert a clone.

## Configuration

Defaults remain compatible with existing installations. The optional file lives at `~/.config/tacit/config.yaml` on Linux/macOS, or `%APPDATA%\tacit\config.yaml` on Windows:

```yaml
store: ~/my-facts
projects: ~/projects
index: ~/.local/state/tacit/index
keys: ~/.config/tacit
local_overlay: ~/.local/share/tacit/local
owner_names: [dana]
verify_roots: []
runner: claude
models:
  curate: opus
  judge: haiku
  dream: haiku
  verify: opus
  verify_fallback: sonnet
```

Use your own values. `~` is the current user's home. Relative paths resolve against the file's directory, so hook working directories cannot change the store. Windows paths can be single-quoted strings, such as `store: 'C:\Users\Dana\My Facts'`, or use forward slashes.

Precedence is file defaults, then `TACIT_*` environment overrides, then command flags. `tacit --config FILE init` or `TACIT_SETTINGS` selects another file. Unknown fields and a missing explicitly selected file are errors. A hook with bad configuration fails open without output. Keep provider credentials in the provider's own configuration or the job env file, not this settings file.

`init --store DIR` overrides that invocation only. Put a custom path in the file or environment for subsequent commands. The file loads before model defaults are selected. A job's explicitly selected env file loads at runtime as well. Installers carry an explicit config selection into their generated jobs and hooks.

### Model runners

Claude is optional. Supported runner names are `claude`, `codex`, `hermes`, `grok`, `pi`, `muse` and `agy`. Install and log in to the CLI you select. Tacit uses that CLI's provider configuration; it holds no provider credentials.

The defaults above preserve the original setup. For another runner, omit the Claude model aliases and let the CLI select its model, or set a model ID accepted by that CLI:

```yaml
runner: codex
runners:
  dream: muse
  judge: agy
commands:
  muse: ~/.local/bin/muse
runner_args:
  codex: ["--profile", "work"]
```

`runners` accepts `dream`, `curate`, `judge` and `verify`. An omitted role inherits `runner`. `tacit --runner muse dream` overrides all role selections for that invocation. `models` accepts the same roles plus `verify_fallback`; those values go to the selected CLI's `--model`. For non-Claude runners an omitted fallback disables the capacity fallback; set a distinct fallback model when needed.

`commands` overrides executable paths, including Windows paths with spaces. `runner_args` adds an argument array without a shell, for provider or profile options. These are trusted operator settings: avoid options that replace Tacit's tool restrictions, output format or prompt source. Command flags take precedence over the job env file too.

The integrations and model runners are separate. You can use Muse for memory and Codex for dreaming. `tacit install muse` prints a `mcpServers` entry for Muse's existing settings (preserve its `schema_version` and other keys). `tacit install agy` prints an `agy mcp add` command. Both expose `tacit_search`, `tacit_fact` and `tacit_add`; shared stores also need their writer keys enrolled.

Transcript mining and automatic owner-quote lookup currently read Claude Code and Codex histories. Muse, agy, Grok, pi and Hermes can read and write memory through their integrations; their transcripts are not yet mined. Owner contributions and decisions in the app remain available without Claude. Curation refuses an empty owner-evidence corpus rather than guessing whether an agent paraphrase is an owner ruling.

### Environment variables

| variable | default | what |
|---|---|---|
| `TACIT_SETTINGS` | platform config file above | optional client YAML settings |
| `TACIT_STORE` | `~/.local/share/tacit/store` | the store clone |
| `TACIT_CONFIG` | `~/.config/tacit` | keys |
| `TACIT_INDEX` | `~/.local/state/tacit/index` | FTS5 index files |
| `TACIT_LOCAL` | `~/.local/share/tacit/local` | local overlay of a read-only clone |
| `TACIT_FOLLOW_STATE` | `~/.local/state/tacit/follow.json` | follower heartbeat |
| `TACIT_PROJECTS` | `~/projects` | where repo checkouts live: derive, results, stale, the timeline, the verifier, the dreamer and import |
| `TACIT_DERIVE_CONFIG` | `~/.config/tacit/derive.yaml` | `branches:` the branch to read per repo (`tacit derive --set-branch repo=branch`); `ordinary_names:` repo names that are also plain words, so a bare mention is not evidence a fact is about that repo |
| `TACIT_WRITER` | | the key a push signs with |
| `TACIT_OWNER_NAMES` | | names agents use for the owner in facts ("Dana 2026-09-05: ..."), comma separated; each counts like "owner" when the rulings audit reads a clause attributed without quote marks |
| `TACIT_RUNNER` | `claude` | default model-job CLI |
| `TACIT_DREAM_RUNNER`, `TACIT_CURATE_RUNNER`, `TACIT_JUDGE_RUNNER`, `TACIT_VERIFY_RUNNER` | global runner | per-job CLI |
| `TACIT_<TOOL>_COMMAND` | tool name | executable path, e.g. `TACIT_MUSE_COMMAND` |
| `TACIT_<TOOL>_ARGS` | | JSON argv array, e.g. `["--profile","work"]` |
| `TACIT_CURATE_MODEL` | `opus` | curator split |
| `TACIT_JUDGE_MODEL` | `haiku` | contradiction judge |
| `TACIT_DREAM_MODEL` | `haiku` | dreamer |
| `TACIT_VERIFY_ROOTS` | | extra read-only source directories, separated like `PATH`; each must exist. Claude auto-memory is included when present |
| `TACIT_VERIFY_MODEL` | `opus` | verifier |
| `TACIT_VERIFY_FALLBACK_MODEL` | `sonnet` | verifier when the first model has no capacity (429, 503, 529) |
| `TACIT_SERVER`, `TACIT_ENROLL_SSH` | | `scripts/mac-setup.sh`: server URL, and the ssh target that runs the enroll command |

The model defaults in this table apply only to Claude. Other runners use their CLI default unless a model is explicitly configured. Model values are anything that runner's `--model` takes. A `--model` flag wins over the variable. Server variables (`TACIT_DATA`, `TACIT_BIND`, `TACIT_PORT`, `TACIT_PUBLIC_PORT`, ...) are listed in `server/lib/tacit_server/config.ex`.


## Linux and macOS

Build the CLI with Go 1.27 and git on PATH. The included browser UI needs no GTK, WebKit or macOS SDK build step:

```sh
mkdir -p ~/.local/bin
(cd client && go build -o ~/.local/bin/tacit ./cmd/tacit)
export PATH="$HOME/.local/bin:$PATH"
tacit init
tacit app --browser
```

Install the harness with `install claude`, `install codex` or another supported target. Codex requires trusting new hooks in `/hooks` once.

Optional model jobs:

```sh
tacit install dream --env-file ~/.config/tacit/env
tacit install curate --env-file ~/.config/tacit/env
```

Install writes files and prints start commands; it starts nothing. Linux gets user services/timers. macOS gets launchd agents. Dreaming runs every four hours; curation and verification run daily. `tacit maintain --env-file FILE` runs the daily sequence manually on any OS. Local jobs need no enrollment. Server-backed jobs retain their key requirements. Create the env file and configure the provider and PATH before starting a model job.

## Windows

The native `.exe` needs neither WSL nor a Linux server in local mode. Install Git for Windows and keep git on PATH. Build from PowerShell with Go 1.27, or unpack a Windows archive made by the distribution tool:

```powershell
Push-Location client
go build -o "$HOME\tacit.exe" ./cmd/tacit
Pop-Location
& "$HOME\tacit.exe" init
& "$HOME\tacit.exe" install claude
& "$HOME\tacit.exe" app --browser
```

Claude hooks use its [executable-plus-arguments form](https://code.claude.com/docs/en/hooks#exec-form-and-shell-form), preserving paths with spaces without shell quoting. Codex hooks use an encoded PowerShell command that preserves arguments and exit codes. MCP uses the executable and argument array directly. No integration depends on `%USERNAME%`.

Model jobs need the selected native agent executable and compatible scrump on PATH. Verifier filesystem probes also need Git for Windows' Unix utilities: include its `usr\bin` directory in the job PATH for grep, find and ls.

`install dream`, `install curate` and server-mode `install follow` write Task Scheduler XML under `~\.config\tacit\tasks`. Run the printed PowerShell registration/start commands under the current Windows account. No administrator account or stored password is requested. Tasks run while that account is logged in and suppress overlapping instances. `install app` writes a PowerShell browser launcher.

Real task registration and default-browser launching require Windows. Generating XML or running under Wine does not verify those Windows services. Both are checked in the native Windows VM described below. Env files may use UTF-8 with or without the BOM written by Windows PowerShell.

## Redaction

Agent writes and model digests need scrump's `text` profile. Published scrump v0.1.8 artifacts lack it. Build the pinned source tacit uses:

```sh
cargo install --locked --root ~/.local --git https://github.com/avifenesh/scrump \
  --rev 17cd5abc9ce19d4fdec51588a35ef73a644e36bb scrump-cli
```

On Windows use those arguments on one PowerShell line with a Windows install root, then put that root's `bin` on PATH. Native Rust MSVC builds need the Windows C++ toolchain. The pinned source also builds for Windows GNU. A binary lacking the text profile is refused before an agent note is written. Owner app writes retain the token-shape commit check without requiring a model account.

## Build archives

From `client/`:

```sh
go run ./tools/dist -out ../dist -version dev
```

This builds Linux, macOS and Windows archives, each on amd64/arm64, plus `SHA256SUMS`. Windows gets ZIP; Linux/macOS get tar.gz. License, notice and bundled-font license texts are included. Git is a runtime dependency. Scrump and the model CLI are separate dependencies for features that use them. Archives contain the CLI/browser app, not native desktop-window builds.

## Validation

The smoke tool initializes a fresh store with a configured path containing spaces, opens the browser API without launching a browser, records an owner ruling, retrieves it, lints and syncs, and confirms that no feed is needed:

```sh
go run ./tools/smoke -binary /path/to/tacit
```

With compatible scrump on PATH, `-with-redactor` checks that a synthetic credential in an agent note is masked before storage.

Linux runs, the existing Go suite, local lifecycle tests, configuration and platform locks are checked locally. The standalone smoke check also passes in a fresh Ubuntu 24.04 Docker container with git installed.

A disposable Windows 11 Enterprise Evaluation VM (build 26300) checks the native executable, Git for Windows, locks/liveness, the browser API, agent redaction, a configured local store and hook installation. Dream and curator tasks register under the logged-in account, run and finish with status 0. Edge opens the local app through the default-browser handler. The job inputs are synthetic and require no provider credentials; this checks scheduling and startup, not a model's output. Native testing found and fixed Task Scheduler's Unicode XML encoding conflict and PowerShell's BOM-prefixed env files.

The amd64 binary also runs in a macOS 14.6.1 Recovery VM (build 23G93) with Apple Git 2.39.3 and the pinned scrump build. Native locks/liveness, the complete local-store/browser-API smoke, agent redaction and hook installation pass there. Both generated launchd agents register in Recovery's Aqua `gui/0` domain, run with synthetic inputs and finish with status 0. The fixture sets HOME and TMPDIR to writable test directories in the job env file.

This is a Recovery environment, not a normal logged-in desktop. It has no `open` command, so default-browser launching is unverified. The full OS installer failed during VM setup. A normal user desktop remains the remaining macOS validation step. Both macOS architectures are cross-built; only amd64 has native VM execution evidence. Windows native evidence is also amd64, and the other client architectures are build checks.

The manual/daily `portable client` workflow runs native Linux, macOS and Windows smoke checks and builds archives after the public repository's owner sets `CI_PUBLIC=1`. A skipped private-repository job is not a platform test result.

On Windows, `./tools/windows-jobs-smoke.ps1 -Binary ./tacit.exe` also registers both generated jobs with Task Scheduler in a temporary profile, then removes the tasks and profile. This check runs in the portable workflow. Job execution and default-browser launching are separate checks that need a logged-in desktop, as used in the VM above.

### Runner validation and limits

The adapters have subprocess tests for all seven protocols, large prompts, cancellation, malformed results and role selection. Installed Linux CLIs completed synthetic calls for Claude, Codex, Hermes, Muse and agy. Pi completed against a localhost synthetic provider; the configured local provider was unavailable. Grok's installed CLI reached a prompt with zero model tools, skills and agent instructions, but did not return a model response before the deadline. Its JSON parser is checked against the upstream result contract; successful provider execution remains unverified. These checks establish protocol compatibility, not equal extraction quality. The published quality measurements remain Claude measurements.

Model jobs use a private temporary directory and stdin or a private prompt file. File, shell and MCP tools are disabled for extraction and judging. Muse uses an empty toolset; agy uses a dedicated agent with an empty external-tool list (internal task bookkeeping can remain). Codex retains only provider/model settings in a disposable profile. Hermes safe mode retains provider settings while excluding plugins and MCP. Grok uses a restricted agent profile, removes its remaining file tool and denies MCP calls. It can still start the operator's configured hooks and MCP servers; those startup integrations are trusted local configuration, and may affect job startup.

Claude verification with API or third-party provider authentication retains its restricted Read/MCP verifier. Claude OAuth and other runners request evidence as JSON; Tacit executes each request through its guarded, redacted probes. No native tool access is needed for this path. Provider context windows still bound the amount of evidence a model can use.

Dollar totals contain only costs the CLI reports. Tacit prints a notice when cost is unavailable. The verifier's dollar threshold cannot enforce unreported spend; its deadline and turn bound still apply. Use provider-side spending controls when a hard monetary limit is required. CLI flags can change between releases; unsupported flags or incomplete output fail the job without advancing its transcript cursor.
