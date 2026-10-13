defmodule TacitServer.Application do
  @moduledoc false
  use Application

  @impl true
  def start(_type, _args) do
    cfg = TacitServer.Config.load!()
    File.mkdir_p!(cfg.spool)
    TacitServer.Git.Repo.ensure!(cfg.repo)

    # No push can be in flight before the listener starts, so anything in the
    # spool is left over from a process that died mid-request.
    for f <- File.ls!(cfg.spool), do: File.rm_rf!(Path.join(cfg.spool, f))

    TacitServer.State.recover!()

    children =
      [
        {Registry, keys: :unique, name: TacitServer.Registry},
        {Phoenix.PubSub, name: TacitServer.PubSub, pool_size: 1},
        TacitServer.State,
        TacitServer.Auth.Cache,
        {TacitServer.Git.Lease, name: TacitServer.Git.Lease},
        {Task.Supervisor, name: TacitServer.Git.Tasks},
        listener(:tailnet_listener, TacitServer.Router, cfg.bind, cfg.port, 8)
      ] ++ public_listener(cfg)

    Supervisor.start_link(children, strategy: :one_for_one, name: TacitServer.Supervisor)
  end

  # The public path (Tailscale Funnel proxies to it) listens on loopback
  # only: nothing off this host reaches it except through tailscaled.
  defp public_listener(%{public_port: nil}), do: []

  defp public_listener(cfg),
    do: [listener(:public_listener, TacitServer.PublicRouter, {127, 0, 0, 1}, cfg.public_port, 4)]

  defp listener(id, plug, ip, port, acceptors) do
    Supervisor.child_spec(
      {Bandit,
       plug: plug,
       ip: ip,
       port: port,
       thousand_island_options: [
         num_acceptors: acceptors,
         transport_options: [send_timeout: 10_000, send_timeout_close: true]
       ]},
      id: id
    )
  end
end
