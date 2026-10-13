# Setup for your own store

This guide describes a shared server installation. For a local store and native Windows setup, see [Local and portable tacit](portable.md).

Use your own server, tailnet, keys, transcript sources and model account. The defaults are local paths and Claude CLI aliases. No developer server address, owner name or model credential is required.

The [README quick start](../README.md#quick-start) builds and installs tacit. This guide explains what each machine needs and how to check the installation. Commands below run from the source checkout unless noted otherwise.

## Choose the roles

| Machine | Needs | Does |
|---|---|---|
| Server | Linux, Docker, git, Tailscale; SSH access for deployment | Owns the private bare store, validates keys and pushes, serves git and the change feed. No model account. |
| Writer | Client binary, git, scrump, tailnet membership, one enrolled key per harness | Receives facts and adds notes to that harness's inbox. Claude CLI is needed only if running the dreamer. |
| Curator | Writer prerequisites, an enrolled curator key, Claude CLI login/provider configuration, repo checkouts and owner sources | Curates notes, computes git facts, verifies findings and hosts the owner's editable app. |
| Off-tailnet reader | Client binary, git, scrump, enrolled reader key, server's optional Funnel URL | Fetches with the reader key. Agent notes stay in a local overlay. No pushes or dreamer. |

The curator can be a writer workstation. Keep its key on that machine. Enroll each machine's printed public key on the server; private keys stay on the client. A reader key does not grant write access.

## Paths and owner configuration

Defaults work with repo checkouts under `~/projects`. Set `TACIT_PROJECTS` when your checkouts live elsewhere. Set `TACIT_OWNER_NAMES` to names used to attribute the owner's decisions. It helps parse attribution; it does not prove that the owner said anything.

For a custom `TACIT_STORE`, export it in your shell before cloning and installing jobs. Keep it available to the harness processes that run hooks. The follower captures `TACIT_STORE` when installed and does not read the model job env file; reinstall it after changing that path. Put the same value in the dreamer/curator env file so those jobs use the same store.

`TACIT_VERIFY_ROOTS` adds read-only source directories, separated like `PATH`. Define it only for directories that exist and that you intend the verifier to read. Claude auto-memory is optional. Owner-proof collection needs readable owner turns or the existing proof ledger; generated agent text cannot supply that proof.

`~/.config/tacit/derive.yaml` can select a non-default branch and disambiguate ordinary words that are also repo names:

```yaml
branches:
  demo-api: dev
ordinary_names: [compass, notebook]
```

Use your actual repo names. An absent configuration file means remote default branches and the built-in word list. See the [configuration table](../README.md#configuration) for store, index and key paths.

## Job configuration

Create the job env file without replacing an existing one:

```sh
mkdir -p ~/.config/tacit
touch ~/.config/tacit/env
chmod 600 ~/.config/tacit/env
```

Put the model variables and any custom path variables there as `KEY=VALUE` lines. The defaults are `opus` for curation and verification, `haiku` for the dreamer and contradiction judge, and `sonnet` for verification's capacity fallback. If your provider needs full model IDs, replace those aliases with IDs that provider supports. Use your own provider login or credentials.

Systemd user services do not inherit your shell's exports. The copied curator unit also needs a `PATH` that finds `claude`, `scrump` and `git`. Use absolute directories in the file; `~` and `$HOME` are not shell-expanded by systemd. To append the current shell's already expanded path:

```sh
printf 'PATH=%s\n' "$PATH" >> ~/.config/tacit/env
```

Check an existing PATH entry before appending another. On macOS, the dreamer reads the same file with `--env-file`. Do not commit the env file or copy credentials into unit files.

Install writes files and prints start commands. Run those commands. On Linux, the follower is a service and the dreamer is a timer:

Before starting the dreamer, generate its separate writer key with `tacit keygen dreamer`. Enroll the printed public key on the server as `dreamer@<host>`, using the same enrollment command as the normal harness keys, then require `tacit authcheck dreamer --url <server-url>` to exit 0. A Claude Code key does not authorize dreamer writes.

```sh
tacit install follow
tacit install dream
systemctl --user daemon-reload
systemctl --user enable --now tacit-follow.service tacit-dream.timer
```

The curator and verifier need the curator key enrolled as well as a normal writer key. Complete [ruling-proof initialization](#initialize-ruling-proof) before enabling their timer. On Linux install the timer from the source checkout:

```sh
mkdir -p ~/.config/systemd/user
cp client/deploy/systemd/tacit-curate.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now tacit-curate.timer
```

This unit expects the client at `~/.local/bin/tacit` and reads `~/.config/tacit/env`. For another binary or env path, edit the unit before starting it. On headless machines enable lingering so user jobs survive logout. macOS gets launchd start commands from the installer; the provided curator timer is Linux-only.

## Initialize ruling proof

Enroll `tacit keygen curator`'s printed public key on the server and require `tacit authcheck curator --url <server-url>` to exit 0. Run the following on that machine, with your store and owner-source path variables exported in its shell:

```sh
tacit rulings audit --dry-run
tacit rulings audit
```

Inspect the dry-run report before applying it. The applied audit writes `review/rulings-audit.json`, the quote ledger and any fact corrections, then pushes them as curator. Check that it exits 0 and that the marker is present in your store clone. The dry-run alone does not activate evidence-based rulings.

The audit requires readable owner turns from Claude Code or Codex, and can also use existing session digests. If you have no owner conversation history yet, create a normal owner session first. An empty or unreadable corpus makes the audit fail without changing the store. Do not fabricate quotes or write the marker by hand.

Before this successful audit, existing and server-created unaudited stores use the legacy type-based ruling definition. New local stores made with `tacit init` start with evidence-based rulings while empty. They can treat an unsupported agent `user_standard` as a ruling. The proof-backed guarantee applies after initialization. Once enabled, the stored ledger preserves verified quotes even if original transcripts are later deleted.

## Check each boundary

After enrollment, `tacit authcheck <harness> --url <server-url>` must exit 0 before you clone. A missing or mismatched key is different from a temporarily unreachable server; the command prints which kind of failure it saw.

After cloning, use your real repo and host names:

```sh
tacit sync
tacit lint
tacit search --repo demo-api --host workstation "Go toolchain"
```

An empty search on a new store is expected. Add notes through your harness, let the curator produce facts, then check a query that names one. Install the hooks for that harness and check a new session. Codex requires trusting newly installed hooks once in `/hooks`.

On Linux inspect job status and logs:

```sh
systemctl --user status tacit-follow.service tacit-dream.timer tacit-curate.timer
journalctl --user -u tacit-follow.service -u tacit-dream.service -u tacit-curate.service
```

The follower needs the server and git. A model job also needs scrump, Claude CLI access, the configured model and any required source directories. Fix that boundary before changing fact or retrieval settings. `tacit app --browser` opens the review UI from the CLI build; editing requires the curator key on that machine.

The git store stays on your server, but model jobs send digests, fact excerpts and permitted verifier tool content to the provider configured in `claude`. Choose that provider with your own data requirements in mind. Scrump redacts recognized secrets; it does not remove all private business or source content.

## Try the methods before deploying

The synthetic sample runs locally, without a server or model account:

```sh
python3 -I eval/delivery/delivery.py --truth eval/examples/truth.sample.jsonl \
  --store eval/examples/store --host workstation
python3 -I eval/baseline/grep_baseline.py --truth eval/examples/truth.sample.jsonl \
  --home eval/examples/home
```

The sample uses invented facts and eight invented situations. It demonstrates input formats and scoring. [Methods](methods.md) explains how to build a real truth set and distinguish coverage, retrieval and agent outcomes. [Client reference](client.md) lists the operational commands and flags.
