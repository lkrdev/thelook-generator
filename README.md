# thelook-generator

[![Open in Cloud Shell](https://gstatic.com/cloudssh/images/open-btn.svg)](https://ssh.cloud.google.com/cloudshell/editor?shellonly=true&cloudshell_git_repo=https://github.com/lkrdev/thelook-generator)

A Go CLI that analyzes and generates TheLook e-commerce data (`thelook.ecomm`) directly into BigQuery or PostgreSQL (including AlloyDB and Cloud SQL).

For BigQuery, it uses free NDJSON batch load jobs for historical backfills and the Storage Write API with native Change Data Capture (`_CHANGE_TYPE = 'UPSERT'`) for sub-second live streaming and order lifecycle transitions. For PostgreSQL and AlloyDB, it uses pipelined batch inserts and native `ON CONFLICT (id) DO UPDATE` upserts.

For a plain English explanation of what makes the generated data realistic, see [VARIABILITY.md](VARIABILITY.md).

## Required environment variables

By default, the CLI targets BigQuery:

- `GCP_PROJECT`: target GCP project ID (for example `looker-private-demo`).
- `GCP_DATASET`: target BigQuery dataset name (for example `ecomm`).
- `SECRET`: authentication key for the HTTP status server (`?key=<SECRET>`).

To target PostgreSQL or AlloyDB instead:

- `TARGET_DB=postgres`: sets PostgreSQL mode (also enabled if `DB_TARGET=postgres`, or if `DATABASE_URL` or `ALLOYDB_URL` is set).
- `DATABASE_URL`: PostgreSQL connection URL (e.g. `postgres://user:password@host:5432/thelook?sslmode=disable`). Standard `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, and `PGDATABASE` environment variables are also supported.
- `ALLOYDB_SCHEMA` (or `PGSCHEMA`): target schema name (defaults to `public`).
- `SECRET`: still required for the HTTP status server. `GCP_PROJECT` and `GCP_DATASET` are ignored in PostgreSQL mode.

## Database connections and authentication

### BigQuery

The generator authenticates with BigQuery through two standard Google Cloud mechanisms:

1. Live streaming, DDL execution, and batch load jobs (`thelook run`, `thelook deploy`, `thelook tables --apply`, `thelook backfill`, `thelook calendar`, and `thelook gaps --fill`) use the Go `cloud.google.com/go/bigquery` and `managedwriter` clients, resolving credentials via Application Default Credentials (ADC):
   - Reads `GOOGLE_APPLICATION_CREDENTIALS` if set.
   - On a local workstation, reads `~/.config/gcloud/application_default_credentials.json` (created by `gcloud auth application-default login`).
   - On GCE, Cloud Run, or GKE, fetches short-lived OAuth2 tokens directly from the metadata server using attached service accounts (no external `bq` CLI required inside containers).
2. Interactive profile analysis queries (`thelook analyze` and `thelook gaps --check-bq`) invoke the `bq` CLI (`bq query --format=json`), which uses your active `gcloud` login.

The principal running the generator needs two IAM roles on `$GCP_PROJECT`:

- `roles/bigquery.dataEditor` on the target dataset (or project) to create tables, load NDJSON files, and append/upsert rows.
- `roles/bigquery.jobUser` on `$GCP_PROJECT` to run load and query jobs.

For local development, log in with both commands:

```bash
gcloud auth login
gcloud auth application-default login
```

### PostgreSQL and AlloyDB

PostgreSQL mode uses `github.com/jackc/pgx/v5` with a connection pool. It connects over standard PostgreSQL protocol to AlloyDB (via private IP or AlloyDB Auth Proxy), Cloud SQL, AWS Aurora, or a local Postgres instance. No external CLI tools (`bq` or `gcloud`) are required in PostgreSQL mode.

## Generated tables and native CDC

Running `thelook deploy` (or `thelook tables --apply` for DDL only) creates the schema and all 14 tables matching the schema of `looker-private-demo.ecomm`, then loads the static `retail_calendar_454` table.

Running `thelook destroy` removes local state and profile files (`state.gob`, `commands.jsonl`, `profile.json`). Passing `--delete-tables` prompts you to type the target dataset name (`<project>.<dataset>` for BigQuery, or the schema name for PostgreSQL) to confirm before dropping all 14 tables.

### Append-only tables (11 tables)

Written once on row creation:

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

### Mutable CDC tables (3 tables)

Three tables evolve over time as orders move through their lifecycle:

- `inventory_items` (primary key: `id`): inserted with `sold_at: null` when stocked, then updated with `sold_at` populated when purchased.
- `order_items` (primary key: `id`): inserted with `status: "Processing"` at checkout, then updated as items move to `Shipped`, `Complete`, `Returned`, or `Cancelled`.
- `transaction_detail` (primary key: `order_id`): mirrors `order_items` lifecycle transitions with nested item data and customer geography. In BigQuery, items and user data are nested `STRUCT` and `ARRAY` types. In PostgreSQL, they are stored as native `JSONB`.

How upserts are handled by target:

- In BigQuery, tables are created with `PRIMARY KEY (...) NOT ENFORCED` and `OPTIONS (max_staleness = INTERVAL 15 MINUTE)`. Rows are streamed via the Storage Write API `DefaultStream` with `_CHANGE_TYPE = "UPSERT"`. BigQuery resolves deduplication natively on read, eliminating manual `MERGE` statements or derived table deduplication in Looker.
- In PostgreSQL and AlloyDB, updates use native `INSERT ... ON CONFLICT (id) DO UPDATE SET ...` (and `ON CONFLICT (order_id) DO UPDATE SET ...` for `transaction_detail`).

### Backfill and batch loading

During historical `thelook backfill` runs:

- In BigQuery mode, the generator deduplicates mutable tables and shared catalog IDs in memory by primary key, writes gzip-compressed `.ndjson.gz` files to temporary storage, and loads each table via free BigQuery API load jobs.
- In PostgreSQL mode, the generator pipes batches directly through `pgx.Batch` into PostgreSQL in pipelined transactions without writing intermediate files.
- Passing `--stdout` emits raw JSON lines to stdout for piping or inspection instead of loading to a database.

## Statistical profiles and live analysis

The binary embeds a baseline statistical profile in `seed_data.json` extracted from `looker-private-demo.ecomm` (the 10 distribution centers, 400 apparel brands, 200 US and UK geographic coordinates, 24 hourly order weights, and 36 department/category price and cost distributions).

In BigQuery mode, running `thelook analyze` (or letting `thelook run --analyze-interval 24h` refresh automatically in the background) queries BigQuery (`$GCP_PROJECT.$GCP_DATASET`) to recompute live distributions and write `profile.json`:

- Queries `order_items` grouped by `EXTRACT(HOUR FROM created_at)` to update the 24 hourly traffic and order volume weights (`hourly_weights`).
- Queries `products` grouped by `(department, category)` to update each category item count weight (`weight`), retail price mean (`avg_price`), standard deviation (`std_price`), minimum (`min_price`), maximum (`max_price`), and average cost-to-retail ratio (`cost_ratio`).
- Hot-reloads the updated `profile.json` into the running generator without restarting or losing state.
- Running `thelook gaps --check-bq` queries `MAX(created_at)` from `$GCP_PROJECT.$GCP_DATASET.events` in BigQuery to compare the latest ingested table timestamp against local `state.gob` coverage intervals.

## Generator behavior

### Organic user signups and email patterns

Users are never generated upfront during bootstrap. Instead, new users sign up organically during web browse sessions:

- First and last names are generated with `github.com/go-faker/faker/v4` (`faker.FirstNameMale()`, `faker.FirstNameFemale()`, and `faker.LastName()`).
- Every new user signup immediately emits both a `users` row and a corresponding `"Register"` (`/register`) web event in `events` with `user_id` populated.
- Email addresses use 10 username patterns across `@gmail.com`, `@yahoo.com`, `@hotmail.com`, `@outlook.com`, `@icloud.com`, and `@aol.com`: `<f_initial><last><num>`, `<first><l_initial><num>`, `<f_initial>.<last><num>`, `<first>.<l_initial><num>`, `<last><f_initial><num>`, `<l_initial><first><num>`, `<first><last>`, `<first>.<last>`, `<first>_<last>`, and `<first><last><num>`. Every first-letter combination includes a random number from `1` to `9999` skewed 80% toward `1` to `999`.

### Lookback funnel and referential integrity

- Tracks active primary keys, signed-up users, unsold inventory IDs, and per-user lookback funnels (`ViewedItems` to `CartedItems` to `Purchase`) in an atomic gzip-compressed `gob` state file (`state.gob`).
- Enforces that a `Product` page view (`/product/<id>`) always happens before a `Cart` (`/cart`) event for that item, and a `Cart` event always happens before a `Purchase` (`/purchase`) event across either the same session or a later return session.

### Brand and catalog growth

- Starts Day 1 of a 10-year load with `14,560` products (half of `29,120`), always onboarding `Levi's`, `Calvin Klein`, `Allegra K`, `Columbia`, `Carhartt`, and `Dockers` in the initial batch.
- Organically onboards ~2 new brands per week (always adding 2 to 3 new products and initial inventory per brand) and ~3 new products per day to reach `~29,120` products at Year 10.
- Once all 400 seed brands have been onboarded, synthesizes new apparel brand names from prefix and suffix combinations so brand growth never stalls.

### Command log and authenticated HTTP status server

- Every CLI invocation appends a debug entry (`timestamp` to the UTC second, `command`, `args`, `pid`) to `commands.jsonl` alongside `state.gob`.
- Whenever any CLI command runs, an HTTP status server starts on `--http :8080` (in the background for generator commands, or in the foreground via `thelook status`).
- Visiting `GET /` without `?key=` renders only a Key input box. Submitting the key updates `window.location` with `?key=<SECRET>` and reloads the page.
- `GET /api/status?key=<SECRET>` validates the query parameter against `SECRET` in constant time (`401 Unauthorized` if missing or invalid) and reads `state.gob` and `commands.jsonl` on demand to return exact UTC start timestamps (`generator_started_at`, `first_tick_at`, `backfill_started_at`, `backfill_completed_at`), entity counts, detected downtime gaps with copy-pasteable `thelook backfill` commands, recorded coverage intervals, and recent CLI executions.
- Downtime is never auto-backfilled on `thelook run` startup; it is recorded as an interval gap that can be inspected and filled with `thelook gaps` or viewed in the status UI.

## Automated Cloud Deployment (Cloud Shell / Console)

Open Google Cloud Console, launch Cloud Shell, and clone this repo:

```bash
git clone https://github.com/lkrdev/thelook-generator.git
cd thelook-generator
```

### Option A: Deploy to BigQuery (Two-Tier VM + Cloud Run Backfill)

Decouples continuous streaming from the bulk historical load so the Free Tier `e2-micro` VM (`thelook-bq-gen` with `--shielded-secure-boot`) immediately applies table DDL, loads `retail_calendar_454`, and starts the live 1-minute Storage Write API daemon (`thelook run --http :8080`), while a Cloud Run Job (`thelook-bq-backfill`, 4 vCPUs, 16 GiB RAM) runs the multi-year gzipped batch load and shuts down with zero idle cost:

```bash
# Step 1: Deploy BigQuery dataset, secrets, live e2-micro VM, and Cloud Run backfill job:
./deploy-bq.sh --mode production

# Or deploy and trigger the Cloud Run backfill job immediately:
./deploy-bq.sh --mode production --job

# Deploy only the dataset and live VM (skip Cloud Run job):
./deploy-bq.sh --vm-only

# Also register the `thelook_bq` BigQuery connection in Looker:
./deploy-bq.sh --mode production --looker

# Step 2: Trigger the historical backfill on Cloud Run (optionally bounded to exact VM first_tick_at):
gcloud run jobs execute thelook-bq-backfill --region=us-central1

gcloud run jobs execute thelook-bq-backfill --region=us-central1 \
  --args="backfill,--days=3650,--to=2026-10-21T20:25:00Z,--state=/tmp/state.gob"

# Or update and execute only the backfill job via the deploy wrapper:
./deploy-bq.sh --backfill-only --mode production --to 2026-10-21T20:25:00Z

# Step 3: View live status or tear down:
curl -s "http://<VM_IP>:8080/api/status?key=<SECRET>"
./deploy-bq.sh --destroy
```

### Option B: Deploy to AlloyDB (Two-Tier VM + Cloud Run Backfill)

Running a 10-year backfill (5.25 million simulated minutes) inside an `e2-micro` VM (1 GB RAM) thrashes swap and delays the systemd daemon. Instead, `./deploy-alloydb.sh` decouples continuous streaming from the bulk historical load:

- A Free Tier Compute Engine VM (`thelook-alloydb-gen`, `e2-micro` with Shielded VM secure boot) applies the schema, loads `retail_calendar_454`, and immediately runs the live 1-minute streaming daemon (`thelook run --http :8080`) in ~12 MB of RAM.
- A serverless Cloud Run Job (`thelook-backfill`, 4 vCPUs, 8 GiB RAM, Direct VPC egress to the AlloyDB private IP) handles the heavy multi-year historical load and shuts down with zero idle cost.
- Faults or retries in the bulk backfill never interrupt live event streaming or the HTTP status server.

```mermaid
flowchart LR
  subgraph VPC["VPC Network (default)"]
    VM["GCE e2-micro VM (thelook run :8080)"]
    JOB["Cloud Run Job (thelook-backfill, 4 vCPU / 8 GiB)"]
    DB[("AlloyDB Primary (ZONAL, Private IP)")]
    VM -->|"1-min CDC upserts"| DB
    JOB -->|"Direct VPC Egress batch upserts"| DB
  end
  SM["Secret Manager (DATABASE_URL, SECRET)"] -.-> VM
  SM -.-> JOB
  LOOKER["Looker (Public IP or PSC)"] --> DB
```

Step 1: Deploy AlloyDB, secrets, live VM generator, and the Cloud Run backfill job definition:

```bash
# Default deploy (1-vCPU ZONAL AlloyDB, e2-micro live VM, and Cloud Run backfill job):
./deploy-alloydb.sh --mode production

# Or deploy and trigger the Cloud Run backfill job immediately:
./deploy-alloydb.sh --mode production --job

# Deploy only the database and live VM (skip Cloud Run job):
./deploy-alloydb.sh --vm-only

# Also whitelist Looker egress IPs on AlloyDB and register `thelook_alloydb`:
./deploy-alloydb.sh --mode production --looker

# Looker Core Hybrid / Private Service Connect (PSC) deployments:
./deploy-alloydb.sh --mode production --looker --looker-host alloydb.thelook.internal
```

Step 2: Trigger the historical backfill on Cloud Run (bounded to the exact VM start timestamp):

When `thelook run` starts on a fresh VM, it records `generator_started_at` (to the UTC second) and `first_tick_at` (its first simulated minute) in `/var/lib/thelook/state.gob`, and offsets live primary keys by `1,000,000,000` while sharing a deterministic initial product catalog so live writes and historical backfill rows never collide.

```bash
# Run with default configured window:
gcloud run jobs execute thelook-backfill --region=us-central1

# Or bound --to using the exact first_tick_at reported by the VM status API:
gcloud run jobs execute thelook-backfill --region=us-central1 \
  --args="backfill,--days=3650,--to=2026-10-21T20:25:00Z,--state=/tmp/state.gob"

# Or update and execute only the backfill job via the deploy wrapper:
./deploy-alloydb.sh --backfill-only --mode production --to 2026-10-21T20:25:00Z
```

Every AlloyDB write uses `ON CONFLICT ... DO NOTHING` or `DO UPDATE` on mutable CDC tables (`inventory_items`, `order_items`, `transaction_detail`), making overlapping minute boundaries idempotent.

Step 3: View live status or tear down:

```bash
# SSH tunnel to the authenticated status dashboard:
gcloud compute ssh thelook-alloydb-gen --zone=us-central1-a -- -L 8080:localhost:8080

# Or query the JSON status API directly for exact start timestamps and counts:
curl -s "http://<VM_IP>:8080/api/status?key=<SECRET>"

# Inspect and heal any missed intervals on the VM:
thelook gaps --state /var/lib/thelook/state.gob --fill

# Tear down AlloyDB, VM, Cloud Run job, PSC, Looker connection, and secrets:
./deploy-alloydb.sh --destroy
```

#### Looker Core Hybrid / Private Service Connect (PSC) Deployments
In a Looker Core Hybrid deployment (`controlledEgressEnabled: true`), Looker Core routes data egress exclusively over Private Service Connect (PSC) rather than public IP.
To connect Looker Core Hybrid to AlloyDB:
1. Create a dedicated PSC NAT subnet and Internal TCP Proxy in the Looker PSC VPC (`looker-psc-demo` in `us-east1`).
2. Publish a PSC Service Attachment (e.g. `alloydb-svc-attachment`) forwarding TCP port `5432` to AlloyDB.
3. Attach the service attachment to Looker with a local FQDN (`alloydb.thelook.internal`):
   ```bash
   gcloud looker instances update <INSTANCE_NAME> --region=<REGION> \
     --psc-service-attachment=domain="alloydb.thelook.internal",attachment="projects/<PROJECT>/regions/<REGION>/serviceAttachments/alloydb-svc-attachment"
   ```
4. Set the Looker connection host to `alloydb.thelook.internal` (port 5432).

## Manual Quick start

### BigQuery

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

# Tear down local state (add --delete-tables to drop all 14 BigQuery tables after confirmation)
./thelook destroy
./thelook destroy --delete-tables
```

### PostgreSQL and AlloyDB

```bash
export TARGET_DB="postgres"
export DATABASE_URL="postgres://postgres:password@localhost:5432/thelook?sslmode=disable"
export ALLOYDB_SCHEMA="ecomm"
export SECRET="change-me"

go build -o thelook .

# Create the schema, all 14 PostgreSQL tables, and load retail_calendar_454
./thelook deploy

# Run initial 10-year backfill directly into PostgreSQL
./thelook backfill --days 3650 --state state.gob

# Run live 1-minute generator loop streaming to PostgreSQL
./thelook run --state state.gob --http :8080

# Inspect or heal downtime gaps
./thelook gaps --state state.gob --fill
./thelook status --state state.gob --http :8080

# Tear down local state (add --delete-tables to drop tables in the schema after confirmation)
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
  --shielded-secure-boot \
  --boot-disk-type=pd-standard \
  --boot-disk-size=30GB \
  --scopes=https://www.googleapis.com/auth/bigquery \
  --container-image=us-central1-docker.pkg.dev/lkr-dev-production/thelook-generator/thelook-generator:latest \
  --container-env=GCP_PROJECT=looker-private-demo,GCP_DATASET=ecomm,SECRET=change-me \
  --container-restart-policy=always \
  --container-mount-host-path=mount-path=/data,host-path=/var/lib/thelook,mode=rw
```

For PostgreSQL/AlloyDB, pass `TARGET_DB=postgres` and `DATABASE_URL` instead of `GCP_PROJECT` and `GCP_DATASET`.

### Native systemd service on Debian or Ubuntu

If running on a standard Debian or Ubuntu `e2-micro` VM, create `/etc/systemd/system/thelook.service`:

```ini
[Unit]
Description=TheLook Data Generator
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
