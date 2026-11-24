# App Builder

App Builder is a self-hosted, mobile-friendly Android IDE. Projects are kept in the browser's IndexedDB; a build sends text files in its manifest and only uploads missing binary assets, then builds a debug APK in an isolated Docker container.

The current MVP supports single-module Android projects using Kotlin or Java with XML Views, debug APK builds, ZIP import/export, a Monaco editor, persistent build history, build log streaming, and configurable public/private access. It does not currently support release signing, Compose, NDK, or multi-module projects.

## Requirements

### Hardware

The practical baseline used for a small, single-user installation is:

| Resource | Minimum baseline |
| --- | ---: |
| CPU | 4 cores |
| RAM | 7 GB |
| Free storage | 15 GB |

This baseline is intended for one Android build at a time. Builds are serialized to avoid competing for memory and CPU. Android SDK layers, Gradle caches, imported projects, and APK artifacts use disk space over time, so leave additional headroom and periodically remove old data when needed.

### Software

- Linux x86-64 host with Docker Engine and Docker Compose.
- Go 1.24+ and a C compiler for building the API locally (SQLite uses CGO).
- Node.js 20.19+ or 22.12+ with npm to build the frontend.
- Git.
- The `android-web-ide-builder` Docker image, which contains JDK 17, Gradle 8.9, Android SDK Platform 34, and Build Tools 34.0.0.

## Quick start with Docker Compose

From the repository root:

```bash
bash start.sh
```

`start.sh` creates `data/IDE.config` from the tracked `IDE.config` template if needed, builds the frontend and Android builder image, starts/restarts the web app and API, then waits for its health endpoint. It also supports `bash start.sh stop`, `status`, `logs`, and `restart`.

The web app and API are served together on the port in `data/IDE.config` (default `8866`). Compose stores the SQLite database, workspaces, APK artifacts, and config in `./data`. `HOST_DATA_DIR` is set by the startup script to the absolute host path needed when Docker mounts a project into an isolated build container.

## Install and offline editing

The web UI is a PWA and requires HTTPS (or localhost). Open it online once and let the initial asset download finish before going offline; this caches the app shell and Monaco editor locally in the browser (about 18 MB of static Monaco assets). Use **Install** in the top bar when the browser supports installation, or its menu's **Install app / Add to Home Screen** option.

Projects and editor state live in that browser's IndexedDB, so they can be created, opened, and edited offline. In private mode, a browser that has logged in successfully can still open its local workspace offline; no password is stored, only a local unlock marker, which is removed on logout. Clearing browser site data also clears projects and the offline unlock.

APK builds, build history/logs, server storage management, and other API-backed operations still require the server connection. They are not queued while offline. The service worker caches static app/Monaco assets only and always sends `/api/*` requests to the network.

## Access mode

Edit `data/IDE.config`; access mode is not controlled from the web UI. The service reads config at startup, so apply changes with `bash start.sh restart`.

- `"mode": "public"` leaves the IDE and its APIs open to anyone who can reach the URL.
- `"mode": "private"` shows a password login and requires a valid session for project/build APIs, build logs, APK downloads, and storage cleanup.
- Set `"password"` to a password of at least 8 characters before switching to private mode. The local config file is created with permission `0600` and is ignored by Git.
- `"secureCookie": true` is recommended when serving the IDE through HTTPS. For local plain-HTTP development only, set it to `false`.

Example:

```json
{
  "mode": "private",
  "password": "replace-this-with-a-long-password",
  "port": 8866,
  "sessionTTLHours": 168,
  "secureCookie": true,
  "build": {
    "maxUploadMB": 50,
    "timeoutMinutes": 30,
    "memoryMB": 4096,
    "cpus": 2,
    "parallel": 1
  }
}
```

In public mode, all app APIs—including cleanup—are public by design. Only use it when the host and its resources are intentionally open to visitors.

Check that the API is ready:

```bash
curl http://localhost:8866/api/health
```

Open `http://localhost:8866` in a browser. For a remote VPS, place the service behind HTTPS and publish/forward port `8866` through your reverse proxy.

## Run the API directly on the host

```bash
npm --prefix frontend ci
npm --prefix frontend run build
docker build -t android-web-ide-builder ./build-image
cd backend
go build -o ../android-ide-server .
cd ..
DATA_DIR="$(pwd)/data" \
HOST_DATA_DIR="$(pwd)/data" \
WEB_DIR="$(pwd)/frontend/dist" \
IDE_CONFIG="$(pwd)/data/IDE.config" ./android-ide-server
```

`DATA_DIR` is where the API stores its SQLite database, temporary project workspaces, and APK artifacts. `HOST_DATA_DIR` must point to the same directory as seen by the host Docker daemon.

## Build workflow

1. Create a Kotlin or Java Empty App, or import an Android Studio ZIP.
2. Edit files in the browser; project files are saved in IndexedDB.
3. Choose **Build APK**. The browser hashes project files (caching hashes in IndexedDB until file contents change), sends a manifest, and uploads only binary assets missing from this browser's cache.
4. The API verifies binary SHA-256 and size, assembles the Android project from text and owner-scoped immutable blobs, then validates the single-module structure.
5. A Docker container runs Gradle `assembleDebug` with CPU, memory, process, and timeout limits.
6. Logs and build status are stored by the API and can be restored after a page refresh or browser restart. Successful APKs are available from the build panel.

Each browser project has a stable ID. A second build for that project is rejected while its first build is queued or running; builds are also serialized globally. Once a build reaches a terminal state, that project can be built again.

## Storage and cleanup

- `DATA_DIR/workspaces/<build-id>` contains temporary upload data and the assembled Gradle workspace while a build is running. New builds remove their workspace when they finish; the cleanup UI can remove leftovers from earlier runs.
- `DATA_DIR/artifacts/<build-id>.apk` contains successful APKs until they are explicitly deleted.
- `DATA_DIR/blobs/sha256/<owner>/<prefix>/<sha256>` stores immutable binary assets scoped to an opaque per-browser cookie owner. Builds retain blob references while their history exists; deleting history releases refs and garbage-collects unreferenced blobs. Incomplete upload reservations expire after one hour.
- `DATA_DIR/ide.db` stores build history and logs.
- The Docker volume `androidide_gradle` stores downloaded Gradle and Maven dependencies. Deleting it saves disk space but the next build must download dependencies again.

Open the drive icon in the web UI to inspect storage and choose what to remove. Active builds block cleanup. The history option also removes associated APKs, workspaces, and unreferenced binary blobs; browser projects in IndexedDB are not affected.

## Security notes

In public mode, anyone who can reach the service can submit build jobs, download artifacts, and use cleanup; this is intentional for a public instance. Use private mode when the service is for personal use. Build scripts are untrusted input even though they run in a separate container.

The API container needs access to the Docker socket to start build containers. Access to that socket is highly privileged; only run the service on a host you control. Keep `data/IDE.config` private, use HTTPS when password login is enabled, and protect server-side workspaces and APK artifacts.

## Development

```bash
npm --prefix frontend ci
npm --prefix frontend run build
cd backend
go test ./...
go vet ./...
```

## License

This project is distributed under the MIT License. See [LICENSE](LICENSE).
