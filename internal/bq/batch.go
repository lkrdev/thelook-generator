package bq

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"thelook-generator/internal/model"

	"cloud.google.com/go/bigquery"
)

func isSharedSeedTable(tbl string) bool {
	return tbl == "distribution_centers" || tbl == "products"
}

// BatchLoadSink writes gzipped NDJSON temp files per table (deduplicating CDC and shared seed tables by PK) and runs free BigQuery load jobs.
type BatchLoadSink struct {
	project string
	dataset string
	TmpDir  string
	files   map[string]*os.File
	gzs     map[string]*gzip.Writer
	writers map[string]*bufio.Writer
	encs    map[string]*json.Encoder
	cdcRows map[string]map[int64]any
}

func NewBatchLoadSink(project, dataset string) (*BatchLoadSink, error) {
	tmpDir, err := os.MkdirTemp("", "thelook-batch-*")
	if err != nil {
		return nil, err
	}
	return &BatchLoadSink{
		project: project,
		dataset: dataset,
		TmpDir:  tmpDir,
		files:   make(map[string]*os.File),
		gzs:     make(map[string]*gzip.Writer),
		writers: make(map[string]*bufio.Writer),
		encs:    make(map[string]*json.Encoder),
		cdcRows: make(map[string]map[int64]any),
	}, nil
}

func (b *BatchLoadSink) writeRow(table string, row any) error {
	enc, ok := b.encs[table]
	if !ok {
		f, err := os.Create(filepath.Join(b.TmpDir, table+".ndjson.gz"))
		if err != nil {
			return err
		}
		bw := bufio.NewWriterSize(f, 256*1024)
		gz, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
		enc = json.NewEncoder(gz)
		enc.SetEscapeHTML(false)
		b.files[table] = f
		b.gzs[table] = gz
		b.writers[table] = bw
		b.encs[table] = enc
	}
	return enc.Encode(row)
}

func (b *BatchLoadSink) Emit(row any) {
	tbl, pk, isCDC := model.RowTable(row)
	if tbl == "" {
		return
	}
	if isCDC || isSharedSeedTable(tbl) {
		if b.cdcRows[tbl] == nil {
			b.cdcRows[tbl] = make(map[int64]any)
		}
		// ponytail: keep only final state per PK (and shared bootstrap catalog IDs) so load jobs write zero duplicate primary keys.
		b.cdcRows[tbl][pk] = row
		return
	}
	_ = b.writeRow(tbl, row)
}

func (b *BatchLoadSink) FlushFiles(existing map[string]map[int64]bool) error {
	for _, tbl := range AllTables {
		if m := b.cdcRows[tbl]; len(m) > 0 {
			for id := range existing[tbl] {
				delete(m, id)
			}
			for _, r := range m {
				if err := b.writeRow(tbl, r); err != nil {
					return err
				}
			}
			clear(b.cdcRows[tbl])
		}
	}
	for tbl, gz := range b.gzs {
		if err := gz.Close(); err != nil {
			return fmt.Errorf("gzip close %s: %w", tbl, err)
		}
		if err := b.writers[tbl].Flush(); err != nil {
			return fmt.Errorf("flush %s: %w", tbl, err)
		}
		if err := b.files[tbl].Close(); err != nil {
			return fmt.Errorf("close %s: %w", tbl, err)
		}
	}
	return nil
}

func (b *BatchLoadSink) FlushAndLoad() error {
	defer os.RemoveAll(b.TmpDir)
	ctx := context.Background()
	client, err := bigquery.NewClient(ctx, b.project)
	if err != nil {
		return fmt.Errorf("bigquery.NewClient: %w", err)
	}
	defer client.Close()

	if err := runJob(ctx, client.Query(TableDDL(b.project, b.dataset)).Run); err != nil {
		return fmt.Errorf("apply table DDL: %w", err)
	}

	existing := make(map[string]map[int64]bool, 2)
	for _, tbl := range []string{"distribution_centers", "products"} {
		if len(b.cdcRows[tbl]) > 0 {
			existing[tbl] = ExistingIDs(ctx, client, b.dataset, tbl)
		}
	}
	if err := b.FlushFiles(existing); err != nil {
		return err
	}

	for _, tbl := range AllTables {
		path := filepath.Join(b.TmpDir, tbl+".ndjson.gz")
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			continue
		}
		target := fmt.Sprintf("%s:%s.%s", b.project, b.dataset, tbl)
		fmt.Fprintf(os.Stderr, "Loading %s (%d compressed bytes)...\n", target, fi.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		src := bigquery.NewReaderSource(f)
		src.SourceFormat = bigquery.JSON
		err = runJob(ctx, client.Dataset(b.dataset).Table(tbl).LoaderFrom(src).Run)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("bq load %s failed: %w", target, err)
		}
	}
	return nil
}
