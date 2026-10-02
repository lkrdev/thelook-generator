package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"thelook-generator/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sink handles unified PostgreSQL ingestion for both bulk backfills and live CDC upserts.
// ponytail: single pgx.Batch pipeline covers both streaming and batching without separate batch loaders.
type Sink struct {
	pool   *pgxpool.Pool
	schema string
	batch  *pgx.Batch
	mu     sync.Mutex
}

func ResolveConnString(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	if v := os.Getenv("ALLOYDB_URL"); v != "" {
		return v
	}
	// Let pgx parse standard PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD
	return ""
}

func NewSink(ctx context.Context, connStr, schema string) (*Sink, error) {
	if schema == "" {
		schema = "public"
	}
	resolved := ResolveConnString(connStr)
	cfg, err := pgxpool.ParseConfig(resolved)
	if err != nil {
		return nil, fmt.Errorf("parse postgres connection: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Sink{
		pool:   pool,
		schema: schema,
		batch:  &pgx.Batch{},
	}, nil
}

func (s *Sink) ApplyDDL(ctx context.Context) error {
	ddl := TableDDL(s.schema)
	_, err := s.pool.Exec(ctx, ddl)
	return err
}

func (s *Sink) DropTables(ctx context.Context) error {
	ddl := DropTablesDDL(s.schema)
	_, err := s.pool.Exec(ctx, ddl)
	return err
}

func (s *Sink) Write(row any) error {
	sql, args, err := s.rowToSQL(row)
	if err != nil {
		return err
	}
	if sql == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.batch.Queue(sql, args...)
	if s.batch.Len() >= 1000 {
		return s.flushLocked(context.Background())
	}
	return nil
}

func (s *Sink) Emit(row any) {
	_ = s.Write(row)
}

func (s *Sink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked(context.Background())
}

func (s *Sink) FlushCtx(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked(ctx)
}

func (s *Sink) flushLocked(ctx context.Context) error {
	if s.batch.Len() == 0 {
		return nil
	}
	b := s.batch
	s.batch = &pgx.Batch{}

	br := s.pool.SendBatch(ctx, b)
	defer br.Close()

	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch exec item %d/%d: %w", i+1, b.Len(), err)
		}
	}
	return nil
}

func (s *Sink) Close() error {
	_ = s.Flush()
	s.pool.Close()
	return nil
}

func (s *Sink) rowToSQL(row any) (string, []any, error) {
	sch := s.schema
	switch r := row.(type) {
	case model.DistributionCenterRow:
		return fmt.Sprintf(`INSERT INTO %s.distribution_centers (id, name, latitude, longitude) VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.Name, r.Latitude, r.Longitude}, nil
	case model.ProductRow:
		return fmt.Sprintf(`INSERT INTO %s.products (id, cost, category, name, brand, retail_price, department, sku, distribution_center_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.Cost, r.Category, r.Name, r.Brand, r.RetailPrice, r.Department, r.SKU, r.DistributionCenterID}, nil
	case model.UserRow:
		return fmt.Sprintf(`INSERT INTO %s.users (id, first_name, last_name, email, age, city, state, country, zip, latitude, longitude, gender, created_at, traffic_source) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.FirstName, r.LastName, r.Email, r.Age, r.City, r.State, r.Country, r.Zip, r.Latitude, r.Longitude, r.Gender, r.CreatedAt, r.TrafficSource}, nil
	case model.InventoryItemRow:
		return fmt.Sprintf(`INSERT INTO %s.inventory_items (id, product_id, created_at, sold_at, cost, product_category, product_name, product_brand, product_retail_price, product_department, product_sku, product_distribution_center_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (id) DO UPDATE SET sold_at = EXCLUDED.sold_at`, sch),
			[]any{r.ID, r.ProductID, r.CreatedAt, r.SoldAt, r.Cost, r.ProductCategory, r.ProductName, r.ProductBrand, r.ProductRetailPrice, r.ProductDepartment, r.ProductSKU, r.ProductDistributionCenterID}, nil
	case model.OrderItemRow:
		return fmt.Sprintf(`INSERT INTO %s.order_items (id, order_id, user_id, inventory_item_id, sale_price, status, created_at, returned_at, shipped_at, delivered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, returned_at = EXCLUDED.returned_at, shipped_at = EXCLUDED.shipped_at, delivered_at = EXCLUDED.delivered_at`, sch),
			[]any{r.ID, r.OrderID, r.UserID, r.InventoryItemID, r.SalePrice, r.Status, r.CreatedAt, r.ReturnedAt, r.ShippedAt, r.DeliveredAt}, nil
	case model.EventRow:
		return fmt.Sprintf(`INSERT INTO %s.events (id, sequence_number, session_id, created_at, ip_address, city, state, country, zip, latitude, longitude, os, browser, traffic_source, user_id, uri, event_type, ad_event_id, referrer_code) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.SequenceNumber, r.SessionID, r.CreatedAt, r.IPAddress, r.City, r.State, r.Country, r.Zip, r.Latitude, r.Longitude, r.OS, r.Browser, r.TrafficSource, r.UserID, r.URI, r.EventType, r.AdEventID, r.ReferrerCode}, nil
	case model.CampaignRow:
		return fmt.Sprintf(`INSERT INTO %s.campaigns (id, advertising_channel, amount, bid_type, campaign_name, period, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.AdvertisingChannel, r.Amount, r.BidType, r.CampaignName, r.Period, r.CreatedAt}, nil
	case model.AdGroupRow:
		return fmt.Sprintf(`INSERT INTO %s.ad_groups (ad_id, campaign_id, created_at, name, period, ad_type, headline) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (ad_id) DO NOTHING`, sch),
			[]any{r.AdID, r.CampaignID, r.CreatedAt, r.Name, r.Period, r.AdType, r.Headline}, nil
	case model.KeywordRow:
		return fmt.Sprintf(`INSERT INTO %s.keywords (keyword_id, ad_id, created_at, criterion_name, cpc_bid_amount, period_id, system_serving_status, bidding_strategy_type, quality_score, keyword_match_type) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (keyword_id) DO NOTHING`, sch),
			[]any{r.KeywordID, r.AdID, r.CreatedAt, r.CriterionName, r.CPCBidAmount, r.PeriodID, r.SystemServingStatus, r.BiddingStrategyType, r.QualityScore, r.KeywordMatchType}, nil
	case model.AdEventRow:
		return fmt.Sprintf(`INSERT INTO %s.ad_events (id, keyword_id, amount, device_type, event_type, created_at) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`, sch),
			[]any{r.ID, r.KeywordID, r.Amount, r.DeviceType, r.EventType, r.CreatedAt}, nil
	case model.DiscountRow:
		return fmt.Sprintf(`INSERT INTO %s.discounts (product_id, inventory_item_id, retail_price, discount_price, discount_amount, date) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (inventory_item_id) DO NOTHING`, sch),
			[]any{r.ProductID, r.InventoryItemID, r.RetailPrice, r.DiscountPrice, r.DiscountAmount, r.Date}, nil
	case model.CombinedOrdersUsersRow:
		return fmt.Sprintf(`INSERT INTO %s.combined_orders_users (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, sch),
			[]any{r.UserID}, nil
	case model.TransactionDetailRow:
		itemsJSON, err := json.Marshal(r.Items)
		if err != nil {
			return "", nil, err
		}
		userJSON, err := json.Marshal(r.User)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf(`INSERT INTO %s.transaction_detail (order_id, status, created_at, shipped_at, delivered_at, items, user_data) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (order_id) DO UPDATE SET status = EXCLUDED.status, shipped_at = EXCLUDED.shipped_at, delivered_at = EXCLUDED.delivered_at, items = EXCLUDED.items, user_data = EXCLUDED.user_data`, sch),
			[]any{r.OrderID, r.Status, r.CreatedAt, r.ShippedAt, r.DeliveredAt, itemsJSON, userJSON}, nil
	case model.RetailCalendar454Row:
		return fmt.Sprintf(`INSERT INTO %s.retail_calendar_454 (reference_date, fiscal_year, fiscal_year_num, fiscal_quarter_of_year, fiscal_quarter_of_year_num, fiscal_week_of_year, fiscal_week_of_year_num, fiscal_period_of_year, fiscal_period_of_year_num, season, season_num, prev_custom_date, prev_custom_week) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (reference_date) DO NOTHING`, sch),
			[]any{r.ReferenceDate, r.FiscalYear, r.FiscalYearNum, r.FiscalQuarterOfYear, r.FiscalQuarterOfYearNum, r.FiscalWeekOfYear, r.FiscalWeekOfYearNum, r.FiscalPeriodOfYear, r.FiscalPeriodOfYearNum, r.Season, r.SeasonNum, r.PrevCustomDate, r.PrevCustomWeek}, nil
	default:
		return "", nil, nil
	}
}
