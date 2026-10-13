defmodule TacitServer.Feed do
  @moduledoc """
  SSE change feed: notify, don't carry. PubSub carries only `{:head_moved, seq}`;
  each stream process replays the journal from the client's `Last-Event-ID`
  (or `?since=`), so a slow or reconnecting client never loses an event and
  holds O(1) memory. Keepalive comment every 20 s.

  A client whose `Last-Event-ID` is past the journal's head (the journal was
  rebuilt and `seq` restarted, see `TacitServer.State.Rebuild`) first gets one
  `event: reset` frame carrying the new head, then the stream continues from
  that head. The client should resync with a `git fetch`.

  On the public path a stream lasts at most `public_stream_s`: it then sends
  one `event: reconnect` frame and ends, and the client reconnects (with
  `Last-Event-ID`, so nothing is lost) and authenticates again with a fresh
  token. A tailnet stream has no such end.
  """
  import Plug.Conn

  alias TacitServer.Config

  @topic "feed"
  @batch 200

  def notify(seq),
    do: Phoenix.PubSub.local_broadcast(TacitServer.PubSub, @topic, {:head_moved, seq})

  def stream(conn) do
    Process.flag(:max_heap_size, %{size: 2_000_000, kill: true, error_logger: true})

    if conn.assigns[:public] == true do
      Process.put(:feed_ends_at, now_ms() + Config.get(:public_stream_s) * 1000)
    end

    :ok = Phoenix.PubSub.subscribe(TacitServer.PubSub, @topic)

    last =
      case get_req_header(conn, "last-event-id") do
        [id] -> int(id)
        _ -> int(conn.query_params["since"])
      end

    conn =
      conn
      |> put_resp_content_type("text/event-stream")
      |> put_resp_header("cache-control", "no-cache")
      |> put_resp_header("x-accel-buffering", "no")
      |> send_chunked(200)

    head = TacitServer.State.head_seq()

    {reset, last} =
      if last > head,
        do: {"event: reset\ndata: {\"head\":#{head}}\n\n", head},
        else: {"", last}

    case chunk(conn, ": connected head=#{head}\n\n" <> reset) do
      {:ok, conn} -> flush(conn, last)
      {:error, _} -> conn
    end
  end

  defp flush(conn, last) do
    case TacitServer.State.since(last, @batch) do
      [] ->
        wait(conn, last)

      events ->
        case chunk(conn, Enum.map(events, &frame/1)) do
          {:ok, conn} -> flush(conn, List.last(events).seq)
          {:error, _} -> conn
        end
    end
  end

  defp wait(conn, last) do
    case Process.get(:feed_ends_at) do
      nil ->
        wait(conn, last, 20_000)

      ends ->
        if now_ms() >= ends,
          do: reconnect(conn),
          else: wait(conn, last, min(20_000, ends - now_ms()))
    end
  end

  defp reconnect(conn) do
    _ = chunk(conn, "event: reconnect\ndata: {}\n\n")
    conn
  end

  defp now_ms, do: System.monotonic_time(:millisecond)

  defp wait(conn, last, timeout) do
    receive do
      {:head_moved, _} -> drain() && flush(conn, last)
    after
      timeout ->
        case chunk(conn, ": keepalive\n\n") do
          {:ok, conn} -> flush(conn, last)
          {:error, _} -> conn
        end
    end
  end

  defp drain do
    receive do
      {:head_moved, _} -> drain()
    after
      0 -> true
    end
  end

  defp frame(e) do
    "id: #{e.seq}\nevent: head_moved\ndata: #{JSON.encode!(e)}\n\n"
  end

  defp int(nil), do: 0

  defp int(s) do
    case Integer.parse(s) do
      {n, _} when n >= 0 -> n
      _ -> 0
    end
  end
end
