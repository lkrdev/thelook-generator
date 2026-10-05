package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

func (e *JSONEmitter) Flush() error {
	return e.w.Flush()
}

func (e *JSONEmitter) Close() error {
	return e.Flush()
}

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
	if override != "" {
		return override
	}
	if s := os.Getenv("ALLOYDB_SCHEMA"); s != "" {
		return s
	}
	if s := os.Getenv("PGSCHEMA"); s != "" {
		return s
	}
	if s := os.Getenv("GCP_DATASET"); s != "" {
		return s
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

func usage() {
	fmt.Fprintf(os.Stderr, `thelook - Modernized Data Generator (BigQuery + PostgreSQL / AlloyDB CDC)

Target Selection:
  Default: BigQuery (requires GCP_PROJECT, GCP_DATASET, and SECRET)
  PostgreSQL / AlloyDB: set TARGET_DB=postgres or DATABASE_URL (requires SECRET, uses DATABASE_URL / PGHOST)

Required Environment Variables:
  SECRET       Authentication key for the HTTP status server (?key=<SECRET>)
  GCP_PROJECT  GCP Project ID (BigQuery mode)
  GCP_DATASET  BigQuery Dataset/Schema (BigQuery mode)
  DATABASE_URL PostgreSQL / AlloyDB connection string (PostgreSQL mode, or standard PG* env vars)

Usage:
  thelook deploy   [--project <gcp-project>] [--dataset <schema>] [--from-fy 2016] [--to-fy 2036] [--state state.gob] [--http :8080]
  thelook destroy  [--delete-tables] [--project <gcp-project>] [--dataset <schema>] [--state state.gob] [--profile profile.json]
  thelook tables   [--project <gcp-project>] [--dataset <schema>] [--apply] [--http :8080]
  thelook backfill [--from 2016-10-02T00:00:00Z --to 2026-10-02T00:00:00Z | --days 3650] [--stdout] [--rate 0] [--state state.gob] [--http :8080]
  thelook calendar [--from-fy 2016] [--to-fy 2036] [--stdout] [--rate 0] [--http :8080]
  thelook run      [--state state.gob] [--profile profile.json] [--stdout] [--http :8080] [--analyze-interval 24h]
  thelook gaps     [--state state.gob] [--fill] [--stdout] [--rate 0] [--check-bq] [--dataset <project.schema>] [--http :8080]
  thelook status   [--state state.gob] [--http :8080]
  thelook analyze  [--dataset <project.schema>] [--out profile.json] [--http :8080]
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	if err := requireEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "deploy":
		err = cmdDeploy(args)
	case "destroy":
		err = cmdDestroy(args, os.Stdin)
	case "run":
		err = cmdRun(args)
	case "backfill":
		err = cmdBackfill(args)
	case "gaps":
		err = cmdGaps(args)
	case "analyze":
		err = cmdAnalyze(args)
	case "calendar":
		err = cmdCalendar(args)
	case "tables":
		err = cmdTables(args)
	case "status":
		err = cmdStatus(args)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	statePath := fs.String("state", "state.gob", "Path to on-disk state")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	server.RecordCommandRun(*statePath, "status", args)
	fmt.Fprintf(os.Stderr, "Serving status on http://%s\n", *httpAddr)
	return http.ListenAndServe(*httpAddr, server.NewStatusMux(*statePath))
}

func cmdCalendar(args []string) error {
	fs := flag.NewFlagSet("calendar", flag.ExitOnError)
	fromFY := fs.Int("from-fy", 2016, "Start fiscal year (inclusive)")
	toFY := fs.Int("to-fy", 2036, "End fiscal year (inclusive)")
	stdout := fs.Bool("stdout", false, "Emit JSON lines to stdout instead of loading into BigQuery")
	rate := fs.Int("rate", 0, "Max JSON lines/sec when --stdout is set (0 = unlimited)")
	statePath := fs.String("state", "state.gob", "Path to on-disk state (for command log)")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)
	if *fromFY > *toFY {
		return fmt.Errorf("--from-fy (%d) must be <= --to-fy (%d)", *fromFY, *toFY)
	}
	server.RecordCommandRun(*statePath, "calendar", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	if *stdout {
		emitter := NewJSONEmitter(os.Stdout, *rate)
		sim.EmitRetailCalendar454(*fromFY, *toFY, emitter.Emit)
		return emitter.Flush()
	}
	if isPostgres() {
		sink, err := postgres.NewSink(context.Background(), "", postgresSchema(""))
		if err != nil {
			return err
		}
		defer sink.Close()
		sim.EmitRetailCalendar454(*fromFY, *toFY, sink.Emit)
		return sink.Flush()
	}
	sink, err := bq.NewBatchLoadSink(os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
	if err != nil {
		return err
	}
	sim.EmitRetailCalendar454(*fromFY, *toFY, sink.Emit)
	return sink.FlushAndLoad()
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	statePath := fs.String("state", "state.gob", "Path to on-disk FK & interval lookup state")
	profilePath := fs.String("profile", "profile.json", "Path to analyzed statistical profile JSON")
	analyzeInterval := fs.Duration("analyze-interval", 24*time.Hour, "Interval to re-analyze BigQuery dataset patterns (0 to disable)")
	dataset := fs.String("dataset", defaultDataset(), "BigQuery project.schema to analyze (env: GCP_PROJECT, GCP_DATASET)")
	initialProducts := fs.Int("initial-products", 14560, "Initial product catalog size on bootstrap")
	stdout := fs.Bool("stdout", false, "Emit JSON lines to stdout instead of streaming to BigQuery")
	rate := fs.Int("rate", 0, "Max JSON lines/sec when --stdout is set (0 = unlimited)")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	server.RecordCommandRun(*statePath, "run", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	st, err := model.LoadState(*statePath)
	if err != nil {
		return err
	}
	eng, err := sim.NewEngine(st, defaultSeedJSON, *profilePath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitFn func(any)
	var flushFn func() error
	if *stdout {
		em := NewJSONEmitter(os.Stdout, *rate)
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
	if *analyzeInterval > 0 {
		analyzeTicker = time.NewTicker(*analyzeInterval)
		defer analyzeTicker.Stop()
		analyzeCh = analyzeTicker.C
	}

	runOnce := func(t time.Time) {
		eng.Tick(t.UTC().Truncate(time.Minute), *initialProducts, emitFn)
		if err := flushFn(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: bigquery flush failed: %v\n", err)
		}
		if err := st.Save(*statePath); err != nil {
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
			return st.Save(*statePath)
		case t := <-ticker.C:
			runOnce(t)
		case <-analyzeCh:
			if err := bq.AnalyzeDataset(*dataset, defaultSeedJSON, *profilePath); err == nil {
				if updated, err := sim.NewEngine(st, defaultSeedJSON, *profilePath); err == nil {
					eng.Seed = updated.Seed
				}
			}
		}
	}
}

func cmdBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	fromStr := fs.String("from", "", "Start timestamp (RFC3339 or YYYY-MM-DD)")
	toStr := fs.String("to", "", "End timestamp (RFC3339 or YYYY-MM-DD, defaults to now)")
	days := fs.Int("days", 0, "Backfill N days ending at --to (e.g. --days 3650 for 10 years)")
	stdout := fs.Bool("stdout", false, "Emit JSON lines to stdout instead of running bq load")
	rate := fs.Int("rate", 0, "Max JSON lines/sec when --stdout is set (0 = unlimited)")
	statePath := fs.String("state", "state.gob", "Path to on-disk FK & interval state")
	profilePath := fs.String("profile", "profile.json", "Path to analyzed statistical profile JSON")
	initialProducts := fs.Int("initial-products", 14560, "Initial product catalog size on bootstrap (14560 = half of 29120)")
	force := fs.Bool("force", false, "Re-generate even if minute is already recorded in state")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	server.RecordCommandRun(*statePath, "backfill", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	toTime := time.Now().UTC().Truncate(time.Minute)
	if *toStr != "" {
		var err error
		toTime, err = parseTimeArg(*toStr)
		if err != nil {
			return err
		}
	}
	var fromTime time.Time
	if *fromStr != "" {
		var err error
		fromTime, err = parseTimeArg(*fromStr)
		if err != nil {
			return err
		}
	} else if *days > 0 {
		fromTime = toTime.Add(-time.Duration(*days) * 24 * time.Hour)
	} else {
		return fmt.Errorf("specify --from <timestamp> or --days <N>")
	}
	if fromTime.After(toTime) {
		return fmt.Errorf("--from (%s) is after --to (%s)", model.FmtTS(fromTime), model.FmtTS(toTime))
	}

	st, err := model.LoadState(*statePath)
	if err != nil {
		return err
	}
	eng, err := sim.NewEngine(st, defaultSeedJSON, *profilePath)
	if err != nil {
		return err
	}

	totalMinutes := int(toTime.Sub(fromTime).Minutes()) + 1
	totalDays := float64(totalMinutes) / 1440.0
	targetDesc := "BigQuery (" + os.Getenv("GCP_PROJECT") + "." + os.Getenv("GCP_DATASET") + ")"
	if *stdout {
		targetDesc = "stdout"
	} else if isPostgres() {
		targetDesc = fmt.Sprintf("PostgreSQL/AlloyDB (%s)", postgresSchema(""))
	}

	fmt.Fprintf(os.Stderr, "==> [backfill] Starting backfill: %s -> %s\n", model.FmtTS(fromTime), model.FmtTS(toTime))
	fmt.Fprintf(os.Stderr, "    Span: %.1f days (%d minutes) | Target: %s | State: %s\n", totalDays, totalMinutes, targetDesc, *statePath)
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

	if *stdout {
		emitter := NewJSONEmitter(os.Stdout, *rate)
		emitFn = emitter.Emit
		batchInterval = 1440
		flushBatch = func(time.Time) error { return emitter.Flush() }
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
			return sink.Flush()
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
		if !*force && st.IsMinuteCovered(cur) {
			skipped++
			if minuteIdx%reportInterval == 0 || cur.Equal(toTime) {
				printProgress(cur, minuteIdx, steps, skipped)
			}
			continue
		}
		eng.Tick(cur, *initialProducts, emitFn)
		steps++
		if steps%batchInterval == 0 {
			if err := flushBatch(cur); err != nil {
				return err
			}
			_ = st.Save(*statePath)
		}
		if minuteIdx%reportInterval == 0 || cur.Equal(toTime) {
			printProgress(cur, minuteIdx, steps, skipped)
		}
	}

	if err := finish(steps, skipped); err != nil {
		return err
	}
	return st.Save(*statePath)
}

func cmdGaps(args []string) error {
	fs := flag.NewFlagSet("gaps", flag.ExitOnError)
	statePath := fs.String("state", "state.gob", "Path to on-disk state")
	profilePath := fs.String("profile", "profile.json", "Path to statistical profile JSON")
	fill := fs.Bool("fill", false, "Immediately backfill all detected gaps")
	stdout := fs.Bool("stdout", false, "Emit JSON lines to stdout when --fill is set")
	rate := fs.Int("rate", 0, "Max JSON lines/sec when --fill and --stdout are set (0 = unlimited)")
	checkBQ := fs.Bool("check-bq", false, "Also query BigQuery dataset for latest timestamp gaps")
	dataset := fs.String("dataset", defaultDataset(), "BigQuery project.schema for --check-bq (env: GCP_PROJECT, GCP_DATASET)")
	initialProducts := fs.Int("initial-products", 14560, "Initial products when filling")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	server.RecordCommandRun(*statePath, "gaps", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	st, err := model.LoadState(*statePath)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Truncate(time.Minute)
	gaps := server.FindGaps(st, now)

	if *checkBQ {
		q := fmt.Sprintf("SELECT max(string(created_at)) AS max_ts FROM `%s.events`", *dataset)
		if out, err := bq.RunQuery("", q, true, 0); err == nil {
			var rows []map[string]string
			if json.Unmarshal(out, &rows) == nil && len(rows) > 0 && rows[0]["max_ts"] != "" {
				fmt.Fprintf(os.Stderr, "BigQuery %s.events latest timestamp: %s\n", *dataset, rows[0]["max_ts"])
			}
		}
	}

	if len(st.Intervals) == 0 {
		fmt.Fprintf(os.Stderr, "No intervals recorded yet in %s. Run initial 10-year backfill:\n  thelook backfill --days 3650 --state %s\n", *statePath, *statePath)
		return nil
	}

	if len(gaps) == 0 {
		fmt.Fprintf(os.Stderr, "No gaps detected across %d recorded interval(s).\n", len(st.Intervals))
		return nil
	}

	for i, g := range gaps {
		mins := int(g.To.Sub(g.From).Minutes()) + 1
		fmt.Fprintf(os.Stderr, "Gap #%d (%d mins): %s -> %s\n  Command: thelook backfill --from %s --to %s --state %s\n",
			i+1, mins, model.FmtTS(g.From), model.FmtTS(g.To), model.FmtTS(g.From), model.FmtTS(g.To), *statePath)
	}

	if *fill {
		eng, err := sim.NewEngine(st, defaultSeedJSON, *profilePath)
		if err != nil {
			return err
		}
		if *stdout {
			emitter := NewJSONEmitter(os.Stdout, *rate)
			for _, g := range gaps {
				for cur := g.From; !cur.After(g.To); cur = cur.Add(time.Minute) {
					eng.Tick(cur, *initialProducts, emitter.Emit)
				}
			}
			_ = emitter.Flush()
			return st.Save(*statePath)
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
					eng.Tick(cur, *initialProducts, sink.Emit)
				}
				if err := sink.Flush(); err != nil {
					return err
				}
			}
			return st.Save(*statePath)
		}
		sink, err := bq.NewStorageWriteSink(ctx, os.Getenv("GCP_PROJECT"), os.Getenv("GCP_DATASET"))
		if err != nil {
			return err
		}
		defer sink.Close()
		for _, g := range gaps {
			for cur := g.From; !cur.After(g.To); cur = cur.Add(time.Minute) {
				eng.Tick(cur, *initialProducts, sink.Emit)
			}
			if err := sink.Flush(); err != nil {
				return err
			}
		}
		return st.Save(*statePath)
	}
	return nil
}

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	dataset := fs.String("dataset", defaultDataset(), "BigQuery project.schema to analyze (env: GCP_PROJECT, GCP_DATASET)")
	outPath := fs.String("out", "profile.json", "Output profile JSON path")
	statePath := fs.String("state", "state.gob", "Path to on-disk state (for command log)")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)
	server.RecordCommandRun(*statePath, "analyze", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	return bq.AnalyzeDataset(*dataset, defaultSeedJSON, *outPath)
}

func cmdTables(args []string) error {
	fs := flag.NewFlagSet("tables", flag.ExitOnError)
	project := fs.String("project", os.Getenv("GCP_PROJECT"), "GCP Project ID (env: GCP_PROJECT)")
	dataset := fs.String("dataset", os.Getenv("GCP_DATASET"), "Target BigQuery dataset/schema name (env: GCP_DATASET)")
	apply := fs.Bool("apply", false, "Execute DDL via bq query directly instead of only printing")
	statePath := fs.String("state", "state.gob", "Path to on-disk state (for command log)")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	server.RecordCommandRun(*statePath, "tables", args)
	server.StartBackgroundServer(*httpAddr, *statePath)
	if isPostgres() {
		sch := postgresSchema(*dataset)
		ddl := postgres.TableDDL(sch)
		fmt.Println(ddl)
		if *apply {
			sink, err := postgres.NewSink(context.Background(), "", sch)
			if err != nil {
				return err
			}
			defer sink.Close()
			return sink.ApplyDDL(context.Background())
		}
		return nil
	}
	ddl := bq.TableDDL(*project, *dataset)
	fmt.Println(ddl)
	if *apply {
		_, err := bq.RunQuery(*project, ddl, false, 0)
		return err
	}
	return nil
}

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	project := fs.String("project", os.Getenv("GCP_PROJECT"), "GCP Project ID (env: GCP_PROJECT)")
	dataset := fs.String("dataset", os.Getenv("GCP_DATASET"), "Target BigQuery dataset/schema name (env: GCP_DATASET)")
	fromFY := fs.Int("from-fy", 2016, "Start fiscal year for retail_calendar_454 (inclusive)")
	toFY := fs.Int("to-fy", 2036, "End fiscal year for retail_calendar_454 (inclusive)")
	statePath := fs.String("state", "state.gob", "Path to on-disk state (for command log)")
	httpAddr := fs.String("http", ":8080", "HTTP listen address for status server")
	fs.Parse(args)

	if *fromFY > *toFY {
		return fmt.Errorf("--from-fy (%d) must be <= --to-fy (%d)", *fromFY, *toFY)
	}
	server.RecordCommandRun(*statePath, "deploy", args)
	server.StartBackgroundServer(*httpAddr, *statePath)

	if isPostgres() {
		sch := postgresSchema(*dataset)
		sink, err := postgres.NewSink(context.Background(), "", sch)
		if err != nil {
			return err
		}
		defer sink.Close()
		fmt.Fprintf(os.Stderr, "Creating PostgreSQL schema and %d tables in %s...\n", len(postgres.AllTables), sch)
		if err := sink.ApplyDDL(context.Background()); err != nil {
			return fmt.Errorf("failed to apply table DDL: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Generating and loading retail_calendar_454 (FY%d..FY%d)...\n", *fromFY, *toFY)
		sim.EmitRetailCalendar454(*fromFY, *toFY, sink.Emit)
		return sink.Flush()
	}

	ddl := bq.TableDDL(*project, *dataset)
	fmt.Fprintf(os.Stderr, "Creating BigQuery dataset and %d tables in %s.%s...\n", len(bq.AllTables), *project, *dataset)
	if _, err := bq.RunQuery(*project, ddl, false, 0); err != nil {
		return fmt.Errorf("failed to apply table DDL: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Generating and loading retail_calendar_454 (FY%d..FY%d)...\n", *fromFY, *toFY)
	sink, err := bq.NewBatchLoadSink(*project, *dataset)
	if err != nil {
		return err
	}
	sim.EmitRetailCalendar454(*fromFY, *toFY, sink.Emit)
	return sink.FlushAndLoad()
}

func cmdDestroy(args []string, in io.Reader) error {
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	project := fs.String("project", os.Getenv("GCP_PROJECT"), "GCP Project ID (env: GCP_PROJECT)")
	dataset := fs.String("dataset", os.Getenv("GCP_DATASET"), "Target BigQuery dataset/schema name (env: GCP_DATASET)")
	deleteTables := fs.Bool("delete-tables", false, "Drop all 14 BigQuery tables in <project>.<dataset> after interactive confirmation")
	statePath := fs.String("state", "state.gob", "Path to on-disk state to remove")
	profilePath := fs.String("profile", "profile.json", "Path to profile JSON to remove")
	fs.Parse(args)

	if *deleteTables {
		if isPostgres() {
			sch := postgresSchema(*dataset)
			fmt.Fprintf(os.Stderr, "Confirm deleting all %d tables in PostgreSQL schema %s by typing %q: ", len(postgres.AllTables), sch, sch)
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && err != io.EOF {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if got := strings.TrimSpace(line); got != sch {
				return fmt.Errorf("aborted: confirmation %q did not match %q", got, sch)
			}
			sink, err := postgres.NewSink(context.Background(), "", sch)
			if err != nil {
				return err
			}
			defer sink.Close()
			fmt.Fprintf(os.Stderr, "Dropping %d tables in %s...\n", len(postgres.AllTables), sch)
			if err := sink.DropTables(context.Background()); err != nil {
				return fmt.Errorf("failed to drop tables: %w", err)
			}
		} else {
			target := fmt.Sprintf("%s.%s", *project, *dataset)
			fmt.Fprintf(os.Stderr, "Confirm deleting all %d tables in %s by typing %q: ", len(bq.AllTables), target, target)
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && err != io.EOF {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if got := strings.TrimSpace(line); got != target {
				return fmt.Errorf("aborted: confirmation %q did not match %q", got, target)
			}
			ddl := bq.DropTablesDDL(*project, *dataset)
			fmt.Fprintf(os.Stderr, "Dropping %d tables in %s...\n", len(bq.AllTables), target)
			if _, err := bq.RunQuery(*project, ddl, false, 0); err != nil {
				return fmt.Errorf("failed to drop tables: %w", err)
			}
		}
	}

	for _, p := range []string{*statePath, *statePath + ".tmp", server.CommandLogPath(*statePath), *profilePath} {
		if err := os.Remove(p); err == nil {
			fmt.Fprintf(os.Stderr, "Removed %s\n", p)
		}
	}
	return nil
}
