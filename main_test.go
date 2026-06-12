package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
