package postgres

import (
	"strings"
	"testing"

	"thelook-generator/internal/model"
)

func TestTableDDLAndDrop(t *testing.T) {
	ddl := TableDDL("ecomm")
	if !strings.Contains(ddl, "CREATE SCHEMA IF NOT EXISTS ecomm;") {
		t.Fatalf("expected CREATE SCHEMA in DDL, got: %s", ddl[:50])
	}
	for _, tbl := range AllTables {
		expected := "CREATE TABLE IF NOT EXISTS ecomm." + tbl
		if !strings.Contains(ddl, expected) {
			t.Errorf("missing table %s in DDL", tbl)
		}
	}

	drop := DropTablesDDL("ecomm")
	for _, tbl := range AllTables {
		expected := "DROP TABLE IF EXISTS ecomm." + tbl
		if !strings.Contains(drop, expected) {
			t.Errorf("missing drop for table %s in drop DDL", tbl)
		}
	}
}

func TestRowToSQLMapping(t *testing.T) {
	s := &Sink{schema: "ecomm"}

	samples := []any{
		model.DistributionCenterRow{ID: 1, Name: "DC1", Latitude: 40.0, Longitude: -73.0},
		model.ProductRow{ID: 10, Cost: 25.0, Category: "Jeans", Name: "Denim", Brand: "BrandA", RetailPrice: 50.0, Department: "Men", SKU: "SKU1", DistributionCenterID: "1"},
		model.UserRow{ID: 100, FirstName: "John", LastName: "Doe", Email: "john@example.com", Age: 30, City: "NYC", State: "NY", Country: "USA", Zip: "10001", Latitude: 40.0, Longitude: -73.0, Gender: "Male", CreatedAt: "2026-01-01T00:00:00Z", TrafficSource: "Search"},
		model.InventoryItemRow{ID: 500, ProductID: 10, CreatedAt: "2026-01-01", Cost: 25.0, ProductCategory: "Jeans", ProductName: "Denim", ProductBrand: "BrandA", ProductRetailPrice: 50.0, ProductDepartment: "Men", ProductSKU: "SKU1", ProductDistributionCenterID: 1},
		model.OrderItemRow{ID: 1000, OrderID: 200, UserID: 100, InventoryItemID: 500, SalePrice: 50.0, Status: "Processing", CreatedAt: "2026-01-01T00:00:00Z"},
		model.EventRow{ID: 9000, SequenceNumber: 1, SessionID: "sess-1", CreatedAt: "2026-01-01T00:00:00Z", IPAddress: "127.0.0.1", City: "NYC", State: "NY", Country: "USA", Zip: "10001", Latitude: 40.0, Longitude: -73.0, OS: "Mac", Browser: "Chrome", TrafficSource: "Search", URI: "/home", EventType: "Home"},
		model.CampaignRow{ID: 1, AdvertisingChannel: "Search", Amount: 100.0, BidType: "CPC", CampaignName: "Campaign 1", Period: "30", CreatedAt: "2026-01-01"},
		model.AdGroupRow{AdID: 2, CampaignID: 1, CreatedAt: "2026-01-01", Name: "Group 1", Period: 30, AdType: "Text", Headline: "Headline"},
		model.KeywordRow{KeywordID: 3, AdID: 2, CreatedAt: "2026-01-01", CriterionName: "kw", CPCBidAmount: 5.0, SystemServingStatus: "Eligible", BiddingStrategyType: "CPC", QualityScore: 8, KeywordMatchType: "Exact"},
		model.AdEventRow{ID: 4, KeywordID: 3, Amount: 1.5, DeviceType: "Mobile", EventType: "Click", CreatedAt: "2026-01-01"},
		model.DiscountRow{ProductID: 10, InventoryItemID: 500, RetailPrice: 50.0, DiscountPrice: 40.0, DiscountAmount: 0.2, Date: "2026-01-01"},
		model.CombinedOrdersUsersRow{UserID: 100},
		model.TransactionDetailRow{
			OrderID:   200,
			Status:    "Processing",
			CreatedAt: "2026-01-01T00:00:00Z",
			Items:     []model.TxItem{{InventoryItemID: 500, SalePrice: 50.0}},
			User:      model.TxUser{UserID: 100, Name: "JOHN DOE", Email: "john@example.com"},
		},
		model.RetailCalendar454Row{ReferenceDate: "2026-01-01", FiscalYear: "2026", FiscalYearNum: 2026},
	}

	for _, sample := range samples {
		sql, args, err := s.rowToSQL(sample)
		if err != nil {
			t.Fatalf("rowToSQL failed for %T: %v", sample, err)
		}
		if sql == "" || len(args) == 0 {
			t.Fatalf("expected non-empty SQL and args for %T", sample)
		}
	}
}

func TestResolveConnString(t *testing.T) {
	if got := ResolveConnString("postgres://custom"); got != "postgres://custom" {
		t.Errorf("expected explicit conn string, got: %s", got)
	}
	t.Setenv("DATABASE_URL", "postgres://from-db-url")
	if got := ResolveConnString(""); got != "postgres://from-db-url" {
		t.Errorf("expected DATABASE_URL, got: %s", got)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ALLOYDB_URL", "postgres://from-alloydb-url")
	if got := ResolveConnString(""); got != "postgres://from-alloydb-url" {
		t.Errorf("expected ALLOYDB_URL, got: %s", got)
	}
}
