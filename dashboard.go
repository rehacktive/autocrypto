package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type DashboardApp struct {
	configPath string
	baseDir    string
	interval   time.Duration
	mu         sync.RWMutex
	lastReport *Report
	lastError  string
	running    bool
}

type StatusResponse struct {
	Report       *Report      `json:"report"`
	DailyReport  *DailyReport `json:"daily_report"`
	History      []EquityRow  `json:"history"`
	Journal      []Event      `json:"journal"`
	LastError    string       `json:"last_error"`
	Running      bool         `json:"running"`
	NextInterval string       `json:"next_interval"`
	CycleCount   int          `json:"cycle_count"`
}

type EquityRow struct {
	Time        string  `json:"time"`
	Cash        float64 `json:"cash"`
	Equity      float64 `json:"equity"`
	RealizedPnL float64 `json:"realized_pnl"`
	DrawdownPct float64 `json:"drawdown_pct"`
}

func serveDashboard(configPath, addr string, interval time.Duration) error {
	baseDir := filepath.Dir(absPath(configPath))
	app := &DashboardApp{
		configPath: configPath,
		baseDir:    baseDir,
		interval:   interval,
	}

	go app.loop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/run", app.handleRun)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("dashboard listening on http://%s\n", addr)
	return server.ListenAndServe()
}

func (app *DashboardApp) loop() {
	app.runNow()
	ticker := time.NewTicker(app.interval)
	defer ticker.Stop()
	for range ticker.C {
		app.runNow()
	}
}

func (app *DashboardApp) runNow() {
	app.mu.Lock()
	if app.running {
		app.mu.Unlock()
		return
	}
	app.running = true
	app.mu.Unlock()

	report, err := runCycle(app.configPath)

	app.mu.Lock()
	defer app.mu.Unlock()
	app.running = false
	if err != nil {
		app.lastError = err.Error()
		return
	}
	app.lastError = ""
	app.lastReport = &report
}

func (app *DashboardApp) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func (app *DashboardApp) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	app.mu.RLock()
	report := app.lastReport
	lastError := app.lastError
	running := app.running
	app.mu.RUnlock()

	history, historyErr := readEquityRows(filepath.Join(app.baseDir, "equity.jsonl"), 300)
	journal, journalErr := readJournal(filepath.Join(app.baseDir, "journal.jsonl"), 80)
	dailyReport, dailyReportErr := readDailyReport(filepath.Join(app.baseDir, "daily_report.json"))
	if history == nil {
		history = []EquityRow{}
	}
	if journal == nil {
		journal = []Event{}
	}
	if report == nil {
		snapshot, snapshotErr := app.readSnapshotReport(history)
		if snapshotErr == nil {
			report = snapshot
		} else if lastError == "" {
			lastError = snapshotErr.Error()
		}
	}
	if lastError == "" {
		if historyErr != nil && !errors.Is(historyErr, os.ErrNotExist) {
			lastError = historyErr.Error()
		} else if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
			lastError = journalErr.Error()
		} else if dailyReportErr != nil && !errors.Is(dailyReportErr, os.ErrNotExist) {
			lastError = dailyReportErr.Error()
		}
	}

	writeAPIJSON(w, StatusResponse{
		Report:       report,
		DailyReport:  dailyReport,
		History:      history,
		Journal:      journal,
		LastError:    lastError,
		Running:      running,
		NextInterval: app.interval.String(),
		CycleCount:   len(history),
	})
}

func (app *DashboardApp) readSnapshotReport(history []EquityRow) (*Report, error) {
	var cfg Config
	if err := readJSON(app.configPath, &cfg); err != nil {
		return nil, err
	}

	state := State{
		Cash:      cfg.StartingBudget,
		Positions: map[string]Position{},
	}
	statePath := filepath.Join(app.baseDir, "state.json")
	if err := readJSON(statePath, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if state.Positions == nil {
		state.Positions = map[string]Position{}
	}

	reportTime := time.Now().UTC().Format(time.RFC3339)
	cash := state.Cash
	equity := state.LastEquity
	realizedPnL := state.RealizedPnL
	drawdownPct := 0.0
	if state.HighWatermark > 0 && equity > 0 {
		drawdownPct = round3((equity/state.HighWatermark - 1) * 100)
	}

	if len(history) > 0 {
		latest := history[len(history)-1]
		reportTime = latest.Time
		cash = latest.Cash
		equity = latest.Equity
		realizedPnL = latest.RealizedPnL
		drawdownPct = latest.DrawdownPct
	} else if equity == 0 {
		equity = cfg.StartingBudget
	}

	return &Report{
		Time:        reportTime,
		Mode:        cfg.Mode,
		Cash:        round6(cash),
		Equity:      round6(equity),
		RealizedPnL: round6(realizedPnL),
		DrawdownPct: drawdownPct,
		Halted:      state.Halted,
		HaltReason:  state.HaltReason,
		Positions:   state.Positions,
		Signals:     []Signal{},
		Events:      []Event{},
	}, nil
}

func (app *DashboardApp) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	app.runNow()
	writeAPIJSON(w, map[string]any{"ok": true})
}

func readEquityRows(path string, limit int) ([]EquityRow, error) {
	var rows []EquityRow
	err := readJSONLLimited(path, limit, func(raw []byte) error {
		var row EquityRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		rows = append(rows, row)
		return nil
	})
	return rows, err
}

func readJournal(path string, limit int) ([]Event, error) {
	var events []Event
	err := readJSONLLimited(path, limit, func(raw []byte) error {
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		events = append(events, event)
		return nil
	})
	return events, err
}

func readDailyReport(path string) (*DailyReport, error) {
	var report DailyReport
	if err := readJSON(path, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

func readJSONLLimited(path string, limit int, decode func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var lines [][]byte
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		lines = append(lines, line)
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, line := range lines {
		if err := decode(line); err != nil {
			return err
		}
	}
	return nil
}

func writeAPIJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

const dashboardHTML = `<!doctype html>
<html lang="it">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Autocrypto Dashboard</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #0b0f14;
      --panel: #121821;
      --panel-soft: #171f2a;
      --ink: #edf2f7;
      --muted: #95a3b8;
      --line: #273241;
      --good: #49d18d;
      --bad: #ff6b6b;
      --warn: #f4b740;
      --accent: #68a7ff;
      --accent-strong: #2f7ff0;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--ink);
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      letter-spacing: 0;
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      padding: 18px 22px;
      background: var(--panel);
      border-bottom: 1px solid var(--line);
      position: sticky;
      top: 0;
      z-index: 5;
    }
    h1 { font-size: 20px; margin: 0; font-weight: 700; }
    button {
      border: 1px solid var(--accent-strong);
      background: var(--accent-strong);
      color: white;
      border-radius: 6px;
      padding: 9px 12px;
      font-weight: 700;
      cursor: pointer;
    }
    button:disabled {
      opacity: 0.65;
      cursor: wait;
    }
    button:hover:not(:disabled) {
      background: #3d8cff;
      border-color: #63a5ff;
    }
    main {
      max-width: 1480px;
      margin: 0 auto;
      padding: 20px;
    }
    .statusbar {
      display: flex;
      gap: 10px;
      align-items: center;
      color: var(--muted);
      font-size: 13px;
      flex-wrap: wrap;
    }
    .dot {
      width: 10px;
      height: 10px;
      border-radius: 999px;
      background: var(--good);
      display: inline-block;
    }
    .dot.busy { background: var(--warn); }
    .dot.error { background: var(--bad); }
    .metrics {
      display: grid;
      grid-template-columns: repeat(6, minmax(140px, 1fr));
      gap: 12px;
      margin-bottom: 18px;
    }
    .metric, .panel {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      box-shadow: 0 12px 32px rgba(0, 0, 0, 0.22);
    }
    .metric {
      padding: 14px;
      min-height: 92px;
    }
    .label {
      color: var(--muted);
      font-size: 12px;
      text-transform: uppercase;
      font-weight: 800;
    }
    .value {
      margin-top: 8px;
      font-size: 25px;
      font-weight: 800;
      white-space: nowrap;
    }
    .sub {
      color: var(--muted);
      font-size: 12px;
      margin-top: 4px;
    }
    .positive { color: var(--good); }
    .negative { color: var(--bad); }
    .grid {
      display: grid;
      grid-template-columns: repeat(12, minmax(0, 1fr));
      gap: 16px;
      align-items: start;
    }
    .panel {
      grid-column: span 6;
      overflow: hidden;
    }
    .panel.wide { grid-column: span 8; }
    .panel.narrow { grid-column: span 4; }
    .panel.full { grid-column: 1 / -1; }
    .main-stack {
      grid-column: span 8;
      display: grid;
      gap: 16px;
      align-content: start;
    }
    .main-stack .panel { grid-column: 1; }
    .side-stack {
      grid-column: span 4;
      display: grid;
      gap: 16px;
    }
    .side-stack .panel { grid-column: 1; }
    .panel h2 {
      margin: 0;
      padding: 14px 16px;
      font-size: 15px;
      border-bottom: 1px solid var(--line);
    }
    .panel-body { padding: 14px 16px; }
    canvas {
      width: 100%;
      height: 300px;
      display: block;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 13px;
      table-layout: fixed;
    }
    th, td {
      padding: 10px 8px;
      border-bottom: 1px solid var(--line);
      text-align: left;
      vertical-align: top;
    }
    th {
      color: var(--muted);
      font-size: 11px;
      text-transform: uppercase;
    }
    tbody tr:hover { background: rgba(104, 167, 255, 0.04); }
    .pill {
      display: inline-flex;
      align-items: center;
      min-width: 54px;
      justify-content: center;
      border-radius: 999px;
      padding: 3px 8px;
      font-size: 12px;
      font-weight: 800;
      background: var(--panel-soft);
      color: var(--ink);
    }
    .pill.buy { background: rgba(73, 209, 141, 0.14); color: var(--good); }
    .pill.sell { background: rgba(255, 107, 107, 0.14); color: var(--bad); }
    .signal-grid {
      display: grid;
      grid-template-columns: repeat(3, minmax(260px, 1fr));
      gap: 12px;
    }
    .signal-card {
      background: linear-gradient(180deg, rgba(23, 31, 42, 0.92), rgba(18, 24, 33, 0.92));
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 14px;
      min-height: 100%;
    }
    .signal-note {
      grid-column: 1 / -1;
      background: rgba(244, 183, 64, 0.09);
      border: 1px solid rgba(244, 183, 64, 0.28);
      border-radius: 10px;
      color: var(--warn);
      padding: 12px 14px;
      font-size: 13px;
      font-weight: 700;
    }
    .signal-head {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 10px;
      margin-bottom: 12px;
    }
    .asset {
      font-size: 15px;
      font-weight: 850;
      letter-spacing: 0.02em;
    }
    .stat-grid {
      display: grid;
      grid-template-columns: repeat(3, 1fr);
      gap: 8px;
      margin-bottom: 12px;
    }
    .stat {
      background: rgba(11, 15, 20, 0.38);
      border: 1px solid rgba(39, 50, 65, 0.72);
      border-radius: 8px;
      padding: 8px;
    }
    .stat .label { font-size: 10px; }
    .stat strong {
      display: block;
      margin-top: 4px;
      font-size: 13px;
      white-space: nowrap;
    }
    .strategy-box, .ai-box {
      background: rgba(11, 15, 20, 0.28);
      border: 1px solid rgba(39, 50, 65, 0.7);
      border-radius: 8px;
      padding: 10px;
      margin-top: 8px;
    }
    .box-title {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 8px;
      margin-bottom: 8px;
      font-size: 12px;
      color: var(--muted);
      text-transform: uppercase;
      font-weight: 800;
    }
    .module-list {
      display: grid;
      gap: 8px;
    }
    .module-row {
      display: grid;
      grid-template-columns: auto 1fr auto;
      gap: 8px;
      align-items: center;
      padding-top: 8px;
      border-top: 1px solid rgba(39, 50, 65, 0.55);
    }
    .module-row:first-child {
      padding-top: 0;
      border-top: 0;
    }
    .module-name {
      font-weight: 800;
      min-width: 0;
    }
    .module-reason {
      grid-column: 2 / 4;
      color: var(--muted);
      font-size: 12px;
      line-height: 1.35;
    }
    .reason-text {
      color: var(--muted);
      font-size: 12px;
      line-height: 1.35;
      margin-top: 6px;
    }
    .reason-text.clamped {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      cursor: help;
    }
    .reason-text.clamped:hover {
      -webkit-line-clamp: unset;
      overflow: visible;
    }
    .errorbox {
      display: none;
      margin-bottom: 16px;
      background: rgba(255, 107, 107, 0.12);
      border: 1px solid rgba(255, 107, 107, 0.35);
      color: var(--bad);
      border-radius: 8px;
      padding: 12px 14px;
      font-size: 13px;
    }
    @media (max-width: 960px) {
      header { align-items: flex-start; flex-direction: column; }
      .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .grid { grid-template-columns: 1fr; }
      .panel, .panel.wide, .panel.narrow, .panel.full { grid-column: 1; }
      .main-stack { grid-column: 1; }
      .side-stack { grid-column: 1; }
      .signal-grid { grid-template-columns: 1fr; }
    }
    @media (max-width: 560px) {
      main { padding: 12px; }
      .metrics { grid-template-columns: 1fr; }
      .value { font-size: 22px; }
      th, td { padding: 9px 6px; }
      .stat-grid { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <header>
    <div>
      <h1>Autocrypto</h1>
      <div class="statusbar">
        <span id="statusDot" class="dot"></span>
        <span id="statusText">loading</span>
        <span id="updatedAt"></span>
      </div>
    </div>
    <button id="runBtn" title="Run a paper trading cycle now">Run now</button>
  </header>
  <main>
    <div id="errorBox" class="errorbox"></div>
    <section class="metrics">
      <div class="metric"><div class="label">Equity</div><div id="equity" class="value">-</div><div id="equitySub" class="sub"></div></div>
      <div class="metric"><div class="label">Cash</div><div id="cash" class="value">-</div><div class="sub">available paper balance</div></div>
      <div class="metric"><div class="label">PnL</div><div id="pnl" class="value">-</div><div id="pnlSub" class="sub"></div></div>
      <div class="metric"><div class="label">Drawdown</div><div id="drawdown" class="value">-</div><div class="sub">from high watermark</div></div>
      <div class="metric"><div class="label">Cycles</div><div id="cycles" class="value">-</div><div id="cycleSub" class="sub"></div></div>
      <div class="metric"><div class="label">Mode</div><div id="mode" class="value">-</div><div id="halt" class="sub"></div></div>
    </section>
    <section class="grid">
      <div class="main-stack">
        <div class="panel">
          <h2>Capital curve</h2>
          <div class="panel-body"><canvas id="equityChart" height="300"></canvas></div>
        </div>
        <div class="panel">
          <h2>Signals</h2>
          <div class="panel-body"><div id="signals" class="signal-grid"></div></div>
        </div>
      </div>
      <aside class="side-stack">
        <div class="panel">
          <h2>Open positions</h2>
          <div class="panel-body"><table><thead><tr><th>Asset</th><th>Qty</th><th>Entry</th><th>Cost</th></tr></thead><tbody id="positions"></tbody></table></div>
        </div>
        <div class="panel">
          <h2>Journal</h2>
          <div class="panel-body"><table><thead><tr><th>Time</th><th>Type</th><th>Asset</th><th>Reason</th></tr></thead><tbody id="journal"></tbody></table></div>
        </div>
        <div class="panel">
          <h2>Daily report</h2>
          <div class="panel-body">
            <table><thead><tr><th>Date</th><th>Performance</th><th>Drawdown</th><th>PnL</th><th>Trades</th></tr></thead><tbody id="dailyReport"></tbody></table>
            <table><thead><tr><th>Time</th><th>Asset</th><th>PnL</th><th>Reason</th></tr></thead><tbody id="worstTrades"></tbody></table>
          </div>
        </div>
      </aside>
    </section>
  </main>
  <script>
    const fmtMoney = new Intl.NumberFormat("it-IT", { style: "currency", currency: "USD", maximumFractionDigits: 2 });
    const fmtNum = new Intl.NumberFormat("it-IT", { maximumFractionDigits: 6 });
    const statusDot = document.getElementById("statusDot");
    const statusText = document.getElementById("statusText");
    const updatedAt = document.getElementById("updatedAt");
    const errorBox = document.getElementById("errorBox");
    const runBtn = document.getElementById("runBtn");

    runBtn.addEventListener("click", async () => {
      runBtn.disabled = true;
      runBtn.textContent = "Running...";
      statusDot.className = "dot busy";
      statusText.textContent = "running manual cycle";
      try {
        const res = await fetch("/api/run", { method: "POST" });
        if (!res.ok) throw new Error("Run failed: HTTP " + res.status);
      } catch (err) {
        renderError(String(err));
      } finally {
        await loadStatus();
        runBtn.disabled = false;
        runBtn.textContent = "Run now";
      }
    });

    async function loadStatus() {
      try {
        const res = await fetch("/api/status", { cache: "no-store" });
        const data = await res.json();
        render(data);
      } catch (err) {
        renderError(String(err));
      }
    }

    function render(data) {
      const report = data.report;
      statusDot.className = "dot" + (data.running ? " busy" : "") + (data.last_error ? " error" : "");
      statusText.textContent = data.running ? "running simulation" : "idle";
      updatedAt.textContent = report ? "last update " + new Date(report.time).toLocaleString() : "";
      runBtn.disabled = data.running;
      runBtn.textContent = data.running ? "Running..." : "Run now";
      if (data.last_error) renderError(data.last_error); else errorBox.style.display = "none";
      if (!report) {
        renderPositions({});
        renderSignals([], []);
        renderJournal(data.journal || []);
        renderDailyReport(data.daily_report);
        drawChart(document.getElementById("equityChart"), data.history || []);
        return;
      }

      const events = report.events || [];
      const start = firstEquity(data.history) || report.equity;
      const pnlPct = start ? ((report.equity / start - 1) * 100) : 0;
      setText("equity", fmtMoney.format(report.equity));
      setText("equitySub", "start " + fmtMoney.format(start));
      setText("cash", fmtMoney.format(report.cash));
      setText("pnl", signed(report.equity - start));
      setText("pnlSub", signedPct(pnlPct));
      document.getElementById("pnl").className = "value " + (report.equity >= start ? "positive" : "negative");
      setText("drawdown", report.drawdown_pct.toFixed(3) + "%");
      setText("cycles", String(data.cycle_count || (data.history || []).length));
      setText("cycleSub", data.running && !events.length ? "showing saved state; cycle in progress" : (events.length ? (events.length + " event(s) in last cycle") : "no trades in last cycle"));
      setText("mode", report.mode);
      setText("halt", report.halted ? ("halted: " + (report.halt_reason || "risk limit")) : "paper trading active");

      renderPositions(report.positions || {});
      renderSignals(report.signals || [], events);
      renderJournal(data.journal || []);
      renderDailyReport(data.daily_report);
      drawChart(document.getElementById("equityChart"), data.history || []);
    }

    function firstEquity(history) {
      return history && history.length ? history[0].equity : 0;
    }

    function renderPositions(positions) {
      const rows = Object.entries(positions).map(([symbol, pos]) =>
        "<tr><td>" + esc(symbol) + "</td><td>" + fmtNum.format(pos.qty) + "</td><td>" + fmtMoney.format(pos.entry_price) + "</td><td>" + fmtMoney.format(pos.cost) + "</td></tr>"
      );
      document.getElementById("positions").innerHTML = rows.join("") || "<tr><td colspan='4'>No open positions</td></tr>";
    }

    function renderSignals(signals, events) {
      const hasEvents = events && events.length > 0;
      const allHold = signals.length > 0 && signals.every(s => s.action === "hold");
      const note = !hasEvents ? "<div class='signal-note'>No trade executed in the last cycle. Current signals are below; hold cards explain why the bot is waiting.</div>" : "";
      document.getElementById("signals").innerHTML = note + (signals.map(s =>
        "<article class='signal-card'><div class='signal-head'><div><div class='asset'>" + esc(s.symbol) + "</div>" + renderExecutionReason(s.execution_reason) + "</div><span class='pill " + esc(s.action) + "'>" + esc(s.action) + "</span></div><div class='stat-grid'><div class='stat'><div class='label'>Price</div><strong>" + fmtMoney.format(s.price) + "</strong></div><div class='stat'><div class='label'>Confidence</div><strong>" + (s.confidence * 100).toFixed(1) + "%</strong></div><div class='stat'><div class='label'>RSI</div><strong>" + (s.rsi || 0).toFixed(2) + "</strong></div></div>" + renderStrategy(s) + renderAIReview(s.ai_review) + "</article>"
      ).join("") || "<div class='sub'>" + (allHold ? "All signals are hold." : "No signals yet") + "</div>");
    }

    function renderExecutionReason(reason) {
      return reason ? "<div class='sub'>" + esc(reason) + "</div>" : "";
    }

    function renderAIReview(review) {
      if (!review) return "<section class='ai-box'><div class='box-title'><span>AI</span><span>-</span></div></section>";
      const label = review.approved ? "ok" : "blocked";
      const reason = review.reason || "";
      return "<section class='ai-box'><div class='box-title'><span>AI review</span><span class='pill " + (review.approved ? "" : "sell") + "'>" + label + "</span></div><div class='reason-text clamped' title='" + escAttr(reason) + "'>" + esc(reason) + "</div></section>";
    }

    function renderStrategy(signal) {
      const mode = signal.strategy_mode || "classic";
      const modules = signal.strategy_modules || [];
      let html = "<section class='strategy-box'><div class='box-title'><span>Strategy</span><span class='pill'>" + esc(mode) + "</span></div>";
      if (!modules.length) {
        return html + "<div class='reason-text'>" + esc(signal.reason || "") + "</div></section>";
      }
      html += "<div class='module-list'>";
      for (const module of modules) {
        const action = module.action || "hold";
        const delta = module.confidence_delta ? signedPct(module.confidence_delta * 100) : "";
        const flags = (module.veto_buy ? " veto" : "") + (module.force_sell ? " force sell" : "");
        html += "<div class='module-row'><span class='pill " + esc(action) + "'>" + esc(action) + "</span><span class='module-name'>" + esc(module.name || "module") + "</span><span class='sub'>" + esc([delta, flags.trim()].filter(Boolean).join(" · ")) + "</span><div class='module-reason'>" + esc(module.reason || "") + "</div></div>";
      }
      html += "</div></section>";
      return html;
    }

    function renderJournal(journal) {
      const latest = journal.slice(-12).reverse();
      document.getElementById("journal").innerHTML = latest.map(e =>
        "<tr><td>" + shortTime(e.time) + "</td><td>" + esc(e.type || "") + "</td><td>" + esc(e.symbol || "") + "</td><td>" + esc(e.reason || "") + "</td></tr>"
      ).join("") || "<tr><td colspan='4'>No trades yet</td></tr>";
    }

    function renderDailyReport(report) {
      if (!report) {
        document.getElementById("dailyReport").innerHTML = "<tr><td colspan='5'>No daily report yet</td></tr>";
        document.getElementById("worstTrades").innerHTML = "<tr><td colspan='4'>No closed trades yet</td></tr>";
        return;
      }
      document.getElementById("dailyReport").innerHTML =
        "<tr><td>" + esc(report.date) + "</td><td>" + signedPct(report.performance_pct || 0) + "</td><td>" + (report.max_drawdown_pct || 0).toFixed(3) + "%</td><td>" + signed(report.realized_pnl || 0) + "</td><td>" + (report.trade_count || 0) + "</td></tr>";
      const worst = report.worst_trades || [];
      document.getElementById("worstTrades").innerHTML = worst.map(t =>
        "<tr><td>" + shortTime(t.time) + "</td><td>" + esc(t.symbol || "") + "</td><td>" + signed(t.pnl || 0) + "</td><td>" + esc(t.reason || "") + "</td></tr>"
      ).join("") || "<tr><td colspan='4'>No closed trades yet</td></tr>";
    }

    function drawChart(canvas, history) {
      const ctx = canvas.getContext("2d");
      const rect = canvas.getBoundingClientRect();
      const ratio = window.devicePixelRatio || 1;
      canvas.width = Math.max(1, Math.floor(rect.width * ratio));
      canvas.height = Math.max(1, Math.floor(rect.height * ratio));
      ctx.scale(ratio, ratio);
      const w = rect.width, h = rect.height, pad = 34;
      ctx.clearRect(0, 0, w, h);
      ctx.strokeStyle = "#273241";
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(pad, 12);
      ctx.lineTo(pad, h - pad);
      ctx.lineTo(w - 10, h - pad);
      ctx.stroke();
      if (!history.length) {
        ctx.fillStyle = "#95a3b8";
        ctx.fillText("Waiting for simulation data", pad + 8, 42);
        return;
      }
      const values = history.map(p => p.equity);
      let min = Math.min(...values), max = Math.max(...values);
      if (min === max) { min -= 1; max += 1; }
      const xFor = i => pad + (i / Math.max(1, history.length - 1)) * (w - pad - 16);
      const yFor = v => 12 + (1 - (v - min) / (max - min)) * (h - pad - 18);
      ctx.strokeStyle = "#68a7ff";
      ctx.lineWidth = 2;
      ctx.beginPath();
      history.forEach((p, i) => {
        const x = xFor(i), y = yFor(p.equity);
        if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      });
      ctx.stroke();
      ctx.fillStyle = "#95a3b8";
      ctx.fillText(fmtMoney.format(max), 6, 22);
      ctx.fillText(fmtMoney.format(min), 6, h - pad);
    }

    function renderError(message) {
      errorBox.textContent = message;
      errorBox.style.display = "block";
      statusDot.className = "dot error";
      statusText.textContent = "error";
    }
    function setText(id, value) { document.getElementById(id).textContent = value; }
    function signed(v) { return (v >= 0 ? "+" : "") + fmtMoney.format(v); }
    function signedPct(v) { return (v >= 0 ? "+" : "") + v.toFixed(2) + "%"; }
    function shortTime(value) { return value ? new Date(value).toLocaleString() : ""; }
    function esc(value) {
      return String(value).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#039;" }[c]));
    }
    function escAttr(value) {
      return esc(value).replace(/\n/g, " ");
    }

    loadStatus();
    setInterval(loadStatus, 5000);
    window.addEventListener("resize", loadStatus);
  </script>
</body>
</html>`
