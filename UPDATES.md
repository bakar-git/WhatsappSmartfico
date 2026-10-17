# Login startup and GitHub updates

Build this version, put it in its permanent installation folder, and run it once.
The default `START_ON_LOGIN=true` registers the installed executable for the
current OS user. Subsequent logins start the API quietly in the background,
regardless of `BACKGROUND_ON_CLOSE`. Manual launches still display pairing QR
codes normally. Keep `.env` and the existing `data` directory beside the app.

This is **startup at user sign-in**, not a machine-wide service before anyone
logs in. It requires no administrator access. Windows uses HKCU's Run key, Linux
uses XDG desktop autostart, and macOS uses a user LaunchAgent with RunAtLoad.
Linux requires a desktop session; WSL without a desktop does not have that login
trigger. Run the Windows executable on the Windows host for Windows sign-in.

Set `START_ON_LOGIN=false` and launch once to remove this app's registration.
Moving the executable requires launching it again to register the new location.
Temporary `go run` executables skip registration and updates.

## Runtime settings

```dotenv
START_ON_LOGIN=true
UPDATE_ENABLED=true
UPDATE_MANIFEST_URL=https://github.com/bakar-git/WhatsappSmartfico/releases/latest/download/.release
UPDATE_CHECK_INTERVAL=6h
```

The service checks once when it starts and then every six hours (configurable,
minimum one minute). Network/404 errors log a warning and leave the API running.
Until the first GitHub release contains a `.release` asset, update checks will
report 404. No GitHub credentials or access token are needed for this public URL.

Disable automatic downloads/restarts with `UPDATE_ENABLED=false`. Keep the
installation directory writable by the current user. GitHub/latest asset URLs
follow redirects, but redirects and downloads must remain HTTPS.

`--version` prints the installed version without starting or registering the
service. The version is also returned by `/api/status` and `/api/health`.

## The .release manifest

Attach a JSON file named **`.release`** to each published GitHub release. The
default URL uses GitHub's latest release asset download, so the app need not
query GitHub's authenticated REST API. A newer semantic version is required;
equal/older versions are ignored. Prerelease versions compare using Go's semver
package, but GitHub's latest-release endpoint normally targets stable releases.

For one platform, a flat manifest is supported:

```json
{
  "version": "0.3.0",
  "link": "https://github.com/bakar-git/WhatsappSmartfico/releases/download/v0.3.0/win-amd64.zip",
  "sha256": "THE_64_CHARACTER_SHA256_OF_THE_ZIP"
}
```

For multiple platforms use `assets`, keyed by Go's `GOOS-GOARCH`:

```json
{
  "version": "0.3.0",
  "assets": {
    "windows-amd64": {
      "link": "https://github.com/bakar-git/WhatsappSmartfico/releases/download/v0.3.0/win-amd64.zip",
      "sha256": "THE_64_CHARACTER_SHA256_OF_THE_ZIP"
    },
    "linux-amd64": {
      "link": "https://github.com/bakar-git/WhatsappSmartfico/releases/download/v0.3.0/linux-amd64.zip",
      "sha256": "THE_64_CHARACTER_SHA256_OF_THE_ZIP"
    }
  }
}
```

Missing platform entries are skipped. Links can target a ZIP or a raw native
executable. ZIPs must contain exactly one `smartfico-whatsapp.exe` on Windows or
`smartfico-whatsapp` on Linux/macOS. Other ZIP entries are ignored. The checksum
is required and covers the entire downloaded ZIP/raw binary, not just the ZIP's
executable. The example placeholders are not valid checksums; use the packaging
script to generate the actual manifest.

The manifest and downloads are trusted publisher input: publish them only from
your controlled repository. Checksums detect corrupt or mismatched assets; they
are not a separate publisher signature if both manifest and asset are replaced.

## Build and publish a release

Use a new version and set it in the binary at build time. Example Windows x64
build from WSL, with MinGW installed:

```sh
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -trimpath -ldflags='-s -w -X main.version=0.3.0' \
  -o dist/windows-amd64/smartfico-whatsapp.exe .

python3 scripts/package_release.py --version 0.3.0 \
  --asset windows-amd64=dist/windows-amd64/smartfico-whatsapp.exe
```

For Linux/macOS, build natively with CGO enabled and package the binary under
its matching platform key. Repeat `--asset` to include more platforms in the
same manifest, for example `--asset linux-amd64=dist/linux-amd64/smartfico-whatsapp`.

The script outputs `dist/release/win-amd64.zip` and `dist/release/.release`
(plus any other platforms). Each ZIP contains the executable and `.env` from
`.env.example`, with no private installation token or session database.

Upload the generated files as assets to a GitHub release tagged `v0.3.0`, using
the GitHub UI or, if you use GitHub CLI:

```sh
gh release create v0.3.0 --repo bakar-git/WhatsappSmartfico \
  --title 'v0.3.0' --generate-notes \
  dist/release/.release dist/release/win-amd64.zip
```

Include every ZIP referenced in the manifest. Do not reuse an old version for a
changed executable. The packaging script does not build binaries or publish to
GitHub; build with the same version before packaging, then publish explicitly.

## Applying an update

The app downloads into its installation folder, validates SHA-256, stages only
the executable, and runs the candidate's `--version` to check OS compatibility
and the advertised version. Downloads and uncompressed binaries are limited to
128 MiB; the manifest is limited to 1 MiB. Failed validation leaves the current
binary in place.

After staging, the API enters maintenance: new send requests receive 503 and
health returns `updating: true`. The current send finishes before shutdown;
queued jobs remain in SQLite. A detached helper waits for the service to exit,
holds the instance lock during replacement, saves the old binary as
`smartfico-whatsapp.exe.previous` (or `smartfico-whatsapp.previous`), then starts
the new binary. This also handles Windows executable file locks.

The helper checks version, database, and worker startup over the local health
endpoint for up to 30 seconds; WhatsApp pairing/connectivity is not required.
If startup fails it restores the previous executable and restarts it. That
failed version is skipped until a different version is published, or until you
remove `failed-update-version` from the app's cache directory. `.env`, customer
messages, queued attachments, and the WhatsApp session are never replaced by
downloaded archive contents. Updates create no extra WhatsApp session.

Helper/startup logs are in the existing cache `smartfico-whatsapp/service.log`.
An updater helper executable is retained there for reuse. Keep the app installed
in its permanent folder and do not manually launch `.previous` or staged files.

## Official references used

- [Go os.Rename and executable/file APIs](https://pkg.go.dev/os#Rename)
- [Go os/exec subprocess API](https://pkg.go.dev/os/exec)
- [Go HTTP timeouts and redirects](https://pkg.go.dev/net/http#Client)
- [Go SHA-256](https://pkg.go.dev/crypto/sha256)
- [Go semantic version comparisons](https://pkg.go.dev/golang.org/x/mod/semver)
- [Windows current-user Run registration](https://learn.microsoft.com/en-us/windows/win32/setupapi/run-and-runonce-registry-keys)
- [XDG desktop autostart](https://specifications.freedesktop.org/autostart/latest/)
- [macOS LaunchAgent startup](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)
