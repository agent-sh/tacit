defmodule TacitServer.Auth.WhoIs do
  @moduledoc """
  Peer identity from tailscaled's LocalAPI (`GET /localapi/v0/whois?addr=`),
  or a static map for tests. Returns `{:ok, %{login: ..., host: ..., tags: [...]}}`
  or `:error`. Cached by IP for 30 s in `TacitServer.Auth.Cache`.
  """

  @ttl_ms 30_000

  def lookup(ip) do
    case TacitServer.Auth.Cache.get({:whois, ip}) do
      {:ok, v} ->
        v

      :miss ->
        v = do_lookup(TacitServer.Config.get(:whois), ip)
        if v != :error, do: TacitServer.Auth.Cache.put({:whois, ip}, v, @ttl_ms)
        v
    end
  end

  defp do_lookup({:static, map}, ip) do
    case Map.fetch(map, ip) do
      {:ok, login} -> {:ok, %{login: login, host: "static", tags: []}}
      :error -> :error
    end
  end

  defp do_lookup(:localapi, ip) do
    sock = TacitServer.Config.get(:tailscaled)
    addr = ip |> :inet.ntoa() |> to_string()
    addr = if String.contains?(addr, ":"), do: "[" <> addr <> "]", else: addr

    with {:ok, s} <-
           :gen_tcp.connect({:local, sock}, 0, [:binary, active: false, packet: :raw], 2_000),
         :ok <-
           :gen_tcp.send(s, [
             "GET /localapi/v0/whois?addr=",
             addr,
             ":1 HTTP/1.0\r\nHost: local-tailscaled.sock\r\n\r\n"
           ]),
         resp <- recv_all(s, []),
         [head, body] <- :binary.split(resp, "\r\n\r\n"),
         true <- String.contains?(hd(String.split(head, "\r\n")), " 200"),
         {:ok, json} <- JSON.decode(body) do
      parse(json)
    else
      _ -> :error
    end
  end

  # A tagged node (a server owned by the tailnet, not by a user) comes back with
  # `"UserProfile": null`, and an older daemon may omit the key; the peer is
  # still a tailnet member, only its login is empty.
  defp parse(%{"Node" => %{} = node} = json) do
    host = node |> Map.get("ComputedName", node["Name"] || "") |> String.split(".") |> hd()
    login = (is_map(json["UserProfile"]) && json["UserProfile"]["LoginName"]) || ""
    {:ok, %{login: login, host: host, tags: node["Tags"] || []}}
  end

  defp parse(_), do: :error

  defp recv_all(s, acc) do
    case :gen_tcp.recv(s, 0, 2_000) do
      {:ok, data} ->
        recv_all(s, [acc | data])

      {:error, _} ->
        :gen_tcp.close(s)
        IO.iodata_to_binary(acc)
    end
  end
end
