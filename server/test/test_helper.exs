ExUnit.start()

defmodule TacitServer.TestHelpers do
  @moduledoc false
  alias TacitServer.Auth.{Keys, Token}

  def base_url, do: "http://127.0.0.1:#{TacitServer.Config.get(:port)}"

  @doc "Generate an Ed25519 key pair; returns {keyid, priv, pub_openssh_text}."
  def keypair(keyid) do
    {pub, priv} = :crypto.generate_key(:eddsa, :ed25519)
    blob = <<0, 0, 0, 11, "ssh-ed25519", 0, 0, 0, 32, pub::binary>>
    {keyid, priv, "ssh-ed25519 " <> Base.encode64(blob) <> " " <> keyid}
  end

  @doc "Generate a key pair for keyid and enroll it; returns {keyid, priv}."
  def enroll!(keyid) do
    {keyid, priv, pub} = keypair(keyid)
    Keys.enroll(keyid, pub)
    {keyid, priv}
  end

  # Git must not see the developer's global config: a global credential
  # helper would store the first test token and replay it for later tests.
  @isolated [
    {"GIT_CONFIG_GLOBAL", "/dev/null"},
    {"GIT_CONFIG_NOSYSTEM", "1"},
    {"GIT_TERMINAL_PROMPT", "0"}
  ]

  @doc "A git env that authenticates as keyid with a fresh token on every request."
  def git_env(keyid, priv) do
    helper = Path.join(TacitServer.Config.get(:data), "cred-#{keyid}.sh")
    token = Token.mint(keyid, priv)
    File.write!(helper, "#!/bin/sh\necho username=#{keyid}\necho password=#{token}\n")
    File.chmod!(helper, 0o700)

    [
      {"GIT_CONFIG_COUNT", "1"},
      {"GIT_CONFIG_KEY_0", "credential.helper"},
      {"GIT_CONFIG_VALUE_0", helper}
    ]
  end

  @doc "An Authorization header value for keyid with a fresh token."
  def basic(keyid, priv),
    do: "Basic " <> Base.encode64(keyid <> ":" <> Token.mint(keyid, priv))

  def git!(dir, args, env \\ []) do
    {out, 0} =
      System.cmd("git", ["-C", dir | args], stderr_to_stdout: true, env: @isolated ++ env)

    out
  end

  def git(dir, args, env \\ []),
    do: System.cmd("git", ["-C", dir | args], stderr_to_stdout: true, env: @isolated ++ env)

  def tmp(name) do
    d =
      Path.join(TacitServer.Config.get(:data), "t-#{name}-#{System.unique_integer([:positive])}")

    File.mkdir_p!(d)
    d
  end

  @doc "Clone the store over HTTP into a fresh directory with a committer identity."
  def clone!(env \\ []) do
    dir = tmp("clone")
    git!(dir, ["clone", "--quiet", base_url() <> "/git", "."], env)
    git!(dir, ["config", "user.email", "t@test"])
    git!(dir, ["config", "user.name", "t"])
    dir
  end

  def write_commit!(dir, rel, content, msg) do
    path = Path.join(dir, rel)
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, content)
    git!(dir, ["add", "-A"])
    git!(dir, ["commit", "--quiet", "-m", msg])
  end

  def http_get(path, headers \\ []) do
    {:ok, {{_, status, _}, hdrs, body}} =
      :httpc.request(
        :get,
        {String.to_charlist(base_url() <> path),
         for({k, v} <- headers, do: {String.to_charlist(k), String.to_charlist(v)})},
        [timeout: 5_000],
        body_format: :binary
      )

    {status, hdrs, body}
  end

  @doc "POST a body with :httpc; headers is a list of {name, value} strings."
  def http_post(path, headers, content_type, body) do
    {:ok, {{_, status, _}, hdrs, resp}} =
      :httpc.request(
        :post,
        {String.to_charlist(base_url() <> path),
         for({k, v} <- headers, do: {String.to_charlist(k), String.to_charlist(v)}),
         String.to_charlist(content_type), body},
        [timeout: 60_000],
        body_format: :binary
      )

    {status, hdrs, resp}
  end

  @doc """
  POST `length` bytes of zeros over a raw socket without building the body in
  memory. The server may answer (413) before it has read everything, so send
  errors are ignored and the response is read regardless. Returns the status.
  """
  def raw_post_zeros(path, headers, length) do
    {:ok, sock} =
      :gen_tcp.connect({127, 0, 0, 1}, TacitServer.Config.get(:port), [:binary, active: false])

    head =
      [
        "POST ",
        path,
        " HTTP/1.1\r\nHost: t\r\nContent-Length: ",
        Integer.to_string(length),
        "\r\n"
      ] ++
        for({k, v} <- headers, do: [k, ": ", v, "\r\n"]) ++ ["\r\n"]

    chunk = :binary.copy(<<0>>, 1_048_576)

    with :ok <- :gen_tcp.send(sock, head) do
      send_zeros(sock, chunk, length)
    end

    status =
      case :gen_tcp.recv(sock, 0, 10_000) do
        {:ok, "HTTP/1.1 " <> <<code::binary-size(3), _::binary>>} -> String.to_integer(code)
        other -> {:no_response, other}
      end

    :gen_tcp.close(sock)
    status
  end

  defp send_zeros(_sock, _chunk, left) when left <= 0, do: :ok

  defp send_zeros(sock, chunk, left) do
    piece = if left >= byte_size(chunk), do: chunk, else: binary_part(chunk, 0, left)

    case :gen_tcp.send(sock, piece) do
      :ok -> send_zeros(sock, chunk, left - byte_size(piece))
      {:error, _} -> :ok
    end
  end

  def pkt(s) do
    String.pad_leading(String.downcase(Integer.to_string(byte_size(s) + 4, 16)), 4, "0") <> s
  end

  @doc "Files in the spool directory (in-flight pushes and enrolments)."
  def spool_files, do: File.ls!(TacitServer.Config.get(:spool)) |> Enum.sort()

  @doc """
  Run fun with the tailscaled LocalAPI replaced by a fake that answers
  `GET /localapi/v0/whois?addr=...` from `answers`, a map of the request's
  `addr` value (for example `"[fd7a:115c:a1e0::5]:1"`) to a `{status, body}`
  tuple. Returns what fun returns plus the list of addr values the server asked
  for, in order.
  """
  def with_fake_localapi(answers, fun) do
    # sun_path holds ~107 bytes, far less than the test data dir path, so the
    # socket goes in the per-user runtime dir (a tmpfs meant for sockets), else
    # in ~/.cache.
    dir =
      System.get_env("XDG_RUNTIME_DIR") || Path.join(System.user_home!(), ".cache")

    File.mkdir_p!(dir)

    sock =
      Path.join(dir, "tacit-test-ts-#{System.pid()}-#{System.unique_integer([:positive])}.sock")

    {:ok, listen} =
      :gen_tcp.listen(0, [:binary, :local, {:ifaddr, {:local, sock}}, active: false])

    {:ok, log} = Agent.start_link(fn -> [] end)
    acceptor = spawn_link(fn -> accept_loop(listen, answers, log) end)

    cfg = TacitServer.Config.get()
    key = {TacitServer.Config, :config}
    :persistent_term.put(key, %{cfg | whois: :localapi, tailscaled: sock})

    try do
      result = fun.()
      {result, Agent.get(log, &Enum.reverse/1)}
    after
      :persistent_term.put(key, cfg)
      Process.unlink(acceptor)
      Process.exit(acceptor, :kill)
      :gen_tcp.close(listen)
      Agent.stop(log)
      File.rm(sock)
    end
  end

  defp accept_loop(listen, answers, log) do
    case :gen_tcp.accept(listen) do
      {:ok, s} ->
        pid = spawn(fn -> serve(s, answers, log) end)
        :gen_tcp.controlling_process(s, pid)
        accept_loop(listen, answers, log)

      {:error, _} ->
        :ok
    end
  end

  defp serve(s, answers, log) do
    {:ok, req} = recv_head(s, "")
    [line | _] = String.split(req, "\r\n")
    ["GET", target, _] = String.split(line, " ")
    "/localapi/v0/whois?addr=" <> addr = target
    Agent.update(log, &[addr | &1])
    {status, body} = Map.get(answers, addr, {404, "no such peer"})

    :gen_tcp.send(s, [
      "HTTP/1.0 #{status} X\r\nContent-Type: application/json\r\n\r\n",
      body
    ])

    :gen_tcp.close(s)
  end

  defp recv_head(s, acc) do
    if String.contains?(acc, "\r\n\r\n") do
      {:ok, acc}
    else
      {:ok, d} = :gen_tcp.recv(s, 0, 2_000)
      recv_head(s, acc <> d)
    end
  end
end

:inets.start()

# Each run gets its own data dir (.test-data/<os pid>); remove it when the suite
# passed, keep it for inspection when something failed.
ExUnit.after_suite(fn
  %{failures: 0} -> File.rm_rf(TacitServer.Config.get(:data))
  _ -> :ok
end)
