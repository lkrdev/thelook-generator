package postgres

import (
	"fmt"
	"strings"
)

var AllTables = []string{
	"distribution_centers",
	"products",
	"users",
	"inventory_items",
	"order_items",
	"events",
	"campaigns",
	"ad_groups",
	"keywords",
	"ad_events",
	"discounts",
	"combined_orders_users",
	"transaction_detail",
	"retail_calendar_454",
}

// TableDDL generates PostgreSQL DDL for all 14 tables.
func TableDDL(schema string) string {
	if schema == "" {
		schema = "public"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE SCHEMA IF NOT EXISTS %s;\n\n", schema)

	fmt.Fprintf(&sb, `CREATE TABLE IF NOT EXISTS %s.distribution_centers (
  id BIGINT PRIMARY KEY,
  name TEXT,
  latitude DOUBLE PRECISION,
  longitude DOUBLE PRECISION
);

CREATE TABLE IF NOT EXISTS %s.products (
  id BIGINT PRIMARY KEY,
  cost NUMERIC(12,4),
  category TEXT,
  name TEXT,
  brand TEXT,
  retail_price NUMERIC(10,2),
  department TEXT,
  sku TEXT,
  distribution_center_id TEXT
);

CREATE TABLE IF NOT EXISTS %s.users (
  id BIGINT PRIMARY KEY,
  first_name TEXT,
  last_name TEXT,
  email TEXT,
  age BIGINT,
  city TEXT,
  state TEXT,
  country TEXT,
  zip TEXT,
  latitude DOUBLE PRECISION,
  longitude DOUBLE PRECISION,
  gender TEXT,
  created_at TIMESTAMPTZ,
  traffic_source TEXT
);

CREATE TABLE IF NOT EXISTS %s.inventory_items (
  id BIGINT PRIMARY KEY,
  product_id BIGINT,
  created_at TIMESTAMPTZ,
  sold_at TIMESTAMPTZ,
  cost NUMERIC(12,4),
  product_category TEXT,
  product_name TEXT,
  product_brand TEXT,
  product_retail_price NUMERIC(10,2),
  product_department TEXT,
  product_sku TEXT,
  product_distribution_center_id BIGINT
);

CREATE TABLE IF NOT EXISTS %s.order_items (
  id BIGINT PRIMARY KEY,
  order_id BIGINT,
  user_id BIGINT,
  inventory_item_id BIGINT,
  sale_price NUMERIC(10,2),
  status TEXT,
  created_at TIMESTAMPTZ,
  returned_at TIMESTAMPTZ,
  shipped_at TIMESTAMPTZ,
  delivered_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS %s.events (
  id BIGINT PRIMARY KEY,
  sequence_number BIGINT,
  session_id TEXT,
  created_at TIMESTAMPTZ,
  ip_address TEXT,
  city TEXT,
  state TEXT,
  country TEXT,
  zip TEXT,
  latitude DOUBLE PRECISION,
  longitude DOUBLE PRECISION,
  os TEXT,
  browser TEXT,
  traffic_source TEXT,
  user_id BIGINT,
  uri TEXT,
  event_type TEXT,
  ad_event_id BIGINT,
  referrer_code TEXT
);

CREATE TABLE IF NOT EXISTS %s.campaigns (
  id BIGINT PRIMARY KEY,
  advertising_channel TEXT,
  amount NUMERIC(10,2),
  bid_type TEXT,
  campaign_name TEXT,
  period TEXT,
  created_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS %s.ad_groups (
  ad_id BIGINT PRIMARY KEY,
  campaign_id BIGINT,
  created_at TIMESTAMPTZ,
  name TEXT,
  period BIGINT,
  ad_type TEXT,
  headline TEXT
);

CREATE TABLE IF NOT EXISTS %s.keywords (
  keyword_id BIGINT PRIMARY KEY,
  ad_id BIGINT,
  created_at TIMESTAMPTZ,
  criterion_name TEXT,
  cpc_bid_amount NUMERIC(10,2),
  period_id BIGINT,
  system_serving_status TEXT,
  bidding_strategy_type TEXT,
  quality_score BIGINT,
  keyword_match_type TEXT
);

CREATE TABLE IF NOT EXISTS %s.ad_events (
  id BIGINT PRIMARY KEY,
  keyword_id BIGINT,
  amount NUMERIC(10,2),
  device_type TEXT,
  event_type TEXT,
  created_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS %s.discounts (
  product_id BIGINT,
  inventory_item_id BIGINT PRIMARY KEY,
  retail_price NUMERIC(10,2),
  discount_price NUMERIC(10,2),
  discount_amount NUMERIC(10,2),
  date TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS %s.combined_orders_users (
  user_id BIGINT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS %s.transaction_detail (
  order_id BIGINT PRIMARY KEY,
  status TEXT,
  created_at TIMESTAMPTZ,
  shipped_at TIMESTAMPTZ,
  delivered_at TIMESTAMPTZ,
  items JSONB,
  user_data JSONB
);

CREATE TABLE IF NOT EXISTS %s.retail_calendar_454 (
  reference_date DATE PRIMARY KEY,
  fiscal_year TEXT,
  fiscal_year_num BIGINT,
  fiscal_quarter_of_year TEXT,
  fiscal_quarter_of_year_num BIGINT,
  fiscal_week_of_year TEXT,
  fiscal_week_of_year_num BIGINT,
  fiscal_period_of_year TEXT,
  fiscal_period_of_year_num BIGINT,
  season TEXT,
  season_num BIGINT,
  prev_custom_date BIGINT,
  prev_custom_week BIGINT
);
`, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema)

	return sb.String()
}

func DropTablesDDL(schema string) string {
	if schema == "" {
		schema = "public"
	}
	var sb strings.Builder
	for _, tbl := range AllTables {
		fmt.Fprintf(&sb, "DROP TABLE IF EXISTS %s.%s CASCADE;\n", schema, tbl)
	}
	return sb.String()
}
