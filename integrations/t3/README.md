# T3 Code token usage on one machine

This fork adds a durable SQLite usage ledger and a dashboard at **http://127.0.0.1:8318**.
It connects to an existing T3 desktop installation without rebuilding T3 or changing its provider authentication.

## Install on Windows

With Go 1.26 or later:

```powershell
powershell -ExecutionPolicy Bypass -File .\integrations\t3\install.ps1
```

The installer builds both binaries, installs them under `%USERPROFILE%\.t3-usage`,
starts the usage collector hidden, and creates a current-user Startup shortcut.
Use `-NoAutoStart` to omit the login shortcut. The dashboard remains loopback-only.

The dashboard reports input, output, cache reads, cache writes, reasoning, and total
tokens by model, provider, UTC day, T3 project, and thread. It refreshes every ten
seconds, supports date ranges, and exports exact counts as CSV. The Codex quota
panel shows the latest provider-reported snapshot and its timestamp; it is not an
estimate derived from token totals. An old snapshot stays visibly dated.

## How T3 integration works

`t3-usage` opens T3's `userdata/state.sqlite` **read-only** and maps its session
resume cursors to Codex and Claude session logs. Only token fields, session IDs,
model names, timestamps, project/thread labels, and quota windows enter the ledger.
It does not copy prompts, completions, auth files, API keys, or access tokens.

Codex cumulative usage becomes per-event deltas. Repeated notifications are ignored.
Claude usage is keyed by message ID, so repeated streaming entries update a record
instead of increasing the total. Claude cache reads/writes are added to its uncached
input; Codex cache counts are already included in input. Reasoning is already part
of output. Never add the cache or reasoning columns to the total.

File offsets, cumulative baselines, and usage rows commit in the same transaction.
Restarts resume from that checkpoint; unfinished final lines wait for the next scan.
The ledger uses WAL and synchronous FULL. Retained logs backfill automatically;
deleted logs and provider usage that was never recorded cannot be reconstructed.
Already-seen T3 session mappings remain available after T3 changes resume cursors.
Codex logs explicitly marked as T3 sessions can be imported even without a current
mapping; those appear as unlinked sessions. Claude imports are restricted to mapped
sessions and their subagent logs. Other T3 provider drivers are not imported.

This is a local companion integration, not an embedded T3 sidebar or a provider
traffic reroute. It leaves the installed T3 app, settings, credentials, and database
unchanged. This avoids interrupting active conversations and preserves native tools.

## Optional proxy traffic tracking

The normal CLIProxyAPI server also writes its native provider usage events into the
same ledger when started with `T3_USAGE_DB`. Configure/authenticate the proxy using
its normal workflow, then run:

```powershell
$env:T3_USAGE_DB = "$env:USERPROFILE\.t3-usage\usage.sqlite"
& "$env:USERPROFILE\.t3-usage\bin\cli-proxy-api.exe" --config .\config.yaml
```

Use `host: "127.0.0.1"`, a unique local API key, and local-only management in your
proxy config. The usage plugin is independent of the upstream ephemeral Redis
usage queue. It receives the upstream canonical accounting breakdown and persists
it locally. Proxy rows count provider attempts (including failures), while imported
rows represent provider usage updates/messages. They are not interchangeable counts.

Select **Proxy traffic** to inspect those records. T3 and proxy views intentionally
stay separate: routing a T3 session through this proxy can otherwise count the same
consumption twice. No proxy account is automatically imported or reauthenticated.
As with the upstream usage dispatcher, a hard process kill can lose events that
have not yet reached the ledger; committed records survive restarts.

## Paths and maintenance

Defaults:

- T3 source: `%USERPROFILE%\.t3\userdata\state.sqlite`
- Codex logs: `%USERPROFILE%\.codex\sessions`
- Claude logs: `%USERPROFILE%\.claude\projects`
- Ledger: `%USERPROFILE%\.t3-usage\usage.sqlite`
- Logs: `%USERPROFILE%\.t3-usage\stderr.log`

The CLI accepts `--t3-db`, `--codex-dir`, `--claude-dir`, `--db`, `--port`,
`--interval`, and `--once` for custom installations. For example:

```powershell
.\bin\t3-usage.exe --once --db .\usage.sqlite
& "$env:USERPROFILE\.t3-usage\stop.ps1"
& "$env:USERPROFILE\.t3-usage\start.ps1"
& "$env:USERPROFILE\.t3-usage\uninstall.ps1"
```

Uninstall stops only the recorded collector process and removes its login shortcut;
it retains your usage history. The ledger has no automatic retention limit. Back it
up with SQLite's backup API, or stop the collector and any connected proxy before
copying the database and its WAL/SHM files. Project/thread labels are local metadata;
CSV export includes those labels, so review exports before sharing them.

## Verify changes

```powershell
go test ./internal/localusage ./sdk/cliproxy/usage
go build -o bin/t3-usage.exe ./cmd/t3-usage
go build -o bin/cli-proxy-api.exe ./cmd/server
```
