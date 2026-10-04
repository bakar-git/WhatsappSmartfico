# Smartfico WhatsApp

A Go HTTP API for sending text messages using a linked WhatsApp account.
On first start, scan the **QR code printed in the terminal** from WhatsApp →
Linked devices → Link a device. The session is saved in `data/whatsapp.db`;
later starts reconnect without a QR. Expired pairing codes refresh automatically.
If you unlink the device from your phone, restart this service to pair again.

## Run

Requires Go 1.26 or newer and a C compiler for the SQLite driver.
Go can download the required toolchain automatically when `GOTOOLCHAIN=auto`.

```sh
go run .
```

The API defaults to `127.0.0.1:8080`. Keep the console open while pairing.
There is no browser login page. Run one process per data directory/session.

Optional environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `DATA_DIR` | `data` | Session, message jobs, and send history |
| `API_TOKEN` | unset | Require `Authorization: Bearer <token>` on all endpoints |

An API token is required if listening outside loopback. For example:

```sh
HTTP_ADDR=0.0.0.0:8080 API_TOKEN='replace-with-a-long-random-secret' go run .
```

## Send a message

```sh
curl -X POST http://127.0.0.1:8080/api/messages \
  -H 'Content-Type: application/json' \
  -d '{"number":"+923205401101","message":"Hello gorgeous"}'
```

If `API_TOKEN` is set, also pass `-H "Authorization: Bearer $API_TOKEN"`.
Use the full international number, with a leading `+` and no spaces or hyphens.
The message limit is 4096 characters. Only text is implemented in this version;
receipt PDFs, images, and other uploads can be added later.

The response is **202 Accepted**, which means queued, not delivered:

```json
{"id":"<job-id>","number":"+923205401101","message":"Hello gorgeous","status":"queued"}
```

Check the result:

```sh
curl http://127.0.0.1:8080/api/messages/<job-id>
curl http://127.0.0.1:8080/api/status
```

| Job status | Meaning |
| --- | --- |
| `queued` | Waiting for the background worker |
| `sending` | WhatsApp send is in progress |
| `sent` | WhatsApp acknowledged the send; includes `message_id`. This does not mean delivered or read. |
| `blocked` | Rate or warm-up limit reached; no message was sent |
| `unknown` | Send failed/timed out, or the process stopped during sending; verify in WhatsApp before resending |

Validation errors return 400, wrong content type 415, missing authentication 401,
disconnected WhatsApp 503, and a full queue 429. There can be up to 1000 pending jobs.
Queued jobs survive restarts and pause during disconnects. Blocked and uncertain
jobs are not automatically retried, to avoid unexpected or duplicate receipts.
Job records retain message content in SQLite; manage retention and back up the
data directory according to your needs.

## Library compatibility and pacing

Checked against the authors' documentation and source:

- [whatsmeow documentation and QR example](https://pkg.go.dev/go.mau.fi/whatsmeow)
- [whatsmeow source](https://github.com/tulir/whatsmeow)
- [whatsmeow-antiban documentation](https://github.com/TA-rathnayaka/whatsmeow-antiban)

Pinned versions are in `go.mod` and `go.sum`: whatsmeow
`v0.0.0-20260929112325-8b41cfe6d9c4` and whatsmeow-antiban `v0.1.0`.
Antiban is a separate package with no whatsmeow-specific types or dependencies,
so it is compatible with this latest whatsmeow version; both build together here.

This service uses antiban's **RateLimiter and Warmup components**, with the
conservative preset: 5 attempts/minute, 100/hour, 800/day, and an initial warm-up
ceiling of 15/day that increases over 10 days. It also limits identical message
content to 5 attempts/hour and applies randomized pacing. Send history is saved
before each network attempt and replayed on restart to preserve those limits.
Failed attempts count toward limits, conservatively.

The full 11-gate wrapper and typing-presence simulation are not enabled. The
wrapper's `snapshot` reads warm-up state outside its lock during a timer-based
save; this service uses the components directly with synchronous SQLite storage
to avoid that path. Whatsmeow uses the WhatsApp Web protocol, rather than Meta's
official Business API. Antiban pacing cannot guarantee an account will avoid bans.

## Verify

```sh
go test -race ./...
go vet ./...
go build ./...
```

API and worker tests use a fake WhatsApp transport and never send real messages.
Actual phone pairing and delivery require scanning the terminal QR with your account.
