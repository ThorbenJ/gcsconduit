# gcsconduit

A lightweight HTTP proxy for Google Cloud Storage. It forwards requests directly to the GCS JSON API, adding OAuth2 authentication where needed, without interpreting the payload or abstracting the protocol.

## How it works

Incoming requests are forwarded verbatim to `https://storage.googleapis.com<path>`. Response headers and body are streamed back as-is. Because gcsconduit operates at the HTTP level rather than through a storage SDK, GCS features that rely on HTTP semantics work transparently — including `Range` requests (enabling video seek and partial downloads), conditional requests (`If-None-Match`, `If-Modified-Since`), and any other standard HTTP mechanism GCS supports.

The same design keeps the maintenance surface small: there is no GCS client library to update, no request/response marshalling, and no SDK version drift.

## Configuration

All flags have an environment variable equivalent prefixed with `GCSC_`.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `-port` | `GCSC_PORT` | `8080` | Listen port |
| `-credential` | `GCSC_CREDENTIAL` | _(none)_ | Path to a GCP service account JSON key file. If unset, requests are forwarded unauthenticated (suitable for public buckets). |
| `-reqmatch` | `GCSC_REQMATCH` | _(none)_ | Repeatable header guard: only forward requests where the named header matches a regex. Format: `Header=regex`. Pipe-separate multiple rules in the env var. |
| `-inspect` | `GCSC_INSPECT` | _(none)_ | Path to a file where request and response headers are appended in YAML for troubleshooting. |

## Running

```sh
# Public bucket, no authentication
docker run -p 8080:8080 <region>-docker.pkg.dev/<project>/<repo>/gcsconduit:latest

# Private bucket with a service account key
docker run -p 8080:8080 \
  -v /path/to/key.json:/creds/key.json:ro \
  -e GCSC_CREDENTIAL=/creds/key.json \
  <region>-docker.pkg.dev/<project>/<repo>/gcsconduit:latest
```

Once running, objects are accessible at `http://localhost:8080/<bucket>/<object>`:

```sh
curl http://localhost:8080/my-bucket/path/to/file.mp4
```

`Range` requests work as expected:

```sh
curl -H "Range: bytes=0-1023" http://localhost:8080/my-bucket/video.mp4
```

## Request guards (`-reqmatch`)

The `-reqmatch` flag (repeatable) rejects any request where a named header is absent or does not match a regular expression. This is useful for restricting access without a full auth layer.

```sh
# Only forward requests that carry a specific token
gcsconduit -reqmatch "X-Access-Token=^mysecret$"

# Multiple guards via env var (pipe-separated)
GCSC_REQMATCH="X-Access-Token=^mysecret$|X-Tenant=^acme$" gcsconduit
```

Requests that fail a guard receive `403 Forbidden`.

## Inspect mode

When `-inspect` points to a writable file, gcsconduit appends a YAML block for every proxied request:

```yaml
---
timestamp: 2026-04-30T12:00:00Z
method: GET
path: /my-bucket/video.mp4
query: ?alt=media
request:
  Authorization: Bearer eyJ...
  Range: bytes=0-1023
response:
  status: 206
  Content-Range: bytes 0-1023/104857600
  Content-Type: video/mp4
```

## Credential setup in sandbox environments

In ephemeral environments where a secret must be injected at runtime, configure `GCSC_CREDENTIAL` to point to a file path and write the key there during environment setup. gcsconduit will wait for the file to appear before accepting proxy requests (the `/health` endpoint responds immediately).

Example setup script:

```sh
#!/bin/sh
echo "$GCS_SERVICE_ACCOUNT_KEY" > /tmp/gcs.creds
```

Set `GCSC_CREDENTIAL=/tmp/gcs.creds` as an environment variable on the container.

## Building

```sh
docker build -t gcsconduit .

# Or publish via Cloud Build
gcloud builds submit --tag <region>-docker.pkg.dev/<project>/<repo>/gcsconduit:$(date -u +%Y-%m-%d)
```

## Extending to other storage backends

gcsconduit proxies to a configurable base URL (`gcsBase` in `main.go`). Since authentication is the only GCS-specific concern — and that is isolated to `buildClient` — adapting the proxy to other HTTP-based object storage APIs (S3-compatible, Azure Blob, etc.) is straightforward: swap the base URL and the token source.
