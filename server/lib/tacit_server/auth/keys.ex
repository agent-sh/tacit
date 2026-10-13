defmodule TacitServer.Auth.Keys do
  @moduledoc """
  Writer public keys live in the store itself under `keys/<keyid>.pub` on
  `main`, so enrollment, rotation and revocation are commits. Cached 30 s.
  Enrollment commits are made server-side under the lease by `enroll/2`.
  """
  alias TacitServer.Auth.{Cache, KeyId, Token}
  alias TacitServer.Config
  alias TacitServer.Git.{Lease, Repo}

  @ttl_ms 30_000

  @doc """
  The public key of keyid, `{:ok, key}` or `:error`. `list` lists the
  enrolled ids (`list_keys/0`; a test passes a failing one).
  """
  def public_key(keyid, list \\ &list_keys/0) do
    case Cache.get({:key, keyid}) do
      {:ok, v} ->
        v

      :miss ->
        v =
          with {:ok, _harness, _host} <- KeyId.parse(keyid),
               true <- maybe_enrolled?(keyid, list) || :error,
               {:ok, text} <- Repo.show(Config.get(:repo), "main", "keys/#{keyid}.pub"),
               {:ok, key} <- Token.parse_pub(text) do
            {:ok, key}
          else
            _ -> :error
          end

        # Only hits are cached here; a miss is answered by the cached set of
        # enrolled ids below, so a probe with an unknown id runs no git.
        if v != :error, do: Cache.put({:key, keyid}, v, @ttl_ms)
        v
    end
  end

  @doc """
  The key ids with a file under `keys/` on main: one `git ls-tree` per 30 s
  (the same TTL as a cached key, so a key removed by a commit stops working
  within the same 30 s either way), dropped at once by `enroll/2` and by
  every push that moves a ref (`TacitServer.Git.Http`), so a key enrolled or
  committed a moment ago works on its next request. Unknown ids, any number of
  them, cost a set lookup.

  Only a listing that succeeded is cached. A failed one (a git error, a
  timeout, a repo without main yet) returns `:error` and caches nothing, and
  `public_key/2` then asks for the one key as before, so a transient failure
  never rejects an enrolled key.
  """
  def enrolled(list \\ &list_keys/0) do
    case Cache.get(:enrolled_keys) do
      {:ok, set} ->
        {:ok, set}

      :miss ->
        case list.() do
          {:ok, set} ->
            Cache.put(:enrolled_keys, set, @ttl_ms)
            {:ok, set}

          :error ->
            :error
        end
    end
  end

  # No listing to go by: the per-key lookup decides.
  defp maybe_enrolled?(keyid, list) do
    case enrolled(list) do
      {:ok, set} -> MapSet.member?(set, keyid)
      :error -> true
    end
  end

  @doc false
  def list_keys do
    case Repo.git(Config.get(:repo), ["ls-tree", "--name-only", "main", "keys/"]) do
      {out, 0} ->
        set =
          for line <- String.split(out, "\n", trim: true),
              String.starts_with?(line, "keys/") and String.ends_with?(line, ".pub"),
              into: MapSet.new(),
              do: line |> String.trim_leading("keys/") |> String.trim_trailing(".pub")

        {:ok, set}

      _ ->
        :error
    end
  end

  @doc """
  Commit `keys/<keyid>.pub` on main without a worktree (hash-object,
  read-tree into a temp index, commit-tree, update-ref with the old sha as
  the compare value). Returns the commit sha; idempotent when the stored
  key already equals pub_text.
  """
  def enroll(keyid, pub_text) do
    case KeyId.parse(keyid) do
      {:ok, _harness, _host} ->
        :ok

      {:error, :unknown_harness} ->
        raise ArgumentError,
              "unknown harness in #{inspect(keyid)}; allowed: #{Enum.join(KeyId.harnesses(), ", ")}"

      {:error, :bad_keyid} ->
        raise ArgumentError, "bad keyid #{inspect(keyid)}, expected <harness>@<host>"
    end

    {:ok, _} = Token.parse_pub(pub_text)
    repo = Config.get(:repo)
    pub_text = String.trim(pub_text) <> "\n"

    Lease.with_lease(10_000, fn ->
      old = Repo.rev_parse(repo, "refs/heads/main")

      case old && Repo.show(repo, old, "keys/#{keyid}.pub") do
        {:ok, ^pub_text} -> old
        _ -> commit_key(repo, old, keyid, pub_text)
      end
    end)
  end

  defp commit_key(repo, old, keyid, pub_text) do
    (fn ->
       idx = Path.join(Config.get(:spool), "enroll-#{System.unique_integer([:positive])}.idx")
       env = [{"GIT_INDEX_FILE", idx}]

       try do
         if old, do: {_, 0} = Repo.git(repo, ["read-tree", old], env: env)
         {blob, 0} = hash_blob(repo, pub_text, env)

         {_, 0} =
           Repo.git(
             repo,
             [
               "update-index",
               "--add",
               "--cacheinfo",
               "100644,#{String.trim(blob)},keys/#{keyid}.pub"
             ],
             env: env
           )

         {tree, 0} = Repo.git(repo, ["write-tree"], env: env)

         parent = if old, do: ["-p", old], else: []

         {commit, 0} =
           Repo.git(
             repo,
             ["commit-tree", String.trim(tree)] ++ parent ++ ["-m", "enroll #{keyid}"],
             env: [
               {"GIT_AUTHOR_NAME", "tacit-server"},
               {"GIT_AUTHOR_EMAIL", "tacit@localhost"},
               {"GIT_COMMITTER_NAME", "tacit-server"},
               {"GIT_COMMITTER_EMAIL", "tacit@localhost"}
             ]
           )

         commit = String.trim(commit)

         # The committer identity lands in the reflog, which is where the
         # journal's writer column is rebuilt from (TacitServer.State.Rebuild).
         {_, 0} =
           Repo.git(
             repo,
             [
               "update-ref",
               "-m",
               "enroll #{keyid}",
               "refs/heads/main",
               commit,
               old || String.duplicate("0", 40)
             ],
             env: [{"GIT_COMMITTER_NAME", "server"}, {"GIT_COMMITTER_EMAIL", "server"}]
           )

         Cache.delete({:key, keyid})
         Cache.delete(:enrolled_keys)

         {:ok, seq} =
           TacitServer.State.append(
             [{"refs/heads/main", old || String.duplicate("0", 40), commit}],
             "server"
           )

         TacitServer.Feed.notify(seq)
         commit
       after
         File.rm(idx)
       end
     end).()
  end

  # System.cmd has no stdin; write the blob through a temp file instead.
  defp hash_blob(repo, text, env) do
    path = Path.join(Config.get(:spool), "blob-#{System.unique_integer([:positive])}")
    File.write!(path, text)

    try do
      Repo.git(repo, ["hash-object", "-w", path], env: env)
    after
      File.rm(path)
    end
  end
end
