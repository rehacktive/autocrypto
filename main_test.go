package main

import "testing"

func TestSMA(t *testing.T) {
	got, ok := sma([]float64{1, 2, 3, 4, 5}, 3)
	if !ok {
		t.Fatal("expected SMA to be available")
	}
	if got != 4 {
		t.Fatalf("expected 4, got %v", got)
	}
}

func TestRSIUpOnly(t *testing.T) {
	got, ok := rsi([]float64{1, 2, 3, 4, 5, 6}, 5)
	if !ok {
		t.Fatal("expected RSI to be available")
	}
	if got != 100 {
		t.Fatalf("expected 100, got %v", got)
	}
}

func TestRealizedVolatilityFlat(t *testing.T) {
	got, ok := realizedVolatility([]float64{10, 10, 10, 10, 10, 10}, 5)
	if !ok {
		t.Fatal("expected volatility to be available")
	}
	if got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}
