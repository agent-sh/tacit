Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release makes a fact's name lead to its live fact when two files hold the name, counts the owner's words once for each turn they typed, and gives the rotation one list of machine roots. The server moves to bandit 1.14.1 and plug_crypto 2.2.1, past every advisory its lock carried, and the daily checks are green again.

- **A name reads as its live curated fact.** A store can hold one name in two files: an agent's note named like a curated fact (`tacit add` checks a name in its own inbox folder only), or a copy a tool outside tacit left in a second folder (#102).
  - `tacit_fact`, `tacit fact`, the successor chain `tacit_fact` follows and `tacit resolve` take the name's live curated fact, else a superseded one, else a retired one, else a note; the store's comes before this machine's overlay. The answer's last line names the other files of the name and what each is (`# also named ...`).
  - Before, the lookup took the first file its walk met. An agent asking for the fact `tacit_search` showed could read an inbox note or a retired copy instead, and `tacit resolve` could write into the retired copy. `tacit resolve` now refuses a name two curated files hold, and names them.
  - The curator makes no such pair. When a name and its hashed form are both taken, a counter follows the hash (`<name>-<hash>-2`), and a file that does not parse keeps its name, so no new fact is written over it. The migration's copies get the same counter.
- **The owner's words count once for each turn they typed.** The corpus that statements and rulings check words against took three kinds of text for theirs (SPEC "A turn two sessions hold", #103).
  - A copy of a turn, which a fork or a session resumed into a new transcript carries over, stays in the session whose prompt history records it. The other sessions leave it out, so one saying no longer stands in two sessions, which is what the statements step promotes on.
  - The same text in two sessions within 2 s is a program's broadcast, and no one's.
  - A prompt of the shape `Automated <job> wake requested ...` is the harness's, as a wake loop's `Wake nonce=` prompt already was. A person's sentence about automated tests stays theirs.
  - Known gaps, both on the side of leaving words out: a person's own fan-out to several sessions through a tool, and a copy whose times differ within 2 s.
- **The rotation reads one list of machine roots.** The reader that puts a fact with a gone path first and the reader that stats paths for the proof now share `~`, `/Users`, `/home`, `/data`, `/opt`, `/etc`, `/var`, `/usr`, `/srv`, `/mnt` and `/Library` (#101).
  - A path under `/data` gets its stat in the proof. A `/Library` path no longer lends its words to the clause of another path.
  - A system root is never a folder of a tree: a repository named `data` holds nothing by it. A projects directory under a root is a folder of checkouts, as the home's is.
- **The server's dependencies.** bandit 1.12.5 -> 1.14.1 and plug_crypto 2.2.0 -> 2.2.1 clear the 15 advisories `mix deps.get` listed, among them HTTP/2 StreamError recovery that bypassed per-connection limits (GHSA-2238-pgfc-w82m). thousand_island 1.6.0 comes with bandit 1.14. The bandit floor in `mix.exs` is the fixed version (#104).
- **The checks.** The daily `ci` and `portable client` runs pass again.
  - Each public-path test drew its source from 250 addresses, and one that drew the source another test had failed with started past its lowered limit: the daily run of 2026-10-30 failed on it. Every test there now starts with no failure counted (#105).
  - The client tests that need redaction to run use their stand-in whether or not scrump is installed, off Windows. With scrump installed, `cmd/tacit` took ten minutes and failed at go test's default timeout. It now takes three (#106).
  - CONTRIBUTING gives the client suite a 20m timeout (#107), and four client tests pass with Go's temp dir under `~/.cache` (#100).

Upgrading: no index, store format or protocol change. Install the client on every machine that reads the store, since the name lookup is in the MCP server and the CLI, and on every machine that curates, whose statements and rulings read the owner's turns. The server takes a redeploy for its new dependencies (`server/deploy/deploy.sh`); a server on the old lock serves this client as before.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
