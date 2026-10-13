# tacit

Fact memory for coding agents. Run one local store or share a store across machines: Claude Code, Codex, Hermes, Grok, pi.

Agents forget. Each session starts cold, rediscovers the same broken PATH, repeats the mistake the last session already fixed, and contradicts what another agent on another machine wrote yesterday. Each harness keeps its own memory files, on its own machine, in its own format. Nobody checks whether those files are still true.

tacit puts the facts in a git repository on your machine, or behind a small server when machines need to share it. It delivers the few that matter at the moment an agent needs them and checks them against their sources: code, git history and the owner's own words.

Facts, not code. User standards, design decisions, past mistakes, machine facts, hot spots and structure derived from git, measured results with the commit that produced them.

## What makes it different

- **Delivery is measured, not assumed.** Every retrieval choice in the client has a number behind it, including the ones we dropped. See [What we measured](#what-we-measured).
- **A verifier grounded in sources.** Contradictions, supersessions and stale facts are not settled by a model's opinion of two sentences. `tacit verify` gives a model read-only tools over the repos, git, docs and the machine, and it marks each finding with its evidence or leaves the owner one question.
- **Your rulings are your verified words.** New local stores start with evidence-based rulings. Existing and server-created stores switch with `tacit rulings audit` on the curator's machine. Whoever runs tacit is its owner. Then a fact counts as a ruling only with proof: a qualifying quote found in one of your own turns and recorded in a ledger, a decision you made on the review page, or a fact you wrote in the app. Unaudited stores use the legacy type-based definition. Agents never retire a ruling; a conflict with one becomes a question for you.
- **Git is a source of facts.** Hot spots and co-change partners, a module map, measured results read from committed result files, and a staleness check that flags facts whose subject git has since deleted. No model in any of these.
- **Plain files.** One fact per Markdown file with YAML frontmatter, linted, in a git repo you own. A local store commits on your machine. A shared store uses the server as authority and transport. Both keep a full-text index locally.

## How it works

1. **Collect.** Agents add short notes to their own inbox. The dreamer extracts candidate facts from redacted transcript deltas. Existing memory files can be imported.
2. **Curate.** The curator splits notes into individual facts, records their sources, checks for contradictions and writes the shared index. An agent's account of a decision remains an observation until the owner's words prove it.
3. **Compute.** Git supplies hot spots, co-change partners, module maps and results with the commit that produced them. These are generated separately from prose observations.
4. **Deliver.** A local lexical index filters by repo and host. Hooks deliver a small start block and strong prompt matches, each fact once per session. Explicit search can return the full fact.
5. **Check.** The verifier checks open findings against read-only sources. Evidence settles clear cases; the owner decides questions that the sources cannot settle and changes to their rulings.

[Methods and evaluation](docs/methods.md) explains the algorithms, proof rules, measurements and how to repeat them. [Setup for your own store](docs/setup.md) covers the roles, configuration and checks for a new installation. You can run the [synthetic evaluation sample](eval/README.md#run-the-sample) without a server or model account.

## What we measured

All numbers come from one owner's live store and that owner's own sessions (Claude Code and Codex on one workstation, Opus 5.5 as the working model). That data is private and not in this repo, so the method is reproducible and the data is not. Method, scripts and every table: [eval/VALUE.md](eval/VALUE.md). Grep baseline: [eval/BASELINE.md](eval/BASELINE.md).

| question | result | limit |
|---|---|---|
| Is there a gap? | 202 situations mined from 1,993 session digests where a stored fact would have cut a detour. 40 percent of those facts were never written anywhere. Grep over the memory files that did exist put the right file in the top 5 for 36 to 49 percent. | One owner, ops-heavy work. |
| Does a delivered fact help? | 14 decision tasks built from owner rulings, 3 reps, 168 judged cells: score 0.64 with no facts, 0.73 with the fact guaranteed, 0.69 with the shipped hooks. Tool calls 134 bare, 123 shipped. On 11 read tasks the guaranteed fact saved 18 percent of tool calls. | Single-prompt tasks. Long sessions and other harnesses not measured. |
| How big should the block be? | A 40-line start block cost 18 percent more and helped nothing. Up to 10 facts at start, up to 3 curated facts per prompt above a score floor, each once per session. A prompt can also add one computed fact. Start block is about 450 tokens. | |
| Dense retrieval? | Below lexical. EmbeddingGemma 2 hit@40 0.73 against the lexical ranker's 0.81 on the curated store; fusion added nothing there. Hindsight 0.10.3 (semantic + BM25 + rerank) 0.68 against 0.81. No embeddings in the client. | Hindsight ran with no LLM extraction and no tuning. |
| One fact per entry? | The curator split lifts offline delivery 6 to 7 points at every block size. | |
| Can a cheap model mine what nobody wrote? | Haiku 5.5 recovers 75 percent of the never-written facts at least partially, about $1.30 for 80 session windows. Opus recovers 3 more at 26 times the cost. | |
| Does the verifier agree with the owner? | Tuning sample: 20 of 27 (Opus 5.5), 18 (Sonnet 5.5). Held-out sample: 3 of 13 for both, missing 8 of the 10 retirements the owner made. It rarely retires a fact the owner kept: 1 or 2 of 27, 0 of 13. Opus does not move the ceiling and costs 6 to 10 times more. | Small samples. The verifier filters clear cases; the owner's queue still holds the hard ones. |
| Does a git timeline help the verifier? | No: median 19 of 27 with and without it over 10 paired runs. It stays on the review page, for the owner. | |
| Staleness from git? | Flagging a changed line that names something the fact names was right 0 of 16 times. Flagging deletions only: 21 flags on 4,688 facts, 18 of them really stale by hand. | Value changes whose name stands are not flagged. |
| Rulings that were really the owner's? | Of 802 facts the old definition treated as owner rulings, 77 had proof the owner said or kept them. 698 had no words of the owner's at all. | Recall bounded by the transcripts that survive. |

Two more measured and not shipped: a second verifier prompt (ties on one sample, more wrong retirements on the other) and two levers to cut the owner's review queue (neither kept recall).

## Architecture

Local mode uses the same fact format, index, curator, verifier and review UI. `tacit init` creates a standalone git store; writes commit locally and sync does no network work. No server, Tailscale, enrollment keys or follower is needed. The OS account controls access. In shared mode the server adds machine identity, signed keys and push-scope enforcement:

```
  workstation / laptop / GPU box (any number)          server host (always on)
 +--------------------------------------------+       +-------------------------------+
 |  Claude Code  Codex  Hermes  Grok  pi      |       |  tacit server (Elixir)        |
 |      |  hooks / MCP / memory provider      |       |                               |
 |      v                                     |  git  |  bare store repo  <-- pushes  |
 |  tacit (Go, one binary)                    | smart |  pre-receive: scope, lint,    |
 |   - start block, prompt facts, search      | HTTP  |    secrets, signed key        |
 |   - add -> inbox/<harness>@<host>/  -------+------>|  journal (seq per push)       |
 |   - clone + FTS5 index (local)  <----------+-------|  /events change feed (SSE)    |
 |   - follow: holds the feed, syncs  <-------+-------|                               |
 |   - dream: transcripts -> inbox (Haiku)    |       |  tailnet listener (writers,   |
 |                                            |       |    readers)                   |
 |  curator's machine only:                   |       |  public listener, optional:   |
 |   - curate: inbox -> one-fact entries      |       |    signed readers only,       |
 |   - derive, results, stale, links (git)    |       |    via Tailscale Funnel       |
 |   - verify: findings vs sources            |       +-------------------------------+
 |   - app / review page: what needs you      |
 +--------------------------------------------+
```

- Agents write only under their own `inbox/<harness>@<host>/`. Everything else is curated and written only by the curator key. The server enforces it at pre-receive.
- The journal is the order of the store; git is the content. The feed carries sequence numbers only and each client fetches. The journal rebuilds itself from the repo.
- The index is client side: SQLite FTS5 keyed by HEAD, bm25 with name and description weighted over body.
- The server holds no model credentials. Model work runs on the curator's machine through `claude -p`.

Store contract: [SPEC.md](SPEC.md). Design history and the dated decisions: [docs/PLAN.md](docs/PLAN.md). Server: [server/README.md](server/README.md).

## Quick start

The CLI and browser app build for Linux, macOS and Windows, on amd64 and arm64. You need git 2.29 or newer, Go 1.27 to build, and [scrump with its text profile](docs/portable.md#redaction) for agent writes. Model jobs also need the `claude` CLI and your own provider configuration. Models are optional for owner rulings, search and review.

Clone the source:

```
git clone https://github.com/agent-sh/tacit.git
cd tacit
```

### Local store

Linux or macOS:

```sh
mkdir -p ~/.local/bin
(cd client && go build -o ~/.local/bin/tacit ./cmd/tacit)
export PATH="$HOME/.local/bin:$PATH"
tacit init
tacit install claude             # or codex, hermes, grok, pi
tacit app --browser
```

Windows, from PowerShell:

```powershell
Push-Location client
go build -o "$HOME\tacit.exe" ./cmd/tacit
Pop-Location
& "$HOME\tacit.exe" init
& "$HOME\tacit.exe" install claude
& "$HOME\tacit.exe" app --browser
```

Set paths, owner names and models in the [configuration file](docs/portable.md#configuration). `init` refuses existing repositories or non-empty directories. Optional model jobs use `tacit install dream` and `tacit install curate`, which print native start commands. [Portable setup](docs/portable.md) covers builds, dependencies, launchd, Windows Task Scheduler and validation limits.

### Shared store

The following setup is for machines exchanging facts. It requires a Linux server with Docker and Tailscale plus SSH access for deployment. Join server and writer machines to your tailnet. Local-only installations can skip this section.

### 1. Server

```
server/deploy/deploy.sh root@<server-host>
```

Builds the release in Docker on the host, installs `tacit-server.service` (sandboxed, unprivileged), creates the bare store on first start, and checks `/healthz` over the tailnet IP. It binds the node's tailnet IPv4 on port 7447. Every request needs a tailnet identity; writes also need a signed key token.

Optional public path for machines that cannot join the tailnet (readers only, every request signed, no push for any key):

```
server/deploy/funnel.sh root@<server-host> on|off|status
```

### 2. Client

```
mkdir -p ~/.local/bin
(cd client && go build -o ~/.local/bin/tacit ./cmd/tacit)
export PATH="$HOME/.local/bin:$PATH"
cargo install --locked --root ~/.local --git https://github.com/avifenesh/scrump \
  --rev 17cd5abc9ce19d4fdec51588a35ef73a644e36bb scrump-cli
```

The pinned scrump commit is the first with the text profile tacit uses. Every write through `tacit add`, the MCP tool and the dreamer is redacted by it first, and fails closed without it.

### 3. Keys and the clone

Each harness on each machine writes with its own Ed25519 key, `<harness>@<host>`.

```
tacit keygen claude                       # prints: ssh-ed25519 AAAA... claude@<host>
# on the server host:
/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll("claude@<host>", "ssh-ed25519 AAAA...")'
tacit authcheck claude --url http://<server-tailnet-ip>:7447    # exit 0 when the server takes the key
tacit clone http://<server-tailnet-ip>:7447/git claude
```

`tacit clone` sets up the credential helper. The curator's machine also needs `tacit keygen curator`, enrolled the same way. A machine off the tailnet clones read-only through the public path: `tacit keygen reader`, enroll it, then `tacit clone --read-only --as reader https://<funnel-host>/git`. On a read-only clone agents' writes stay in a local overlay on that machine and nothing is pushed.

### 4. Hooks

| harness | install | what it gets |
|---|---|---|
| Claude Code | `tacit install claude` | SessionStart (up to 10 facts for the repo and host, rulings first) and UserPromptSubmit (up to 3 facts above a score floor) |
| Codex | `tacit install codex` | the same two hooks in `~/.codex/hooks.json`; trust them once in `/hooks` |
| Hermes | `tacit install hermes`, then `memory.provider: tacit` | a memory provider: the start block, per-prompt facts, Hermes' own memory writes mirrored into the inbox |
| Grok, pi | `tacit install grok` / `tacit install pi` | prints the MCP snippet (`tacit mcp`: `tacit_search`, `tacit_fact`, `tacit_add`); Grok ignores hook output, so MCP is its path |

Hooks fail open: no store, no network, no output, exit 0.

### 5. Background jobs

The dreamer writes as `dreamer@<host>`. Before enabling it, run `tacit keygen dreamer`, enroll its printed public key on the server using the same RPC command as in step 3, and check `tacit authcheck dreamer --url <server-url>`. It needs its own key even when Claude Code already has one.

```
mkdir -p ~/.config/tacit
touch ~/.config/tacit/env
chmod 600 ~/.config/tacit/env
tacit install follow     # holds the change feed; syncs within a second or two of each push
tacit install dream      # every 4 h: new transcript turns -> facts in inbox/dreamer@<host>/
```

Install writes the job files and prints the commands to start them. Run those commands after configuring the env file. Linux gets systemd user units, macOS launchd agents. On Linux:

```
systemctl --user daemon-reload
systemctl --user enable --now tacit-follow.service tacit-dream.timer
```

On a headless Linux box also run `loginctl enable-linger $USER`. The dreamer reads `~/.config/tacit/env` (KEY=VALUE lines) for whatever `claude -p` needs on that machine. Model aliases work with the CLI's usual login; providers that require full model IDs use the model variables below. Jobs do not inherit your shell environment. The curator also needs an absolute `PATH` in that file that includes `claude`, `scrump` and `git`; see [job configuration](docs/setup.md#job-configuration).

Before scheduling curation, initialize ruling proof on the curator's machine. It needs the enrolled curator key and readable owner turns from Claude Code or Codex. Inspect the dry-run, then apply the audit:

```
tacit rulings audit --dry-run
tacit rulings audit
```

The applied audit writes `review/rulings-audit.json` and the proof ledger to the shared store. If no owner turns can be read, it fails without enabling the new ruling definition. See [setup](docs/setup.md#initialize-ruling-proof).

On the curator's machine, install the daily timer:

```
mkdir -p ~/.config/systemd/user
cp client/deploy/systemd/tacit-curate.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now tacit-curate.timer
```

That runs `tacit curate` (split, rulings check, contradiction check, then derive, results, links and stale) and `tacit verify`.

### 6. The app

What needs you, every fact with its history and what git says, edit, retire, and new rulings in your own words.

```
(cd client && go build -tags desktop,production,webkit2_41 -o ~/.local/bin/tacit-app ./cmd/tacit)   # Linux: libgtk-3-dev, libwebkit2gtk-4.1-dev
tacit-app install app
tacit app --browser       # the same page from any build, 127.0.0.1:7450 behind a token
```

On macOS build with `-tags desktop,production`. `scripts/mac-setup.sh --server <url>` does the whole Mac setup: checks, scrump, build, keys, enrollment wait, clone, hooks, follower, app ([docs/client.md](docs/client.md#mac)). The app writes only on a machine that holds the curator key; elsewhere it is view only.

Every command and flag: [docs/client.md](docs/client.md).

## Configuration

Optional YAML: `~/.config/tacit/config.yaml` on Linux/macOS, `%APPDATA%\tacit\config.yaml` on Windows. `--config FILE` or `TACIT_SETTINGS` selects another file. Relative paths resolve against it. Environment variables override file values; explicit command flags override defaults. [Configuration example](docs/portable.md#configuration).

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
| `TACIT_CURATE_MODEL` | `opus` | curator split |
| `TACIT_JUDGE_MODEL` | `haiku` | contradiction judge |
| `TACIT_DREAM_MODEL` | `haiku` | dreamer |
| `TACIT_VERIFY_ROOTS` | | extra read-only source directories, separated like `PATH`; each must exist. Claude auto-memory is included when present |
| `TACIT_VERIFY_MODEL` | `opus` | verifier |
| `TACIT_VERIFY_FALLBACK_MODEL` | `sonnet` | verifier when the first model has no capacity (429, 503, 529) |
| `TACIT_SERVER`, `TACIT_ENROLL_SSH` | | `scripts/mac-setup.sh`: server URL, and the ssh target that runs the enroll command |

Model values are anything `claude --model` takes. A `--model` flag wins over the variable. Server variables (`TACIT_DATA`, `TACIT_BIND`, `TACIT_PORT`, `TACIT_PUBLIC_PORT`, ...) are listed in `server/lib/tacit_server/config.ex`.

## Security model

- **Identity.** The tailnet listener takes only tailnet peers, identified by tailscaled's WhoIs. A write also needs a short-lived token signed by an enrolled Ed25519 key, and the key's host must be the peer's tailnet name. Replayed tokens are refused.
- **Scope.** A writer key pushes only into its own `inbox/<harness>@<host>/`, with no merge commits, symlinks or submodules. Only the curator key writes curated facts. The harness must be on the server's allow list.
- **Public path.** Off by default. A second listener on loopback, reached through Tailscale Funnel. A reader key token on every route, one 401 body for every failure, no push for any key, a per-source failure limit, and feed streams that end every 300 s so a revoked key stops receiving.
- **Secrets.** scrump redacts every agent write and every digest before it leaves the machine, and the write fails closed without it. The server's pre-receive scan is the second net: a push holding a token shape is refused.
- **Model access.** The server has none. The verifier's tools are read-only (`tacit probe`): git, grep, find, ls, `gh` view and list, unit status. Secret directories are excluded.
- **Storage.** A local store stays on your machine. A shared store uses your private server and client clones. Local writes retain client redaction and the server's token-shape check, but local mode uses the OS account rather than remote identities and inbox enforcement.
- **Model calls.** The dreamer sends redacted digests, and the curator and contradiction judge send note and fact excerpts through `claude` to its configured provider. The verifier can read permitted store and source files through its tools. These model calls can send that content to the provider; hosting the git store yourself does not make model processing local.

Report a vulnerability: [SECURITY.md](SECURITY.md).

## Status and limits

- Running daily for one owner since 2026-10-08. Other setups are new ground.
- The curator, verifier and app assume the `claude` CLI. Other model front ends are not wired.
- The owner-words corpus reads Claude Code and Codex transcripts and prompt histories. Claude Code deletes old transcripts, so the proof of an old ruling depends on the quote ledger.
- The verifier catches few of the retirements the owner would make (see the table). The owner's queue is the backstop.
- Delivery numbers are single-prompt tasks with Claude Code. Long sessions and the other harnesses are not measured.
- CLI/browser UI: Linux, macOS and Windows. Desktop-window builds remain optional on Linux/macOS. Windows uses its default browser and Task Scheduler. See [actual platform validation](docs/portable.md#validation).

## Repository layout

```
client/     Go client: hooks, search, MCP, dreamer, curator, verifier, app
server/     Elixir server: git smart HTTP, pre-receive, journal, change feed
eval/       measurement scripts and results (VALUE.md)
docs/       plan and design history
scripts/    Mac setup, icon build
SPEC.md     the store contract the client and server implement
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

SPDX-License-Identifier: Apache-2.0

Apache License 2.0: [LICENSE](LICENSE). Bundled fonts are under the SIL Open Font License 1.1: [NOTICE](NOTICE).
