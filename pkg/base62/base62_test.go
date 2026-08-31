package base62_test

import (
	"math"
	"testing"

	"github.com/hokagedno/urlshortener/pkg/base62"
)

// Табличный тест — идиоматичный для Go способ покрыть много случаев одним телом.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   int64
	}{
		{"zero", 0},
		{"one", 1},
		{"base-1", 61},
		{"base", 62},
		{"typical id", 1_000_000},
		{"max int64", math.MaxInt64},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := base62.Encode(tc.in)
			if !base62.IsValid(encoded) {
				t.Fatalf("Encode(%d) = %q, строка не проходит IsValid", tc.in, encoded)
			}
			got, err := base62.Decode(encoded)
			if err != nil {
				t.Fatalf("Decode(%q) вернул ошибку: %v", encoded, err)
			}
			if got != tc.in {
				t.Errorf("round-trip: получили %d, ожидали %d (код %q)", got, tc.in, encoded)
			}
		})
	}
}

func TestEncodeIsMonotonicAndUnique(t *testing.T) {
	seen := make(map[string]int64, 10000)
	for i := int64(0); i < 10000; i++ {
		code := base62.Encode(i)
		if prev, dup := seen[code]; dup {
			t.Fatalf("коллизия: %d и %d дают один код %q", prev, i, code)
		}
		seen[code] = i
	}
}

func TestDecodeRejectsInvalid(t *testing.T) {
	for _, s := range []string{"", "hello world", "abc-def", "тест"} {
		if _, err := base62.Decode(s); err == nil {
			t.Errorf("Decode(%q) должен был вернуть ошибку", s)
		}
	}
}

// Бенчмарк: go test -bench=. ./pkg/base62/
func BenchmarkEncode(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = base62.Encode(int64(i))
	}
}
