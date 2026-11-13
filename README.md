# pilldrops CLI

The command line for [PillDrops](https://github.com/pilldrops-oss/pilldrops), a
self-hosted engine that runs untrusted code in isolated microVMs. Create
sandboxes, run commands, open a real shell, move files, expose ports, and drive
the whole thing from an AI agent over MCP.

## Install

```bash
npm install -g @pilldrops/cli       # fetches the binary for your platform
# or download pilldrops_<os>_<arch> from the latest release:
curl -fsSL -o pilldrops https://github.com/pilldrops-oss/pilldrops-cli/releases/latest/download/pilldrops_linux_amd64
chmod +x pilldrops
# or build it:
go install github.com/pilldrops-oss/pilldrops-cli/cmd/pilldrops@latest
```

macOS, Linux and Windows, amd64 and arm64 (`pilldrops_windows_amd64.exe`). On macOS and Linux `pilldrops upgrade` replaces the binary with
the latest release.

## Point it at your server

```bash
pilldrops login                              # asks for the server and the key
pilldrops whoami
```

On a terminal, `login` asks for the server URL, tells you where to mint a key
(`<server>/dashboard`, API keys), reads the key with echo off, checks it with
the server and stores it only if accepted; a rejected key exits 3 and stores
nothing. For scripts, or anywhere without a terminal, pass the key yourself:
there `login` without `--key` exits 2 rather than wait.

```bash
pilldrops login --api-url http://localhost:8080 --key "$(cat /var/lib/sandboxd/bootstrap.key)"
```

The server defaults to `http://localhost:8080`, where the engine's quick start
runs one. `--api-url` or `PILLDROPS_API_URL` / `PILLDROPS_API_KEY` override the
stored login, which lives in `~/.sandforge/config.json`.

## Use it

```bash
pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- python3 -c 'print(6*7)'
pilldrops connect <id>                       # a real PTY: editors, REPLs, installers
pilldrops start <id> --port 8000 -- python3 -m http.server 8000
pilldrops sandbox open <id>                  # the preview in a browser
pilldrops sandbox pause <id> && pilldrops sandbox resume <id>
pilldrops sandbox rm <id> --confirm
```

`pilldrops --help` lists every command. Output is machine-readable with
`--json`, exit codes are stable (0 ok, 1 error, 2 bad usage, 3 not
authenticated, and `exec` exits with the remote command's code), and nothing
prompts unless stdin is a terminal, so scripts and agents can call it safely.
`pilldrops ai` prints a guide written for AI agents.

## The shell

`pilldrops shell`, or bare `pilldrops` on a terminal, opens a `pilldrops> `
prompt that runs every command here, logging you in first if it has to. Quotes
and backslashes work as in a POSIX shell, though nothing is expanded. `help`
lists the commands; `exit`, `quit` or Ctrl-D leave. Ctrl-C stops the running
command, or abandons the line at the prompt, and a failing command just prints
its error. Each command runs in its own process, so `connect` comes back to the
prompt with the terminal as it was. It is for people: scripts and agents should
run commands directly. `pilldrops shell <id>` still means `connect <id>`.

## Everything else the API offers

```bash
pilldrops volume create data --size-mb 1024  # a disk that outlives its sandbox
pilldrops sandbox create --volume <vol-id>:/data
pilldrops agent ask <id> 'fix the failing test' --wait   # exits 1 unless the turn succeeded
pilldrops agent events <id> -f
pilldrops agent token create <id> --scope agent:read --ttl 3600
pilldrops cron create <id> '0 3 * * *' 'make backup' --name nightly
pilldrops webhook add https://example.com/hook --event sandbox.created --event sandbox.ended
pilldrops function create hello --start 'python3 main.py' --port 8000 --file main.py
pilldrops project list
pilldrops thread list
pilldrops sandbox integrations <id> add github --env GITHUB_PERSONAL_ACCESS_TOKEN=...
pilldrops chat <id> 'what is running?'
pilldrops chat-link <id> --read-only
pilldrops channels <id>
pilldrops audit --limit 20
```

`pilldrops api` reaches any `/v1` route without a typed verb, with the stored
credential and the same exit codes:

```bash
pilldrops api GET /v1/whoami
pilldrops api PATCH /v1/projects/<id> --data '{"name":"renamed"}'
echo '{"cap_usd":5}' | pilldrops api PATCH /v1/sandboxes/<id>/agent/budget --data -
```

`/v1/members` and `/v1/orgs` refuse API keys on purpose; use the dashboard for
those.

## For AI agents (MCP)

```bash
claude mcp add pilldrops -- pilldrops mcp
```

`pilldrops mcp` serves the platform as MCP tools over stdio using the stored
login. `pkg/mcp` is the same server as a library, with an HTTP transport for a
server that wants to mount it.

## Layout

- `cmd/pilldrops` — the CLI. Its version is in `internal/cli/version.go`; keep it
  in step with `pkg/mcp.ServerVersion`.
- `pkg/mcp` — the MCP server.
- `npm/` — the `@pilldrops/cli` package, a launcher that downloads the binary.

Releases: tag `vX.Y.Z` and the release workflow builds the four binaries and a
`SHA256SUMS`.

## License

[MIT](LICENSE).
