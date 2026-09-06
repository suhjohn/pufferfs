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
	stage := flag.String("stage", getenv("PUFFERFS_WORKER_STAGE", queue.StageTransform), "consumer role: transform or index")
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

	q, err := queue.NewFromEnv(context.Background())
	if err != nil {
		log.Fatalf("connecting to sync queue: %v", err)
	}

	srv := server.New(db, s3Client, modalClient, tpClient)

	consumer, err := server.NewFileConsumer(srv, q, *stage, *concurrency)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("file consumer running stage=%s concurrency=%d", *stage, *concurrency)
	if err := consumer.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
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
