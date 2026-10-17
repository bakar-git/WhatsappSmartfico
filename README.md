# Smartfico WhatsApp

A Go HTTP API for sending text messages, PDF receipts, and JPEG/PNG images using a linked WhatsApp account.
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
There is no browser login page. An OS file lock allows one WhatsApp service per
OS user, even if another launch uses a different port or data directory.

Optional environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `DATA_DIR` | `data` | Session, message jobs, and send history |
| `API_TOKEN` | unset | Require `Authorization: Bearer <token>` on all endpoints |
| `BACKGROUND_ON_CLOSE` | `false` | Keep the service running when its console closes |
| `START_ON_LOGIN` | `true` | Register the installed binary to start at current-user sign-in |
| `UPDATE_ENABLED` | `true` | Check/download/restart for newer public GitHub releases |
| `UPDATE_MANIFEST_URL` | GitHub latest `default.release` asset | JSON manifest with version, link, and SHA-256 |
| `UPDATE_CHECK_INTERVAL` | `6h` | Update polling interval, minimum `1m` |

You can also put these settings in a `.env` file. Start with:

```sh
cp .env.example .env
```

```dotenv
HTTP_ADDR=127.0.0.1:8080
DATA_DIR=data
API_TOKEN=your-per-installation-secret
BACKGROUND_ON_CLOSE=true
```

For a built app, place `.env` beside the executable:

```text
whatsapp-service/
  smartfico-whatsapp
  .env
  data/
```

```sh
go build -o smartfico-whatsapp .
./smartfico-whatsapp
```

The app loads `.env` beside the executable first, even when launched from another
working directory. If that file is absent, it looks in the working directory,
which supports `go run .`. It reads only the first file found; existing process
environment variables take precedence, including values explicitly set empty.
Relative `DATA_DIR` paths resolve beside the loaded `.env`; without a file they
resolve from the working directory. A missing `.env` is optional; malformed or
unreadable files stop startup. Restart after editing settings; rebuilding is not
required. `.env` is ignored by Git and is not embedded in the binary.

## Keep running after closing the console

Set `BACKGROUND_ON_CLOSE=true` in `.env`, build the app, and launch that binary.
On Windows, Linux, and macOS, it starts a detached WhatsApp service and displays
its QR codes/logs in the launcher's console. Closing that console or pressing
Ctrl+C closes the log viewer; the service continues sending through the API.
With `BACKGROUND_ON_CLOSE=false`, the app runs in the foreground and Ctrl+C
stops it normally. Changing the setting requires stopping the existing service
and starting it again.

This is the background-process fallback; it does not create a tray icon. Windows
console close events terminate attached processes, so the service runs detached
from the outset to survive closing the window. See
[Microsoft's console close documentation](https://learn.microsoft.com/en-us/windows/console/ctrl-close-signal).

The launcher prints the background PID and full log path. Stop that PID through
Windows Task Manager, macOS Activity Monitor, or `kill -TERM <pid>` on Linux/macOS.
Reopening the app while it is running reports the existing instance and does not
start another session. The lock is released by the OS when the service exits,
including after a crash; do not manually delete its lock file while it is running.

Instance files are under the current user's cache directory in
`smartfico-whatsapp/`: `instance.lock`, `service.pid`, and (in background mode)
`service.log`. Pairing QR codes continue to refresh in that log if the viewer is
closed before pairing. Keep the viewer open until you scan successfully, or read
the latest log in a terminal. The WhatsApp session/database still use `DATA_DIR`.

Use a **built binary** for this setting; `go run` uses a temporary executable
and is intended for foreground development. `START_ON_LOGIN=true` now registers
the installed binary for the current user's next login. It does not run before
login or keep the process alive through logout/shutdown. See [UPDATES.md](UPDATES.md)
for OS behavior, configuration, GitHub `default.release` manifests, publishing commands,
checksum validation, and update rollback.

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
The message limit is 4096 characters. For PDF/image blobs, use multipart uploads
with `number`, `type=pdf` or `type=image`, `file`, and optional `caption`.
Files can be up to 16 MiB. See [API.md](API.md) for all endpoints, browser JS,
upload examples, authentication, and CORS behavior for `*.smartfico.test` and
`*.smartfico.app`.

The response is **202 Accepted**, which means queued, not delivered:

```json
{"id":"<job-id>","number":"+923205401101","message":"Hello gorgeous","status":"queued"}
```

Check the result:

```sh
curl http://127.0.0.1:8080/api/messages/<job-id>
curl http://127.0.0.1:8080/api/status
curl http://127.0.0.1:8080/api/health
```

| Job status | Meaning |
| --- | --- |
| `queued` | Waiting for the background worker |
| `sending` | WhatsApp send is in progress |
| `sent` | WhatsApp acknowledged the send; includes `message_id`. This does not mean delivered or read. |
| `blocked` | Rate or warm-up limit reached; no message was sent |
| `failed` | Attachment preparation/upload failed; no message was sent |
| `unknown` | Send failed/timed out, or the process stopped during sending; verify in WhatsApp before resending |

Validation errors return 400, wrong content type 415, missing authentication 401,
disconnected WhatsApp 503, and a full queue 429. There can be up to 1000 pending jobs.
Queued jobs survive restarts and pause during disconnects. Blocked and uncertain
jobs are not automatically retried, to avoid unexpected or duplicate receipts.
Job records retain message content and metadata in SQLite; attachment bytes are
cleared after terminal results. Manage retention and back up the
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



mkdir -p dist/windows-amd64

CGO_ENABLED=1 \
GOOS=windows \
GOARCH=amd64 \
CC=x86_64-w64-mingw32-gcc \
go build -trimpath -ldflags="-s -w" \
  -o dist/windows-amd64/smartfico-whatsapp.exe .
