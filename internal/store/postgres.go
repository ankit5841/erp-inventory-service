package store

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/domain"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
    pool *pgxpool.Pool
}

func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
    cfg, err := pgxpool.ParseConfig(dsn)
    if err != nil {
        return nil, fmt.Errorf("parse postgres DSN: %w", err)
    }
    cfg.MaxConns = 20
    cfg.MinConns = 2
    cfg.MaxConnLifetime = 30 * time.Minute
    cfg.MaxConnIdleTime = 5 * time.Minute

    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil {
        return nil, fmt.Errorf("create pgx pool: %w", err)
    }
    if err := pool.Ping(ctx); err != nil {
        pool.Close()
        return nil, fmt.Errorf("ping postgres: %w", err)
    }
    ps := &PostgresStore{pool: pool}
    if err := ps.initSchemaAndSeed(ctx); err != nil {
        pool.Close()
        return nil, err
    }
    return ps, nil
}

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *PostgresStore) Close()                         { s.pool.Close() }

func (s *PostgresStore) initSchemaAndSeed(ctx context.Context) error {
    schema := `
CREATE TABLE IF NOT EXISTS plants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    zone TEXT NOT NULL,
    specialization TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ACTIVE'
);

CREATE TABLE IF NOT EXISTS inventory_items (
    plant_id TEXT NOT NULL REFERENCES plants(id),
    sku TEXT NOT NULL,
    material_name TEXT NOT NULL,
    category TEXT NOT NULL,
    unit TEXT NOT NULL DEFAULT 'kg',
    quantity_kg DOUBLE PRECISION NOT NULL CHECK (quantity_kg >= 0),
    reorder_level_kg DOUBLE PRECISION NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (plant_id, sku)
);

CREATE TABLE IF NOT EXISTS stock_ledger (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT,
    plant_id TEXT NOT NULL REFERENCES plants(id),
    sku TEXT NOT NULL,
    movement_type TEXT NOT NULL,
    delta_kg DOUBLE PRECISION NOT NULL,
    balance_after_kg DOUBLE PRECISION NOT NULL,
    reference_id TEXT,
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stock_ledger_plant_created ON stock_ledger(plant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS idempotency_records (
    idempotency_key TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    response_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

    if _, err := s.pool.Exec(ctx, schema); err != nil {
        return fmt.Errorf("init schema: %w", err)
    }

    var count int
    if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM plants`).Scan(&count); err != nil {
        return err
    }
    if count > 0 {
        return nil
    }

    tx, err := s.pool.Begin(ctx)
    if err != nil {
        return err
    }
    defer tx.Rollback(ctx)

    for _, p := range SeedPlants() {
        if _, err := tx.Exec(ctx,
            `INSERT INTO plants (id, name, zone, specialization, status) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
            p.ID, p.Name, p.Zone, p.Specialization, p.Status,
        ); err != nil {
            return err
        }
    }
    for _, it := range SeedInventory() {
        if _, err := tx.Exec(ctx,
            `INSERT INTO inventory_items (plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
            it.PlantID, it.SKU, it.MaterialName, it.Category, it.Unit, it.QuantityKg, it.ReorderLevelKg, it.Version, it.UpdatedAt,
        ); err != nil {
            return err
        }
    }
    return tx.Commit(ctx)
}

func lockIdempotencyKey(ctx context.Context, tx pgx.Tx, key, expectedOp string) ([]byte, bool, error) {
    if key == "" {
        return nil, false, nil
    }
    // Serialize concurrent requests carrying the exact same Idempotency-Key within PostgreSQL.
    if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, key); err != nil {
        return nil, false, fmt.Errorf("advisory lock idempotency key: %w", err)
    }

    var (
        op  string
        raw []byte
    )
    err := tx.QueryRow(ctx, `SELECT operation, response_json FROM idempotency_records WHERE idempotency_key=$1`, key).Scan(&op, &raw)
    if errors.Is(err, pgx.ErrNoRows) {
        return nil, false, nil
    }
    if err != nil {
        return nil, false, err
    }
    if op != expectedOp {
        return nil, false, domain.ErrIdempotencyConflict
    }
    return raw, true, nil
}

func (s *PostgresStore) ListPlants(ctx context.Context) ([]domain.Plant, error) {
    rows, err := s.pool.Query(ctx, `SELECT id, name, zone, specialization, status FROM plants ORDER BY id`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    plants := make([]domain.Plant, 0, 23)
    for rows.Next() {
        var p domain.Plant
        if err := rows.Scan(&p.ID, &p.Name, &p.Zone, &p.Specialization, &p.Status); err != nil {
            return nil, err
        }
        plants = append(plants, p)
    }
    return plants, rows.Err()
}

func (s *PostgresStore) GetNetworkOverview(ctx context.Context) (domain.NetworkOverview, error) {
    rows, err := s.pool.Query(ctx, `
        SELECT p.id, p.name, p.zone, p.specialization, p.status,
               COUNT(i.sku) AS total_skus,
               COALESCE(SUM(i.quantity_kg), 0) AS total_kg,
               COALESCE(SUM(CASE WHEN i.quantity_kg <= i.reorder_level_kg THEN 1 ELSE 0 END), 0) AS low_count
        FROM plants p
        LEFT JOIN inventory_items i ON i.plant_id = p.id
        GROUP BY p.id, p.name, p.zone, p.specialization, p.status
        ORDER BY p.id`)
    if err != nil {
        return domain.NetworkOverview{}, err
    }
    defer rows.Close()

    ov := domain.NetworkOverview{}
    for rows.Next() {
        var ps domain.PlantSummary
        if err := rows.Scan(
            &ps.Plant.ID, &ps.Plant.Name, &ps.Plant.Zone, &ps.Plant.Specialization, &ps.Plant.Status,
            &ps.TotalSKUs, &ps.TotalStockKg, &ps.LowStockCount,
        ); err != nil {
            return domain.NetworkOverview{}, err
        }
        ps.TotalStockKg = roundKg(ps.TotalStockKg)
        ov.TotalPlants++
        if ps.Plant.Status == "ACTIVE" {
            ov.ActivePlants++
        }
        ov.TotalStockRecords += ps.TotalSKUs
        ov.TotalStockKg = roundKg(ov.TotalStockKg + ps.TotalStockKg)
        ov.LowStockAlerts += ps.LowStockCount
        ov.Plants = append(ov.Plants, ps)
    }
    return ov, rows.Err()
}

func (s *PostgresStore) GetPlantInventory(ctx context.Context, plantID string, lowStockOnly bool) ([]domain.InventoryItem, error) {
    pid := strings.ToUpper(plantID)
    var exists bool
    if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plants WHERE id=$1)`, pid).Scan(&exists); err != nil {
        return nil, err
    }
    if !exists {
        return nil, domain.ErrNotFound
    }

    q := `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
          FROM inventory_items WHERE plant_id=$1`
    if lowStockOnly {
        q += ` AND quantity_kg <= reorder_level_kg`
    }
    q += ` ORDER BY sku`

    rows, err := s.pool.Query(ctx, q, pid)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    out := make([]domain.InventoryItem, 0)
    for rows.Next() {
        var it domain.InventoryItem
        if err := rows.Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt); err != nil {
            return nil, err
        }
        it.LowStock = it.QuantityKg <= it.ReorderLevelKg
        out = append(out, it)
    }
    return out, rows.Err()
}

func (s *PostgresStore) GetStockItem(ctx context.Context, plantID, sku string) (domain.InventoryItem, error) {
    var it domain.InventoryItem
    err := s.pool.QueryRow(ctx,
        `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
         FROM inventory_items WHERE plant_id=$1 AND sku=$2`,
        strings.ToUpper(plantID), strings.ToUpper(sku),
    ).Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt)
    if errors.Is(err, pgx.ErrNoRows) {
        return domain.InventoryItem{}, domain.ErrNotFound
    }
    if err != nil {
        return domain.InventoryItem{}, err
    }
    it.LowStock = it.QuantityKg <= it.ReorderLevelKg
    return it, nil
}

func (s *PostgresStore) AdjustStock(ctx context.Context, plantID, sku string, req domain.AdjustStockRequest) (domain.AdjustResult, error) {
    if req.DeltaKg == 0 {
        return domain.AdjustResult{}, fmt.Errorf("%w: delta_kg cannot be zero", domain.ErrInvalidInput)
    }
    movement := strings.ToUpper(strings.TrimSpace(req.MovementType))
    if movement == "" {
        if req.DeltaKg > 0 {
            movement = "INBOUND"
        } else {
            movement = "CONSUMPTION"
        }
    }

    tx, err := s.pool.Begin(ctx)
    if err != nil {
        return domain.AdjustResult{}, err
    }
    defer tx.Rollback(ctx)

    if raw, found, err := lockIdempotencyKey(ctx, tx, req.IdempotencyKey, "ADJUST"); err != nil {
        return domain.AdjustResult{}, err
    } else if found {
        var prev domain.AdjustResult
        if err := json.Unmarshal(raw, &prev); err != nil {
            return domain.AdjustResult{}, err
        }
        prev.IdempotentReplay = true
        return prev, nil
    }

    pid, code := strings.ToUpper(plantID), strings.ToUpper(sku)
    var it domain.InventoryItem
    err = tx.QueryRow(ctx,
        `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
         FROM inventory_items WHERE plant_id=$1 AND sku=$2 FOR UPDATE`,
        pid, code,
    ).Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt)
    if errors.Is(err, pgx.ErrNoRows) {
        return domain.AdjustResult{}, domain.ErrNotFound
    }
    if err != nil {
        return domain.AdjustResult{}, err
    }

    if req.ExpectedVersion != nil && it.Version != *req.ExpectedVersion {
        return domain.AdjustResult{}, domain.ErrVersionConflict
    }

    nextQty := roundKg(it.QuantityKg + req.DeltaKg)
    if nextQty < 0 {
        return domain.AdjustResult{}, domain.ErrInsufficientStock
    }

    now := time.Now().UTC()
    it.QuantityKg = nextQty
    it.LowStock = it.QuantityKg <= it.ReorderLevelKg
    it.Version++
    it.UpdatedAt = now

    if _, err := tx.Exec(ctx,
        `UPDATE inventory_items SET quantity_kg=$1, version=$2, updated_at=$3 WHERE plant_id=$4 AND sku=$5`,
        it.QuantityKg, it.Version, it.UpdatedAt, it.PlantID, it.SKU,
    ); err != nil {
        return domain.AdjustResult{}, err
    }

    entry := domain.StockLedgerEntry{
        ID:             newID("LDG"),
        IdempotencyKey: req.IdempotencyKey,
        PlantID:        it.PlantID,
        SKU:            it.SKU,
        MovementType:   movement,
        DeltaKg:        roundKg(req.DeltaKg),
        BalanceAfterKg: it.QuantityKg,
        ReferenceID:    req.ReferenceID,
        Notes:          req.Notes,
        CreatedAt:      now,
    }
    if _, err := tx.Exec(ctx,
        `INSERT INTO stock_ledger (id, idempotency_key, plant_id, sku, movement_type, delta_kg, balance_after_kg, reference_id, notes, created_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
        entry.ID, entry.IdempotencyKey, entry.PlantID, entry.SKU, entry.MovementType, entry.DeltaKg, entry.BalanceAfterKg, entry.ReferenceID, entry.Notes, entry.CreatedAt,
    ); err != nil {
        return domain.AdjustResult{}, err
    }

    res := domain.AdjustResult{Item: it, LedgerEntry: entry}
    if req.IdempotencyKey != "" {
        b, _ := json.Marshal(res)
        if _, err := tx.Exec(ctx,
            `INSERT INTO idempotency_records (idempotency_key, operation, response_json) VALUES ($1,'ADJUST',$2)`,
            req.IdempotencyKey, b,
        ); err != nil {
            return domain.AdjustResult{}, err
        }
    }

    if err := tx.Commit(ctx); err != nil {
        return domain.AdjustResult{}, err
    }
    return res, nil
}

func (s *PostgresStore) TransferStock(ctx context.Context, req domain.TransferStockRequest) (domain.TransferResult, error) {
    fromPlant := strings.ToUpper(strings.TrimSpace(req.FromPlantID))
    toPlant := strings.ToUpper(strings.TrimSpace(req.ToPlantID))
    sku := strings.ToUpper(strings.TrimSpace(req.SKU))

    if fromPlant == "" || toPlant == "" || sku == "" || req.QuantityKg <= 0 {
        return domain.TransferResult{}, fmt.Errorf("%w: from_plant_id, to_plant_id, sku, and positive quantity_kg are required", domain.ErrInvalidInput)
    }
    if fromPlant == toPlant {
        return domain.TransferResult{}, domain.ErrSamePlantTransfer
    }

    tx, err := s.pool.Begin(ctx)
    if err != nil {
        return domain.TransferResult{}, err
    }
    defer tx.Rollback(ctx)

    if raw, found, err := lockIdempotencyKey(ctx, tx, req.IdempotencyKey, "TRANSFER"); err != nil {
        return domain.TransferResult{}, err
    } else if found {
        var prev domain.TransferResult
        if err := json.Unmarshal(raw, &prev); err != nil {
            return domain.TransferResult{}, err
        }
        prev.IdempotentReplay = true
        return prev, nil
    }

    var destExists bool
    if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plants WHERE id=$1)`, toPlant).Scan(&destExists); err != nil {
        return domain.TransferResult{}, err
    }
    if !destExists {
        return domain.TransferResult{}, domain.ErrNotFound
    }

    // Lock rows in deterministic plant order to prevent deadlocks under concurrent bidirectional transfers
    first, second := fromPlant, toPlant
    if first > second {
        first, second = second, first
    }
    locked := make(map[string]domain.InventoryItem, 2)
    for _, pid := range []string{first, second} {
        var it domain.InventoryItem
        err := tx.QueryRow(ctx,
            `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
             FROM inventory_items WHERE plant_id=$1 AND sku=$2 FOR UPDATE`,
            pid, sku,
        ).Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt)
        if err == nil {
            locked[pid] = it
        } else if !errors.Is(err, pgx.ErrNoRows) {
            return domain.TransferResult{}, err
        }
    }

    fromItem, ok := locked[fromPlant]
    if !ok {
        return domain.TransferResult{}, domain.ErrNotFound
    }
    if fromItem.QuantityKg < req.QuantityKg {
        return domain.TransferResult{}, domain.ErrInsufficientStock
    }

    now := time.Now().UTC()
    fromItem.QuantityKg = roundKg(fromItem.QuantityKg - req.QuantityKg)
    fromItem.LowStock = fromItem.QuantityKg <= fromItem.ReorderLevelKg
    fromItem.Version++
    fromItem.UpdatedAt = now

    if _, err := tx.Exec(ctx,
        `UPDATE inventory_items SET quantity_kg=$1, version=$2, updated_at=$3 WHERE plant_id=$4 AND sku=$5`,
        fromItem.QuantityKg, fromItem.Version, fromItem.UpdatedAt, fromItem.PlantID, fromItem.SKU,
    ); err != nil {
        return domain.TransferResult{}, err
    }

    toItem, destHadSKU := locked[toPlant]
    if destHadSKU {
        toItem.QuantityKg = roundKg(toItem.QuantityKg + req.QuantityKg)
        toItem.LowStock = toItem.QuantityKg <= toItem.ReorderLevelKg
        toItem.Version++
        toItem.UpdatedAt = now
        if _, err := tx.Exec(ctx,
            `UPDATE inventory_items SET quantity_kg=$1, version=$2, updated_at=$3 WHERE plant_id=$4 AND sku=$5`,
            toItem.QuantityKg, toItem.Version, toItem.UpdatedAt, toItem.PlantID, toItem.SKU,
        ); err != nil {
            return domain.TransferResult{}, err
        }
    } else {
        toItem = domain.InventoryItem{
            PlantID:        toPlant,
            SKU:            fromItem.SKU,
            MaterialName:   fromItem.MaterialName,
            Category:       fromItem.Category,
            Unit:           fromItem.Unit,
            QuantityKg:     roundKg(req.QuantityKg),
            ReorderLevelKg: fromItem.ReorderLevelKg,
            Version:        1,
            UpdatedAt:      now,
        }
        toItem.LowStock = toItem.QuantityKg <= toItem.ReorderLevelKg
        if _, err := tx.Exec(ctx,
            `INSERT INTO inventory_items (plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
            toItem.PlantID, toItem.SKU, toItem.MaterialName, toItem.Category, toItem.Unit, toItem.QuantityKg, toItem.ReorderLevelKg, toItem.Version, toItem.UpdatedAt,
        ); err != nil {
            return domain.TransferResult{}, err
        }
    }

    txID := newID("TRF")
    ref := req.ReferenceID
    if ref == "" {
        ref = txID
    }
    if _, err := tx.Exec(ctx,
        `INSERT INTO stock_ledger (id, idempotency_key, plant_id, sku, movement_type, delta_kg, balance_after_kg, reference_id, notes, created_at)
         VALUES ($1,$2,$3,$4,'TRANSFER_OUT',$5,$6,$7,$8,$9), ($10,$2,$11,$4,'TRANSFER_IN',$12,$13,$7,$14,$9)`,
        newID("LDG"), req.IdempotencyKey, fromItem.PlantID, fromItem.SKU,
        -roundKg(req.QuantityKg), fromItem.QuantityKg, ref, fmt.Sprintf("Transfer to %s: %s", toPlant, req.Notes), now,
        newID("LDG"), toItem.PlantID, roundKg(req.QuantityKg), toItem.QuantityKg,
        fmt.Sprintf("Transfer from %s: %s", fromPlant, req.Notes),
    ); err != nil {
        return domain.TransferResult{}, err
    }

    res := domain.TransferResult{TransferID: txID, FromItem: fromItem, ToItem: toItem}
    if req.IdempotencyKey != "" {
        b, _ := json.Marshal(res)
        if _, err := tx.Exec(ctx,
            `INSERT INTO idempotency_records (idempotency_key, operation, response_json) VALUES ($1,'TRANSFER',$2)`,
            req.IdempotencyKey, b,
        ); err != nil {
            return domain.TransferResult{}, err
        }
    }

    if err := tx.Commit(ctx); err != nil {
        return domain.TransferResult{}, err
    }
    return res, nil
}

func (s *PostgresStore) ListLedger(ctx context.Context, plantID string, limit int) ([]domain.StockLedgerEntry, error) {
    if limit <= 0 || limit > 200 {
        limit = 50
    }
    pid := strings.ToUpper(strings.TrimSpace(plantID))

    var (
        rows pgx.Rows
        err  error
    )
    if pid != "" {
        rows, err = s.pool.Query(ctx,
            `SELECT id, COALESCE(idempotency_key,''), plant_id, sku, movement_type, delta_kg, balance_after_kg, COALESCE(reference_id,''), COALESCE(notes,''), created_at
             FROM stock_ledger WHERE plant_id=$1 ORDER BY created_at DESC LIMIT $2`, pid, limit)
    } else {
        rows, err = s.pool.Query(ctx,
            `SELECT id, COALESCE(idempotency_key,''), plant_id, sku, movement_type, delta_kg, balance_after_kg, COALESCE(reference_id,''), COALESCE(notes,''), created_at
             FROM stock_ledger ORDER BY created_at DESC LIMIT $1`, limit)
    }
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    out := make([]domain.StockLedgerEntry, 0, limit)
    for rows.Next() {
        var e domain.StockLedgerEntry
        if err := rows.Scan(&e.ID, &e.IdempotencyKey, &e.PlantID, &e.SKU, &e.MovementType, &e.DeltaKg, &e.BalanceAfterKg, &e.ReferenceID, &e.Notes, &e.CreatedAt); err != nil {
            return nil, err
        }
        out = append(out, e)
    }
    return out, rows.Err()
}

func (s *PostgresStore) ListLowStockAlerts(ctx context.Context) ([]domain.InventoryItem, error) {
    rows, err := s.pool.Query(ctx,
        `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
         FROM inventory_items WHERE quantity_kg <= reorder_level_kg ORDER BY plant_id, sku`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    out := make([]domain.InventoryItem, 0)
    for rows.Next() {
        var it domain.InventoryItem
        if err := rows.Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt); err != nil {
            return nil, err
        }
        it.LowStock = true
        out = append(out, it)
    }
    return out, rows.Err()
}

func (s *PostgresStore) GetSKUAcrossPlants(ctx context.Context, sku string) ([]domain.InventoryItem, error) {
    code := strings.ToUpper(strings.TrimSpace(sku))
    rows, err := s.pool.Query(ctx,
        `SELECT plant_id, sku, material_name, category, unit, quantity_kg, reorder_level_kg, version, updated_at
         FROM inventory_items WHERE sku=$1 ORDER BY plant_id`, code)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    out := make([]domain.InventoryItem, 0)
    for rows.Next() {
        var it domain.InventoryItem
        if err := rows.Scan(&it.PlantID, &it.SKU, &it.MaterialName, &it.Category, &it.Unit, &it.QuantityKg, &it.ReorderLevelKg, &it.Version, &it.UpdatedAt); err != nil {
            return nil, err
        }
        it.LowStock = it.QuantityKg <= it.ReorderLevelKg
        out = append(out, it)
    }
    if err := rows.Err(); err != nil {
        return nil, err
    }
    if len(out) == 0 {
        return nil, domain.ErrNotFound
    }
    return out, nil
}