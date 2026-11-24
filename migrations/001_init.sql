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
);