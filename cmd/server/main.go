package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bengobox/marketflow-ai/internal/app"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// @title MarketFlow AI Service
// @version 1.0.0
// @description RAG-powered AI chatbot for Forever Living Products marketing automation. Two-tier routing: 8B classifier + 70B responder via Groq, with pgvector RAG and FLP compliance filtering.
// @BasePath /api/v1
// @schemes https http
// @host marketflowai.codevertexafrica.com
// @securityDefinitions.apikey bearerAuth
// @in header
// @name Authorization
// @description JWT token from auth-service (SSO). Format: Bearer {token}
func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("warning: could not load .env file: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	zapCfg := zap.NewProductionConfig()
	if cfg.App.Environment == "development" || cfg.App.Environment == "local" {
		zapCfg = zap.NewDevelopmentConfig()
		zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	logger, err := zapCfg.Build()
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	defer logger.Sync() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatal("failed to bootstrap application", zap.Error(err))
	}

	go func() {
		if err := application.Run(); err != nil {
			logger.Fatal("server encountered error", zap.Error(err))
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutdown signal received")

	// Running video jobs finish before Redis and the HTTP callback close (a deploy used to kill an
	// owner's render mid-way, 2026-11-24). The pod's grace period is set above this bound.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), media.DrainTimeout)
	if left := application.DrainJobs(drainCtx); left > 0 {
		logger.Warn("video jobs still running at shutdown", zap.Int("jobs", left))
	}
	drainCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer shutdownCancel()

	if err := application.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", zap.Error(err))
		time.Sleep(2 * time.Second)
	}
}
