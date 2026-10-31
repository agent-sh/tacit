defmodule TacitServer.PublicTest do
  @moduledoc """
  The public path (Tailscale Funnel): the loopback listener on
  TACIT_PUBLIC_PORT. Every route needs a reader key token there, every
  failure looks the same, a verified token is always served, no key pushes
  through it, a public feed stream ends after a bounded time, and the tailnet
  listener refuses Funnel requests and otherwise answers as before.
  """
  use ExUnit.Case, async: false
  import TacitServer.TestHelpers
  alias TacitServer.Auth.{Cache, Keys, Plug}
  alias TacitServer.Config

  setup_all do
    {reader, rpriv} = enroll!("reader@host-b")
    {writer, wpriv} = enroll!("claude@pubtest")
    {dreamer, dpriv} = enroll!("dreamer@pubtest")
    {curator, cpriv} = enroll!("curator@pubtest")

    %{
      reader: reader,
      rpriv: rpriv,
      writer: writer,
      wpriv: wpriv,
      others: [{writer, wpriv}, {dreamer, dpriv}, {curator, cpriv}]
    }
  end

  # Every test starts with no failure counted. A test's source is one of 250
  # addresses, so two tests can draw the same one, and the failures one left
  # would reach the other: a 429 where it expects a 401. The module is not
  # async, so no other test counts failures meanwhile.
  setup do
    Cache.clear_fails()
    %{src: "203.0.113.#{rem(System.unique_integer([:positive]), 250) + 1}"}
  end

  defp public_port, do: Config.get(:public_port)
  defp tailnet_port, do: Config.get(:port)

  @routes [
    {"GET", "/healthz"},
    {"GET", "/journal?since=0"},
    {"GET", "/events"},
    {"GET", "/git/info/refs?service=git-upload-pack"},
    {"GET", "/git/info/refs?service=git-receive-pack"},
    {"POST", "/git/git-upload-pack"},
    {"POST", "/git/git-receive-pack"},
    {"GET", "/no-such-route"}
  ]

  # One request over a raw socket: {status, body}. For a stream (the feed),
  # reads until `until` shows up (or the server closes) and closes.
  defp req(port, method, path, headers, body \\ "", until \\ nil) do
    {:ok, sock} = :gen_tcp.connect({127, 0, 0, 1}, port, [:binary, active: false], 2_000)

    head =
      [
        method,
        " ",
        path,
        " HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Length: ",
        Integer.to_string(byte_size(body)),
        "\r\n"
      ] ++ for({k, v} <- headers, do: [k, ": ", v, "\r\n"]) ++ ["\r\n", body]

    :ok = :gen_tcp.send(sock, head)
    resp = recv(sock, "", until)
    :gen_tcp.close(sock)
    "HTTP/1.1 " <> <<code::binary-size(3), rest::binary>> = resp
    [_, payload] = :binary.split(rest, "\r\n\r\n")
    {String.to_integer(code), payload}
  end

  defp recv(sock, acc, until) do
    if until && String.contains?(acc, until) do
      acc
    else
      case :gen_tcp.recv(sock, 0, 5_000) do
        {:ok, d} -> recv(sock, acc <> d, until)
        {:error, :closed} -> acc
      end
    end
  end

  defp content_type("/git/git-upload-pack"),
    do: [{"Content-Type", "application/x-git-upload-pack-request"}]

  defp content_type("/git/git-receive-pack"),
    do: [{"Content-Type", "application/x-git-receive-pack-request"}]

  defp content_type(_), do: []

  defp main_sha,
    do: git!(Path.join(Config.get(:data), "store.git"), ["rev-parse", "main"]) |> String.trim()

  defp with_config(changes, fun) do
    cfg = Config.get()
    key = {Config, :config}
    :persistent_term.put(key, Map.merge(cfg, changes))

    try do
      fun.()
    after
      :persistent_term.put(key, cfg)
    end
  end

  describe "refusals" do
    test "no token: every route on the public listener is 401 before any work", %{src: src} do
      before = TacitServer.State.head_seq()

      for {method, path} <- @routes do
        {status, body} =
          req(
            public_port(),
            method,
            path,
            [{"X-Forwarded-For", src} | content_type(path)],
            "0000"
          )

        assert status == 401, "#{method} #{path} answered #{status}"
        assert body == Plug.public_401()
      end

      assert TacitServer.State.head_seq() == before
      assert spool_files() == []
    end

    test "writer, dreamer and curator keys are refused on every route; a reader is served", %{
      others: others,
      reader: reader,
      rpriv: rpriv,
      src: src
    } do
      for {keyid, priv} <- others, {method, path} <- @routes do
        {status, body} =
          req(
            public_port(),
            method,
            path,
            [
              {"X-Forwarded-For", src},
              {"Authorization", basic(keyid, priv)} | content_type(path)
            ],
            "0000"
          )

        assert {status, body} == {401, Plug.public_401()}, "#{keyid} #{method} #{path}"
      end

      assert {200, _} =
               req(public_port(), "GET", "/healthz", [{"Authorization", basic(reader, rpriv)}])
    end

    test "every failure has the same body: no oracle for which key ids exist", %{
      reader: reader,
      rpriv: rpriv,
      writer: writer,
      wpriv: wpriv,
      src: src
    } do
      {_, other_priv, _} = keypair("reader@host-b")
      expired = TacitServer.Auth.Token.mint(reader, rpriv, System.os_time(:second) - 3600)

      creds = [
        {"malformed", "Basic " <> Base.encode64("reader@probe:x.y.z")},
        {"not base64", "Basic %%%"},
        {"bad key id", "Basic " <> Base.encode64("Not A Key:x.y.z")},
        {"unknown id",
         "Basic " <>
           Base.encode64(
             "reader@nosuchhost:" <> TacitServer.Auth.Token.mint("reader@nosuchhost", rpriv)
           )},
        {"expired", "Basic " <> Base.encode64(reader <> ":" <> expired)},
        {"bad signature",
         "Basic " <>
           Base.encode64(reader <> ":" <> TacitServer.Auth.Token.mint(reader, other_priv))},
        {"writer key", basic(writer, wpriv)},
        {"not basic", "Bearer abc"}
      ]

      for {what, auth} <- creds do
        assert {401, Plug.public_401()} ==
                 req(public_port(), "GET", "/healthz", [
                   {"X-Forwarded-For", src},
                   {"Authorization", auth}
                 ]),
               what
      end
    end

    test "unknown key ids are answered from the cached set of enrolled ids; a new key works at once",
         %{src: src} do
      assert Keys.public_key("reader@probe-#{System.unique_integer([:positive])}") == :error
      assert {:ok, set} = Cache.get(:enrolled_keys)
      assert MapSet.member?(set, "reader@host-b")

      {fresh, fpriv} = enroll!("reader@fresh#{System.unique_integer([:positive])}")
      assert Cache.get(:enrolled_keys) == :miss

      assert {200, _} =
               req(public_port(), "GET", "/healthz", [
                 {"X-Forwarded-For", src},
                 {"Authorization", basic(fresh, fpriv)}
               ])
    end
  end

  test "the client's authcheck knows the public 401 body" do
    go = File.read!(Path.expand("../../../client/cmd/tacit/authcheck.go", __DIR__))
    [_, phrase] = Regex.run(~r/publicRefusal = "([^"]+)"/, go)
    assert Plug.public_401() =~ phrase
  end

  test "a failed key listing caches nothing and does not reject an enrolled key", %{
    reader: reader
  } do
    Cache.delete(:enrolled_keys)
    Cache.delete({:key, reader})
    failing = fn -> :error end

    assert Keys.enrolled(failing) == :error
    assert Cache.get(:enrolled_keys) == :miss
    assert {:ok, _} = Keys.public_key(reader, failing)
    assert Cache.get(:enrolled_keys) == :miss
    # An unknown id is still refused, by the per-key lookup.
    assert Keys.public_key("reader@never-enrolled", failing) == :error
    # The next good listing is cached as before.
    assert {:ok, set} = Keys.enrolled()
    assert MapSet.member?(set, reader)
    assert {:ok, ^set} = Cache.get(:enrolled_keys)
  end

  describe "the rate limit" do
    test "past the limit a source gets 429 on failure, but a verified token is still served", %{
      reader: reader,
      rpriv: rpriv,
      src: src
    } do
      with_config(%{public_fail_limit: 3}, fn ->
        for _ <- 1..3 do
          assert {401, _} = req(public_port(), "GET", "/healthz", [{"X-Forwarded-For", src}])
        end

        {429, body} = req(public_port(), "GET", "/healthz", [{"X-Forwarded-For", src}])
        assert body =~ "too many failed requests"

        assert {200, _} =
                 req(public_port(), "GET", "/healthz", [
                   {"X-Forwarded-For", src},
                   {"Authorization", basic(reader, rpriv)}
                 ])
      end)
    end

    test "the counter table is bounded: a new source past the bound is 429 on failure, 200 with a token",
         %{reader: reader, rpriv: rpriv} do
      Cache.clear_fails()

      with_config(%{public_fail_sources: 2}, fn ->
        for s <- ["198.51.100.1", "198.51.100.2"] do
          assert {401, _} = req(public_port(), "GET", "/healthz", [{"X-Forwarded-For", s}])
        end

        assert {429, _} =
                 req(public_port(), "GET", "/healthz", [{"X-Forwarded-For", "198.51.100.3"}])

        assert {200, _} =
                 req(public_port(), "GET", "/healthz", [
                   {"X-Forwarded-For", "198.51.100.3"},
                   {"Authorization", basic(reader, rpriv)}
                 ])

        assert :ets.info(:tacit_public_fails, :size) == 2
      end)

      Cache.clear_fails()
    end
  end

  describe "the tailnet listener" do
    test "a Funnel request there is 403 at once, on every route", %{src: src} do
      for {method, path} <- @routes do
        {status, _} =
          req(
            tailnet_port(),
            method,
            path,
            [{"Tailscale-Funnel-Request", "?1"}, {"X-Forwarded-For", src} | content_type(path)],
            "0000"
          )

        assert status == 403, "#{method} #{path} answered #{status}"
      end
    end

    test "spoofed Funnel requests there cannot lock a source out of the public path", %{
      reader: reader,
      rpriv: rpriv,
      src: src
    } do
      with_config(%{public_fail_limit: 3}, fn ->
        for _ <- 1..10 do
          assert {403, _} =
                   req(tailnet_port(), "GET", "/healthz", [
                     {"Tailscale-Funnel-Request", "?1"},
                     {"X-Forwarded-For", src}
                   ])
        end

        assert {401, _} = req(public_port(), "GET", "/healthz", [{"X-Forwarded-For", src}])

        assert {200, _} =
                 req(public_port(), "GET", "/healthz", [
                   {"X-Forwarded-For", src},
                   {"Authorization", basic(reader, rpriv)}
                 ])
      end)
    end

    test "an X-Forwarded-For there changes nothing: still a tailnet request", %{
      writer: writer,
      wpriv: wpriv
    } do
      h = [{"X-Forwarded-For", "192.0.2.77"}]
      assert {200, _} = req(tailnet_port(), "GET", "/healthz", h)
      assert {200, _} = req(tailnet_port(), "GET", "/journal?since=0", h)

      dir = clone!()
      write_commit!(dir, "inbox/#{writer}/tailnet.md", "x\n", "tailnet push")
      git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(writer, wpriv))
      assert main_sha() == git!(dir, ["rev-parse", "HEAD"]) |> String.trim()
    end
  end

  describe "a reader token" do
    test "reads healthz, the journal and the feed", %{reader: reader, rpriv: rpriv, src: src} do
      h = fn -> [{"X-Forwarded-For", src}, {"Authorization", basic(reader, rpriv)}] end

      {200, body} = req(public_port(), "GET", "/healthz", h.())
      assert JSON.decode!(body)["peer"] == reader

      {200, body} = req(public_port(), "GET", "/journal?since=0", h.())
      assert is_list(JSON.decode!(body))

      {200, body} = req(public_port(), "GET", "/events", h.(), "", "connected head=")
      assert body =~ ": connected head="
    end

    test "is not tied to the source address, but stays one key's and is a replay elsewhere", %{
      reader: reader,
      rpriv: rpriv
    } do
      auth = basic(reader, rpriv)

      for s <- ["203.0.113.200", "2001:db8::1", "198.51.100.50"] do
        assert {200, _} =
                 req(public_port(), "GET", "/healthz", [
                   {"X-Forwarded-For", s},
                   {"Authorization", auth}
                 ])
      end

      # A token first used on the tailnet listener is a replay on the public one.
      tok = basic(reader, rpriv)
      assert {200, _} = req(tailnet_port(), "GET", "/healthz", [{"Authorization", tok}])
      assert {401, _} = req(public_port(), "GET", "/healthz", [{"Authorization", tok}])
      # A fresh token works from any address.
      assert {200, _} =
               req(public_port(), "GET", "/healthz", [
                 {"X-Forwarded-For", "192.0.2.9"},
                 {"Authorization", basic(reader, rpriv)}
               ])
    end

    test "clones and fetches with git", %{reader: reader, rpriv: rpriv} do
      dir = tmp("pubclone")
      url = "http://127.0.0.1:#{public_port()}/git"
      git!(dir, ["clone", "--quiet", url, "."], git_env(reader, rpriv))
      assert File.exists?(Path.join(dir, "keys/reader@host-b.pub"))
      git!(dir, ["fetch", "--quiet", "origin", "main"], git_env(reader, rpriv))

      {out, code} = git(dir, ["fetch", "--quiet", "origin", "main"])
      assert code != 0
      assert out =~ "401" or out =~ "Authentication failed" or out =~ "could not read Username"
    end

    test "never pushes, on the tailnet or the public path", %{reader: reader, rpriv: rpriv} do
      before = main_sha()

      for port <- [tailnet_port(), public_port()] do
        {403, body} =
          req(port, "GET", "/git/info/refs?service=git-receive-pack", [
            {"Authorization", basic(reader, rpriv)}
          ])

        assert body =~ "cannot push" or body =~ "public path is read-only"

        {403, _} =
          req(
            port,
            "POST",
            "/git/git-receive-pack",
            [{"Authorization", basic(reader, rpriv)} | content_type("/git/git-receive-pack")],
            "0000"
          )
      end

      dir = clone!()
      write_commit!(dir, "inbox/reader@host-b/x.md", "x\n", "reader push")
      {out, code} = git(dir, ["push", "origin", "HEAD:main"], git_env(reader, rpriv))
      assert code != 0
      assert out =~ "403"
      assert main_sha() == before
      assert spool_files() == []
    end

    test "the feed ends after the public stream lifetime with one reconnect frame", %{
      reader: reader,
      rpriv: rpriv
    } do
      with_config(%{public_stream_s: 1}, fn ->
        t0 = System.monotonic_time(:millisecond)

        {200, body} =
          req(public_port(), "GET", "/events", [{"Authorization", basic(reader, rpriv)}])

        elapsed = System.monotonic_time(:millisecond) - t0
        assert body =~ ": connected head="
        assert body =~ "event: reconnect\ndata: {}\n\n"
        assert elapsed >= 900 and elapsed < 5_000
      end)
    end
  end

  describe "pushes through the public path" do
    test "no key pushes: a writer cannot even authenticate, real git too", %{
      writer: writer,
      wpriv: wpriv
    } do
      before = main_sha()
      url = "http://127.0.0.1:#{public_port()}/git"
      dir = clone!()
      git!(dir, ["remote", "set-url", "origin", url])
      write_commit!(dir, "inbox/#{writer}/pub.md", "x\n", "public push")
      {out, code} = git(dir, ["push", "origin", "HEAD:main"], git_env(writer, wpriv))
      assert code != 0
      assert out =~ "401" or out =~ "Authentication failed"
      assert main_sha() == before
    end
  end
end
