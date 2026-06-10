package research

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sdr-dialer/config"
	"sdr-dialer/leads"
)

type Brief struct {
	WhatTheyDo        string `json:"what_they_do"`
	WhoTheyServe      string `json:"who_they_serve"`
	PaymentComplexity string `json:"payment_complexity"`
	VennFit           string `json:"venn_fit"`
	Hook              string `json:"hook"`
	Raw               string `json:"-"` // fallback if JSON parse fails
}

// Run performs the full research pipeline for a company and sends the result on ch.
func Run(co *leads.Company, cfg *config.Config, ch chan<- Brief) {
	scraped := scrapeWebsite(co.Website, cfg.BrowserlessToken)
	webSearch := claudeWebSearch(co, cfg.AnthropicAPIKey)
	brief := claudeReason(co, scraped, webSearch, cfg.AnthropicAPIKey)
	ch <- brief
}

// Prefetcher keeps a sliding window of in-flight research goroutines so results
// are ready before the user reaches each company.
type Prefetcher struct {
	companies     []*leads.Company
	cfg           *config.Config
	ahead         int
	noResearch    bool
	mu            sync.Mutex
	channels      map[int]chan Brief
	doneCount     int32      // atomic: incremented when each goroutine finishes
	browserlessMu sync.Mutex // serializes Browserless API calls (one at a time)
}

func NewPrefetcher(companies []*leads.Company, cfg *config.Config, ahead int, noResearch bool) *Prefetcher {
	return &Prefetcher{
		companies:  companies,
		cfg:        cfg,
		ahead:      ahead,
		noResearch: noResearch,
		channels:   make(map[int]chan Brief),
	}
}

// Chan returns the result channel for index i, starting it and the lookahead
// window if not already running. Safe to call concurrently.
func (p *Prefetcher) Chan(i int) chan Brief {
	p.mu.Lock()
	defer p.mu.Unlock()
	end := i + p.ahead
	if end > len(p.companies) {
		end = len(p.companies)
	}
	for idx := i; idx < end; idx++ {
		if _, exists := p.channels[idx]; !exists {
			ch := make(chan Brief, 1)
			p.channels[idx] = ch
			if p.noResearch {
				ch <- Brief{}
				atomic.AddInt32(&p.doneCount, 1)
			} else {
				co := p.companies[idx]
				go func(co *leads.Company, ch chan Brief) {
					// Browserless: one scrape at a time.
					p.browserlessMu.Lock()
					scraped := scrapeWebsite(co.Website, p.cfg.BrowserlessToken)
					p.browserlessMu.Unlock()
					// Claude calls run in parallel; browserless mutex staggers them ~8s apart.
					webSearch := claudeWebSearch(co, p.cfg.AnthropicAPIKey)
					brief := claudeReason(co, scraped, webSearch, p.cfg.AnthropicAPIKey)
					ch <- brief
					atomic.AddInt32(&p.doneCount, 1)
				}(co, ch)
			}
		}
	}
	return p.channels[i]
}

// Set replaces the channel for index i (used after consuming a result from the
// loading screen to restore it for the main loop).
func (p *Prefetcher) Set(i int, ch chan Brief) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.channels[i] = ch
}

// DoneCount returns how many research goroutines have completed so far.
func (p *Prefetcher) DoneCount() int {
	return int(atomic.LoadInt32(&p.doneCount))
}

// WarmCount returns how many goroutines will be started for the window beginning at from.
func (p *Prefetcher) WarmCount(from int) int {
	end := from + p.ahead
	if end > len(p.companies) {
		end = len(p.companies)
	}
	return end - from
}

// --- Step A: Browserless scrape ---

type browserlessReq struct {
	URL             string   `json:"url"`
	WaitFor         int      `json:"waitFor"`
	RejectResources []string `json:"rejectResources"`
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)
var multiSpaceRe = regexp.MustCompile(`\s{2,}`)

func scrapeWebsite(website, token string) string {
	if website == "" || token == "" {
		return "Website not available"
	}
	url := "https://" + website
	if strings.HasPrefix(website, "http") {
		url = website
	}

	payload, _ := json.Marshal(browserlessReq{
		URL:             url,
		WaitFor:         2000,
		RejectResources: []string{"image", "font", "stylesheet"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://chrome.browserless.io/content?token="+token, bytes.NewReader(payload))
	if err != nil {
		return "Website not available"
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "Website not available"
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "Website not available"
	}

	// Strip HTML tags
	text := htmlTagRe.ReplaceAllString(string(body), " ")
	text = multiSpaceRe.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)

	// Truncate to ~3000 tokens (approx 4 chars/token)
	const maxChars = 12000
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return text
}

// --- Step B: Claude web search ---

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicTool struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
}

func claudePost(payload anthropicRequest, apiKey string, timeout time.Duration) (string, error) {
	body, _ := json.Marshal(payload)

	delays := []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second}
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "web-search-2025-03-05")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return "", err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if resp.StatusCode == http.StatusTooManyRequests && attempt < len(delays) {
			// Honour the server's reset timestamp if present, otherwise use backoff.
			wait := delays[attempt]
			if reset := resp.Header.Get("anthropic-ratelimit-input-tokens-reset"); reset != "" {
				if t, err := time.Parse(time.RFC3339, reset); err == nil {
					if d := time.Until(t) + time.Second; d > wait {
						wait = d
					}
				}
			}
			time.Sleep(wait)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("anthropic API %d: %s", resp.StatusCode, string(respBody))
		}

		var ar anthropicResponse
		if err := json.Unmarshal(respBody, &ar); err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, c := range ar.Content {
			if c.Type == "text" && c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), nil
	}
}

func claudeWebSearch(co *leads.Company, apiKey string) string {
	location := co.City
	if co.Province != "" {
		location += ", " + co.Province
	}
	prompt := fmt.Sprintf(
		"Search for information about %s located in %s, Canada. "+
			"Find: what the business does, their size, any recent news, their customers, "+
			"how they handle payments or business banking, and any notable facts. "+
			"Return only factual information found. Be concise.",
		co.Name, location,
	)

	result, err := claudePost(anthropicRequest{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 500,
		Tools:     []anthropicTool{{Type: "web_search_20250305", Name: "web_search"}},
		Messages:  []anthropicMessage{{Role: "user", Content: []anthropicContent{{Type: "text", Text: prompt}}}},
	}, apiKey, 15*time.Second)
	if err != nil {
		return ""
	}
	return result
}

// --- Step C: Combined reasoning ---

const systemPrompt = `You are a sales research assistant for Venn, a Canadian fintech company.

Venn's products:
- Corporate Visa cards for Canadian SMBs (physical + virtual)
- Expense management and spend controls
- Multi-user card issuance (one card per employee)
- FX capabilities for businesses that transact in USD
- Integrated with accounting software (QuickBooks, Xero)
- Target market: Canadian SMBs with 5-200 employees that have real business expenses
  (contractors, supplies, travel, subscriptions, field staff, etc.)
- Best fit: businesses with multiple employees spending money, or owners tired of
  personal credit cards for business expenses

You will be given:
1. Company info from a lead list (name, industry, size, location)
2. Raw text scraped from their website
3. External research from web search

Your job: reason over all three sources and produce a pre-call research brief
for an SDR who is about to cold call this company.`

func claudeReason(co *leads.Company, scraped, webSearch, apiKey string) Brief {
	userPrompt := fmt.Sprintf(`Company: %s
Location: %s, %s
Industry: %s
Employees: %s
Website: %s

--- WEBSITE CONTENT ---
%s

--- WEB SEARCH RESULTS ---
%s

---

Produce a research brief with exactly these 5 points, each one sentence:
1. WHAT THEY DO: Core business and revenue model in plain language
2. WHO THEY SERVE: Their customers (B2B/B2C, who buys from them)
3. PAYMENT COMPLEXITY: Why this business likely has real expenses (employees, contractors, supplies, travel, equipment, etc.)
4. VENN FIT: The single strongest reason Venn is relevant to this company
5. HOOK: One specific, natural opening line the SDR can use on the call

Format as JSON:
{
  "what_they_do": "...",
  "who_they_serve": "...",
  "payment_complexity": "...",
  "venn_fit": "...",
  "hook": "..."
}

Return only valid JSON. No preamble, no markdown.`,
		co.Name, co.City, co.Province, co.Industry, co.Employees, co.Website, scraped, webSearch,
	)

	result, err := claudePost(anthropicRequest{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 600,
		System:    systemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: []anthropicContent{{Type: "text", Text: userPrompt}}}},
	}, apiKey, 30*time.Second)
	if err != nil {
		return Brief{Raw: fmt.Sprintf("Research unavailable: %v", err)}
	}

	// Strip markdown code fences if present
	result = strings.TrimSpace(result)
	result = strings.TrimPrefix(result, "```json")
	result = strings.TrimPrefix(result, "```")
	result = strings.TrimSuffix(result, "```")
	result = strings.TrimSpace(result)

	var brief Brief
	if err := json.Unmarshal([]byte(result), &brief); err != nil {
		return Brief{Raw: result}
	}
	return brief
}
