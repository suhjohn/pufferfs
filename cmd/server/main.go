// PufferFs server — API gateway for sync and query operations.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/pufferfs/pufferfs/internal/auth"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/server"
	"github.com/pufferfs/pufferfs/internal/storage"
)

func main() {
	cfg, err := appconfig.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Database
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://localhost:5432/pufferfs?sslmode=disable"
	}
	db, err := server.NewDB(dbURL)
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer db.Close()

	// S3
	s3Client, err := storage.NewClient(cfg.Storage)
	if err != nil {
		log.Fatalf("creating S3 client: %v", err)
	}

	// Modal
	modalClient := server.NewModalClient()

	// Turbopuffer
	tpClient := server.NewTPClient(cfg.Turbopuffer.APIKey, cfg.Turbopuffer.Region)

	// Server
	srv := server.New(db, s3Client, modalClient, tpClient)

	// Wrap with auth middleware
	apiKey := os.Getenv("PUFFERFS_API_KEY")
	var handler http.Handler = srv.Handler()
	if apiKey != "" {
		handler = auth.Middleware(apiKey)(handler)
		log.Println("API key authentication enabled")
	} else {
		log.Println("WARNING: No PUFFERFS_API_KEY set, running without authentication")
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("PufferFs server listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
