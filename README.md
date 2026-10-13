# tacit

**Memory for coding agents, with the evidence to keep it honest.**

[![CI](https://github.com/agent-sh/tacit/actions/workflows/ci.yml/badge.svg)](https://github.com/agent-sh/tacit/actions/workflows/ci.yml)
[![agnix](https://github.com/agent-sh/tacit/actions/workflows/agnix.yml/badge.svg)](https://github.com/agent-sh/tacit/actions/workflows/agnix.yml)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue.svg)](LICENSE)

A decision made in one session should survive into the next. Tacit keeps project decisions, past mistakes and machine facts in a git repository, then gives your agents a small set of facts matched to the repo and prompt.

Run a local store on your laptop, or share one across machines. Works with **Claude Code, Codex, Hermes, Grok and pi** on **Linux, macOS and Windows**.

- **Files you own.** One fact per Markdown file, with its source, scope and git history.
- **Context that fits.** Local full-text search and small context blocks. Hooks avoid repeating facts within a session.
- **Decisions with proof.** Your own words and recorded review decisions establish your rulings. Agents leave changes to those rulings for you.

## Quick start

Install git 2.29+, Go 1.27 and [scrump with its text profile](docs/portable.md#redaction). Scrump is required for agent writes. Model jobs also need the `claude` CLI and your provider configuration; search, owner edits and review work without a model account.

```sh
git clone https://github.com/agent-sh/tacit.git
cd tacit
```

### Linux or macOS

```sh
mkdir -p ~/.local/bin
(cd client && go build -o ~/.local/bin/tacit ./cmd/tacit)
export PATH="$HOME/.local/bin:$PATH"
tacit init
tacit install claude
tacit app --browser
```

### Windows

```powershell
Push-Location client
go build -o "$HOME\tacit.exe" ./cmd/tacit
Pop-Location
& "$HOME\tacit.exe" init
& "$HOME\tacit.exe" install claude
& "$HOME\tacit.exe" app --browser
```

`init` creates a local store. It needs no server, tailnet or enrollment keys. Open the app to add your own rulings, inspect facts and review changes.

Choose the integration for your agent:

| Agent | Install | Connection |
|---|---|---|
| Claude Code | `tacit install claude` | Session-start and prompt hooks |
| Codex | `tacit install codex` | Hooks; trust them once in `/hooks` |
| Hermes | `tacit install hermes` | Memory provider; follow the printed configuration steps |
| Grok / pi | `tacit install grok` / `tacit install pi` | Prints the MCP configuration |

[Portable setup](docs/portable.md) covers dependencies, native jobs, build archives and platform validation. To exchange facts between machines, follow the [shared-store setup](docs/setup.md#install-the-shared-store).

## How it works

1. **Collect.** Agents write short inbox notes. Imports bring in existing memory files. An optional dreamer extracts candidates from redacted transcript deltas.
2. **Curate.** Notes become individual facts with sources and scopes. Contradictions and uncertain changes become review items.
3. **Deliver.** A local SQLite FTS5 index matches facts to the repo, host and prompt. Hooks supply up to 10 facts at session start and 3 curated facts per prompt, with a separate match for a computed fact. Explicit search can return the full text.
4. **Check.** A verifier reads code, git, docs and permitted machine sources. It records evidence for its decisions and leaves unresolved questions for you.

Git also supplies hot spots, module maps, committed measurements and deletion-based staleness checks. Those steps need no model. The app brings facts, sources, history and review decisions together.

New local stores require evidence for owner rulings. Existing and server-created stores switch to that definition with the [ruling audit](docs/setup.md#initialize-ruling-proof).

The [methods guide](docs/methods.md) explains the ranker, curation, ruling proof and verification. The [store contract](SPEC.md) defines the file format and write rules.

### Architecture

The Go client holds the git store and search index locally. Hooks and MCP read from that local copy. Model jobs run through `claude -p` on the machine you configure for them.

For shared stores, a small Elixir server adds git transport, signed writer identities and a change feed. Writers add notes to their own inboxes; the curator writes the rest of the store. Clients fetch changes and rebuild their local indexes. The server holds no model credentials.

## Configuration

Put optional settings in `~/.config/tacit/config.yaml` on Linux/macOS or `%APPDATA%\tacit\config.yaml` on Windows:

```yaml
store: ~/my-facts
projects: ~/projects
owner_names: [dana]
```

Use your own paths and names. Relative paths resolve against the settings file. `tacit --config FILE` selects another file; environment variables override file values, and command flags take precedence.

[Configuration reference](docs/portable.md#configuration) includes model settings, all environment variables and Windows path examples.

For optional background work, `tacit install dream` and `tacit install curate` write native job files and print the commands to start them. Configure the provider and job environment before starting them. See [job setup](docs/portable.md#linux-and-macos).

## What we measured

The initial evaluation uses one owner's private store and Claude Code/Codex sessions. Results cover that workload; the original data is private. The scripts and methods are available to run on your own data.

Those measurements shaped the client: lexical retrieval beat the dense variants tested, splitting notes into individual facts improved delivery, and larger context blocks cost more without helping. The verifier remains limited: in the held-out sample it agreed with the owner on 3 of 13 findings and missed 8 of 10 retirements. Human review remains part of the method.

[Full measurements](eval/VALUE.md) · [Grep baseline](eval/BASELINE.md) · [Run the synthetic sample](eval/README.md#run-the-sample)

The synthetic sample runs without a server or model account. It demonstrates the formats and scoring. Long-session outcomes and delivery in other harnesses have not been measured.

## Security model

Agent writes and model digests pass through scrump before storage or a model call. A failed redaction stops the write. Local commits and shared pushes also check for known token shapes.

Local mode uses your OS account for access. Shared mode adds tailnet identity, signed keys and per-writer inbox enforcement. An optional public listener accepts signed readers only.

Model jobs send redacted digests and fact excerpts to the provider configured in Claude CLI. The verifier can read permitted source files. Choose the provider with that access in mind.

[Security policy and private reporting](SECURITY.md) · [Shared-store details](server/README.md)

## Documentation

| Guide | Covers |
|---|---|
| [Local and portable setup](docs/portable.md) | Configuration, dependencies, native jobs and validation |
| [Shared-store setup](docs/setup.md) | Server deployment, keys, machine roles and job configuration |
| [Client reference](docs/client.md) | Commands, flags, hooks and the app |
| [Methods](docs/methods.md) | How facts are collected, delivered and checked |
| [Evaluation](eval/README.md) | Scripts, samples and how to measure another store |
| [Contributing](CONTRIBUTING.md) | Development, tests and pull requests |

Agent configurations are checked in CI with [agnix](https://github.com/agent-sh/agnix), including the hooks and MCP settings generated by the client.

## License

[Apache 2.0](LICENSE). Bundled fonts use the SIL Open Font License 1.1; see [NOTICE](NOTICE).
