defmodule TacitServer.E2ETest do
  @moduledoc """
  The phase 2 proofs over a real git client against the running server on
  loopback: clone, push to own inbox, rejection outside the inbox, rejection
  without a key, rejection of a secret, journal and feed carry the push.
  """
  use ExUnit.Case, async: false
  import TacitServer.TestHelpers
  alias TacitServer.Auth.Keys

  setup_all do
    {keyid, priv, pub} = keypair("claude@host-a")
    {other, opriv, opub} = keypair("codex@host-a")
    Keys.enroll(keyid, pub)
    Keys.enroll(other, opub)
    %{keyid: keyid, priv: priv, other: other, opriv: opriv}
  end

  test "clone works for a tailnet peer without a key" do
    dir = clone!()
    assert File.exists?(Path.join(dir, "keys/claude@host-a.pub"))
  end

  test "enroll is idempotent for an unchanged key" do
    {k, _, pub} = keypair("pi@host-a")
    before = TacitServer.State.head_seq()
    sha = Keys.enroll(k, pub)
    assert TacitServer.State.head_seq() == before + 1
    assert Keys.enroll(k, pub) == sha
    assert TacitServer.State.head_seq() == before + 1
  end

  test "push into own inbox is accepted and journaled, outside is rejected", %{
    keyid: keyid,
    priv: priv
  } do
    dir = clone!()
    before = TacitServer.State.head_seq()

    write_commit!(
      dir,
      "inbox/#{keyid}/first-fact.md",
      "---\nname: first-fact\n---\n\nbody\n",
      "first"
    )

    out = git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    refute out =~ "rejected"
    assert TacitServer.State.head_seq() == before + 1
    [ev] = TacitServer.State.since(before, 10)
    assert ev.ref == "refs/heads/main" and ev.writer == keyid

    # The hook keeps no log of its own; the journal and the unit log cover it.
    refute File.exists?(Path.join(TacitServer.Config.get(:data), "hook.log"))

    write_commit!(dir, "projects/memra/sneaky.md", "x\n", "escape")
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "may write only under inbox/#{keyid}/"
    assert TacitServer.State.head_seq() == before + 1
  end

  test "push with another writer's inbox is rejected", %{keyid: keyid, other: other, opriv: opriv} do
    dir = clone!()
    write_commit!(dir, "inbox/#{keyid}/impostor.md", "x\n", "impostor")
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(other, opriv))
    assert status != 0
    assert out =~ "may write only under inbox/#{other}/"
  end

  test "push without credentials or with an unknown key is 401" do
    dir = clone!()
    write_commit!(dir, "inbox/claude@host-a/anon.md", "x\n", "anon")
    {out, status} = git(dir, ["push", "origin", "HEAD:main"])
    assert status != 0
    assert out =~ "401" or out =~ "Authentication failed" or out =~ "could not read Username"

    # A well-formed key id with an allowed harness whose key was never enrolled.
    {_, upriv, _} = keypair("human@ghost")
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env("human@ghost", upriv))
    assert status != 0
    assert out =~ "401" or out =~ "Authentication failed"
  end

  test "a secret in a fact is rejected", %{keyid: keyid, priv: priv} do
    dir = clone!()

    write_commit!(
      dir,
      "inbox/#{keyid}/leak.md",
      "token: ghp_" <> String.duplicate("a", 30) <> "\n",
      "leak"
    )

    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "contains a secret"
  end

  test "a secret at the top of a large file is still found", %{keyid: keyid, priv: priv} do
    dir = clone!()
    # Past the 64 KiB pipe buffer: a scan that pipes `git cat-file` into
    # `grep -q` under pipefail sees the SIGPIPE status of cat-file instead of
    # the match and lets the push through.
    padding = String.duplicate("filler line that is not a secret at all\n", 8_000)

    write_commit!(
      dir,
      "inbox/#{keyid}/big-leak.md",
      "token: ghp_" <> String.duplicate("b", 30) <> "\n" <> padding,
      "big leak"
    )

    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "contains a secret"
  end

  # A fact name is lowercase letters, digits and hyphens. The rule before
  # v0.9.15 read `sk-` and 16 such characters as a key, so every curator push
  # whose MEMORY.md, folder index or file named such a fact was refused.
  test "a fact name with an sk- segment is no secret", %{keyid: keyid, priv: priv} do
    dir = clone!()

    body =
      for name <- [
            "lint-cc-sk-017-only-plain-commands",
            "sk-learn-pipeline-cache-notes",
            "docs-sk-locale-fallback-order-v2"
          ],
          into: "",
          do: "- [[#{name}]]: a fact\n- [#{name}](#{name}.md) - a fact\n"

    write_commit!(dir, "inbox/#{keyid}/lint-cc-sk-017-only-plain-commands.md", body, "sk names")
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status == 0, out
  end

  # The push lease is one for every writer, so the scan of one blob must cost
  # what its size does. Under the server's C.UTF-8, GNU grep took minutes on
  # a long line of sk- runs; the hook scans in the C locale.
  test "a long line of sk- runs is scanned in linear time and taken", %{keyid: keyid, priv: priv} do
    dir = clone!()

    write_commit!(
      dir,
      "inbox/#{keyid}/long-line.md",
      String.duplicate("sk-", 90_000) <> "\n",
      "long line"
    )

    {elapsed, {out, status}} =
      in_server_locale(fn ->
        :timer.tc(fn -> git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv)) end)
      end)

    assert status == 0, out
    assert elapsed < 20_000_000, "the push took #{div(elapsed, 1000)} ms"
  end

  # The scan reads bytes: a NUL does not make the blob "binary" and hide what
  # follows, and a byte that is not UTF-8 right before a key does not hide it.
  test "a key after a NUL or a byte that is not UTF-8 is still refused", %{
    keyid: keyid,
    priv: priv
  } do
    dir = clone!()
    [{_, key} | _] = synthetic_sk_keys(1)

    for {name, body} <- [
          {"after-nul", <<0>> <> "\nexport KEY=\"" <> key <> "\"\n"},
          {"after-ff", <<0xFF>> <> "sk-abcdefgh-ijklmnopQ\n"}
        ] do
      write_commit!(dir, "inbox/#{keyid}/#{name}.md", body, name)

      {out, status} =
        in_server_locale(fn ->
          git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
        end)

      assert status != 0 and out =~ "contains a secret", "#{name} got in: #{out}"
      git!(dir, ["reset", "--quiet", "--hard", "HEAD~1"])
    end
  end

  # The hook runs with the server's environment; production sets LANG=C.UTF-8.
  defp in_server_locale(fun) do
    lang = System.get_env("LANG")
    lc_all = System.get_env("LC_ALL")
    System.put_env("LANG", "C.UTF-8")
    System.delete_env("LC_ALL")

    try do
      fun.()
    after
      if lang, do: System.put_env("LANG", lang), else: System.delete_env("LANG")
      if lc_all, do: System.put_env("LC_ALL", lc_all)
    end
  end

  test "sk- keys of every shape are still refused", %{keyid: keyid, priv: priv} do
    dir = clone!()

    for seed <- 1..3, {shape, key} <- synthetic_sk_keys(seed) do
      write_commit!(dir, "inbox/#{keyid}/key.md", "export KEY=\"#{key}\"\n", shape)
      {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
      assert status != 0 and out =~ "contains a secret", "#{shape} (seed #{seed}) got in: #{key}"
      git!(dir, ["reset", "--quiet", "--hard", "HEAD~1"])
    end
  end

  # Random keys of the shapes providers issue, never a real one: Anthropic and
  # OpenAI (gitleaks' current rules), the legacy 48 characters, 32 hex,
  # OpenRouter, Langfuse, LiteLLM, and an Anthropic body with no 20-character
  # alphanumeric run. The fixed ones can each be caught by one arm of the
  # rule only: an uppercase letter or underscore anywhere in the run, a
  # 16-character token among them, and a lowercase UUID.
  defp synthetic_sk_keys(seed) do
    :rand.seed(:exsss, {seed, 7, 11})
    b64url = ~c"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
    alnum = ~c"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
    hex = ~c"0123456789abcdef"
    pick = fn chars, n -> for _ <- 1..n, into: "", do: <<Enum.random(chars)>> end
    no_run = Enum.map_join(1..5, "-", fn _ -> pick.(alnum, 19) end) |> binary_part(0, 93)

    [
      {"anthropic api03", "sk-ant-api03-" <> pick.(b64url, 93) <> "AA"},
      {"anthropic admin01", "sk-ant-admin01-" <> pick.(b64url, 93) <> "AA"},
      {"anthropic no run", "sk-ant-api03-" <> no_run <> "AA"},
      {"openai project", "sk-proj-" <> pick.(b64url, 74) <> "T3BlbkFJ" <> pick.(b64url, 74)},
      {"openai service", "sk-svcacct-" <> pick.(b64url, 58) <> "T3BlbkFJ" <> pick.(b64url, 58)},
      {"openai legacy", "sk-" <> pick.(alnum, 20) <> "T3BlbkFJ" <> pick.(alnum, 20)},
      {"legacy 48", "sk-" <> pick.(alnum, 48)},
      {"32 hex", "sk-" <> pick.(hex, 32)},
      {"openrouter", "sk-or-v1-" <> pick.(hex, 64)},
      {"langfuse",
       "sk-lf-#{pick.(hex, 8)}-#{pick.(hex, 4)}-4#{pick.(hex, 3)}-a#{pick.(hex, 3)}-#{pick.(hex, 12)}"},
      {"litellm", "sk-" <> pick.(b64url, 22)},
      {"mark at start", "sk-Qabcd-efghi-jklmn-opqrs"},
      {"mark in middle", "sk-abcd-efghi_jklm-nopqrs"},
      {"mark at end", "sk-abcd-efghi-jklmn-opqrsQ"},
      {"short token", "sk-abcd-efgh-ijkLmn"},
      {"uuid only", "sk-lf-00000000-0000-4000-8000-000000000000"}
    ]
  end

  test "deleting a fact is rejected for a writer", %{keyid: keyid, priv: priv} do
    dir = clone!()
    write_commit!(dir, "inbox/#{keyid}/to-delete.md", "x\n", "add")
    git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    File.rm!(Path.join(dir, "inbox/#{keyid}/to-delete.md"))
    git!(dir, ["add", "-A"])
    git!(dir, ["commit", "--quiet", "-m", "rm"])
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "supersede, never delete"
  end

  test "a merge commit from a writer is rejected even when its net diff touches other paths", %{
    keyid: keyid,
    priv: priv
  } do
    dir = clone!()
    # Side branch that edits a curated path, merged with -s ours into a legitimate-looking commit.
    git!(dir, ["checkout", "--quiet", "-b", "evil"])
    write_commit!(dir, "keys/curator@host-a.pub", "ssh-ed25519 AAAA pwned\n", "pwn")
    git!(dir, ["checkout", "--quiet", "-"])
    write_commit!(dir, "inbox/#{keyid}/legit.md", "x\n", "legit")
    git!(dir, ["merge", "--quiet", "--no-ff", "-m", "merge", "evil"])
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "may not push merge commits" or out =~ "may write only under"
    refute File.exists?(Path.join(clone!(), "keys/curator@host-a.pub"))
  end

  test "a merge commit is refused by itself when every path it touches is the writer's own", %{
    keyid: keyid,
    priv: priv
  } do
    dir = clone!()
    git!(dir, ["checkout", "--quiet", "-b", "side"])
    write_commit!(dir, "inbox/#{keyid}/side.md", "a\n", "side")
    git!(dir, ["checkout", "--quiet", "-"])
    write_commit!(dir, "inbox/#{keyid}/mainline.md", "b\n", "mainline")
    git!(dir, ["merge", "--quiet", "--no-ff", "-m", "merge", "side"])
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0
    assert out =~ "may not push merge commits"

    # The same content as linear commits is accepted: only the merge was the problem.
    git!(dir, ["reset", "--quiet", "--hard", "HEAD^"])
    git!(dir, ["cherry-pick", "--quiet", "side"])
    out = git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    refute out =~ "rejected"
  end

  test "symlinks are rejected, unicode file names are fine", %{keyid: keyid, priv: priv} do
    dir = clone!()
    File.mkdir_p!(Path.join(dir, "inbox/#{keyid}"))
    File.ln_s!("/etc/passwd", Path.join(dir, "inbox/#{keyid}/link.md"))
    git!(dir, ["add", "-A"])
    git!(dir, ["commit", "--quiet", "-m", "link"])
    {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(keyid, priv))
    assert status != 0 and out =~ "symlinks and submodules"
    git!(dir, ["reset", "--quiet", "--hard", "HEAD~1"])
    write_commit!(dir, "inbox/#{keyid}/café-ünï.md", "task-management-system\n", "unicode")
    out = git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    refute out =~ "rejected"
  end

  test "journal endpoint and SSE feed replay the push", %{keyid: keyid, priv: priv} do
    since = TacitServer.State.head_seq()
    dir = clone!()

    write_commit!(
      dir,
      "inbox/#{keyid}/feed-#{System.unique_integer([:positive])}.md",
      "x\n",
      "feed"
    )

    git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))

    {200, _, body} = http_get("/journal?since=#{since}")
    [%{"seq" => seq, "writer" => ^keyid}] = JSON.decode!(body)
    assert seq == since + 1

    # The feed replays from Last-Event-ID; read one frame then close.
    {:ok, sock} =
      :gen_tcp.connect(
        {127, 0, 0, 1},
        TacitServer.Config.get(:port),
        [:binary, active: false],
        2_000
      )

    :ok =
      :gen_tcp.send(sock, "GET /events HTTP/1.1\r\nHost: t\r\nLast-Event-ID: #{since}\r\n\r\n")

    data = recv_until(sock, "event: head_moved", "")
    :gen_tcp.close(sock)
    assert data =~ "id: #{seq}\nevent: head_moved\ndata: {"
  end

  test "non-tailnet, non-loopback peers are refused" do
    # The plug is unit-tested here since the test server only listens on loopback.
    conn = Plug.Test.conn(:get, "/healthz") |> Map.put(:remote_ip, {8, 8, 8, 8})
    conn = TacitServer.Router.call(conn, TacitServer.Router.init([]))
    assert conn.status == 403
  end

  # The socket carries chunked transfer framing, so match on the frame body.
  defp recv_until(sock, marker, acc) do
    if String.contains?(acc, marker) and String.contains?(acc, "\ndata: {") and
         String.contains?(acc, "}\n") do
      acc
    else
      {:ok, d} = :gen_tcp.recv(sock, 0, 5_000)
      recv_until(sock, marker, acc <> d)
    end
  end
end
