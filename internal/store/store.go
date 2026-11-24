package store

import (
    "context"
	"crypto/rand"
	"encoding/hex"
    "fmt"
    "math"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/domain"
)

type InventoryStore interface {
    ListPlants(ctx context.Context) ([]domain.Plant, error)
    GetNetworkOverview(ctx context.Context) (domain.NetworkOverview, error)
    GetPlantInventory(ctx context.Context, plantID string, lowStockOnly bool) ([]domain.InventoryItem, error)
    GetStockItem(ctx context.Context, plantID, sku string) (domain.InventoryItem, error)
    AdjustStock(ctx context.Context, plantID, sku string, req domain.AdjustStockRequest) (domain.AdjustResult, error)
    TransferStock(ctx context.Context, req domain.TransferStockRequest) (domain.TransferResult, error)
    ListLedger(ctx context.Context, plantID string, limit int) ([]domain.StockLedgerEntry, error)
	ListLowStockAlerts(ctx context.Context) ([]domain.InventoryItem, error)
	GetSKUAcrossPlants(ctx context.Context, sku string) ([]domain.InventoryItem, error)
    Ping(ctx context.Context) error
    Close()
}

func newID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}

func roundKg(v float64) float64 {
    return math.Round(v*1000) / 1000
}

func SeedPlants() []domain.Plant {
    plants := make([]domain.Plant, 0, 23)
    for i := 1; i <= 23; i++ {
        id := fmt.Sprintf("PLANT-%02d", i)
        var zone, spec string
        switch {
        case i <= 9:
            zone = "Disperse Block"
            spec = "Disperse Dyes"
        case i <= 15:
            zone = "Reactive Block"
            spec = "Reactive Dyes"
        case i <= 19:
            zone = "Backward Integration - Intermediates"
            spec = "Dye Intermediates"
        case i <= 21:
            zone = "Backward Integration - Crude"
            spec = "Crude Dyes"
        default:
            zone = "Auxiliaries & Formulation"
            spec = "In-House Dispersing Agents"
        }
        plants = append(plants, domain.Plant{
            ID:             id,
            Name:           fmt.Sprintf("Manufacturing Unit %02d (%s)", i, spec),
            Zone:           zone,
            Specialization: spec,
            Status:         "ACTIVE",
        })
    }
    return plants
}

func SeedInventory() []domain.InventoryItem {
    now := time.Now().UTC()
    var items []domain.InventoryItem

    add := func(plantID, sku, name, cat string, qty, reorder float64) {
        items = append(items, domain.InventoryItem{
            PlantID:        plantID,
            SKU:            sku,
            MaterialName:   name,
            Category:       cat,
            Unit:           "kg",
            QuantityKg:     qty,
            ReorderLevelKg: reorder,
            LowStock:       qty <= reorder,
            Version:        1,
            UpdatedAt:      now,
        })
    }

    for i := 1; i <= 23; i++ {
        pid := fmt.Sprintf("PLANT-%02d", i)
        switch {
        case i <= 9:
            qtyRed := 1200.0 + float64(i*75)
            if i == 3 {
                qtyRed = 240.0 // deliberate low-stock alert for ERP monitoring
            }
            add(pid, "DSP-RED-60", "Disperse Red 60 (200%)", "Disperse Dyes", qtyRed, 300.0)
            add(pid, "DSP-BLU-56", "Disperse Blue 56", "Disperse Dyes", 950.0+float64(i*50), 250.0)
            add(pid, "AUX-DISP-NNO", "Dispersing Agent NNO", "Dispersing Agents", 600.0+float64(i*20), 150.0)
        case i <= 15:
            qtyChr := 1100.0 + float64(i*40)
            if i == 12 {
                qtyChr = 180.0 // deliberate low-stock alert
            }
            add(pid, "RCT-RED-195", "Reactive Red 195", "Reactive Dyes", qtyChr, 250.0)
            add(pid, "RCT-BLU-21", "Reactive Blue 21", "Reactive Dyes", 880.0+float64(i*30), 220.0)
        case i <= 19:
            add(pid, "INT-VS-BASE", "Vinyl Sulphone Intermediate", "Dye Intermediates", 2400.0+float64(i*100), 500.0)
            add(pid, "INT-HACID", "H-Acid Technical Grade", "Dye Intermediates", 1600.0+float64(i*60), 400.0)
        case i <= 21:
            add(pid, "CRD-DISP-01", "Crude Disperse Azo Presscake", "Crude Dyes", 3200.0+float64(i*90), 700.0)
            add(pid, "CRD-REAC-01", "Crude Reactive Synthesis Cake", "Crude Dyes", 2100.0+float64(i*70), 500.0)
        default:
            qtyAux := 4500.0 + float64(i*80)
            if i == 23 {
                qtyAux = 320.0 // deliberate low-stock alert
            }
            add(pid, "AUX-DISP-NNO", "Dispersing Agent NNO Bulk", "Dispersing Agents", qtyAux, 500.0)
            add(pid, "AUX-DISP-MF", "Dispersing Agent MF High-Temp", "Dispersing Agents", 2800.0, 450.0)
        }
    }
    return items
}