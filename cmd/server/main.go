package main

import (
    "context"
    "errors"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/ankit5841/erp-inventory-service/internal/api"
    "github.com/ankit5841/erp-inventory-service/internal/store"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
        Level: slog.LevelInfo,
    }))

    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }
    dsn := os.Getenv("DATABASE_URL")
    apiKey := os.Getenv("ERP_API_KEY")
    corsOrigins := os.Getenv("CORS_ORIGINS")

    var (
        invStore    store.InventoryStore
        storageMode string
    )

    if dsn != "" {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        pgStore, err := store.NewPostgresStore(ctx, dsn)
        cancel()
        if err != nil {
            logger.Error("failed to initialize PostgreSQL store", "error", err)
            os.Exit(1)
        }
        invStore = pgStore
        storageMode = "postgres"
        logger.Info("connected to PostgreSQL inventory store")
    } else {
        invStore = store.NewMemoryStore()
        storageMode = "memory"
        logger.Info("DATABASE_URL not set; running with thread-safe in-memory 23-plant store")
    }
    defer invStore.Close()

    srv := api.NewServer(invStore, logger, apiKey, storageMode, corsOrigins)
    httpServer := &http.Server{
        Addr:              ":" + port,
        Handler:           srv.Handler(),
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       10 * time.Second,
        WriteTimeout:      15 * time.Second,
        IdleTimeout:       60 * time.Second,
    }

    go func() {
        logger.Info("starting ERP inventory microservice", "addr", httpServer.Addr, "storage", storageMode)
        if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            logger.Error("server exited unexpectedly", "error", err)
            os.Exit(1)
        }
    }()

    stop := make(chan os.Signal, 1)
    signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
    <-stop

    logger.Info("shutdown signal received; draining active requests")
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := httpServer.Shutdown(shutdownCtx); err != nil {
        logger.Error("graceful shutdown failed", "error", err)
    }
    logger.Info("ERP inventory microservice stopped cleanly")
}