defmodule TacitServer.State.Rebuild do
  @moduledoc """
  Recovers the journal from the store repository, so `state.db` needs no backup.

  What `state.db` holds:

    * `journal`: one row per accepted push (or enrolment) that moved
      `refs/heads/main`. Rebuildable, see below.
    * `nonces`: replay protection for signed tokens, rows live for two token
      windows (120 s). Not rebuilt and not worth keeping: a token is bound to
      the peer it was first seen from and to the tailnet host in its key id, so
      losing the table only reopens a replay inside one window.

  Only `refs/heads/main` can move (the pre-receive hook refuses every other
  ref and deletes), so the journal is the history of that one ref.

  Two sources, best first:

    1. The reflog (`logs/refs/heads/main`, kept by `core.logAllRefUpdates=true`
       and protected from expiry by `gc.reflogExpire=never`, see `Repo`).
       One line per ref update, which is exactly one journal row: same order,
       same old and new sha, push-boundaries intact, timestamp to the second.
       The writer is the reflog identity, which receive-pack and enrolment set
       to the key id. It is used only when it is complete: the first entry
       starts from the zero sha, each entry starts where the previous ended,
       and the last ends at the current `main`.
    2. The first-parent history of `main`, when the reflog is absent or
       incomplete (a store restored from a git mirror has no reflog). Order is
       recovered exactly. A push that carried several commits comes back as one
       row per commit, `ts` is the commit's committer time, and the writer is
       derived from the paths the commit touched.

  Writer derivation, for entries whose reflog identity is not a key id: every
  path under `inbox/<keyid>/` with a single owner gives that key id; only
  `keys/` paths give `server` (enrolment); anything else gives `unknown`.

  `seq` is assigned 1..n in that order. It equals the lost values when the
  journal and the reflog agreed, which holds unless the server died between
  `receive-pack` finishing and the journal append (`catch_up` below covers that
  case on the next start by appending the missing rows).
  """

  alias TacitServer.Auth.KeyId
  alias TacitServer.Git.Repo

  @main "refs/heads/main"
  @zero String.duplicate("0", 40)
  @empty_tree "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

  @type row :: %{
          ref: String.t(),
          old: String.t(),
          new: String.t(),
          writer: String.t(),
          ts: integer()
        }

  @doc "Every journal row the repo supports, oldest first. `[]` for an empty store."
  @spec rows(String.t()) :: [row()]
  def rows(repo) do
    case Repo.rev_parse(repo, @main) do
      nil ->
        []

      main ->
        case from_reflog(repo, main) do
          {:ok, rows} -> rows
          :error -> from_history(repo, main)
        end
    end
  end

  @doc """
  Rows to append to a journal whose newest `main` entry ends at `journal_head`
  so that it ends at the current `main`. When `journal_head` is not on the
  recovered chain, one row spanning the gap.
  """
  @spec catch_up(String.t(), String.t()) :: [row()]
  def catch_up(repo, journal_head) do
    chain = rows(repo)

    case Enum.find_index(chain, &(&1.new == journal_head)) do
      nil ->
        main = Repo.rev_parse(repo, @main)

        [
          %{
            ref: @main,
            old: journal_head,
            new: main,
            writer: derive_writer(repo, journal_head, main),
            ts: System.os_time(:second)
          }
        ]

      i ->
        Enum.drop(chain, i + 1)
    end
  end

  # ---- reflog -------------------------------------------------------------

  # "<old> <new> <name> <<email>> <ts> <tz>\t<message>"
  @reflog_line ~r/\A([0-9a-f]{40}) ([0-9a-f]{40}) .*? <([^>]*)> (\d+) [+-]\d{4}(?:\t.*)?\z/

  defp from_reflog(repo, main) do
    with {:ok, text} <- File.read(Path.join([repo, "logs", "refs", "heads", "main"])),
         rows when rows != [] <- parse_reflog(repo, text),
         true <- chain?(rows, main) do
      {:ok, rows}
    else
      _ -> :error
    end
  end

  defp parse_reflog(repo, text) do
    for line <- String.split(text, "\n", trim: true),
        [_, old, new, email, ts] <- [Regex.run(@reflog_line, line)],
        old != new do
      %{
        ref: @main,
        old: old,
        new: new,
        writer: ident_writer(email) || derive_writer(repo, old, new),
        ts: String.to_integer(ts)
      }
    end
  end

  defp chain?([first | _] = rows, main) do
    first.old == @zero and
      rows |> Enum.chunk_every(2, 1, :discard) |> Enum.all?(fn [a, b] -> a.new == b.old end) and
      List.last(rows).new == main
  end

  defp ident_writer("server"), do: "server"

  defp ident_writer(email) do
    case KeyId.parse(email) do
      {:ok, _, _} -> email
      _ -> nil
    end
  end

  # ---- history ------------------------------------------------------------

  defp from_history(repo, main) do
    {out, 0} =
      Repo.git(repo, ["log", "--first-parent", "--reverse", "--format=%H%x09%P%x09%ct", main])

    for line <- String.split(out, "\n", trim: true) do
      [sha, parents, ct] = String.split(line, "\t")
      old = parents |> String.split(" ", trim: true) |> List.first() || @zero

      %{
        ref: @main,
        old: old,
        new: sha,
        writer: derive_writer(repo, old, sha),
        ts: String.to_integer(ct)
      }
    end
  end

  # ---- writer from the diff ----------------------------------------------

  defp derive_writer(repo, old, new) do
    base = if old == @zero, do: @empty_tree, else: old

    case Repo.git(repo, ["diff-tree", "-r", "--name-only", "--no-renames", "-z", base, new]) do
      {out, 0} -> writer_from_paths(String.split(out, <<0>>, trim: true))
      _ -> "unknown"
    end
  end

  defp writer_from_paths([]), do: "unknown"

  defp writer_from_paths(paths) do
    cond do
      Enum.all?(paths, &String.starts_with?(&1, "keys/")) ->
        "server"

      Enum.all?(paths, &String.starts_with?(&1, "inbox/")) ->
        owners = paths |> Enum.map(&(&1 |> String.split("/") |> Enum.at(1))) |> Enum.uniq()

        case owners do
          [owner] -> if match?({:ok, _, _}, KeyId.parse(owner)), do: owner, else: "unknown"
          _ -> "unknown"
        end

      true ->
        "unknown"
    end
  end
end
