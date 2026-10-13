defmodule TacitServer.Auth.TokenTest do
  use ExUnit.Case, async: true
  alias TacitServer.Auth.Token

  setup do
    {pub, priv} = :crypto.generate_key(:eddsa, :ed25519)
    %{pub: pub, priv: priv, seen: :ets.new(:seen, [:set, :public])}
  end

  defp claim(tab) do
    fn nonce, _ttl, p ->
      :ets.insert_new(tab, {nonce, p})
      [{_, owner}] = :ets.lookup(tab, nonce)
      owner == p
    end
  end

  test "mint and verify", %{pub: pub, priv: priv, seen: seen} do
    t = Token.mint("claude@host-a", priv, 1_000)

    assert {:ok, "claude@host-a"} =
             Token.verify("claude@host-a", t, pub, now: 1_010, claim_nonce: claim(seen))
  end

  test "replay from another peer is refused, same peer may reuse", %{
    pub: pub,
    priv: priv,
    seen: seen
  } do
    t = Token.mint("claude@host-a", priv, 1_000)

    assert {:ok, _} =
             Token.verify("claude@host-a", t, pub,
               now: 1_000,
               peer: "a",
               claim_nonce: claim(seen)
             )

    assert {:ok, _} =
             Token.verify("claude@host-a", t, pub,
               now: 1_000,
               peer: "a",
               claim_nonce: claim(seen)
             )

    assert {:error, :replay} =
             Token.verify("claude@host-a", t, pub,
               now: 1_000,
               peer: "b",
               claim_nonce: claim(seen)
             )
  end

  test "outside the window", %{pub: pub, priv: priv, seen: seen} do
    t = Token.mint("claude@host-a", priv, 1_000)

    assert {:error, :expired} =
             Token.verify("claude@host-a", t, pub, now: 1_061, claim_nonce: claim(seen))

    assert {:error, :expired} =
             Token.verify("claude@host-a", t, pub, now: 939, claim_nonce: claim(seen))
  end

  test "wrong key, wrong keyid, garbage", %{pub: pub, priv: priv, seen: seen} do
    {other_pub, _} = :crypto.generate_key(:eddsa, :ed25519)
    t = Token.mint("claude@host-a", priv, 1_000)

    assert {:error, :bad_signature} =
             Token.verify("claude@host-a", t, other_pub, now: 1_000, claim_nonce: claim(seen))

    assert {:error, :bad_signature} =
             Token.verify("codex@host-a", t, pub, now: 1_000, claim_nonce: claim(seen))

    assert {:error, :malformed} =
             Token.verify("claude@host-a", "nope", pub, now: 1_000, claim_nonce: claim(seen))

    assert {:error, :malformed} =
             Token.verify("claude@host-a", "1000.n.!!!", pub,
               now: 1_000,
               claim_nonce: claim(seen)
             )
  end

  test "parse_pub accepts openssh and raw base64", %{pub: pub} do
    blob = <<0, 0, 0, 11, "ssh-ed25519", 0, 0, 0, 32, pub::binary>>
    assert {:ok, ^pub} = Token.parse_pub("ssh-ed25519 " <> Base.encode64(blob) <> " comment\n")
    assert {:ok, ^pub} = Token.parse_pub(Base.encode64(pub))
    assert :error = Token.parse_pub("ssh-rsa AAAA")
    assert :error = Token.parse_pub("")
  end
end
