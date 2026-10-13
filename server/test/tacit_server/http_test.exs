defmodule TacitServer.HttpTest do
  @moduledoc """
  The smart-HTTP layer with hand-built requests where a real git client does
  not produce them (gzip push, oversize bodies, protocol v2 advertisement) and
  with real clients where it does (concurrent pushes, v2 fetch).
  """
  use ExUnit.Case, async: false
  import TacitServer.TestHelpers
  alias TacitServer.State

  @cap_push 64 * 1024 * 1024
  @cap_upload 10 * 1024 * 1024

  setup_all do
    {keyid, priv} = enroll!("claude@http")
    %{keyid: keyid, priv: priv}
  end

  # The receive-pack request git would send: one command line with
  # capabilities, a flush, then the pack of what the server lacks.
  defp receive_pack_request(dir, old, new) do
    pack =
      ["git", "-C", dir, "pack-objects", "--revs", "--stdout", "-q"]
      |> Exile.stream!(input: ["#{new}\n^#{old}\n"], stderr: :disable)
      |> Enum.into([])
      |> IO.iodata_to_binary()

    pkt("#{old} #{new} refs/heads/main\0report-status\n") <> "0000" <> pack
  end

  defp new_commit!(dir, keyid, name) do
    write_commit!(dir, "inbox/#{keyid}/#{name}.md", "#{name}\n", name)
    git!(dir, ["rev-parse", "HEAD"]) |> String.trim()
  end

  describe "gzip" do
    test "a gzip-encoded push is inflated, accepted and journaled", %{keyid: keyid, priv: priv} do
      dir = clone!()
      old = git!(dir, ["rev-parse", "HEAD"]) |> String.trim()
      new = new_commit!(dir, keyid, "gz-#{System.unique_integer([:positive])}")
      before = State.head_seq()

      body = :zlib.gzip(receive_pack_request(dir, old, new))

      {200, _, resp} =
        http_post(
          "/git/git-receive-pack",
          [{"content-encoding", "gzip"}, {"authorization", basic(keyid, priv)}],
          "application/x-git-receive-pack-request",
          body
        )

      assert resp =~ "unpack ok"
      assert resp =~ "ok refs/heads/main"

      assert String.trim(
               git!(Path.join(TacitServer.Config.get(:data), "store.git"), ["rev-parse", "main"])
             ) == new

      assert State.head_seq() == before + 1
      assert [%{writer: ^keyid, old: ^old, new: ^new}] = State.since(before, 10)
      assert spool_files() == []
    end

    test "a corrupt gzip body is 400 and leaves nothing in the spool", %{keyid: keyid, priv: priv} do
      before = State.head_seq()

      {400, _, resp} =
        http_post(
          "/git/git-receive-pack",
          [{"content-encoding", "gzip"}, {"authorization", basic(keyid, priv)}],
          "application/x-git-receive-pack-request",
          "this is not gzip" <> :binary.copy("x", 100)
        )

      assert resp =~ "bad gzip body"
      assert spool_files() == []
      assert State.head_seq() == before
    end

    test "a gzip body cut short is 400, not handed to git as if it were whole", %{
      keyid: keyid,
      priv: priv
    } do
      dir = clone!()
      old = git!(dir, ["rev-parse", "HEAD"]) |> String.trim()
      new = new_commit!(dir, keyid, "cut-#{System.unique_integer([:positive])}")
      whole = :zlib.gzip(receive_pack_request(dir, old, new))
      cut = binary_part(whole, 0, div(byte_size(whole), 2))
      before = State.head_seq()

      {400, _, resp} =
        http_post(
          "/git/git-receive-pack",
          [{"content-encoding", "gzip"}, {"authorization", basic(keyid, priv)}],
          "application/x-git-receive-pack-request",
          cut
        )

      assert resp =~ "truncated gzip body"
      assert State.head_seq() == before
      assert spool_files() == []

      {400, _, resp} =
        http_post(
          "/git/git-upload-pack",
          [{"content-encoding", "gzip"}],
          "application/x-git-upload-pack-request",
          cut
        )

      assert resp =~ "truncated gzip body"
    end

    test "a gzip bomb is stopped at the inflated cap, not at the wire size", %{
      keyid: keyid,
      priv: priv
    } do
      bomb = :zlib.gzip(:binary.copy(<<0>>, @cap_push + 1_048_576))
      assert byte_size(bomb) < 1_048_576

      {413, _, resp} =
        http_post(
          "/git/git-receive-pack",
          [{"content-encoding", "gzip"}, {"authorization", basic(keyid, priv)}],
          "application/x-git-receive-pack-request",
          bomb
        )

      assert resp =~ "inflated push larger than #{@cap_push} bytes"
      assert spool_files() == []
    end

    test "a gzip upload-pack request is inflated and answered" do
      body =
        pkt("command=ls-refs\n") <>
          pkt("agent=test\n") <>
          "0001" <>
          pkt("ref-prefix refs/heads/\n") <>
          "0000"

      {200, _, resp} =
        http_post(
          "/git/git-upload-pack",
          [{"content-encoding", "gzip"}, {"git-protocol", "version=2"}],
          "application/x-git-upload-pack-request",
          :zlib.gzip(body)
        )

      assert resp =~ " refs/heads/main"
    end

    test "a gzip bomb sent to upload-pack is 413 at the inflated cap" do
      bomb = :zlib.gzip(:binary.copy(<<0>>, @cap_upload + 1))

      {413, _, resp} =
        http_post(
          "/git/git-upload-pack",
          [{"content-encoding", "gzip"}],
          "application/x-git-upload-pack-request",
          bomb
        )

      assert resp =~ "inflated request larger than #{@cap_upload} bytes"
    end
  end

  describe "size caps" do
    test "a push body over 64 MiB is 413", %{keyid: keyid, priv: priv} do
      status =
        raw_post_zeros(
          "/git/git-receive-pack",
          [
            {"Content-Type", "application/x-git-receive-pack-request"},
            {"Authorization", basic(keyid, priv)}
          ],
          @cap_push + 1
        )

      assert status == 413
      assert spool_files() == []
    end

    test "a push body of exactly 64 MiB is not refused for its size", %{keyid: keyid, priv: priv} do
      # Zeros are not a valid command list, so receive-pack fails on them (400);
      # what matters is that the cap did not fire (413) and nothing is left behind.
      status =
        raw_post_zeros(
          "/git/git-receive-pack",
          [
            {"Content-Type", "application/x-git-receive-pack-request"},
            {"Authorization", basic(keyid, priv)}
          ],
          @cap_push
        )

      assert status == 400
      assert spool_files() == []
    end

    test "an early refusal does not leave the connection waiting for body bytes already read",
         %{keyid: keyid, priv: priv} do
      bomb = :zlib.gzip(:binary.copy(<<0>>, @cap_push + 1_048_576))

      {:ok, s} =
        :gen_tcp.connect({127, 0, 0, 1}, TacitServer.Config.get(:port), [:binary, active: false])

      :ok =
        :gen_tcp.send(s, [
          "POST /git/git-receive-pack HTTP/1.1\r\nHost: t\r\n",
          "Content-Type: application/x-git-receive-pack-request\r\nContent-Encoding: gzip\r\n",
          "Authorization: #{basic(keyid, priv)}\r\nContent-Length: #{byte_size(bomb)}\r\n\r\n",
          bomb
        ])

      {:ok, resp} = :gen_tcp.recv(s, 0, 5_000)
      assert resp =~ "413"
      assert resp =~ "connection: close"

      # The server closes at once; a stale conn used to hold it for the 15 s read timeout.
      t0 = System.monotonic_time(:millisecond)
      assert {:error, :closed} = :gen_tcp.recv(s, 0, 5_000)
      assert System.monotonic_time(:millisecond) - t0 < 3_000
    end

    test "an upload-pack request over 10 MiB is 413" do
      status =
        raw_post_zeros(
          "/git/git-upload-pack",
          [{"Content-Type", "application/x-git-upload-pack-request"}],
          @cap_upload + 1
        )

      assert status == 413
    end

    test "an upload-pack request of exactly 10 MiB is read" do
      status =
        raw_post_zeros(
          "/git/git-upload-pack",
          [{"Content-Type", "application/x-git-upload-pack-request"}],
          @cap_upload
        )

      assert status == 200
    end

    test "a push needs a writer token before any body is read" do
      status =
        raw_post_zeros(
          "/git/git-receive-pack",
          [{"Content-Type", "application/x-git-receive-pack-request"}],
          1_000
        )

      assert status == 401
    end
  end

  describe "protocol version 2" do
    test "info/refs for upload-pack answers v2 capabilities, without the service banner" do
      {200, hdrs, body} =
        http_get("/git/info/refs?service=git-upload-pack", [{"git-protocol", "version=2"}])

      assert "000eversion 2\n" <> _ = body
      assert body =~ "ls-refs"
      assert body =~ "fetch"
      refute body =~ "# service="
      assert content_type(hdrs) == "application/x-git-upload-pack-advertisement"
    end

    test "info/refs without the header is the v0 advertisement with the service banner" do
      {200, _, body} = http_get("/git/info/refs?service=git-upload-pack")

      assert "001e# service=git-upload-pack\n0000" <> rest = body
      assert rest =~ "refs/heads/main"
    end

    test "a real client negotiates v2 for a fetch" do
      dir = tmp("v2")
      git!(dir, ["init", "--quiet", "."])

      {out, 0} =
        git(dir, ["-c", "protocol.version=2", "fetch", base_url() <> "/git", "main"], [
          {"GIT_TRACE_PACKET", "1"}
        ])

      assert out =~ "version 2"
      assert out =~ "ls-refs"
    end

    test "receive-pack never advertises v2: a push client is downgraded to v0", %{
      keyid: keyid,
      priv: priv
    } do
      {200, _, body} =
        http_get("/git/info/refs?service=git-receive-pack", [
          {"git-protocol", "version=2"},
          {"authorization", basic(keyid, priv)}
        ])

      refute body =~ "version 2"
      assert body =~ "report-status"
    end
  end

  describe "big pushes and the feed" do
    @tag timeout: 120_000
    test "a 3 MiB push from a real client (chunked transfer) is accepted", %{
      keyid: keyid,
      priv: priv
    } do
      dir = clone!()
      before = State.head_seq()

      # Base64 of random bytes: incompressible, no `-` or `_`, so no secret pattern can match.
      body = :crypto.strong_rand_bytes(3 * 1024 * 1024) |> Base.encode64(padding: false)
      lines = body |> String.graphemes() |> Enum.chunk_every(76) |> Enum.map(&Enum.join/1)
      write_commit!(dir, "inbox/#{keyid}/big.md", Enum.join(lines, "\n") <> "\n", "big")

      out = git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
      refute out =~ "rejected"
      assert State.head_seq() == before + 1
      assert spool_files() == []
    end

    test "a Last-Event-ID past the head (journal rebuilt) gets one reset frame with the head" do
      head = State.head_seq()

      {:ok, sock} =
        :gen_tcp.connect({127, 0, 0, 1}, TacitServer.Config.get(:port), [:binary, active: false])

      :ok =
        :gen_tcp.send(
          sock,
          "GET /events HTTP/1.1\r\nHost: t\r\nLast-Event-ID: #{head + 500}\r\n\r\n"
        )

      data = recv_until(sock, "event: reset", "")
      :gen_tcp.close(sock)

      assert data =~ "event: reset\ndata: {\"head\":#{head}}\n"
      refute data =~ "head_moved"
    end
  end

  defp recv_until(sock, marker, acc) do
    if String.contains?(acc, marker) and String.contains?(acc, "}\n") do
      acc
    else
      {:ok, d} = :gen_tcp.recv(sock, 0, 5_000)
      recv_until(sock, marker, acc <> d)
    end
  end

  describe "concurrent pushes" do
    # Six git clients and a dozen pushes; a loaded runner needs more than the 60 s default.
    @tag timeout: 300_000
    test "the journal is one total order: a chain of old to new, one row per accepted push" do
      writers =
        for i <- 1..6 do
          {id, priv} = enroll!("#{Enum.at(~w(claude codex pi hermes grok dreamer), i - 1)}@race")
          {id, priv}
        end

      before = State.head_seq()
      pushes_each = 2

      results =
        writers
        |> Task.async_stream(
          fn {id, priv} ->
            dir = clone!()

            for n <- 1..pushes_each do
              push_until_accepted!(dir, id, priv, "race-#{n}")
            end

            id
          end,
          max_concurrency: 6,
          timeout: 120_000,
          ordered: false
        )
        |> Enum.map(fn {:ok, id} -> id end)

      assert Enum.sort(results) == writers |> Enum.map(&elem(&1, 0)) |> Enum.sort()

      rows = State.since(before, 1_000)
      assert length(rows) == length(writers) * pushes_each

      # Contiguous seq, and every row starts where the previous one ended:
      # the lease made the pushes a single line of history.
      assert Enum.map(rows, & &1.seq) == Enum.to_list((before + 1)..(before + length(rows)))

      rows
      |> Enum.chunk_every(2, 1, :discard)
      |> Enum.each(fn [a, b] -> assert a.new == b.old end)

      repo = Path.join(TacitServer.Config.get(:data), "store.git")
      assert List.last(rows).new == String.trim(git!(repo, ["rev-parse", "main"]))

      # Each writer's own pushes keep their order, and each file landed.
      for {id, _} <- writers do
        mine = Enum.filter(rows, &(&1.writer == id))
        assert length(mine) == pushes_each
      end

      tree = git!(repo, ["ls-tree", "-r", "--name-only", "main"])

      for {id, _} <- writers, n <- 1..pushes_each do
        assert tree =~ "inbox/#{id}/race-#{n}.md"
      end
    end
  end

  # A push that loses the race is refused as non-fast-forward (and says so);
  # fetch, rebase and try again, the way a real client would.
  defp push_until_accepted!(dir, id, priv, name, attempts \\ 40)

  defp push_until_accepted!(_dir, id, _priv, name, 0),
    do: flunk("#{id} could not push #{name} in time")

  defp push_until_accepted!(dir, id, priv, name, attempts) do
    unless File.exists?(Path.join(dir, "inbox/#{id}/#{name}.md")) do
      write_commit!(dir, "inbox/#{id}/#{name}.md", "#{name} by #{id}\n", name)
    end

    case git(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(id, priv)) do
      {_, 0} ->
        :ok

      {out, _} ->
        refute out =~ "may write only under", "unexpected hook refusal: #{out}"
        git!(dir, ["pull", "--quiet", "--rebase", "origin", "main"])
        push_until_accepted!(dir, id, priv, name, attempts - 1)
    end
  end

  defp content_type(hdrs) do
    hdrs |> Enum.find_value(fn {k, v} -> if k == ~c"content-type", do: to_string(v) end)
  end
end
