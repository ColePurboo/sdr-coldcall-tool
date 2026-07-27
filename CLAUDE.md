# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build -o sdr .

# Run (full mode)
./sdr

# Common flags
./sdr --no-research          # Skip all API calls (for UI/flow work)
./sdr --dry-run              # Load CSV, print companies, exit (validates parsing)
./sdr --research-only        # Run research on first company only, print, exit
./sdr --file path/to/list.csv
./sdr --from N --fresh       # Start at position N with a fresh session

# No test suite — use --dry-run and --research-only for validation
```

## Architecture

Go CLI app. `main.go` is the orchestrator; all other logic lives in packages.

```
main.go           CSV/HubSpot selection, session management, main company loop
config/           Load/validate .env (Anthropic, Browserless, Aircall, HubSpot keys)
leads/            CSV parsing, company grouping, contact title-tier prioritization
research/         AI research pipeline: scrape → web search → Claude reasoning → Brief struct
dialer/           Terminal UI (raw key reads), call flow, Aircall API integration
logger/           Atomic JSON call log writes, HubSpot-compatible format
session/          Session state persistence (.sessions/*.session.json), progress tracking
hubspot/          HubSpot contact list fetching, company grouping, call engagement writes
```

**Data flow through main loop:**

1. `leads.Load()` parses CSV → `[]Company` (contacts grouped, primary contact chosen by title tier)
2. `research.Prefetcher` runs a sliding window of goroutines — Browserless serialized, Claude parallelized — and feeds `briefCache` ahead of current position
3. `dialer.RunCard()` displays the company card (with cached research `Brief`) and reads single-keystroke input: `c`all / `s`kip / `n`ext contact / `b`ack / `q`uit
4. On call: `logger.Append()` writes atomically to `logs/call_log_*.json`; `session.Advance()` updates `.sessions/*.session.json`
5. On back: `logger.RemoveLast()` removes last entry, `session.Retreat()` decrements counts, `briefCache` serves instantly from map

**Key invariants:**
- All disk writes are atomic (write `.tmp` → `os.Rename()`), safe to kill mid-run
- Browserless calls are serialized (one at a time); Claude calls parallelize freely
- Session files live in `.sessions/` (gitignored); log files in `logs/` (gitignored)
- CSV filenames get prefixed `[in progress]` / `[completed]` automatically by session logic

## External APIs

| API | Purpose | Config key(s) |
|-----|---------|--------------|
| Anthropic Claude Haiku 4.5 | Web search + research reasoning | `ANTHROPIC_API_KEY` |
| Browserless.io | Website scraping | `BROWSERLESS_TOKEN` |
| Aircall | Optional softphone dialing | `AIRCALL_API_ID`, `AIRCALL_API_TOKEN`, `AIRCALL_NUMBER_ID`, `AIRCALL_USER_ID` |
| HubSpot | Contact list source + call engagement writes | `HUBSPOT_ACCESS_TOKEN` |

All keys except `ANTHROPIC_API_KEY` and `BROWSERLESS_TOKEN` are optional. Copy `.env.example` to `.env` to get started.

- Aircall falls back to a simulated call if not configured; `--no-aircall` forces this even when Aircall is configured.
- HubSpot: when `HUBSPOT_ACCESS_TOKEN` is set, the main menu offers HubSpot lists as a lead source and writes call engagements back after each call.
- Research pipeline is skipped entirely with `--no-research`.

## Research pipeline detail

`research.Run()` in three steps:
1. **Scrape** — Browserless fetches website HTML, strips tags, caps ~12K chars (8s timeout)
2. **Web search** — Claude with `web_search_20250305` tool gathers external signals
3. **Reason** — Claude synthesizes a `Brief` struct: `WhatTheyDo`, `WhoTheyServe`, `PaymentComplexity`, `VennFit`, `Hook`

Rate-limit handling: reads Anthropic 429 `retry-after` headers and backs off with exponential waits.

## Dialer call flow

The company card shows a **CONTACTS roster** (all contacts at the company, each with title and available number-type tags) plus the selected contact's dialable number. Contacts are ordered **finance-first, then executives, then the rest** (`leads.SortContacts`), so the top finance DM is selected by default.

Navigation keys on the card:
- `n` / `p` — scroll to the next / previous contact (resets the number selection)
- `m` — cycle the selected contact's numbers (mobile → office → any custom HubSpot phone fields, each tagged)
- `c` calls the **currently selected number**; `s` skip; `b` undoes the last logged call; `q` quit

After `c`all, the SDR is prompted for:
1. **Disposition** (1–8): `no_answer`, `number_not_in_service`, `contact_retired`, `instant_hangup_dm`, `connected_dm`, `connected_reception`, `need_to_enrich`, `other`
2. **Sentiment** (a–k, only if `connected_dm`): ranges from `call_back_later` through `demo_scheduled`
3. **Free-text notes** (optional, any string)

The exact dialed number and its type are captured on `CallResult` (`DialedPhone`/`DialedPhoneLabel`) and logged (`main.go` passes them to `logger.BuildEntry`) so the CRM record reflects what was actually dialed, not the default. Phone numbers live on `leads.Contact.Phones` (`[]leads.Phone{Number,Label}`); HubSpot phone-number properties (including custom fields) are auto-discovered in `hubspot.LoadContacts`.

Disposition keys map to hardcoded HubSpot GUIDs in `logger/logger.go` — update both the internal key list and the `dispositionGUID()` map if adding new outcomes.
