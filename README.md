# Tideline

A modern, self-hostable diabetes data server that speaks the Nightscout API.

Tideline is a single small binary with SQLite built in. Point xDrip+, Loop,
Trio or a Nightscout watch face at it and they work unchanged. It also brings
things Nightscout never had: alerts evaluated on the server, escalation to a
second person, a token for each person or device with an audit log, and
consensus-standard AGP reports.

> **Not a medical device.** Tideline displays and relays data. It does not make
> treatment decisions and must not be your only alarm.

## What's in v1

| | |
|---|---|
| **Nightscout API v1** | `entries`, `treatments`, `devicestatus`, `profile`, `food`, `status`, `verifyauth`, `/pebble`, with Mongo-style `find[field][$op]` queries and de-duplication on resend |
| **Nightscout API v3** | CRUD, search, incremental `history` sync, `lastModified` and JWT authorization: what AndroidAPS's NSClient v3 uses |
| **Live web app** | Current glucose, trend, delta, IOB/COB/pump/phone status, a 3–24h chart with treatments, glucose forecasts from Loop, Trio and AndroidAPS, a basal lane with temp basals, and treatment logging. Updates instantly over Server-Sent Events. Installable as a PWA, with light and dark themes |
| **Server-side alerts** | Urgent low, low, predicted low, high, urgent high, stale and urgent stale. Snooze from any device |
| **Push notifications** | Anyone signed in, followers included, can get alerts on their own phone or computer with Web Push: no extra app or account. Admins can also send to [ntfy](https://ntfy.sh), Pushover (emergency priority for urgent alerts) or a webhook |
| **Escalation** | Each notification target can wait N minutes before it fires, e.g. your phone first, then a partner after 15 minutes |
| **Access control** | Private by default. Admin, uploader and follower tokens, each with an optional expiry, a last-used time and one-click revoke. Every change is audit-logged |
| **Reports** | Time in ranges (including tight range), mean, GMI, CV, CGM coverage, low events, an AGP percentile chart and a daily table |
| **Dexcom Share and LibreLinkUp bridges** | Pull readings directly from Dexcom or FreeStyle Libre, so you don't need an uploader phone |
| **Nightscout import** | `tideline import` copies your history from an existing site |

## Run it

With [mise](https://mise.jdx.dev), which installs the right Go and Node:

```sh
mise install
mise run dev       # http://localhost:8080, sign in with dev-secret-change-me
mise run seed      # optional: 14 days of synthetic data to look at
```

Without mise you need Go 1.26:

```sh
TIDELINE_API_SECRET='a-long-random-secret' go run ./cmd/tideline
```

### Docker

```sh
TIDELINE_API_SECRET='a-long-random-secret' docker compose up -d
```

Data lives in the `tideline-data` volume (`/data/tideline.db`).

### Fly.io

With [flyctl](https://fly.io/docs/flyctl/install/) installed and logged in:

```sh
mise run deploy:fly          # or: scripts/deploy-fly.sh [app-name] [region]
```

This creates the app, a 1 GB volume and an API secret, deploys a single
machine and prints your site's URL and secret. It costs about $2–3 a month.
`fly.toml` keeps the machine running when idle (auto-stop would silence
alerts) and checks `/healthz`.

Run exactly one machine: the database is on that machine's volume. If you
deploy by hand, use `fly deploy --ha=false`. Fly snapshots volumes daily and
keeps them for 5 days.

### Railway

1. New project → **Deploy from GitHub repo** → choose this repository.
   `railway.json` sets the Dockerfile build, health check and restart policy.
2. On the service, add a **volume** mounted at `/data`.
3. Add a variable `TIDELINE_API_SECRET` (12+ random characters).
4. Under **Settings → Networking**, generate a domain.

### Other hosts

Deploy the `Dockerfile` with a persistent volume at `/data` and set
`TIDELINE_API_SECRET`. The container starts as root only to take ownership
of the volume (many hosts mount volumes as root), then drops to an
unprivileged user before opening the database or the network. On Railway and
Fly.io the public URL is detected automatically; elsewhere set
`TIDELINE_PUBLIC_URL` so notifications link back to your site.

## Configuration

| Variable | Default | |
|---|---|---|
| `TIDELINE_API_SECRET` | required | Admin secret, 12+ characters. `API_SECRET` is also accepted |
| `TIDELINE_DB` | `tideline.db` | SQLite path |
| `PORT` | `8080` | |
| `TIDELINE_PUBLIC_URL` | detected on Railway and Fly.io | Linked from notifications |
| `TIDELINE_RUN_AS` | `65532:65532` in the image | When started as root, take ownership of the data directory and run as this user |
| `TIDELINE_PUBLIC_READ` | `false` | Let anyone with the URL view data (Nightscout's default) |
| `TIDELINE_UNITS` | `mg/dl` | Initial units (`mg/dl` or `mmol`), changeable in Settings |
| `TIDELINE_NAME` | `Tideline` | Initial site name |
| `TIDELINE_LOG_LEVEL` | `info` | |
| `TIDELINE_VAPID_SUBJECT` | public URL | Contact URL or `mailto:` given to push services |
| `DEXCOM_USERNAME` / `DEXCOM_PASSWORD` / `DEXCOM_REGION` | | Enables the Share bridge. Region is `us`, `ous` or `jp` |
| `LIBRE_EMAIL` / `LIBRE_PASSWORD` | | Enables the LibreLinkUp bridge: use a LibreLinkUp *follower* account that the Libre user shares with |
| `LIBRE_REGION` | auto | Usually detected automatically; set (e.g. `eu`, `us`, `de`) to skip the redirect |
| `LIBRE_PATIENT_ID` | first connection | Which person to follow if the account follows several |
| `LIBRE_VERSION` | `4.16.0` | LibreLinkUp app version to present; raise it if Abbott starts rejecting logins |

Thresholds, alerts and notification targets are configured in the web app
under **Settings**.

## Getting alerts on your phone

Open **Settings → Alerts on this device** on each phone or computer and choose
**Get alerts on this device**. Each person can choose all alerts or urgent
only, and a delay, so a partner is only woken if an alert hasn't been dealt
with. Alerts keep arriving when Tideline isn't open.

- **iPhone and iPad** (iOS 16.4 or later): first add Tideline to the Home
  Screen (Share → Add to Home Screen) and open it from there. Apple only
  allows web notifications for home-screen apps.
- **Android, Windows, Mac and Linux**: works in Chrome, Edge, Firefox and Safari.

Push needs HTTPS, so set up your host's TLS (Railway, Fly.io and Render do
this for you). Revoking someone's token stops alerts to all their devices.

## Connecting uploaders

Create a token in **Settings → Access** for each device, with *Uploader*
access, then:

- **xDrip+**: set the base URL to `https://<token>@your-site/api/v1/`
- **Loop, Trio and other Nightscout uploaders**: set the site URL to
  `https://your-site` and paste the token into the **API secret** field.
  These apps send a SHA-1 of that field, and Tideline matches it to the token.
- **AndroidAPS**: choose the **NSClient v3** plugin, set the URL to
  `https://your-site` and paste the token as the **access token**. AndroidAPS
  trades it for a short-lived JWT; revoking the token cuts it off immediately.
- **Followers**: create a *Follower* token and share the link it gives you.
  Nightscout watch faces accept `?token=` too.

The site API secret still works everywhere, but a separate token for each
device means you can revoke a lost phone without reconfiguring everything else.

## Moving from Nightscout

```sh
TIDELINE_DB=/data/tideline.db tideline import \
  -from https://old-site.example.com -token <read-token> -days 365
```

This copies entries, treatments, device status (skip with
`-skip-devicestatus`) and profiles. Imports are idempotent, so it's safe to
re-run.

## Other commands

```sh
tideline token -name "Mum" -role reader -expires-days 30   # create a token from the CLI
tideline version
```

## API

The native API lives under `/api/tl/v1`:

| | |
|---|---|
| `GET /me` | Who you are, your role and the settings |
| `GET /timeline?hours=24` | Readings, treatments, latest device status and active alerts |
| `GET /stream` | Server-Sent Events: `entries`, `treatments`, `devicestatus`, `alerts`, `settings` |
| `GET /report?days=14&tz=Europe/London` | Summary, AGP and daily stats |
| `GET`, `PUT /settings` | Settings (`PUT` is admin only) |
| `GET /alerts`, `POST /alerts/{kind}/snooze` | Active alerts; snooze with `{"minutes": 30}` |
| `GET`, `POST /tokens`, `DELETE /tokens/{id}` | Token management (admin) |
| `POST /notifiers/test` | Send a test notification (admin) |
| `GET /push/key`, `GET`, `POST /push/subscriptions`, `DELETE /push/subscriptions/{id}`, `POST /push/subscriptions/{id}/test` | Web Push device registration |
| `GET /audit` | Audit log (admin) |

Authenticate with `Authorization: Bearer <token>`, `?token=` or the
Nightscout-style `api-secret` header.

## Development

Tasks are defined in `mise.toml` (run `mise tasks` to list them):

| | |
|---|---|
| `mise run dev` | Run locally with debug logging, using `tideline-dev.db` |
| `mise run seed` | Load synthetic data into the running dev server |
| `mise run check` | Lint and test: the same as CI |
| `mise run test` | `go test -race ./...` |
| `mise run fmt` | Format Go code |
| `mise run build` | Build a static binary into `bin/` |
| `mise run docker` | Build the Docker image |
| `mise run deploy:fly` | Deploy to Fly.io |
| `mise run reset-dev-db` | Delete the dev database |

The dev API secret in `mise.toml` is for local use only.

The code layout:

- `cmd/tideline`: entry point and CLI
- `internal/store`: SQLite storage and Nightscout queries
- `internal/auth`: credentials and roles
- `internal/server`: HTTP API, event stream and Nightscout compatibility
- `internal/alerts`: alert engine and notification senders
- `internal/stats`: consensus metrics and AGP
- `internal/dexcom`: Share bridge
- `internal/importer`: Nightscout import
- `web/static`: the web app (plain JavaScript, no build step, embedded in the binary)

## Roadmap

- IOB and COB curves over time, and profile switches on the basal lane
- Clinician PDF export of the AGP report
- Postgres as an alternative store
