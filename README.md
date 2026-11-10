# db-backup

[![build](https://github.com/lucianoscastro/db-backup/actions/workflows/docker.yml/badge.svg)](https://github.com/lucianoscastro/db-backup/actions/workflows/docker.yml)
[![CVE scan](https://github.com/lucianoscastro/db-backup/actions/workflows/cve.yml/badge.svg)](https://github.com/lucianoscastro/db-backup/actions/workflows/cve.yml)
[![Docker pulls](https://img.shields.io/docker/pulls/lucianoscastro/db-backup)](https://hub.docker.com/r/lucianoscastro/db-backup)
[![Image size](https://img.shields.io/docker/image-size/lucianoscastro/db-backup/latest)](https://hub.docker.com/r/lucianoscastro/db-backup)

Docker image for scheduled backups of **MongoDB** and **SQL Server** (with a driver architecture ready for other databases), plus a **login-protected web UI** to list servers, browse backups, trigger a backup and restore. Every backup can optionally be shipped to **S3**.

**Image:** [lucianoscastro/db-backup](https://hub.docker.com/r/lucianoscastro/db-backup) on Docker Hub · **Source:** [github.com/lucianoscastro/db-backup](https://github.com/lucianoscastro/db-backup)

- **One driver per engine**: `scripts/drivers/<engine>.sh` — a new script = a new database, nothing else changes
- **One directory per database**: `/backups/<engine>/<db>/<db>_<timestamp>.<ext>`
- **Built-in scheduler**: runs every `INTERVAL_HOURS`, no host cron needed
- **Optional web UI**: rate-limited login, signed sessions, one job at a time
- **Optional S3**: `S3_ENABLED=false` keeps backups local only
- **Restore** from the UI, from a local file or straight from S3

## Usage

```bash
cp .env.example .env    # configure the engines, S3 and ADMIN_PASSWORD

docker run -d --name db-backup \
  --restart unless-stopped \
  --env-file .env \
  -p 8080:8080 \
  -v ./backups:/backups \
  lucianoscastro/db-backup:latest
```

The UI is served at `http://localhost:8080` (user `admin` by default). To run **without the UI** (no exposed port), use the `daemon` command:

```bash
docker run -d ... lucianoscastro/db-backup:latest daemon
```

With `docker-compose.yml` (example with SQL Server on the same host):

```yaml
services:
  db-backup:
    image: lucianoscastro/db-backup:latest
    container_name: db-backup
    restart: unless-stopped
    env_file: .env
    ports:
      - "8080:8080"
    volumes:
      - ./backups:/backups
      - mssql-backup:/mssql-backup   # same volume as SQL Server (see below)
    group_add:
      - "10001"                      # mssql gid: allows reading the .bak files (mode 640)
    # Hardening (optional, but recommended): immutable rootfs, no privilege escalation.
    # mongodump stages in /tmp — with read_only it becomes a tmpfs (RAM) and
    # counts against the memory limit. For large databases, swap the tmpfs for
    # a disk directory (e.g. ./tmp:/tmp) and adjust the limits.
    read_only: true
    tmpfs:
      - /tmp:size=128m
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    deploy:
      resources:
        limits:
          memory: 256m
          cpus: "0.5"

  mssql:
    image: mcr.microsoft.com/mssql/server:2022-latest
    environment:
      ACCEPT_EULA: "Y"
      MSSQL_SA_PASSWORD: "${MSSQL_PASSWORD}"
    volumes:
      - mssql-data:/var/opt/mssql
      - mssql-backup:/var/opt/mssql/backup

volumes:
  mssql-data:
  mssql-backup:
```

## Engines

An engine is enabled when its required variables are set — configure one, the other, or both.

### MongoDB

| Variable | Default | Description |
|---|---|---|
| `MONGO_URI` | — | Enables the engine. Every database in the cluster is included |
| `EXCLUDE_DBS` | `admin config local` | Databases to skip |
| `DUMP_TIMEOUT_MINUTES` | `60` | Aborts a stuck dump. **Increase it for large databases** |

### SQL Server

SQL Server's `BACKUP DATABASE` is **always server-side**: the `.bak` is written to the server's own disk. That's why SQL Server's backup directory must be a volume mounted **in this container too** (as in the compose file above). The driver tells the server to write the `.bak` there, moves the file to `/backups` and continues with the normal flow (S3 + retention).

| Variable | Default | Description |
|---|---|---|
| `MSSQL_HOST` / `MSSQL_PASSWORD` | — | Enable the engine |
| `MSSQL_PORT` | `1433` | |
| `MSSQL_USER` | `sa` | Needs BACKUP/RESTORE permission |
| `MSSQL_DATABASES` | *(empty)* | Space-separated list; empty = all user databases |
| `MSSQL_EXCLUDE_DBS` | *(empty)* | Databases to skip |
| `MSSQL_DIR_SERVER` | `/var/opt/mssql/backup` | Backup directory **as SQL Server sees it** |
| `MSSQL_DIR_LOCAL` | `/mssql-backup` | The **same volume**, mounted in this container |
| `MSSQL_COMPRESS` | `false` | `WITH COMPRESSION` — requires an edition that supports it (not Express) |
| `MSSQL_TIMEOUT_MINUTES` | `60` | Timeout for each BACKUP/RESTORE |
| `MSSQL_CA_FILE` | — | CA (PEM) that signs the server certificate. When set, the connection is encrypted and the certificate verified; when empty, the certificate is accepted without verification (exposed to network MITM) |

**Shared volume permissions** (once, after creating the containers): SQL Server runs as uid 10001 and creates `.bak` files with mode 640; this container runs as uid 1000.

```bash
# the directory must be writable from both sides
docker exec -u root <mssql-container> chmod 0777 /var/opt/mssql/backup
# and this container needs the mssql group to read the .bak files (group_add above,
# or --group-add 10001 with docker run)
```

> Restore assumes the same server (or the same disk layout): it uses `WITH REPLACE`, without `MOVE`.

### Adding databases

A new driver is a script at `scripts/drivers/<engine>.sh` implementing 4 subcommands: `enabled`, `info`, `backup`, `restore <db> <file>`. The scheduler, UI, S3 upload and retention work unchanged — the engine shows up in the UI automatically.

## Web UI

- **Login required**: `ADMIN_PASSWORD` (min. 8 characters) must be set in `serve` mode; `ADMIN_USER` defaults to `admin`
- **Protections**: constant-time comparison, 5 failed attempts = 15-minute lockout per IP, signed (HMAC) `HttpOnly`+`SameSite=Strict` cookie, `Origin` check on POSTs, strict CSP
- **Exposure**: the container serves plain HTTP — on the internet, put it behind a reverse proxy with HTTPS (the `Secure` cookie turns on automatically via `X-Forwarded-Proto`)
- **Sessions**: set `SESSION_SECRET` so logins survive container restarts. Logging out or changing `ADMIN_PASSWORD` invalidates every open session
- **Jobs**: one at a time (scheduled backup, manual backup and restore never run concurrently); each job's log is visible in the UI

| Variable | Default | Description |
|---|---|---|
| `ADMIN_PASSWORD` | — | **Required in serve mode** |
| `ADMIN_USER` | `admin` | |
| `WEB_PORT` | `8080` | |
| `SESSION_TTL_HOURS` | `12` | Login lifetime |
| `SESSION_SECRET` | *(random)* | Set it to keep sessions across restarts (min. 32 characters: `openssl rand -hex 32`) |
| `COOKIE_SECURE` | `auto` | `auto` enables the Secure cookie behind HTTPS |
| `PUBLIC_URL` | — | Public domain, when the reverse proxy doesn't forward the original Host (see below) |

### "origem inválida" (invalid origin) behind a reverse proxy

As CSRF protection, the UI compares the `Origin` header sent by the browser with the `Host` the container receives. If your reverse proxy (Cloudflare, nginx, Traefik...) doesn't forward the original `Host` untouched — e.g. nginx without `proxy_set_header Host $host;` — `Origin` and `Host` won't match and login fails with **"origem inválida"**.

The right fix is to configure the proxy to forward the original `Host`. If that's not possible, set `PUBLIC_URL` to the public domain (e.g. `PUBLIC_URL=https://backup.example.com`): it's a container environment variable set by the admin, not a request header — so it doesn't reopen any CSRF hole (unlike trusting `X-Forwarded-Host`, which a cross-origin `fetch()` could forge).

## Scheduling, retention and S3

| Variable | Default | Description |
|---|---|---|
| `INTERVAL_HOURS` | `6` | Interval between backups. Accepts fractions (`0.5` = 30 min) |
| `MAX_BACKUPS` | `3` | Files kept **per database** on local disk |
| `RETRY_MINUTES` | `15` | Wait before retrying when a cycle fails |
| `RUN_ON_START` | `true` | Runs a backup when the container starts |
| `BACKUP_DIR` | `/backups` | Where files are written (mount a volume here) |
| `S3_ENABLED` | `false` | Enables offsite upload |
| `S3_BUCKET` | — | Target bucket |
| `S3_PREFIX` | *(empty)* | Keys: `<prefix>/<engine>/<db>/<file>` |
| `S3_REGION` | `us-east-1` | Region |
| `S3_ENDPOINT` | — | Only for S3-compatible services (MinIO, R2). Empty = AWS |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | — | Upload credentials |

## Commands (CLI, no UI)

Prefix with `docker run --rm --env-file .env -v ./backups:/backups lucianoscastro/db-backup:latest`:

```bash
backup                     # back up every enabled engine
backup mongo               # a single engine
list                       # list local backups
restore mongo shop         # restore from the most recent local file
restore mongo shop --from-s3
restore mongo shop --drop  # (mongo) drop collections before restoring
restore mssql sales        # SQL Server: always WITH REPLACE
```

## Security: the backup credential shouldn't be able to restore

Give the container an IAM credential with **only `s3:PutObject`** on the prefix:

```json
{
  "Effect": "Allow",
  "Action": "s3:PutObject",
  "Resource": "arn:aws:s3:::MY-BUCKET/*"
}
```

Whoever compromises the server can't **read or delete** the backups — only write new keys. The trade-off is that `restore --from-s3` **doesn't work with this credential**: restoring needs a separate one with `GetObject`/`ListBucket`, used only at restore time.

With `PutObject` only, remote retention is handled by the **bucket lifecycle policy** — the container can't delete objects.

## How it works

- **mongodump** runs once per cycle without `--db`, which discovers and dumps every database; each database becomes its own `.tar`. The image needs neither `mongosh` (Node.js, ~200 MB) nor `aws-cli` — uploads use `curl --aws-sigv4`.
- `mongodump`/`mongorestore` are **built from upstream source** ([mongo-tools](https://github.com/mongodb/mongo-tools) 100.17.0) during the image build, with a current Go toolchain and `x/crypto`/`x/net` — the distro's prebuilt package carried dozens of CVEs from outdated dependencies.
- SQL Server is accessed through **sqlrun**, the project's own minimal T-SQL runner (`web/sqlrun`, ~150 lines on top of the official [go-mssqldb](https://github.com/microsoft/go-mssqldb) driver). It replaces Microsoft's go-sqlcmd, which bundled a Docker client, SSH and the Azure SDK — dozens of CVEs in code that would never run here. `BACKUP DATABASE ... WITH INIT, FORMAT, CHECKSUM` writes the `.bak` to the shared volume and the driver moves it to `/backups`.
- The **web UI** is a Go binary (stdlib, no dependencies) that only runs the same scripts as the CLI — backup logic always lives in the scripts.

Known limits:

- Uploads use a single PUT: **5 GB cap per file**. Above that the local backup still succeeds, but the upload fails with an explicit error (multipart would be required).
- The Mongo dump goes to a temporary directory before becoming a `.tar`, so peak disk usage is ~2× the compressed dump size.
- Local retention only deletes the old file **after** the new one has been written and validated.
- Backups are written with mode `0600` (readable only by uid 1000) and **unencrypted**: on S3, enable the bucket's default encryption (SSE).

### Migrating from the mongo-only image

The local layout changed from `/backups/<db>/` to `/backups/<engine>/<db>/`:

```bash
mkdir -p backups/mongo && mv backups/<db> backups/mongo/
```

Nothing changes on S3 if you used the old default `S3_PREFIX=mongo` — the prefix is now empty and the engine (`mongo/`) takes its place. The restore CLI gained the engine as its first argument (`restore mongo shop` instead of `restore shop`).
