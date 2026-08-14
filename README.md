# smtp2ntfy

`smtp2ntfy` accepts verification emails over SMTP on a private container network, extracts the first independent six-digit code, and publishes it to an ntfy topic over HTTPS.

For `ALLOWED_DOMAIN=ipao.vip` and `TOPIC_PREFIX=multica-`:

```text
rogee@ipao.vip -> https://ntfy.example.com/multica-rogee
```

The local part is lowercased and must match `[a-z0-9_-]{1,48}`. Messages for another domain, invalid local parts, multiple recipients, or messages over 64 KiB are rejected by SMTP.

## Configure

Copy the example and replace every placeholder:

```sh
cp .env.example .env
```

| Variable | Default | Purpose |
| --- | --- | --- |
| `LISTEN_ADDR` | `:2525` | Private SMTP listener |
| `ALLOWED_DOMAIN` | required | The only accepted recipient domain |
| `TOPIC_PREFIX` | `multica-` | Prefix before the lowercased local part |
| `NTFY_BASE_URL` | required | HTTPS ntfy base URL; credentials in URLs are rejected |
| `NTFY_USERNAME` | required | ntfy HTTP Basic Auth username |
| `NTFY_PASSWORD` | required | ntfy HTTP Basic Auth password |
| `HTTP_TIMEOUT` | `3s` | Positive timeout for one publish attempt |

Start it with:

```sh
docker compose up --build -d
```

The Compose file uses `expose`, not `ports`; attach the Multica service to the same Compose network. Configure Multica with:

```env
SMTP_HOST=smtp2ntfy
SMTP_PORT=2525
SMTP_USERNAME=
SMTP_PASSWORD=
SMTP_FROM_EMAIL=noreply@ipao.vip
```

The login address must use `ALLOWED_DOMAIN`. For example, mail to `rogee@ipao.vip` publishes the extracted code to topic `multica-rogee`.

## Security and delivery boundary

- SMTP has no AUTH or TLS and must remain on a trusted container-private network. Never publish port 2525 on the host.
- ntfy must use a valid HTTPS certificate and a dedicated account limited to the generated topics. Credentials and verification codes are never logged.
- Every ntfy request uses HTTP Basic Auth and `Cache: no`, so offline subscribers can miss a notification.
- Publishing is best effort: an ntfy error or timeout does not fail SMTP acceptance. There is no retry, queue, storage, attachment processing, or delivery guarantee.
- A phone notification preview can expose the code. Configure device previews according to your threat model.

## Test

```sh
go test ./...
go test -race ./...
go vet ./...
```

## Images and tags

Pulling a public image does not require a GitHub login:

```sh
docker pull ghcr.io/rogeecn/smtp2ntfy:edge
```

Pull requests build both target architectures without publishing. Merges to `main` publish `edge` and `sha-<commit>` for `linux/amd64` and `linux/arm64`. A `vX.Y.Z` tag publishes `X.Y.Z`, `X.Y`, `X`, and `latest` through the metadata action's semver behavior.

GHCR creates a new package as private. After the first default-branch publish, a repository owner must link the package to this repository if needed and change its package visibility to **Public** once; anonymous pulls then work as shown above.
