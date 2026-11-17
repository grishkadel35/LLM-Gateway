package main

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/pricing"
)

func TestMixIsDeterministic(t *testing.T) {
	a, b := mix(1, 1000, 200), mix(1, 1000, 200)
	if !slices.Equal(a, b) {
		t.Fatal("two mixes from seed 1 differ")
	}
	if slices.Equal(a, mix(2, 1000, 200)) {
		t.Fatal("the mixes from seeds 1 and 2 are the same")
	}
	for _, p := range a {
		if p < 0 || p >= 200 {
			t.Fatalf("prompt %d is outside [0, 200)", p)
		}
	}
}

// Zipf(s) makes prompt k about (k+1)^-s as frequent as prompt 0.
func TestMixIsZipfShaped(t *testing.T) {
	counts := make([]float64, 200)
	for _, p := range mix(1, 200_000, 200) {
		counts[p]++
	}
	for k := 1; k < 10; k++ {
		if counts[k] >= counts[k-1] {
			t.Errorf("prompt %d drawn %v times, prompt %d %v: not falling", k, counts[k], k-1, counts[k-1])
		}
	}
	for _, k := range []int{1, 4, 9} {
		want := math.Pow(float64(k+1), zipfS)
		if got := counts[0] / counts[k]; math.Abs(got/want-1) > 0.1 {
			t.Errorf("prompt 0 / prompt %d = %.2f, want %.2f within 10%%", k, got, want)
		}
	}
}

func TestPercentile(t *testing.T) {
	hundred := make([]float64, 100)
	for i := range hundred {
		hundred[i] = float64(100 - i)
	}
	three := []float64{5, 1, 3}
	tests := []struct {
		xs   []float64
		p    float64
		want float64
	}{
		{hundred, 50, 50},
		{hundred, 95, 95},
		{hundred, 99, 99},
		{hundred, 100, 100},
		{hundred, 0, 1},
		{three, 50, 3},
		{three, 34, 3},
		{three, 33, 1},
		{three, 99, 5},
		{[]float64{7}, 99, 7},
		{nil, 50, 0},
	}
	for _, tt := range tests {
		if got := percentile(tt.xs, tt.p); got != tt.want {
			t.Errorf("percentile(%v, %v) = %v, want %v", tt.xs, tt.p, got, tt.want)
		}
	}
	if !slices.Equal(three, []float64{5, 1, 3}) {
		t.Errorf("percentile reordered its input: %v", three)
	}
}

// The stream request is a JSON object with no media, so usage.Request has
// its input estimate as ceil(bytes / 4) and its budget reserve as
// ceil(bytes / 3), and its output cap is its max_tokens.
func TestStreamCharge(t *testing.T) {
	got, err := streamCharge()
	if err != nil {
		t.Fatal(err)
	}
	if want := ceilDiv(len(streamBody), 4) + 256; got != want {
		t.Errorf("streamCharge() = %d, want %d", got, want)
	}
}

func TestStreamReservation(t *testing.T) {
	at := time.Now()
	got, err := streamReservation(at)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := pricing.Reserve("openai", "gpt-4o-mini", ceilDiv(len(streamBody), 3), 256, 1, at)
	if !ok || got != want {
		t.Errorf("streamReservation() = %d, want %d (priced %v)", got, want, ok)
	}
}

// A model with no price gets 400 unknown_model on a paid route.
func TestCacheModelsArePriced(t *testing.T) {
	for _, m := range cacheModels {
		if _, ok := pricing.Reserve(m.provider, m.model, 1, 16, 1, time.Now()); !ok {
			t.Errorf("%s %s has no price", m.provider, m.model)
		}
	}
}

func ceilDiv(a, b int) int64 {
	return int64((a + b - 1) / b)
}
