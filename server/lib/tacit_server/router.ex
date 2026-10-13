defmodule TacitServer.Router do
  @moduledoc false
  use Plug.Router

  alias TacitServer.{Feed, State}
  alias TacitServer.Git.Http, as: GitHttp

  plug(Plug.Logger, log: :debug)
  plug(:match)
  plug(Plug.Parsers, parsers: [], pass: ["*/*"])
  plug(TacitServer.Auth.Plug)
  plug(:dispatch)

  get "/healthz" do
    body = JSON.encode!(%{ok: true, head_seq: State.head_seq(), peer: conn.assigns.peer.login})
    conn |> put_resp_content_type("application/json") |> send_resp(200, body)
  end

  get("/git/info/refs", do: conn |> fetch_query_params() |> GitHttp.info_refs())
  post("/git/git-upload-pack", do: GitHttp.upload_pack(conn))
  post("/git/git-receive-pack", do: GitHttp.receive_pack(conn))

  get("/events", do: conn |> fetch_query_params() |> Feed.stream())

  get "/journal" do
    conn = fetch_query_params(conn)

    since =
      case Integer.parse(conn.query_params["since"] || "0") do
        {n, _} when n >= 0 -> n
        _ -> 0
      end

    body = JSON.encode!(State.since(since, 500))
    conn |> put_resp_content_type("application/json") |> send_resp(200, body)
  end

  match _ do
    send_resp(conn, 404, "not found")
  end
end
