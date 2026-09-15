// Package server implements the PufferFs API server.
package server

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// ModalClient calls independently deployed transformation and indexing roles.
type ModalClient struct {
	transformURL string
	fileIndexURL string
	secretKey    string
	httpClient   *http.Client
}

// NewModalClient creates a client for calling Modal endpoints.
func NewModalClient() *ModalClient {
	return &ModalClient{
		transformURL: os.Getenv("MODAL_TRANSFORM_ENDPOINT"),
		fileIndexURL: os.Getenv("MODAL_FILE_INDEX_ENDPOINT"),
		secretKey:    os.Getenv("MODAL_SECRET_KEY"),
		httpClient: &http.Client{
			Timeout: time.Hour,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				// Modal polls long-running web inputs with a 303 every 150s.
				// Go's default ten redirects ends a valid one-hour call early.
				// Retain a finite redirect bound alongside the overall timeout.
				if len(via) >= 32 {
					return fmt.Errorf("Modal exceeded 32 redirects")
				}
				return nil
			},
		},
	}
}
