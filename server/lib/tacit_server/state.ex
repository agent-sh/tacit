defmodule TacitServer.State do
  @moduledoc """
  The state database: one SQLite file, one writer process, WAL mode.

  Tables:

    journal(seq, ref, old_sha, new_sha, writer, ts)   total order of accepted pushes
    nonces(nonce, peer, expires_at)                     replay protection for signed tokens

  Reads go through the same process; the volume is tiny (a feed read per
  wake-up, a nonce check per push). `seq` is the ordering primitive the
  change feed carries, so it is assigned here and nowhere else.

  The file is disposable. Once at boot, before the supervision tree starts,
  `recover!/0` checks it and reconciles the journal with the repository (see
  `TacitServer.State.Rebuild` for what is recoverable and in which order):

    * missing, or an empty journal beside a non-empty store: the journal is
      rebuilt from the repo;
    * unreadable (`PRAGMA quick_check` fails, or SQLite says "not a database"
      or "malformed"): the file is renamed to `state.db.corrupt-<UTC time>` and
      the journal rebuilt;
    * journal behind `main` (the server died between receive-pack and the
      journal append): the missing rows are appended;
    * journal off the repo (`main` is not a descendant of the journal's newest
      sha, for example a store restored from an older backup): the file is
      renamed to `state.db.stale-<UTC time>` and the journal rebuilt.

  Any other failure (permissions, disk full, git failing) stops the boot; it is
  never taken for corruption. Renamed files are kept for inspection and never
  pruned by the server.

  Recovery is boot-only on purpose. If this process crashes and the supervisor
  restarts it while pushes are in flight, `init/1` only reopens the file: a push
  between receive-pack's ref update and its `append/2` would otherwise be
  journaled twice (once by a catch-up, once by the push itself).
  """
  use GenServer
  require Logger
  alias Exqlite.Sqlite3
  alias TacitServer.Git.Repo
  alias TacitServer.State.Rebuild

  @main "refs/heads/main"

  def start_link(opts \\ []), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  @doc "Append journal rows for one push in one transaction; returns the last seq."
  def append(rows, writer) when is_list(rows),
    do: GenServer.call(__MODULE__, {:append, rows, writer})

  @doc "Journal rows with seq > since, oldest first, at most limit."
  def since(since, limit \\ 200), do: GenServer.call(__MODULE__, {:since, since, limit})

  def head_seq, do: GenServer.call(__MODULE__, :head_seq)

  @doc """
  Record a nonce for a peer. True when unseen, or seen from the same peer
  (git reuses one credential across the requests of a single push, so a
  token is valid for its window from the node it was minted on and from
  nowhere else).
  """
  def claim_nonce(nonce, ttl_s, peer),
    do: GenServer.call(__MODULE__, {:nonce, nonce, ttl_s, peer})

  @doc """
  Check the state file and reconcile the journal with the repo. Run once, at
  boot, before `start_link/1`. Raises when the file or the repo cannot be read.
  """
  def recover! do
    path = TacitServer.Config.get(:state_db)
    repo = TacitServer.Config.get(:repo)
    File.mkdir_p!(Path.dirname(path))

    db = open_or_quarantine(path)

    db =
      case reconcile(db, repo) do
        :ok ->
          db

        {:stale, head, main} ->
          Logger.error(
            "journal head #{inspect(head)} is not an ancestor of main #{inspect(main)}; " <>
              "moving state.db aside and rebuilding the journal from the repo"
          )

          Sqlite3.close(db)
          quarantine(path, "stale")
          db = open_or_quarantine(path)
          :ok = reconcile(db, repo)
          db
      end

    Sqlite3.close(db)
    :ok
  end

  @impl true
  def init(_opts) do
    # Trap exits so a supervisor shutdown (systemctl stop) runs terminate/2
    # and the WAL is checkpointed into the main file.
    Process.flag(:trap_exit, true)
    path = TacitServer.Config.get(:state_db)

    case open_checked(path) do
      {:ok, db} -> {:ok, %{db: db}}
      {:corrupt, reason} -> {:stop, {:state_db_unusable, reason}}
    end
  end

  @impl true
  def terminate(_reason, %{db: db}), do: Sqlite3.close(db)

  # Trapping exits delivers the exit of any linked port or process; none of them
  # carries information for this server.
  @impl true
  def handle_info(_msg, s), do: {:noreply, s}

  @impl true
  def handle_call({:append, rows, writer}, _from, %{db: db} = s) do
    now = System.os_time(:second)

    last =
      transaction(db, fn ->
        Enum.reduce(rows, nil, fn {ref, old, new}, _ ->
          insert(db, %{ref: ref, old: old, new: new, writer: writer, ts: now})
        end)
      end)

    {:reply, {:ok, last}, s}
  end

  def handle_call({:since, since, limit}, _from, %{db: db} = s) do
    {:ok, st} =
      Sqlite3.prepare(
        db,
        "SELECT seq, ref, old_sha, new_sha, writer, ts FROM journal WHERE seq > ?1 ORDER BY seq LIMIT ?2"
      )

    :ok = Sqlite3.bind(st, [since, limit])
    {:ok, rows} = Sqlite3.fetch_all(db, st)
    :ok = Sqlite3.release(db, st)

    events =
      for [seq, ref, old, new, writer, ts] <- rows do
        %{seq: seq, ref: ref, old: old, new: new, writer: writer, ts: ts}
      end

    {:reply, events, s}
  end

  def handle_call(:head_seq, _from, %{db: db} = s) do
    {:ok, st} = Sqlite3.prepare(db, "SELECT COALESCE(MAX(seq), 0) FROM journal")
    {:ok, [[seq]]} = Sqlite3.fetch_all(db, st)
    :ok = Sqlite3.release(db, st)
    {:reply, seq, s}
  end

  def handle_call({:nonce, nonce, ttl_s, peer}, _from, %{db: db} = s) do
    now = System.os_time(:second)
    {:ok, st} = Sqlite3.prepare(db, "DELETE FROM nonces WHERE expires_at < ?1")
    :ok = Sqlite3.bind(st, [now])
    :done = Sqlite3.step(db, st)
    :ok = Sqlite3.release(db, st)

    {:ok, st} =
      Sqlite3.prepare(
        db,
        "INSERT OR IGNORE INTO nonces(nonce, peer, expires_at) VALUES (?1, ?2, ?3)"
      )

    :ok = Sqlite3.bind(st, [nonce, peer, now + ttl_s])
    :done = Sqlite3.step(db, st)
    :ok = Sqlite3.release(db, st)
    {:ok, st} = Sqlite3.prepare(db, "SELECT peer FROM nonces WHERE nonce = ?1")
    :ok = Sqlite3.bind(st, [nonce])
    {:ok, [[owner]]} = Sqlite3.fetch_all(db, st)
    :ok = Sqlite3.release(db, st)
    {:reply, owner == peer, s}
  end

  # ---- open, check, reconcile --------------------------------------------

  # A file SQLite calls "not a database" or "malformed", or that fails
  # quick_check, is set aside and a fresh one created. Anything else that goes
  # wrong (a read-only directory, root-owned -wal files, a full disk, a busy
  # lock) is not corruption: it raises and the boot stops with the reason.
  defp open_or_quarantine(path) do
    case open_checked(path) do
      {:ok, db} ->
        db

      {:corrupt, reason} ->
        Logger.error("state.db unusable (#{inspect(reason)}); moving it aside and rebuilding")
        quarantine(path, "corrupt")
        {:ok, db} = open_checked(path)
        db
    end
  end

  defp open_checked(path) do
    {:ok, db} = Sqlite3.open(path)

    case init_schema(db) do
      :ok ->
        {:ok, db}

      {:error, reason} ->
        Sqlite3.close(db)

        if corrupt?(reason),
          do: {:corrupt, reason},
          else: raise("state.db #{path} could not be opened: #{inspect(reason)}")
    end
  end

  defp corrupt?({:quick_check, _}), do: true

  defp corrupt?(msg) when is_binary(msg),
    do: String.contains?(msg, ["file is not a database", "malformed"])

  defp corrupt?(_), do: false

  defp init_schema(db) do
    with :ok <- Sqlite3.execute(db, "PRAGMA busy_timeout=5000"),
         :ok <- Sqlite3.execute(db, "PRAGMA journal_mode=WAL"),
         :ok <- Sqlite3.execute(db, "PRAGMA synchronous=NORMAL"),
         :ok <- quick_check(db),
         :ok <-
           Sqlite3.execute(db, """
           CREATE TABLE IF NOT EXISTS journal(
             seq INTEGER PRIMARY KEY,
             ref TEXT NOT NULL,
             old_sha TEXT NOT NULL,
             new_sha TEXT NOT NULL,
             writer TEXT NOT NULL,
             ts INTEGER NOT NULL
           )
           """) do
      Sqlite3.execute(db, """
      CREATE TABLE IF NOT EXISTS nonces(nonce TEXT PRIMARY KEY, peer TEXT NOT NULL, expires_at INTEGER NOT NULL)
      """)
    end
  end

  defp quick_check(db) do
    with {:ok, st} <- Sqlite3.prepare(db, "PRAGMA quick_check"),
         {:ok, rows} <- Sqlite3.fetch_all(db, st) do
      Sqlite3.release(db, st)
      if rows == [["ok"]], do: :ok, else: {:error, {:quick_check, rows}}
    end
  end

  # Microsecond UTC stamp, so two files set aside in one second never collide.
  defp quarantine(path, tag) do
    stamp = "#{path}.#{tag}-#{Calendar.strftime(DateTime.utc_now(), "%Y%m%dT%H%M%S.%f")}"

    for ext <- ["", "-wal", "-shm"], File.exists?(path <> ext) do
      File.rename!(path <> ext, stamp <> ext)
    end

    :ok
  end

  # Make the journal agree with the repo. Returns :ok, or {:stale, head, main}
  # when the journal cannot belong to this repo and the caller must start over.
  # Repo.rev_parse and Repo.ancestor? raise when git itself fails, so a failing
  # git never reads as "no main" or "not an ancestor".
  defp reconcile(db, repo) do
    main = Repo.rev_parse(repo, @main)
    head = journal_head(db)

    cond do
      main == nil and head == nil ->
        :ok

      main != nil and head == main ->
        :ok

      head == nil ->
        rows = Rebuild.rows(repo)
        Logger.warning("journal empty beside a store at #{main}; rebuilt #{length(rows)} row(s)")
        insert_all(db, rows)

      main != nil and Repo.ancestor?(repo, head, main) ->
        rows = Rebuild.catch_up(repo, head)
        Logger.warning("journal behind the store; appended #{length(rows)} row(s)")
        insert_all(db, rows)

      true ->
        {:stale, head, main}
    end
  end

  defp insert_all(db, rows) do
    transaction(db, fn -> Enum.each(rows, &insert(db, &1)) end)
    :ok
  end

  # sha of the newest journal row for main, or nil when the journal has none.
  defp journal_head(db) do
    {:ok, st} =
      Sqlite3.prepare(
        db,
        "SELECT new_sha FROM journal WHERE ref = ?1 ORDER BY seq DESC LIMIT 1"
      )

    :ok = Sqlite3.bind(st, [@main])
    {:ok, rows} = Sqlite3.fetch_all(db, st)
    :ok = Sqlite3.release(db, st)

    case rows do
      [[sha]] -> sha
      [] -> nil
    end
  end

  # ---- writes -------------------------------------------------------------

  # BEGIN IMMEDIATE ... COMMIT, with a ROLLBACK when the body raises so a
  # failed step never leaves the connection inside an open transaction.
  defp transaction(db, fun) do
    :ok = Sqlite3.execute(db, "BEGIN IMMEDIATE")

    try do
      result = fun.()
      :ok = Sqlite3.execute(db, "COMMIT")
      result
    rescue
      e ->
        Sqlite3.execute(db, "ROLLBACK")
        reraise e, __STACKTRACE__
    end
  end

  defp insert(db, %{ref: ref, old: old, new: new, writer: writer, ts: ts}) do
    {:ok, st} =
      Sqlite3.prepare(
        db,
        "INSERT INTO journal(ref, old_sha, new_sha, writer, ts) VALUES (?1, ?2, ?3, ?4, ?5)"
      )

    try do
      :ok = Sqlite3.bind(st, [ref, old, new, writer, ts])
      :done = Sqlite3.step(db, st)
    after
      Sqlite3.release(db, st)
    end

    last_insert_rowid(db)
  end

  defp last_insert_rowid(db) do
    {:ok, st} = Sqlite3.prepare(db, "SELECT last_insert_rowid()")
    {:ok, [[id]]} = Sqlite3.fetch_all(db, st)
    :ok = Sqlite3.release(db, st)
    id
  end
end
