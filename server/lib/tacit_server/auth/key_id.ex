defmodule TacitServer.Auth.KeyId do
  @moduledoc """
  Writer key ids: `<harness>@<host>`.

  The harness must be on `@harnesses`. That list is the server's copy of
  `Harnesses` in `client/internal/fact/fact.go` and of the list in SPEC.md
  ("Store layout"). All three change together; `server/test/tacit_server/key_id_test.exs`
  reads the Go file and fails when the lists differ.

  Enrolment (`TacitServer.Auth.Keys.enroll/2`) and every authenticated
  request (`TacitServer.Auth.Plug`) go through `parse/1`, so a key id with
  an unknown harness can neither be enrolled nor push, even if its public key
  was committed to `keys/` some other way.
  """

  # `reader` is a key that only reads: it may fetch (also over the public
  # path) and is refused at every push (`TacitServer.Git.Http`).
  @harnesses ~w(claude codex pi hermes grok dreamer import curator human reader)

  @doc "A key id whose harness is `reader`: it may read and never push."
  def reader?(keyid) when is_binary(keyid), do: match?({:ok, "reader", _}, parse(keyid))
  def reader?(_), do: false

  # \A..\z, not ^..$: PCRE's `$` also matches before a trailing newline.
  @pattern ~r/\A([a-z0-9-]+)@([a-z0-9-]+)\z/

  def harnesses, do: @harnesses

  @doc """
  Split a key id into `{:ok, harness, host}`, or `{:error, :bad_keyid}` when it is
  not `<slug>@<slug>`, or `{:error, :unknown_harness}` when the harness is not allowed.
  """
  @spec parse(term()) :: {:ok, String.t(), String.t()} | {:error, :bad_keyid | :unknown_harness}
  def parse(keyid) when is_binary(keyid) do
    case Regex.run(@pattern, keyid) do
      [_, harness, host] when harness in @harnesses -> {:ok, harness, host}
      [_, _, _] -> {:error, :unknown_harness}
      nil -> {:error, :bad_keyid}
    end
  end

  def parse(_), do: {:error, :bad_keyid}
end
