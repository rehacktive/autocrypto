package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testConfig() Config {
	return Config{
		Mode:           "paper",
		StartingBudget: 1000,
		AI: AIConfig{
			Enabled:                false,
			RequireApprovalForBuys: true,
			Provider:               "local",
			Model:                  "nvidia/nemotron-3-nano-4b",
		},
		Risk: Risk{
			MaxPositionPct:    0.25,
			MaxTradeRiskPct:   0.01,
			DailyLossLimitPct: 0.03,
			TotalLossStopPct:  0.25,
			MaxTradesPerDay:   4,
			StopLossPct:       0.035,
			TakeProfitPct:     0.07,
		},
		Strategy: Strategy{
			FastSMA:       3,
			SlowSMA:       6,
			RSIPeriod:     4,
			BuyRSIMax:     100,
			SellRSIMin:    74,
			MinConfidence: 0,
		},
	}
}

func candlesFromCloses(closes ...float64) []Candle {
	candles := make([]Candle, 0, len(closes))
	for _, close := range closes {
		candles = append(candles, Candle{
			Open:  close,
			High:  close,
			Low:   close,
			Close: close,
		})
	}
	return candles
}

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

func TestBuildSignalBuySellAndInsufficientHistory(t *testing.T) {
	cfg := testConfig()

	buy := buildSignal("BTCUSDT", candlesFromCloses(
		100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100,
		101, 102, 103,
	), cfg)
	if buy.Action != "buy" {
		t.Fatalf("expected buy signal, got %q with reason %q", buy.Action, buy.Reason)
	}
	if buy.Price != 103 {
		t.Fatalf("expected latest price 103, got %v", buy.Price)
	}

	sell := buildSignal("BTCUSDT", candlesFromCloses(
		100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100,
		99, 98, 97,
	), cfg)
	if sell.Action != "sell" {
		t.Fatalf("expected sell signal, got %q with reason %q", sell.Action, sell.Reason)
	}

	hold := buildSignal("BTCUSDT", candlesFromCloses(100, 101, 102), cfg)
	if hold.Action != "hold" || hold.Reason != "not enough market history" {
		t.Fatalf("expected insufficient-history hold, got %q with reason %q", hold.Action, hold.Reason)
	}
}

func TestFetchHistoricalKlinesUsesDateRange(t *testing.T) {
	previousClient := http.DefaultClient
	t.Cleanup(func() {
		http.DefaultClient = previousClient
	})
	requests := 0
	http.DefaultClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.URL.Query().Get("symbol") != "BTCUSDT" {
				t.Fatalf("unexpected symbol query: %s", req.URL.RawQuery)
			}
			if req.URL.Query().Get("interval") != "1h" {
				t.Fatalf("unexpected interval query: %s", req.URL.RawQuery)
			}
			if req.URL.Query().Get("startTime") == "" || req.URL.Query().Get("endTime") == "" {
				t.Fatalf("expected startTime and endTime query params: %s", req.URL.RawQuery)
			}
			if requests > 1 {
				return jsonResponse(`[]`), nil
			}
			return jsonResponse(`[
				[1781222400000, "100", "101", "99", "100", "10", 1781225999999],
				[1781226000000, "100", "102", "100", "101", "11", 1781229599999]
			]`), nil
		}),
	}

	start, end, err := parseBacktestRange("2026-06-12", "2026-06-12")
	if err != nil {
		t.Fatalf("parseBacktestRange returned error: %v", err)
	}
	candles, err := fetchHistoricalKlines("BTCUSDT", "1h", start, end)
	if err != nil {
		t.Fatalf("fetchHistoricalKlines returned error: %v", err)
	}
	if len(candles) != 2 || candles[1].Close != 101 {
		t.Fatalf("unexpected candles: %#v", candles)
	}
}

func TestParseBacktestRangeIncludesToDateOnly(t *testing.T) {
	start, end, err := parseBacktestRange("2026-06-01", "2026-06-03")
	if err != nil {
		t.Fatalf("parseBacktestRange returned error: %v", err)
	}
	if start.Format(time.RFC3339) != "2026-06-01T00:00:00Z" {
		t.Fatalf("unexpected start %s", start.Format(time.RFC3339))
	}
	if end.Format(time.RFC3339Nano) != "2026-06-03T23:59:59.999Z" {
		t.Fatalf("unexpected end %s", end.Format(time.RFC3339Nano))
	}
}

func TestSimulateBacktestProducesReport(t *testing.T) {
	cfg := testConfig()
	cfg.LookbackLimit = 26
	cfg.Strategy.MinConfidence = 0
	candlesBySymbol := map[string][]Candle{
		"BTCUSDT": historicalCandles(100, 36),
	}
	cfg.Symbols = []string{"BTCUSDT"}

	report := simulateBacktest(cfg, candlesBySymbol, "2026-06-12", "2026-06-13", len(candlesBySymbol["BTCUSDT"]))
	if report.From != "2026-06-12" || report.To != "2026-06-13" {
		t.Fatalf("unexpected report range: %#v", report)
	}
	if report.Cycles == 0 {
		t.Fatal("expected backtest cycles")
	}
	if report.Equity <= 0 {
		t.Fatalf("expected positive final equity, got %v", report.Equity)
	}
	if len(report.DailyReports) == 0 {
		t.Fatal("expected daily reports")
	}
}

func TestFormatBacktestReportIncludesUsefulSummary(t *testing.T) {
	report := BacktestReport{
		From:           "2026-06-01",
		To:             "2026-06-03",
		Interval:       "1h",
		Symbols:        []string{"BTCUSDT", "ETHUSDT"},
		Cycles:         42,
		Cash:           1005,
		Equity:         1005,
		RealizedPnL:    5,
		PerformancePct: 0.5,
		MaxDrawdownPct: -1.2,
		Positions:      map[string]Position{},
		Events: []Event{
			{"time": "2026-06-01T10:00:00Z", "type": "buy", "symbol": "BTCUSDT"},
			{"time": "2026-06-01T12:00:00Z", "type": "sell", "symbol": "BTCUSDT", "price": 101.0, "qty": 1.0, "pnl": 7.0, "reason": "take_profit"},
			{"time": "2026-06-02T10:00:00Z", "type": "buy", "symbol": "ETHUSDT"},
			{"time": "2026-06-02T12:00:00Z", "type": "sell", "symbol": "ETHUSDT", "price": 99.0, "qty": 1.0, "pnl": -2.0, "reason": "strategy_sell"},
		},
		DailyReports: []DailyReport{
			{Date: "2026-06-01", StartEquity: 1000, EndEquity: 1007, PerformancePct: 0.7, RealizedPnL: 7, MaxDrawdownPct: -0.1, TradeCount: 1},
			{Date: "2026-06-02", StartEquity: 1007, EndEquity: 1005, PerformancePct: -0.199, RealizedPnL: -2, MaxDrawdownPct: -0.5, TradeCount: 1},
		},
	}

	text := formatBacktestReport(report)
	for _, want := range []string{
		"Backtest report",
		"Performance: +0.500%",
		"Win rate: 50.00% (1 win / 1 loss)",
		"Profit factor: 3.5000",
		"Peggior trade: ETHUSDT -2.000000",
		"Giorno peggiore: 2026-06-02 -0.199%",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected report to contain %q, got:\n%s", want, text)
		}
	}
}

func TestOptimizationRanksCandidates(t *testing.T) {
	cfg := testConfig()
	cfg.Symbols = []string{"BTCUSDT"}
	cfg.LookbackLimit = 26
	cfg.AI.Enabled = false
	candlesBySymbol := map[string][]Candle{
		"BTCUSDT": historicalCandles(100, 48),
	}

	candidates := generateOptimizationConfigs(cfg, 5)
	if len(candidates) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(candidates))
	}

	results := make([]OptimizationResult, 0, len(candidates))
	for _, candidate := range candidates {
		report := simulateBacktest(candidate, candlesBySymbol, "2026-06-12", "2026-06-13", len(candlesBySymbol["BTCUSDT"]))
		results = append(results, optimizationResult(0, candidate, report))
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if len(results) == 0 {
		t.Fatal("expected optimization results")
	}
	text := formatOptimizationReport(OptimizationReport{
		From:     "2026-06-12",
		To:       "2026-06-13",
		Runs:     len(results),
		Baseline: results[0],
		Best:     results[:1],
	})
	if !strings.Contains(text, "Backtest optimization") || !strings.Contains(text, "Migliori configurazioni") {
		t.Fatalf("unexpected optimization report:\n%s", text)
	}
}

func TestOptimizationSortPrefersValidationQuality(t *testing.T) {
	trainHeroValidationBad := OptimizationResult{
		Rank:             1,
		Score:            100,
		Qualified:        true,
		WalkForwardScore: 15,
		Validation:       &OptimizationResult{Score: -10, Qualified: false, Quality: "rejected"},
	}
	steadyValidated := OptimizationResult{
		Rank:             2,
		Score:            20,
		Qualified:        true,
		WalkForwardScore: 25,
		Validation:       &OptimizationResult{Score: 30, Qualified: true, Quality: "qualified"},
	}

	results := []OptimizationResult{trainHeroValidationBad, steadyValidated}
	sort.Slice(results, func(i, j int) bool {
		return betterOptimizationResult(results[i], results[j])
	})
	if results[0].Rank != steadyValidated.Rank {
		t.Fatalf("expected validated candidate to rank first, got rank %d", results[0].Rank)
	}
}

func TestOptimizationRejectsInactiveLowQualityResult(t *testing.T) {
	cfg := testConfig()
	report := BacktestReport{
		PerformancePct: -0.5,
		BenchmarkPct:   -30,
		AlphaPct:       29.5,
		MaxDrawdownPct: -0.5,
		Events: []Event{
			{"type": "sell", "symbol": "BTCUSDT", "pnl": -1.0, "reason": "strategy_sell", "time": "2026-01-02T10:00:00Z"},
			{"type": "sell", "symbol": "ETHUSDT", "pnl": -2.0, "reason": "strategy_sell", "time": "2026-01-03T10:00:00Z"},
		},
	}

	result := optimizationResultWithQuality(0, cfg, report, 20)
	if result.Qualified {
		t.Fatalf("expected low-sample losing result to be rejected: %#v", result)
	}
	if !strings.Contains(result.Quality, "low sample") || !strings.Contains(result.Quality, "profit factor") {
		t.Fatalf("expected quality reason to explain rejection, got %q", result.Quality)
	}
	if result.Score > 0 {
		t.Fatalf("expected penalties to push score below zero, got %v", result.Score)
	}
}

func TestRecommendedOptimizationResultPrefersQualified(t *testing.T) {
	rejected := OptimizationResult{Rank: 1, Score: 100, Qualified: false, Quality: "rejected"}
	qualified := OptimizationResult{Rank: 2, Score: 10, Qualified: true, Quality: "ok"}

	got, ok := recommendedOptimizationResult([]OptimizationResult{rejected, qualified})
	if !ok {
		t.Fatal("expected recommended result")
	}
	if got.Rank != qualified.Rank {
		t.Fatalf("expected qualified result to be preferred, got rank %d", got.Rank)
	}
}

func TestRecommendedOptimizationResultPrefersWalkForwardQualified(t *testing.T) {
	trainOnly := OptimizationResult{
		Rank:       1,
		Qualified:  true,
		Validation: &OptimizationResult{Qualified: false, Quality: "rejected"},
	}
	walkForward := OptimizationResult{
		Rank:       2,
		Qualified:  true,
		Validation: &OptimizationResult{Qualified: true, Quality: "qualified"},
	}

	got, ok := recommendedOptimizationResult([]OptimizationResult{trainOnly, walkForward})
	if !ok {
		t.Fatal("expected recommended result")
	}
	if got.Rank != walkForward.Rank {
		t.Fatalf("expected walk-forward qualified result, got rank %d", got.Rank)
	}
}

func TestApplyOptimizationResultUpdatesRiskAndStrategyOnly(t *testing.T) {
	cfg := testConfig()
	cfg.Symbols = []string{"BTCUSDT", "ETHUSDT"}
	cfg.Costs = Costs{FeePct: 0.001, SlippagePct: 0.0005}
	cfg.AI.Enabled = true
	result := OptimizationResult{
		FastSMA:        8,
		SlowSMA:        32,
		BuyRSIMax:      45,
		SellRSIMin:     68,
		MinConfidence:  0.7,
		StopLossPct:    0.05,
		TakeProfitPct:  0.1,
		MaxPositionPct: 0.1,
	}

	optimized := applyOptimizationResult(cfg, result)
	if optimized.Strategy.FastSMA != 8 || optimized.Strategy.SlowSMA != 32 || optimized.Strategy.BuyRSIMax != 45 || optimized.Strategy.SellRSIMin != 68 || optimized.Strategy.MinConfidence != 0.7 {
		t.Fatalf("strategy was not updated from optimization result: %#v", optimized.Strategy)
	}
	if optimized.Risk.StopLossPct != 0.05 || optimized.Risk.TakeProfitPct != 0.1 || optimized.Risk.MaxPositionPct != 0.1 {
		t.Fatalf("risk was not updated from optimization result: %#v", optimized.Risk)
	}
	if optimized.Strategy.RSIPeriod != cfg.Strategy.RSIPeriod || optimized.Risk.MaxTradeRiskPct != cfg.Risk.MaxTradeRiskPct || !optimized.AI.Enabled || optimized.Costs != cfg.Costs || len(optimized.Symbols) != len(cfg.Symbols) {
		t.Fatalf("unexpected non-optimized fields changed: %#v", optimized)
	}
}

func TestWriteOptimizedConfig(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "config.json")
	outputPath := filepath.Join(dir, "config.optimized.json")
	cfg := testConfig()
	cfg.AI.Enabled = true
	if err := writeJSON(inputPath, cfg); err != nil {
		t.Fatalf("write input config: %v", err)
	}
	report := OptimizationReport{
		Best: []OptimizationResult{{
			Qualified:      true,
			FastSMA:        8,
			SlowSMA:        32,
			BuyRSIMax:      45,
			SellRSIMin:     68,
			MinConfidence:  0.7,
			StopLossPct:    0.05,
			TakeProfitPct:  0.1,
			MaxPositionPct: 0.1,
		}},
	}

	if err := writeOptimizedConfig(inputPath, outputPath, report); err != nil {
		t.Fatalf("write optimized config: %v", err)
	}
	var optimized Config
	if err := readJSON(outputPath, &optimized); err != nil {
		t.Fatalf("read optimized config: %v", err)
	}
	if optimized.Strategy.FastSMA != 8 || optimized.Risk.MaxPositionPct != 0.1 {
		t.Fatalf("optimized config did not contain selected parameters: %#v", optimized)
	}
	if !optimized.AI.Enabled {
		t.Fatalf("expected non-risk/strategy settings to be preserved")
	}
}

func historicalCandles(startPrice float64, count int) []Candle {
	candles := make([]Candle, 0, count)
	start := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		price := startPrice + float64(i)
		openTime := start.Add(time.Duration(i) * time.Hour)
		candles = append(candles, Candle{
			OpenTime:  openTime.UnixMilli(),
			Open:      price,
			High:      price + 1,
			Low:       price - 1,
			Close:     price,
			Volume:    10,
			CloseTime: openTime.Add(time.Hour - time.Millisecond).UnixMilli(),
		})
	}
	return candles
}

func TestMaybeEnterPositionBuysWithinRiskLimits(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash:            1000,
		Positions:       map[string]Position{},
		TradeCountByDay: map[string]int{},
	}
	signal := Signal{Symbol: "BTCUSDT", Action: "buy", Price: 100, Confidence: 0.8}
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")

	event, err := maybeEnterPosition(&state, cfg, &signal, journalPath)
	if err != nil {
		t.Fatalf("maybeEnterPosition returned error: %v", err)
	}
	if event == nil || event["type"] != "buy" {
		t.Fatalf("expected buy event, got %#v", event)
	}
	if state.Cash != 750 {
		t.Fatalf("expected cash 750, got %v", state.Cash)
	}
	position := state.Positions["BTCUSDT"]
	if position.Cost != 250 || position.Qty != 2.5 {
		t.Fatalf("unexpected position: %#v", position)
	}
	if state.TradeCountByDay[todayKey()] != 1 {
		t.Fatalf("expected today's trade count to be 1, got %d", state.TradeCountByDay[todayKey()])
	}
	if signal.AIReview == nil || !signal.AIReview.Approved {
		t.Fatalf("expected disabled AI reviewer to approve, got %#v", signal.AIReview)
	}
	if info, err := os.Stat(journalPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected journal entry, info=%#v err=%v", info, err)
	}
}

func TestTradeCostsApplyFeeAndSlippage(t *testing.T) {
	cfg := testConfig()
	cfg.Costs = Costs{FeePct: 0.001, SlippagePct: 0.01}
	state := State{
		Cash:            1000,
		Positions:       map[string]Position{},
		TradeCountByDay: map[string]int{},
	}
	signal := Signal{Symbol: "BTCUSDT", Action: "buy", Price: 100, Confidence: 0.8}

	event, err := maybeEnterPositionAt(&state, cfg, &signal, "", "2026-06-12T10:00:00Z")
	if err != nil {
		t.Fatalf("maybeEnterPositionAt returned error: %v", err)
	}
	if event == nil || eventFloat(event, "price") != 101 {
		t.Fatalf("expected slippage-adjusted buy event, got %#v", event)
	}
	if state.Cash != 749.75 {
		t.Fatalf("expected cash 749.75 after fee, got %v", state.Cash)
	}

	exit := Signal{Symbol: "BTCUSDT", Action: "sell", Price: 100}
	event, err = maybeExitPositionAt(&state, cfg, exit, "", "2026-06-12T11:00:00Z")
	if err != nil {
		t.Fatalf("maybeExitPositionAt returned error: %v", err)
	}
	if event == nil || eventFloat(event, "price") != 99 {
		t.Fatalf("expected slippage-adjusted sell event, got %#v", event)
	}
	if eventFloat(event, "pnl") >= 0 {
		t.Fatalf("expected costs to make this round trip negative, got %#v", event)
	}
}

func TestMaybeEnterPositionBlocksWhenTradeLimitReached(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash:            1000,
		Positions:       map[string]Position{},
		TradeCountByDay: map[string]int{todayKey(): cfg.Risk.MaxTradesPerDay},
	}
	signal := Signal{Symbol: "BTCUSDT", Action: "buy", Price: 100, Confidence: 0.8}

	event, err := maybeEnterPosition(&state, cfg, &signal, filepath.Join(t.TempDir(), "journal.jsonl"))
	if err != nil {
		t.Fatalf("maybeEnterPosition returned error: %v", err)
	}
	if event != nil {
		t.Fatalf("expected no event when trade limit is reached, got %#v", event)
	}
	if _, exists := state.Positions["BTCUSDT"]; exists {
		t.Fatal("did not expect a position to be opened")
	}
}

func TestMaybeEnterPositionBlocksRejectedAISignal(t *testing.T) {
	cfg := testConfig()
	cfg.AI.Enabled = true
	state := State{
		Cash:            1000,
		Positions:       map[string]Position{},
		TradeCountByDay: map[string]int{},
	}
	signal := Signal{
		Symbol:     "BTCUSDT",
		Action:     "buy",
		Price:      100,
		Confidence: 0.8,
		AIReview:   &AIReview{Approved: false, Confidence: 0.2, Reason: "weak setup"},
	}
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")

	event, err := maybeEnterPosition(&state, cfg, &signal, journalPath)
	if err != nil {
		t.Fatalf("maybeEnterPosition returned error: %v", err)
	}
	if event != nil {
		t.Fatalf("expected rejected buy to return no trade event, got %#v", event)
	}
	if _, exists := state.Positions["BTCUSDT"]; exists {
		t.Fatal("did not expect rejected AI signal to open a position")
	}
	if info, err := os.Stat(journalPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected blocked buy journal entry, info=%#v err=%v", info, err)
	}
}

func TestMaybeExitPositionAppliesTakeProfit(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash: 100,
		Positions: map[string]Position{
			"BTCUSDT": {EntryPrice: 100, Qty: 2, Cost: 200},
		},
	}
	signal := Signal{Symbol: "BTCUSDT", Action: "hold", Price: 108}
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")

	event, err := maybeExitPosition(&state, cfg, signal, journalPath)
	if err != nil {
		t.Fatalf("maybeExitPosition returned error: %v", err)
	}
	if event == nil || event["type"] != "sell" || event["reason"] != "take_profit" {
		t.Fatalf("expected take-profit sell event, got %#v", event)
	}
	if state.Cash != 316 {
		t.Fatalf("expected cash 316, got %v", state.Cash)
	}
	if state.RealizedPnL != 16 {
		t.Fatalf("expected realized PnL 16, got %v", state.RealizedPnL)
	}
	if _, exists := state.Positions["BTCUSDT"]; exists {
		t.Fatal("expected position to be closed")
	}
}

func TestMaybeExitPositionSkipsRejectedStrategySell(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash: 100,
		Positions: map[string]Position{
			"BTCUSDT": {EntryPrice: 100, Qty: 2, Cost: 200},
		},
	}
	signal := Signal{
		Symbol:   "BTCUSDT",
		Action:   "sell",
		Price:    101,
		AIReview: &AIReview{Approved: false, Confidence: 0.3, Reason: "sell not justified"},
	}

	event, err := maybeExitPosition(&state, cfg, signal, filepath.Join(t.TempDir(), "journal.jsonl"))
	if err != nil {
		t.Fatalf("maybeExitPosition returned error: %v", err)
	}
	if event != nil {
		t.Fatalf("expected rejected strategy sell to be skipped, got %#v", event)
	}
	if _, exists := state.Positions["BTCUSDT"]; !exists {
		t.Fatal("expected position to remain open")
	}
}

func TestMaybeExitPositionRiskExitIgnoresRejectedAI(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash: 100,
		Positions: map[string]Position{
			"BTCUSDT": {EntryPrice: 100, Qty: 2, Cost: 200},
		},
	}
	signal := Signal{
		Symbol:   "BTCUSDT",
		Action:   "hold",
		Price:    95,
		AIReview: &AIReview{Approved: false, Confidence: 0.1, Reason: "do not sell"},
	}

	event, err := maybeExitPosition(&state, cfg, signal, filepath.Join(t.TempDir(), "journal.jsonl"))
	if err != nil {
		t.Fatalf("maybeExitPosition returned error: %v", err)
	}
	if event == nil || event["reason"] != "stop_loss" {
		t.Fatalf("expected stop-loss exit despite rejected AI review, got %#v", event)
	}
}

func TestAnnotateSignalExecutionReasonsForIgnoredSell(t *testing.T) {
	signals := []Signal{
		{Symbol: "BTCUSDT", Action: "sell"},
		{Symbol: "ETHUSDT", Action: "sell"},
		{Symbol: "SOLUSDT", Action: "hold"},
	}
	state := State{
		Positions: map[string]Position{
			"ETHUSDT": {EntryPrice: 100, Qty: 1, Cost: 100},
		},
	}

	annotateSignalExecutionReasons(signals, state)
	if signals[0].ExecutionReason != "sell signal ignored: no open position" {
		t.Fatalf("expected ignored sell reason, got %q", signals[0].ExecutionReason)
	}
	if signals[1].ExecutionReason != "" {
		t.Fatalf("did not expect reason for sell with open position, got %q", signals[1].ExecutionReason)
	}
	if signals[2].ExecutionReason != "" {
		t.Fatalf("did not expect reason for hold, got %q", signals[2].ExecutionReason)
	}
}

func TestEstimateEquityAndHalts(t *testing.T) {
	cfg := testConfig()
	state := State{
		Cash: 500,
		Positions: map[string]Position{
			"BTCUSDT": {EntryPrice: 100, Qty: 2},
			"ETHUSDT": {EntryPrice: 50, Qty: 1},
		},
		DayStartEquity: map[string]float64{},
	}

	equity := estimateEquity(state, map[string]float64{"BTCUSDT": 110})
	if equity != 770 {
		t.Fatalf("expected equity 770, got %v", equity)
	}

	applyHalts(&state, cfg, 900)
	if state.Halted {
		t.Fatalf("did not expect halt, got %q", *state.HaltReason)
	}
	if state.DayStartEquity[todayKey()] != 900 {
		t.Fatalf("expected day start equity 900, got %v", state.DayStartEquity[todayKey()])
	}

	applyHalts(&state, cfg, 870)
	if !state.Halted || state.HaltReason == nil || *state.HaltReason != "daily loss limit reached" {
		t.Fatalf("expected daily loss halt, halted=%v reason=%v", state.Halted, state.HaltReason)
	}
}

func TestLoadStateInitializesMissingMaps(t *testing.T) {
	cfg := testConfig()
	path := filepath.Join(t.TempDir(), "state.json")

	state, err := loadState(path, cfg)
	if err != nil {
		t.Fatalf("loadState for missing file returned error: %v", err)
	}
	if state.Cash != cfg.StartingBudget || state.HighWatermark != cfg.StartingBudget {
		t.Fatalf("unexpected initial state: %#v", state)
	}

	if err := writeJSON(path, State{Cash: 12}); err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}
	state, err = loadState(path, cfg)
	if err != nil {
		t.Fatalf("loadState for existing file returned error: %v", err)
	}
	if state.Positions == nil || state.TradeCountByDay == nil || state.DayStartEquity == nil {
		t.Fatalf("expected maps to be initialized: %#v", state)
	}
}

func TestAIReviewSignalDisabledAndUnsupportedProvider(t *testing.T) {
	cfg := testConfig()
	state := State{Cash: 1000, Positions: map[string]Position{}}
	signal := Signal{Symbol: "BTCUSDT", Action: "buy", Price: 100}

	review := aiReviewSignal(cfg, signal, state)
	if !review.Approved || review.Confidence != 1 {
		t.Fatalf("expected disabled AI reviewer to approve, got %#v", review)
	}

	cfg.AI.Enabled = true
	cfg.AI.Provider = "unknown"
	review = aiReviewSignal(cfg, signal, state)
	if review.Approved || review.Reason != "unsupported AI provider" {
		t.Fatalf("expected unsupported provider rejection, got %#v", review)
	}
}

func TestAIReviewSignalNeverBlocksHoldWhenModelRejectsIt(t *testing.T) {
	cfg := testConfig()
	cfg.AI.Enabled = true
	signal := Signal{Symbol: "BTCUSDT", Action: "hold", Price: 100}
	state := State{Cash: 1000, Positions: map[string]Position{}}

	previousClient := http.DefaultClient
	t.Cleanup(func() {
		http.DefaultClient = previousClient
	})
	http.DefaultClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(`{
				"choices": [
					{
						"message": {
							"content": "{\"approved\":false,\"confidence\":0.48,\"reason\":\"low confidence and no directional bias\"}"
						}
					}
				]
			}`), nil
		}),
	}

	review := aiReviewSignal(cfg, signal, state)
	if !review.Approved {
		t.Fatalf("expected hold review to be accepted, got %#v", review)
	}
	if review.Reason != "Hold accepted: low confidence and no directional bias" {
		t.Fatalf("unexpected hold reason %q", review.Reason)
	}
}

func TestReviewSignalsAnnotatesSignals(t *testing.T) {
	cfg := testConfig()
	state := State{Cash: 1000, Positions: map[string]Position{}}
	signals := []Signal{
		{Symbol: "BTCUSDT", Action: "buy", Price: 100},
		{Symbol: "ETHUSDT", Action: "hold", Price: 50},
	}

	reviewSignals(cfg, signals, state)
	for _, signal := range signals {
		if signal.AIReview == nil {
			t.Fatalf("expected AI review for signal %#v", signal)
		}
		if !signal.AIReview.Approved || signal.AIReview.Reason != "AI reviewer disabled" {
			t.Fatalf("unexpected AI review for signal %#v", signal)
		}
	}
}

func TestJSONLReadersKeepNewestRows(t *testing.T) {
	dir := t.TempDir()
	equityPath := filepath.Join(dir, "equity.jsonl")
	journalPath := filepath.Join(dir, "journal.jsonl")

	for i := 1; i <= 4; i++ {
		if err := appendJSONL(equityPath, EquityRow{Time: "t", Cash: float64(i), Equity: float64(i * 10)}); err != nil {
			t.Fatalf("append equity row: %v", err)
		}
		if err := appendJSONL(journalPath, Event{"type": "event", "symbol": i}); err != nil {
			t.Fatalf("append journal event: %v", err)
		}
	}

	equityRows, err := readEquityRows(equityPath, 2)
	if err != nil {
		t.Fatalf("readEquityRows returned error: %v", err)
	}
	if len(equityRows) != 2 || equityRows[0].Equity != 30 || equityRows[1].Equity != 40 {
		t.Fatalf("expected newest two equity rows, got %#v", equityRows)
	}

	journal, err := readJournal(journalPath, 3)
	if err != nil {
		t.Fatalf("readJournal returned error: %v", err)
	}
	if len(journal) != 3 || journal[0]["symbol"].(float64) != 2 || journal[2]["symbol"].(float64) != 4 {
		t.Fatalf("expected newest three journal events, got %#v", journal)
	}
}

func TestHandleStatusReturnsDashboardData(t *testing.T) {
	dir := t.TempDir()
	if err := appendJSONL(filepath.Join(dir, "equity.jsonl"), EquityRow{Time: "now", Cash: 900, Equity: 1100}); err != nil {
		t.Fatalf("append equity row: %v", err)
	}
	if err := appendJSONL(filepath.Join(dir, "journal.jsonl"), Event{"type": "buy", "symbol": "BTCUSDT"}); err != nil {
		t.Fatalf("append journal event: %v", err)
	}
	if err := writeJSON(filepath.Join(dir, "daily_report.json"), DailyReport{Date: todayKey(), EndEquity: 1100}); err != nil {
		t.Fatalf("write daily report: %v", err)
	}
	report := &Report{Mode: "paper", Cash: 900, Equity: 1100}
	app := &DashboardApp{
		baseDir:    dir,
		interval:   time.Minute,
		lastReport: report,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	app.handleStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var status StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.Report == nil || status.Report.Equity != 1100 {
		t.Fatalf("unexpected report in status response: %#v", status.Report)
	}
	if status.DailyReport == nil || status.DailyReport.EndEquity != 1100 {
		t.Fatalf("unexpected daily report in status response: %#v", status.DailyReport)
	}
	if len(status.History) != 1 || len(status.Journal) != 1 || status.CycleCount != 1 {
		t.Fatalf("unexpected history/journal response: %#v", status)
	}
	if status.NextInterval != "1m0s" {
		t.Fatalf("expected interval 1m0s, got %q", status.NextInterval)
	}
}

func TestBuildDailyReportSummarizesPerformanceAndWorstTrades(t *testing.T) {
	dir := t.TempDir()
	equityPath := filepath.Join(dir, "equity.jsonl")
	journalPath := filepath.Join(dir, "journal.jsonl")
	date := "2026-06-12"

	for _, row := range []EquityRow{
		{Time: "2026-06-11T23:55:00Z", Equity: 950, RealizedPnL: -50},
		{Time: "2026-06-12T08:00:00Z", Equity: 1000, RealizedPnL: 0},
		{Time: "2026-06-12T12:00:00Z", Equity: 1100, RealizedPnL: 20},
		{Time: "2026-06-12T16:00:00Z", Equity: 990, RealizedPnL: -10},
		{Time: "2026-06-12T20:00:00Z", Equity: 1050, RealizedPnL: 5},
	} {
		if err := appendJSONL(equityPath, row); err != nil {
			t.Fatalf("append equity row: %v", err)
		}
	}

	for _, event := range []Event{
		{"time": "2026-06-12T09:00:00Z", "type": "buy", "symbol": "BTCUSDT"},
		{"time": "2026-06-12T10:00:00Z", "type": "sell", "symbol": "BTCUSDT", "price": 100.0, "qty": 1.0, "pnl": -12.5, "reason": "stop_loss"},
		{"time": "2026-06-12T11:00:00Z", "type": "sell", "symbol": "ETHUSDT", "price": 50.0, "qty": 2.0, "pnl": 7.0, "reason": "take_profit"},
		{"time": "2026-06-12T12:00:00Z", "type": "sell", "symbol": "SOLUSDT", "price": 25.0, "qty": 4.0, "pnl": -20.0, "reason": "strategy_sell"},
		{"time": "2026-06-13T10:00:00Z", "type": "sell", "symbol": "BNBUSDT", "pnl": -99.0, "reason": "other_day"},
	} {
		if err := appendJSONL(journalPath, event); err != nil {
			t.Fatalf("append journal event: %v", err)
		}
	}

	report, err := buildDailyReport(equityPath, journalPath, date)
	if err != nil {
		t.Fatalf("buildDailyReport returned error: %v", err)
	}
	if report.Date != date {
		t.Fatalf("expected date %s, got %s", date, report.Date)
	}
	if report.StartEquity != 1000 || report.EndEquity != 1050 {
		t.Fatalf("unexpected equity range: %#v", report)
	}
	if report.PerformancePct != 5 {
		t.Fatalf("expected performance 5, got %v", report.PerformancePct)
	}
	if report.MaxDrawdownPct != -10 {
		t.Fatalf("expected max drawdown -10, got %v", report.MaxDrawdownPct)
	}
	if report.TradeCount != 3 || report.RealizedPnL != -25.5 {
		t.Fatalf("unexpected trade summary: %#v", report)
	}
	if len(report.WorstTrades) != 3 || report.WorstTrades[0].Symbol != "SOLUSDT" || report.WorstTrades[1].Symbol != "BTCUSDT" {
		t.Fatalf("worst trades not sorted as expected: %#v", report.WorstTrades)
	}
}

func TestWriteDailyReportPersistsJSON(t *testing.T) {
	dir := t.TempDir()
	equityPath := filepath.Join(dir, "equity.jsonl")
	journalPath := filepath.Join(dir, "journal.jsonl")
	reportPath := filepath.Join(dir, "daily_report.json")

	if err := appendJSONL(equityPath, EquityRow{Time: "2026-06-12T08:00:00Z", Equity: 1000}); err != nil {
		t.Fatalf("append equity row: %v", err)
	}
	report, err := writeDailyReport(reportPath, equityPath, journalPath, "2026-06-12")
	if err != nil {
		t.Fatalf("writeDailyReport returned error: %v", err)
	}
	if report.EndEquity != 1000 {
		t.Fatalf("unexpected report: %#v", report)
	}

	loaded, err := readDailyReport(reportPath)
	if err != nil {
		t.Fatalf("readDailyReport returned error: %v", err)
	}
	if loaded == nil || loaded.Date != "2026-06-12" || loaded.EndEquity != 1000 {
		t.Fatalf("unexpected persisted daily report: %#v", loaded)
	}
}

func TestMustFloatStringParsesAndPanicsOnInvalidType(t *testing.T) {
	if got := mustFloatString("12.34"); got != 12.34 {
		t.Fatalf("expected 12.34, got %v", got)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-string input")
		}
	}()
	_ = mustFloatString(12.34)
}

func TestLocalAIReviewWithLocalModel(t *testing.T) {
	previousClient := http.DefaultClient
	t.Cleanup(func() {
		http.DefaultClient = previousClient
	})

	http.DefaultClient = &http.Client{
		Timeout: 45 * time.Second,
	}

	review, err := localAIReview("nvidia/nemotron-3-nano-4b", map[string]any{
		"signal": Signal{
			Symbol:     "BTCUSDT",
			Action:     "buy",
			Confidence: 0.75,
			Price:      100,
			Reason:     "test signal",
		},
	})
	if err != nil {
		t.Fatalf("localAIReview returned error; is the local AI server running at %s? %v", localAIAPI, err)
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		t.Fatalf("expected confidence between 0 and 1, got %v", review.Confidence)
	}
	if review.Reason == "" {
		t.Fatal("expected non-empty AI review reason")
	}
}
