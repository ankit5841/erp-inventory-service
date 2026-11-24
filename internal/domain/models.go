package domain

import (
    "errors"
    "time"
)

var (
    ErrNotFound            = errors.New("resource not found")
    ErrInsufficientStock   = errors.New("insufficient stock balance")
    ErrVersionConflict     = errors.New("optimistic lock version conflict")
    ErrIdempotencyConflict = errors.New("idempotency key reused for a different operation")
    ErrInvalidInput        = errors.New("invalid input")
    ErrSamePlantTransfer   = errors.New("source and destination plant cannot be the same")
)

type Plant struct {
    ID             string `json:"id"`
    Name           string `json:"name"`
    Zone           string `json:"zone"`
    Specialization string `json:"specialization"`
    Status         string `json:"status"`
}

type InventoryItem struct {
    PlantID        string    `json:"plant_id"`
    SKU            string    `json:"sku"`
    MaterialName   string    `json:"material_name"`
    Category       string    `json:"category"`
    Unit           string    `json:"unit"`
    QuantityKg     float64   `json:"quantity_kg"`
    ReorderLevelKg float64   `json:"reorder_level_kg"`
    LowStock       bool      `json:"low_stock"`
    Version        int64     `json:"version"`
    UpdatedAt      time.Time `json:"updated_at"`
}

type StockLedgerEntry struct {
    ID             string    `json:"id"`
    IdempotencyKey string    `json:"idempotency_key,omitempty"`
    PlantID        string    `json:"plant_id"`
    SKU            string    `json:"sku"`
    MovementType   string    `json:"movement_type"`
    DeltaKg        float64   `json:"delta_kg"`
    BalanceAfterKg float64   `json:"balance_after_kg"`
    ReferenceID    string    `json:"reference_id,omitempty"`
    Notes          string    `json:"notes,omitempty"`
    CreatedAt      time.Time `json:"created_at"`
}

type AdjustStockRequest struct {
    DeltaKg         float64 `json:"delta_kg"`
    MovementType    string  `json:"movement_type"`
    ReferenceID     string  `json:"reference_id"`
    Notes           string  `json:"notes"`
    ExpectedVersion *int64  `json:"expected_version,omitempty"`
    IdempotencyKey  string  `json:"-"`
}

type AdjustResult struct {
    Item             InventoryItem    `json:"item"`
    LedgerEntry      StockLedgerEntry `json:"ledger_entry"`
    IdempotentReplay bool             `json:"idempotent_replay"`
}

type TransferStockRequest struct {
    FromPlantID    string  `json:"from_plant_id"`
    ToPlantID      string  `json:"to_plant_id"`
    SKU            string  `json:"sku"`
    QuantityKg     float64 `json:"quantity_kg"`
    ReferenceID    string  `json:"reference_id"`
    Notes          string  `json:"notes"`
    IdempotencyKey string  `json:"-"`
}

type TransferResult struct {
    TransferID       string        `json:"transfer_id"`
    FromItem         InventoryItem `json:"from_item"`
    ToItem           InventoryItem `json:"to_item"`
    IdempotentReplay bool          `json:"idempotent_replay"`
}

type PlantSummary struct {
    Plant         Plant   `json:"plant"`
    TotalSKUs     int     `json:"total_skus"`
    TotalStockKg  float64 `json:"total_stock_kg"`
    LowStockCount int     `json:"low_stock_count"`
}

type NetworkOverview struct {
    TotalPlants       int            `json:"total_plants"`
    ActivePlants      int            `json:"active_plants"`
    TotalStockRecords int            `json:"total_stock_records"`
    TotalStockKg      float64        `json:"total_stock_kg"`
    LowStockAlerts    int            `json:"low_stock_alerts"`
    Plants            []PlantSummary `json:"plants"`
}