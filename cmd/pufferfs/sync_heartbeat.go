package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"
)

const defaultSyncSessionHeartbeatInterval = time.Minute

type syncSessionHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startSyncSessionHeartbeat(client *apiClient, rootID, generationID string, log io.Writer) *syncSessionHeartbeat {
	return startSyncSessionHeartbeatWithInterval(client, rootID, generationID, defaultSyncSessionHeartbeatInterval, log)
}

func startSyncSessionHeartbeatWithInterval(client *apiClient, rootID, generationID string, interval time.Duration, log io.Writer) *syncSessionHeartbeat {
	ctx, cancel := context.WithCancel(context.Background())
	h := &syncSessionHeartbeat{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if client == nil || rootID == "" || generationID == "" {
		cancel()
		close(h.done)
		return h
	}
	if interval <= 0 {
		interval = defaultSyncSessionHeartbeatInterval
	}
	path := fmt.Sprintf("/roots/%s/sync/%s/heartbeat", url.PathEscape(rootID), url.PathEscape(generationID))
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failed := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := client.postContext(ctx, path, nil)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					if !failed && log != nil {
						fmt.Fprintf(log, "Warning: sync upload heartbeat failed: %v\n", err)
					}
					failed = true
					continue
				}
				if failed && log != nil {
					fmt.Fprintln(log, "Sync upload heartbeat recovered.")
				}
				failed = false
			}
		}
	}()
	return h
}

func (h *syncSessionHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.once.Do(h.cancel)
	<-h.done
}
