package api

import (
    "crypto/rand"
    "crypto/subtle"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log/slog"
    "math"
    "net/http"
    "runtime/debug"
    "strconv"
    "strings"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/domain"
    "github.com/ankit5841/erp-inventory-service/internal/store"
)

const ServiceVersion = "1.1.0"

type Server struct {
    store          store.InventoryStore
    logger         *slog.Logger
    apiKey         string
    storageMode    string
    allowedOrigins []string
    mux            *http.ServeMux
}

func NewServer(st store.InventoryStore, logger *slog.Logger, apiKey, storageMode, corsOrigins string) *Server {
    if logger == nil {
        logger = slog.Default()
    }
    origins := parseAllowedOrigins(corsOrigins)
    s := &Server{
        store:          st,
        logger:         logger,
        apiKey:         strings.TrimSpace(apiKey),
        storageMode:    storageMode,
        allowedOrigins: origins,
        mux:            http.NewServeMux(),
    }
    s.registerRoutes()
    return s
}

func parseAllowedOrigins(raw string) []string {
    raw = strings.TrimSpace(raw)
    if raw == "" || raw == "*" {
        return []string{"*"}
    }
    parts := strings.Split(raw, ",")
    out := make([]string, 0, len(parts))
    for _, p := range parts {
        if trimmed := strings.TrimSpace(p); trimmed != "" {
            out = append(out, trimmed)
        }
    }
    if len(out) == 0 {
        return []string{"*"}
    }
    return out
}

func (s *Server) Handler() http.Handler {
    return s.corsMiddleware(s.observabilityMiddleware(s.recoveryMiddleware(s.mux)))
}

func (s *Server) registerRoutes() {
    s.mux.HandleFunc("GET /health", s.handleHealth)
    s.mux.HandleFunc("GET /api/v1/overview", s.handleNetworkOverview)
    s.mux.HandleFunc("GET /api/v1/plants", s.handleListPlants)
    s.mux.HandleFunc("GET /api/v1/plants/{plantID}/inventory", s.handlePlantInventory)
    s.mux.HandleFunc("GET /api/v1/plants/{plantID}/inventory/{sku}", s.handleGetStockItem)
    s.mux.HandleFunc("POST /api/v1/plants/{plantID}/inventory/{sku}/adjust", s.requireAPIKey(s.handleAdjustStock))
    s.mux.HandleFunc("POST /api/v1/transfers", s.requireAPIKey(s.handleTransferStock))
    s.mux.HandleFunc("GET /api/v1/ledger", s.handleListLedger)
    s.mux.HandleFunc("GET /api/v1/alerts/low-stock", s.handleLowStockAlerts)
    s.mux.HandleFunc("GET /api/v1/inventory/sku/{sku}", s.handleGetSKUAcrossPlants)
}

type responseRecorder struct {
    http.ResponseWriter
    statusCode  int
    start       time.Time
    wroteHeader bool
}

func (rr *responseRecorder) WriteHeader(code int) {
    if !rr.wroteHeader {
        rr.statusCode = code
        rr.wroteHeader = true
        elapsedMs := float64(time.Since(rr.start).Microseconds()) / 1000.0
        rr.Header().Set("X-Process-Time-Ms", fmt.Sprintf("%.2f", elapsedMs))
        rr.ResponseWriter.WriteHeader(code)
    }
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
    if !rr.wroteHeader {
        rr.WriteHeader(http.StatusOK)
    }
    return rr.ResponseWriter.Write(b)
}

func newRequestID() string {
    b := make([]byte, 4)
    if _, err := rand.Read(b); err != nil {
        return fmt.Sprintf("%x", time.Now().UnixNano())
    }
    return hex.EncodeToString(b)
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                s.logger.Error("http_panic_recovered",
                    "request_id", w.Header().Get("X-Request-ID"),
                    "method", r.Method,
                    "path", r.URL.Path,
                    "panic", fmt.Sprint(rec),
                    "stack", string(debug.Stack()),
                )
                writeError(w, http.StatusInternalServerError, "internal server error")
            }
        }()
        next.ServeHTTP(w, r)
    })
}

func (s *Server) observabilityMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        reqID := r.Header.Get("X-Request-ID")
        if reqID == "" {
            reqID = newRequestID()
        }
        w.Header().Set("X-Request-ID", reqID)

        rec := &responseRecorder{
            ResponseWriter: w,
            statusCode:     http.StatusOK,
            start:          time.Now(),
        }
        next.ServeHTTP(rec, r)

        elapsedMs := float64(time.Since(rec.start).Microseconds()) / 1000.0
        s.logger.Info("http_request",
            "request_id", reqID,
            "method", r.Method,
            "path", r.URL.Path,
            "status", rec.statusCode,
            "latency_ms", fmt.Sprintf("%.2f", elapsedMs),
        )
    })
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        origin := r.Header.Get("Origin")
        if len(s.allowedOrigins) == 1 && s.allowedOrigins[0] == "*" {
            w.Header().Set("Access-Control-Allow-Origin", "*")
        } else if origin != "" {
            for _, allowed := range s.allowedOrigins {
                if allowed == origin {
                    w.Header().Set("Access-Control-Allow-Origin", origin)
                    w.Header().Add("Vary", "Origin")
                    break
                }
            }
        }
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Idempotency-Key, X-Request-ID")
        if r.Method == http.MethodOptions {
            w.WriteHeader(http.StatusNoContent)
            return
        }
        next.ServeHTTP(w, r)
    })
}

func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if s.apiKey != "" {
            provided := r.Header.Get("X-API-Key")
            if subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiKey)) != 1 {
                writeError(w, http.StatusUnauthorized, "invalid or missing X-API-Key header")
                return
            }
        }
        next(w, r)
    }
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    if err := s.store.Ping(r.Context()); err != nil {
        writeError(w, http.StatusServiceUnavailable, "storage health check failed")
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "status":       "healthy",
        "service":      "erp-inventory-service",
        "version":      ServiceVersion,
        "storage_mode": s.storageMode,
        "plants_count": 23,
    })
}

func (s *Server) handleNetworkOverview(w http.ResponseWriter, r *http.Request) {
    overview, err := s.store.GetNetworkOverview(r.Context())
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleListPlants(w http.ResponseWriter, r *http.Request) {
    plants, err := s.store.ListPlants(r.Context())
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "count":  len(plants),
        "plants": plants,
    })
}

func (s *Server) handlePlantInventory(w http.ResponseWriter, r *http.Request) {
    plantID := r.PathValue("plantID")
    lowStockOnly := strings.EqualFold(r.URL.Query().Get("low_stock"), "true")

    items, err := s.store.GetPlantInventory(r.Context(), plantID, lowStockOnly)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "plant_id": strings.ToUpper(plantID),
        "count":    len(items),
        "items":    items,
    })
}

func (s *Server) handleGetStockItem(w http.ResponseWriter, r *http.Request) {
    plantID := r.PathValue("plantID")
    sku := r.PathValue("sku")

    item, err := s.store.GetStockItem(r.Context(), plantID, sku)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleAdjustStock(w http.ResponseWriter, r *http.Request) {
    plantID := r.PathValue("plantID")
    sku := r.PathValue("sku")

    var req domain.AdjustStockRequest
    if err := decodeJSONBody(w, r, &req); err != nil {
        writeError(w, http.StatusBadRequest, err.Error())
        return
    }
    req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))

    res, err := s.store.AdjustStock(r.Context(), plantID, sku, req)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTransferStock(w http.ResponseWriter, r *http.Request) {
    var req domain.TransferStockRequest
    if err := decodeJSONBody(w, r, &req); err != nil {
        writeError(w, http.StatusBadRequest, err.Error())
        return
    }
    req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))

    res, err := s.store.TransferStock(r.Context(), req)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListLedger(w http.ResponseWriter, r *http.Request) {
    plantID := r.URL.Query().Get("plant_id")
    limit := 50
    if raw := r.URL.Query().Get("limit"); raw != "" {
        if parsed, err := strconv.Atoi(raw); err == nil {
            limit = parsed
        }
    }

    entries, err := s.store.ListLedger(r.Context(), plantID, limit)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "count":   len(entries),
        "entries": entries,
    })
}

func (s *Server) handleLowStockAlerts(w http.ResponseWriter, r *http.Request) {
    alerts, err := s.store.ListLowStockAlerts(r.Context())
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "count":  len(alerts),
        "alerts": alerts,
    })
}

func (s *Server) handleGetSKUAcrossPlants(w http.ResponseWriter, r *http.Request) {
    sku := r.PathValue("sku")
    items, err := s.store.GetSKUAcrossPlants(r.Context(), sku)
    if err != nil {
        s.handleDomainError(w, err)
        return
    }
    var totalKg float64
    for _, it := range items {
        totalKg += it.QuantityKg
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "sku":               strings.ToUpper(sku),
        "plants_count":      len(items),
        "total_quantity_kg": math.Round(totalKg*1000) / 1000,
        "locations":         items,
    })
}

func (s *Server) handleDomainError(w http.ResponseWriter, err error) {
    switch {
    case errors.Is(err, domain.ErrNotFound):
        writeError(w, http.StatusNotFound, err.Error())
    case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrIdempotencyConflict):
        writeError(w, http.StatusConflict, err.Error())
    case errors.Is(err, domain.ErrInsufficientStock):
        writeError(w, http.StatusUnprocessableEntity, err.Error())
    case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrSamePlantTransfer):
        writeError(w, http.StatusBadRequest, err.Error())
    default:
        s.logger.Error("internal_server_error", "error", err)
        writeError(w, http.StatusInternalServerError, "internal server error")
    }
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
    r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()
    if err := dec.Decode(dst); err != nil {
        return fmt.Errorf("invalid JSON payload: %w", err)
    }
    if err := dec.Decode(&struct{}{}); err != io.EOF {
        return errors.New("request body must contain a single JSON object")
    }
    return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
    writeJSON(w, status, map[string]any{
        "error":  http.StatusText(status),
        "detail": msg,
    })
}