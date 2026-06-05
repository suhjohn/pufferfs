package main

import "testing"

func TestListenAddrPrefersExplicitListenAddr(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9000")
	t.Setenv("PORT", "8081")

	if got := listenAddr(); got != "127.0.0.1:9000" {
		t.Fatalf("listenAddr() = %q, want explicit LISTEN_ADDR", got)
	}
}

func TestListenAddrUsesRailwayPort(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "8081")

	if got := listenAddr(); got != ":8081" {
		t.Fatalf("listenAddr() = %q, want :8081", got)
	}
}

func TestListenAddrDefault(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "")

	if got := listenAddr(); got != ":8080" {
		t.Fatalf("listenAddr() = %q, want :8080", got)
	}
}
