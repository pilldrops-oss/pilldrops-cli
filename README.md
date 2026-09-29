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

macOS and Linux, amd64 and arm64. `pilldrops upgrade` replaces the binary with
the latest release.

## Point it at your server

```bash
pilldrops login --api-url http://localhost:8080 --key "$(cat /var/lib/sandboxd/bootstrap.key)"
pilldrops whoami
```

The server defaults to `http://localhost:8080`, where the engine's quick start
runs one. `--api-url` or `PILLDROPS_API_URL` / `PILLDROPS_API_KEY` override the
stored login.

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
