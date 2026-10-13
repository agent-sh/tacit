import Config

config :logger, level: :warning

# The app boots before test files run, so the test environment is fixed here.
config :tacit_server,
  env: %{
    "TACIT_DATA" => Path.expand("../.test-data/#{System.pid()}", __DIR__),
    "TACIT_BIND" => "127.0.0.1",
    "TACIT_PORT" => "17447",
    "TACIT_PUBLIC_PORT" => "17448",
    "TACIT_WHOIS" => "static:",
    "TACIT_ALLOW_LOOPBACK" => "1"
  }
