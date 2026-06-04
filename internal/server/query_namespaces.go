package server

import (
	"fmt"
	"sync"

	"github.com/pufferfs/pufferfs/pkg/models"
)

type namespaceQueryFunc func(namespace string) ([]map[string]any, error)

func queryRootIndexNamespaces(namespaces []models.RootIndexNamespace, limit int, fn namespaceQueryFunc) ([]map[string]any, error) {
	active := activeRootIndexNamespaces(namespaces)
	if len(active) == 0 {
		return nil, fmt.Errorf("root has no active index namespaces")
	}
	if len(active) == 1 {
		rows, err := fn(active[0].Namespace)
		if err != nil {
			return nil, fmt.Errorf("querying turbopuffer namespace %s: %w", active[0].Namespace, err)
		}
		if limit > 0 && len(rows) > limit {
			return rows[:limit], nil
		}
		return rows, nil
	}

	resultSets := make([][]map[string]any, len(active))
	errs := make([]error, len(active))
	var wg sync.WaitGroup
	for i, ns := range active {
		wg.Add(1)
		go func(i int, namespace string) {
			defer wg.Done()
			rows, err := fn(namespace)
			if err != nil {
				errs[i] = fmt.Errorf("querying turbopuffer namespace %s: %w", namespace, err)
				return
			}
			resultSets[i] = rows
		}(i, ns.Namespace)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	rows := reciprocalRankFusion(resultSets, 60)
	if limit > 0 && len(rows) > limit {
		return rows[:limit], nil
	}
	return rows, nil
}
