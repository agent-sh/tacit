defmodule TacitServer.Git.Repo do
  @moduledoc """
  The bare store repository: creation with the fixed config, ref listing,
  file reads at a ref, and the git subprocess helper. Every call shells out
  to the git binary with argv lists, never a shell string.
  """

  @config [
    {"receive.autogc", "false"},
    {"gc.auto", "0"},
    {"receive.denyNonFastForwards", "true"},
    {"receive.denyDeletes", "true"},
    {"receive.fsckObjects", "true"},
    {"receive.maxInputSize", "67108864"},
    # The reflog of refs/heads/main is the source the journal is rebuilt from
    # (TacitServer.State.Rebuild), so it is on and `git gc`/`git reflog expire`
    # must never drop entries.
    {"core.logAllRefUpdates", "true"},
    {"gc.reflogExpire", "never"},
    {"gc.reflogExpireUnreachable", "never"},
    {"init.defaultBranch", "main"}
  ]

  @doc "Create the bare repo with its config if it does not exist. Idempotent."
  def ensure!(path) do
    unless File.dir?(Path.join(path, "objects")) do
      File.mkdir_p!(path)
      {_, 0} = git(path, ["init", "--bare", "--initial-branch=main", "--quiet", path])
    end

    for {k, v} <- @config, do: {_, 0} = git(path, ["config", k, v])
    :ok
  end

  @doc "Map of refname to sha."
  def refs(path) do
    {out, 0} = git(path, ["for-each-ref", "--format=%(refname) %(objectname)"])

    out
    |> String.split("\n", trim: true)
    |> Map.new(fn line ->
      [ref, sha] = String.split(line, " ", parts: 2)
      {ref, sha}
    end)
  end

  @doc "Ref updates between two ref maps as {ref, old, new}; deletions carry the zero sha."
  def diff_refs(before, after_) do
    zero = String.duplicate("0", 40)

    changed =
      for {ref, new} <- after_,
          Map.get(before, ref) != new,
          do: {ref, Map.get(before, ref, zero), new}

    deleted = for {ref, old} <- before, not Map.has_key?(after_, ref), do: {ref, old, zero}
    Enum.sort(changed ++ deleted)
  end

  @doc "Contents of a file at a ref, or :error."
  def show(path, ref, file) do
    case git(path, ["show", ref <> ":" <> file]) do
      {out, 0} -> {:ok, out}
      _ -> :error
    end
  end

  @doc """
  Sha of a ref, or nil when the ref does not exist. Anything else (git failing,
  not a repository, a broken ref file) raises: callers act on "missing", so a
  failure must never look like it.
  """
  def rev_parse(path, ref) do
    case git(path, ["rev-parse", "--verify", "--quiet", ref]) do
      {out, 0} -> String.trim(out)
      # --quiet prints nothing for a missing ref; a broken ref file also exits 1 but warns.
      {"", 1} -> nil
      {out, status} -> raise "git rev-parse #{ref} failed (exit #{status}): #{String.trim(out)}"
    end
  end

  @doc """
  True when `ancestor` is `descendant` or reachable from it. False when it is
  not, and when the `ancestor` object is not in the repository at all. Any
  other git failure raises.
  """
  def ancestor?(path, ancestor, descendant) do
    case git(path, ["merge-base", "--is-ancestor", ancestor, descendant]) do
      {_, 0} ->
        true

      {_, 1} ->
        false

      {out, status} ->
        # 128 is also what an unknown object gives; tell the two apart.
        {_, 0} = git(path, ["rev-parse", "--git-dir"])

        case git(path, ["cat-file", "-e", ancestor <> "^{commit}"]) do
          {_, 0} -> raise "git merge-base failed (exit #{status}): #{String.trim(out)}"
          _ -> false
        end
    end
  end

  @doc "Run git in the repo with argv. Returns {output, status}."
  def git(path, args, opts \\ []) do
    env = Keyword.get(opts, :env, [])
    System.cmd("git", ["-C", path | args], stderr_to_stdout: true, env: env)
  end
end
