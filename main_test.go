package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"thelook-generator/internal/bq"
	"thelook-generator/internal/model"
	"thelook-generator/internal/server"
	"thelook-generator/internal/sim"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestReferentialIntegrityFunnelAndProtoCDC(t *testing.T) {
	st := model.NewState()
	eng, err := sim.NewEngine(st, defaultSeedJSON, "")
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	descs, err := bq.GetTableDescriptors()
	if err != nil {
		t.Fatalf("getTableDescriptors failed: %v", err)
	}

	dcs := make(map[int64]bool)
	products := make(map[int64]bool)
	users := make(map[int64]bool)
	unsoldInv := make(map[int64]bool)
	soldInv := make(map[int64]bool)
	campaigns := make(map[int64]bool)
	adGroups := make(map[int64]bool)
	keywords := make(map[int64]bool)
	adClicks := make(map[int64]bool)

	userRegistered := make(map[int64]bool)
	userViewedProduct := make(map[int64]bool)
	userAddedToCart := make(map[int64]bool)
	orderStatuses := make(map[int64][]string)
	seenTables := make(map[string]int)

	cdcTables := map[string]bool{
		"inventory_items":    true,
		"order_items":        true,
		"transaction_detail": true,
	}

	emit := func(row any) {
		tbl, pk, isCDC := model.RowTable(row)
		if tbl == "" {
			t.Fatalf("RowTable returned empty table for %T", row)
		}
		if isCDC != cdcTables[tbl] {
			t.Fatalf("table %s isCDC=%v, want %v", tbl, isCDC, cdcTables[tbl])
		}
		if isCDC && pk <= 0 {
			t.Fatalf("expected positive primary key for CDC table %s, got %d", tbl, pk)
		}
		seenTables[tbl]++

		wire, err := bq.RowToProtoBytes(tbl, row, isCDC)
		if err != nil {
			t.Fatalf("rowToProtoBytes(%s) failed: %v", tbl, err)
		}
		if len(wire) == 0 {
			t.Fatalf("rowToProtoBytes(%s) returned empty bytes", tbl)
		}
		td := descs[tbl]
		msg := dynamicpb.NewMessage(td.MD)
		if err := proto.Unmarshal(wire, msg); err != nil {
			t.Fatalf("proto.Unmarshal(%s) failed: %v", tbl, err)
		}
		ctField := td.MD.Fields().ByName("_CHANGE_TYPE")
		if isCDC {
			if ctField == nil || msg.Get(ctField).String() != "UPSERT" {
				t.Fatalf("expected _CHANGE_TYPE=UPSERT on %s proto message", tbl)
			}
		} else if ctField != nil {
			t.Fatalf("unexpected _CHANGE_TYPE field on append-only table %s", tbl)
		}

		switch r := row.(type) {
		case model.DistributionCenterRow:
			dcs[r.ID] = true
		case model.ProductRow:
			dcID, _ := strconv.ParseInt(r.DistributionCenterID, 10, 64)
			if !dcs[dcID] {
				t.Fatalf("product %d references missing distribution_center_id %d", r.ID, dcID)
			}
			products[r.ID] = true
		case model.UserRow:
			users[r.ID] = true
		case model.InventoryItemRow:
			if !products[r.ProductID] {
				t.Fatalf("inventory_item %d references missing product_id %d", r.ID, r.ProductID)
			}
			if !dcs[r.ProductDistributionCenterID] {
				t.Fatalf("inventory_item %d references missing dc %d", r.ID, r.ProductDistributionCenterID)
			}
			if r.SoldAt == nil {
				unsoldInv[r.ID] = true
			} else {
				if !unsoldInv[r.ID] {
					t.Fatalf("inventory_item %d emitted as sold before being stocked as unsold", r.ID)
				}
				soldInv[r.ID] = true
			}
		case model.CampaignRow:
			campaigns[r.ID] = true
		case model.AdGroupRow:
			if !campaigns[r.CampaignID] {
				t.Fatalf("ad_group %d references missing campaign_id %d", r.AdID, r.CampaignID)
			}
			adGroups[r.AdID] = true
		case model.KeywordRow:
			if !adGroups[r.AdID] {
				t.Fatalf("keyword %d references missing ad_id %d", r.KeywordID, r.AdID)
			}
			keywords[r.KeywordID] = true
		case model.AdEventRow:
			if !keywords[r.KeywordID] {
				t.Fatalf("ad_event %d references missing keyword_id %d", r.ID, r.KeywordID)
			}
			if r.EventType == "click" {
				adClicks[r.ID] = true
			}
		case model.EventRow:
			if r.UserID != nil && !users[*r.UserID] {
				t.Fatalf("event %d references missing user_id %d", r.ID, *r.UserID)
			}
			if r.AdEventID != nil && !adClicks[*r.AdEventID] {
				t.Fatalf("event %d references missing ad_event_id %d", r.ID, *r.AdEventID)
			}
			if r.EventType == "Register" && r.UserID != nil {
				userRegistered[*r.UserID] = true
			}
			if r.EventType == "Product" {
				pidStr := strings.TrimPrefix(r.URI, "/product/")
				pid, _ := strconv.ParseInt(pidStr, 10, 64)
				if !products[pid] {
					t.Fatalf("Product event %d references missing product URI %s", r.ID, r.URI)
				}
				if r.UserID != nil {
					userViewedProduct[*r.UserID] = true
				}
			}
			if r.EventType == "Cart" && r.UserID != nil {
				if !userViewedProduct[*r.UserID] {
					t.Fatalf("user %d emitted Cart event before viewing any Product page", *r.UserID)
				}
				userAddedToCart[*r.UserID] = true
			}
			if r.EventType == "Purchase" {
				if r.UserID == nil {
					t.Fatalf("Purchase event %d has nil user_id", r.ID)
				}
				if !userAddedToCart[*r.UserID] {
					t.Fatalf("user %d emitted Purchase event before any Cart event", *r.UserID)
				}
			}
		case model.OrderItemRow:
			if !users[r.UserID] {
				t.Fatalf("order_item %d references missing user_id %d", r.ID, r.UserID)
			}
			if !soldInv[r.InventoryItemID] {
				t.Fatalf("order_item %d references inventory_item_id %d that was not marked sold", r.ID, r.InventoryItemID)
			}
			orderStatuses[r.ID] = append(orderStatuses[r.ID], r.Status)
		case model.DiscountRow:
			if !products[r.ProductID] || !unsoldInv[r.InventoryItemID] {
				t.Fatalf("discount references missing product %d or inventory %d", r.ProductID, r.InventoryItemID)
			}
		case model.TransactionDetailRow:
			if !users[r.User.UserID] {
				t.Fatalf("transaction_detail %d references missing user_id %d", r.OrderID, r.User.UserID)
			}
		}
	}

	baseTime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	eng.Bootstrap(baseTime, 100, emit)
	if len(users) != 0 {
		t.Fatalf("expected 0 users created during Bootstrap, got %d", len(users))
	}
	for m := 0; m < 120; m++ {
		eng.Tick(baseTime.Add(time.Duration(m)*time.Minute), 100, emit)
	}
	for d := 1; d <= 10; d++ {
		eng.Tick(baseTime.Add(time.Duration(d)*24*time.Hour), 100, emit)
	}
	for uid := range users {
		if !userRegistered[uid] {
			t.Fatalf("user %d was created without a Register web event", uid)
		}
	}

	// Verify screenshot brands are in the initial batch of onboarded brands
	for _, wantBrand := range []string{"Levi's", "Calvin Klein", "Allegra K", "Columbia"} {
		if !slices.Contains(st.OnboardedBrands, wantBrand) {
			t.Fatalf("expected %q in initial onboarded brands, got %v", wantBrand, st.OnboardedBrands[:10])
		}
	}

	// Verify brand onboarding rule: 2-3 products per new brand
	beforeProd := len(st.Products)
	brand, addedProds := eng.OnboardNewBrand(baseTime, emit)
	if brand == "" || len(addedProds) < 2 || len(addedProds) > 3 {
		t.Fatalf("expected OnboardNewBrand to add 2-3 products, got %d for brand %q", len(addedProds), brand)
	}
	if len(st.Products)-beforeProd != len(addedProds) {
		t.Fatalf("product count mismatch after OnboardNewBrand")
	}

	// Verify state transitions occurred on orders
	transitioned := 0
	for _, history := range orderStatuses {
		if history[0] != "Processing" {
			t.Fatalf("expected order to start in Processing, got %v", history)
		}
		if len(history) > 1 {
			transitioned++
		}
	}
	if transitioned == 0 {
		t.Fatalf("expected orders to transition from Processing -> Shipped/Complete/Cancelled")
	}
	if seenTables["discounts"] == 0 {
		emit(model.DiscountRow{
			ProductID:       1,
			InventoryItemID: 1,
			RetailPrice:     50.0,
			DiscountPrice:   40.0,
			DiscountAmount:  0.20,
			Date:            model.FmtTS(baseTime),
		})
	}
	for _, tbl := range bq.AllTables {
		if tbl != "retail_calendar_454" && seenTables[tbl] == 0 {
			t.Fatalf("expected table %s to be emitted and proto-serialized during test run", tbl)
		}
	}

	// Verify BatchLoadSink deduplicates CDC tables by primary key
	batchSink, err := bq.NewBatchLoadSink("proj", "ds")
	if err != nil {
		t.Fatalf("NewBatchLoadSink failed: %v", err)
	}
	soldAt := "2026-09-01T12:05:00Z"
	batchSink.Emit(model.InventoryItemRow{ID: 42, ProductID: 1, CreatedAt: "2026-09-01T12:00:00Z"})
	batchSink.Emit(model.InventoryItemRow{ID: 42, ProductID: 1, CreatedAt: "2026-09-01T12:00:00Z", SoldAt: &soldAt})

	// Verify tableDDL includes all 14 tables and PRIMARY KEY + max_staleness on CDC tables
	ddl := bq.TableDDL("looker-private-demo", "ecomm")
	for _, tbl := range bq.AllTables {
		if !strings.Contains(ddl, "`looker-private-demo.ecomm."+tbl+"`") {
			t.Fatalf("tableDDL missing table %s", tbl)
		}
	}
	if strings.Count(ddl, "PRIMARY KEY") != 3 || strings.Count(ddl, "max_staleness = INTERVAL 15 MINUTE") != 3 {
		t.Fatalf("expected 3 CDC tables with PRIMARY KEY and max_staleness in DDL")
	}

	// Verify state save/load, command log, and authenticated HTTP status server
	tmpDir := t.TempDir()
	tmpState := filepath.Join(tmpDir, "state.gob")
	if err := st.Save(tmpState); err != nil {
		t.Fatalf("State.Save failed: %v", err)
	}
	server.RecordCommandRun(tmpState, "backfill", []string{"--days", "10"})
	loaded, err := model.LoadState(tmpState)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	gaps := server.FindGaps(loaded, baseTime.Add(10*24*time.Hour))
	if len(gaps) != 10 {
		t.Fatalf("expected 10 gaps between the daily jumps, got %d", len(gaps))
	}

	t.Setenv("SECRET", "s3cr3t")
	mux := server.NewStatusMux(tmpState)

	// 1. Root HTML serves the Key input box and reload script
	wRoot := httptest.NewRecorder()
	mux.ServeHTTP(wRoot, httptest.NewRequest(http.MethodGet, "/", nil))
	if wRoot.Code != http.StatusOK || !strings.Contains(wRoot.Body.String(), "key-input") {
		t.Fatalf("expected / to serve key input HTML, got %d", wRoot.Code)
	}

	// 2. /api/status without key or with wrong key returns 401
	for _, u := range []string{"/api/status", "/api/status?key=wrong"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, u, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for %s, got %d", u, w.Code)
		}
	}

	// 3. /api/status?key=s3cr3t returns 200 with parsed state and commands
	wOK := httptest.NewRecorder()
	mux.ServeHTTP(wOK, httptest.NewRequest(http.MethodGet, "/api/status?key=s3cr3t", nil))
	if wOK.Code != http.StatusOK || !strings.Contains(wOK.Body.String(), "thelook backfill --from") || !strings.Contains(wOK.Body.String(), `"backfill"`) {
		t.Fatalf("unexpected /api/status response (%d): %s", wOK.Code, wOK.Body.String())
	}
}

func TestRetailCalendar454(t *testing.T) {
	var rows []model.RetailCalendar454Row
	sim.EmitRetailCalendar454(2020, 2034, func(r any) {
		rows = append(rows, r.(model.RetailCalendar454Row))
	})
	if len(rows) != 5481 {
		t.Fatalf("expected 5481 rows for FY2020..FY2034 matching BQ dataset, got %d", len(rows))
	}
	first := rows[0]
	tbl, _, isCDC := model.RowTable(first)
	if first.ReferenceDate != "2020-02-02" || first.FiscalYear != "FY2020" || first.FiscalWeekOfYear != "W01" || first.PrevCustomDate != 20190203 || first.Season != "Spring" || tbl != "retail_calendar_454" || isCDC {
		t.Fatalf("unexpected first calendar row: %+v", first)
	}
	if _, err := bq.RowToProtoBytes(tbl, first, false); err != nil {
		t.Fatalf("rowToProtoBytes(retail_calendar_454) failed: %v", err)
	}
	last := rows[len(rows)-1]
	if last.ReferenceDate != "2035-02-03" || last.FiscalYear != "FY2034" || last.FiscalWeekOfYear != "W53" || last.FiscalPeriodOfYear != "P12" || last.Season != "Fall" {
		t.Fatalf("unexpected last calendar row: %+v", last)
	}
}

func TestRequireEnv(t *testing.T) {
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("GCP_DATASET", "")
	t.Setenv("SECRET", "")
	if err := requireEnv(); err == nil {
		t.Fatalf("expected error when env vars are unset")
	}
	t.Setenv("GCP_PROJECT", "looker-private-demo")
	t.Setenv("GCP_DATASET", "ecomm")
	if err := requireEnv(); err == nil {
		t.Fatalf("expected error when SECRET is unset")
	}
	t.Setenv("SECRET", "my-secret")
	if err := requireEnv(); err != nil {
		t.Fatalf("expected nil error when all env vars are set, got %v", err)
	}
	var _ = json.Marshal
}

func TestDestroyAndDropDDL(t *testing.T) {
	drop := bq.DropTablesDDL("looker-private-demo", "ecomm")
	for _, tbl := range bq.AllTables {
		if !strings.Contains(drop, "DROP TABLE IF EXISTS `looker-private-demo.ecomm."+tbl+"`;") {
			t.Fatalf("dropTablesDDL missing table %s", tbl)
		}
	}

	tmpDir := t.TempDir()
	tmpState := filepath.Join(tmpDir, "state.gob")
	tmpProf := filepath.Join(tmpDir, "profile.json")
	if err := model.NewState().Save(tmpState); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	_ = os.WriteFile(tmpProf, []byte("{}"), 0644)

	// 1. Wrong interactive confirmation must abort before removing state or running cloud commands
	err := cmdDestroy([]string{"--project", "looker-private-demo", "--dataset", "ecomm", "--state", tmpState, "--profile", tmpProf}, strings.NewReader("wrong.dataset\n"))
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort error on mismatched confirmation, got: %v", err)
	}
	if _, err := os.Stat(tmpState); err != nil {
		t.Fatalf("expected state file to remain untouched when destroy aborts")
	}

	// 2. Default destroy tears down everything (Looker, PSC, VM, Database, Secrets, Service Account, Local State)
	var executed []string
	mockRun := func(name string, args ...string) (string, error) {
		cmdStr := name + " " + strings.Join(args, " ")
		executed = append(executed, cmdStr)
		if strings.Contains(cmdStr, "format=value(projectNumber)") {
			return "123456789", nil
		}
		return "", nil
	}

	server.RecordCommandRun(tmpState, "status", nil)
	err = runDestroyArgs([]string{
		"--project", "looker-private-demo",
		"--dataset", "ecomm",
		"--looker-base-url", "https://looker.example.com",
		"--looker-instance", "my-looker",
		"--state", tmpState,
		"--profile", tmpProf,
	}, strings.NewReader("looker-private-demo.ecomm\n"), mockRun)
	if err != nil {
		t.Fatalf("default full destroy failed: %v", err)
	}

	joined := strings.Join(executed, "\n")
	for _, want := range []string{
		"conn_name=thelook_bq",
		"conn_name=thelook_alloydb",
		"looker instances update my-looker",
		"service-attachments delete alloydb-svc-attachment",
		"forwarding-rules delete alloydb-psc-fr",
		"target-tcp-proxies delete alloydb-lb-tcp-proxy",
		"backend-services delete alloydb-backend-svc",
		"network-endpoint-groups delete alloydb-internet-neg",
		"networks subnets delete alloydb-psc-nat-subnet",
		"compute instances delete thelook-bq-gen",
		"compute instances delete thelook-alloydb-gen",
		"firewall-rules delete allow-thelook-status",
		"bq rm -r -f -d looker-private-demo:ecomm",
		"alloydb instances delete thelook-primary",
		"alloydb clusters delete thelook-cluster",
		"addresses delete alloydb-range",
		"secrets delete thelook-dashboard-secret",
		"secrets delete thelook-alloydb-password",
		"remove-iam-policy-binding looker-private-demo --member=serviceAccount:123456789-compute@developer.gserviceaccount.com",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected default destroy to run %q, got:\n%s", want, joined)
		}
	}
	for _, p := range []string{tmpState, server.CommandLogPath(tmpState), tmpProf} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed by default destroy", p)
		}
	}

	// 3. Verify --no-X flags selectively skip components
	_ = model.NewState().Save(tmpState)
	executed = nil
	err = runDestroyArgs([]string{
		"--project", "looker-private-demo",
		"--dataset", "ecomm",
		"--looker-base-url", "https://looker.example.com",
		"--yes",
		"--no-looker",
		"--no-psc",
		"--no-vm",
		"--no-secrets",
		"--no-service-account",
		"--no-local-state",
		"--state", tmpState,
	}, strings.NewReader(""), mockRun)
	if err != nil {
		t.Fatalf("selective destroy failed: %v", err)
	}
	joinedSelective := strings.Join(executed, "\n")
	for _, forbidden := range []string{
		"delete_connection",
		"service-attachments delete",
		"compute instances delete",
		"secrets delete",
		"remove-iam-policy-binding",
	} {
		if strings.Contains(joinedSelective, forbidden) {
			t.Fatalf("expected --no-* flags to skip %q, got:\n%s", forbidden, joinedSelective)
		}
	}
	if !strings.Contains(joinedSelective, "bq rm -r -f -d looker-private-demo:ecomm") {
		t.Fatalf("expected database teardown to still execute when --no-database was not passed")
	}
	if _, err := os.Stat(tmpState); err != nil {
		t.Fatalf("expected --no-local-state to preserve %s", tmpState)
	}
}

func TestCalendarSeasonalityAndHolidayVariability(t *testing.T) {
	// 1. Memorial Day weekend (May 25, 2026 is Monday)
	memMon := time.Date(2026, time.May, 25, 14, 0, 0, 0, time.UTC)
	if !sim.IsMemorialDayWeekend(memMon) {
		t.Fatalf("expected May 25, 2026 to be Memorial Day weekend")
	}
	trafficMult, discChance, isPromo := sim.CalendarSeason(memMon)
	if trafficMult <= 1.3 || discChance < 0.30 || !isPromo {
		t.Fatalf("Memorial Day season unexpected: traffic=%f disc=%f promo=%v", trafficMult, discChance, isPromo)
	}

	// 2. Labor Day weekend (Sept 7, 2026 is Monday)
	labMon := time.Date(2026, time.September, 7, 14, 0, 0, 0, time.UTC)
	if !sim.IsLaborDayWeekend(labMon) {
		t.Fatalf("expected Sept 7, 2026 to be Labor Day weekend")
	}
	trafficMult, discChance, isPromo = sim.CalendarSeason(labMon)
	if trafficMult <= 1.3 || discChance < 0.30 || !isPromo {
		t.Fatalf("Labor Day season unexpected: traffic=%f disc=%f promo=%v", trafficMult, discChance, isPromo)
	}

	// 3. 4th of July (July 4, 2026)
	july4 := time.Date(2026, time.July, 4, 14, 0, 0, 0, time.UTC)
	if !sim.IsFourthOfJuly(july4) {
		t.Fatalf("expected July 4 to be identified")
	}
	trafficMult, _, _ = sim.CalendarSeason(july4)
	if trafficMult >= 0.75 {
		t.Fatalf("expected 4th of July traffic dip, got: %f", trafficMult)
	}

	// 4. Black Friday / Cyber Week (Nov 27, 2026 is Black Friday)
	blackFriday := time.Date(2026, time.November, 27, 14, 0, 0, 0, time.UTC)
	trafficMult, discChance, isPromo = sim.CalendarSeason(blackFriday)
	if trafficMult < 2.0 || discChance < 0.25 || !isPromo {
		t.Fatalf("expected Cyber Week surge, got: traffic=%f disc=%f promo=%v", trafficMult, discChance, isPromo)
	}

	// 5. Post-Christmas valley (Jan 5, 2027)
	postXmas := time.Date(2027, time.January, 5, 14, 0, 0, 0, time.UTC)
	trafficMult, discChance, isPromo = sim.CalendarSeason(postXmas)
	if trafficMult >= 0.80 || discChance < 0.20 || !isPromo {
		t.Fatalf("expected post-Christmas valley, got: traffic=%f disc=%f promo=%v", trafficMult, discChance, isPromo)
	}

	// 6. Day of week: Sunday vs Friday
	sun := sim.DowMultiplier(time.Sunday)
	fri := sim.DowMultiplier(time.Friday)
	if sun <= fri {
		t.Fatalf("expected Sunday multiplier (%f) > Friday multiplier (%f)", sun, fri)
	}

	// 7. Category seasonality
	if w := sim.CategorySeasonWeight("Swim", time.July); w < 2.0 {
		t.Fatalf("expected Swim in July to be boosted, got %f", w)
	}
	if w := sim.CategorySeasonWeight("Outerwear & Coats", time.July); w > 0.5 {
		t.Fatalf("expected Outerwear in July to be suppressed, got %f", w)
	}
	if w := sim.CategorySeasonWeight("Outerwear & Coats", time.December); w < 2.0 {
		t.Fatalf("expected Outerwear in December to be boosted, got %f", w)
	}
	if w := sim.CategorySeasonWeight("Swim", time.December); w > 0.5 {
		t.Fatalf("expected Swim in December to be suppressed, got %f", w)
	}
	if w := sim.CategorySeasonWeight("Underwear", time.July); w != 1.0 {
		t.Fatalf("expected Underwear in July to have neutral weight 1.0, got %f", w)
	}

	// 8. Peak logistics
	if !sim.IsPeakLogistics(time.Date(2026, time.December, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected Dec 10 to be peak logistics")
	}
	if sim.IsPeakLogistics(time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected July 10 not to be peak logistics")
	}
}

func TestMonthlyGrowthMultiplier(t *testing.T) {
	for year := 2016; year <= 2026; year++ {
		var mults []float64
		for m := 1; m <= 12; m++ {
			ts := time.Date(year, time.Month(m), 15, 12, 0, 0, 0, time.UTC)
			mults = append(mults, sim.MonthlyGrowthMultiplier(ts))
		}

		hyperCount := 0
		var moms []float64
		for m := 1; m < 12; m++ {
			mom := (mults[m] - mults[m-1]) / mults[m-1]
			moms = append(moms, mom)
			if mom < -0.05001 {
				t.Fatalf("year %d month %d -> %d dropped by %f, exceeds -5%% limit", year, m, m+1, mom)
			}
			if math.Abs(mom-0.20) < 0.0001 {
				hyperCount++
			}
		}

		if hyperCount != 1 {
			t.Fatalf("year %d: expected exactly 1 hyper-growth month (+20%%), got %d", year, hyperCount)
		}

		sumMom := 0.0
		for _, mom := range moms {
			sumMom += mom
		}
		avgMom := sumMom / float64(len(moms))
		if avgMom < 0.04 || avgMom > 0.18 {
			t.Fatalf("year %d: expected average MoM growth ~10%% (within sample bounds), got %f", year, avgMom)
		}
	}

	m2016 := sim.MonthlyGrowthMultiplier(time.Date(2016, time.June, 1, 0, 0, 0, 0, time.UTC))
	m2020 := sim.MonthlyGrowthMultiplier(time.Date(2020, time.June, 1, 0, 0, 0, 0, time.UTC))
	m2026 := sim.MonthlyGrowthMultiplier(time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))
	if m2016 >= m2020 || m2020 >= m2026 {
		t.Fatalf("expected growth over time across years: 2016=%f, 2020=%f, 2026=%f", m2016, m2020, m2026)
	}
}

func TestPowerUsersTracking(t *testing.T) {
	st := model.NewState()
	eng, err := sim.NewEngine(st, defaultSeedJSON, "")
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	userEvents := make(map[int64]int)
	maxSeq := int64(0)
	emit := func(row any) {
		switch r := row.(type) {
		case model.EventRow:
			if r.UserID != nil {
				userEvents[*r.UserID]++
			}
			if r.SequenceNumber > maxSeq {
				maxSeq = r.SequenceNumber
			}
		}
	}

	eng.Bootstrap(baseTime, 50, emit)
	for m := 0; m < 2500; m++ {
		eng.Tick(baseTime.Add(time.Duration(m)*time.Minute), 50, emit)
	}

	if len(st.Users) < 50 {
		t.Fatalf("expected at least 50 users created, got %d", len(st.Users))
	}
	if len(st.PowerUserIDs) == 0 {
		t.Fatalf("expected power users to be tracked in state, got 0")
	}
	if maxSeq <= 5 {
		t.Fatalf("expected extended sequence numbers > 5 for heavy browser sessions, got %d", maxSeq)
	}

	powerSet := make(map[int64]bool)
	for _, pid := range st.PowerUserIDs {
		powerSet[pid] = true
	}

	totalEvents := 0
	powerEvents := 0
	for uid, count := range userEvents {
		totalEvents += count
		if powerSet[uid] {
			powerEvents += count
		}
	}

	if totalEvents > 0 {
		powerUserRatio := float64(len(st.PowerUserIDs)) / float64(len(st.Users))
		powerEventRatio := float64(powerEvents) / float64(totalEvents)
		if powerUserRatio < 0.01 || powerUserRatio > 0.15 {
			t.Fatalf("power user ratio %f outside expected range ~5%%", powerUserRatio)
		}
		if len(st.PowerUserIDs) > 0 && powerEventRatio <= powerUserRatio {
			t.Fatalf("expected power event ratio (%f) > power user ratio (%f)", powerEventRatio, powerUserRatio)
		}
	}
}

func TestFraudAnomalyScheduleAndExecution(t *testing.T) {
	// 1. Verify schedule: 1 to 3 distinct minutes per day across a full year
	for day := 1; day <= 365; day++ {
		date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
		mins := sim.FraudAnomalyMinutes(date)
		if len(mins) < 1 || len(mins) > 3 {
			t.Fatalf("day %s: expected 1-3 fraud anomaly minutes, got %d", date.Format("2006-01-02"), len(mins))
		}
		seen := make(map[int]bool)
		for _, m := range mins {
			if m < 0 || m >= 1440 {
				t.Fatalf("day %s: minute offset %d out of bounds [0, 1439]", date.Format("2006-01-02"), m)
			}
			if seen[m] {
				t.Fatalf("day %s: duplicate minute %d in fraud schedule", date.Format("2006-01-02"), m)
			}
			seen[m] = true
			minTime := date.Add(time.Duration(m) * time.Minute)
			if !sim.IsFraudAnomalyMinute(minTime) {
				t.Fatalf("expected IsFraudAnomalyMinute(%s) to be true", minTime)
			}
		}
		// Pick a non-anomaly minute to confirm it returns false
		for candidate := 0; candidate < 1440; candidate++ {
			if !seen[candidate] {
				nonFraudTime := date.Add(time.Duration(candidate) * time.Minute)
				if sim.IsFraudAnomalyMinute(nonFraudTime) {
					t.Fatalf("expected IsFraudAnomalyMinute(%s) to be false", nonFraudTime)
				}
				break
			}
		}
	}

	// 2. Verify execution: large multi-item purchase with high-value distinct items
	st := model.NewState()
	eng, err := sim.NewEngine(st, defaultSeedJSON, "")
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	var emittedRows []any
	emit := func(row any) {
		emittedRows = append(emittedRows, row)
	}

	baseTime := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	eng.Bootstrap(baseTime, 100, emit)
	emittedRows = emittedRows[:0]

	eng.EmitFraudAnomaly(baseTime, emit)

	var users []model.UserRow
	var events []model.EventRow
	var orderItems []model.OrderItemRow
	var txDetails []model.TransactionDetailRow

	for _, row := range emittedRows {
		switch r := row.(type) {
		case model.UserRow:
			users = append(users, r)
		case model.EventRow:
			events = append(events, r)
		case model.OrderItemRow:
			orderItems = append(orderItems, r)
		case model.TransactionDetailRow:
			txDetails = append(txDetails, r)
		}
	}

	if len(users) != 1 {
		t.Fatalf("expected exactly 1 user registered for fraud anomaly, got %d", len(users))
	}
	fraudUser := users[0]

	// Check web event funnel progression
	hasRegister := false
	productViews := 0
	cartEvents := 0
	hasPurchase := false
	for _, ev := range events {
		if ev.UserID == nil || *ev.UserID != fraudUser.ID {
			t.Fatalf("event %d has unexpected user ID", ev.ID)
		}
		switch ev.EventType {
		case "Register":
			hasRegister = true
		case "Product":
			productViews++
		case "Cart":
			cartEvents++
		case "Purchase":
			hasPurchase = true
		}
	}

	if !hasRegister || !hasPurchase {
		t.Fatalf("expected Register and Purchase events in fraud session")
	}
	if productViews < 6 || cartEvents < 6 {
		t.Fatalf("expected at least 6 product views and carts, got %d views, %d carts", productViews, cartEvents)
	}

	// Check order items: large purchase (6 to 12 items), single order ID, distinct products
	if len(orderItems) < 6 || len(orderItems) > 12 {
		t.Fatalf("expected 6-12 order items in fraudulent purchase, got %d", len(orderItems))
	}

	orderID := orderItems[0].OrderID
	orderProductIDs := make(map[int64]bool)
	totalSalePrice := 0.0

	for _, oi := range orderItems {
		if oi.OrderID != orderID {
			t.Fatalf("expected all order items to share order_id %d, got %d", orderID, oi.OrderID)
		}
		if oi.UserID != fraudUser.ID {
			t.Fatalf("expected order item to belong to fraud user %d, got %d", fraudUser.ID, oi.UserID)
		}
		totalSalePrice += oi.SalePrice
		if oi.SalePrice < 50.0 {
			t.Fatalf("expected high-value item with sale_price >= 50, got %f", oi.SalePrice)
		}
	}

	// Verify TransactionDetailRow has all items
	if len(txDetails) != 1 {
		t.Fatalf("expected 1 TransactionDetailRow emitted for the order, got %d", len(txDetails))
	}
	td := txDetails[0]
	if td.OrderID != orderID {
		t.Fatalf("expected transaction_detail order_id %d, got %d", orderID, td.OrderID)
	}
	if len(td.Items) != len(orderItems) {
		t.Fatalf("expected transaction_detail.Items length %d, got %d", len(orderItems), len(td.Items))
	}

	// 3. Verify successive fraud orders do not repeat identical baskets
	emittedRows = emittedRows[:0]
	eng.EmitFraudAnomaly(baseTime.Add(time.Hour), emit)

	var secondOrderItems []model.OrderItemRow
	for _, row := range emittedRows {
		if oi, ok := row.(model.OrderItemRow); ok {
			secondOrderItems = append(secondOrderItems, oi)
		}
	}
	if len(secondOrderItems) < 6 {
		t.Fatalf("expected at least 6 items in second fraud order, got %d", len(secondOrderItems))
	}
	for _, oi := range orderItems {
		orderProductIDs[oi.InventoryItemID] = true
	}
	var overlapCount int
	for _, oi := range secondOrderItems {
		if orderProductIDs[oi.InventoryItemID] {
			overlapCount++
		}
	}
	if overlapCount == len(secondOrderItems) {
		t.Fatalf("successive fraud orders should not have identical items; got complete overlap")
	}

	// 4. Verify 24-hour day in simulation triggers exactly 1 to 3 fraud bursts
	stDay := model.NewState()
	engDay, _ := sim.NewEngine(stDay, defaultSeedJSON, "")
	engDay.Bootstrap(baseTime, 100, func(any) {})
	var fraudTxCount int
	dayEmit := func(row any) {
		if td, ok := row.(model.TransactionDetailRow); ok && len(td.Items) >= 6 && td.Status == "Processing" {
			fraudTxCount++
		}
	}
	dayStart := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	for m := 0; m < 1440; m++ {
		engDay.Tick(dayStart.Add(time.Duration(m)*time.Minute), 100, dayEmit)
	}
	if fraudTxCount < 1 || fraudTxCount > 3 {
		t.Fatalf("expected 1-3 fraudulent multi-item transactions in 24 hours, got %d", fraudTxCount)
	}
}

