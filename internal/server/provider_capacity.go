package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const embeddingModel = "qwen/qwen3-embedding-8b"

type embeddingTenantKey struct{}

type providerBudget struct {
	db       *DB
	requests int
	tokens   int64
}

type embeddingCapacityError struct{ delay float64 }

func (e *embeddingCapacityError) Error() string {
	return "shared embedding capacity unavailable; retry later"
}

func (s *Server) ConfigureEmbeddingBudgetFromEnv() error {
	requests, tokens := int64(1024), int64(2000000)
	for _, option := range []struct {
		name             string
		value            *int64
		minimum, maximum int64
	}{
		{"PUFFERFS_EMBEDDING_REQUESTS_PER_MINUTE", &requests, 1, 1000000},
		{"PUFFERFS_EMBEDDING_TOKENS_PER_MINUTE", &tokens, 32768, 1000000000000},
	} {
		if raw := os.Getenv(option.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < option.minimum || value > option.maximum {
				return fmt.Errorf("invalid %s", option.name)
			}
			*option.value = value
		}
	}
	s.tp.embeddingBudget = &providerBudget{db: s.db, requests: int(requests), tokens: tokens}
	return nil
}

func (b *providerBudget) reserve(ctx context.Context, tokens int64) (string, error) {
	org, _ := ctx.Value(embeddingTenantKey{}).(string)
	if org == "" {
		return "", errors.New("embedding request missing tenant")
	}
	if tokens > b.tokens {
		return "", errors.New("embedding request exceeds configured token budget")
	}
	id := uuid.NewString()
	var delay float64
	var available int64
	// Queries return 429 rather than waiting in a durable queue. Their tenant
	// hint expires promptly if the caller does not retry.
	err := b.db.pool.QueryRow(ctx, `SELECT * FROM reserve_provider_capacity($1,$2,$3,$4,$5,$6,1)`,
		embeddingModel, org, id, tokens, b.requests, b.tokens).Scan(&delay, &available)
	if err != nil {
		return "", err
	}
	if delay > 0 {
		return "", &embeddingCapacityError{delay: delay}
	}
	return id, nil
}

func (b *providerBudget) settle(id string, response []byte) {
	var metadata struct {
		Performance struct {
			Tokens *int64 `json:"embedding_tokens"`
		} `json:"performance"`
	}
	if json.Unmarshal(response, &metadata) != nil || metadata.Performance.Tokens == nil || *metadata.Performance.Tokens < 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// On bookkeeping failure, the conservative reservation survives. Do not
	// turn a confirmed provider response into a second billable attempt.
	_, _ = b.db.pool.Exec(ctx, `UPDATE provider_reservations SET tokens=$2 WHERE id=$1`, id, *metadata.Performance.Tokens)
}

func (b *providerBudget) throttled(id string, header http.Header) error {
	delay := 5.0
	if value, err := strconv.ParseFloat(header.Get("Retry-After"), 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
		delay = min(3600, max(1, value))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := b.db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE provider_capacity SET blocked_until=GREATEST(blocked_until,
		clock_timestamp()+make_interval(secs=>$2)) WHERE model=$1`, embeddingModel, delay); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE provider_reservations SET tokens=0 WHERE id=$1`, id); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return &embeddingCapacityError{delay: delay}
}

func queryEmbeddingTokens(body any) int64 {
	switch value := body.(type) {
	case TPQuery:
		return rankEmbeddingTokens(value.RankBy)
	case map[string]any:
		var total int64
		if queries, ok := value["queries"].([]TPQuery); ok {
			for _, query := range queries {
				total += rankEmbeddingTokens(query.RankBy)
			}
		}
		return total
	}
	return 0
}

func rankEmbeddingTokens(rank any) int64 {
	items, ok := rank.([]any)
	if !ok || len(items) == 0 {
		return 0
	}
	name, _ := items[0].(string)
	if len(items) == 2 && name == "Embed" {
		if text, ok := items[1].(string); ok {
			return int64(len(text)) + 128
		}
	}
	var total int64
	for _, item := range items {
		total += rankEmbeddingTokens(item)
	}
	return total
}
