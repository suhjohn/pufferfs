package main

import (
	"log"
	"os"
	"strings"
	"syscall"
)

func main() {
	target := "/pufferfs-server"
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_PROCESS")), "worker") ||
		strings.TrimSpace(os.Getenv("PUFFERFS_WORKER_STAGE")) != "" {
		target = "/pufferfs-worker"
	}
	if err := syscall.Exec(target, []string{target}, os.Environ()); err != nil {
		log.Fatalf("starting %s: %v", target, err)
	}
}
