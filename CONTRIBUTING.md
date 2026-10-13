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
go test ./...
```

Tests that need [scrump](https://github.com/avifenesh/scrump) with its text profile skip when it is not on `PATH`. Install it to run them (see the README).

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

CI checks format, vet and build on every pull request. The full suites run on pull requests from forks, once a day, and on demand. A fork of this repo runs CI once its owner sets `CI_PUBLIC=1` (GitHub-hosted runners) or `CI_CPU_RUNNER` (a self-hosted runner). Run the suites locally before you push.

## Pull requests

- One change per pull request, with the why in the description.
- Say what you ran: the commands and their result.
- New flags, environment variables or defaults go in the README configuration table and in SPEC.md.
