package store

import (
    "context"
    "fmt"
    "sort"
    "strings"
    "sync"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/domain"
)

type MemoryStore struct {
    mu            sync.RWMutex
    plants        map[string]domain.Plant
    items         map[string]domain.InventoryItem
    ledger        []domain.StockLedgerEntry
    adjustIdemp   map[string]domain.AdjustResult
    transferIdemp map[string]domain.TransferResult
    seq           uint64
}

func NewMemoryStore() *MemoryStore {
    s := &MemoryStore{
        plants:        make(map[string]domain.Plant),
        items:         make(map[string]domain.InventoryItem),
        ledger:        make([]domain.StockLedgerEntry, 0, 128),
        adjustIdemp:   make(map[string]domain.AdjustResult),
        transferIdemp: make(map[string]domain.TransferResult),
    }
    for _, p := range SeedPlants() {
        s.plants[p.ID] = p
    }
    for _, item := range SeedInventory() {
        s.items[itemKey(item.PlantID, item.SKU)] = item
    }
    return s
}

func itemKey(plantID, sku string) string {
    return strings.ToUpper(plantID) + "::" + strings.ToUpper(sku)
}

func (s *MemoryStore) nextID(prefix string) string {
    s.seq++
    return fmt.Sprintf("%s-%06d", prefix, s.seq)
}

func (s *MemoryStore) Ping(_ context.Context) error { return nil }
func (s *MemoryStore) Close()                       {}

func (s *MemoryStore) ListPlants(_ context.Context) ([]domain.Plant, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    out := make([]domain.Plant, 0, len(s.plants))
    for _, p := range s.plants {
        out = append(out, p)
    }
    sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
    return out, nil
}

func (s *MemoryStore) GetNetworkOverview(_ context.Context) (domain.NetworkOverview, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    summaryByPlant := make(map[string]*domain.PlantSummary, len(s.plants))
    activePlants := 0
    for _, p := range s.plants {
        if p.Status == "ACTIVE" {
            activePlants++
        }
        summaryByPlant[p.ID] = &domain.PlantSummary{Plant: p}
    }

    var totalKg float64
    lowAlerts := 0
    for _, item := range s.items {
        ps := summaryByPlant[item.PlantID]
        if ps == nil {
            continue
        }
        ps.TotalSKUs++
        ps.TotalStockKg = roundKg(ps.TotalStockKg + item.QuantityKg)
        if item.QuantityKg <= item.ReorderLevelKg {
            ps.LowStockCount++
            lowAlerts++
        }
        totalKg = roundKg(totalKg + item.QuantityKg)
    }

    summaries := make([]domain.PlantSummary, 0, len(summaryByPlant))
    for _, ps := range summaryByPlant {
        summaries = append(summaries, *ps)
    }
    sort.Slice(summaries, func(i, j int) bool { return summaries[i].Plant.ID < summaries[j].Plant.ID })

    return domain.NetworkOverview{
        TotalPlants:       len(s.plants),
        ActivePlants:      activePlants,
        TotalStockRecords: len(s.items),
        TotalStockKg:      totalKg,
        LowStockAlerts:    lowAlerts,
        Plants:            summaries,
    }, nil
}

func (s *MemoryStore) GetPlantInventory(_ context.Context, plantID string, lowStockOnly bool) ([]domain.InventoryItem, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    pid := strings.ToUpper(plantID)
    if _, ok := s.plants[pid]; !ok {
        return nil, domain.ErrNotFound
    }

    out := make([]domain.InventoryItem, 0)
    for _, item := range s.items {
        if item.PlantID != pid {
            continue
        }
        item.LowStock = item.QuantityKg <= item.ReorderLevelKg
        if lowStockOnly && !item.LowStock {
            continue
        }
        out = append(out, item)
    }
    sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
    return out, nil
}

func (s *MemoryStore) GetStockItem(_ context.Context, plantID, sku string) (domain.InventoryItem, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    item, ok := s.items[itemKey(plantID, sku)]
    if !ok {
        return domain.InventoryItem{}, domain.ErrNotFound
    }
    item.LowStock = item.QuantityKg <= item.ReorderLevelKg
    return item, nil
}

func (s *MemoryStore) AdjustStock(_ context.Context, plantID, sku string, req domain.AdjustStockRequest) (domain.AdjustResult, error) {
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

    s.mu.Lock()
    defer s.mu.Unlock()

    if req.IdempotencyKey != "" {
        if _, usedInTransfer := s.transferIdemp[req.IdempotencyKey]; usedInTransfer {
            return domain.AdjustResult{}, domain.ErrIdempotencyConflict
        }
        if prev, ok := s.adjustIdemp[req.IdempotencyKey]; ok {
            prev.IdempotentReplay = true
            return prev, nil
        }
    }

    key := itemKey(plantID, sku)
    item, ok := s.items[key]
    if !ok {
        return domain.AdjustResult{}, domain.ErrNotFound
    }
    if req.ExpectedVersion != nil && item.Version != *req.ExpectedVersion {
        return domain.AdjustResult{}, domain.ErrVersionConflict
    }

    nextQty := roundKg(item.QuantityKg + req.DeltaKg)
    if nextQty < 0 {
        return domain.AdjustResult{}, domain.ErrInsufficientStock
    }

    now := time.Now().UTC()
    item.QuantityKg = nextQty
    item.LowStock = item.QuantityKg <= item.ReorderLevelKg
    item.Version++
    item.UpdatedAt = now
    s.items[key] = item

    entry := domain.StockLedgerEntry{
        ID:             s.nextID("LDG"),
        IdempotencyKey: req.IdempotencyKey,
        PlantID:        item.PlantID,
        SKU:            item.SKU,
        MovementType:   movement,
        DeltaKg:        roundKg(req.DeltaKg),
        BalanceAfterKg: item.QuantityKg,
        ReferenceID:    req.ReferenceID,
        Notes:          req.Notes,
        CreatedAt:      now,
    }
    s.ledger = append(s.ledger, entry)

    res := domain.AdjustResult{
        Item:        item,
        LedgerEntry: entry,
    }
    if req.IdempotencyKey != "" {
        s.adjustIdemp[req.IdempotencyKey] = res
    }
    return res, nil
}

func (s *MemoryStore) TransferStock(_ context.Context, req domain.TransferStockRequest) (domain.TransferResult, error) {
    fromPlant := strings.ToUpper(strings.TrimSpace(req.FromPlantID))
    toPlant := strings.ToUpper(strings.TrimSpace(req.ToPlantID))
    sku := strings.ToUpper(strings.TrimSpace(req.SKU))

    if fromPlant == "" || toPlant == "" || sku == "" || req.QuantityKg <= 0 {
        return domain.TransferResult{}, fmt.Errorf("%w: from_plant_id, to_plant_id, sku, and positive quantity_kg are required", domain.ErrInvalidInput)
    }
    if fromPlant == toPlant {
        return domain.TransferResult{}, domain.ErrSamePlantTransfer
    }

    s.mu.Lock()
    defer s.mu.Unlock()

    if req.IdempotencyKey != "" {
        if _, usedInAdjust := s.adjustIdemp[req.IdempotencyKey]; usedInAdjust {
            return domain.TransferResult{}, domain.ErrIdempotencyConflict
        }
        if prev, ok := s.transferIdemp[req.IdempotencyKey]; ok {
            prev.IdempotentReplay = true
            return prev, nil
        }
    }

    if _, ok := s.plants[toPlant]; !ok {
        return domain.TransferResult{}, domain.ErrNotFound
    }

    fromKey := itemKey(fromPlant, sku)
    fromItem, ok := s.items[fromKey]
    if !ok {
        return domain.TransferResult{}, domain.ErrNotFound
    }
    if fromItem.QuantityKg < req.QuantityKg {
        return domain.TransferResult{}, domain.ErrInsufficientStock
    }

    now := time.Now().UTC()
    toKey := itemKey(toPlant, sku)
    toItem, exists := s.items[toKey]
    if !exists {
        toItem = domain.InventoryItem{
            PlantID:        toPlant,
            SKU:            fromItem.SKU,
            MaterialName:   fromItem.MaterialName,
            Category:       fromItem.Category,
            Unit:           fromItem.Unit,
            QuantityKg:     0,
            ReorderLevelKg: fromItem.ReorderLevelKg,
            Version:        0,
            UpdatedAt:      now,
        }
    }

    fromItem.QuantityKg = roundKg(fromItem.QuantityKg - req.QuantityKg)
    fromItem.LowStock = fromItem.QuantityKg <= fromItem.ReorderLevelKg
    fromItem.Version++
    fromItem.UpdatedAt = now
    s.items[fromKey] = fromItem

    toItem.QuantityKg = roundKg(toItem.QuantityKg + req.QuantityKg)
    toItem.LowStock = toItem.QuantityKg <= toItem.ReorderLevelKg
    toItem.Version++
    toItem.UpdatedAt = now
    s.items[toKey] = toItem

    txID := s.nextID("TRF")
    ref := req.ReferenceID
    if ref == "" {
        ref = txID
    }
    s.ledger = append(s.ledger,
        domain.StockLedgerEntry{
            ID:             s.nextID("LDG"),
            IdempotencyKey: req.IdempotencyKey,
            PlantID:        fromItem.PlantID,
            SKU:            fromItem.SKU,
            MovementType:   "TRANSFER_OUT",
            DeltaKg:        -roundKg(req.QuantityKg),
            BalanceAfterKg: fromItem.QuantityKg,
            ReferenceID:    ref,
            Notes:          fmt.Sprintf("Transfer to %s: %s", toPlant, req.Notes),
            CreatedAt:      now,
        },
        domain.StockLedgerEntry{
            ID:             s.nextID("LDG"),
            IdempotencyKey: req.IdempotencyKey,
            PlantID:        toItem.PlantID,
            SKU:            toItem.SKU,
            MovementType:   "TRANSFER_IN",
            DeltaKg:        roundKg(req.QuantityKg),
            BalanceAfterKg: toItem.QuantityKg,
            ReferenceID:    ref,
            Notes:          fmt.Sprintf("Transfer from %s: %s", fromPlant, req.Notes),
            CreatedAt:      now,
        },
    )

    res := domain.TransferResult{
        TransferID: txID,
        FromItem:   fromItem,
        ToItem:     toItem,
    }
    if req.IdempotencyKey != "" {
        s.transferIdemp[req.IdempotencyKey] = res
    }
    return res, nil
}

func (s *MemoryStore) ListLedger(_ context.Context, plantID string, limit int) ([]domain.StockLedgerEntry, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    if limit <= 0 || limit > 200 {
        limit = 50
    }
    pid := strings.ToUpper(strings.TrimSpace(plantID))

    out := make([]domain.StockLedgerEntry, 0, limit)
    for i := len(s.ledger) - 1; i >= 0 && len(out) < limit; i-- {
        entry := s.ledger[i]
        if pid != "" && entry.PlantID != pid {
            continue
        }
        out = append(out, entry)
    }
    return out, nil
}

func (s *MemoryStore) ListLowStockAlerts(_ context.Context) ([]domain.InventoryItem, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    out := make([]domain.InventoryItem, 0)
    for _, item := range s.items {
        if item.QuantityKg <= item.ReorderLevelKg {
            item.LowStock = true
            out = append(out, item)
        }
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].PlantID != out[j].PlantID {
            return out[i].PlantID < out[j].PlantID
        }
        return out[i].SKU < out[j].SKU
    })
    return out, nil
}

func (s *MemoryStore) GetSKUAcrossPlants(_ context.Context, sku string) ([]domain.InventoryItem, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    code := strings.ToUpper(strings.TrimSpace(sku))
    out := make([]domain.InventoryItem, 0)
    for _, item := range s.items {
        if item.SKU != code {
            continue
        }
        item.LowStock = item.QuantityKg <= item.ReorderLevelKg
        out = append(out, item)
    }
    if len(out) == 0 {
        return nil, domain.ErrNotFound
    }
    sort.Slice(out, func(i, j int) bool { return out[i].PlantID < out[j].PlantID })
    return out, nil
}