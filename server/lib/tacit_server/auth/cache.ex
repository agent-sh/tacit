defmodule TacitServer.Auth.Cache do
  @moduledoc """
  ETS tables with per-entry TTL: WhoIs results and public keys (`get`/`put`),
  and the public path's failure counters (`fails`/`bump_fail`), which live in
  their own table with a hard size bound.
  """
  use GenServer

  @table :tacit_auth_cache
  @fails :tacit_public_fails

  def start_link(_), do: GenServer.start_link(__MODULE__, :ok, name: __MODULE__)

  def get(key) do
    now = System.monotonic_time(:millisecond)

    case :ets.lookup(@table, key) do
      [{^key, v, exp}] when exp > now -> {:ok, v}
      _ -> :miss
    end
  end

  def put(key, v, ttl_ms),
    do: :ets.insert(@table, {key, v, System.monotonic_time(:millisecond) + ttl_ms})

  def delete(key), do: :ets.delete(@table, key)

  @doc """
  The failure count at key, or `:full` when key has no counter and the table
  holds `max` counters already (the caller treats that as over the limit:
  its memory stays bounded whatever the number of sources).
  """
  def fails(key, max) do
    now = System.monotonic_time(:millisecond)

    case :ets.lookup(@fails, key) do
      [{^key, n, exp}] when exp > now -> n
      _ -> if :ets.info(@fails, :size) >= max, do: :full, else: 0
    end
  end

  @doc """
  Add one failure at key. A new counter lives ttl_ms; bumps do not extend it
  (fixed windows: the window is in the key). Nothing is added past `max`.
  """
  def bump_fail(key, ttl_ms, max) do
    if :ets.member(@fails, key) or :ets.info(@fails, :size) < max do
      exp = System.monotonic_time(:millisecond) + ttl_ms
      :ets.update_counter(@fails, key, {2, 1}, {key, 0, exp})
    end

    :ok
  end

  @doc false
  def clear_fails, do: :ets.delete_all_objects(@fails)

  @impl true
  def init(:ok) do
    :ets.new(@table, [:set, :named_table, :public, read_concurrency: true])
    :ets.new(@fails, [:set, :named_table, :public, write_concurrency: true])
    Process.send_after(self(), :sweep, 60_000)
    {:ok, %{}}
  end

  @impl true
  def handle_info(:sweep, s) do
    now = System.monotonic_time(:millisecond)

    for t <- [@table, @fails],
        do: :ets.select_delete(t, [{{:_, :_, :"$1"}, [{:<, :"$1", now}], [true]}])

    Process.send_after(self(), :sweep, 60_000)
    {:noreply, s}
  end
end
