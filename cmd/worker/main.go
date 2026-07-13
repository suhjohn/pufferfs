package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/internal/server"
	"github.com/pufferfs/pufferfs/internal/storage"
)

func main() {
	stage := flag.String("stage", getenv("PUFFERFS_WORKER_STAGE", queue.StageChunk), "sync stage to run: chunk, embed, index, commit, cleanup")
	concurrency := flag.Int("concurrency", getenvInt("PUFFERFS_WORKER_CONCURRENCY", 4), "maximum jobs processed concurrently")
	flag.Parse()

	cfg, err := appconfig.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	dbURL := getenv("DATABASE_URL", "postgres://localhost:5432/pufferfs?sslmode=disable")
	db, err := server.NewDB(dbURL)
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer db.Close()

	s3Client, err := storage.NewClient(cfg.Storage)
	if err != nil {
		log.Fatalf("creating S3 client: %v", err)
	}
	modalClient := server.NewModalClient()
	tpClient := server.NewTPClient(cfg.Turbopuffer.APIKey, cfg.Turbopuffer.Region)

	q, backend, err := queue.NewFromEnv(context.Background(), true)
	if err != nil {
		log.Fatalf("connecting to sync queue: %v", err)
	}
	defer q.Close()

	srv := server.New(db, s3Client, modalClient, tpClient)
	worker := server.NewSyncDispatcher(srv, q, *stage, *concurrency)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stage == queue.StageCleanup {
		go srv.RunSyncJobWatchdog(ctx)
	}
	log.Printf("pufferfs worker running stage=%s concurrency=%d queue=%s", *stage, *concurrency, backend)
	if err := worker.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("worker stopped: %v", err)
	}
}

func getenv(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
