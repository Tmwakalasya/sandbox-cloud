# HTTP API

[Back to README](../README.md)

The control API is internal and has no application authentication. Access it
through Kubernetes port-forwarding, as described in the
[deployment guide](../deploy/README.md).

| Method and path | Result |
| --- | --- |
| `GET /healthz` | HTTP process health, not Kubernetes connectivity |
| `POST /v1/workspaces` | Create or retrieve a workspace with matching settings |
| `GET /v1/workspaces` | List managed workspaces |
| `GET /v1/workspaces/{id}` | Inspect workspace state |
| `DELETE /v1/workspaces/{id}` | Request asynchronous deletion |
| `PUT /v1/workspaces/{id}/files?path=relative/path` | Upload one file |
| `POST /v1/workspaces/{id}/commands` | Start a command |
| `GET /v1/workspaces/{id}/commands/{command}` | Read a retained result |
| `GET /v1/workspaces/{id}/commands/{command}/events` | Replay and follow NDJSON output |
| `POST /v1/workspaces/{id}/commands/{command}/cancel` | Request cancellation |

Workspace creation body:

```json
{"id":"sbx-0123456789abcdef01234567","ttl_seconds":1200}
```

Command submission body:

```json
{"id":"cmd-0123456789abcdef01234567","command":["go","test","./..."],"timeout_seconds":60}
```

Events carry either output or a final result:

```json
{"stream":"stdout","data":"ok example\n"}
{"stream":"result","result":{"id":"cmd-0123456789abcdef01234567","state":"succeeded","exit_code":0,"started_at":"2026-10-31T12:00:00Z","finished_at":"2026-10-31T12:00:01Z","truncated":false}}
```

Errors use `{"error":"message"}`. Validation errors return 400, missing objects
404, conflicts 409, oversized uploads 413, and process-start failures 422.
Kubernetes permission and quota failures are surfaced to callers. A broken stream
may end without a final result; the CLI reports an unknown outcome.
