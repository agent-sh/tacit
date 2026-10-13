defmodule TacitServer.PublicRouter do
  @moduledoc """
  The public listener (`TACIT_PUBLIC_PORT`, bound to 127.0.0.1 only) that
  Tailscale Funnel proxies to. It is `TacitServer.Router` with every request
  marked public, so `TacitServer.Auth.Plug` asks a key token on every route
  and `TacitServer.Git.Http` refuses every push. The tailnet listener and its
  rules are unchanged.
  """
  @behaviour Plug

  @impl true
  def init(opts), do: TacitServer.Router.init(opts)

  @impl true
  def call(conn, opts) do
    conn
    |> Plug.Conn.put_private(:tacit_public, true)
    |> TacitServer.Router.call(opts)
  end
end
