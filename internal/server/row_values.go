package server

import (
	"encoding/json"
	"strconv"
)

func intFromAny(value any, fallback int) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, err := strconv.Atoi(v.String())
		if err == nil {
			return n
		}
	}
	return fallback
}
