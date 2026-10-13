defmodule TacitServer.WhoIsTest do
  @moduledoc """
  The peer-identity path through the real plug and the real LocalAPI client,
  against a fake tailscaled on a unix socket: IPv6 tailnet peers, tagged nodes
  (`"UserProfile": null`), the key id's host against the peer's node name, and
  addresses outside the tailnet ranges.
  """
  use ExUnit.Case, async: false
  import Plug.Conn, only: [put_req_header: 3]
  import TacitServer.TestHelpers

  setup_all do
    {keyid, priv} = enroll!("claude@host-a")
    %{keyid: keyid, priv: priv}
  end

  defp whois_json(name, login, tags \\ nil) do
    profile =
      if login,
        do: %{"ID" => 7, "LoginName" => login, "DisplayName" => "Someone"},
        else: nil

    JSON.encode!(%{
      "Node" => %{
        "ID" => 1,
        "Name" => name <> ".tail1234.ts.net.",
        "ComputedName" => name,
        "Tags" => tags
      },
      "UserProfile" => profile,
      "CapMap" => %{}
    })
  end

  defp get(ip, headers \\ []) do
    conn = Plug.Test.conn(:get, "/healthz") |> Map.put(:remote_ip, ip)
    conn = Enum.reduce(headers, conn, fn {k, v}, c -> put_req_header(c, k, v) end)
    TacitServer.Router.call(conn, TacitServer.Router.init([]))
  end

  test "an IPv6 tailnet peer is looked up as [addr]:1 and identified" do
    ip = {0xFD7A, 0x115C, 0xA1E0, 0, 0, 0, 0, 0x2A}

    {conn, asked} =
      with_fake_localapi(
        %{"[fd7a:115c:a1e0::2a]:1" => {200, whois_json("host-b", "owner@example.com")}},
        fn -> get(ip) end
      )

    assert conn.status == 200
    assert JSON.decode!(conn.resp_body)["peer"] == "owner@example.com"
    assert asked == ["[fd7a:115c:a1e0::2a]:1"]
  end

  test "a tagged node (UserProfile null) is a tailnet peer with an empty login" do
    ip = {100, 100, 0, 7}

    {conn, asked} =
      with_fake_localapi(
        %{"100.100.0.7:1" => {200, whois_json("ci-box", nil, ["tag:ci"])}},
        fn -> get(ip) end
      )

    assert conn.status == 200
    assert JSON.decode!(conn.resp_body)["peer"] == ""
    assert asked == ["100.100.0.7:1"]
  end

  test "a tagged node whose host matches the key id can write", %{keyid: _} do
    {keyid, priv} = enroll!("human@ci-box")
    ip = {100, 100, 0, 8}

    {conn, _} =
      with_fake_localapi(
        %{"100.100.0.8:1" => {200, whois_json("ci-box", nil, ["tag:ci"])}},
        fn -> get(ip, [{"authorization", basic(keyid, priv)}]) end
      )

    assert conn.status == 200
  end

  test "a token for claude@host-a from a peer whose node is called host-b is 401 host_mismatch",
       %{
         keyid: keyid,
         priv: priv
       } do
    {conn, _} =
      with_fake_localapi(
        %{
          "100.100.0.9:1" => {200, whois_json("host-b", "owner@example.com")},
          "100.100.0.10:1" => {200, whois_json("host-a", "owner@example.com")}
        },
        fn ->
          auth = [{"authorization", basic(keyid, priv)}]
          %{wrong: get({100, 100, 0, 9}, auth), right: get({100, 100, 0, 10}, auth)}
        end
      )

    assert conn.wrong.status == 401
    assert conn.wrong.resp_body =~ "host_mismatch"
    assert conn.right.status == 200
  end

  test "a daemon answer that is not JSON, or has no Node, is 403 and not a crash" do
    {conn, _} =
      with_fake_localapi(
        %{
          "100.100.1.1:1" => {200, "<html>nope</html>"},
          "100.100.1.2:1" => {200, JSON.encode!(%{"UserProfile" => nil})},
          "100.100.1.3:1" => {404, "no such peer"}
        },
        fn -> for(d <- 1..3, do: get({100, 100, 1, d})) end
      )

    assert Enum.map(conn, & &1.status) == [403, 403, 403]
  end

  test "addresses just outside the tailnet ranges are refused without asking the daemon" do
    outside = [
      {100, 63, 255, 255},
      {100, 128, 0, 0},
      {10, 0, 0, 1},
      {0xFD7A, 0x115C, 0xA1E1, 0, 0, 0, 0, 1},
      {0x2001, 0xDB8, 0, 0, 0, 0, 0, 1}
    ]

    {statuses, asked} =
      with_fake_localapi(%{}, fn -> for ip <- outside, do: get(ip).status end)

    assert statuses == List.duplicate(403, length(outside))
    assert asked == []
  end

  test "the edges inside the ranges are asked about", %{} do
    {statuses, asked} =
      with_fake_localapi(
        %{
          "100.64.0.0:1" => {200, whois_json("a", "x@y")},
          "100.127.255.255:1" => {200, whois_json("b", "x@y")},
          "[fd7a:115c:a1e0:ffff:ffff:ffff:ffff:ffff]:1" => {200, whois_json("c", "x@y")}
        },
        fn ->
          for ip <- [
                {100, 64, 0, 0},
                {100, 127, 255, 255},
                {0xFD7A, 0x115C, 0xA1E0, 0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}
              ],
              do: get(ip).status
        end
      )

    assert statuses == [200, 200, 200]
    assert length(asked) == 3
  end
end
