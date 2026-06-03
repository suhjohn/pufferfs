// PufferFs server — API gateway for sync and query operations.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// JWT secret
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		jwtSecret = []byte("pufferfs-dev-secret-change-in-production")
		log.Println("WARNING: Using default JWT secret. Set JWT_SECRET in production.")
	}

	// Auth middleware: supports both JWT and API key
	handler := auth.Middleware(jwtSecret, db.ResolveAPIKey)(srv.Handler())

	// OAuth2 handler (Google)
	googleClientID := os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	redirectURL := os.Getenv("OAUTH_REDIRECT_URL")
	if redirectURL == "" {
		redirectURL = "http://localhost:8080/auth/callback"
	}

	if googleClientID != "" && googleClientSecret != "" {
		oauthHandler := auth.NewOAuthHandler(auth.OAuthConfig{
			GoogleClientID:     googleClientID,
			GoogleClientSecret: googleClientSecret,
			RedirectURL:        redirectURL,
			JWTSecret:          jwtSecret,
		}, db.UpsertUser)

		// Register OAuth routes on a separate mux that wraps the auth-protected one
		topMux := http.NewServeMux()
		topMux.HandleFunc("GET /auth/google", oauthHandler.HandleLogin)
		topMux.HandleFunc("GET /auth/callback", oauthHandler.HandleCallback)
		topMux.Handle("/", handler)
		handler = topMux

		log.Println("Google OAuth2 enabled")
	} else {
		log.Println("Google OAuth2 disabled (set GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET to enable)")
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("PufferFs server listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
