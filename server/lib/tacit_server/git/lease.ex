defmodule TacitServer.Git.Lease do
  @moduledoc """
  FIFO write lease for the store repo. The GenServer is only a lock server:
  the work (receive-pack, gc) runs in the caller, so a slow push never blocks
  the lease process itself. The holder is monitored, so a crashed pusher
  frees the lease.

  A caller that gives up waiting (the `GenServer.call` timeout) cancels its
  place by the reference it queued with. Without that, its turn would still come,
  the lease would be handed to a process that is no longer waiting (for example a
  request handler serving the next request on a keep-alive connection), and
  nobody would ever release it.
  """
  use GenServer

  def start_link(opts),
    do: GenServer.start_link(__MODULE__, :ok, name: Keyword.fetch!(opts, :name))

  @doc "Run fun while holding the lease. Waits up to timeout ms for the turn."
  def with_lease(server \\ __MODULE__, timeout, fun) do
    ref = make_ref()

    mref =
      try do
        GenServer.call(server, {:acquire, ref}, timeout)
      catch
        :exit, reason ->
          GenServer.cast(server, {:cancel, ref})
          exit(reason)
      end

    try do
      fun.()
    after
      GenServer.cast(server, {:release, mref})
    end
  end

  # holder: {pid, monitor ref, request ref}; queue holds {from, request ref}.
  @impl true
  def init(:ok), do: {:ok, %{holder: nil, queue: :queue.new()}}

  @impl true
  def handle_call({:acquire, ref}, {pid, _tag}, %{holder: nil} = s) do
    mref = Process.monitor(pid)
    {:reply, mref, %{s | holder: {pid, mref, ref}}}
  end

  def handle_call({:acquire, ref}, from, s),
    do: {:noreply, %{s | queue: :queue.in({from, ref}, s.queue)}}

  @impl true
  def handle_cast({:release, mref}, %{holder: {_, mref, _}} = s), do: {:noreply, next(s)}
  def handle_cast({:release, _stale}, s), do: {:noreply, s}

  def handle_cast({:cancel, ref}, s) do
    s = %{s | queue: :queue.filter(fn {_from, r} -> r != ref end, s.queue)}

    case s.holder do
      # Granted in the instant the caller gave up: hand it on.
      {_, _, ^ref} -> {:noreply, next(s)}
      _ -> {:noreply, s}
    end
  end

  @impl true
  def handle_info({:DOWN, mref, :process, _, _}, %{holder: {_, mref, _}} = s),
    do: {:noreply, next(s)}

  def handle_info(_other, s), do: {:noreply, s}

  defp next(%{holder: {_, old, _}} = s) do
    Process.demonitor(old, [:flush])

    case :queue.out(s.queue) do
      {{:value, {{pid, _} = from, ref}}, q} ->
        mref = Process.monitor(pid)
        GenServer.reply(from, mref)
        %{s | holder: {pid, mref, ref}, queue: q}

      {:empty, q} ->
        %{s | holder: nil, queue: q}
    end
  end
end
