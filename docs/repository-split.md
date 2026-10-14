# mobd repository split

On 2026-10-15 the daemon was extracted from `xrdavies/mob` at commit
`0606c832acf9a9c2729ca468a1ddb9ba3e3e1ead` into `xrdavies/mobd`.
The original history remains in the source repository; this repository starts
with a focused import rather than the entire monorepo history.

## Layout

- `cmd/mobd/`: CLI entrypoint and tests.
- `internal/`: API, configuration, macOS actuation, MCP, runners, service and state.
- `go.mod`: module `github.com/xrdavies/mobd`.
- `.env.example`, `config.example.json`: developer environment and user JSON examples.
- `.github/`: independent Apple signing, notarization, R2 publishing and installer.
- `docs/`: server architecture, API contract, runners and historical product plans.

The desktop app is in [xrdavies/mob](https://github.com/xrdavies/mob), with its frontend
and Tauri shell at that repository's root. No sibling checkout is needed to build
mobd. Go 1.27+ and Xcode Command Line Tools build the daemon; Node.js 24+ runs the
release/installer tests. `make build`, `make check` and `make race` run here.

## Local development and an existing service

Move the daemon developer `.env` into this checkout and export it as described in
the [README](../README.md#environment-variables-developers-only). Keep local secrets,
logs and binaries untracked. Historical `scripts/` runtime files may be retained
locally, but are ignored and are not part of this project or its build commands.

Existing user configuration, state, attachments and the `com.xrdavies.mobd`
LaunchAgent identity are unchanged. Repository extraction does not move user data.
If your LaunchAgent points at the old checkout's `bin/mobd`, finish active tasks,
stop the service, then build and start from this checkout after loading your
configuration. Use `--login` again if you want login startup. Check explicit
`mcpCommand`, executable paths and saved `--config` paths for old checkout locations.
A working directory may still point to any existing project, including mob.

## Releases

Create new `vX.Y.Z` tags in this repository independently of desktop tags. The
publisher reads and writes only `mobd/` in R2, including `mobd/install.sh`,
`mobd/latest.json` and `mobd/releases.json`. The desktop's `mob/` prefix is untouched.
Public paths and the website's manifest format are unchanged. Choose versions above
any existing public mobd release; release history is not reset by the repository move.

Create this repository's `release` environment using the
[release checklist](releases.md): 8 Secrets and 3 Variables. Existing mob environment
values are not inherited. Apple credentials and R2 infrastructure can be reused;
Tauri signing keys are not needed here. The source repository may be public or
private without changing the download URLs.
