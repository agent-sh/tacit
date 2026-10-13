defmodule TacitServer.Auth.KeyIdTest do
  @moduledoc """
  The harness allow-list: one list on the server, kept equal to the client's
  (`client/internal/fact/fact.go`) and to SPEC.md by the sync tests below.
  Enrolment and every authenticated request refuse a key id whose harness is
  not on it.
  """
  use ExUnit.Case, async: false
  import Plug.Conn, only: [put_req_header: 3]
  import TacitServer.TestHelpers
  alias TacitServer.Auth.{KeyId, Keys}

  @root Path.expand("../../..", __DIR__)

  describe "parse/1" do
    test "accepts every listed harness" do
      for h <- KeyId.harnesses() do
        assert {:ok, ^h, "host-a"} = KeyId.parse("#{h}@host-a")
      end
    end

    test "refuses an unknown harness, with the harness named in the reason" do
      assert {:error, :unknown_harness} = KeyId.parse("evil@host-a")
      assert {:error, :unknown_harness} = KeyId.parse("claude-code@host-a")
      assert {:error, :unknown_harness} = KeyId.parse("ghost@host-a")
    end

    test "refuses anything that is not <slug>@<slug>" do
      for bad <- [
            "",
            "claude",
            "claude@",
            "@host-a",
            "a@b@c",
            "Claude@host-a",
            "claude@RIG",
            "claude@host-a\n",
            "claude@host-a/../x",
            "claude@ri.g",
            nil,
            7,
            :claude
          ] do
        assert {:error, :bad_keyid} = KeyId.parse(bad), "should refuse #{inspect(bad)}"
      end
    end
  end

  describe "sync with the client and the spec" do
    test "equals Harnesses in client/internal/fact/fact.go" do
      go = File.read!(Path.join(@root, "client/internal/fact/fact.go"))
      [_, args] = Regex.run(~r/\bHarnesses\s*=\s*set\(([^)]*)\)/, go)
      client = ~r/"([a-z0-9-]+)"/ |> Regex.scan(args, capture: :all_but_first) |> List.flatten()

      assert Enum.sort(client) == Enum.sort(KeyId.harnesses()),
             "update @harnesses in lib/tacit_server/auth/key_id.ex and Harnesses in fact.go together"
    end

    test "equals the list in SPEC.md" do
      spec = File.read!(Path.join(@root, "SPEC.md"))
      [_, line] = Regex.run(~r/`<harness>` is one of ([^\n]*)\./, spec)
      listed = ~r/`([a-z0-9-]+)`/ |> Regex.scan(line, capture: :all_but_first) |> List.flatten()

      assert Enum.sort(listed) == Enum.sort(KeyId.harnesses())
    end
  end

  describe "enroll/2" do
    test "refuses an unknown harness and commits nothing" do
      {id, _priv, pub} = keypair("evil@host-a")
      before_seq = TacitServer.State.head_seq()

      assert_raise ArgumentError,
                   ~r/unknown harness in "evil@host-a"; allowed: claude, codex/,
                   fn ->
                     Keys.enroll(id, pub)
                   end

      assert TacitServer.State.head_seq() == before_seq
      assert Keys.public_key(id) == :error
    end

    test "refuses a malformed key id" do
      {_, _, pub} = keypair("claude@host-a")
      assert_raise ArgumentError, ~r/bad keyid/, fn -> Keys.enroll("claude@host-a\n", pub) end
      assert_raise ArgumentError, ~r/bad keyid/, fn -> Keys.enroll("claude", pub) end
    end

    test "accepts every listed harness" do
      for h <- KeyId.harnesses() do
        {id, _priv, pub} = keypair("#{h}@allow")
        assert is_binary(Keys.enroll(id, pub))
        assert {:ok, _} = Keys.public_key(id)
      end
    end
  end

  describe "authenticated requests" do
    # A key with an unlisted harness that reached keys/ some other way (a
    # curator push, a restored store) is still refused, before any signature
    # or nonce work.
    test "a key committed under an unknown harness cannot authenticate or push" do
      {curator, cpriv} = enroll!("curator@kid")
      {rogue, rpriv, rpub} = keypair("rogue@kid")

      # The same public key under an allowed harness (pi@twin) and an unlisted
      # one (rogue@kid), so the harness is the only difference between them.
      dir = clone!()
      File.mkdir_p!(Path.join(dir, "keys"))
      File.write!(Path.join(dir, "keys/rogue@kid.pub"), rpub <> "\n")
      File.write!(Path.join(dir, "keys/pi@twin.pub"), rpub <> "\n")
      git!(dir, ["add", "-A"])
      git!(dir, ["commit", "--quiet", "-m", "keys"])
      git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(curator, cpriv))

      assert {:ok, _} = Keys.public_key("pi@twin")
      assert :error = Keys.public_key(rogue)

      auth = fn id -> {"authorization", basic(id, rpriv)} end

      assert {200, _, _} = http_get("/healthz", [auth.("pi@twin")])
      assert {401, _, body} = http_get("/healthz", [auth.(rogue)])
      assert body =~ "unknown_harness"

      write_commit!(dir, "inbox/#{rogue}/x.md", "x\n", "as rogue")
      {out, status} = git(dir, ["push", "origin", "HEAD:main"], git_env(rogue, rpriv))
      assert status != 0
      assert out =~ "401" or out =~ "Authentication failed"
    end

    test "the plug answers 401 unknown_harness for a well-formed id with an unlisted harness" do
      conn =
        Plug.Test.conn(:get, "/healthz")
        |> Map.put(:remote_ip, {127, 0, 0, 1})
        |> put_req_header("authorization", "Basic " <> Base.encode64("evil@host-a:whatever"))

      conn = TacitServer.Router.call(conn, TacitServer.Router.init([]))
      assert conn.status == 401
      assert conn.resp_body =~ "unknown_harness"
    end
  end
end
