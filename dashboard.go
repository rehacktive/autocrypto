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
      max-width: 1280px;
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
      grid-template-columns: minmax(0, 1.4fr) minmax(360px, 0.9fr);
      gap: 16px;
      align-items: start;
    }
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
    }
    @media (max-width: 560px) {
      main { padding: 12px; }
      .metrics { grid-template-columns: 1fr; }
      .value { font-size: 22px; }
      th, td { padding: 9px 6px; }
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
      <div class="panel">
        <h2>Capital curve</h2>
        <div class="panel-body"><canvas id="equityChart" height="300"></canvas></div>
      </div>
      <div class="panel">
        <h2>Open positions</h2>
        <div class="panel-body"><table><thead><tr><th>Asset</th><th>Qty</th><th>Entry</th><th>Cost</th></tr></thead><tbody id="positions"></tbody></table></div>
      </div>
      <div class="panel">
        <h2>Signals</h2>
        <div class="panel-body"><table><thead><tr><th>Asset</th><th>Action</th><th>Price</th><th>Confidence</th><th>RSI</th><th>AI</th></tr></thead><tbody id="signals"></tbody></table></div>
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
      if (!report) return;

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
      setText("cycleSub", report.events.length ? (report.events.length + " event(s) in last cycle") : "last cycle completed");
      setText("mode", report.mode);
      setText("halt", report.halted ? ("halted: " + (report.halt_reason || "risk limit")) : "paper trading active");

      renderPositions(report.positions || {});
      renderSignals(report.signals || []);
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

    function renderSignals(signals) {
      document.getElementById("signals").innerHTML = signals.map(s =>
        "<tr><td>" + esc(s.symbol) + "</td><td><span class='pill " + esc(s.action) + "'>" + esc(s.action) + "</span></td><td>" + fmtMoney.format(s.price) + "</td><td>" + (s.confidence * 100).toFixed(1) + "%</td><td>" + (s.rsi || 0).toFixed(2) + "</td><td>" + renderAIReview(s.ai_review) + "</td></tr>"
      ).join("") || "<tr><td colspan='6'>No signals yet</td></tr>";
    }

    function renderAIReview(review) {
      if (!review) return "-";
      const label = review.approved ? "ok" : "blocked";
      return "<span class='pill " + (review.approved ? "" : "sell") + "'>" + label + "</span><div class='sub'>" + esc(review.reason || "") + "</div>";
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

    loadStatus();
    setInterval(loadStatus, 5000);
    window.addEventListener("resize", loadStatus);
  </script>
</body>
</html>`
