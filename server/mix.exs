defmodule TacitServer.MixProject do
  use Mix.Project

  def project do
    [
      app: :tacit_server,
      version: "0.1.0",
      elixir: "~> 1.20",
      elixirc_options: [warnings_as_errors: true],
      releases: [tacit_server: [include_executables_for: [:unix], strip_beams: true]],
      start_permanent: Mix.env() == :prod,
      deps: deps()
    ]
  end

  # Run "mix help compile.app" to learn about applications.
  def application do
    [
      extra_applications: [:logger, :crypto, :public_key, :inets],
      mod: {TacitServer.Application, []}
    ]
  end

  # Run "mix help deps" to learn about dependencies.
  defp deps do
    [
      {:bandit, "~> 1.12"},
      {:plug, "~> 1.20"},
      {:phoenix_pubsub, "~> 2.4"},
      {:exqlite, "~> 0.42"},
      {:exile, "~> 0.15"},
      {:telemetry, "~> 1.4"}
    ]
  end
end
