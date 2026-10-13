defmodule TacitServer.Config do
  @moduledoc """
  Runtime configuration. Read once at boot into `:persistent_term`
  (constant-time reads, never updated after boot).

  Environment:

    TACIT_DATA        state root: <data>/store.git (bare repo), <data>/state.db, <data>/spool, <data>/index
    TACIT_BIND        IP to bind, default the node's tailnet IPv4; `127.0.0.1` for local tests
    TACIT_PORT        default 7447
    TACIT_TAILSCALED  tailscaled LocalAPI socket, default /var/run/tailscale/tailscaled.sock
    TACIT_WHOIS       `localapi` (default) or `static:<ip>=<login>,<ip>=<login>` for tests
    TACIT_ALLOW_LOOPBACK  `1` lets 127.0.0.1 through the tailnet check (tests only)
    TACIT_PUBLIC_PORT  a second listener on 127.0.0.1 for the public path (Tailscale Funnel
                       proxies to it). Every request on it needs a key token and nothing can
                       push through it. Unset: no public listener.
  """

  @key {__MODULE__, :config}

  def load! do
    data = env("TACIT_DATA") || default_data()

    cfg = %{
      data: data,
      repo: Path.join(data, "store.git"),
      state_db: Path.join(data, "state.db"),
      spool: Path.join(data, "spool"),
      hooks: Application.app_dir(:tacit_server, "priv/hooks"),
      bind: parse_ip!(env("TACIT_BIND") || tailnet_ipv4() || "127.0.0.1"),
      port: String.to_integer(env("TACIT_PORT") || "7447"),
      tailscaled: env("TACIT_TAILSCALED") || "/var/run/tailscale/tailscaled.sock",
      whois: parse_whois(env("TACIT_WHOIS") || "localapi"),
      allow_loopback: env("TACIT_ALLOW_LOOPBACK") == "1",
      public_port: parse_port(env("TACIT_PUBLIC_PORT")),
      # Requests on the public path that fail authentication (no token or a
      # bad one), per source and minute, before the server answers 429
      # without looking at the token. A legitimate client fails about once per
      # git command (git asks without credentials first, then with the
      # helper's token): a sync per session start, a fetch per burst of
      # pushes, ls-remote every 5 minutes, so tens a minute is already a
      # client that is broken or not ours.
      public_fail_limit: 60,
      # Distinct failing sources counted at once. One counter is about 25
      # words of ETS, so 4096 bound the table near 1 MiB; past that every new
      # failing source is treated as over the limit (429), while any request
      # whose token verifies is still served.
      public_fail_sources: 4096,
      # A feed stream on the public path ends after this long with an
      # `event: reconnect` frame, and the follower reconnects with a fresh
      # token: auth runs again, so a revoked reader key stops receiving
      # journal rows within this time. 300 s is the follower's own
      # verification interval (one more small request per 5 minutes).
      public_stream_s: 300,
      max_push_bytes: 64 * 1024 * 1024,
      max_upload_request: 10 * 1024 * 1024,
      token_window_s: 60
    }

    :persistent_term.put(@key, cfg)
    cfg
  end

  # Application env (config/test.exs) wins over the process environment so
  # tests are hermetic; production sets only the process environment.
  defp env(name) do
    case Application.get_env(:tacit_server, :env, %{}) do
      %{^name => v} -> v
      _ -> System.get_env(name)
    end
  end

  def get, do: :persistent_term.get(@key)
  def get(k), do: Map.fetch!(get(), k)

  defp default_data do
    case System.get_env("XDG_STATE_HOME") do
      nil -> Path.expand("~/.local/state/tacit-server")
      x -> Path.join(x, "tacit-server")
    end
  end

  defp parse_ip!(s) do
    case :inet.parse_address(String.to_charlist(s)) do
      {:ok, ip} -> ip
      {:error, _} -> raise ArgumentError, "TACIT_BIND is not an IP: #{s}"
    end
  end

  defp parse_port(nil), do: nil
  defp parse_port(""), do: nil
  defp parse_port(s), do: String.to_integer(s)

  defp parse_whois("localapi"), do: :localapi

  defp parse_whois("static:" <> rest) do
    map =
      rest
      |> String.split(",", trim: true)
      |> Map.new(fn pair ->
        [ip, login] = String.split(pair, "=", parts: 2)
        {:ok, addr} = :inet.parse_address(String.to_charlist(ip))
        {addr, login}
      end)

    {:static, map}
  end

  # The tailnet IPv4 of this node, from `tailscale ip -4`; nil when absent.
  defp tailnet_ipv4 do
    case System.find_executable("tailscale") do
      nil ->
        nil

      bin ->
        case System.cmd(bin, ["ip", "-4"], stderr_to_stdout: true) do
          {out, 0} -> out |> String.trim() |> String.split("\n") |> List.first()
          _ -> nil
        end
    end
  end
end
