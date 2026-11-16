# history

`crabbox history` lists recorded remote command runs from the broker. Each run is
created by [`crabbox run`](run.md) (and run-backed flows such as
[`crabbox job run`](job.md) or `capsule replay`) and persists its state, phase,
exit code, duration, and telemetry on the coordinator. This command requires a
configured broker by default. Existing opt-in local records are also readable
without a broker; use `--source local` to select local data explicitly.

```sh
crabbox history
crabbox history --lease cbx_0a1b2c3d4e5f
crabbox history --owner alice@example.com
crabbox history --org example-org --json
crabbox history --state failed --limit 100
```

## Flags

```text
--lease <lease-id>   Filter to runs on a single lease.
--owner <email>      Filter to runs owned by an email.
--org <name>         Filter to runs scoped to an org.
--state <state>      Filter by state: running, succeeded, or failed.
--limit <n>          Maximum runs to return (default 50, broker-capped at 500).
--json               Print the raw run records as JSON.
--source <source>     local, coordinator, or all; configured-broker default is unchanged.
```

## Output

The default output prints one line per run with the run ID, lease ID, lease slug,
state, phase, exit code, duration, start time, an optional resource summary, and
the executed command:

```text
run_a1b2c3d4 cbx_0a1b2c3d4e5f swift-crab succeeded phase=command.finished exit=0 duration=42.1s started=2026-05-29T10:15:04Z resources=load=0.42 mem=38% disk=12% command=npm test
```

Missing values render as `-` (for example a still-running run has no exit code).
The `resources=…` segment appears only when a coordinator-backed Linux run
captured [telemetry](../features/history-logs.md). `--json` emits the full run
records, including the start/end telemetry snapshots, JUnit result summaries, and
classification fields (`blockedStage`, `retryLikely`) when present.

Use a run ID from this list with [`logs`](logs.md), [`events`](events.md), or
[`attach`](attach.md) to inspect a specific run.

## Close orphaned runs

A run stays `running` on the broker until its client reports the finish. A
client that is killed (`SIGKILL`, a tool timeout, a sleeping laptop) never
does, and the record stays open. `history reap` closes such runs:

```sh
crabbox history reap --dry-run
crabbox history reap --older-than 2h --json
```

```text
--older-than <duration>  Least time since the run's last event (default 1h, minimum 1m).
--limit <n>              Maximum running runs to inspect (default 500).
--dry-run                Report what would be closed; change nothing.
--json                   Print one record per inspected run.
```

A run is closed only when it has had no event for `--older-than` and every
lease it used is released, expired, failed or gone. A run on a live lease is
kept, however long it has been silent. A run that never got a lease is kept
until it has been silent for 6 hours. If a lease lookup fails, the run is kept.

Just before the close, the command reads the run again. It keeps the run if the
run finished or got a new event since the first read. The broker cannot close a
run on a condition, so a client that reports in the moment between that second
read and the close still loses its record; keep `--older-than` well above the
longest silence of a healthy command.

A closed run becomes `failed` with exit code 1 and `blockedStage`
`client-orphaned`; its log says that the command's real exit status is unknown.
The command never stops a lease. It reads and closes only the runs your token
can write. Exit code 1 means at least one run could not be closed.

## Related docs

Local records are created by `run --record-local`, not by reading history.
`history prune --source local` applies the bounded retention policy, and
`history delete <run-id> --source local` removes one inactive local record.
Neither operation stops a lease or changes coordinator records. Active writers
are not pruned. Local source output includes provenance and incomplete status;
see [private local history](../features/history-logs.md#private-local-history).

- [logs](logs.md) — print captured run logs.
- [events](events.md) — print phase and stream events.
- [attach](attach.md) — follow a live run's events.
- [results](results.md) — parsed test-result summaries.
- [History and logs](../features/history-logs.md) — concept overview.
