defmodule TacitServer.Auth.Token do
  @moduledoc """
  Writer tokens for git pushes. Git cannot sign requests, so the client's
  credential helper mints a short-lived token and sends it as the Basic
  password with the writer id as the user name.

      message = "tacit-v1\\n" <> keyid <> "\\n" <> ts <> "\\n" <> nonce
      token   = ts <> "." <> nonce <> "." <> base64url(ed25519_sign(message))

  The server checks: keyid is `<harness>@<host>` and its public key is in
  the store under `keys/<keyid>.pub`, |now - ts| <= window, the nonce was
  not seen from another peer inside 2x window (git reuses one credential
  for all requests of a push, so same-peer reuse is allowed), the signature
  verifies, and the tailnet host of the peer equals `<host>` (or the peer is
  loopback in dev).
  """

  @spec mint(binary(), binary(), integer() | nil) :: binary()
  def mint(keyid, priv, now \\ nil) do
    ts = Integer.to_string(now || System.os_time(:second))
    nonce = Base.url_encode64(:crypto.strong_rand_bytes(12), padding: false)
    sig = :crypto.sign(:eddsa, :none, message(keyid, ts, nonce), [priv, :ed25519])
    ts <> "." <> nonce <> "." <> Base.url_encode64(sig, padding: false)
  end

  @doc "Verify; returns {:ok, keyid} or {:error, reason}."
  def verify(keyid, token, pub, opts \\ []) do
    now = Keyword.get(opts, :now, System.os_time(:second))
    window = Keyword.get(opts, :window, 60)
    peer = Keyword.get(opts, :peer, "")
    claim = Keyword.get(opts, :claim_nonce, &TacitServer.State.claim_nonce/3)

    with [ts_s, nonce, sig_b64] <- String.split(token, ".", parts: 3),
         {ts, ""} <- Integer.parse(ts_s),
         true <- abs(now - ts) <= window || {:error, :expired},
         {:ok, sig} <- Base.url_decode64(sig_b64, padding: false),
         true <-
           :crypto.verify(:eddsa, :none, message(keyid, ts_s, nonce), sig, [pub, :ed25519]) ||
             {:error, :bad_signature},
         true <- claim.(keyid <> ":" <> nonce, 2 * window, peer) || {:error, :replay} do
      {:ok, keyid}
    else
      {:error, _} = e -> e
      _ -> {:error, :malformed}
    end
  end

  @doc "Parse an OpenSSH `ssh-ed25519 AAAA... comment` or raw base64 public key to 32 bytes."
  def parse_pub(text) do
    text = String.trim(text)

    with [_, b64 | _] <- String.split(text, " "),
         {:ok, blob} <- Base.decode64(b64),
         <<0, 0, 0, 11, "ssh-ed25519", 0, 0, 0, 32, key::binary-size(32)>> <- blob do
      {:ok, key}
    else
      _ ->
        case Base.decode64(text) do
          {:ok, <<key::binary-size(32)>>} -> {:ok, key}
          _ -> :error
        end
    end
  end

  def message(keyid, ts, nonce), do: "tacit-v1\n" <> keyid <> "\n" <> ts <> "\n" <> nonce
end
