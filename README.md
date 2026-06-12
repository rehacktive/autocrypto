# Crypto AI Paper Bot

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

## Cosa fa

- Usa un budget iniziale configurabile.
- Opera solo in paper trading.
- Tiene posizioni virtuali su BTC, ETH e SOL.
- Applica stop loss, take profit, limite di perdita giornaliera, limite di perdita totale, massimo trade al giorno.
- Puo usare un revisore AI opzionale prima degli ingressi.
- Salva stato e diario in `state.json` e `journal.jsonl`.
- Salva la curva capitale in `equity.jsonl`.
- Genera un report leggibile dopo ogni ciclo.
- Espone una dashboard locale con capitale, posizioni, segnali e journal.

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
  "provider": "openai",
  "model": "gpt-4.1-mini"
}
```

Poi esporta la chiave:

```bash
export OPENAI_API_KEY="..."
```

Il revisore AI non genera trade da solo: riceve il segnale quantitativo e puo approvarlo o bocciarlo. Se `require_approval_for_buys` e attivo e l'AI non risponde, il bot non compra.

## Perche questa forma

L'obiettivo e capire se una strategia ha un edge prima di rischiare capitale vero. L'AI, quando verra aggiunta, dovrebbe essere un revisore del contesto e del rischio, non un permesso illimitato a comprare.

## Prossimi step sensati

1. Far girare paper trading per 2-4 settimane.
2. Aggiungere un report giornaliero con performance, drawdown e trade peggiori.
3. Aggiungere un modulo AI opzionale che spiega o boccia i segnali.
4. Solo dopo, valutare integrazione exchange con API key senza permesso di withdrawal.
