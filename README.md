# Miabi Guestbook — example app

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
- **Version badge** — the serving build's version is shown in the UI and
  `/api/info`; ideal for watching a **canary rollout** decide which build answers
- **[GORM](https://gorm.io) with PostgreSQL _or_ SQLite** — `uint` PKs and soft
  deletes, the same conventions Miabi uses. Attach a Miabi managed Postgres, or
  fall back to a zero-config SQLite file (pure-Go driver, no CGO).
- **Structured logging** via [`jkaninda/logger`](https://github.com/jkaninda/logger)
- **Env-based config** — auto-detects the driver; reads `DATABASE_URL` / `DB_*`
- **`AutoMigrate` on startup** with retry (tolerates the DB coming up a beat later)
- **First-run seeder** — populates the wall when the table is empty (`SEED=false` to skip)
- **`/healthz` readiness probe** that returns `503` until the database is reachable
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
| `GET`    | `/healthz`           | Liveness/readiness (checks the DB), reports `version` |
| `GET`    | `/api/info`          | App name, serving `version`, connected-client count |
| `GET`    | `/api/entries`       | List entries + total; paginated via `?limit=&offset=` |
| `POST`   | `/api/entries`       | Create `{ "name", "message" }` (broadcast live) |
| `DELETE` | `/api/entries/{id}`  | Delete an entry (broadcast live)   |
| `GET`    | `/api/stream`        | **SSE** stream: `welcome`, `created`, `deleted`, `presence` events |
| `GET`    | `/` · `/all`         | The web UI (home · all-signatures page) |

### Live updates (SSE)

`/api/stream` is a Server-Sent Events endpoint. On connect the client gets a
`welcome` event (serving version + online count); thereafter the server pushes
`created` / `deleted` events as the wall changes and `presence` events whenever
the number of connected clients changes. The UI uses these to update the wall in
real time and show a live **“N online”** indicator — no polling.

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
   different colour), the rest `v1.0.0`. Because both versions share the same
   database, signatures created on either build appear on both — and stream
   live to every open tab via SSE.
4. Shift the weight up and promote `2.0.0` when you're happy.

> No build args? Set `APP_VERSION=1.0.0` / `APP_VERSION=2.0.0` in each app's
> environment instead — same effect.

## Deploy on Miabi

1. **Create a database** — provision a PostgreSQL database in your workspace.
2. **Create an application** — deploy this repo (Git build) or the image above.
3. **Attach the database** — Miabi injects the connection as `DATABASE_URL`.
4. **Set the health check** to `GET /healthz` and the port to `8080`.
5. **Connect a domain** — Miabi issues automatic SSL and routes traffic.

The app migrates its schema on first boot, so there is no separate release step.
