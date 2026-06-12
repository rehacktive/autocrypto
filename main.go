package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	binanceAPI = "https://api.binance.com"
	localAIAPI = "http://127.0.0.1:1234/v1/chat/completions"
	userAgent  = "crypto-ai-paper-bot-go/0.1"
)

type Config struct {
	Mode           string   `json:"mode"`
	QuoteAsset     string   `json:"quote_asset"`
	StartingBudget float64  `json:"starting_budget"`
	Symbols        []string `json:"symbols"`
	Interval       string   `json:"interval"`
	LookbackLimit  int      `json:"lookback_limit"`
	AI             AIConfig `json:"ai"`
	Risk           Risk     `json:"risk"`
	Strategy       Strategy `json:"strategy"`
}

type AIConfig struct {
	Enabled                bool   `json:"enabled"`
	RequireApprovalForBuys bool   `json:"require_approval_for_buys"`
	Provider               string `json:"provider"`
	Model                  string `json:"model"`
}

type Risk struct {
	MaxPositionPct    float64 `json:"max_position_pct"`
	MaxTradeRiskPct   float64 `json:"max_trade_risk_pct"`
	DailyLossLimitPct float64 `json:"daily_loss_limit_pct"`
	TotalLossStopPct  float64 `json:"total_loss_stop_pct"`
	MaxTradesPerDay   int     `json:"max_trades_per_day"`
	StopLossPct       float64 `json:"stop_loss_pct"`
	TakeProfitPct     float64 `json:"take_profit_pct"`
}

type Strategy struct {
	FastSMA       int     `json:"fast_sma"`
	SlowSMA       int     `json:"slow_sma"`
	RSIPeriod     int     `json:"rsi_period"`
	BuyRSIMax     float64 `json:"buy_rsi_max"`
	SellRSIMin    float64 `json:"sell_rsi_min"`
	MinConfidence float64 `json:"min_confidence"`
}

type Position struct {
	EntryTime  string  `json:"entry_time"`
	EntryPrice float64 `json:"entry_price"`
	Qty        float64 `json:"qty"`
	Cost       float64 `json:"cost"`
}

type State struct {
	CreatedAt       string              `json:"created_at"`
	Cash            float64             `json:"cash"`
	Positions       map[string]Position `json:"positions"`
	RealizedPnL     float64             `json:"realized_pnl"`
	TradeCountByDay map[string]int      `json:"trade_count_by_day"`
	DayStartEquity  map[string]float64  `json:"day_start_equity"`
	Halted          bool                `json:"halted"`
	HaltReason      *string             `json:"halt_reason"`
	LastEquity      float64             `json:"last_equity"`
	HighWatermark   float64             `json:"high_watermark"`
}

type Candle struct {
	OpenTime  int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	CloseTime int64
}

type Signal struct {
	Symbol     string    `json:"symbol"`
	Action     string    `json:"action"`
	Confidence float64   `json:"confidence"`
	Price      float64   `json:"price"`
	FastSMA    float64   `json:"fast_sma,omitempty"`
	SlowSMA    float64   `json:"slow_sma,omitempty"`
	RSI        float64   `json:"rsi,omitempty"`
	Volatility float64   `json:"volatility,omitempty"`
	Reason     string    `json:"reason"`
	AIReview   *AIReview `json:"ai_review,omitempty"`
}

type AIReview struct {
	Approved   bool    `json:"approved"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type Event map[string]any

type Report struct {
	Time        string              `json:"time"`
	Mode        string              `json:"mode"`
	Cash        float64             `json:"cash"`
	Equity      float64             `json:"equity"`
	RealizedPnL float64             `json:"realized_pnl"`
	DrawdownPct float64             `json:"drawdown_pct"`
	Halted      bool                `json:"halted"`
	HaltReason  *string             `json:"halt_reason"`
	Positions   map[string]Position `json:"positions"`
	Signals     []Signal            `json:"signals"`
	Events      []Event             `json:"events"`
}

type DailyReport struct {
	Date           string       `json:"date"`
	StartEquity    float64      `json:"start_equity"`
	EndEquity      float64      `json:"end_equity"`
	PerformancePct float64      `json:"performance_pct"`
	RealizedPnL    float64      `json:"realized_pnl"`
	MaxDrawdownPct float64      `json:"max_drawdown_pct"`
	TradeCount     int          `json:"trade_count"`
	WorstTrades    []WorstTrade `json:"worst_trades"`
}

type WorstTrade struct {
	Time   string  `json:"time"`
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Qty    float64 `json:"qty"`
	PnL    float64 `json:"pnl"`
	Reason string  `json:"reason"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	once := flag.Bool("once", false, "run a single cycle")
	loop := flag.Bool("loop", false, "run forever")
	serve := flag.Bool("serve", false, "start the local web dashboard")
	addr := flag.String("addr", "127.0.0.1:8787", "web dashboard address")
	sleep := flag.Duration("sleep", 5*time.Minute, "sleep duration between loop cycles")
	flag.Parse()

	if *serve {
		if err := serveDashboard(*configPath, *addr, *sleep); err != nil {
			exitErr(err.Error())
		}
		return
	}

	if !*once && !*loop {
		exitErr("choose --once, --loop, or --serve")
	}

	for {
		report, err := runCycle(*configPath)
		if err != nil {
			exitErr(err.Error())
		}
		if err := printJSON(report); err != nil {
			exitErr(err.Error())
		}
		if !*loop {
			return
		}
		time.Sleep(*sleep)
	}
}

func runCycle(configPath string) (Report, error) {
	var cfg Config
	if err := readJSON(configPath, &cfg); err != nil {
		return Report{}, err
	}
	if cfg.Mode != "paper" {
		return Report{}, errors.New("only paper mode is enabled in this version")
	}

	baseDir := filepath.Dir(absPath(configPath))
	statePath := filepath.Join(baseDir, "state.json")
	journalPath := filepath.Join(baseDir, "journal.jsonl")
	equityPath := filepath.Join(baseDir, "equity.jsonl")
	dailyReportPath := filepath.Join(baseDir, "daily_report.json")

	state, err := loadState(statePath, cfg)
	if err != nil {
		return Report{}, err
	}

	latestPrices := map[string]float64{}
	signals := make([]Signal, 0, len(cfg.Symbols))
	events := []Event{}

	for _, symbol := range cfg.Symbols {
		candles, err := fetchKlines(symbol, cfg.Interval, cfg.LookbackLimit)
		if err != nil {
			return Report{}, fmt.Errorf("fetch %s: %w", symbol, err)
		}
		signal := buildSignal(symbol, candles, cfg)
		latestPrices[symbol] = signal.Price
		signals = append(signals, signal)
	}

	equity := estimateEquity(state, latestPrices)
	state.LastEquity = equity
	if equity > state.HighWatermark {
		state.HighWatermark = equity
	}
	applyHalts(&state, cfg, equity)

	reviewSignals(cfg, signals, state)

	if !state.Halted {
		for i := range signals {
			event, err := maybeExitPosition(&state, cfg, signals[i], journalPath)
			if err != nil {
				return Report{}, err
			}
			if event != nil {
				events = append(events, event)
			}
		}
		for i := range signals {
			event, err := maybeEnterPosition(&state, cfg, &signals[i], journalPath)
			if err != nil {
				return Report{}, err
			}
			if event != nil {
				events = append(events, event)
			}
		}
	}

	finalEquity := estimateEquity(state, latestPrices)
	state.LastEquity = finalEquity
	if finalEquity > state.HighWatermark {
		state.HighWatermark = finalEquity
	}
	if err := writeJSON(statePath, state); err != nil {
		return Report{}, err
	}

	report := Report{
		Time:        time.Now().UTC().Format(time.RFC3339),
		Mode:        cfg.Mode,
		Cash:        round6(state.Cash),
		Equity:      round6(finalEquity),
		RealizedPnL: round6(state.RealizedPnL),
		DrawdownPct: round3((finalEquity/state.HighWatermark - 1) * 100),
		Halted:      state.Halted,
		HaltReason:  state.HaltReason,
		Positions:   state.Positions,
		Signals:     signals,
		Events:      events,
	}
	if err := appendJSONL(equityPath, map[string]any{
		"time":         report.Time,
		"cash":         report.Cash,
		"equity":       report.Equity,
		"realized_pnl": report.RealizedPnL,
		"drawdown_pct": report.DrawdownPct,
	}); err != nil {
		return Report{}, err
	}
	if _, err := writeDailyReport(dailyReportPath, equityPath, journalPath, todayKey()); err != nil {
		return Report{}, err
	}
	return report, nil
}

func fetchKlines(symbol, interval string, limit int) ([]Candle, error) {
	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d", binanceAPI, symbol, interval, limit)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("binance status %d: %s", resp.StatusCode, string(body))
	}

	var raw [][]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	candles := make([]Candle, 0, len(raw))
	for _, row := range raw {
		if len(row) < 7 {
			continue
		}
		candles = append(candles, Candle{
			OpenTime:  int64(row[0].(float64)),
			Open:      mustFloatString(row[1]),
			High:      mustFloatString(row[2]),
			Low:       mustFloatString(row[3]),
			Close:     mustFloatString(row[4]),
			Volume:    mustFloatString(row[5]),
			CloseTime: int64(row[6].(float64)),
		})
	}
	return candles, nil
}

func buildSignal(symbol string, candles []Candle, cfg Config) Signal {
	closes := make([]float64, 0, len(candles))
	for _, c := range candles {
		closes = append(closes, c.Close)
	}
	price := closes[len(closes)-1]
	fast, okFast := sma(closes, cfg.Strategy.FastSMA)
	slow, okSlow := sma(closes, cfg.Strategy.SlowSMA)
	currentRSI, okRSI := rsi(closes, cfg.Strategy.RSIPeriod)
	vol, okVol := realizedVolatility(closes, 24)
	if !okFast || !okSlow || !okRSI || !okVol {
		return Signal{Symbol: symbol, Action: "hold", Confidence: 0, Price: price, Reason: "not enough market history"}
	}

	trendScore := clamp((fast/slow-1)*25, -1, 1)
	rsiScore := clamp((55-currentRSI)/25, -1, 1)
	volPenalty := math.Min(vol*18, 0.35)
	confidence := clamp(0.5+trendScore*0.28+rsiScore*0.22-volPenalty, 0, 1)

	action := "hold"
	if fast > slow && currentRSI <= cfg.Strategy.BuyRSIMax && confidence >= cfg.Strategy.MinConfidence {
		action = "buy"
	} else if fast < slow || currentRSI >= cfg.Strategy.SellRSIMin {
		action = "sell"
	}

	return Signal{
		Symbol:     symbol,
		Action:     action,
		Confidence: round4(confidence),
		Price:      price,
		FastSMA:    round4(fast),
		SlowSMA:    round4(slow),
		RSI:        round2(currentRSI),
		Volatility: round6(vol),
		Reason:     fmt.Sprintf("fast_sma=%.2f, slow_sma=%.2f, rsi=%.2f, vol=%.4f", fast, slow, currentRSI, vol),
	}
}

func maybeExitPosition(state *State, cfg Config, signal Signal, journalPath string) (Event, error) {
	pos, ok := state.Positions[signal.Symbol]
	if !ok {
		return nil, nil
	}
	changePct := signal.Price/pos.EntryPrice - 1
	exitReason := ""
	switch {
	case changePct <= -cfg.Risk.StopLossPct:
		exitReason = "stop_loss"
	case changePct >= cfg.Risk.TakeProfitPct:
		exitReason = "take_profit"
	case signal.Action == "sell" && !aiRejected(signal):
		exitReason = "strategy_sell"
	default:
		return nil, nil
	}

	proceeds := pos.Qty * signal.Price
	pnl := proceeds - pos.Cost
	state.Cash += proceeds
	state.RealizedPnL += pnl
	delete(state.Positions, signal.Symbol)

	event := Event{
		"time":   time.Now().UTC().Format(time.RFC3339),
		"type":   "sell",
		"symbol": signal.Symbol,
		"price":  signal.Price,
		"qty":    pos.Qty,
		"pnl":    round6(pnl),
		"reason": exitReason,
		"signal": signal,
	}
	return event, appendJSONL(journalPath, event)
}

func maybeEnterPosition(state *State, cfg Config, signal *Signal, journalPath string) (Event, error) {
	if signal.Action != "buy" {
		return nil, nil
	}
	if _, exists := state.Positions[signal.Symbol]; exists {
		return nil, nil
	}

	ensureSignalAIReview(cfg, signal, *state)
	if signal.AIReview != nil && !signal.AIReview.Approved {
		event := Event{
			"time":   time.Now().UTC().Format(time.RFC3339),
			"type":   "blocked_buy",
			"symbol": signal.Symbol,
			"reason": "ai_review_rejected",
			"signal": signal,
		}
		return nil, appendJSONL(journalPath, event)
	}

	day := todayKey()
	if state.TradeCountByDay[day] >= cfg.Risk.MaxTradesPerDay {
		return nil, nil
	}

	maxNotional := cfg.StartingBudget * cfg.Risk.MaxPositionPct
	riskNotional := cfg.StartingBudget * cfg.Risk.MaxTradeRiskPct / cfg.Risk.StopLossPct
	notional := math.Min(state.Cash, math.Min(maxNotional, riskNotional))
	if notional <= 10 {
		return nil, nil
	}

	qty := notional / signal.Price
	state.Cash -= notional
	state.Positions[signal.Symbol] = Position{
		EntryTime:  time.Now().UTC().Format(time.RFC3339),
		EntryPrice: signal.Price,
		Qty:        qty,
		Cost:       notional,
	}
	state.TradeCountByDay[day]++

	event := Event{
		"time":     time.Now().UTC().Format(time.RFC3339),
		"type":     "buy",
		"symbol":   signal.Symbol,
		"price":    signal.Price,
		"qty":      qty,
		"notional": round6(notional),
		"reason":   "strategy_buy",
		"signal":   signal,
	}
	return event, appendJSONL(journalPath, event)
}

func reviewSignals(cfg Config, signals []Signal, state State) {
	for i := range signals {
		ensureSignalAIReview(cfg, &signals[i], state)
	}
}

func ensureSignalAIReview(cfg Config, signal *Signal, state State) {
	if signal.AIReview != nil {
		return
	}
	review := aiReviewSignal(cfg, *signal, state)
	signal.AIReview = &review
}

func aiRejected(signal Signal) bool {
	return signal.AIReview != nil && !signal.AIReview.Approved
}

func aiReviewSignal(cfg Config, signal Signal, state State) AIReview {
	if !cfg.AI.Enabled {
		return AIReview{Approved: true, Confidence: 1, Reason: "AI reviewer disabled"}
	}
	if cfg.AI.Provider != "local" && cfg.AI.Provider != "openai" {
		return AIReview{Approved: false, Confidence: 0, Reason: "unsupported AI provider"}
	}
	review, err := localAIReview(cfg.AI.Model, map[string]any{
		"signal":          signal,
		"cash":            state.Cash,
		"positions":       state.Positions,
		"risk":            cfg.Risk,
		"starting_budget": cfg.StartingBudget,
	})
	if err != nil {
		if cfg.AI.RequireApprovalForBuys && signal.Action == "buy" {
			return AIReview{Approved: false, Confidence: 0, Reason: "AI review failed: " + err.Error()}
		}
		return AIReview{Approved: true, Confidence: 0, Reason: "AI review unavailable: " + err.Error()}
	}
	if signal.Action == "hold" && !review.Approved {
		review.Approved = true
		review.Reason = "Hold accepted: " + review.Reason
	}
	return review
}

func localAIReview(model string, payload map[string]any) (AIReview, error) {
	payloadBytes, _ := json.Marshal(payload)
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are a conservative crypto trading signal reviewer. Return only JSON with keys approved, confidence, reason. Explain the quantitative signal briefly. Interpret approved as whether the proposed action is reasonable, not whether a trade should be opened. For action=hold, low confidence, neutral RSI, weak trend, or lack of directional edge usually support the hold and should be approved; reject hold only if the metrics contradict holding. For action=buy or action=sell, reject if the signal is incoherent, low quality, or risk is excessive. You must not invent new trades.",
			},
			{
				"role":    "user",
				"content": "Payload: " + string(payloadBytes),
			},
		},
		"temperature": 0,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "trade_review",
				"strict": true,
				"schema": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"approved":   map[string]string{"type": "boolean"},
						"confidence": map[string]string{"type": "number"},
						"reason":     map[string]string{"type": "string"},
					},
					"required": []string{"approved", "confidence", "reason"},
				},
			},
		},
	}
	reqBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, localAIAPI, bytes.NewReader(reqBytes))
	if err != nil {
		return AIReview{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return AIReview{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return AIReview{}, fmt.Errorf("local AI status %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return AIReview{}, err
	}
	for _, choice := range data.Choices {
		content := choice.Message.Content
		if content == "" {
			content = choice.Message.ReasoningContent
		}
		if content == "" {
			continue
		}
		var review AIReview
		if err := json.Unmarshal([]byte(content), &review); err != nil {
			return AIReview{}, err
		}
		return review, nil
	}
	return AIReview{}, errors.New("local AI response did not include message content")
}

func loadState(path string, cfg Config) (State, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return State{
			CreatedAt:       time.Now().UTC().Format(time.RFC3339),
			Cash:            cfg.StartingBudget,
			Positions:       map[string]Position{},
			RealizedPnL:     0,
			TradeCountByDay: map[string]int{},
			DayStartEquity:  map[string]float64{},
			Halted:          false,
			HaltReason:      nil,
			LastEquity:      cfg.StartingBudget,
			HighWatermark:   cfg.StartingBudget,
		}, nil
	}
	var state State
	if err := readJSON(path, &state); err != nil {
		return State{}, err
	}
	if state.Positions == nil {
		state.Positions = map[string]Position{}
	}
	if state.TradeCountByDay == nil {
		state.TradeCountByDay = map[string]int{}
	}
	if state.DayStartEquity == nil {
		state.DayStartEquity = map[string]float64{}
	}
	return state, nil
}

func estimateEquity(state State, latestPrices map[string]float64) float64 {
	equity := state.Cash
	for symbol, pos := range state.Positions {
		price := latestPrices[symbol]
		if price == 0 {
			price = pos.EntryPrice
		}
		equity += pos.Qty * price
	}
	return equity
}

func applyHalts(state *State, cfg Config, equity float64) {
	totalFloor := cfg.StartingBudget * (1 - cfg.Risk.TotalLossStopPct)
	if equity <= totalFloor {
		reason := "total loss stop reached"
		state.Halted = true
		state.HaltReason = &reason
		return
	}

	day := todayKey()
	dayStart, ok := state.DayStartEquity[day]
	if !ok {
		state.DayStartEquity[day] = equity
		return
	}
	dailyFloor := dayStart * (1 - cfg.Risk.DailyLossLimitPct)
	if equity <= dailyFloor {
		reason := "daily loss limit reached"
		state.Halted = true
		state.HaltReason = &reason
	}
}

func writeDailyReport(reportPath, equityPath, journalPath, date string) (DailyReport, error) {
	report, err := buildDailyReport(equityPath, journalPath, date)
	if err != nil {
		return DailyReport{}, err
	}
	if err := writeJSON(reportPath, report); err != nil {
		return DailyReport{}, err
	}
	return report, nil
}

func buildDailyReport(equityPath, journalPath, date string) (DailyReport, error) {
	equityRows, err := readEquityRows(equityPath, 10000)
	if err != nil {
		return DailyReport{}, err
	}
	journal, err := readJournal(journalPath, 10000)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return DailyReport{}, err
	}

	report := DailyReport{
		Date:        date,
		WorstTrades: []WorstTrade{},
	}

	var dayRows []EquityRow
	for _, row := range equityRows {
		if sameUTCDate(row.Time, date) {
			dayRows = append(dayRows, row)
		}
	}
	if len(dayRows) > 0 {
		start := dayRows[0].Equity
		end := dayRows[len(dayRows)-1].Equity
		report.StartEquity = round6(start)
		report.EndEquity = round6(end)
		if start != 0 {
			report.PerformancePct = round3((end/start - 1) * 100)
		}

		peak := dayRows[0].Equity
		maxDrawdown := 0.0
		for _, row := range dayRows {
			if row.Equity > peak {
				peak = row.Equity
			}
			if peak > 0 {
				drawdown := (row.Equity/peak - 1) * 100
				if drawdown < maxDrawdown {
					maxDrawdown = drawdown
				}
			}
		}
		report.MaxDrawdownPct = round3(maxDrawdown)
	}

	for _, event := range journal {
		if event["type"] != "sell" || !sameUTCDate(eventString(event, "time"), date) {
			continue
		}
		trade := WorstTrade{
			Time:   eventString(event, "time"),
			Symbol: eventString(event, "symbol"),
			Price:  eventFloat(event, "price"),
			Qty:    eventFloat(event, "qty"),
			PnL:    round6(eventFloat(event, "pnl")),
			Reason: eventString(event, "reason"),
		}
		report.RealizedPnL = round6(report.RealizedPnL + trade.PnL)
		report.TradeCount++
		report.WorstTrades = append(report.WorstTrades, trade)
	}
	sort.Slice(report.WorstTrades, func(i, j int) bool {
		return report.WorstTrades[i].PnL < report.WorstTrades[j].PnL
	})
	if len(report.WorstTrades) > 5 {
		report.WorstTrades = report.WorstTrades[:5]
	}

	return report, nil
}

func sameUTCDate(value, date string) bool {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value == date
	}
	return t.UTC().Format("2006-01-02") == date
}

func eventString(event Event, key string) string {
	value, ok := event[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func eventFloat(event Event, key string) float64 {
	value, ok := event[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	default:
		return 0
	}
}

func sma(values []float64, period int) (float64, bool) {
	if len(values) < period || period <= 0 {
		return 0, false
	}
	sum := 0.0
	for _, v := range values[len(values)-period:] {
		sum += v
	}
	return sum / float64(period), true
}

func rsi(values []float64, period int) (float64, bool) {
	if len(values) <= period || period <= 0 {
		return 0, false
	}
	gain := 0.0
	loss := 0.0
	for i := len(values) - period; i < len(values); i++ {
		change := values[i] - values[i-1]
		if change >= 0 {
			gain += change
		} else {
			loss += -change
		}
	}
	avgGain := gain / float64(period)
	avgLoss := loss / float64(period)
	if avgLoss == 0 {
		return 100, true
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs)), true
}

func realizedVolatility(values []float64, period int) (float64, bool) {
	if len(values) <= period || period <= 1 {
		return 0, false
	}
	recent := values[len(values)-period:]
	returns := make([]float64, 0, len(recent)-1)
	for i := 1; i < len(recent); i++ {
		if recent[i-1] > 0 {
			returns = append(returns, math.Log(recent[i]/recent[i-1]))
		}
	}
	if len(returns) < 2 {
		return 0, false
	}
	mean := 0.0
	for _, v := range returns {
		mean += v
	}
	mean /= float64(len(returns))
	sumSquares := 0.0
	for _, v := range returns {
		d := v - mean
		sumSquares += d * d
	}
	return math.Sqrt(sumSquares / float64(len(returns)-1)), true
}

func readJSON(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(target)
}

func writeJSON(path string, value any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendJSONL(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	bytes, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = f.Write(append(bytes, '\n'))
	return err
}

func printJSON(value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(bytes))
	return nil
}

func mustFloatString(value any) float64 {
	var s string
	switch v := value.(type) {
	case string:
		s = v
	default:
		panic(fmt.Sprintf("expected string float, got %T", value))
	}
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		panic(err)
	}
	return f
}

func todayKey() string {
	return time.Now().UTC().Format("2006-01-02")
}

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round6(v float64) float64 { return math.Round(v*1000000) / 1000000 }

func exitErr(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
