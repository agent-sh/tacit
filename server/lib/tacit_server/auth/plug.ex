defmodule TacitServer.Auth.Plug do
  @moduledoc """
  Runs before any body is read. Two paths, told apart by the listener.

  **Tailnet** (the listener on the node's tailnet address):

  1. A request carrying `Tailscale-Funnel-Request` is 403 at once. tailscaled
     deletes that header from what clients send and sets it on every request
     that came in through Funnel (`addTailscaleIdentityHeaders` in tailscale's
     ipn/ipnlocal/serve.go), and Funnel is pointed only at the public
     listener, so here it is a misconfigured Funnel or a spoof. Either way it
     must not be taken for the node itself making a tailnet request.
  2. Peer must be a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48)
     with a WhoIs hit, or loopback when TACIT_ALLOW_LOOPBACK=1. Sets
     `conn.assigns.peer` to the WhoIs map. Anything else: 403.
  3. If Basic credentials are present, the user name is the key id and
     the password a `TacitServer.Auth.Token`. On success `conn.assigns.writer`
     is set. A bad token is 401; no credentials means read-only.

  **Public** (the listener on 127.0.0.1:`TACIT_PUBLIC_PORT`, the only
  address Funnel proxies to; `TacitServer.PublicRouter` marks its requests):
  there is no tailnet identity, so a `reader@<host>` key token is required on
  every route. Any other key (a writer's, the curator's) is refused: a private
  key copied off a harness machine must not become an internet-wide read
  credential. Every failure (no token, malformed, unknown key, not a reader,
  expired, bad signature, replay) gets the same 401 body, so an
  unauthenticated caller learns nothing about which key ids exist; the reason
  is logged here only. A request whose token verifies is always served. A
  source (`X-Forwarded-For`, which tailscaled overwrites with the Funnel
  client's address) that has failed `public_fail_limit` times this minute
  gets 429 instead of 401; the counters live in a table bounded at
  `public_fail_sources` entries. `conn.assigns.public` is true, and
  `TacitServer.Git.Http` refuses every push on it.
  """
  import Plug.Conn
  require Logger

  alias TacitServer.Auth.{Cache, KeyId, Keys, Token, WhoIs}
  alias TacitServer.Config

  @behaviour Plug

  @public_401 "unauthorized: the public path needs a reader key token"

  @impl true
  def init(opts), do: opts

  @impl true
  def call(conn, _opts) do
    cond do
      public?(conn) ->
        public(conn)

      get_req_header(conn, "tailscale-funnel-request") != [] ->
        Logger.warning(
          "Funnel request on the tailnet listener from #{:inet.ntoa(conn.remote_ip)}"
        )

        refuse(conn, 403, "Funnel requests are served by the public listener only")

      true ->
        tailnet(conn)
    end
  end

  @doc "A request that came in on the public listener."
  def public?(conn), do: conn.private[:tacit_public] == true

  @doc false
  def public_401, do: @public_401

  # ---- tailnet ------------------------------------------------------------

  defp tailnet(conn) do
    ip = conn.remote_ip

    case identify(ip) do
      {:ok, peer} ->
        conn = assign(conn, :peer, peer)

        case writer(conn, peer, to_string(:inet.ntoa(ip))) do
          {:ok, nil} ->
            conn

          {:ok, writer} ->
            assign(conn, :writer, writer)

          {:error, reason} ->
            conn
            |> put_resp_header("www-authenticate", ~s(Basic realm="tacit"))
            |> send_resp(401, "writer token rejected: #{reason}")
            |> halt()
        end

      :error ->
        Logger.warning("rejected peer #{:inet.ntoa(ip)}")
        conn |> send_resp(403, "tailnet only") |> halt()
    end
  end

  defp identify(ip) do
    cond do
      loopback?(ip) and Config.get(:allow_loopback) ->
        {:ok, %{login: "loopback", host: "loopback", tags: []}}

      tailnet?(ip) ->
        WhoIs.lookup(ip)

      true ->
        :error
    end
  end

  # ---- public -------------------------------------------------------------

  # The nonce is claimed under one constant peer for the whole public path:
  # git reuses one token for every request of a command, and a Funnel client
  # behind a NAT pool or switching between IPv4 and IPv6 can arrive from
  # another address mid-command. The token is still single-key and lives one
  # window; a token seen on the tailnet listener stays a replay here.
  @public_peer "public"

  defp public(conn) do
    src = source(conn)

    case writer(conn, :public, @public_peer) do
      {:ok, keyid} when is_binary(keyid) ->
        conn
        |> assign(:peer, %{login: keyid, host: "public", tags: []})
        |> assign(:writer, keyid)
        |> assign(:public, true)

      other ->
        reason =
          case other do
            {:error, r} -> r
            {:ok, nil} -> :no_token
          end

        Logger.info("public path refused #{src}: #{reason}")
        key = {src, div(System.os_time(:second), 60)}
        max = Config.get(:public_fail_sources)
        limit = Config.get(:public_fail_limit)

        case Cache.fails(key, max) do
          n when is_integer(n) and n < limit ->
            Cache.bump_fail(key, 120_000, max)

            conn
            |> put_resp_header("www-authenticate", ~s(Basic realm="tacit"))
            |> refuse(401, @public_401)

          _ ->
            conn
            |> put_resp_header("retry-after", "60")
            |> refuse(429, "too many failed requests; wait a minute")
        end
    end
  end

  # The body is never read on a refusal, so the connection is closed rather
  # than kept for a next request that would start inside this one's body.
  defp refuse(conn, status, msg) do
    conn
    |> put_resp_header("connection", "close")
    |> send_resp(status, msg)
    |> halt()
  end

  # The Funnel client's address, from the header tailscaled overwrites on
  # every proxied request (only the rate limit uses it); the socket peer when
  # it is absent.
  defp source(conn) do
    case get_req_header(conn, "x-forwarded-for") do
      [v | _] -> v |> String.split(",") |> List.first() |> String.trim() |> String.slice(0, 64)
      [] -> to_string(:inet.ntoa(conn.remote_ip))
    end
  end

  # ---- shared -------------------------------------------------------------

  defp writer(conn, peer, nonce_peer) do
    case get_req_header(conn, "authorization") do
      ["Basic " <> b64] ->
        with {:ok, creds} <- Base.decode64(b64),
             [keyid, token] <- String.split(creds, ":", parts: 2),
             {:ok, _harness, _host} <- KeyId.parse(keyid),
             true <- host_matches?(keyid, peer) || {:error, mismatch(peer)},
             {:ok, pub} <- Keys.public_key(keyid),
             {:ok, ^keyid} <-
               Token.verify(keyid, token, pub,
                 window: Config.get(:token_window_s),
                 peer: nonce_peer
               ) do
          {:ok, keyid}
        else
          {:error, r} -> {:error, r}
          :error -> {:error, :unknown_key}
          _ -> {:error, :malformed}
        end

      # No Basic credentials: anonymous. On the tailnet that reads; the
      # public path refuses it.
      _ ->
        {:ok, nil}
    end
  end

  # On the tailnet the key's host must be the node making the request;
  # loopback (dev) skips that. The public path has no node: only reader keys
  # are taken there, and the signature names the key.
  defp host_matches?(keyid, :public), do: KeyId.reader?(keyid)
  defp host_matches?(keyid, %{host: "loopback"}) when is_binary(keyid), do: true

  defp host_matches?(keyid, %{host: host}) do
    [_, h] = String.split(keyid, "@", parts: 2)
    String.downcase(host) == h
  end

  defp mismatch(:public), do: :not_reader
  defp mismatch(_), do: :host_mismatch

  defp loopback?({127, _, _, _}), do: true
  defp loopback?({0, 0, 0, 0, 0, 0, 0, 1}), do: true
  defp loopback?(_), do: false

  defp tailnet?({100, b, _, _}) when b >= 64 and b <= 127, do: true
  defp tailnet?({0xFD7A, 0x115C, 0xA1E0, _, _, _, _, _}), do: true
  defp tailnet?(_), do: false
end
