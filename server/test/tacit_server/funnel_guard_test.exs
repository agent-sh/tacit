defmodule TacitServer.FunnelGuardTest do
  @moduledoc """
  `deploy/funnel_guard.py`, which `deploy/funnel.sh on` runs on the node's
  `tailscale serve status --json` before it turns Funnel on: Funnel is per
  host:port, so any other handler on 443 would go public with it. Samples
  follow ipn.ServeConfig: TCP keyed by port, Web and AllowFunnel by host:port.
  """
  use ExUnit.Case, async: true

  @guard Path.expand("../../deploy/funnel_guard.py", __DIR__)
  @host "tacit.example.ts.net"
  @hp @host <> ":443"
  @target "http://127.0.0.1:7448"

  defp guard(json) do
    py = System.find_executable("python3") || flunk("python3 is needed for this test")

    path =
      Path.join(TacitServer.Config.get(:data), "serve-#{System.unique_integer([:positive])}.json")

    File.write!(path, json)

    try do
      System.cmd(
        "sh",
        ["-c", ~s(exec "$0" "$1" "$2" "$3" < "$4"), py, @guard, @hp, @target, path],
        stderr_to_stdout: true
      )
    after
      File.rm(path)
    end
  end

  test "an empty config (no output, or {}) is safe" do
    assert {"fresh\n", 0} = guard("")
    assert {"fresh\n", 0} = guard("{}")
  end

  test "the exact expected mapping is safe (a rerun)" do
    cfg = %{
      "TCP" => %{"443" => %{"HTTPS" => true}},
      "Web" => %{@hp => %{"Handlers" => %{"/" => %{"Proxy" => @target}}}},
      "AllowFunnel" => %{@hp => true}
    }

    assert {"already\n", 0} = guard(JSON.encode!(cfg))
  end

  test "another Web mount on host:443 is refused, also next to the tacit one" do
    other = %{
      "TCP" => %{"443" => %{"HTTPS" => true}},
      "Web" => %{@hp => %{"Handlers" => %{"/grafana" => %{"Proxy" => "http://127.0.0.1:3000"}}}}
    }

    assert {out, 1} = guard(JSON.encode!(other))
    assert out =~ "already serves"

    both =
      put_in(other, ["Web", @hp, "Handlers", "/"], %{"Proxy" => @target})

    assert {_, 1} = guard(JSON.encode!(both))

    root_elsewhere = %{
      "Web" => %{@hp => %{"Handlers" => %{"/" => %{"Proxy" => "http://127.0.0.1:9000"}}}}
    }

    assert {_, 1} = guard(JSON.encode!(root_elsewhere))
  end

  test "a raw TCP forward on 443 is refused" do
    cfg = %{"TCP" => %{"443" => %{"TCPForward" => "127.0.0.1:8443"}}}
    assert {out, 1} = guard(JSON.encode!(cfg))
    assert out =~ "forwards raw TCP"
  end

  test "a foreground serve session on 443 is refused" do
    cfg = %{
      "Foreground" => %{
        "sess1" => %{
          "TCP" => %{"443" => %{"HTTPS" => true}},
          "Web" => %{@hp => %{"Handlers" => %{"/" => %{"Proxy" => "http://127.0.0.1:5000"}}}}
        }
      }
    }

    assert {_, 1} = guard(JSON.encode!(cfg))
  end

  test "another port or another host on the node is not in the way" do
    cfg = %{
      "TCP" => %{"8443" => %{"HTTPS" => true}},
      "Web" => %{
        (@host <> ":8443") => %{"Handlers" => %{"/" => %{"Proxy" => "http://127.0.0.1:3000"}}}
      }
    }

    assert {"fresh\n", 0} = guard(JSON.encode!(cfg))
  end

  test "input that is not a serve config stops it" do
    assert {_, 2} = guard("not json")
    assert {_, 2} = guard("[]")
  end
end
