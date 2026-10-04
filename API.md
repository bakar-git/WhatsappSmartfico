# Smartfico local WhatsApp API

This service runs on the user's computer. Smartfico browser JavaScript calls
`http://127.0.0.1:8080`; this address refers to the **computer running the browser**.
The remote Smartfico backend supplies the authorized receipt PDF/image to the
browser, which then uploads the bytes to this service. The backend does not call
the local address itself.

Start with `go run .` and scan the QR printed in the console using WhatsApp →
Linked devices → Link a device. The account session is saved across restarts.

## Authentication and browser access

Set `API_TOKEN` for the local installation to require this header on all actual
requests, including health and job polling:

```http
Authorization: Bearer <local-installation-token>
```

Settings can be supplied through the process environment or a `.env` file beside
the built executable. See [README.md](README.md) and [.env.example](.env.example).
Changes to `.env` take effect after restarting the service; no rebuild is needed.

When unset, loopback requests do not require a token. Listening outside loopback
requires a token. Configure the token per computer in your integration; do not
hard-code one shared token into the public Smartfico JS bundle.

CORS accepts HTTP and HTTPS origins whose hostname is a subdomain of:

- `*.smartfico.test`
- `*.smartfico.app`

Examples: `https://shop.smartfico.app`, `https://branch.shop.smartfico.app`, and
`http://shop.smartfico.test:5173`. Ports are allowed. The bare domains
`smartfico.app` and `smartfico.test` are not included in these wildcard patterns.
Other origins (including `null`) receive 403 before any message is queued.
Non-browser clients such as curl may omit `Origin` and use the API normally.

The response reflects the validated origin instead of sending a wildcard.
`OPTIONS` preflights run without authentication and allow `GET`, `POST`,
`Authorization`, and `Content-Type`. `Location` is exposed to browser JS. Cookie
credentials are not used. CORS is browser access control; bearer authentication
remains the control for non-browser callers.

Serve Smartfico over **HTTPS**. Supporting browsers may ask the user to allow
local-network/loopback access; the user must allow that connection. CORS does not
grant this browser permission. The examples annotate requests with
`targetAddressSpace: "loopback"` for supporting browsers. If your site's Content
Security Policy restricts `connect-src`, it must also permit
`http://127.0.0.1:8080`. Confirm this flow in your supported browsers.

References: [Chrome local network access](https://developer.chrome.com/blog/local-network-access),
[MDN local network access](https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Local_network_access),
[MDN CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS).

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/health` | Check API, database, background worker, and WhatsApp readiness |
| GET | `/api/status` | Read WhatsApp connection and pairing status |
| POST | `/api/messages` | Queue JSON text or a multipart PDF/image upload |
| POST | `/api/documents` | Queue a multipart PDF/image upload |
| GET | `/api/messages/{id}` | Read the result of any queued message |

`POST /api/message` is an alias for `POST /api/messages`, for integrations using
the singular path. Poll jobs through the plural `/api/messages/{id}` path.

## Health

```sh
curl http://127.0.0.1:8080/api/health
```

**200** means the local service is ready to attempt sending:

```json
{
  "service": "smartfico-whatsapp",
  "api_running": true,
  "ready": true,
  "database_ok": true,
  "worker_running": true,
  "connected": true,
  "logged_in": true,
  "status": "connected"
}
```

**503** means the API is reachable but not ready: pairing is required, WhatsApp
is disconnected, the worker is stopped, or the database is unavailable. The
same response fields identify the condition; `ready` is false. If the process
is stopped, fetch fails with a network error rather than returning health JSON.
This endpoint does not send a test message or guarantee recipient delivery.
Antiban limits can still block individual jobs while health reports ready.

```js
const baseUrl = "http://127.0.0.1:8080";
// Load this installation's token from your local integration settings.
const localToken = "";
const authHeaders = localToken
  ? { Authorization: `Bearer ${localToken}` }
  : {};

async function checkWhatsApp() {
  const response = await fetch(`${baseUrl}/api/health`, {
    headers: authHeaders,
    credentials: "omit",
    targetAddressSpace: "loopback",
  });
  const health = await response.json();
  if (response.status === 503) return health; // Reachable, but not ready.
  if (!response.ok) throw new Error(health.error || "Health check failed");
  return health;
}
```

## Send PDF or image

Use **multipart/form-data**, with the actual file bytes in `file`. The browser
can supply a `Blob` or `File`; do not send a blob URL, remote URL, JSON object,
or base64 string in place of the file.

| Field | Required | Value |
| --- | --- | --- |
| `number` | Yes | International WhatsApp number, e.g. `+923205401101`; no spaces or hyphens |
| `type` | Yes | Exactly `pdf` or `image` |
| `file` | Yes | One PDF, JPEG, or PNG file/blob |
| `caption` | No | Caption up to 1024 Unicode characters |

Files must be nonempty and at most **16 MiB (16,777,216 bytes)**. Images must have
valid JPEG/PNG headers, positive dimensions, and at most 40 megapixels. PDF
files must start with `%PDF-`; this is a signature check, not a full PDF parser.
The service detects the MIME type from the contents instead of trusting the
upload's extension or Content-Type. Unsupported image formats (GIF, WebP, SVG,
HEIC) receive 415; export them as JPEG/PNG before uploading.

Send each field once and exactly one `file`. Unknown fields, multiple files,
and invalid `type` values are rejected. Supply a filename when appending a blob;
the service strips path components and normalizes its extension. Without a
useful filename, it uses `receipt.pdf`, `receipt.jpg`, or `receipt.png`.

### Browser JavaScript

This example uses `baseUrl` and `authHeaders` from the health example:

```js
async function sendReceipt({ number, type, blob, filename, caption = "" }) {
  const form = new FormData();
  form.append("number", number);
  form.append("type", type);
  form.append("file", blob, filename);
  form.append("caption", caption);

  const response = await fetch(`${baseUrl}/api/documents`, {
    method: "POST",
    headers: authHeaders,
    credentials: "omit",
    targetAddressSpace: "loopback",
    body: form,
  });
  // Do not set Content-Type: the browser supplies the multipart boundary.
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "Could not queue receipt");
  return result; // 202 means queued, not sent or delivered.
}

// receiptPdfUrl is your existing authorized PDF endpoint on Smartfico.
const pdfResponse = await fetch(receiptPdfUrl);
if (!pdfResponse.ok) throw new Error("Could not download receipt PDF");
const job = await sendReceipt({
  number: "+923205401101",
  type: "pdf",
  blob: await pdfResponse.blob(),
  filename: "receipt-1001.pdf",
  caption: "Thank you for your purchase!",
});

// Image example: imageFile comes from an input[type=file] or an image Blob.
const imageJob = await sendReceipt({
  number: "+923205401101",
  type: "image",
  blob: imageFile,
  filename: "receipt-1001.png",
});
```

### curl

```sh
curl http://127.0.0.1:8080/api/documents \
  -F 'number=+923205401101' \
  -F 'type=pdf' \
  -F 'caption=Thank you for your purchase!' \
  -F 'file=@receipt.pdf;type=application/pdf'

curl http://127.0.0.1:8080/api/documents \
  -F 'number=+923205401101' \
  -F 'type=image' \
  -F 'file=@receipt.png;type=image/png'
```

Add `-H "Authorization: Bearer $API_TOKEN"` when a token is configured.

### Accepted response

**202 Accepted**, with `Location: /api/messages/<job-id>`:

```json
{
  "id": "<job-id>",
  "number": "+923205401101",
  "message": "Thank you for your purchase!",
  "type": "pdf",
  "filename": "receipt.pdf",
  "mime_type": "application/pdf",
  "size": 12345,
  "status": "queued"
}
```

For media jobs, `message` contains the caption and is omitted when empty.
PDFs are forwarded as WhatsApp document messages, and JPEG/PNG uploads as
WhatsApp image messages with their caption. The worker uses whatsmeow's
[Upload and SendMessage APIs](https://pkg.go.dev/go.mau.fi/whatsmeow#Client.Upload).

## Send text

`POST /api/messages` with `Content-Type: application/json`:

```json
{"number":"+923205401101","message":"Hello gorgeous"}
```

Text accepts 1–4096 Unicode characters and must not be blank. The JSON request
fields are only `number` and `message`; `type` is for multipart uploads.
The 202 response includes `type: "text"`, its job ID, and `status: "queued"`.

## Check a job

```sh
curl http://127.0.0.1:8080/api/messages/<job-id>
```

```js
async function getMessageJob(id) {
  const response = await fetch(`${baseUrl}/api/messages/${encodeURIComponent(id)}`, {
    headers: authHeaders,
    credentials: "omit",
    targetAddressSpace: "loopback",
  });
  const job = await response.json();
  if (!response.ok) throw new Error(job.error || "Could not check job");
  return job;
}
```

Poll about once per second until the status is terminal:

| Status | Terminal? | Meaning |
| --- | --- | --- |
| `queued` | No | Waiting for the worker/pacing/connection |
| `sending` | No | Upload/send attempt in progress |
| `sent` | Yes | WhatsApp acknowledged the message; `message_id` is supplied. Delivery/read is not confirmed. |
| `blocked` | Yes | Antiban rate or warm-up limit reached; message was not sent |
| `failed` | Yes | Preparing/uploading the attachment failed before sending; no message was sent |
| `unknown` | Yes | Send failed/timed out or the process stopped during an attempt; check WhatsApp before resending |

Queued attachment bytes and metadata persist in SQLite across restarts. The
worker pauses when disconnected. Attachment bytes are cleared when jobs become
terminal; job metadata, captions, and send history are retained. Interrupted
`sending` jobs become `unknown`, and are not automatically resent.

There are up to 1000 pending jobs. Text and media share the conservative antiban
pacing and limits. Different file contents count as distinct content even when
their captions match. Failed upload attempts conservatively count toward limits.

## Error responses

Errors use JSON: `{"error":"Human-readable explanation"}`.

| HTTP status | Meaning |
| --- | --- |
| 400 | Invalid fields, malformed JSON/multipart, invalid number/type/caption, or wrong file count |
| 401 | Missing or incorrect bearer token |
| 403 | Disallowed browser origin or CORS preflight method/header |
| 404 | Unknown endpoint or job ID |
| 413 | Empty/oversized file, or multipart request above 16 MiB plus 64 KiB overhead |
| 415 | Wrong request Content-Type or unsupported/mismatched attachment content |
| 429 | Pending queue is full |
| 500 | Local database/storage or internal error |
| 503 | WhatsApp is not connected for sending, or health reports not ready |

The upload response cannot report a later WhatsApp failure: it only queues the
job. Read the job endpoint for the final result. Browser connection failures
(service stopped, access permission denied, or CSP restrictions) surface as
fetch errors instead of these HTTP responses.
