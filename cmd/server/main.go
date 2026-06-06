// PufferFs server — API gateway for sync and query operations.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pufferfs/pufferfs/internal/auth"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/queue"
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
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		q, err := queue.NewNATSQueue(natsURL)
		if err != nil {
			log.Fatalf("connecting to NATS JetStream: %v", err)
		}
		defer q.Close()
		srv.SetQueue(q)
		log.Printf("NATS JetStream sync queue enabled: %s", natsURL)
	}

	// JWT secret
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		jwtSecret = []byte("pufferfs-dev-secret-change-in-production")
		log.Println("WARNING: Using default JWT secret. Set JWT_SECRET in production.")
	}

	// Frontend integration: the web app lives on a different origin (the app.*
	// subdomain), so we set the session as a cross-subdomain cookie and allow
	// that origin through CORS.
	frontendURL := strings.TrimSpace(os.Getenv("FRONTEND_URL"))
	cookieCfg := auth.CookieConfig{
		Domain: strings.TrimSpace(os.Getenv("COOKIE_DOMAIN")),
		Secure: strings.HasPrefix(frontendURL, "https://"),
	}

	// Billing (Stripe) — optional. Enabled only when ENABLE_BILLING=true and a
	// secret key is present, mirroring the OAuth wiring below.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ENABLE_BILLING")), "true") {
		stripeKey := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
		if stripeKey == "" {
			log.Println("ENABLE_BILLING is set but STRIPE_SECRET_KEY is missing; billing disabled")
		} else {
			srv.SetBilling(server.NewStripeClient(server.StripeConfig{
				SecretKey:     stripeKey,
				WebhookSecret: strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
				PriceID:       strings.TrimSpace(os.Getenv("STRIPE_PRICE_ID")),
				FrontendURL:   frontendURL,
			}))
			log.Println("Billing (Stripe) enabled")
		}
	}

	// Auth middleware: supports both JWT and tenant API key for normal routes.
	appHandler := auth.Middleware(jwtSecret, db.ResolveAPIKey)(srv.Handler())
	adminHandler := auth.AdminMiddleware(adminKeyHash())(srv.Handler())

	topMux := http.NewServeMux()
	topMux.Handle("/admin/", adminHandler)
	topMux.Handle("/", appHandler)

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
			FrontendURL:        frontendURL,
			Cookie:             cookieCfg,
			CreateAPIKey:       db.CreateAPIKey,
		}, db.UpsertUser)

		topMux.HandleFunc("GET /auth/google", oauthHandler.HandleLogin)
		topMux.HandleFunc("GET /auth/callback", oauthHandler.HandleCallback)
		topMux.HandleFunc("POST /auth/logout", oauthHandler.HandleLogout)

		log.Println("Google OAuth2 enabled")
	} else {
		log.Println("Google OAuth2 disabled (set GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET to enable)")
	}

	// CORS wraps everything so the browser can send credentialed requests from
	// the frontend origin. No-op when FRONTEND_URL is unset.
	handler := auth.CORS(frontendURL)(topMux)

	addr := listenAddr()

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

func adminKeyHash() string {
	if rawHash := strings.TrimSpace(os.Getenv("PUFFERFS_ADMIN_KEY_HASH")); rawHash != "" {
		log.Println("PufferFS admin API key enabled")
		return rawHash
	}
	if rawKey := strings.TrimSpace(os.Getenv("PUFFERFS_ADMIN_KEY")); rawKey != "" {
		log.Println("PufferFS admin API key enabled")
		return auth.HashAPIKey(rawKey)
	}
	return ""
}

func listenAddr() string {
	if addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); addr != "" {
		return addr
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		if strings.Contains(port, ":") {
			return port
		}
		return ":" + port
	}
	return ":8080"
}
