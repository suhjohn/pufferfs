package server

import "testing"

// evalGenerationFilter interprets the Turbopuffer filter AST produced by
// activeGenerationFilter against a single row, so these tests assert the actual
// visibility semantics rather than just the AST shape.
func evalGenerationFilter(t *testing.T, node any, row map[string]int64) bool {
	t.Helper()
	clause, ok := node.([]any)
	if !ok {
		t.Fatalf("filter node is not []any: %#v", node)
	}
	switch clause[0] {
	case "And":
		children := clause[1].([]any)
		for _, c := range children {
			if !evalGenerationFilter(t, c, row) {
				return false
			}
		}
		return true
	case "Or":
		children := clause[1].([]any)
		for _, c := range children {
			if evalGenerationFilter(t, c, row) {
				return true
			}
		}
		return false
	default:
		field := clause[0].(string)
		op := clause[1].(string)
		want := toInt64(t, clause[2])
		got := row[field]
		switch op {
		case "Lte":
			return got <= want
		case "Gt":
			return got > want
		case "Eq":
			return got == want
		default:
			t.Fatalf("unexpected op %q", op)
			return false
		}
	}
}

func toInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	default:
		t.Fatalf("filter value is not an integer: %#v", v)
		return 0
	}
}

// TestActiveGenerationFilterFailsClosedAtZero is the regression guard for the
// fail-open query bug: when a root has no visible generation (visibleSeq == 0),
// the query handler still applies activeGenerationFilter, and that filter must
// match no rows. Every indexed row has valid_from_generation_seq >= 1 (the seq
// column is BIGSERIAL), so an in-flight or failed first sync is never served.
func TestActiveGenerationFilterFailsClosedAtZero(t *testing.T) {
	filter := activeGenerationFilter(0)

	rows := []map[string]int64{
		{"valid_from_generation_seq": 1, "valid_to_generation_seq": 0}, // open row from gen 1
		{"valid_from_generation_seq": 5, "valid_to_generation_seq": 0}, // open row from gen 5
		{"valid_from_generation_seq": 2, "valid_to_generation_seq": 4}, // closed row
	}
	for i, row := range rows {
		if evalGenerationFilter(t, filter, row) {
			t.Fatalf("row %d matched at visibleSeq=0; expected fail-closed (no rows visible)", i)
		}
	}
}

// TestActiveGenerationFilterVisibilityWindow verifies the normal-path semantics:
// a row is visible at seq S iff valid_from <= S and (valid_to == 0 or valid_to > S).
func TestActiveGenerationFilterVisibilityWindow(t *testing.T) {
	cases := []struct {
		name      string
		seq       int64
		validFrom int64
		validTo   int64
		want      bool
	}{
		{"open row at its own generation", 1, 1, 0, true},
		{"open row at a later generation", 5, 1, 0, true},
		{"row not yet visible", 2, 3, 0, false},
		{"closed row before close", 3, 1, 4, true},
		{"closed row at close generation", 4, 1, 4, false},
		{"closed row after close", 5, 1, 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := activeGenerationFilter(tc.seq)
			row := map[string]int64{
				"valid_from_generation_seq": tc.validFrom,
				"valid_to_generation_seq":   tc.validTo,
			}
			if got := evalGenerationFilter(t, filter, row); got != tc.want {
				t.Fatalf("visible(seq=%d, from=%d, to=%d) = %v, want %v",
					tc.seq, tc.validFrom, tc.validTo, got, tc.want)
			}
		})
	}
}
