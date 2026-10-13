defmodule TacitServer.Git.LeaseTest do
  use ExUnit.Case, async: true
  alias TacitServer.Git.Lease

  setup do
    name = :"lease_#{System.unique_integer([:positive])}"
    start_supervised!({Lease, name: name})
    %{lease: name}
  end

  test "holders run one at a time in FIFO order", %{lease: lease} do
    parent = self()

    tasks =
      for i <- 1..5 do
        Task.async(fn ->
          Lease.with_lease(lease, 5_000, fn ->
            send(parent, {:enter, i})
            Process.sleep(20)
            send(parent, {:exit, i})
          end)
        end)
      end

    Task.await_many(tasks, 5_000)
    events = for _ <- 1..10, do: receive(do: (e -> e))
    # No two holders overlap: every enter is immediately followed by its own exit.
    assert Enum.chunk_every(events, 2) |> Enum.all?(fn [{:enter, a}, {:exit, b}] -> a == b end)
  end

  test "a crashed holder frees the lease", %{lease: lease} do
    {pid, ref} =
      spawn_monitor(fn ->
        Lease.with_lease(lease, 5_000, fn ->
          Process.sleep(:infinity)
        end)
      end)

    Process.sleep(20)
    Process.exit(pid, :kill)
    assert_receive {:DOWN, ^ref, :process, ^pid, :killed}
    assert :got_it = Lease.with_lease(lease, 1_000, fn -> :got_it end)
  end

  test "a waiter that dies in the queue does not block the next", %{lease: lease} do
    holder = spawn(fn -> Lease.with_lease(lease, 5_000, fn -> Process.sleep(150) end) end)
    Process.sleep(10)
    waiter = spawn(fn -> Lease.with_lease(lease, 5_000, fn -> :never end) end)
    Process.sleep(10)
    Process.exit(waiter, :kill)
    assert :next = Lease.with_lease(lease, 2_000, fn -> :next end)
    assert is_pid(holder)
  end

  test "a waiter that gave up is not handed the lease when its turn comes", %{lease: lease} do
    parent = self()

    holder =
      Task.async(fn ->
        Lease.with_lease(lease, 1_000, fn ->
          send(parent, :held)
          Process.sleep(300)
        end)
      end)

    assert_receive :held

    # Times out while queued but stays alive, like a request handler that
    # answered 503 and went on to serve the next request on its connection.
    waiter =
      spawn(fn ->
        result =
          try do
            Lease.with_lease(lease, 50, fn -> :got_it end)
          catch
            :exit, {:timeout, _} -> :timed_out
          end

        send(parent, {:waiter, result})
        Process.sleep(:infinity)
      end)

    assert_receive {:waiter, :timed_out}, 1_000
    Task.await(holder)

    # The lease is free: a new caller gets it at once instead of queueing
    # behind a holder that never releases.
    assert :next = Lease.with_lease(lease, 1_000, fn -> :next end)
    Process.exit(waiter, :kill)
  end
end
