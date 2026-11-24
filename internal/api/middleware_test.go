package api

import (
    "io"
    "log/slog"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/ankit5841/erp-inventory-service/internal/store"
)

func TestRecoveryMiddlewareReturnsJSON500(t *testing.T) {
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    srv := NewServer(store.NewMemoryStore(), logger, "", "memory", "*")

    panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
        panic("simulated database driver panic")
    })
    h := srv.observabilityMiddleware(srv.recoveryMiddleware(panicking))

    rec := httptest.NewRecorder()
    req := httptest.NewRequest(http.MethodGet, "/boom", nil)
    h.ServeHTTP(rec, req)

    if rec.Code != http.StatusInternalServerError {
        t.Fatalf("expected 500 on panic, got %d", rec.Code)
    }
    if !strings.Contains(rec.Body.String(), "internal server error") {
        t.Fatalf("expected JSON error body, got %s", rec.Body.String())
    }
    if rec.Header().Get("X-Request-ID") == "" {
        t.Fatal("expected X-Request-ID header even after panic recovery")
    }
}

func TestCORSAllowlistRestriction(t *testing.T) {
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    srv := NewServer(store.NewMemoryStore(), logger, "", "memory", "https://erp.chemplant.example")

    allowedReq := httptest.NewRequest(http.MethodGet, "/health", nil)
    allowedReq.Header.Set("Origin", "https://erp.chemplant.example")
    allowedRec := httptest.NewRecorder()
    srv.Handler().ServeHTTP(allowedRec, allowedReq)
    if got := allowedRec.Header().Get("Access-Control-Allow-Origin"); got != "https://erp.chemplant.example" {
        t.Fatalf("expected allowed origin echoed, got %q", got)
    }

    blockedReq := httptest.NewRequest(http.MethodGet, "/health", nil)
    blockedReq.Header.Set("Origin", "https://evil.example")
    blockedRec := httptest.NewRecorder()
    srv.Handler().ServeHTTP(blockedRec, blockedReq)
    if got := blockedRec.Header().Get("Access-Control-Allow-Origin"); got != "" {
        t.Fatalf("expected disallowed origin omitted, got %q", got)
    }
}