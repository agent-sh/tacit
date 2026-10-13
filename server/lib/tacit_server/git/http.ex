defmodule TacitServer.Git.Http do
  @moduledoc """
  Git smart HTTP, the way `http-backend.c` does it, without CGI.

    GET  /git/info/refs?service=git-upload-pack|git-receive-pack
    POST /git/git-upload-pack
    POST /git/git-receive-pack

  Reads need a tailnet identity, or a key token on the public path (the auth
  plug). Writes need a signed token naming a writer `<harness>@<host>`; the
  writer is handed to the pre-receive hook through TACIT_WRITER so it can
  enforce the inbox path scope. Two pushes are refused before git runs, on
  the advertisement and on the pack: any push on the public path (it is
  read-only for every key), and any push by a `reader@<host>` key.
  """
  import Plug.Conn
  require Logger

  alias TacitServer.{Config, Feed, State}
  alias TacitServer.Auth.KeyId
  alias TacitServer.Git.{Lease, Repo}

  @zero String.duplicate("0", 40)

  def info_refs(conn) do
    svc = conn.query_params["service"]

    cond do
      svc not in ["git-upload-pack", "git-receive-pack"] ->
        send_resp(conn, 403, "service not enabled")

      svc == "git-receive-pack" and conn.assigns[:writer] == nil ->
        unauthorized(conn, "git push needs a writer token")

      svc == "git-receive-pack" and push_refusal(conn) != nil ->
        send_resp(conn, 403, push_refusal(conn))

      true ->
        proto = git_protocol(conn)
        cmd = String.trim_leading(svc, "git-")

        {out, 0} =
          Repo.git(Config.get(:repo), [cmd, "--http-backend-info-refs", "."],
            env: proto_env(proto)
          )

        prefix =
          if proto =~ ~r/version=2/ do
            ""
          else
            pkt("# service=#{svc}\n") <> "0000"
          end

        conn
        |> no_cache()
        |> put_resp_content_type("application/x-#{svc}-advertisement", nil)
        |> send_resp(200, prefix <> out)
    end
  end

  def upload_pack(conn) do
    with :ok <- expect_type(conn, "application/x-git-upload-pack-request"),
         {:ok, body, conn} <- read_body_capped(conn, Config.get(:max_upload_request)),
         {:ok, body} <- maybe_gunzip(conn, body, Config.get(:max_upload_request)) do
      conn =
        conn
        |> no_cache()
        |> put_resp_content_type("application/x-git-upload-pack-result", nil)
        |> send_chunked(200)

      argv = ["git", "-C", Config.get(:repo), "upload-pack", "--stateless-rpc", "."]

      # The conn of the last chunk sent survives in the process dictionary: the
      # stream raises on a non-zero exit only after its last element, past the
      # point where the reducer's accumulator is lost.
      Process.put(:upload_pack_conn, conn)

      try do
        argv
        |> with_env(proto_env(git_protocol(conn)))
        |> Exile.stream!(input: [body], ignore_epipe: true, stderr: :disable)
        |> Enum.reduce_while(conn, fn data, conn ->
          case chunk(conn, data) do
            {:ok, conn} ->
              Process.put(:upload_pack_conn, conn)
              {:cont, conn}

            {:error, _} ->
              {:halt, conn}
          end
        end)
      rescue
        # upload-pack exits non-zero on a request it cannot parse, after the
        # 200 and any output were sent; there is nothing left to answer with.
        e in Exile.Stream.AbnormalExit ->
          if e.exit_status in [1, 128] do
            Logger.warning("upload-pack failed: #{Exception.message(e)}")
            Process.get(:upload_pack_conn)
          else
            reraise e, __STACKTRACE__
          end
      after
        Process.delete(:upload_pack_conn)
      end
    else
      {:error, status, msg} -> send_resp(conn, status, msg)
      {:error, status, msg, conn} -> refuse(conn, status, msg)
    end
  end

  def receive_pack(conn) do
    writer = conn.assigns[:writer]

    with true <- writer != nil || {:error, 401, "git push needs a writer token"},
         nil <- push_refusal(conn),
         :ok <- expect_type(conn, "application/x-git-receive-pack-request"),
         {:ok, spool, conn} <- spool_body(conn, Config.get(:max_push_bytes)) do
      # The spool goes before the answer, so a client that has its response can
      # rely on nothing being left behind.
      result =
        try do
          run_push(spool, writer, git_protocol(conn))
        after
          File.rm(spool)
        end

      case result do
        {:ok, out} ->
          conn
          |> no_cache()
          |> put_resp_content_type("application/x-git-receive-pack-result", nil)
          |> send_resp(200, out)

        {:error, :busy} ->
          conn |> put_resp_header("retry-after", "5") |> send_resp(503, "store busy, retry")

        {:error, :git_failed} ->
          send_resp(conn, 400, "receive-pack could not process this request")
      end
    else
      {:error, status, msg} -> send_resp(conn, status, msg)
      {:error, status, msg, conn} -> refuse(conn, status, msg)
      # A refused push: the pack was never read, so the connection goes.
      msg when is_binary(msg) -> refuse(conn, 403, msg)
    end
  end

  # Why this authenticated request may not push, or nil. The public path is
  # read-only for every key; a reader key is read-only on every path.
  defp push_refusal(conn) do
    cond do
      conn.assigns[:public] == true -> "the public path is read-only: push over the tailnet"
      KeyId.reader?(conn.assigns[:writer]) -> "reader keys cannot push"
      true -> nil
    end
  end

  # Answer a request whose body was partly or wholly read. The conn must be the
  # one `read_body` returned (the stale one would make Bandit wait for body
  # bytes already consumed), and the connection is closed because the rest of
  # an oversize body is never read.
  defp refuse(conn, status, msg) do
    conn |> put_resp_header("connection", "close") |> send_resp(status, msg)
  end

  # Inside the lease: refs before, receive-pack fed from the spool, refs after,
  # journal the diff. The hook sees TACIT_WRITER; rejected pushes change no refs.
  defp run_push(spool, writer, proto) do
    repo = Config.get(:repo)

    try do
      do_push(repo, spool, writer, proto)
    catch
      # Only the wait for the lease is "busy". A timeout of State.append inside
      # the lease means the push already moved main and must not be retried.
      :exit, {:timeout, {GenServer, :call, [Lease | _]}} -> {:error, :busy}
    end
  end

  defp do_push(repo, spool, writer, proto) do
    Lease.with_lease(30_000, fn ->
      before = Repo.refs(repo)

      # TACIT_WRITER is what the hook enforces. The committer identity is what
      # receive-pack writes into the reflog, which is where the journal's
      # writer column is rebuilt from (TacitServer.State.Rebuild).
      env =
        [
          {"TACIT_WRITER", writer},
          {"GIT_COMMITTER_NAME", writer},
          {"GIT_COMMITTER_EMAIL", writer}
        ] ++ proto_env(proto)

      argv = [
        "git",
        "-C",
        repo,
        "-c",
        "core.hooksPath=#{Config.get(:hooks)}",
        "receive-pack",
        "--stateless-rpc",
        "."
      ]

      # A request receive-pack cannot parse (garbage, a cut-off pack) makes it
      # exit 1 or 128. That is the client's mistake, not a server fault. Any
      # other exit (a signal, an OOM kill) is raised, after the refs below are
      # diffed in case it moved one before dying.
      result =
        try do
          out =
            argv
            |> with_env(env)
            |> Exile.stream!(input: File.stream!(spool, 65_536), stderr: :disable)
            |> Enum.into([])
            |> IO.iodata_to_binary()

          {:ok, out}
        rescue
          e in Exile.Stream.AbnormalExit ->
            if e.exit_status in [1, 128] do
              Logger.warning("receive-pack for #{writer} failed: #{Exception.message(e)}")
              {:error, :git_failed}
            else
              {:crash, e, __STACKTRACE__}
            end
        end

      after_ = Repo.refs(repo)

      case Repo.diff_refs(before, after_) do
        [] ->
          Logger.info("push by #{writer}: no ref changed (refused by the hook or nothing new)")

        rows ->
          # A push may add or remove keys/ files: the next lookup reads the set again.
          TacitServer.Auth.Cache.delete(:enrolled_keys)
          {:ok, seq} = State.append(rows, writer)
          Feed.notify(seq)
          Logger.info("push by #{writer}: #{length(rows)} ref(s), seq #{seq}")
      end

      case result do
        {:crash, e, stacktrace} -> reraise e, stacktrace
        result -> result
      end
    end)
  end

  # ---- helpers ------------------------------------------------------------

  # Stream the request body to a spool file, inflating gzip on the fly, so
  # the lease is held only for the git run, not for the client's upload. The
  # cap applies to the bytes on the wire and, for gzip, to the inflated bytes
  # as well, so a few KiB of compressed zeros cannot fill the disk.
  defp spool_body(conn, cap) do
    path =
      Path.join(
        Config.get(:spool),
        "push-" <> Base.url_encode64(:crypto.strong_rand_bytes(9), padding: false)
      )

    z = if gzip?(conn), do: open_inflate(), else: nil
    {:ok, fh} = File.open(path, [:write, :binary, :raw])

    result =
      try do
        spool_loop(conn, fh, z, 0, 0, cap)
      catch
        kind, reason ->
          File.rm(path)
          :erlang.raise(kind, reason, __STACKTRACE__)
      after
        File.close(fh)
        if z, do: :zlib.close(z)
      end

    case result do
      {:ok, conn} ->
        {:ok, path, conn}

      err ->
        File.rm(path)
        err
    end
  end

  defp spool_loop(conn, fh, z, n, out, cap) do
    case read_body(conn, length: 1_048_576, read_length: 65_536, read_timeout: 30_000) do
      {:error, e} ->
        {:error, 400, "body: #{inspect(e)}", conn}

      {status, data, conn} when status in [:ok, :more] ->
        n = n + byte_size(data)

        with :ok <- check_cap(n, cap, "push"),
             {:ok, out} <- spool_chunk(fh, z, data, out, cap),
             :ok <- finish_inflate(status, z) do
          if status == :ok, do: {:ok, conn}, else: spool_loop(conn, fh, z, n, out, cap)
        else
          {:error, status, msg} -> {:error, status, msg, conn}
        end
    end
  end

  defp spool_chunk(fh, nil, data, out, _cap) do
    :ok = IO.binwrite(fh, data)
    {:ok, out}
  end

  defp spool_chunk(fh, z, data, out, cap) do
    with {:ok, out, :ok} <-
           inflate(z, data, out, cap, "push", :ok, fn pieces, :ok ->
             :ok = IO.binwrite(fh, pieces)
           end) do
      {:ok, out}
    end
  end

  defp check_cap(n, cap, what) when n > cap, do: {:error, 413, "#{what} larger than #{cap} bytes"}
  defp check_cap(_n, _cap, _what), do: :ok

  defp open_inflate do
    z = :zlib.open()
    :ok = :zlib.inflateInit(z, 31)
    z
  end

  # At the end of the body the gzip stream must have ended too: safeInflate
  # reports `finished` when it runs out of input, not at the end of the stream,
  # so a cut-off body would otherwise be handed to git as if it were whole.
  defp finish_inflate(:more, _z), do: :ok
  defp finish_inflate(:ok, nil), do: :ok

  defp finish_inflate(:ok, z) do
    :ok = :zlib.inflateEnd(z)
    :ok
  rescue
    ErlangError -> {:error, 400, "truncated gzip body"}
  end

  # Feed `data` through the inflater, folding each piece of output into `acc`
  # with `fun.(pieces, acc)`. `safeInflate` yields at most 16 KiB per call (OTP
  # 28), so a compressed chunk of zeros never becomes a gigabyte binary before
  # the cap sees it. `total` is the inflated size so far. Returns
  # {:ok, new_total, acc}, {:error, 413, _} past `cap`, or {:error, 400, _}
  # for a corrupt stream.
  defp inflate(z, data, total, cap, what, acc, fun) do
    drain_inflate(:zlib.safeInflate(z, data), z, total, cap, what, acc, fun)
  rescue
    ErlangError -> {:error, 400, "bad gzip body"}
  end

  defp drain_inflate({state, pieces}, z, total, cap, what, acc, fun) do
    total = total + IO.iodata_length(pieces)

    with :ok <- check_cap(total, cap, "inflated #{what}") do
      acc = fun.(pieces, acc)

      case state do
        :finished -> {:ok, total, acc}
        :continue -> drain_inflate(:zlib.safeInflate(z, []), z, total, cap, what, acc, fun)
      end
    end
  end

  defp read_body_capped(conn, cap) do
    case read_body(conn, length: cap, read_length: 65_536, read_timeout: 30_000) do
      {:ok, body, conn} -> {:ok, body, conn}
      {:more, _, conn} -> {:error, 413, "request larger than #{cap} bytes", conn}
      {:error, e} -> {:error, 400, "body: #{inspect(e)}", conn}
    end
  end

  defp maybe_gunzip(conn, body, cap) do
    if gzip?(conn) do
      z = open_inflate()

      try do
        case inflate(z, body, 0, cap, "request", [], fn pieces, acc -> [acc | pieces] end) do
          {:ok, _total, acc} ->
            case finish_inflate(:ok, z) do
              :ok -> {:ok, IO.iodata_to_binary(acc)}
              {:error, status, msg} -> {:error, status, msg, conn}
            end

          {:error, status, msg} ->
            {:error, status, msg, conn}
        end
      after
        :zlib.close(z)
      end
    else
      {:ok, body}
    end
  end

  defp gzip?(conn), do: get_req_header(conn, "content-encoding") in [["gzip"], ["x-gzip"]]

  defp expect_type(conn, type) do
    case get_req_header(conn, "content-type") do
      [^type] ->
        :ok

      [t] when is_binary(t) ->
        if String.starts_with?(t, type), do: :ok, else: {:error, 415, "expected #{type}"}

      _ ->
        {:error, 415, "expected #{type}"}
    end
  end

  defp git_protocol(conn) do
    case get_req_header(conn, "git-protocol") do
      [v] -> v
      _ -> ""
    end
  end

  defp proto_env(""), do: []
  defp proto_env(v), do: [{"GIT_PROTOCOL", v}]

  defp with_env(argv, []), do: argv
  defp with_env(argv, env), do: ["env" | Enum.map(env, fn {k, v} -> "#{k}=#{v}" end)] ++ argv

  defp pkt(s) do
    len = byte_size(s) + 4
    String.pad_leading(String.downcase(Integer.to_string(len, 16)), 4, "0") <> s
  end

  defp no_cache(conn) do
    conn
    |> put_resp_header("expires", "Fri, 01 Jan 1980 00:00:00 GMT")
    |> put_resp_header("pragma", "no-cache")
    |> put_resp_header("cache-control", "no-cache, max-age=0, must-revalidate")
  end

  defp unauthorized(conn, msg) do
    conn
    |> put_resp_header("www-authenticate", ~s(Basic realm="tacit"))
    |> send_resp(401, msg)
  end

  @doc false
  def zero_sha, do: @zero
end
