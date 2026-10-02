# thelook-generator

A Go CLI that analyzes and generates TheLook e-commerce data (`thelook.ecomm`) directly into BigQuery using free NDJSON batch load jobs for historical backfills and the BigQuery Storage Write API with native Change Data Capture (`_CHANGE_TYPE = 'UPSERT'`) for sub-second live streaming and order lifecycle transitions.

For a plain English explanation of what makes the generated data realistic, see [VARIABILITY.md](file:///usr/local/google/home/bryanweber/lkrdev/thelook-generator/VARIABILITY.md).

## Required environment variables

By default, the CLI targets BigQuery and requires:

- `GCP_PROJECT` sets the target GCP project ID (for example `looker-private-demo`).
- `GCP_DATASET` sets the target BigQuery dataset/schema name (for example `ecomm`).
- `SECRET` sets the authentication key required by the HTTP status server (`?key=<SECRET>`).

To target PostgreSQL or AlloyDB instead of BigQuery, set:

- `TARGET_DB=postgres` (or `DB_TARGET=postgres`, or set `DATABASE_URL` / `ALLOYDB_URL`).
- `DATABASE_URL` sets the PostgreSQL connection URL (e.g. `postgres://user:password@host:5432/dbname?sslmode=disable`). Standard `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, and `PGDATABASE` environment variables are also recognized.
- `ALLOYDB_SCHEMA` (or `PGSCHEMA`) sets the target schema name (defaults to `public`).
- `SECRET` is still required for the HTTP status server. `GCP_PROJECT` and `GCP_DATASET` are ignored in PostgreSQL mode.

## BigQuery authentication and IAM permissions

The generator authenticates with BigQuery through two standard Google Cloud mechanisms depending on the subcommand:

1. Live streaming (`thelook run` and `thelook gaps --fill`) uses the Go `cloud.google.com/go/bigquery/storage/managedwriter` client, which resolves credentials automatically via Google Application Default Credentials (ADC) in this order:
   - If `GOOGLE_APPLICATION_CREDENTIALS` is set in the environment, it reads the service account JSON key at that file path.
   - On a local workstation without `GOOGLE_APPLICATION_CREDENTIALS`, it reads your local ADC file (`~/.config/gcloud/application_default_credentials.json`) created by `gcloud auth application-default login`.
   - On GCE, Cloud Run, or GKE, it fetches short-lived OAuth2 tokens directly from the local instance metadata server using the attached VM service account and `--scopes=https://www.googleapis.com/auth/bigquery` with no key files required.
2. One-shot DDL, batch loads, and SQL queries (`thelook tables --apply`, `thelook backfill`, `thelook calendar`, `thelook analyze`, and `thelook gaps --check-bq`) invoke the `bq` CLI (`bq query` and `bq load`), which uses your active `gcloud` CLI credentials (`gcloud auth login` locally or the GCE VM service account on a VM).

The principal running the generator needs two IAM roles on `$GCP_PROJECT`:

- `roles/bigquery.dataEditor` on the target dataset (or project) so it can create tables, load NDJSON files, and append/upsert rows via the Storage Write API.
- `roles/bigquery.jobUser` on `$GCP_PROJECT` so it can submit `bq load` and `bq query` jobs.

For local development, log in once with both commands before running the CLI:

```bash
gcloud auth login
gcloud auth application-default login
```

## Generated tables and native BigQuery CDC

Running `thelook deploy` (or `thelook tables --apply` for DDL only) creates the dataset and all 14 tables in `$GCP_PROJECT.$GCP_DATASET` matching the exact table and column names of `looker-private-demo.ecomm` with no wrapper columns or `_stream` suffixes, and loads the static `retail_calendar_454` table.

Running `thelook destroy` removes local state and profile files (`state.gob`, `commands.jsonl`, `profile.json`). Passing `--delete-tables` (`thelook destroy --delete-tables`) prompts you to type `$GCP_PROJECT.$GCP_DATASET` to confirm before dropping all 14 tables in BigQuery.

The 11 append-only tables are written once:

- `distribution_centers`
- `products`
- `users`
- `events`
- `campaigns`
- `ad_groups`
- `keywords`
- `ad_events`
- `discounts`
- `combined_orders_users`
- `retail_calendar_454`

The 3 mutable tables are created with `PRIMARY KEY (...) NOT ENFORCED` and `OPTIONS (max_staleness = INTERVAL 15 MINUTE)` so BigQuery handles upserts natively when rows are streamed over the Storage Write API `DefaultStream` with `_CHANGE_TYPE = "UPSERT"`:

- `inventory_items` (`PRIMARY KEY (id) NOT ENFORCED`) is streamed with `sold_at: null` when stocked, then upserted by `id` with `sold_at` populated when purchased.
- `order_items` (`PRIMARY KEY (id) NOT ENFORCED`) is streamed with `status: "Processing"` at checkout, then upserted by `id` as it transitions to `Shipped`, `Complete`, `Returned`, or `Cancelled`.
- `transaction_detail` (`PRIMARY KEY (order_id) NOT ENFORCED`) mirrors `order_items` lifecycle transitions with nested order item structs and user geography.

Because BigQuery resolves primary key upserts natively on read and compacts them in the background within `max_staleness`, Looker Explores query `inventory_items`, `order_items`, and `transaction_detail` directly without any `MERGE` jobs, `ROW_NUMBER()` window scans, or `UNION ALL` derived tables.

During historical `thelook backfill` runs, the generator deduplicates `inventory_items`, `order_items`, and `transaction_detail` by primary key in memory before writing temporary `.ndjson` files and loading each table in a single free `bq load --source_format=NEWLINE_DELIMITED_JSON` job. Any command also accepts `--stdout` to emit raw JSON lines to `stdout` instead of writing to BigQuery.

## What the CLI analyzes in BigQuery

The binary embeds a baseline statistical profile in `seed_data.json` extracted from `looker-private-demo.ecomm` (the 10 distribution centers, 400 real apparel brands, 200 US and UK city/state/zip/lat/lon coordinates, 24 hourly order weights, and 36 department/category price and cost distributions).

Running `thelook analyze` (or letting `thelook run --analyze-interval 24h` refresh automatically in the background) queries BigQuery (`$GCP_PROJECT.$GCP_DATASET`) to recompute live distributions and write `profile.json`:

- Queries `order_items` grouped by `EXTRACT(HOUR FROM created_at)` to update the 24 hourly traffic and order volume weights (`hourly_weights`).
- Queries `products` grouped by `(department, category)` to update each category's item count weight (`weight`), retail price mean (`avg_price`), standard deviation (`std_price`), minimum (`min_price`), maximum (`max_price`), and average cost-to-retail ratio (`cost_ratio`).
- Hot-reloads the updated `profile.json` into the running generator without restarting or losing state.
- Running `thelook gaps --check-bq` also queries `MAX(created_at)` from `$GCP_PROJECT.$GCP_DATASET.events` in BigQuery so you can compare the latest ingested table timestamp against the local `state.gob` coverage intervals.

## Generator behavior

### Organic user signups and email patterns

Users are never generated upfront during bootstrap. Instead, new users sign up organically during web browse sessions:

- First and last names are generated with `github.com/go-faker/faker/v4` (`faker.FirstNameMale()`, `faker.FirstNameFemale()`, and `faker.LastName()`).
- Every new user signup immediately emits both a `users` row and a corresponding `"Register"` (`/register`) web event in `events` with `user_id` populated.
- Email addresses use 10 username patterns across `@example.com`, `@gmail.com`, `@yahoo.com`, `@hotmail.com`, `@outlook.com`, and `@icloud.com`: `<f_initial><last><num>`, `<first><l_initial><num>`, `<f_initial>.<last><num>`, `<first>.<l_initial><num>`, `<last><f_initial><num>`, `<l_initial><first><num>`, `<first><last>`, `<first>.<last>`, `<first>_<last>`, and `<first><last><num>`. Every first-letter combination includes a random number from `1` to `9999` skewed 80% toward `1` to `999`.

### Lookback funnel and referential integrity

- Tracks active primary keys, signed-up users, unsold inventory IDs, and per-user lookback funnels (`ViewedItems` to `CartedItems` to `Purchase`) in an atomic gzip-compressed `gob` state file (`state.gob`).
- Enforces that a `Product` page view (`/product/<id>`) always happens before a `Cart` (`/cart`) event for that item, and a `Cart` event always happens before a `Purchase` (`/purchase`) event across either the same session or a later return session.

### Brand and catalog growth

- Starts Day 1 of a 10-year load with `14,560` products (half of `29,120`), always onboarding `Levi's`, `Calvin Klein`, `Allegra K`, `Columbia`, `Carhartt`, and `Dockers` in the initial batch.
- Organically onboards ~2 new brands per week (always adding 2 to 3 new products and initial inventory per brand) and ~3 new products per day to reach `~29,120` products at Year 10.
- Once all 400 seed brands have been onboarded, synthesizes new apparel brand names from prefix and suffix combinations so brand growth never stalls.

### Command log and authenticated HTTP status server

- Every CLI invocation appends a debug entry (`timestamp`, `command`, `args`, `pid`) to `commands.jsonl` alongside `state.gob`.
- Whenever any CLI command runs, an HTTP status server starts on `--http :8080` (in the background for generator commands, or in the foreground via `thelook status`).
- Visiting `GET /` without `?key=` renders only a Key input box. Submitting the key updates `window.location` with `?key=<SECRET>` and reloads the page.
- `GET /api/status?key=<SECRET>` validates the query parameter against `SECRET` in constant time (`401 Unauthorized` if missing or invalid) and reads and parses `state.gob` and `commands.jsonl` on demand to return entity counts, detected downtime gaps with copy-pasteable `thelook backfill` commands, recorded coverage intervals, and recent CLI executions.
- Never auto-backfills on `thelook run` startup; downtime is recorded as an interval gap that can be inspected and filled with `thelook gaps` or viewed in the status UI.

## Quick start

```bash
export GCP_PROJECT="looker-private-demo"
export GCP_DATASET="ecomm"
export SECRET="change-me"

go build -o thelook .

# Create the dataset, all 14 BigQuery tables, and load retail_calendar_454
./thelook deploy

# Refresh statistical profile from BigQuery ($GCP_PROJECT.$GCP_DATASET)
./thelook analyze --out profile.json

# Run initial 10-year backfill and load into BigQuery via bq load
./thelook backfill --days 3650 --state state.gob --profile profile.json

# Run live 1-minute generator loop streaming via BigQuery Storage Write API (status server on :8080)
./thelook run --state state.gob --profile profile.json --http :8080

# Inspect or heal downtime gaps via Storage Write API, or run standalone status server
./thelook gaps --state state.gob --check-bq
./thelook gaps --state state.gob --fill
./thelook status --state state.gob --http :8080

# Tear down local state (add --delete-tables to drop all 14 BigQuery tables after interactive confirmation)
./thelook destroy
./thelook destroy --delete-tables
```

## Running on GCE free tier with auto-healing

GCP Free Tier includes one `e2-micro` instance per month in `us-central1`, `us-west1`, or `us-east1` with a 30 GB Standard Persistent Disk, and the BigQuery Storage Write API includes 2 TB per month of free streaming ingestion.

### Provisioning with Container-Optimized OS

```bash
gcloud compute instances create-with-container thelook-gen \
  --zone=us-central1-a \
  --machine-type=e2-micro \
  --boot-disk-type=pd-standard \
  --boot-disk-size=30GB \
  --scopes=https://www.googleapis.com/auth/bigquery \
  --container-image=gcr.io/YOUR_PROJECT_ID/thelook-generator:latest \
  --container-env=GCP_PROJECT=looker-private-demo,GCP_DATASET=ecomm,SECRET=change-me \
  --container-restart-policy=always \
  --container-mount-host-path=mount-path=/data,host-path=/var/lib/thelook,mode=rw
```

### Native systemd service on Debian or Ubuntu

If running on a standard Debian or Ubuntu `e2-micro` VM, create `/etc/systemd/system/thelook.service`:

```ini
[Unit]
Description=TheLook BigQuery Data Generator
After=docker.service network-online.target
Requires=docker.service
StartLimitIntervalSec=0

[Service]
Type=simple
Restart=always
RestartSec=5
ExecStartPre=-/usr/bin/docker rm -f thelook
ExecStartPre=/bin/mkdir -p /var/lib/thelook
ExecStart=/usr/bin/docker run --name thelook --rm \
  --memory=512m \
  -e GCP_PROJECT=looker-private-demo \
  -e GCP_DATASET=ecomm \
  -e SECRET=change-me \
  -p 8080:8080 \
  -v /var/lib/thelook:/data \
  thelook-generator:latest run --state /data/state.gob --profile /data/profile.json --http :8080
ExecStop=/usr/bin/docker stop thelook

[Install]
WantedBy=multi-user.target
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now thelook.service
```

## TODO

- [ ] `kmeans_training` (BQML K-Means feature table) and dependent `fan_experience_promo_email` centroid segmentation table

