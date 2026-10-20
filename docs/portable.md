# Local and portable tacit

Choose a local store when your agents run on one machine. Choose the server when machines need to share facts. Both use the same Markdown facts, git history, retrieval and review methods.

## Local storage

`tacit init` creates a standalone repository at the configured store path. It refuses existing non-local repositories and non-empty directories, and is safe to rerun. Initialization is staged before the completed repository is moved into place.

The store records `tacit.local=true` in its local git configuration. This explicit marker selects local behavior; a failed server connection never changes the backend. Writes commit only their named files. Sync does no network work. `install follow` explains that a feed is unnecessary. `follow` returns its non-restarting configuration status (78, translated to 0 under launchd) so an existing service does not loop.

New local stores start with evidence-based ruling rules. An agent's `user_standard` label is not proof. The app records owner contributions as owner commits; imported quotes need backed evidence. Existing and server-created unaudited stores still need `tacit rulings audit` before relying on the new definition.

Local mode needs no enrollment keys. The OS account controls access. Agent notes still require scrump. Local commits reject the same obvious token shapes and private-key headers as the server's pre-receive check. A refused untracked inbox note is quarantined as `.rejected`; tracked files are restored and cached indexes invalidated. Rejected files are not facts and are excluded from verifier probes. This does not provide the server's authentication or multi-machine inbox boundary. Keep the store in your own account's private directory.

To use a server later, choose a separate store path and use `tacit clone` with your own server and enrolled keys. Back up or keep the local repository before choosing another store. `init` does not convert a clone.

## Configuration

Operator settings live in an ignored `config/local.yaml` or the normal user-config location. [The README](../README.md#configuration) lists every client option and environment equivalent. The optional file lives at `~/.config/tacit/config.yaml` on Linux/macOS, or `%APPDATA%\tacit\config.yaml` on Windows:

```yaml
store: ~/my-facts
projects: ~/projects
index: ~/.local/state/tacit/index
keys: ~/.config/tacit
local_overlay: ~/.local/share/tacit/local
local_layer: off
owner_names: [dana]
verify_roots: []
# Model routes belong in your private config; see config/example.yaml.
```

Use your own values. `~` is the current user's home. Relative paths resolve against the file's directory, so hook working directories cannot change the store. Windows paths can be single-quoted strings, such as `store: 'C:\Users\Dana\My Facts'`, or use forward slashes.

Precedence is file defaults, then `TACIT_*` environment overrides, then command flags. `tacit --config FILE init` or `TACIT_SETTINGS` selects another file. Unknown fields and a missing explicitly selected file are errors. A hook with bad configuration fails open without output. Keep provider credentials in the provider's own configuration or the job env file, not this settings file.

`init --store DIR` overrides that invocation only. Put a custom path in the file or environment for subsequent commands. The file loads before model defaults are selected. A job's explicitly selected env file loads at runtime as well. Installers carry an explicit config selection into their generated jobs and hooks.

### Model runners

Claude is optional. Supported runner names are `claude`, `codex`, `hermes`, `grok`, `pi`, `muse` and `agy`. Install and log in to the CLI you select. Tacit uses that CLI's provider configuration; it holds no provider credentials.

[config/example.yaml](../config/example.yaml) provides generic Claude aliases. Users select their own runners, model versions, effort, profiles and fallback routes in the ignored local file. Each fallback is attempted once and does not start another fallback.

A Codex profile is `~/.codex/<name>.config.toml` (or under `CODEX_HOME`). Select it through `profiles`, or use the CLI's provider configuration. Provider credentials stay in Codex's own auth store or environment.

Tacit reads provider/model settings from that profile and supplies explicit Codex configuration overrides. `--ignore-user-config` also ignores named profiles, so passing only `--profile` would silently lose the Bedrock route. Auth remains in Codex's own store or provider environment. Embedded provider credentials and literal provider headers are refused; use `env_key` or `env_http_headers` references instead.

For another runner, select it explicitly and let the CLI choose its model, or set a model ID it accepts:

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

`runners` accepts `dream`, `curate`, `judge` and `verify`. An omitted role inherits `runner`. `tacit --runner muse dream` overrides all role selections for that invocation. `models` accepts the same roles plus `verify_fallback`; those values go to the selected CLI's `--model`. An explicit runner selection disables the shipped fallback for that role. Configure your desired fallback separately. `models.verify_fallback` remains a legacy same-runner override.

`efforts` sets per-role reasoning effort. `profiles` selects per-role Codex profiles. These override general runner arguments without editing the CLI's shared profile. `fallbacks` accepts a route with `runner`, `model`, `effort` and `profile`; `null` disables it:

```yaml
fallbacks:
  dream:
    runner: claude
    model: haiku
  verify: null
```

Fallbacks run for provider capacity, temporary service failures, expired authentication, missing runners or an attempt deadline. Invalid model output, provider refusals, bad model IDs and access-denied responses stop the job. Model attempts reuse the verifier's existing 20-minute bound; `timeouts: {dream: 10m}` changes a role's bound. The caller's overall deadline still applies. Explicit empty `efforts` or `profiles` values clear the role default.

`commands` overrides executable paths, including Windows paths with spaces. `runner_args` adds an argument array without a shell, for provider or profile options. These are trusted operator settings: avoid options that replace Tacit's tool restrictions, output format or prompt source. Command flags take precedence over the job env file too.

The integrations and model runners are separate. You can use Muse for memory and Codex for dreaming. `tacit install muse` prints a `mcpServers` entry for Muse's existing settings (preserve its `schema_version` and other keys). `tacit install agy` prints an `agy mcp add` command. Both expose `tacit_search`, `tacit_fact` and `tacit_add`; shared stores also need their writer keys enrolled.

Transcript mining and automatic owner-quote lookup currently read Claude Code and Codex histories. Muse, agy, Grok, pi and Hermes can read and write memory through their integrations; their transcripts are not yet mined. Owner contributions and decisions in the app remain available without Claude. Curation refuses an empty owner-evidence corpus rather than guessing whether an agent paraphrase is an owner ruling.

### Optional utility selector

A model judges which candidate facts help each prompt. It sends redacted prompts and candidate summaries to the provider you choose; no fact bodies are sent. Recommendations, every provider's setup and the benchmark are in [Choosing what each prompt sees](selectors.md). Keep keys in the environment or a private dotenv file, not in YAML:

```yaml
utility:
  provider: jev          # jev | systemone | bedrock | openai
  threshold: 0.70        # required for every provider except jev
  timeout: 2s
  key_file: ./typesafe.env
  audit_dir: ~/.local/state/tacit/utility
  # model, endpoint, region: for systemone, bedrock and openai
```

`jev` is pinned to `jev-1.13.0` at the official TypeSafe origin and reads `TYPESAFE_API_KEY`. `systemone` posts the same request to `endpoint` (any System One-compatible server). `bedrock` calls Converse for `model` in `region` (default `us-east-1`) and reads `AWS_BEARER_TOKEN_BEDROCK` (a Bedrock API key). `openai` posts to `endpoint` + `/chat/completions` with `model`. `systemone` and `openai` read `TACIT_UTILITY_API_KEY`, which is optional for a local server. Only the provider's own variable is read from a regular `key_file`; the file is not executed, and an inherited variable wins. A key is sent only as a bearer token to its provider's endpoint, so a TypeSafe or AWS key never reaches a custom endpoint. Endpoints must use HTTPS unless they are on `localhost`, `127.0.0.1` or `::1`. The timeout covers key reading, redaction and the request, defaults to 2s, and cannot exceed 2s. Cancellation allows up to 250 ms of subprocess cleanup; optional audit adds at most 100 ms of wait. Missing credentials, invalid settings, unavailable redaction, timeout or invalid responses use the original local selector. Clear `provider` to return to local-only selection.

The audit directory is optional. It contains private daily JSONL files with redacted task and summary text, scores, selected flags and fallback reasons. Keep it out of the store and source control. On Unix, the directory must be owned by the caller and private (0700); files use mode 0600, a best-effort 10 MiB daily cap, and eight-day retention. Symlinks, hard links and special files are refused. Private audit is currently disabled on non-Unix systems rather than claiming Unix permissions enforce Windows ACLs; scoring still works. Audit failure or a stalled filesystem does not block a prompt. See [the measured comparison](../eval/JEV.md) for limits and the threshold tradeoff.

### Environment variables

| variable | default | what |
|---|---|---|
| `TACIT_UTILITY_PROVIDER` | empty (local only) | `jev`, `systemone`, `bedrock` or `openai` enables utility selection |
| `TACIT_UTILITY_MODEL` | empty | model for `systemone` (optional), `bedrock` and `openai` |
| `TACIT_UTILITY_ENDPOINT` | empty | `systemone` POST URL or `openai` base URL; HTTPS unless loopback |
| `TACIT_UTILITY_REGION` | `us-east-1` | Bedrock region for `bedrock` |
| `TACIT_UTILITY_THRESHOLD` | `0.70` for `jev`; required otherwise | score cutoff in [0,1] |
| `TACIT_UTILITY_TIMEOUT` | `2s` | redaction + request deadline, positive and at most `2s` |
| `TACIT_UTILITY_KEY_FILE` | empty | dotenv file; only the provider's key variable is read |
| `TACIT_UTILITY_AUDIT_DIR` | empty (no audit) | private daily selection receipts |
| `TYPESAFE_API_KEY` | empty | `jev` credential, sent only to the official TypeSafe origin |
| `AWS_BEARER_TOKEN_BEDROCK` | empty | `bedrock` credential, sent only to `bedrock-runtime.<region>.amazonaws.com` |
| `TACIT_UTILITY_API_KEY` | empty | `systemone`/`openai` credential, sent only to the configured endpoint |
| `TACIT_SETTINGS` | platform config file above | optional client YAML settings |
| `TACIT_STORE` | `~/.local/share/tacit/store` | the store clone |
| `TACIT_CONFIG` | `~/.config/tacit` | keys |
| `TACIT_INDEX` | `$TACIT_STATE_DIR/index` | FTS5 index files |
| `TACIT_STATE_DIR` | `~/.local/state/tacit` | per-machine state: dreamer and curator cursors, stale and verify state, session marks, logs; a second store on one machine (a shadow rebuild) takes its own without changing `HOME` |
| `TACIT_LOCAL` | `~/.local/share/tacit/local` | local overlay of a read-only clone |
| `TACIT_LOCAL_LAYER` | `off` | `on`: a read-only clone dreams and curates its own sessions into the local overlay (SPEC.md "Local layer") |
| `TACIT_FOLLOW_STATE` | `~/.local/state/tacit/follow.json` | follower heartbeat |
| `TACIT_PROJECTS` | `~/projects` | where repo checkouts live: derive, results, stale, the timeline, the verifier, the dreamer and import |
| `TACIT_DERIVE_CONFIG` | `~/.config/tacit/derive.yaml` | `branches:` the branch to read per repo (`tacit derive --set-branch repo=branch`); `ordinary_names:` repo names that are also plain words, so a bare mention is not evidence a fact is about that repo |
| `TACIT_WRITER` | | the key a push signs with |
| `TACIT_OWNER_NAMES` | | names agents use for the owner in facts ("Dana 2026-09-05: ..."), comma separated; each counts like "owner" when the rulings audit reads a clause attributed without quote marks |
| `TACIT_HERMES_DB` | `~/.hermes/state.db` when it exists | the Hermes session store the dreamer and the owner's words read, opened read-only; `off` reads none |
| `PI_CODING_AGENT_SESSION_DIR`, `PI_CODING_AGENT_DIR` | `~/.pi/agent` | pi's own variables: where the dreamer and the owner's words find pi sessions, read as pi reads them |
| `TACIT_RUNNER` | `claude` | select one CLI for all roles |
| `TACIT_DREAM_RUNNER`, `TACIT_CURATE_RUNNER`, `TACIT_JUDGE_RUNNER`, `TACIT_VERIFY_RUNNER` | global runner | per-job CLI |
| `TACIT_<TOOL>_COMMAND` | tool name | executable path, e.g. `TACIT_MUSE_COMMAND` |
| `TACIT_<TOOL>_ARGS` | | JSON argv array, e.g. `["--profile","work"]` |
| `TACIT_CURATE_MODEL` | `opus` | curator split |
| `TACIT_JUDGE_MODEL` | `haiku` | contradiction judge |
| `TACIT_DREAM_MODEL` | `opus` | dreamer |
| `TACIT_VERIFY_ROOTS` | | extra read-only source directories, separated like `PATH`; each must exist. Claude auto-memory is included when present |
| `TACIT_VERIFY_MODEL` | `opus` | verifier |
| `TACIT_VERIFY_FALLBACK_MODEL` | | legacy same-runner fallback model; empty disables it |
| `TACIT_<ROLE>_EFFORT`, `TACIT_<ROLE>_PROFILE` | CLI default | reasoning effort and Codex profile |
| `TACIT_<ROLE>_FALLBACK` | CLI default | JSON route or `null`; explicit route wins over the legacy model override |
| `TACIT_<ROLE>_TIMEOUT` | `20m` | per-attempt deadline |
| `TACIT_SERVER`, `TACIT_ENROLL_SSH` | | `scripts/mac-setup.sh`: server URL, and the ssh target that runs the enroll command |

The defaults in this table apply when no runner override is supplied. An explicit runner uses its own model unless configured, except that Claude retains its legacy aliases. Model values are anything that runner's `--model` takes. A `--model` flag wins over the variable. Server variables (`TACIT_DATA`, `TACIT_BIND`, `TACIT_PORT`, `TACIT_PUBLIC_PORT`, ...) are listed in `server/lib/tacit_server/config.ex`.


## Source builds

### Linux and macOS

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

Install writes files and prints start commands; it starts nothing. Linux gets user services/timers. macOS gets launchd agents. Dreaming runs every four hours; curation and verification run daily. `install dream` also writes the idle job, every 10 minutes: `tacit dream --idle 20m` (the sessions quiet for 20 minutes or just compacted), then `tacit curate --quick --min-interval 30m` (the last day's notes, at most every 30 minutes; it does nothing on a machine without the curator key). `tacit maintain --env-file FILE` runs the daily sequence manually on any OS. Local jobs need no enrollment. Server-backed jobs retain their key requirements. Create the env file and configure the provider and PATH before starting a model job.

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

`install dream` (two tasks, the dreamer and the idle job), `install curate` and server-mode `install follow` write Task Scheduler XML under `~\.config\tacit\tasks`. Run the printed PowerShell registration/start commands under the current Windows account. No administrator account or stored password is requested. Tasks run while that account is logged in and suppress overlapping instances. `install app` writes a PowerShell browser launcher.

Real task registration and default-browser launching require Windows. Generating XML or running under Wine does not verify those Windows services. Both are checked in the native Windows VM described below. Env files may use UTF-8 with or without the BOM written by Windows PowerShell.

## Redaction

Tacit release archives include scrump with its required `text` profile, built from pinned source [17cd5ab](https://github.com/avifenesh/scrump/commit/17cd5abc9ce19d4fdec51588a35ef73a644e36bb). The installers check a masking canary before installation. A compatible redactor is required for agent writes and model digests.

If building Tacit from source, build the same scrump source:

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

This builds Linux, macOS and Windows archives, each on amd64/arm64, plus `SHA256SUMS`. Windows gets ZIP; Linux/macOS get tar.gz. License, notice and bundled-font license texts are included. This development command builds the CLI only. Published releases also bundle native scrump and its license. Git and the model CLI remain external runtime dependencies. Archives contain the CLI/browser app, not native desktop-window builds.

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

The adapters have subprocess tests for all seven protocols, large prompts, cancellation, malformed results and role selection. Installed Linux CLIs completed synthetic calls for Claude, Codex, Hermes, Muse and agy. Pi completed against a localhost synthetic provider; the configured local provider was unavailable. Grok completed live extraction and conflict-judging calls with a fresh runtime home per request. A scheduled-job environment also completed transcript extraction and pushed its notes. These checks establish protocol compatibility, not equal extraction quality. The published quality measurements remain Claude measurements.

Model jobs use a private temporary directory and stdin or a private prompt file. File, shell and MCP tools are disabled for extraction and judging. Muse uses an empty toolset; agy uses a dedicated agent with an empty external-tool list (internal task bookkeeping can remain). Codex reconstructs only provider/model settings as explicit configuration overrides while ignoring hooks and MCP settings. Hermes safe mode retains provider settings while excluding plugins and MCP. Grok uses a fresh runtime home and a restricted agent profile, removes its remaining file tool and denies MCP calls. User hooks and MCP startup integrations are excluded; managed policy hooks remain.

Claude verification with API or third-party provider authentication retains its restricted Read/MCP verifier. Claude OAuth and other runners request evidence as JSON; Tacit executes each request through its guarded, redacted probes. No native tool access is needed for this path. Provider context windows still bound the amount of evidence a model can use.

Dollar totals contain only costs the CLI reports. Tacit prints a notice when cost is unavailable. Local accounting logs retain the actual primary and fallback routes, usage from failed calls, and `null` for unreported cost. Summaries show reported dollars and count calls with unknown cost. The verifier's dollar threshold cannot enforce unreported spend; its deadline and turn bound still apply. Use provider-side spending controls when a hard monetary limit is required. CLI flags can change between releases; unsupported flags or incomplete output fail the job without advancing its transcript cursor.

### Grok model jobs

Each request uses a fresh `GROK_HOME` inside the private job directory. This avoids stale native agent/session state blocking independent headless jobs. Tacit retains provider/model settings and references the original native sign-in and requirements files. It excludes user hooks, plugins, memory and MCP servers, while retaining managed defaults and mandatory policy hooks. Embedded provider keys and literal request headers are refused; use native auth, `env_key` or `env_http_headers`.

The job directory is removed after the process exits. Symlinks keep native files in place; Windows falls back to hard links when symlinks are denied, which requires the source and job cache to share a filesystem.

### Dreamer recovery

Each successful transcript window writes complete notes, commits them and pushes them before the next model call. Its cursor advances only after every window for that transcript succeeds. Completed chunk hashes let capped runs resume at the next window; changed chunks are processed again. A failed model call, write, commit or push leaves that transcript available for retry.

At startup, the dreamer retries retained commits and validates untracked notes in its own host inbox before pushing them. It preserves existing note bytes and leaves quarantined `.md.rejected` files alone. `tacit dream --max-calls 0` runs this recovery without new model calls. Note publication is atomic, and named commits and their rollback preserve unrelated staged changes.

## Published releases

[Releases](https://github.com/agent-sh/tacit/releases) provide Linux, macOS and Windows builds on amd64 and arm64. Linux's bundled redactor is built against musl for a static executable. The browser app needs no native desktop SDK. Each archive includes `tacit`, `scrump`, their license files and the README. `SHA256SUMS` covers all six archives.

Download `install.sh` or `install.ps1` from the release. Unix defaults to `~/.local/bin`; Windows defaults to `%LOCALAPPDATA%\tacit\bin`. Both accept a version and destination, check the downloaded archive and its reported version, and verify scrump's text profile before replacing files. Installers do not change model configuration, stores, credentials or PATH. `tacit --version` prints the installed release; source builds print `tacit dev`.

Changes to the release workflow, archive builder or installer scripts run native package checks on the PR head with a preview version label. A `v*` tag runs the release workflow. All six native package/install checks must pass before publication. Manual workflow runs validate the selected git ref with an explicit version label and upload artifacts; they do not publish or create a tag. The primary commit is retained in Go build metadata, and scrump is always checked out at the pinned source revision.

### Runtime pauses

`stale_workers` in YAML or `TACIT_STALE_WORKERS` sets how many facts the curator's stale step checks at once (default 4). Each runs its own git commands against the checkouts, tens of thousands per run on a large store, so a machine that does other work sets 1.

`dream_max_runtime` in YAML or `TACIT_DREAM_MAX_RUNTIME` controls the extraction/persistence phase of a batch. Its existing default is `45m`. `tacit dream --max-runtime 1h` overrides it for a run; `0` disables the batch limit while per-attempt deadlines still apply. Preparation and pending-note recovery retain their separate bounds.

Reaching the batch runtime or call limit is a pause with exit 0. Successful notes are already pushed and cursor/chunk progress is saved. The interrupted window remains pending. A prior genuine failure still makes that run exit 1; a model-attempt timeout on its own remains a failure. Accounting includes `pause_reason` (`runtime` or `call_limit`) and the number of `remaining_windows`.

### Idle dreaming and quick curation

`tacit dream --idle D` (`--idle` alone: 20m) dreams only the transcripts with new content that had no write for D, or whose new content holds a compaction (taken at once). `tacit dream --session T` dreams one transcript (a path, or a Hermes session id) from its saved cursor; the prompt hook starts it once the undreamed part reaches about one dream window, and the PreCompact hook before a compaction. A transcript whose window failed waits 4 hours for these runs; the 4-hour run retries it. One dream runs at a time (`dreamer.lock` in the state directory): an idle or session dream that finds one running exits 0 with "busy", the 4-hour dream waits up to 45 minutes. `tacit curate --quick [--min-interval D]` curates the notes added in the last 24 hours (at most `--max-batches 2` in `--budget 20m` unless set), with no derive, results, links or staleness step; it shares `curator.lock` with the full run (a quick run that finds one running exits 0; a full one waits, and its deadline starts once it holds the lock) and does nothing within `--min-interval` (default 30m, kept in `curate-quick.json`) of the last quick run. `tacit curate --env-file F` loads provider variables like `tacit dream --env-file F`. Rules: SPEC.md "When a transcript is dreamed" and "Curator runs".
