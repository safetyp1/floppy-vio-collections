# Floppy to Vio Collections

This separate version mirrors Floppy lists into Vio collections and requests
missing titles through Vio's `/api/v2/requests` endpoint. It does not need Sonarr
or Radarr in Vio mode. Vio handles fulfillment; later syncs add playable titles
to the collections.

## Vio Setup

Start with [config.example.json](config.example.json), which selects
`"acquisition_mode": "vio"`. Remove any `radarr` and `sonarr` sections: combining
them with Vio mode is rejected at startup.

The inherited configuration names `silo`, `SILO_TOKEN`, and `SILO_DATABASE_URL`
are retained, but **all three must point to Vio**, not the original Silo server:

- Set `silo.url` to the Vio API origin, without `/api/v2`.
- Supply Vio's token through `SILO_TOKEN` or `silo.token_file`. It must permit
   catalog reads, collection management, request details and request creation.
- Set `silo.profile_id` to the Vio profile used for requests and collections.
- Set `SILO_DATABASE_URL` to Vio's database using a read-only matching account.
   It needs `SELECT` on `media_items`, `media_files`, and `episodes`.
- Use Vio library IDs and Vio collection IDs in the bindings. Set `collection_id`
   to adopt an existing manual collection instead of creating another one.
- Use a **new state database and separate container**. Never reuse the original
   Silo worker's state or change its live configuration for this version.

Keep the Floppy URL and list IDs for the source lists you want to mirror. Use the
existing binding options documented below, including `max_items` and
`latest_released`. The example IDs are placeholders, not deployment settings.

```sh
go test -race ./...
go build -o floppy-vio-collections .
./floppy-vio-collections -config config.json -check-config
./floppy-vio-collections -config config.json -once
./floppy-vio-collections -config config.json -once -apply
```

Dry-run performs collection reads and previews missing-media requests, but does
not contact the Vio request endpoints or submit anything. For continuous operation,
omit `-once`; the default interval is three hours after the previous cycle.

For Docker, build this folder's Dockerfile. The Compose example in the legacy
reference below also works after removing Arr credentials and its `arr` network,
setting the server credentials to Vio, and using a distinct state volume and
the Vio database network. Do not attach this worker to Silo's private database
network. The image entrypoint is `floppy-vio-collections`; it runs as UID 10001.

## Vio Request Behavior

- Acquisition requires an exact **TMDB** identity. Floppy `tv` maps to Vio
   `series`. IMDb-only and TVDB-only entries still match available catalog media,
   but missing entries with those identities report an acquisition error. No title
   guessing or identity rewriting occurs.
- Metadata comes from `/api/v2/requests/detail/{movie|series}/{tmdb_id}`. Its type
   and ID must match before the title, year, external IDs and artwork paths are
   submitted to `/api/v2/requests`.
- Existing requests and available titles are recognized before submission.
   SQLite records accepted submissions across restarts and overlapping lists,
   scoped to the Vio URL, profile, media type and TMDB ID.
- Up to **16** missing items per list are processed concurrently. API starts
   remain paced at 10 per second. A successful submission is not proof that the
   item is playable; Vio owns fulfillment.
- After a timeout, error, or missing response ID, the worker reads the detail
   again. A stored request or confirmed availability completes the submission.
   Otherwise an uncertain result stays blocked across restarts, rather than being
   blindly POSTed again. Later cycles can reconcile it when a request appears.
- Definite 4xx rejections, except 408/409, may be tried next cycle. Cancelled or
   failed requests already recorded as submitted are not automatically replaced.
- The per-list log says `Vio: ... requests submitted`. The inherited summary's
   `Searched` column counts these submissions, not downloads or indexer searches.

No services are deployed or restarted by building this source. The original
Floppy-to-Silo project remains separate. Legacy Arr acquisition is still available
with `"acquisition_mode": "arr"` (also the default when the field is omitted).

## Published Image

GitHub Actions runs the tests and publishes AMD64 and ARM64 images to
`ghcr.io/safetyp1/floppy-vio-collections` on pushes to `main`. Builds publish
`latest` and immutable commit-based `sha-...` tags; version tags also publish
their semantic version. Pull requests run tests without publishing.

Use the image digest printed in the workflow summary for a pinned deployment.
The first published GHCR package must be made public in its package settings
for anonymous pulls. Runtime credentials and deployment configuration are not
part of the source repository or container image.

## Legacy Silo and Arr Reference

The remainder documents the inherited **Arr mode**, not the Vio setup above.
In Vio mode, use Vio's URL, credentials, database, library IDs and collection IDs
where this reference says Silo, and ignore the Arr-specific setup and behavior.

A background worker that turns your Floppy lists into ordered Silo collections.
It includes media available in your Silo libraries and can optionally add missing
movies to Radarr or TV series to Sonarr and trigger a search.

## Scenarios This Solves

- **Keep curated collections current:** mirror a Floppy watchlist or regularly
   updated list into Silo without manually maintaining collection membership.
- **Show only available media:** keep missing titles pending and add them to the
   collection when Silo discovers their files, even if the Floppy list has not changed.
- **Request missing media automatically:** send missing movies to Radarr and series
   to Sonarr, then include them in Silo collections on a later sync after import.
- **Sync without downloading:** omit both Arr integrations to manage collections
   using only media you already have.

This is a worker, not a media server, download client or web interface. It does not
modify Floppy lists or configure your indexers and download clients.

Silo matching supports **TMDB, IMDb and TVDB IDs**, with movies and series kept
separate. Radarr acquisition supports **TMDB and IMDb movie IDs**, not TVDB-only
movie IDs. Sonarr acquisition accepts TMDB, IMDb or TVDB identities only when its
lookup service resolves the ID and returns the same identity. Missing or ambiguous
matches are reported; the worker does not guess by title.

## Features and Behavior

- Preserves Floppy item order by default and avoids duplicate collection members.
- Optionally selects up to 200 of the most recently released titles from a larger
   list, ordered newest first using Floppy's release-date metadata.
- Defaults to dry-run. Remote writes require `-apply`.
- Retains previously matched members while temporarily unavailable, provided their
   source entry remains listed. Source changes defer removals until a later cycle
   sees the same source snapshot. A confirmed empty list can empty its collection.
- Adds new Arr items as monitored and enables search-on-add. New series use
   `monitor: all` and search for missing episodes. Existing items receive a search
   without changing their paths, profiles or monitoring choices; unmonitored items
   or series with no monitored seasons report an error.
- Persists collection mappings and submitted acquisition requests in SQLite,
   preventing repeated searches across syncs, restarts and overlapping list bindings.

**What counts as missing:** no playable match in the binding's configured Silo
libraries. A movie needs a file version; a series needs at least one playable
episode. This is not a series-completion checker, and it does not test playback.

**Collection ownership:** treat each managed collection as an authoritative mirror.
Manual additions may be removed. Titles are set when creating collections, not
continually updated. Removing a list binding does not delete its collection.

**Search is not download completion:** submitted requests are not automatically
searched again by this worker. Sonarr/Radarr handles monitoring and downloads;
repeat searches can be started in its UI. This worker does not poll download queues,
cancel downloads or delete Arr items when a Floppy entry disappears. Collection
updates and acquisition are separate operations, so partial progress is possible.

## Docker Compose Setup

### Prerequisites

- Docker with Docker Compose v2 or newer, and this project's source files.
- Existing Silo and Floppy services, with tokens allowed to read the relevant
   libraries/lists and, for Silo, manage collections.
- A PostgreSQL account with `CONNECT` on Silo's database, `USAGE` on schema `public`,
   and `SELECT` on `public.media_items`, `public.media_files` and `public.episodes`.
   Use a dedicated read-only account, not a database administrator.
- For acquisition: existing Sonarr/Radarr instances with working indexers, download
   clients, quality profiles and root folders. Imported media must become visible
   in the Silo libraries configured below.

### 1. Configure Lists and Services

Use [config.example.json](config.example.json) as the template for `config.json`.
Replace the placeholder URLs, list IDs, library IDs, profile IDs and root folders.
The example enables both Arr integrations; remove either section to disable it.
Its TV binding selects the latest 200 released series. Remove `max_items` and
`selection` to mirror a complete list of up to 200 entries in Floppy order.
Use `"state_path": "/data/sync.db"` to store state in the Compose volume below.

- Service URLs must be reachable **from the worker container**. For shared Docker
   networks, examples are `http://silo:8080`, `http://floppy:8000`,
   `http://radarr:7878` and `http://sonarr:8989`.
- Use actual container names or network aliases and their internal listening ports.
   `localhost` means the worker itself. For services on another host, use a reachable
   hostname/IP and exposed port instead.
- Base paths are supported, for example `https://example.com/sonarr`. Do not append
   `/api/v3` to Arr URLs. HTTP redirects are not followed, so use the final API origin.
- Choose quality profiles in each Arr app under **Settings > Profiles**. The
   numeric ID is normally visible in the profile editor URL; API v3's
   `/qualityprofile` endpoint also returns IDs and names. IDs are instance-specific.
- Use an existing root folder from **Settings > Media Management > Root Folders**.
   This is a path inside the Arr container, such as `/movies` or `/tv`, not a path
   inside the worker. The worker itself does not need media-volume mounts.

### 2. Define the Worker

Create `compose.yaml` beside the [Dockerfile](Dockerfile):

```yaml
services:
   collection-sync:
      build:
         context: .
      restart: unless-stopped
      command: ["-config", "/config/config.json"]
      environment:
         SILO_TOKEN: ${SILO_TOKEN:-}
         FLOPPY_TOKEN: ${FLOPPY_TOKEN:-}
         SILO_DATABASE_URL: ${SILO_DATABASE_URL:?Set SILO_DATABASE_URL}
         RADARR_API_KEY: ${RADARR_API_KEY:-}
         SONARR_API_KEY: ${SONARR_API_KEY:-}
      volumes:
         - ./config.json:/config/config.json:ro
         - collections-state:/data
      networks:
         - media
         - database
         - arr

volumes:
   collections-state:

networks:
   media:
      external: true
      name: ${MEDIA_NETWORK:-proxy}
   database:
      external: true
      name: ${DATABASE_NETWORK:-silo_backend}
   arr:
      external: true
      name: ${ARR_NETWORK:-arr}
```

This starts only the worker, not Silo, Floppy, PostgreSQL, Sonarr or Radarr.
The external networks must already exist on this Docker host and connect the
relevant services. Adjust their names to your setup. If Arr integration is disabled,
remove `arr` from both the service's network list and the top-level networks section.
If all services share one network, a single shared network is sufficient.

No published ports are needed: the worker makes outbound requests only. Use HTTPS
or trusted private networks for API and database connections.

### 3. Supply Credentials

Set these variables in your Compose manager, or in an uncommitted `.env` file
beside `compose.yaml`. Replace every example value:

```dotenv
SILO_TOKEN='replace-with-silo-token'
FLOPPY_TOKEN='replace-with-floppy-token'
SILO_DATABASE_URL='postgresql://silo_collections:CHANGE_ME@silo-postgres:5432/silo'
RADARR_API_KEY='replace-with-radarr-api-key'
SONARR_API_KEY='replace-with-sonarr-api-key'
MEDIA_NETWORK=proxy
DATABASE_NETWORK=silo_backend
ARR_NETWORK=arr
```

Use your actual database host/network alias, database name and read-only account.
Percent-encode special characters in URL credentials; configure PostgreSQL TLS as
appropriate for your network. Never commit real credentials. Sonarr/Radarr API keys
are under **Settings > General > Security**. Omit keys for disabled integrations.

### 4. Validate, Preview and Enable

Before starting the persistent worker:

```sh
docker compose build
docker compose run --rm --no-deps collection-sync -config /config/config.json -check-config
docker compose run --rm --no-deps collection-sync -config /config/config.json -once
```

The first run validates config without API calls. The second reads Floppy and Silo
and previews changes without remote writes. Dry-run logs missing-media intentions
but does **not** contact Sonarr/Radarr or validate their profiles, paths or keys.
Configured integrations still require credentials at startup.

After reviewing the preview, change the service's `command` to enable writes:

```yaml
command: ["-config", "/config/config.json", "-apply"]
```

```sh
docker compose up -d collection-sync
docker compose logs --tail=100 -f collection-sync
```

With Arr integrations enabled, the first applied cycle may request all currently
missing entries in your configured lists. Review those lists before enabling it.

Only one worker may use a state database. Stop the running service before using
`docker compose run` for another sync. After configuration, environment or source
changes, use `docker compose up -d --build --force-recreate collection-sync` to
reload them. Do not use `docker compose down -v` unless you intend to delete state.

## Configuration Reference

### JSON Configuration

Unknown JSON fields are rejected. URLs, profiles, root folders and lists belong
in this file: there are no worker environment variables for those settings, and
`${VARIABLE}` placeholders in JSON are not expanded.

| Option | Required? | Default / description |
| --- | --- | --- |
| `silo.url` | Yes | Silo base URL. HTTP(S), optional base path; no embedded credentials, query or fragment. |
| `silo.token_file` | No | Token file path inside the worker; overrides `SILO_TOKEN`. |
| `silo.profile_id` | No | Silo profile UUID sent as `X-Profile-Id`. Use the profile with access to the target libraries. |
| `floppy.url` | Yes | Floppy base URL, with the same URL restrictions as Silo. |
| `floppy.token_file` | No | Token file path inside the worker; overrides `FLOPPY_TOKEN`. |
| `interval` | No | `3h`. Wait after each cycle; accepts Go durations from `1m` to `24h`. Recommended `3h`-`6h`. |
| `state_path` | No | `state/sync.db`, relative to the container's `/data` working directory. Set `/data/sync.db` for an explicit persistent path. |
| `lists` | Yes | Array of 1-100 bindings described below. A mixed-media source list can have separate movie and TV bindings. |
| `radarr` | No | Omit or set to `null` to disable movie acquisition. |
| `sonarr` | No | Omit or set to `null` to disable TV acquisition. |
| `radarr.url` / `sonarr.url` | If integration enabled | Arr base URL, without `/api/v3`; same URL restrictions as Silo. |
| `radarr.api_key_file` / `sonarr.api_key_file` | No | API key file path inside the worker; overrides the corresponding API key environment variable. |
| `radarr.quality_profile_id` / `sonarr.quality_profile_id` | If integration enabled | Positive integer ID of an existing profile. Used only for new additions. |
| `radarr.root_folder_path` / `sonarr.root_folder_path` | If integration enabled | Existing root folder as seen by that Arr instance. Used only for new additions. |

Each object in `lists` has these options:

| Option | Required? | Description |
| --- | --- | --- |
| `key` | Yes | Unique, stable lowercase slug, 1-64 characters; letters, digits and hyphens, starting with a letter or digit. Identifies durable state. |
| `list_id` | Yes | Positive numeric Floppy list ID, encoded as a string, e.g. `"12"`. |
| `media_type` | Yes | `movie` or `tv`. Selects source entries and the appropriate Arr integration. |
| `library_ids` | Yes | Nonempty array of distinct positive numeric Silo library ID strings, e.g. `["1", "2"]`. Matching uses these as Silo `media_folder_id` values. |
| `title` | Yes | Nonblank title used when creating the collection. Does not rename an existing collection. |
| `collection_id` | No | Explicitly adopt an existing Silo collection; otherwise create one. Must be manual type/management mode with the same libraries. Two bindings cannot own the same collection. |
| `max_items` | No | Integer 1-200 to select a bounded subset. Omitted or `0` requires the complete source list to contain at most 200 entries. Without `selection`, takes the first N source entries before filtering media type. |
| `selection` | No | `latest_released` requires `max_items`. Reads the complete source list (maximum 10,000 entries), filters to the binding's media type and known release dates on or before today (UTC calendar date), then selects the newest N. Date ties use provider identity for deterministic ordering. |

For Fresh TV, add these fields to its existing binding:

```json
"max_items": 200,
"selection": "latest_released"
```

This uses series premiere dates, not the newest episode or when a show was added
to Floppy. Unknown dates and future premieres are excluded; inaccurate source
dates can affect selection. Only the selected entries are matched or requested
through Sonarr/Radarr. The Silo collection may contain fewer than 200 shows until
files are available. Selection is recalculated each cycle, with the usual source
recheck and deferred-removal protection. Changing these two options keeps the
existing collection mapping; confirmed removals may occur on a later cycle.

Keep binding identities stable after the first applied run. For an existing key,
changing the Silo/Floppy URL, Silo profile, source list, media type or library mapping
is rejected and requires an explicit migration. Renaming a key creates a new mapping
instead of migrating the old one. Changing an Arr URL changes acquisition deduplication
scope and can cause repeat requests; changing an Arr profile/root affects only new additions.

### Environment Variables

| Variable | Required? | Description |
| --- | --- | --- |
| `SILO_TOKEN` | Unless `silo.token_file` is set | Silo bearer token for catalog reads and collection management. |
| `FLOPPY_TOKEN` | Unless `floppy.token_file` is set | Floppy bearer token with access to configured lists. |
| `SILO_DATABASE_URL` | Yes for sync | PostgreSQL connection string for read-only matching. No corresponding JSON or secret-file option. |
| `RADARR_API_KEY` | When Radarr enabled, unless `radarr.api_key_file` is set | Radarr API key, sent as `X-Api-Key`. |
| `SONARR_API_KEY` | When Sonarr enabled, unless `sonarr.api_key_file` is set | Sonarr API key, sent as `X-Api-Key`. |
| `MEDIA_NETWORK` | No; Compose example only | Existing network shared with Silo/Floppy. Defaults to `proxy`. |
| `DATABASE_NETWORK` | No; Compose example only | Existing network reaching Silo PostgreSQL. Defaults to `silo_backend`. |
| `ARR_NETWORK` | No; Compose example only | Existing network shared with Sonarr/Radarr. Defaults to `arr`. |

Compose reads `.env` to substitute variables into this example; the worker does
not load `.env` itself. Network variables configure Compose, not the worker.
`-check-config` itself does not require credentials, although Compose still
evaluates required environment expressions before starting a container.

To use secret files instead of token/API key environment variables, mount them
read-only or use Compose secrets and point the corresponding JSON field to its
container path, such as `/run/secrets/sonarr_api_key`. Files must be readable by
UID/GID `10001:10001`; a configured file takes precedence over the environment.

### Compose and Command Options

| Option | Required? | Description |
| --- | --- | --- |
| `build.context` | For local builds | Project directory containing the Dockerfile and Go source. The example uses `.`. |
| `image` | Alternative to local builds | Your own prebuilt worker image, accessible to the Docker host. The example does not assume a published worker image. |
| `command` | No | Image defaults to `-config /config/config.json`, with writes disabled. Add flags below as needed. |
| Config mount | Yes with this example | Mount your existing JSON file to `/config/config.json:ro`. Create it before starting Docker. |
| State volume | Required for durable operation | Mount writable storage at `/data`. Retains collection mappings and acquisition deduplication across container recreation. |
| `networks` | As needed | Attach to networks that reach all configured services. External networks must already exist. |
| `restart` | No | Example uses `unless-stopped` to resume continuous operation after Docker/host restarts. |
| `ports` | No | Not needed; there is no listening HTTP service or UI. |
| `-config PATH` | No | Config location; binary default `config.json`, Docker image default `/config/config.json`. |
| `-apply` | No | Enables Silo collection writes and configured Arr add/search requests. Without it, remote operations are read-only. |
| `-once` | No | Runs one cycle and exits; otherwise repeats at the configured interval. |
| `-check-config` | No | Validates configuration without API/database access. Does not verify remote permissions, profiles or paths. |

The image runs as UID/GID `10001:10001`. The named volume in the example uses the
image's writable `/data` directory; if you substitute a host bind mount, grant that
user write access. Back up state and configuration together.

## Understanding Logs

Read worker logs with `docker compose logs --tail=100 -f collection-sync`.
Use the corresponding app's logs/activity view for Silo import or Arr download issues.

Logs are plain text, with per-list progress and a summary grouped by TV and Movies:

```text
12:00:01  [1/2] Trending TV | TV | key=trending-tv
12:00:04    Source: 50 items | Matched in Silo: 42 | Missing: 8
12:00:04    Collection: 5 added | 2 removed
12:00:04    Sonarr: 6 searches submitted | 2 previously requested

12:00:04  [2/2] Popular Movies | Movies | key=popular-movies
12:00:07    Source: 30 items | Matched in Silo: 25 | Missing: 5
12:00:07    Collection: 3 added | 0 removed
12:00:07    Radarr: 5 searches submitted | 0 previously requested

12:00:07  Sync finished | 7s | 2 lists OK | 0 lists with errors | 0 not processed

Type    List            Items  Matched  Added  Removed  Searched  Errors  Status
------  --------------  -----  -------  -----  -------  --------  ------  ------
TV      Trending TV        50       42      5        2         6       0  OK
Movies  Popular Movies     30       25      3        0         5       0  OK
------  --------------  -----  -------  -----  -------  --------  ------  ------
TOTAL                      80       67      8        2        11       0
```

`Items` counts source entries of the configured media type. `Matched` counts entries
with a currently playable Silo match, not temporarily retained collection members.
`Added` and `Removed` count confirmed Silo membership writes; `Searched` counts
accepted Arr requests, including search-on-add, but excludes previous submissions.
Errors retain partial success counts. Unavailable counts use `-`, and totals then
include known counts only. Totals count across bindings, not globally unique titles.

Dry-run uses `Would Add`, `Would Remove` and `Would Request` instead. Requests are
intentions, not verified searches: dry-run does not contact Arr or check previous
acquisition state. Deferred removals, disabled acquisition and per-entry errors
appear beneath the relevant list. Failed acquisition entries are identified by
media type and provider ID rather than a guessed title.

When a cycle has errors, an `Error summary` after the table repeats each failure
with its TV/Movies type, list title and binding key. This includes source, Silo and
acquisition failures, so you do not need to scroll back through the progress logs.
Cycles without errors omit this section.

Silo API requests (reads, writes and retries) are spaced at least 100 ms apart,
limiting this worker to 10 requests/second. Explicit HTTP 429 responses may be
retried twice using `Retry-After` (seconds or an HTTP date), with a maximum wait
of one minute per retry. Missing or invalid headers use a short backoff. Write
timeouts and 5xx responses are not retried because their outcome is uncertain.
Floppy and Arr write retry behavior is unchanged. Other users of the same Silo
API key can still consume its shared quota.

| Message / symptom | Meaning and action |
| --- | --- |
| `would request` | Dry-run only; nothing was added or searched. |
| `searches submitted` | Arr accepted the add/search requests. Check its Activity/History for progress; this does not mean files were downloaded. |
| `Missing` remains nonzero | No playable Silo match yet. Check Arr import, Silo scanning and the configured libraries. Previously requested titles are not searched again automatically. |
| `no exact acquisition match` | Arr could not return the exact external ID. Check the source identity and Arr metadata support; no title fallback is used. |
| Existing item is unmonitored | Enable the intended monitoring in Sonarr/Radarr; the worker preserves existing choices. |
| `acquisition outcome uncertain` | A write may have succeeded despite a timeout/failure. Further submissions for that item are blocked; inspect Arr before retrying. |
| `state is locked` | Another worker is using the same database. Stop it before running a one-off sync. |
| HTTP 401/403 or transport failure | Check credentials, permissions, Docker networks, service aliases, ports and reverse-proxy routing. |
| HTTP 429 | Silo's shared quota remained exhausted after bounded retries, or requested a wait over one minute. Check other clients sharing the key and the server's limits; remaining work resumes next cycle. |

For an uncertain acquisition, you can leave the block in place and search manually
in Arr. To permit a worker retry, stop the worker, back up SQLite, verify the remote
outcome, then remove only the `state` table row matching the `acquisition:` key in
the error. Never delete the entire database to reset one request. Collection changes
already applied are not rolled back when acquisition fails.