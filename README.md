# Miabi Guestbook — example app

[![Running on Miabi](https://miabi.io/badges/running-on-miabi-dark.svg)](https://miabi.io?ref=badge)

A tiny, self-contained web app used as a **reference for deploying to
[Miabi](https://github.com/miabi-io/miabi)**. It is a classic guestbook:
visitors leave a name and a message, and the wall shows every signature.

It deliberately exercises everything a real Miabi deployment touches, in
miniature:

- **[Okapi](https://github.com/jkaninda/okapi) (Go)** REST API, driven by
  **`okapicli`** — a `server` subcommand with graceful-shutdown lifecycle hooks
  (the same CLI layout Miabi uses)
- **SPA served by `app.WebFS`** — the embedded UI is mounted straight from an
  `embed.FS`; Okapi serves real assets and falls back to `index.html` for
  client-side routes (home + an **All signatures** page), while registered API
  routes keep precedence
- **Live updates over SSE** — new/removed signatures stream to every open tab,
  with a real-time **connected-clients** ("N online") indicator
- **Version badge + live server-time card** — the serving build's version is
  shown in the UI and `/api/info`, and a **live clock** (streamed over SSE with
  the server's `version` + `host`) makes a **canary rollout** obvious: the card
  is new in v2, and its time/host reveal exactly which build/replica served you
- **[GORM](https://gorm.io) with PostgreSQL _or_ SQLite** — `uint` PKs and soft
  deletes, the same conventions Miabi uses. Attach a Miabi managed Postgres, or
  fall back to a zero-config SQLite file (pure-Go driver, no CGO).
- **Structured logging** via [`jkaninda/logger`](https://github.com/jkaninda/logger)
- **Env-based config** — auto-detects the driver; reads `DATABASE_URL` / `DB_*`
- **`AutoMigrate` on startup** with retry (tolerates the DB coming up a beat later)
- **First-run seeder** — populates the wall when the table is empty (`SEED=false` to skip)
- **`/healthz` readiness probe** that returns `503` until the database is reachable
- **Scales horizontally** — attach a Redis and live updates + presence fan out
  across every replica via pub/sub; without it, the in-process broker makes the
  classic "works on one replica" bug easy to demonstrate
- **Admin console** at `/admin`, guarded by a single `ADMIN_TOKEN` secret:
  overview (stats, replicas, runtime), moderation (pin / hide / delete, signer
  IP, CSV export), wall settings (pause signing, announcement banner) — all pushed live
- **Chaos lab** (`DEBUG_ENDPOINTS=true`): crash, fail health checks, burn CPU,
  hold memory, burst logs, inject latency/5xx, plus an in-browser traffic probe
  that shows load-balancing and canary splits
- **Scheduled-job friendly CLI** — `guestbook cleanup` (purge/expire) and
  `guestbook migrate` subcommands for Miabi cron jobs and one-off jobs

**Presenting it?** [`DEMO.md`](DEMO.md) is a step-by-step training runbook.
- **Single static binary** — the whole UI is embedded with `go:embed`, so the
  image is one distroless layer with **no Node build step**

## Why this UI approach

For an example that should deploy in seconds, the frontend is a **single
embedded `index.html`** — modern CSS (glassmorphism cards, gradient mesh, dark
theme) plus a little vanilla JS calling the JSON API. No CDN, no bundler, no
`node_modules`. It looks polished, loads instantly, works offline/air-gapped,
and keeps the container tiny.



## API

| Method   | Path                 | Description                        |
|----------|----------------------|------------------------------------|
| `GET`    | `/healthz`           | Liveness/readiness (checks the DB), reports `version`, `host`, `redis` |
| `GET`    | `/api/info`          | App name, serving `version`, `host`, online count, `broker` mode, `replicas` |
| `GET`    | `/api/time`          | Current server time + `version` + `host` (v2 — canary probe) |
| `GET`    | `/api/settings`      | Public wall settings: `signing_paused`, `banner` |
| `GET`    | `/api/entries`       | Visible entries (pinned first) + total; paginated via `?limit=&offset=` |
| `POST`   | `/api/entries`       | Create `{ "name", "message" }` (broadcast live; `503` while paused) |
| `GET`    | `/api/stream`        | **SSE** stream: `welcome`, `created`, `updated`, `deleted`, `settings`, `presence`, `tick` events |
| `GET`    | `/` · `/all` · `/admin` | The web UI (home · all signatures · admin console) |

### Admin API

Authenticated with the session cookie from `POST /api/admin/login {"token"}`
or an `Authorization: Bearer <ADMIN_TOKEN>` header. Disabled (`404`) when
`ADMIN_TOKEN` is unset.

| Method   | Path                          | Description |
|----------|-------------------------------|-------------|
| `GET`    | `/api/admin/overview`         | Stats (14-day histogram), replicas, runtime of the answering replica |
| `GET`    | `/api/admin/entries`          | All entries; `?status=visible\|hidden\|pinned&q=&limit=&offset=` |
| `PATCH`  | `/api/admin/entries/{id}`     | `{ "pinned"?: bool, "hidden"?: bool }` |
| `DELETE` | `/api/admin/entries/{id}`     | Soft-delete an entry |
| `GET`·`PUT` | `/api/admin/settings`      | `{ "signing_paused": bool, "banner": string }` |
| `GET`    | `/api/admin/export.csv`       | Every entry as CSV |

### Debug API (chaos)

Requires admin auth **and** `DEBUG_ENDPOINTS=true`. Each call affects only the
replica that receives it and echoes its `host`.

| Method | Path | Effect |
|--------|------|--------|
| `POST` | `/api/debug/crash` | Exit with status 1 |
| `POST` | `/api/debug/health` | `{ "healthy": false }` makes `/healthz` return 503 |
| `GET`  | `/api/debug/slow?ms=2000` | Sleep before answering (max 30 s) |
| `GET`  | `/api/debug/error?status=500` | Answer with a 4xx/5xx |
| `POST` | `/api/debug/cpu?seconds=30` | Saturate all cores (max 120 s) |
| `POST` | `/api/debug/memory?mb=256&seconds=30` | Hold resident memory (max 2048 MiB) |
| `POST` | `/api/debug/logs?count=20&level=error` | Write a burst of log lines |

### Live updates (SSE)

`/api/stream` is a Server-Sent Events endpoint. On connect the client gets a
`welcome` event (serving version + host + online count); thereafter the server
pushes `created` / `deleted` events as the wall changes, `presence` events when
the connected-client count changes, and a `tick` event once a second carrying
the live server time (with `version` + `host`). The UI uses these to update the
wall in real time, show a live **“N online”** indicator, and drive the
**server-time card** — all without polling.

With `REDIS_URL` set, `created`/`updated`/`deleted`/`settings` events go through
a Redis pub/sub channel so every replica delivers them, and each replica
advertises its client count under a short-TTL key so presence is cluster-wide.
`tick` events stay per-replica on purpose: the clock shows who served you.

## Configuration

| Variable       | Default          | Purpose                                   |
|----------------|------------------|-------------------------------------------|
| `PORT`         | `8080`           | HTTP listen port                          |
| `APP_NAME`     | `Miabi Guestbook`| Display name in the UI / health payload   |
| `APP_VERSION`  | build version    | Overrides the version shown in the UI / `/api/info`. Defaults to the value baked at build time (`-ldflags "-X main.version=…"`, or the `VERSION` build-arg). |
| `SEED`         | `true`           | Seed sample entries on first run (empty table only) |
| `DB_DRIVER`    | _(auto)_         | `postgres` or `sqlite`. Unset → auto: Postgres when configured, else SQLite. |
| `DB_PATH`      | `data/guestbook.db` (`/data/guestbook.db` in the image) | SQLite file path (or `:memory:`). Its parent dir is created on startup. The container stores it on the `/data` volume. |
| `DATABASE_URL` | —                | Full Postgres DSN (URL or key=value). Preferred — set for you when you attach a Miabi **managed database**. Takes precedence over `DB_*`. |
| `DB_HOST`      | `localhost`      | Database host (used when `DATABASE_URL` is empty) |
| `DB_PORT`      | `5432`           | Database port                             |
| `DB_USER`      | `postgres`       | Database user                             |
| `DB_PASSWORD`  | `postgres`       | Database password                         |
| `DB_NAME`      | `guestbook`      | Database name                             |
| `DB_SSL_MODE`  | `disable`        | Postgres `sslmode`                        |
| `REDIS_URL`    | —                | Redis URL (`redis://:pass@host:6379`). Enables cross-replica live updates. Falls back to `REDIS_DATABASE_URL`, which is what Miabi injects when you attach a managed Redis with the `REDIS` prefix. |
| `ADMIN_TOKEN`  | —                | Enables `/admin`. Store it as a **secret**. |
| `TRUSTED_PROXIES` | —             | Comma-separated CIDRs whose `X-Forwarded-For` is trusted when recording a signer's IP (e.g. `10.0.0.0/8`). Unset: the header is taken as-is — fine behind the Miabi gateway, spoofable if the app is exposed directly. |
| `DEBUG_ENDPOINTS` | `false`       | `true` enables the `/api/debug/*` chaos endpoints (still admin-only). |

> **On Miabi:** attach a managed database to the app and Miabi injects these
> variables automatically — `DATABASE_URL` for the full DSN, plus the discrete
> `DB_*` parts. You don't set them by hand.

## Run locally

With Docker Compose (Postgres + app):

```bash
docker compose up --build
# open http://localhost:8080
```

Or run the binary directly. With nothing configured it uses a zero-config
SQLite file, so this just works:

```bash
go run .                 # runs the default "server" command
go run . server -p 9000  # okapicli flags: choose the port
go run . --help          # list commands and flags
go run . migrate         # apply migrations and exit
go run . cleanup --purge-after 168h --max-age 720h --dry-run
```

To point it at your own Postgres, set `DATABASE_URL` (or the `DB_*` vars) first:

```bash
cp .env.example .env            # edit DATABASE_URL
export $(grep -v '^#' .env | xargs)
go run .
```

## Build the image

```bash
docker build --build-arg VERSION=1.0.0 -t miabi-guestbook:1.0.0 .
docker run -p 8080:8080 -e DATABASE_URL=postgres://... miabi/guestbook:1.0.0
```

The version can also be overridden at runtime with `-e APP_VERSION=…`.

## Canary deployment demo

The version badge makes this app a ready-made **canary** example. Build two
otherwise-identical images and let Miabi split traffic between them — the badge
(and `/api/info`) tells you which build served each request:

```bash
docker build --build-arg VERSION=1.0.0 -t miabi-guestbook:1.0.0 .
docker build --build-arg VERSION=2.0.0 -t miabi/guestbook:2.0.0 .
```

1. Deploy `1.0.0` as the app; attach a **shared** managed Postgres.
2. Roll out `2.0.0` as a **canary** with a small weight (e.g. 10%).
3. Refresh the page a few times: ~1 in 10 loads shows the `v2.0.0` badge (a
   different colour) **and the new live server-time card**, the rest `v1.0.0`
   without it. Because both versions share the same database, signatures created
   on either build appear on both — and stream live to every open tab via SSE.
4. Watch routing from the shell — each request may hit a different build/replica:
   ```bash
   watch -n1 'curl -s https://your-domain/api/time'
   # {"time":"…","version":"2.0.0","host":"…"}  ← version/host flips under canary
   ```
5. Shift the weight up and promote `2.0.0` when you're happy.

> The server-time card is the **v2 change**: v1 (built before this feature) has
> no clock, so its presence — and the version/host it shows — is an at-a-glance
> signal of which build a viewer landed on.

> No build args? Set `APP_VERSION=1.0.0` / `APP_VERSION=2.0.0` in each app's
> environment instead — same effect.

## Deploy on Miabi

1. **Create a database** — provision a PostgreSQL database in your workspace.
2. **Create an application** — deploy this repo (Git build) or the image above.
3. **Attach the database** — Miabi injects the connection as `DATABASE_URL`.
4. **Set the health check** to `GET /healthz` and the port to `8080`.
5. **Connect a domain** — Miabi issues automatic SSL and routes traffic.
6. **Optional:** add `ADMIN_TOKEN` as a secret, scale replicas, and attach a
   managed Redis with the `REDIS` prefix. See [`DEMO.md`](DEMO.md).

The app migrates its schema on first boot, so there is no separate release step.
