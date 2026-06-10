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

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "web-search-2025-03-05")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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
		Model:     "claude-sonnet-4-6",
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
		Model:     "claude-sonnet-4-6",
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
