# Guestbook demo runbook

A step-by-step script for showing Miabi with the guestbook. Each act builds on
the previous one and takes 3–8 minutes. Skip acts freely: everything after
act 2 only assumes the app is deployed.

Keep two windows open: the **wall** (`https://<domain>/`) and the **admin
console** (`https://<domain>/admin`). A third tab on the Miabi app page is
handy for logs, events and scaling.

| Act | Shows | Miabi features |
|-----|-------|----------------|
| 1 | Deploy from Git / image | pipeline, build, domain + TLS |
| 2 | Managed Postgres | databases, env injection |
| 3 | Secrets + admin console | secret env vars |
| 4 | Scale out → broken live updates | replicas |
| 5 | Attach Redis → fixed | managed Redis, env prefix |
| 6 | Break it on purpose (+ rolling wave) | restarts, health gating, alerts, logs, analytics |
| 7 | Canary v1 → v2 | canary weights, promote |
| 8 | Scheduled cleanup + reset | cron jobs, one-off jobs |

---

## 0. Prepare (before the session)

```bash
docker build --build-arg VERSION=1.0.0 -t <registry>/guestbook:1.0.0 .
docker build --build-arg VERSION=2.0.0 -t <registry>/guestbook:2.0.0 .
docker push <registry>/guestbook:1.0.0 && docker push <registry>/guestbook:2.0.0
```

- Pick an admin token: `openssl rand -hex 16`.
- Create an empty Postgres and a Redis in the workspace ahead of time, since provisioning is slower than talking.
- Open `scripts/hammer.sh` in a terminal (see act 7).

## 1. Deploy

1. **New application**, from the Git repo (the `.miabi/pipeline.yaml` builds, scans and deploys) or from image `guestbook:1.0.0`.
2. Port `8080`. **Health check**: HTTP, path `/healthz`. (The default is *none*, and without it act 6 loses its punch.)
3. Add a domain. TLS is issued automatically.
4. Open the wall. It runs on **SQLite**, as the first seeded signature says.

> Talking point: zero config. With no database attached the app falls back
> to a local SQLite file. Mount a volume at `/data` if you want it to survive
> redeploys.

## 2. Attach Postgres

1. App → **Databases** → attach the Postgres, **no prefix**.
2. Miabi injects `DATABASE_URL` (as a secret reference) plus `DB_*` and flags the app for redeploy.
3. Redeploy. The app migrates the schema and seeds the wall on first boot. The welcome signature now says *PostgreSQL*.

## 3. Secrets and the admin console

1. App → **Environment** → add `ADMIN_TOKEN` = your token, marked **secret**. Add `DEBUG_ENDPOINTS=true` too (used in act 6).
2. Redeploy, open `/admin` and sign in with the token.
3. Tour it:
   - **Overview**: signatures per day, total reactions, replicas (with each one's **request counter**), and *this replica*'s runtime (host, DB, uptime, memory).
   - **Moderation**: each signature shows the signer's IP, forwarded by the gateway, plus its reaction tally. Click the IP to filter by it. Pin a signature. It jumps to the top of every open wall, live. Hide one and it disappears.
   - **Settings**: set a banner ("Welcome to the Miabi training 👋"), pick a **wall theme** (force light or dark — every open tab flips instantly), then toggle **Pause signing**. The wall goes read-only instantly in every tab.

> Talking point: the token never appears in the app's config in plain text.
> Rotating it (edit + redeploy) signs every admin out, because sessions are
> HMAC-signed with it.

Ask the audience to open the wall on their phones and sign it. The
**online** counter climbs live. Have them tap the ❤️ / 🎉 reactions — the
tallies move on every screen at once, with no refresh.

> Talking point — visitor identity: each browser also gets a `gb_visitor`
> cookie, and the wall greets it ("you are guest-… · N signatures"). The
> backend only accepts the cookie while its id exists in the database — in
> the admin console, the **Visitors** tile and table show every identity
> issued, and **Visiting** counts the ones seen in the last 5 minutes.
> Reactions are keyed on the same identity.

## 4. Scale out, and watch it break

1. App → **Replicas** → scale to **3**.
2. In the admin **Overview**, the *served by* chip changes as you refresh, but the **Replicas** table only ever lists one host. The hint says *in-process broker*.
3. Open the wall in two browsers (or phones) until their clock cards show **different hosts**. Sign in one: **the other doesn't update.** Each replica only pushes to its own SSE clients. Reactions are the same: a ❤️ on one host never shows on the other.
4. Also note the `via replica-x` tag on new signatures: each one records which replica accepted it. And `curl -s https://<domain>/healthz` a few times — the `requests` counter climbs on whichever replica answered, so you can watch the load-balancer spread traffic even though live updates stay broken.

> Talking point: this is the classic "works on one box" bug. Stateless apps
> need shared state for fan-out. Scaling is a platform feature; being
> scalable is an app property.

## 5. Attach Redis, and it's fixed

1. App → **Databases** → attach the Redis with prefix **`REDIS`**. Miabi injects `REDIS_DATABASE_URL`, which the app reads (so does `REDIS_URL`).
2. Redeploy. Startup logs show `live updates ready broker=redis`.
3. Repeat the two-browser test: signatures **and reactions** now appear everywhere. The **Replicas** table lists all 3 hosts with their client counts **and request counters**, and the wall header says *3 replicas*.
4. **Chaos lab → Traffic probe** → *Run probe*: the bars show requests spread across the three hosts. Compare with the per-replica `requests` counters in the table — they tell the same story server-side.

## 6. Break it on purpose

All buttons are in the admin **Chaos lab**. Each acts on *the replica that
receives the request*, and the activity log shows which one.

| Button | What happens | Where to look in Miabi |
|--------|--------------|------------------------|
| 💥 Crash it | process exits 1; the restart policy restarts it; the page reports when it's back | app **Events**, **Notifications** (crash-loop if repeated) |
| 🩺 Unhealthy | `/healthz` → 503 on that replica | `app_unhealthy` alert; a deploy made now fails the health gate |
| 🔥 CPU burn | all cores busy for N s | app metrics |
| 🧠 Hold memory | allocates N MiB. Set a **Memory limit** of 256 MB first, then hold 400 MiB → OOM kill → restart | `app_oom` alert |
| 📜 Log burst | N error lines | **Logs** tab (Live) |
| Probe `/api/debug/slow` | 400 ms latency | **Analytics → Performance** |
| Probe `/api/debug/error` | 5xx responses | **Analytics → HTTP Traffic** |

The same endpoints work from a shell:

```bash
T=<admin-token>; D=https://<domain>
curl -XPOST -H "Authorization: Bearer $T" $D/api/debug/crash
curl -XPOST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
     -d '{"healthy":false}' $D/api/debug/health
curl -XPOST -H "Authorization: Bearer $T" "$D/api/debug/memory?mb=400&seconds=30"
```

Remember to click **Healthy** afterwards, or redeploy.

### The rolling-update wave

Still in the Chaos lab, **Rolling-update wave → Bounce each replica, one at a
time** restarts the fleet exactly like a rolling update does, but driven by the
crash endpoint. Keep the **Replicas** table in view: each replica disappears,
comes back, and its **request counter restarts from ~0** while the others keep
climbing. The wall never goes down — which is the whole point of rolling
updates, shown from the inside.

> Talking point: recreate would drop every replica at once (all counters would
> reset together, with a gap of downtime). Rolling keeps capacity by replacing
> one at a time. Same mechanism, two strategies.

## 7. Canary v1 → v2

1. In a terminal: `scripts/hammer.sh https://<domain>`. It prints a live count and, on Ctrl-C, a tally of `status version host`.
2. Set **deploy strategy** to *canary* (initial weight 10%), and deploy image `2.0.0`.
3. On the wall, reload a few times: ~1 in 10 loads shows the **v2 badge** in a different colour **and the new live server-time card**. In the admin **Traffic probe**, the bars split by version; in the **Replicas** table, the two versions sit side by side with their own request counters.
4. **Promote now.** The probe goes 100% v2, and `hammer.sh` should show `0 failed`.

> The v2 build is the same code with a different `VERSION`. Nothing needs to
> differ for the demo to work — but the clock card (absent in v1) makes the
> split visible to the audience without a terminal.

## 8. Scheduled cleanup and reset

The binary has a `cleanup` subcommand that permanently removes deleted
signatures, optionally expires old ones, and prunes idle visitor identities:

```bash
guestbook cleanup --purge-after 168h              # default: purge deletes older than 7 days
guestbook cleanup --max-age 720h --dry-run        # preview expiring >30-day-old, unpinned entries
guestbook cleanup --prune-visitors 720h           # forget identities idle for 30 days (and their reactions)
```

1. **Jobs** → *New cronjob*, image blank (uses the app's image), command `/app/guestbook cleanup --purge-after 1h`, schedule `*/15 * * * *`.
2. Delete a signature in **Moderation**, then **Run now**. The job log shows `cleanup finished purged=… visitors_pruned=… remaining=…`.

There is also a `reset` subcommand to **clean the database** between sessions
— entries, reactions and visitor identities go (soft-deleted rows are wiped
for good), wall settings are kept unless `--hard`:

```bash
guestbook reset --dry-run          # report what would be removed
guestbook reset --reseed           # wipe, then insert the sample signatures
guestbook reset --hard --reseed    # also reset banner/pause/theme
```

3. **Jobs** → *New job*, command `/app/guestbook reset --reseed`. The wall
   empties immediately; every browser's `gb_visitor` cookie stops matching a
   database row, so on the next request each visitor is handed a **fresh
   identity** — the "you are …" card on the wall changes, and the admin
   Visitors table starts refilling from zero.

`guestbook migrate` works the same way as a one-off **Job**, for teams that
want migrations as an explicit step rather than on boot.

---

## Reset between sessions

- Admin → Settings: clear the banner, set the theme back to *System*, un-pause.
- Chaos lab: **Healthy**.
- To wipe the wall: run `/app/guestbook reset --reseed` as a one-off **Job**
  (entries, reactions and visitor identities go; the wall is re-seeded), or
  detach/recreate the database. Browsers pick up a fresh identity cookie on
  their next request either way.
