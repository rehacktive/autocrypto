# AutoCrypto / AI Paper Bot

Un bot prudente per sperimentare trading crypto automatico in modalita paper trading.

Questa versione e scritta in Go e non invia ordini reali. Scarica candele pubbliche da Binance, genera segnali con una strategia semplice, applica limiti di rischio rigidi e salva un diario locale.

## Avvio rapido

```bash
cp config.example.json config.json
go run . --config config.json --serve
```

Poi apri:

```text
http://127.0.0.1:8787
```

Per eseguire un singolo ciclo da terminale:

```bash
go run . --config config.json --once
```

Per simulare piu cicli senza dashboard:

```bash
go run . --config config.json --loop --sleep 5m
```

Per simulare subito un periodo storico:

```bash
go run . --config config.json --backtest --from 2026-01-01 --to 2026-03-31
```

Se nel config l'AI e attiva ma vuoi un backtest veloce:

```bash
go run . --config config.json --backtest --backtest-no-ai --from 2026-01-01 --to 2026-03-31
```

Per un report leggibile invece del JSON:

```bash
go run . --config config.json --backtest --backtest-no-ai --backtest-format report --from 2026-01-01 --to 2026-03-31
```

Per cercare parametri migliori sullo stesso periodo:

```bash
go run . --config config.json --backtest --backtest-no-ai --backtest-format report --optimize 300 --from 2026-01-01 --to 2026-03-31
```

## Cosa fa

- Usa un budget iniziale configurabile.
- Opera solo in paper trading.
- Tiene posizioni virtuali su BTC, ETH e SOL.
- Applica stop loss, take profit, limite di perdita giornaliera, limite di perdita totale, massimo trade al giorno.
- Puo usare un revisore AI opzionale che spiega o boccia i segnali.
- Puo fare backtest su candele storiche Binance per valutare periodi estesi.
- Confronta il backtest con benchmark buy-and-hold equal-weight.
- Include fee e slippage configurabili.
- Salva stato e diario in `state.json` e `journal.jsonl`.
- Salva la curva capitale in `equity.jsonl`.
- Salva il report giornaliero in `daily_report.json`.
- Genera un report leggibile dopo ogni ciclo.
- Espone una dashboard locale con capitale, posizioni, segnali, journal e report giornaliero.

## Cosa non fa ancora

- Non usa leva.
- Non fa futures.
- Non fa prelievi.
- Non opera con soldi reali.
- Non promette rendimento.

## Build

```bash
go build -o autocrypto .
./autocrypto --config config.json --serve
```

Puoi cambiare indirizzo e frequenza della simulazione:

```bash
./autocrypto --config config.json --serve --addr 127.0.0.1:8787 --sleep 5m
```

## Revisore AI opzionale

Nel file `config.json` puoi abilitare:

```json
"ai": {
  "enabled": true,
  "require_approval_for_buys": true,
  "provider": "local",
  "model": "nvidia/nemotron-3-nano-4b"
}
```

Il bot chiama un server locale compatibile OpenAI su `http://127.0.0.1:1234/v1/chat/completions`.

Il revisore AI non genera trade da solo: riceve ogni segnale quantitativo e puo spiegarlo, approvarlo o bocciarlo. Se boccia un buy, il bot non compra; se boccia un sell strategico, il bot evita quell'uscita. Stop loss e take profit restano sempre prioritari. Se `require_approval_for_buys` e attivo e l'AI non risponde su un buy, il bot non compra.

## Backtest storico

La modalita backtest scarica candele storiche da Binance per i simboli configurati e simula il periodo in memoria, senza modificare `state.json` o `journal.jsonl`.

```bash
go run . --config config.json --backtest --from 2026-01-01 --to 2026-03-31
```

Il risultato JSON include equity finale, performance, max drawdown, benchmark, alpha, breakdown per simbolo/motivo uscita, eventi, posizioni aperte e report giornalieri. Se vuoi una sintesi leggibile con win rate, profit factor, giorni migliori/peggiori e peggiori trade, usa `--backtest-format report`. Se `ai.enabled` e attivo, anche il backtest chiamera il revisore AI per ogni segnale, quindi per test lunghi conviene usare `--backtest-no-ai`.

### Ottimizzazione

Puoi eseguire N simulazioni sullo stesso periodo variando parametri di strategia e rischio:

```bash
go run . --config config.json --backtest --backtest-no-ai --backtest-format report --optimize 300 --from 2026-01-01 --to 2026-03-31
```

Lo score premia performance, alpha rispetto al benchmark, profit factor e win rate, penalizzando drawdown, pochi trade e risultati con qualita statistica debole. Le configurazioni con pochi trade, profit factor sotto 1 o win rate molto basso vengono marcate come `rejected`. Non e una garanzia: serve per trovare candidati da verificare su periodi diversi, non per scegliere automaticamente una strategia definitiva.

Per generare un file di configurazione pronto da provare, aggiungi `--optimized-config`. Il file mantiene la configurazione originale e aggiorna solo le sezioni `risk` e `strategy` con la migliore configurazione qualificata; se nessuna configurazione e qualificata, usa comunque la migliore disponibile.

```bash
go run . --config config.json --backtest --backtest-no-ai --backtest-format report --optimize 300 --optimized-config config.optimized.json --from 2026-01-01 --to 2026-03-31
```

Per ridurre il rischio di overfitting puoi fare una validazione walk-forward: il bot ottimizza sul periodo `--from/--to`, poi testa ogni candidato su `--validate-from/--validate-to` e preferisce configurazioni che restano qualificate anche fuori campione.

```bash
go run . --config config.json --backtest --backtest-no-ai --backtest-format report --optimize 300 --optimized-config config.optimized.json --from 2026-01-01 --to 2026-03-31 --validate-from 2026-04-01 --validate-to 2026-05-31
```

## Perche questa forma

L'obiettivo e capire se una strategia ha un edge prima di rischiare capitale vero. L'AI, quando verra aggiunta, dovrebbe essere un revisore del contesto e del rischio, non un permesso illimitato a comprare.

## Prossimi step sensati

1. Confrontare backtest su periodi diversi.
2. Far girare paper trading per 2-4 settimane.
3. Solo dopo, valutare integrazione exchange con API key senza permesso di withdrawal.
