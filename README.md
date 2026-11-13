# cx-log

`tail` for Amazon Connect Agentic CX Designer logs. Prints the last N log events
of a workspace and optionally follows new ones, using `QueryLogs` from
[agentic-cx-designer-sdk-go](https://github.com/megaproaktiv/agentic-cx-designer-sdk-go).

## Setup

```bash
cp .env.example .env    # fill in region, workspace ID and API key
chmod 600 .env          # cx-log warns if other users can read it
go build -o cx-log .    # or: go install .
```

`.env`:

```bash
ACXD_REGION=eu-central-1
ACXD_WORKSPACE_ID=REPLACE_WITH_WORKSPACE_ID
ACXD_API_KEY=acxd_live_<prefix>.<secret>
```

Precedence: flags > environment variables > `.env` file. The API key can only
come from `.env` or the environment, never from a flag, so it does not leak into
shell history or `ps`. Use a programmatic user with a read-only or analyst role;
tailing logs needs no write access.

> The package spans several files: use `go run .`, not `go run main.go`.

## Usage

```bash
cx-log [flags]
```

```bash
cx-log                                   # last 10 events from the past hour
cx-log -n 50 -f                          # last 50 events, then follow (Ctrl-C to stop)
cx-log -f -n 0 --conversation-id <id>    # only new events of one conversation
cx-log -f --event-type Error,ModelError,DataRequestFailure
cx-log -n 200 --since 24h -o json | jq .
```

Flags accept one or two dashes (`-f`, `--since 24h`, `--since=24h`).

## Flags

### Connection

| Flag | Default | Description |
|---|---|---|
| `--env-file <file>` | `.env` | File with `ACXD_*` settings. Missing default `.env` is ignored; a missing explicit file is an error. |
| `--region <region>` | `$ACXD_REGION` | AWS region, e.g. `eu-central-1`. |
| `--workspace-id <id>` | `$ACXD_WORKSPACE_ID` | Workspace to read logs from. |
| `--endpoint-url <url>` | `$ACXD_ENDPOINT_URL` | Override the region-derived API endpoint (rarely needed). |

### Tail behavior

| Flag | Default | Description |
|---|---|---|
| `-n <N>` | `10` | Print the last N events, oldest first. `0` prints none (useful with `-f`). |
| `-f` | off | Follow: keep polling and print new events as they arrive. |
| `--since <duration>` | `1h` | How far back to look for the initial `-n` events, e.g. `30m`, `24h`. |
| `--interval <duration>` | `5s` | Poll interval in follow mode. |
| `--lag <duration>` | `2m` | In follow mode, re-scan this far back on each poll to catch late-ingested events. Duplicates are suppressed. Events arriving later than this are missed. |
| `--page-size <n>` | `100` | `QueryLogs` page size (5-100). |

### Filters

All filters combine (AND).

| Flag | Description |
|---|---|
| `--application-id <id>` | Only events for this application. |
| `--conversation-id <id>` | Only events for this conversation. |
| `--correlation-id <id>` | Only events for this correlation ID. |
| `--deployment-id <id>` | Only events for this deployment. |
| `--build-id <id>` | Only events for this build. |
| `--user-id <id>` | Only events for this end user. |
| `--language <code>` | Only events for this language code, e.g. `en-US`. |
| `--event-type <list>` | Comma-separated event types, e.g. `Error,ModelError`. Applied locally after fetching, because the API's search filter has no event-type field. |

### Output

| Flag | Default | Description |
|---|---|---|
| `-o <format>` | `text` | `text` (one line per event) or `json` (JSON Lines, properties as objects). |
| `-v` | off | Text output: also print common properties. |
| `--no-color` | off | Disable colors. Colors are also off when `NO_COLOR` is set or stdout is not a terminal. |
| `--debug` | off | Print query status (window, item count, progress, bytes scanned) per page to stderr. |
| `-h` | | Show help. |

## Output format

Text:

```
2026-11-13T14:10:03.123Z  NodeTraversal                nodeId=... flowId=MainFlow
```

Errors and failures are red; timeouts, escalations, fallbacks and triggered
guardrails are yellow.

JSON (`-o json`):

```json
{"eventTime":"2026-11-13T14:10:03.123Z","eventType":"NodeTraversal","eventProperties":{"flowId":"MainFlow"}}
```

## Debugging: "I see no logs"

### Test chat in the console is not logged

Conversations in the designer's **test chat** do not show up in `QueryLogs`
(or in `ListConversations`). In a workspace where the console test chat was
used all day, both APIs returned nothing and `QueryLogs` scanned 0 bytes. The
AWS docs split the lifecycle the same way:

- **Test** validates behavior *before deployment*. Per-turn events are shown in
  the console's debugger only: select an application message in the test
  transcript, or choose the debugger icon
  ([Testing your application](https://docs.aws.amazon.com/connect/latest/adminguide/acxd-testing.html)).
- **Observe** covers conversation history, transcripts and analytics *after
  deployment* ([Agentic CX designer](https://docs.aws.amazon.com/connect/latest/adminguide/acxd.html)).

The docs do not state the exclusion explicitly; it is what the API returns in practice.
`cx-log` therefore only sees traffic of **deployed** applications.

### Setup to get logs

1. In the designer, create a build of your application and deploy it.
2. Find the application ID (shown in the console, or via the SDK's
   `ListApplications`).
3. Start following, with query status on stderr:

   ```bash
   ./cx-log -f -n 0 --application-id <application-id> --debug
   ```

4. Hold a conversation through the **deployed** endpoint or channel, not the
   designer's test chat.

### Reading `--debug` output

Each `QueryLogs` page prints one line to stderr:

```
cx-log: debug: window=2026-11-13T12:11:27Z..2026-11-13T15:11:27Z order=desc items=0 progress=100.0% scanned=0B nextToken=false
```

| Field | Meaning |
|---|---|
| `window` | Absolute time range queried (UTC). |
| `order` | `desc` for the initial `-n` events, `asc` while following. |
| `items` | Events on this page, before the local `--event-type` filter. |
| `progress` | Query completion. `100.0%` means the result is final, not still loading. |
| `scanned` | Log bytes the service read. `0B` means there is no log data in the window at all, independent of filters. |
| `nextToken` | Whether another page follows. |

| Symptom | Likely cause |
|---|---|
| `scanned=0B` | No logged traffic in the window: only test chat was used, nothing is deployed, or the wrong workspace/region. |
| `scanned` > 0, `items=0` | Logs exist but your search filters (`--application-id`, `--conversation-id`, ...) exclude them. |
| `items` > 0 but nothing printed | `--event-type` filters everything out locally. |
| Events appear only after some delay | Ingestion lag. Follow mode re-scans `--lag` (default `2m`) per poll; raise it if events are missed. |

### Checklist

- `.env` points to the workspace and region you are working in. The SDK uses
  `https://api.acxd.connect.<region>.amazonaws.com`; a wrong region fails or
  returns an empty workspace.
- The programmatic user's role covers that workspace (an `AccessDenied` or
  `Unauthorized` error is printed and the tool exits non-zero).
- Widen the window for the initial read, e.g. `-n 50 --since 24h --debug`.

## Data protection

Log events can contain end-user utterances and identifiers (personal data).
Handle saved output according to your company's data protection policy.

## Development

```bash
go vet ./...
go test ./...
```
