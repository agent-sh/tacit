defmodule TacitServer.State.RebuildTest do
  @moduledoc """
  What the journal can be rebuilt from, on private bare repos: the reflog when
  it is complete, the first-parent history when it is not. No server involved.
  """
  use ExUnit.Case, async: true
  import TacitServer.TestHelpers
  alias TacitServer.Git.Repo
  alias TacitServer.State.Rebuild

  @zero String.duplicate("0", 40)

  defp new_store! do
    repo = Path.join(tmp("rebuild-repo"), "store.git")
    Repo.ensure!(repo)
    work = tmp("rebuild-work")
    git!(work, ["init", "--quiet", "-b", "main", "."])
    git!(work, ["config", "user.email", "t@test"])
    git!(work, ["config", "user.name", "t"])
    {repo, work}
  end

  defp commit!(work, rel, body) do
    write_commit!(work, rel, body, "add #{rel}")
    git!(work, ["rev-parse", "HEAD"]) |> String.trim()
  end

  # Push HEAD into the bare repo; `ident` becomes the reflog committer, the way
  # the server sets it from the writer's key id.
  defp push!(repo, work, ident) do
    git!(work, ["push", "--quiet", repo, "HEAD:main"], [
      {"GIT_COMMITTER_NAME", ident},
      {"GIT_COMMITTER_EMAIL", ident}
    ])

    git!(work, ["rev-parse", "HEAD"]) |> String.trim()
  end

  defp reflog(repo), do: Path.join(repo, "logs/refs/heads/main")

  # Four pushes: a key id identity, a two-commit push, a foreign identity that
  # touched one inbox, and an enrolment-like keys/ commit by "server".
  defp build_history! do
    {repo, work} = new_store!()
    commit!(work, "inbox/claude@a/one.md", "1\n")
    c1 = push!(repo, work, "claude@a")

    commit!(work, "inbox/codex@a/two.md", "2\n")
    c3 = commit!(work, "inbox/codex@a/three.md", "3\n")
    push!(repo, work, "codex@a")

    c4 = commit!(work, "inbox/pi@a/four.md", "4\n")

    git!(work, ["push", "--quiet", repo, "HEAD:main"], [
      {"GIT_COMMITTER_NAME", "Someone"},
      {"GIT_COMMITTER_EMAIL", "someone@example.com"}
    ])

    c5 = commit!(work, "keys/human@a.pub", "ssh-ed25519 AAAA x\n")
    push!(repo, work, "server")
    %{repo: repo, work: work, c1: c1, c3: c3, c4: c4, c5: c5}
  end

  test "rev_parse tells a missing ref from a failing git" do
    {repo, _work} = new_store!()
    assert Repo.rev_parse(repo, "refs/heads/main") == nil

    # Not a repository: git exits 128, which must not read as "no main".
    assert_raise RuntimeError, ~r/rev-parse/, fn ->
      Repo.rev_parse(Path.join(tmp("not-a-repo"), "nope.git"), "refs/heads/main")
    end

    # A ref file git cannot parse exits 1 like a missing ref, but warns.
    File.write!(Path.join(repo, "refs/heads/main"), "not a sha\n")
    assert_raise RuntimeError, ~r/rev-parse/, fn -> Repo.rev_parse(repo, "refs/heads/main") end
  end

  test "ancestor? answers false for an unknown object and raises when git fails" do
    %{repo: repo, c1: c1, c5: c5} = build_history!()

    assert Repo.ancestor?(repo, c1, c5)
    assert Repo.ancestor?(repo, c5, c5)
    refute Repo.ancestor?(repo, c5, c1)
    refute Repo.ancestor?(repo, String.duplicate("f", 40), c5)

    assert_raise MatchError, fn ->
      Repo.ancestor?(Path.join(tmp("not-a-repo"), "nope.git"), c1, c5)
    end
  end

  test "an empty store has no rows" do
    {repo, _} = new_store!()
    assert Rebuild.rows(repo) == []
  end

  test "the reflog gives one row per push, in order, with the writer from its identity" do
    %{repo: repo, c1: c1, c3: c3, c4: c4, c5: c5} = build_history!()
    now = System.os_time(:second)

    rows = Rebuild.rows(repo)

    assert length(rows) == 4
    assert Enum.map(rows, & &1.new) == [c1, c3, c4, c5]
    assert Enum.map(rows, & &1.old) == [@zero, c1, c3, c4]
    # claude@a and codex@a come from the reflog; Someone is not a key id, so
    # the writer is read off the paths (inbox/pi@a/); keys/ only is the server.
    assert Enum.map(rows, & &1.writer) == ["claude@a", "codex@a", "pi@a", "server"]
    assert Enum.all?(rows, &(&1.ref == "refs/heads/main" and abs(&1.ts - now) < 120))
  end

  test "without a reflog the first-parent history is used: order kept, push boundaries lost" do
    %{repo: repo, c1: c1, c3: c3, c4: c4, c5: c5, work: work} = build_history!()
    File.rm!(reflog(repo))

    rows = Rebuild.rows(repo)

    # Five commits, one row each; the two-commit push is now two rows.
    assert length(rows) == 5
    assert List.last(rows).new == c5
    assert hd(rows).old == @zero
    assert Enum.map(rows, & &1.new) |> Enum.member?(c1)
    assert Enum.map(rows, & &1.new) |> Enum.member?(c3)
    assert Enum.map(rows, & &1.new) |> Enum.member?(c4)
    rows |> Enum.chunk_every(2, 1, :discard) |> Enum.each(fn [a, b] -> assert a.new == b.old end)

    assert Enum.map(rows, & &1.writer) == ["claude@a", "codex@a", "codex@a", "pi@a", "server"]

    # ts is the commit's committer time.
    {out, 0} =
      System.cmd("git", ["-C", work, "log", "--first-parent", "--reverse", "--format=%ct"])

    assert Enum.map(rows, & &1.ts) ==
             out |> String.split("\n", trim: true) |> Enum.map(&String.to_integer/1)
  end

  test "an incomplete reflog is not trusted" do
    %{repo: repo} = build_history!()
    [_first | rest] = File.read!(reflog(repo)) |> String.split("\n", trim: true)
    File.write!(reflog(repo), Enum.join(rest, "\n") <> "\n")

    # The first entry no longer starts from the zero sha: fall back to history.
    assert length(Rebuild.rows(repo)) == 5
  end

  test "a reflog that stops short of main is not trusted" do
    %{repo: repo} = build_history!()
    lines = File.read!(reflog(repo)) |> String.split("\n", trim: true)
    File.write!(reflog(repo), Enum.join(Enum.drop(lines, -1), "\n") <> "\n")

    assert length(Rebuild.rows(repo)) == 5
  end

  test "a push that touched paths outside any inbox, by a non-key identity, has writer unknown" do
    {repo, work} = new_store!()
    commit!(work, "projects/x/y.md", "y\n")

    git!(work, ["push", "--quiet", repo, "HEAD:main"], [
      {"GIT_COMMITTER_NAME", "Someone"},
      {"GIT_COMMITTER_EMAIL", "someone@example.com"}
    ])

    assert [%{writer: "unknown"}] = Rebuild.rows(repo)
  end

  test "catch_up returns the rows after a known head" do
    %{repo: repo, c1: c1, c3: c3, c5: c5} = build_history!()

    assert [%{new: ^c3}, %{new: _}, %{new: ^c5}] = Rebuild.catch_up(repo, c1)
    assert [] = Rebuild.catch_up(repo, c5)
  end

  test "catch_up from a sha not on the chain is one row spanning the gap" do
    %{repo: repo, c5: c5} = build_history!()
    stray = String.duplicate("a", 40)

    assert [%{old: ^stray, new: ^c5, writer: "unknown"}] = Rebuild.catch_up(repo, stray)
  end
end

defmodule TacitServer.StateRecoveryTest do
  @moduledoc """
  state.db is disposable: delete it, corrupt it, let it fall behind or point
  at another history, run the boot recovery (`State.recover!/0`), and the journal
  is back as the repository says it was. A supervised restart of the process only
  reopens the file.
  """
  use ExUnit.Case, async: false
  import TacitServer.TestHelpers
  alias Exqlite.Sqlite3
  alias TacitServer.{Config, State}

  @zero String.duplicate("0", 40)

  setup_all do
    {keyid, priv} = enroll!("claude@rec")
    dir = clone!()
    # A one-commit push, a two-commit push, and another one: the journal then
    # holds enrolment rows by "server" and push rows by claude@rec.
    write_commit!(dir, "inbox/#{keyid}/a.md", "a\n", "a")
    git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    write_commit!(dir, "inbox/#{keyid}/b.md", "b\n", "b")
    write_commit!(dir, "inbox/#{keyid}/c.md", "c\n", "c")
    git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    %{keyid: keyid, priv: priv}
  end

  defp db_path, do: Config.get(:state_db)
  defp repo, do: Config.get(:repo)
  defp db_files, do: [db_path(), db_path() <> "-wal", db_path() <> "-shm"]
  defp journal, do: State.since(0, 100_000)

  defp strip(rows), do: Enum.map(rows, &Map.take(&1, [:seq, :ref, :old, :new, :writer]))

  defp same_ts?(a, b), do: Enum.all?(Enum.zip(a, b), fn {x, y} -> abs(x.ts - y.ts) <= 10 end)

  # Stop the State process, let `fun` change the files, run the boot recovery
  # (unless boot: false) and start the process again.
  defp with_state_stopped(fun, opts \\ []) do
    :ok = Supervisor.terminate_child(TacitServer.Supervisor, State)

    try do
      fun.()
      if Keyword.get(opts, :boot, true), do: :ok = State.recover!()
    after
      {:ok, _} = Supervisor.restart_child(TacitServer.Supervisor, State)
    end
  end

  defp with_db(fun) do
    {:ok, db} = Sqlite3.open(db_path())

    try do
      fun.(db)
    after
      Sqlite3.close(db)
    end
  end

  defp quarantined(tag),
    do:
      Path.wildcard(db_path() <> ".#{tag}-*")
      |> Enum.reject(&String.ends_with?(&1, ["-wal", "-shm"]))

  defp main_sha, do: git!(repo(), ["rev-parse", "main"]) |> String.trim()

  test "a deleted state.db is rebuilt from the reflog: same rows, same order, same writers",
       %{keyid: keyid, priv: priv} do
    before = journal()
    assert length(before) >= 3
    assert Enum.any?(before, &(&1.writer == "server"))
    assert Enum.any?(before, &(&1.writer == keyid))

    with_state_stopped(fn -> Enum.each(db_files(), &File.rm/1) end)

    after_ = journal()
    assert strip(after_) == strip(before)
    assert same_ts?(before, after_)
    assert State.head_seq() == length(before)

    # And it keeps going where it left off: the next push is the next seq.
    dir = clone!()
    write_commit!(dir, "inbox/#{keyid}/after-rebuild.md", "x\n", "after")
    git!(dir, ["push", "--quiet", "origin", "HEAD:main"], git_env(keyid, priv))
    assert [%{seq: seq, writer: ^keyid}] = State.since(length(before), 10)
    assert seq == length(before) + 1
  end

  test "a state.db that is not a database is set aside and rebuilt" do
    before = journal()
    known = length(quarantined("corrupt"))

    with_state_stopped(fn ->
      Enum.each(db_files(), &File.rm/1)
      File.write!(db_path(), :crypto.strong_rand_bytes(8192))
    end)

    assert length(quarantined("corrupt")) == known + 1
    assert strip(journal()) == strip(before)
  end

  test "a state.db with a damaged page is set aside and rebuilt" do
    before = journal()
    known = length(quarantined("corrupt"))

    with_state_stopped(fn ->
      # Clean shutdown checkpointed the WAL, so the table pages are in the main file.
      assert File.stat!(db_path()).size >= 3 * 4096
      {:ok, fd} = :file.open(db_path(), [:read, :write, :raw, :binary])
      :ok = :file.pwrite(fd, 4096, :crypto.strong_rand_bytes(4096))
      :ok = :file.close(fd)
      Enum.each([db_path() <> "-wal", db_path() <> "-shm"], &File.rm/1)
    end)

    assert length(quarantined("corrupt")) == known + 1
    assert strip(journal()) == strip(before)
  end

  test "a journal that fell behind main (died between receive-pack and the append) is caught up" do
    before = journal()
    stale = length(quarantined("stale"))
    corrupt = length(quarantined("corrupt"))

    with_state_stopped(fn ->
      with_db(fn db ->
        :ok = Sqlite3.execute(db, "DELETE FROM journal WHERE seq > #{length(before) - 2}")
      end)
    end)

    assert strip(journal()) == strip(before)
    assert same_ts?(before, journal())
    # Repaired in place, nothing set aside.
    assert length(quarantined("stale")) == stale
    assert length(quarantined("corrupt")) == corrupt
  end

  test "a journal that does not belong to this repository is set aside and rebuilt" do
    before = journal()
    known = length(quarantined("stale"))

    with_state_stopped(fn ->
      with_db(fn db ->
        :ok =
          Sqlite3.execute(
            db,
            "INSERT INTO journal(ref, old_sha, new_sha, writer, ts) VALUES " <>
              "('refs/heads/main', '#{main_sha()}', '#{String.duplicate("f", 40)}', 'ghost@host-a', 1)"
          )
      end)
    end)

    assert length(quarantined("stale")) == known + 1
    assert strip(journal()) == strip(before)
  end

  test "recovery on a healthy state.db changes nothing" do
    before = journal()
    with_state_stopped(fn -> :ok end)
    assert journal() == before
  end

  test "a supervised restart only reopens the file: a journal behind main waits for the next boot" do
    before = journal()

    with_state_stopped(
      fn ->
        with_db(fn db ->
          :ok = Sqlite3.execute(db, "DELETE FROM journal WHERE seq > #{length(before) - 1}")
        end)
      end,
      boot: false
    )

    # A crash-and-restart of the process while pushes are in flight must not
    # journal a half-finished push a second time, so it does not reconcile.
    assert length(journal()) == length(before) - 1

    with_state_stopped(fn -> :ok end)
    assert strip(journal()) == strip(before)
  end

  test "a state.db that cannot be opened for another reason is not taken for corruption" do
    before = journal()
    known = length(quarantined("corrupt"))

    :ok = Supervisor.terminate_child(TacitServer.Supervisor, State)

    try do
      # A directory where the file should be: not a database, but not corrupt either.
      Enum.each(db_files(), &File.rm/1)
      File.mkdir!(db_path())
      assert catch_error(State.recover!())
      assert length(quarantined("corrupt")) == known
      assert File.dir?(db_path())
    after
      File.rmdir!(db_path())
      :ok = State.recover!()
      {:ok, _} = Supervisor.restart_child(TacitServer.Supervisor, State)
    end

    assert strip(journal()) == strip(before)
  end

  test "without a reflog the journal is rebuilt from history: same order, one row per commit" do
    before = journal()
    reflog = Path.join(repo(), "logs/refs/heads/main")
    saved = File.read!(reflog)

    try do
      File.rm!(reflog)
      with_state_stopped(fn -> Enum.each(db_files(), &File.rm/1) end)

      rows = journal()
      assert Enum.map(rows, & &1.seq) == Enum.to_list(1..length(rows))
      assert hd(rows).old == @zero
      assert List.last(rows).new == main_sha()

      rows
      |> Enum.chunk_every(2, 1, :discard)
      |> Enum.each(fn [a, b] -> assert a.new == b.old end)

      # Every push tip in the original journal is still a row, in the same relative order.
      tips = Enum.map(before, & &1.new)
      kept = rows |> Enum.map(& &1.new) |> Enum.filter(&(&1 in tips))
      assert kept == tips
      assert length(rows) >= length(before)
    after
      # Put the reflog back and rebuild from it so the other tests see the original journal.
      File.mkdir_p!(Path.dirname(reflog))
      File.write!(reflog, saved)
      with_state_stopped(fn -> Enum.each(db_files(), &File.rm/1) end)
    end

    assert strip(journal()) == strip(before)
  end
end
