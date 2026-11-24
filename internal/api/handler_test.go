package api_test

import (
    "bytes"
    "context"
    "encoding/json"
    "io"
    "log/slog"
    "net/http"
    "net/http/httptest"
    "os"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/api"
    "github.com/ankit5841/erp-inventory-service/internal/domain"
    "github.com/ankit5841/erp-inventory-service/internal/store"
)

func newTestServer(apiKey string) http.Handler {
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    st := store.NewMemoryStore()
    return api.NewServer(st, logger, apiKey, "memory", "*").Handler()
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
    t.Helper()
    var buf bytes.Buffer
    if body != nil {
        if err := json.NewEncoder(&buf).Encode(body); err != nil {
            t.Fatalf("encode body: %v", err)
        }
    }
    req := httptest.NewRequest(method, path, &buf)
    if body != nil {
        req.Header.Set("Content-Type", "application/json")
    }
    for k, v := range headers {
        req.Header.Set(k, v)
    }
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    return rec
}

func TestHealthAndObservabilityHeaders(t *testing.T) {
    h := newTestServer("")
    rec := doJSON(t, h, http.MethodGet, "/health", nil, nil)
    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", rec.Code)
    }
    if rec.Header().Get("X-Request-ID") == "" {
        t.Fatal("missing X-Request-ID header")
    }
    if rec.Header().Get("X-Process-Time-Ms") == "" {
        t.Fatal("missing X-Process-Time-Ms header")
    }
}

func TestNetworkOverviewCovers23Plants(t *testing.T) {
    h := newTestServer("")
    rec := doJSON(t, h, http.MethodGet, "/api/v1/overview", nil, nil)
    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", rec.Code)
    }
    var ov domain.NetworkOverview
    if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
        t.Fatalf("decode overview: %v", err)
    }
    if ov.TotalPlants != 23 || ov.ActivePlants != 23 {
        t.Fatalf("expected 23 active plants, got total=%d active=%d", ov.TotalPlants, ov.ActivePlants)
    }
    if ov.LowStockAlerts < 3 {
        t.Fatalf("expected at least 3 seeded low-stock alerts, got %d", ov.LowStockAlerts)
    }
}

func TestPlantInventoryAndLowStockFilter(t *testing.T) {
    h := newTestServer("")
    rec := doJSON(t, h, http.MethodGet, "/api/v1/plants/PLANT-03/inventory?low_stock=true", nil, nil)
    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", rec.Code)
    }
    var resp struct {
        Count int                    `json:"count"`
        Items []domain.InventoryItem `json:"items"`
    }
    if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
        t.Fatalf("decode inventory: %v", err)
    }
    if resp.Count != 1 || resp.Items[0].SKU != "DSP-RED-60" || !resp.Items[0].LowStock {
        t.Fatalf("unexpected low-stock response for PLANT-03: %+v", resp)
    }
}

func TestAdjustStockOptimisticLockAndIdempotency(t *testing.T) {
    h := newTestServer("")
    v1 := int64(1)

    payload := map[string]any{
        "delta_kg":         -100.0,
        "movement_type":    "PRODUCTION_BATCH",
        "reference_id":     "BATCH-9001",
        "expected_version": v1,
    }
    headers := map[string]string{"Idempotency-Key": "idemp-batch-9001"}
    first := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", payload, headers)
    if first.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
    }
    var res1 domain.AdjustResult
    _ = json.Unmarshal(first.Body.Bytes(), &res1)
    if res1.Item.QuantityKg != 1175.0 || res1.Item.Version != 2 || res1.IdempotentReplay {
        t.Fatalf("unexpected first adjust result: %+v", res1)
    }

    // Duplicate retry replays without deducting again
    replay := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", payload, headers)
    if replay.Code != http.StatusOK {
        t.Fatalf("expected 200 on replay, got %d", replay.Code)
    }
    var res2 domain.AdjustResult
    _ = json.Unmarshal(replay.Body.Bytes(), &res2)
    if !res2.IdempotentReplay || res2.Item.QuantityKg != 1175.0 {
        t.Fatalf("expected idempotent replay at 1175kg, got %+v", res2)
    }

    // Reusing the same Idempotency-Key on /transfers must return 409 Conflict
    crossOp := doJSON(t, h, http.MethodPost, "/api/v1/transfers", map[string]any{
        "from_plant_id": "PLANT-22",
        "to_plant_id":   "PLANT-03",
        "sku":           "AUX-DISP-NNO",
        "quantity_kg":   10.0,
    }, headers)
    if crossOp.Code != http.StatusConflict {
        t.Fatalf("expected 409 Conflict on cross-operation idempotency key reuse, got %d", crossOp.Code)
    }

    // Stale expected_version=1 must return 409 Conflict
    stale := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", payload, nil)
    if stale.Code != http.StatusConflict {
        t.Fatalf("expected 409 Conflict on stale version, got %d", stale.Code)
    }
}

func TestAtomicInterPlantTransfer(t *testing.T) {
    h := newTestServer("")

    req := map[string]any{
        "from_plant_id": "PLANT-22",
        "to_plant_id":   "PLANT-03",
        "sku":           "AUX-DISP-NNO",
        "quantity_kg":   200.0,
        "reference_id":  "REQ-P03-77",
        "notes":         "Replenish Disperse Block unit 03",
    }
    rec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", req, map[string]string{"Idempotency-Key": "trf-001"})
    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
    }
    var res domain.TransferResult
    _ = json.Unmarshal(rec.Body.Bytes(), &res)
    if res.FromItem.QuantityKg != 6060.0 || res.ToItem.QuantityKg != 860.0 {
        t.Fatalf("unexpected transfer balances: from=%.2f to=%.2f", res.FromItem.QuantityKg, res.ToItem.QuantityKg)
    }

    ledgerRec := doJSON(t, h, http.MethodGet, "/api/v1/ledger?limit=10", nil, nil)
    var ledgerResp struct {
        Count int `json:"count"`
    }
    _ = json.Unmarshal(ledgerRec.Body.Bytes(), &ledgerResp)
    if ledgerResp.Count != 2 {
        t.Fatalf("expected 2 ledger entries from transfer, got %d", ledgerResp.Count)
    }
}

func TestAPIKeyEnforcement(t *testing.T) {
    h := newTestServer("erp-prod-secret")
    body := map[string]any{"delta_kg": 10.0}

    unauth := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", body, nil)
    if unauth.Code != http.StatusUnauthorized {
        t.Fatalf("expected 401 without API key, got %d", unauth.Code)
    }

    auth := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", body, map[string]string{
        "X-API-Key": "erp-prod-secret",
    })
    if auth.Code != http.StatusOK {
        t.Fatalf("expected 200 with valid API key, got %d", auth.Code)
    }
}

func TestConcurrentStockDeductionsConsistency(t *testing.T) {
    h := newTestServer("")

    const workers = 50
    var (
        wg        sync.WaitGroup
        succeeded int32
        rejected  int32
    )
    wg.Add(workers)
    for i := 0; i < workers; i++ {
        go func() {
            defer wg.Done()
            rec := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", map[string]any{
                "delta_kg":      -50.0,
                "movement_type": "CONCURRENT_BATCH",
            }, nil)
            switch rec.Code {
            case http.StatusOK:
                atomic.AddInt32(&succeeded, 1)
            case http.StatusUnprocessableEntity:
                atomic.AddInt32(&rejected, 1)
            default:
                t.Errorf("unexpected status code under concurrency: %d", rec.Code)
            }
        }()
    }
    wg.Wait()

    if succeeded != 25 || rejected != 25 {
        t.Fatalf("expected 25 succeeded & 25 rejected, got succeeded=%d rejected=%d", succeeded, rejected)
    }

    check := doJSON(t, h, http.MethodGet, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60", nil, nil)
    var item domain.InventoryItem
    _ = json.Unmarshal(check.Body.Bytes(), &item)
    if item.QuantityKg != 25.0 || item.Version != 26 {
        t.Fatalf("expected final balance 25.0kg (v26), got %.2fkg (v%d)", item.QuantityKg, item.Version)
    }
}

func TestListPlantsAndCrossPlantSKULookup(t *testing.T) {
    h := newTestServer("")

    plantsRec := doJSON(t, h, http.MethodGet, "/api/v1/plants", nil, nil)
    if plantsRec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", plantsRec.Code)
    }
    var plantsResp struct {
        Count int `json:"count"`
    }
    _ = json.Unmarshal(plantsRec.Body.Bytes(), &plantsResp)
    if plantsResp.Count != 23 {
        t.Fatalf("expected 23 plants, got %d", plantsResp.Count)
    }

    skuRec := doJSON(t, h, http.MethodGet, "/api/v1/inventory/sku/AUX-DISP-NNO", nil, nil)
    if skuRec.Code != http.StatusOK {
        t.Fatalf("expected 200 for AUX-DISP-NNO lookup, got %d", skuRec.Code)
    }
    var skuResp struct {
        PlantsCount int     `json:"plants_count"`
        TotalKg     float64 `json:"total_quantity_kg"`
    }
    _ = json.Unmarshal(skuRec.Body.Bytes(), &skuResp)
    if skuResp.PlantsCount != 11 || skuResp.TotalKg <= 0 {
        t.Fatalf("expected AUX-DISP-NNO across 11 plants, got %+v", skuResp)
    }
}

func TestLowStockAlertsEndpoint(t *testing.T) {
    h := newTestServer("")
    rec := doJSON(t, h, http.MethodGet, "/api/v1/alerts/low-stock", nil, nil)
    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", rec.Code)
    }
    var resp struct {
        Count int `json:"count"`
    }
    _ = json.Unmarshal(rec.Body.Bytes(), &resp)
    if resp.Count != 3 {
        t.Fatalf("expected 3 seeded low-stock alerts across 23 units, got %d", resp.Count)
    }
}

func TestTransferEdgeCasesAndNewPlantProvisioning(t *testing.T) {
    h := newTestServer("")

    sameRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", map[string]any{
        "from_plant_id": "PLANT-01",
        "to_plant_id":   "PLANT-01",
        "sku":           "DSP-RED-60",
        "quantity_kg":   10.0,
    }, nil)
    if sameRec.Code != http.StatusBadRequest {
        t.Fatalf("expected 400 for same-plant transfer, got %d", sameRec.Code)
    }

    overRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", map[string]any{
        "from_plant_id": "PLANT-16",
        "to_plant_id":   "PLANT-10",
        "sku":           "INT-VS-BASE",
        "quantity_kg":   999999.0,
    }, nil)
    if overRec.Code != http.StatusUnprocessableEntity {
        t.Fatalf("expected 422 for insufficient stock, got %d", overRec.Code)
    }

    payload := map[string]any{
        "from_plant_id": "PLANT-16",
        "to_plant_id":   "PLANT-10",
        "sku":           "INT-VS-BASE",
        "quantity_kg":   300.0,
        "reference_id":  "BI-TRF-16-10",
    }
    headers := map[string]string{"Idempotency-Key": "trf-bi-16-10"}
    okRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", payload, headers)
    if okRec.Code != http.StatusOK {
        t.Fatalf("expected 200 on backward-integration transfer, got %d: %s", okRec.Code, okRec.Body.String())
    }
    var res domain.TransferResult
    _ = json.Unmarshal(okRec.Body.Bytes(), &res)
    if res.ToItem.PlantID != "PLANT-10" || res.ToItem.QuantityKg != 300.0 || res.ToItem.Version != 1 {
        t.Fatalf("unexpected provisioned destination item: %+v", res.ToItem)
    }

    replayRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", payload, headers)
    var replay domain.TransferResult
    _ = json.Unmarshal(replayRec.Body.Bytes(), &replay)
    if !replay.IdempotentReplay || replay.ToItem.QuantityKg != 300.0 {
        t.Fatalf("expected idempotent transfer replay at 300kg, got %+v", replay)
    }
}

func TestPostgresIntegrationEndToEnd(t *testing.T) {
    dsn := os.Getenv("TEST_DATABASE_URL")
    if dsn == "" {
        t.Skip("TEST_DATABASE_URL not set; skipping live PostgreSQL 16 integration test")
    }

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    pgStore, err := store.NewPostgresStore(ctx, dsn)
    if err != nil {
        t.Fatalf("connect postgres: %v", err)
    }
    defer pgStore.Close()

    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    h := api.NewServer(pgStore, logger, "pg-ci-key", "postgres", "*").Handler()
    authHeaders := map[string]string{"X-API-Key": "pg-ci-key"}

    // 1. Health & 23-plant seeded overview
    healthRec := doJSON(t, h, http.MethodGet, "/health", nil, nil)
    if healthRec.Code != http.StatusOK {
        t.Fatalf("pg health failed: %d %s", healthRec.Code, healthRec.Body.String())
    }
    overviewRec := doJSON(t, h, http.MethodGet, "/api/v1/overview", nil, nil)
    var ov domain.NetworkOverview
    _ = json.Unmarshal(overviewRec.Body.Bytes(), &ov)
    if ov.TotalPlants != 23 || ov.LowStockAlerts != 3 {
        t.Fatalf("expected 23 plants & 3 low-stock alerts in postgres, got plants=%d alerts=%d", ov.TotalPlants, ov.LowStockAlerts)
    }

    // 2. Concurrent identical Idempotency-Key storm (10 goroutines -> 1 fresh + 9 replays, zero 500s)
    const idempWorkers = 10
    var (
        wg      sync.WaitGroup
        fresh   int32
        replays int32
    )
    wg.Add(idempWorkers)
    for i := 0; i < idempWorkers; i++ {
        go func() {
            defer wg.Done()
            rec := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-01/inventory/DSP-RED-60/adjust", map[string]any{
                "delta_kg":      -100.0,
                "movement_type": "PG_IDEMP_RACE",
            }, map[string]string{
                "X-API-Key":       "pg-ci-key",
                "Idempotency-Key": "pg-race-key-01",
            })
            if rec.Code != http.StatusOK {
                t.Errorf("expected 200 under concurrent idempotent storm, got %d: %s", rec.Code, rec.Body.String())
                return
            }
            var r domain.AdjustResult
            _ = json.Unmarshal(rec.Body.Bytes(), &r)
            if r.IdempotentReplay {
                atomic.AddInt32(&replays, 1)
            } else {
                atomic.AddInt32(&fresh, 1)
            }
        }()
    }
    wg.Wait()
    if fresh != 1 || replays != idempWorkers-1 {
        t.Fatalf("expected 1 fresh & %d replays on postgres, got fresh=%d replays=%d", idempWorkers-1, fresh, replays)
    }

    // 3. Cross-endpoint idempotency key reuse -> 409 Conflict
    conflictRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", map[string]any{
        "from_plant_id": "PLANT-22",
        "to_plant_id":   "PLANT-03",
        "sku":           "AUX-DISP-NNO",
        "quantity_kg":   50.0,
    }, map[string]string{
        "X-API-Key":       "pg-ci-key",
        "Idempotency-Key": "pg-race-key-01",
    })
    if conflictRec.Code != http.StatusConflict {
        t.Fatalf("expected 409 on cross-operation key reuse in postgres, got %d", conflictRec.Code)
    }

    // 4. Atomic inter-plant transfer + ledger verification
    trfRec := doJSON(t, h, http.MethodPost, "/api/v1/transfers", map[string]any{
        "from_plant_id": "PLANT-22",
        "to_plant_id":   "PLANT-03",
        "sku":           "AUX-DISP-NNO",
        "quantity_kg":   150.0,
        "reference_id":  "PG-TRF-01",
    }, map[string]string{
        "X-API-Key":       "pg-ci-key",
        "Idempotency-Key": "pg-trf-key-01",
    })
    if trfRec.Code != http.StatusOK {
        t.Fatalf("postgres transfer failed: %d %s", trfRec.Code, trfRec.Body.String())
    }

    // 5. Concurrent FOR UPDATE stock deduction race on PLANT-02 DSP-BLU-56 (starts at 1050kg)
    var (
        wg2      sync.WaitGroup
        okCount  int32
        errCount int32
    )
    wg2.Add(20)
    for i := 0; i < 20; i++ {
        go func() {
            defer wg2.Done()
            rec := doJSON(t, h, http.MethodPost, "/api/v1/plants/PLANT-02/inventory/DSP-BLU-56/adjust", map[string]any{
                "delta_kg": -100.0,
            }, authHeaders)
            switch rec.Code {
            case http.StatusOK:
                atomic.AddInt32(&okCount, 1)
            case http.StatusUnprocessableEntity:
                atomic.AddInt32(&errCount, 1)
            default:
                t.Errorf("unexpected postgres status under race: %d %s", rec.Code, rec.Body.String())
            }
        }()
    }
    wg2.Wait()
    if okCount != 10 || errCount != 10 {
        t.Fatalf("expected 10 succeeded & 10 rejected on postgres FOR UPDATE race, got ok=%d err=%d", okCount, errCount)
    }
}