# App Builder

App Builder is a self-hosted, mobile-friendly Android IDE. Projects are kept in the browser's IndexedDB; the server receives a ZIP snapshot only when a build is requested, then builds a debug APK in an isolated Docker container.

The current MVP supports single-module Android projects using Kotlin or Java with XML Views, debug APK builds, ZIP import/export, a Monaco editor, persistent build history, and build log streaming. It does not currently support release signing, Compose, NDK, multi-module projects, or public multi-user access.

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
npm --prefix frontend ci
npm --prefix frontend run build
docker build -t android-web-ide-builder ./build-image
HOST_DATA_DIR="$(pwd)/data" docker compose up --build -d
```

The web app and API are served together on port `8866`. Compose stores the SQLite database, workspaces, and APK artifacts in `./data`. `HOST_DATA_DIR` must be an absolute path to the same host directory mounted at `/data`; the Docker daemon uses this path when it mounts a project into a build container.

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
PORT=8866 ./android-ide-server
```

`DATA_DIR` is where the API stores its SQLite database, temporary project workspaces, and APK artifacts. `HOST_DATA_DIR` must point to the same directory as seen by the host Docker daemon.

## Build workflow

1. Create a Kotlin or Java Empty App, or import an Android Studio ZIP.
2. Edit files in the browser; project files are saved in IndexedDB.
3. Choose **Build APK**. The browser validates the project and uploads a ZIP snapshot.
4. The API validates the archive and Android single-module structure.
5. A Docker container runs Gradle `assembleDebug` with CPU, memory, process, and timeout limits.
6. Logs and build status are stored by the API and can be restored after a page refresh or browser restart. Successful APKs are available from the build panel.

Each browser project has a stable ID. A second build for that project is rejected while its first build is queued or running; builds are also serialized globally. Once a build reaches a terminal state, that project can be built again.

## Security notes

**Do not expose this service directly to the public internet as-is.** The current MVP does not implement user authentication or authorization. Anyone who can reach the API can submit build jobs and consume server resources. Build scripts are untrusted input even though they run in a separate container. Restrict access with a trusted network, VPN, firewall, or an authenticated reverse proxy.

The API container needs access to the Docker socket to start build containers. Access to that socket is highly privileged; only run the service on a host you control, and do not give untrusted users access to the API. Build workspaces and artifacts are stored on the server and should be protected and cleaned up according to your retention needs.

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
