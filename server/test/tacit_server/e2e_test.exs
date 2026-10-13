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
