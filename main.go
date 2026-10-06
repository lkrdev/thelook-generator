package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"thelook-generator/internal/bq"
	"thelook-generator/internal/model"
	"thelook-generator/internal/postgres"
	"thelook-generator/internal/server"
	"thelook-generator/internal/sim"
)

//go:embed seed_data.json
var defaultSeedJSON []byte

type JSONEmitter struct {
	w         *bufio.Writer
	enc       *json.Encoder
	rateLimit int
	windowCnt int
	windowSt  time.Time
}

func NewJSONEmitter(out io.Writer, rateLimit int) *JSONEmitter {
	bw := bufio.NewWriterSize(out, 256*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &JSONEmitter{
		w:         bw,
		enc:       enc,
		rateLimit: rateLimit,
		windowSt:  time.Now(),
	}
}

func (e *JSONEmitter) Emit(row any) {
	if e.rateLimit > 0 {
		e.windowCnt++
		if e.windowCnt >= e.rateLimit {
			elapsed := time.Since(e.windowSt)
			if elapsed < time.Second {
				e.w.Flush()
				time.Sleep(time.Second - elapsed)
			}
			e.windowCnt = 0
			e.windowSt = time.Now()
		}
	}
	_ = e.enc.Encode(row)
}

func (e *JSONEmitter) Flush() error { return e.w.Flush() }
func (e *JSONEmitter) Close() error { return e.Flush() }

func parseTimeArg(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Truncate(time.Minute), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or YYYY-MM-DD)", s)
}

func isPostgres() bool {
	target := strings.ToLower(os.Getenv("TARGET_DB"))
	if target == "" {
		target = strings.ToLower(os.Getenv("DB_TARGET"))
	}
	return target == "postgres" || target == "alloydb" || os.Getenv("DATABASE_URL") != "" || os.Getenv("ALLOYDB_URL") != ""
}

func postgresSchema(override string) string {
	for _, s := range []string{override, os.Getenv("ALLOYDB_SCHEMA"), os.Getenv("PGSCHEMA"), os.Getenv("GCP_DATASET")} {
		if s != "" {
			return s
		}
	}
	return "public"
}

func requireEnv() error {
	if strings.TrimSpace(os.Getenv("SECRET")) == "" {
		return fmt.Errorf("SECRET environment variable is required")
	}
	if isPostgres() {
		return nil
	}
	if strings.TrimSpace(os.Getenv("GCP_PROJECT")) == "" || strings.TrimSpace(os.Getenv("GCP_DATASET")) == "" {
		return fmt.Errorf("GCP_PROJECT and GCP_DATASET environment variables are required (or set TARGET_DB=postgres)")
	}
	return nil
}

func defaultDataset() string {
	return os.Getenv("GCP_PROJECT") + "." + os.Getenv("GCP_DATASET")
}

type CLI struct {
	Deploy   DeployCmd   `cmd:"" help:"Deploy tables and/or cloud infrastructure (BigQuery or AlloyDB)."`
	Destroy  DestroyCmd  `cmd:"" help:"Tear down cloud infrastructure, Looker connections, service account roles, and state."`
	Run      RunCmd      `cmd:"" help:"Run live minute-by-minute event and CDC streaming daemon."`
	Backfill BackfillCmd `cmd:"" help:"Backfill historical simulation data across a date range or N days."`
	Gaps     GapsCmd     `cmd:"" help:"Inspect and optionally fill time gaps in recorded simulation intervals."`
	Analyze  AnalyzeCmd  `cmd:"" help:"Analyze an existing BigQuery dataset to generate a seed profile."`
	Calendar CalendarCmd `cmd:"" help:"Generate NRF 4-5-4 retail calendar rows."`
	Tables   TablesCmd   `cmd:"" help:"Print or apply BigQuery / PostgreSQL DDL."`
	Status   StatusCmd   `cmd:"" help:"Serve the authenticated HTTP status UI."`
}

type StatusCmd struct {
	State string `default:"state.gob" help:"Path to on-disk state."`
	HTTP  string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *StatusCmd) Run() error {
	if err := requireEnv(); err != nil {
		return err
	}
	server.RecordCommandRun(c.State, "status", os.Args[2:])
	fmt.Fprintf(os.Stderr, "Serving status on http://%s\n", c.HTTP)
	return http.ListenAndServe(c.HTTP, server.NewStatusMux(c.State))
}

type CalendarCmd struct {
	FromFY int    `name:"from-fy" default:"2016" help:"Start fiscal year (inclusive)."`
	ToFY   int    `name:"to-fy" default:"2036" help:"End fiscal year (inclusive)."`
	Stdout bool   `help:"Emit JSON lines to stdout instead of loading into database."`
	Rate   int    `default:"0" help:"Max JSON lines/sec when --stdout is set (0 = unlimited)."`
	State  string `default:"state.gob" help:"Path to on-disk state (for command log)."`
	HTTP   string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *CalendarCmd) Run() error {
	if !c.Stdout {
		if err := requireEnv(); err != nil {
			return err
		}
	}
	if c.FromFY > c.ToFY {
		return fmt.Errorf("--from-fy (%d) must be <= --to-fy (%d)", c.FromFY, c.ToFY)
	}
	server.RecordCommandRun(c.State, "calendar", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	if c.Stdout {
		emitter := NewJSONEmitter(os.Stdout, c.Rate)
		sim.EmitRetailCalendar454(c.FromFY, c.ToFY, emitter.Emit)
		return emitter.Flush()
	}
	if isPostgres() {
		sink, err := postgres.NewSink(context.Background(), "", postgresSchema(""))
		if err != nil {
			return err
		}
		defer sink.Close()
		sim.EmitRetailCalendar454(c.FromFY, c.ToFY, sink.Emit)
		return sink.Flush()
	}
	sink, err := bq.NewBatchLoadSink(os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
	if err != nil {
		return err
	}
	sim.EmitRetailCalendar454(c.FromFY, c.ToFY, sink.Emit)
	return sink.FlushAndLoad()
}

type RunCmd struct {
	State           string        `default:"state.gob" help:"Path to on-disk FK & interval lookup state."`
	Profile         string        `default:"profile.json" help:"Path to analyzed statistical profile JSON."`
	AnalyzeInterval time.Duration `name:"analyze-interval" default:"24h" help:"Interval to re-analyze BigQuery dataset patterns (0 to disable)."`
	Dataset         string        `help:"BigQuery project.schema to analyze."`
	InitialProducts int           `name:"initial-products" default:"14560" help:"Initial product catalog size on bootstrap."`
	Stdout          bool          `help:"Emit JSON lines to stdout instead of streaming to database."`
	Rate            int           `default:"0" help:"Max JSON lines/sec when --stdout is set (0 = unlimited)."`
	HTTP            string        `default:":8080" help:"HTTP listen address for status server."`
}

func (c *RunCmd) Run() error {
	if !c.Stdout {
		if err := requireEnv(); err != nil {
			return err
		}
	}
	if c.Dataset == "" {
		c.Dataset = defaultDataset()
	}
	server.RecordCommandRun(c.State, "run", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	st, err := model.LoadState(c.State)
	if err != nil {
		return err
	}
	eng, err := sim.NewEngine(st, defaultSeedJSON, c.Profile)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitFn func(any)
	var flushFn func() error
	if c.Stdout {
		em := NewJSONEmitter(os.Stdout, c.Rate)
		emitFn = em.Emit
		flushFn = em.Flush
	} else if isPostgres() {
		sink, err := postgres.NewSink(ctx, "", postgresSchema(""))
		if err != nil {
			return err
		}
		defer sink.Close()
		emitFn = sink.Emit
		flushFn = sink.Flush
	} else {
		sink, err := bq.NewStorageWriteSink(ctx, os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
		if err != nil {
			return err
		}
		defer sink.Close()
		emitFn = sink.Emit
		flushFn = sink.Flush
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var analyzeTicker *time.Ticker
	var analyzeCh <-chan time.Time
	if c.AnalyzeInterval > 0 {
		analyzeTicker = time.NewTicker(c.AnalyzeInterval)
		defer analyzeTicker.Stop()
		analyzeCh = analyzeTicker.C
	}

	runOnce := func(t time.Time) {
		eng.Tick(t.UTC().Truncate(time.Minute), c.InitialProducts, emitFn)
		if err := flushFn(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: flush failed: %v\n", err)
		}
		if err := st.Save(c.State); err != nil {
			fmt.Fprintf(os.Stderr, "warn: state save failed: %v\n", err)
		}
	}

	runOnce(time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			_ = flushFn()
			return st.Save(c.State)
		case t := <-ticker.C:
			runOnce(t)
		case <-analyzeCh:
			if err := bq.AnalyzeDataset(c.Dataset, defaultSeedJSON, c.Profile); err == nil {
				if updated, err := sim.NewEngine(st, defaultSeedJSON, c.Profile); err == nil {
					eng.Seed = updated.Seed
				}
			}
		}
	}
}

type BackfillCmd struct {
	From            string `help:"Start timestamp (RFC3339 or YYYY-MM-DD)."`
	To              string `help:"End timestamp (RFC3339 or YYYY-MM-DD, defaults to now)."`
	Days            int    `default:"0" help:"Backfill N days ending at --to."`
	Stdout          bool   `help:"Emit JSON lines to stdout instead of loading into database."`
	Rate            int    `default:"0" help:"Max JSON lines/sec when --stdout is set (0 = unlimited)."`
	State           string `default:"state.gob" help:"Path to on-disk FK & interval state."`
	Profile         string `default:"profile.json" help:"Path to analyzed statistical profile JSON."`
	InitialProducts int    `name:"initial-products" default:"14560" help:"Initial product catalog size on bootstrap."`
	Force           bool   `help:"Re-generate even if minute is already recorded in state."`
	HTTP            string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *BackfillCmd) Run() error {
	if !c.Stdout {
		if err := requireEnv(); err != nil {
			return err
		}
	}
	server.RecordCommandRun(c.State, "backfill", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	toTime := time.Now().UTC().Truncate(time.Minute)
	if c.To != "" {
		var err error
		toTime, err = parseTimeArg(c.To)
		if err != nil {
			return err
		}
	}
	var fromTime time.Time
	if c.From != "" {
		var err error
		fromTime, err = parseTimeArg(c.From)
		if err != nil {
			return err
		}
	} else if c.Days > 0 {
		fromTime = toTime.Add(-time.Duration(c.Days) * 24 * time.Hour)
	} else {
		return fmt.Errorf("specify --from <timestamp> or --days <N>")
	}
	if fromTime.After(toTime) {
		return fmt.Errorf("--from (%s) is after --to (%s)", model.FmtTS(fromTime), model.FmtTS(toTime))
	}

	st, err := model.LoadState(c.State)
	if err != nil {
		return err
	}
	eng, err := sim.NewEngine(st, defaultSeedJSON, c.Profile)
	if err != nil {
		return err
	}

	totalMinutes := int(toTime.Sub(fromTime).Minutes()) + 1
	totalDays := float64(totalMinutes) / 1440.0
	targetDesc := "BigQuery (" + os.Getenv("GCP_PROJECT") + "." + os.Getenv("GCP_DATASET") + ")"
	if c.Stdout {
		targetDesc = "stdout"
	} else if isPostgres() {
		targetDesc = fmt.Sprintf("PostgreSQL/AlloyDB (%s)", postgresSchema(""))
	}

	fmt.Fprintf(os.Stderr, "==> [backfill] Starting backfill: %s -> %s\n", model.FmtTS(fromTime), model.FmtTS(toTime))
	fmt.Fprintf(os.Stderr, "    Span: %.1f days (%d minutes) | Target: %s | State: %s\n", totalDays, totalMinutes, targetDesc, c.State)
	startTime := time.Now()

	reportInterval := 1440
	if totalDays > 180 {
		reportInterval = 43200
	} else if totalDays > 14 {
		reportInterval = 10080
	}

	printProgress := func(cur time.Time, minuteIdx, steps, skipped int) {
		pct := float64(minuteIdx) / float64(totalMinutes) * 100
		elapsed := time.Since(startTime)
		rate := float64(minuteIdx) / elapsed.Seconds()
		etaStr := ""
		if rate > 0 {
			remSec := float64(totalMinutes-minuteIdx) / rate
			etaStr = fmt.Sprintf(" | ETA: %s", (time.Duration(remSec) * time.Second).Round(time.Second))
		}
		fmt.Fprintf(os.Stderr, "--> [backfill] %s | %5.1f%% (%d/%d min) | Orders: %d, Events: %d, Users: %d, Products: %d (%.0f min/s%s)\n",
			model.FmtTS(cur), pct, minuteIdx, totalMinutes, max(0, st.NextOrderID-1), max(0, st.NextEventID-1), max(0, st.NextUserID-1), max(0, st.NextProductID-1), rate, etaStr)
	}

	var emitFn func(any)
	var flushBatch func(cur time.Time) error
	var finish func(steps, skipped int) error
	batchInterval := 14400

	if c.Stdout {
		emitter := NewJSONEmitter(os.Stdout, c.Rate)
		emitFn = emitter.Emit
		batchInterval = 1440
		flushBatch = func(time.Time) error {
			if err := emitter.Flush(); err != nil {
				return err
			}
			return st.Save(c.State)
		}
		finish = func(steps, skipped int) error {
			_ = emitter.Flush()
			fmt.Fprintf(os.Stderr, "==> [backfill] Completed in %s! Generated %d steps (%d skipped).\n",
				time.Since(startTime).Round(time.Second), steps, skipped)
			return nil
		}
	} else if isPostgres() {
		sink, err := postgres.NewSink(context.Background(), "", postgresSchema(""))
		if err != nil {
			return err
		}
		defer sink.Close()
		emitFn = sink.Emit
		flushBatch = func(cur time.Time) error {
			fmt.Fprintf(os.Stderr, "    [backfill] Flushing batch to %s at %s...\n", targetDesc, model.FmtTS(cur))
			if err := sink.Flush(); err != nil {
				return err
			}
			return st.Save(c.State)
		}
		finish = func(steps, skipped int) error {
			fmt.Fprintf(os.Stderr, "    [backfill] Final batch flush to %s...\n", targetDesc)
			if err := sink.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "==> [backfill] Completed in %s! Generated %d steps (%d skipped) into %s. Final state: %d products, %d users, %d orders, %d events.\n",
				time.Since(startTime).Round(time.Second), steps, skipped, targetDesc, max(0, st.NextProductID-1), max(0, st.NextUserID-1), max(0, st.NextOrderID-1), max(0, st.NextEventID-1))
			return nil
		}
	} else {
		sink, err := bq.NewBatchLoadSink(os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
		if err != nil {
			return err
		}
		emitFn = sink.Emit
		flushBatch = func(time.Time) error { return nil }
		finish = func(steps, skipped int) error {
			fmt.Fprintf(os.Stderr, "==> [backfill] Simulation finished in %s (%d minutes simulated, %d skipped). Loading tables into BigQuery via bq load...\n",
				time.Since(startTime).Round(time.Second), steps, skipped)
			if err := sink.FlushAndLoad(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "==> [backfill] Completed in %s! All tables loaded into %s. Final state: %d products, %d users, %d orders, %d events.\n",
				time.Since(startTime).Round(time.Second), targetDesc, max(0, st.NextProductID-1), max(0, st.NextUserID-1), max(0, st.NextOrderID-1), max(0, st.NextEventID-1))
			return nil
		}
	}

	steps, skipped, minuteIdx := 0, 0, 0
	for cur := fromTime; !cur.After(toTime); cur = cur.Add(time.Minute) {
		minuteIdx++
		if !c.Force && st.IsMinuteCovered(cur) {
			skipped++
			if minuteIdx%reportInterval == 0 || cur.Equal(toTime) {
				printProgress(cur, minuteIdx, steps, skipped)
			}
			continue
		}
		eng.Tick(cur, c.InitialProducts, emitFn)
		steps++
		if steps%batchInterval == 0 {
			if err := flushBatch(cur); err != nil {
				return err
			}
		}
		if minuteIdx%reportInterval == 0 || cur.Equal(toTime) {
			printProgress(cur, minuteIdx, steps, skipped)
		}
	}

	if err := finish(steps, skipped); err != nil {
		return err
	}
	return st.Save(c.State)
}

type GapsCmd struct {
	State           string `default:"state.gob" help:"Path to on-disk state."`
	Profile         string `default:"profile.json" help:"Path to statistical profile JSON."`
	Fill            bool   `help:"Immediately backfill all detected gaps."`
	Stdout          bool   `help:"Emit JSON lines to stdout when --fill is set."`
	Rate            int    `default:"0" help:"Max JSON lines/sec when --fill and --stdout are set (0 = unlimited)."`
	CheckBQ         bool   `name:"check-bq" help:"Also query BigQuery dataset for latest timestamp gaps."`
	Dataset         string `help:"BigQuery project.schema for --check-bq."`
	InitialProducts int    `name:"initial-products" default:"14560" help:"Initial products when filling."`
	HTTP            string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *GapsCmd) Run() error {
	if err := requireEnv(); err != nil {
		return err
	}
	if c.Dataset == "" {
		c.Dataset = defaultDataset()
	}
	server.RecordCommandRun(c.State, "gaps", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	st, err := model.LoadState(c.State)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Truncate(time.Minute)
	gaps := server.FindGaps(st, now)

	if c.CheckBQ {
		q := fmt.Sprintf("SELECT max(string(created_at)) AS max_ts FROM `%s.events`", c.Dataset)
		if out, err := bq.RunQuery("", q, true, 0); err == nil {
			var rows []map[string]string
			if json.Unmarshal(out, &rows) == nil && len(rows) > 0 && rows[0]["max_ts"] != "" {
				fmt.Fprintf(os.Stderr, "BigQuery %s.events latest timestamp: %s\n", c.Dataset, rows[0]["max_ts"])
			}
		}
	}

	if len(st.Intervals) == 0 {
		fmt.Fprintf(os.Stderr, "No intervals recorded yet in %s. Run initial 10-year backfill:\n  thelook backfill --days 3650 --state %s\n", c.State, c.State)
		return nil
	}
	if len(gaps) == 0 {
		fmt.Fprintf(os.Stderr, "No gaps detected across %d recorded interval(s).\n", len(st.Intervals))
		return nil
	}

	for i, g := range gaps {
		mins := int(g.To.Sub(g.From).Minutes()) + 1
		fmt.Fprintf(os.Stderr, "Gap #%d (%d mins): %s -> %s\n  Command: thelook backfill --from %s --to %s --state %s\n",
			i+1, mins, model.FmtTS(g.From), model.FmtTS(g.To), model.FmtTS(g.From), model.FmtTS(g.To), c.State)
	}

	if c.Fill {
		eng, err := sim.NewEngine(st, defaultSeedJSON, c.Profile)
		if err != nil {
			return err
		}
		if c.Stdout {
			emitter := NewJSONEmitter(os.Stdout, c.Rate)
			for _, g := range gaps {
				for cur := g.From; !cur.After(g.To); cur = cur.Add(time.Minute) {
					eng.Tick(cur, c.InitialProducts, emitter.Emit)
				}
			}
			_ = emitter.Flush()
			return st.Save(c.State)
		}
		ctx := context.Background()
		if isPostgres() {
			sink, err := postgres.NewSink(ctx, "", postgresSchema(""))
			if err != nil {
				return err
			}
			defer sink.Close()
			for _, g := range gaps {
				for cur := g.From; !cur.After(g.To); cur = cur.Add(time.Minute) {
					eng.Tick(cur, c.InitialProducts, sink.Emit)
				}
				if err := sink.Flush(); err != nil {
					return err
				}
			}
			return st.Save(c.State)
		}
		sink, err := bq.NewStorageWriteSink(ctx, os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
		if err != nil {
			return err
		}
		defer sink.Close()
		for _, g := range gaps {
			for cur := g.From; !cur.After(g.To); cur = cur.Add(time.Minute) {
				eng.Tick(cur, c.InitialProducts, sink.Emit)
			}
			if err := sink.Flush(); err != nil {
				return err
			}
		}
		return st.Save(c.State)
	}
	return nil
}

type AnalyzeCmd struct {
	Dataset string `help:"BigQuery project.schema to analyze."`
	Out     string `default:"profile.json" help:"Output profile JSON path."`
	State   string `default:"state.gob" help:"Path to on-disk state (for command log)."`
	HTTP    string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *AnalyzeCmd) Run() error {
	if err := requireEnv(); err != nil {
		return err
	}
	if c.Dataset == "" {
		c.Dataset = defaultDataset()
	}
	server.RecordCommandRun(c.State, "analyze", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	return bq.AnalyzeDataset(c.Dataset, defaultSeedJSON, c.Out)
}

type TablesCmd struct {
	Project string `env:"GCP_PROJECT" help:"GCP Project ID."`
	Dataset string `env:"GCP_DATASET" help:"Target BigQuery dataset/schema name."`
	Apply   bool   `help:"Execute DDL directly instead of only printing."`
	State   string `default:"state.gob" help:"Path to on-disk state (for command log)."`
	HTTP    string `default:":8080" help:"HTTP listen address for status server."`
}

func (c *TablesCmd) Run() error {
	server.RecordCommandRun(c.State, "tables", os.Args[2:])
	server.StartBackgroundServer(c.HTTP, c.State)
	if isPostgres() {
		sch := postgresSchema(c.Dataset)
		ddl := postgres.TableDDL(sch)
		fmt.Println(ddl)
		if c.Apply {
			sink, err := postgres.NewSink(context.Background(), "", sch)
			if err != nil {
				return err
			}
			defer sink.Close()
			return sink.ApplyDDL(context.Background())
		}
		return nil
	}
	ddl := bq.TableDDL(c.Project, c.Dataset)
	fmt.Println(ddl)
	if c.Apply {
		_, err := bq.RunQuery(c.Project, ddl, false, 0)
		return err
	}
	return nil
}

type DeployCmd struct {
	Cloud              bool   `help:"Provision full cloud infrastructure (APIs, IAM, Secret Manager, VM, Looker)."`
	Target             string `enum:"bq,alloydb," default:"" help:"Cloud deployment target: bq or alloydb."`
	Mode               string `default:"testing" help:"Deployment mode: testing (7d) or production (3650d)."`
	Project            string `env:"GCP_PROJECT" help:"GCP Project ID."`
	Dataset            string `env:"GCP_DATASET" help:"Target BigQuery dataset or PostgreSQL schema."`
	Location           string `default:"US" help:"BigQuery dataset location."`
	Region             string `default:"us-central1" help:"GCP region."`
	Zone               string `default:"us-central1-a" help:"GCP zone."`
	Network            string `default:"default" help:"VPC network."`
	Cluster            string `default:"thelook-cluster" help:"AlloyDB cluster ID."`
	Instance           string `default:"thelook-primary" help:"AlloyDB primary instance ID."`
	AlloyDBMachineType string `name:"alloydb-machine-type" default:"c4a-highmem-1" help:"AlloyDB machine type."`
	VMName             string `name:"vm-name" help:"Compute Engine VM name."`
	MachineType        string `name:"machine-type" default:"e2-micro" help:"Compute Engine VM machine type."`
	Image              string `env:"IMAGE" default:"us-central1-docker.pkg.dev/lkr-dev-production/thelook-generator/thelook-generator:latest" help:"Container image URI."`
	UseBinary          bool   `name:"use-binary" help:"Compile binary from source instead of pulling container."`
	Local              bool   `help:"Run generator locally instead of provisioning a Compute Engine VM."`
	Days               int    `default:"0" help:"Backfill days (defaults to 7 for testing, 3650 for production)."`
	Password           string `env:"DB_PASSWORD" help:"AlloyDB postgres password."`
	Secret             string `env:"SECRET" help:"Dashboard HTTP authentication secret."`
	Port               string `default:"8080" help:"Status server port."`
	Looker             bool   `help:"Register Looker connection via Looker SDK."`
	LookerPSC          bool   `name:"looker-psc" help:"Configure Looker Core Hybrid Private Service Connect pipeline."`
	LookerInstance     string `name:"looker-instance" help:"Looker Core instance name."`
	LookerRegion       string `name:"looker-region" help:"Looker Core region."`
	LookerNetwork      string `name:"looker-network" help:"Looker PSC VPC network."`
	PSCDomain          string `name:"psc-domain" default:"alloydb.thelook.internal" help:"PSC domain for AlloyDB."`
	LookerConn         string `name:"looker-connection" help:"Looker connection name."`
	LookerHost         string `name:"looker-host" help:"Override Looker database host."`
	LookerBaseURL      string `name:"looker-base-url" env:"LOOKERSDK_BASE_URL" help:"Looker SDK base URL."`
	LookerClientID     string `name:"looker-client-id" env:"LOOKERSDK_CLIENT_ID" help:"Looker SDK client ID."`
	LookerSecret       string `name:"looker-client-secret" env:"LOOKERSDK_CLIENT_SECRET" help:"Looker SDK client secret."`
	SAKey              string `name:"sa-key" env:"SA_KEY_FILE" help:"Service account JSON key file for BigQuery Looker connection."`
	AuthorizedNetworks string `name:"authorized-networks" help:"Comma-separated CIDR blocks for AlloyDB public IP."`
	FromFY             int    `name:"from-fy" default:"2016" help:"Start fiscal year for retail_calendar_454."`
	ToFY               int    `name:"to-fy" default:"2036" help:"End fiscal year for retail_calendar_454."`
	State              string `default:"state.gob" help:"Path to on-disk state."`
	HTTP               string `default:":8080" help:"HTTP listen address for status server."`
}

func (d *DeployCmd) Run() error {
	if d.Cloud || d.Target != "" || d.Looker || d.LookerPSC || d.VMName != "" {
		if d.Target == "" {
			if isPostgres() {
				d.Target = "alloydb"
			} else {
				d.Target = "bq"
			}
		}
		return d.runCloud(defaultRunner)
	}
	return d.deploySchemaOnly()
}

func (d *DeployCmd) deploySchemaOnly() error {
	if err := requireEnv(); err != nil && d.Project == "" && !isPostgres() {
		return err
	}
	if d.FromFY > d.ToFY {
		return fmt.Errorf("--from-fy (%d) must be <= --to-fy (%d)", d.FromFY, d.ToFY)
	}
	server.RecordCommandRun(d.State, "deploy", os.Args[2:])
	server.StartBackgroundServer(d.HTTP, d.State)

	if isPostgres() {
		sch := postgresSchema(d.Dataset)
		sink, err := postgres.NewSink(context.Background(), "", sch)
		if err != nil {
			return err
		}
		defer sink.Close()
		fmt.Fprintf(os.Stderr, "Creating PostgreSQL schema and %d tables in %s...\n", len(postgres.AllTables), sch)
		if err := sink.ApplyDDL(context.Background()); err != nil {
			return fmt.Errorf("failed to apply table DDL: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Generating and loading retail_calendar_454 (FY%d..FY%d)...\n", d.FromFY, d.ToFY)
		sim.EmitRetailCalendar454(d.FromFY, d.ToFY, sink.Emit)
		return sink.Flush()
	}

	ddl := bq.TableDDL(d.Project, d.Dataset)
	fmt.Fprintf(os.Stderr, "Creating BigQuery dataset and %d tables in %s.%s...\n", len(bq.AllTables), d.Project, d.Dataset)
	if _, err := bq.RunQuery(d.Project, ddl, false, 0); err != nil {
		return fmt.Errorf("failed to apply table DDL: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Generating and loading retail_calendar_454 (FY%d..FY%d)...\n", d.FromFY, d.ToFY)
	sink, err := bq.NewBatchLoadSink(d.Project, d.Dataset)
	if err != nil {
		return err
	}
	sim.EmitRetailCalendar454(d.FromFY, d.ToFY, sink.Emit)
	return sink.FlushAndLoad()
}

type DestroyCmd struct {
	Target         string `default:"all" enum:"bq,alloydb,all" help:"Cloud target to tear down: bq, alloydb, or all."`
	Project        string `env:"GCP_PROJECT" help:"GCP Project ID."`
	Dataset        string `env:"GCP_DATASET" help:"Target BigQuery dataset or PostgreSQL schema."`
	Region         string `default:"us-central1" help:"GCP region."`
	Zone           string `default:"us-central1-a" help:"GCP zone."`
	Network        string `default:"default" help:"VPC network."`
	Cluster        string `default:"thelook-cluster" help:"AlloyDB cluster ID."`
	Instance       string `default:"thelook-primary" help:"AlloyDB primary instance ID."`
	VMName         string `name:"vm-name" help:"Compute Engine VM name."`
	LookerConn     string `name:"looker-connection" help:"Looker connection name."`
	LookerInstance string `name:"looker-instance" help:"Looker Core instance name."`
	LookerRegion   string `name:"looker-region" default:"us-east1" help:"Looker Core region."`
	LookerNetwork  string `name:"looker-network" help:"Looker PSC VPC network."`
	PSCDomain      string `name:"psc-domain" default:"alloydb.thelook.internal" help:"PSC domain attached to Looker."`
	LookerBaseURL  string `name:"looker-base-url" env:"LOOKERSDK_BASE_URL" help:"Looker SDK base URL."`
	LookerClientID string `name:"looker-client-id" env:"LOOKERSDK_CLIENT_ID" help:"Looker SDK client ID."`
	LookerSecret   string `name:"looker-client-secret" env:"LOOKERSDK_CLIENT_SECRET" help:"Looker SDK client secret."`
	DeleteTables   bool   `name:"delete-tables" help:"Only drop the 14 database tables and local state."`
	State          string `default:"state.gob" help:"Path to on-disk state to remove."`
	Profile        string `default:"profile.json" help:"Path to profile JSON to remove."`
	Yes            bool   `short:"y" help:"Skip interactive confirmation."`
	DryRun         bool   `name:"dry-run" help:"Print teardown commands without executing them."`

	// Component flags: default to tearing down everything; pass --no-<flag> to keep specific resources.
	VM             bool `default:"true" negatable:"" help:"Delete Compute Engine generator VM(s) and firewall rule."`
	Database       bool `default:"true" negatable:"" help:"Drop BigQuery dataset/tables and/or delete AlloyDB instance, cluster, and VPC range."`
	Looker         bool `default:"true" negatable:"" help:"Delete Looker connection(s) and detach PSC from Looker Core instance."`
	PSC            bool `default:"true" negatable:"" help:"Delete Looker Core Hybrid PSC pipeline (attachment, forwarding rule, proxy, backend, NEG, subnet)."`
	Secrets        bool `default:"true" negatable:"" help:"Delete TheLook secrets from GCP Secret Manager."`
	ServiceAccount bool `name:"service-account" default:"true" negatable:"" help:"Revoke IAM role bindings added to the Compute Engine default service account."`
	LocalState     bool `name:"local-state" default:"true" negatable:"" help:"Remove local state.gob, profile.json, and command logs."`
}

func (d *DestroyCmd) Run() error {
	return d.execute(os.Stdin, defaultRunner)
}

func cmdDestroy(args []string, in io.Reader) error {
	return runDestroyArgs(args, in, defaultRunner)
}

func runDestroyArgs(args []string, in io.Reader, run cmdRunner) error {
	var cli CLI
	parser, err := kong.New(&cli)
	if err != nil {
		return err
	}
	if _, err := parser.Parse(append([]string{"destroy"}, args...)); err != nil {
		return err
	}
	return cli.Destroy.execute(in, run)
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("thelook"),
		kong.Description("TheLook synthetic e-commerce event and CDC data generator (BigQuery & AlloyDB)."),
		kong.UsageOnError(),
	)
	if err := ctx.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
