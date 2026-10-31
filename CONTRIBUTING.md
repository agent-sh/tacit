# Contributing

Issues and pull requests are welcome. A bug report with the command, what it printed and what you expected is the fastest to act on.

## Ground rules

- **SPEC.md is the contract.** The client, the server and the curator follow it. A change in behavior changes SPEC.md in the same pull request.
- **Measure before you add.** tacit's retrieval and curation choices each have a number behind them in `eval/VALUE.md`, including the ones that lost. A new ranker, prompt or delivery rule comes with its measurement against the current one, and the method in `eval/`. A change that does not beat the current behavior does not ship, and its code does not stay.
- **A test proves something only if it failed before the change and passes after it.** Prefer an integration test of the behavior over unit tests of internals.
- **Fail closed on secrets, fail open on delivery.** Redaction that cannot run stops the write. A hook that cannot reach the store prints nothing and exits 0.
- **Never put real data in the repo.** No facts from a real store, no transcripts, no hostnames, no keys. Test fixtures are synthetic.
- Plain prose in docs: short sentences, the fact first.

## Build and test

Client (Go 1.27):

```
cd client
gofmt -l .          # must print nothing
go vet ./...
go test -timeout 20m ./...
```

`cmd/tacit` is the slowest package, about 3 minutes on a hosted runner. It has taken 10 minutes on a slower setup, which is where go test stops a test binary by default, so the command gives each 20.

Tests that need [scrump](https://github.com/avifenesh/scrump) with its text profile skip when it is not on `PATH`. Install it to run them (see [redaction setup](docs/portable.md#redaction)).

Server (Elixir 1.20, OTP 28):

```
cd server
mix deps.get
mix format --check-formatted
MIX_ENV=test mix test
```

The server tests start the real server on loopback, use a real git client and need a free port 17447.

Desktop app (optional): `go build -tags desktop,production,webkit2_41 ./cmd/tacit` on Linux with `libgtk-3-dev` and `libwebkit2gtk-4.1-dev`, or `-tags desktop,production` on macOS.

## CI

CI checks format, vet and build on every pull request. The full suites run on pull requests from forks, once a day, and on demand. Public forks enable GitHub-hosted CI with `CI_PUBLIC=1`. Private repositories use `CI_CPU_RUNNER` to select a self-hosted runner. Run the suites locally before you push.

## Agent configuration checks

[agnix](https://github.com/agent-sh/agnix) runs in strict mode on every pull request and push to main. The action and binary are pinned to 0.57.0. Run `agnix --strict .` for the repository check.

CI also builds Tacit and runs `scripts/agent-configs.py` in a disposable profile. It lints the actual Claude hooks, Codex hooks and Codex MCP settings printed by the installer. To repeat this locally, pass a built client with `--binary` and a new directory with `--out`, then run `agnix --strict` on that directory. The generator refuses existing profiles. Agnix 0.57 recognizes the Claude-compatible hook schema at `.claude/settings.json`, so a byte-for-byte copy of Codex's generated hooks is checked at that path too.

Synthetic evaluation corpora and build artifacts are excluded from the repository scan. All generated integration files are in scope for the separate check.

## Pull requests

- One change per pull request, with the why in the description.
- Say what you ran: the commands and their result.
- New flags, environment variables or defaults go in the [configuration reference](docs/portable.md#configuration) and in SPEC.md.
