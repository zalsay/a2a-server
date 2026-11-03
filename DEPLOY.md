# Deployment

Two ways to run the Go server. The container route is the recommended one
for a server that outlives this VM; the direct route matches the systemd
unit in README.md.

## Option A: container, mount the prebuilt binary (recommended)

The container does not build anything. It runs a binary you built (or
downloaded from a release) via a read-only mount.

### 1. Prepare a deploy directory

```bash
mkdir -p a2a-deploy/bin a2a-deploy/a2a-data
cd a2a-deploy
cp /path/to/repo/docker-compose.yml .
```

### 2. Get the binary

Either build it (needs Go + gcc):

```bash
cd /path/to/repo && go build -o /path/to/a2a-deploy/bin/a2a .
```

or download it from a GitHub release asset (see "Releases" below):

```bash
# replace <tag> with a real release tag, e.g. v1.0.0
curl -sL -o bin/a2a \
  https://github.com/zalsay/a2a-server/releases/download/<tag>/a2a
chmod +x bin/a2a
sha256sum bin/a2a   # compare with the checksum printed by the release job
```

### 3. Configure

```bash
cp /path/to/repo/config.example.json a2a-data/config.json
# edit a2a-data/config.json: set "token" (keep the file mode 600)
chmod 600 a2a-data/config.json
```

`config.json` holds the Bearer token: never commit it, never bake it into
an image. It lives only in `a2a-data/`, which is git-ignored.

### 4. Start and verify

```bash
docker compose up -d
curl -s http://127.0.0.1:9877/health
curl -s http://127.0.0.1:9877/.well-known/agent-card.json | head -c 300
docker compose logs -f a2a
```

### 5. Update the binary later

```bash
# replace bin/a2a with the new build, then:
docker compose restart a2a
```

State (`tasks.db`) lives in `a2a-data/` on the host, so restarts and binary
swaps never lose queued tasks.

### Notes that will save you a debugging round

- **glibc base is mandatory.** The binary is dynamically linked against
  glibc (CGO + SQLite driver, check with `ldd bin/a2a`). The compose file
  uses `golang:1.27-bookworm` (Debian glibc) for that reason. Do not switch
  the image to alpine or scratch.
- **`--host 0.0.0.0` is mandatory in the container.** The default bind is
  127.0.0.1, which rejects Docker-forwarded traffic (it arrives at the
  container IP, not loopback). The compose command already sets it.
- **Port exposure.** The compose file maps `127.0.0.1:9877:9877` (host
  loopback only). Change it to `9877:9877` only if the service should be
  reachable from other machines — and then put it behind TLS/reverse proxy
  and firewall rules, since the API is token-authenticated but plain HTTP.
- **Token rotation.** Change `a2a-data/config.json` and
  `docker compose restart a2a`. Clients must use the new token.

## Option B: run the binary directly

```bash
A2A_DIR=/path/to/data ./a2a serve --host 127.0.0.1 --port 9877
```

See the systemd unit in README.md for a persistent setup.

## Releases (how the binary gets to GitHub)

> Note: the workflow file itself cannot be pushed through the GitHub App
> API (the integration lacks the Workflows permission, so the API rejects
> any tree containing `.github/workflows/` files). If
> `.github/workflows/release.yml` is missing from the repo, add it via the
> GitHub web UI (Add file → Upload files) using the file from the source
> tree, then the flow below works as described.

The connector used to publish this repo cannot upload binary blobs
(it rejects non-UTF-8 content), so releases are built by automation:
`.github/workflows/release.yml` builds the binary on every `v*` tag push
and attaches it to the release.

To cut a release:

1. On GitHub, open the repo → Releases → "Draft a new release".
2. Choose a tag (e.g. `v1.0.0`), publish the release.
3. The workflow builds `a2a` on `ubuntu-latest` and attaches it to that
   release (it uploads to the existing release if one exists, otherwise it
   creates it). The job log prints the SHA-256 for verification.
4. Download URL pattern:
   `https://github.com/zalsay/a2a-server/releases/download/<tag>/a2a`
