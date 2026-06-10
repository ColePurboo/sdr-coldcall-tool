# SDR Dialer

A local Go CLI tool for SDR cold calling. Loads a CSV of prospects, groups them by company, runs AI-powered pre-call research (website scrape + Claude web search), drives a minimal-keystroke call flow, and outputs a HubSpot-compatible JSON log.

---

## What it does

- Groups CSV rows by company, picks the best contact by title tier (CEO > COO > Manager > Accountant)
- Prefetches research for the next 3 companies in the background — brief is ready before you get there
- Single-keystroke call outcomes and tags
- **Back button on every screen** — go back to the previous company and redo the call; log entry is edited automatically
- Saves progress automatically — quit any time, resume exactly where you left off
- Outputs structured JSON logs ready for HubSpot import

---

## Requirements

- **Go 1.21+** — [download here](https://go.dev/dl/)
- **Anthropic API key** — for AI research briefs (uses Claude Haiku 4.5)
- **Browserless token** — for website scraping ([browserless.io](https://www.browserless.io))

---

## Setup

**1. Clone the repo**
```bash
git clone https://github.com/ColePurboo/sdr-coldcall-tool.git
cd sdr-coldcall-tool
```

**2. Add your API keys**
```bash
cp .env.example .env
```
Open `.env` and fill in:
```
ANTHROPIC_API_KEY=sk-ant-...
BROWSERLESS_TOKEN=...
```

**3. Build the binary**
```bash
go build -o sdr .
```

**4. Add your lead list CSV**

Drop a CSV file into this folder. Required columns (order doesn't matter):

| Column | Notes |
|--------|-------|
| Company Name | Used for grouping |
| First Name, Last Name | |
| Job Title | Drives contact prioritization |
| Email | |
| Company Phone | |
| Mobile Phone | Preferred for calling |
| Company City, Company State | Province |
| # Employees | |
| Industry | |
| Website | Used for research scrape |

Multi-phone cells (comma-separated values) are handled — the first number is used.

**5. Run it**
```bash
./sdr
```

---

## The Three Modes

### Full mode (default)
```bash
./sdr
```
Runs everything: research pipeline (Browserless scrape + Claude web search + AI brief), call flow, logging, and session management. Requires both API keys in `.env`.

### No-research mode
```bash
./sdr --no-research
```
Full call flow and logging, but skips all API calls. No Anthropic or Browserless keys needed. Use this to test the UI, practice the flow, or call when you don't need research.

### Dry-run mode
```bash
./sdr --dry-run
```
Loads and prints the full company list — no calls, no API requests, no log files written. Use this to verify your CSV parsed correctly before starting a session.

---

## All Flags

```
--file string       Path to a specific CSV (skips the selection menu)
--dry-run           Print all companies, no calls, no API requests
--no-research       Full call flow but skip the research pipeline
--research-only     Run research on the first company, print the brief, then exit
--from int          Start from company number N (1-indexed, overrides saved session)
--fresh             Ignore any saved session and start from the beginning
```

---

## Daily Workflow

1. Run `./sdr` and select your list from the menu
2. A loading screen prefetches research for the first 3 companies — you start with the brief already there
3. For each company:
   - Read the research brief (already loaded for upcoming companies)
   - Press **c** to call, **s** to skip, **n** for next contact, **b** to go back, **q** to quit
   - Press **ENTER** when the call ends
   - Press **1–5** for the outcome (Interested / Voicemail / No answer / Not interested / Wrong number)
   - Press **b** at the outcome or tag screen to cancel back to the card
   - Press **b** at the tag screen to re-pick the outcome
   - Type any notes and press **ENTER** (or just ENTER to skip)
4. Press **q** at any time — your position saves automatically
5. Run `./sdr` again and select the `[in progress]` list to resume

**Going back** — pressing **b** on any card returns you to the previous company. If that company already had a call logged, the log entry is removed so you can redo it cleanly.

---

## Contact Prioritization

When a company has multiple contacts, the best one is selected automatically:

| Tier | Titles |
|------|--------|
| 1 (best) | CEO, President, Owner, Founder, Co-Founder |
| 2 | COO, CFO, CTO, VP, Director |
| 3 | Controller, Manager, GM, General Manager |
| 4 | Accountant, Bookkeeper, Finance, Operations |
| 5 | Everything else |

Mobile phone is preferred over office phone. All other contacts are kept for fallback via **n**.

---

## Output & Logs

Logs are written to `logs/call_log_{csv_name}_{date_time}.json` after every call (atomic write — safe to quit mid-session).

Each entry includes full HubSpot engagement fields for direct import:

```json
{
  "hs_call_title": "SDR Call — BluPlanet Recycling",
  "hs_call_direction": "OUTBOUND",
  "hs_call_disposition": "CONNECTED",
  "hs_call_duration": 124000,
  "hs_timestamp": "2026-06-10T14:23:00Z",
  "hs_call_body": "Outcome: Interested\nTag: Book demo\nNotes: ...",
  "contact": { "firstname": "Devin", "lastname": "Goss", ... },
  "sdr_notes": { "outcome": "interested", "quick_tag": "Book demo", ... }
}
```

---

## Session Files

Progress is stored in `.sessions/` (hidden, stays out of Finder). Your CSV is automatically renamed:

```
Quickbooks_accounts.csv          ← fresh, no session
[in progress] Quickbooks...csv   ← paused mid-session
[completed] Quickbooks...csv     ← all companies done
```

If you choose **Start over**, the session file is deleted and a new log file is created (old log is preserved).

---

## Troubleshooting

**"Missing required key: ANTHROPIC_API_KEY"**
Open `.env`, paste your key on the right side of `=`. No spaces.

**"Missing required key: BROWSERLESS_TOKEN"**
Same for your Browserless token. Get one at [browserless.io](https://www.browserless.io).

**Website shows "not available" in research**
The scrape timed out or was blocked. The AI brief will still run using web search. Normal behaviour for sites that block bots.

**CSV doesn't appear in the menu**
The `.csv` file must be in the same directory as the `sdr` binary.

**Build fails: "go: command not found"**
Install Go from [go.dev/dl](https://go.dev/dl/) and open a new terminal window.
