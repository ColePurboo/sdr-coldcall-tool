package hubspot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"sdr-dialer/leads"
)

const baseURL = "https://api.hubapi.com"

// HSList represents a HubSpot contact list.
type HSList struct {
	ListID int
	Name   string
	Size   int
}

// Engagement holds call data to write back to HubSpot.
type Engagement struct {
	ContactID   string
	Title       string
	Disposition string // HubSpot disposition GUID
	Duration    int64  // milliseconds
	Timestamp   string // RFC3339
	Body        string // formatted notes
}

// FetchLists retrieves all contact lists from HubSpot, paginating as needed.
func FetchLists(token string) ([]HSList, error) {
	var out []HSList
	offset := 0
	for {
		url := fmt.Sprintf("%s/contacts/v1/lists?count=250&offset=%d", baseURL, offset)
		body, err := get(token, url)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Lists []struct {
				ListID   int    `json:"listId"`
				Name     string `json:"name"`
				MetaData struct {
					Size int `json:"size"`
				} `json:"metaData"`
			} `json:"lists"`
			HasMore bool `json:"has-more"`
			Offset  int  `json:"offset"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parse lists response: %w", err)
		}

		for _, l := range resp.Lists {
			out = append(out, HSList{
				ListID: l.ListID,
				Name:   l.Name,
				Size:   l.MetaData.Size,
			})
		}

		if !resp.HasMore {
			break
		}
		offset = resp.Offset
	}
	return out, nil
}

// LoadContacts fetches all contacts from a HubSpot list and groups them by company,
// mirroring the CSV loader's behaviour. Primary contact is chosen by title tier.
func LoadContacts(token string, listID int) ([]*leads.Company, error) {
	properties := []string{
		"firstname", "lastname", "jobtitle", "email",
		"phone", "mobilephone", "company",
		"city", "state", "industry", "website", "numberofemployees",
	}
	propQuery := "&property=" + strings.Join(properties, "&property=")

	companyMap := make(map[string]*leads.Company)
	var order []string

	vidOffset := 0
	for {
		url := fmt.Sprintf(
			"%s/contacts/v1/lists/%d/contacts/all?count=100%s",
			baseURL, listID, propQuery,
		)
		if vidOffset > 0 {
			url += fmt.Sprintf("&vidOffset=%d", vidOffset)
		}

		body, err := get(token, url)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Contacts []struct {
				VID        int `json:"vid"`
				Properties map[string]struct {
					Value string `json:"value"`
				} `json:"properties"`
			} `json:"contacts"`
			HasMore   bool `json:"has-more"`
			VidOffset int  `json:"vid-offset"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parse contacts response: %w", err)
		}

		for _, c := range resp.Contacts {
			prop := func(key string) string {
				return strings.TrimSpace(c.Properties[key].Value)
			}

			companyName := prop("company")
			firstName := prop("firstname")
			lastName := prop("lastname")
			if companyName == "" {
				// Fall back to contact's full name as company key
				companyName = strings.TrimSpace(firstName + " " + lastName)
			}
			if companyName == "" {
				continue
			}

			key := normalizeKey(companyName)
			co, exists := companyMap[key]
			if !exists {
				website := prop("website")
				website = strings.TrimPrefix(website, "http://")
				website = strings.TrimPrefix(website, "https://")
				website = strings.TrimSuffix(website, "/")

				co = &leads.Company{
					Name:      companyName,
					City:      prop("city"),
					Province:  prop("state"),
					Employees: prop("numberofemployees"),
					Industry:  prop("industry"),
					Website:   website,
				}
				companyMap[key] = co
				order = append(order, key)
			}

			contact := leads.Contact{
				FirstName:    firstName,
				LastName:     lastName,
				Title:        prop("jobtitle"),
				Email:        prop("email"),
				CompanyPhone: normalizePhone(prop("phone")),
				MobilePhone:  normalizePhone(prop("mobilephone")),
				HubSpotID:    fmt.Sprintf("%d", c.VID),
			}
			co.Contacts = append(co.Contacts, contact)
		}

		if !resp.HasMore {
			break
		}
		vidOffset = resp.VidOffset
	}

	// Resolve primary contact per company (highest title tier = lowest tier number).
	companies := make([]*leads.Company, 0, len(order))
	for _, key := range order {
		co := companyMap[key]
		best := co.Contacts[0]
		bestTier := titleTier(best.Title)
		for _, c := range co.Contacts[1:] {
			if t := titleTier(c.Title); t < bestTier {
				best = c
				bestTier = t
			}
		}
		co.Primary = best
		companies = append(companies, co)
	}

	return companies, nil
}

// WriteCallEngagement creates a call engagement in HubSpot and associates it
// with the given contact ID.
func WriteCallEngagement(token string, eng Engagement) error {
	// 1. Create the call object.
	payload := map[string]any{
		"properties": map[string]any{
			"hs_call_title":       eng.Title,
			"hs_call_direction":   "OUTBOUND",
			"hs_call_disposition": eng.Disposition,
			"hs_call_duration":    eng.Duration,
			"hs_timestamp":        eng.Timestamp,
			"hs_call_body":        eng.Body,
			"hs_call_status":      "COMPLETED",
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	respBody, err := post(token, baseURL+"/crm/v3/objects/calls", data)
	if err != nil {
		return fmt.Errorf("create call engagement: %w", err)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil || created.ID == "" {
		return fmt.Errorf("parse engagement response: %w", err)
	}

	// 2. Associate the call with the contact.
	assocURL := fmt.Sprintf(
		"%s/crm/v3/objects/calls/%s/associations/contacts/%s/call_to_contact",
		baseURL, created.ID, eng.ContactID,
	)
	if err := put(token, assocURL); err != nil {
		return fmt.Errorf("associate call with contact: %w", err)
	}

	return nil
}

// get performs an authenticated GET and returns the response body.
func get(token, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HubSpot API %s: %s", resp.Status, body)
	}
	return body, nil
}

// post performs an authenticated POST and returns the response body.
func post(token, url string, data []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HubSpot API %s: %s", resp.Status, body)
	}
	return body, nil
}

// put performs an authenticated PUT (no body, just association).
func put(token, url string) error {
	req, err := http.NewRequest(http.MethodPut, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HubSpot API %s: %s", resp.Status, body)
	}
	return nil
}

// normalizeKey produces a lowercase, whitespace-collapsed company key.
func normalizeKey(name string) string {
	var b strings.Builder
	prev := ' '
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsSpace(r) {
			if !unicode.IsSpace(prev) {
				b.WriteRune(' ')
			}
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return strings.TrimSpace(b.String())
}

// titleTier returns 1 (highest) through 5 (lowest) priority for a job title.
func titleTier(title string) int {
	t := strings.ToLower(title)
	tier1 := []string{"ceo", "president", "owner", "founder", "co-founder", "co founder"}
	tier2 := []string{"coo", "cfo", "cto", "vp ", "vice president", "director"}
	tier3 := []string{"controller", "manager", "gm", "general manager"}
	tier4 := []string{"accountant", "bookkeeper", "finance", "operations", "accounting"}
	for _, kw := range tier1 {
		if strings.Contains(t, kw) {
			return 1
		}
	}
	for _, kw := range tier2 {
		if strings.Contains(t, kw) {
			return 2
		}
	}
	for _, kw := range tier3 {
		if strings.Contains(t, kw) {
			return 3
		}
	}
	for _, kw := range tier4 {
		if strings.Contains(t, kw) {
			return 4
		}
	}
	return 5
}

// normalizePhone converts a raw phone string to E.164 format (best-effort).
func normalizePhone(raw string) string {
	raw = strings.Split(raw, ",")[0]
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(strings.ToLower(raw), "ext"); idx != -1 {
		raw = raw[:idx]
	}
	digits := ""
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits += string(r)
		}
	}
	if digits == "" {
		return ""
	}
	if len(digits) == 11 && digits[0] == '1' {
		digits = digits[1:]
	}
	if len(digits) != 10 {
		return raw
	}
	return "+1" + digits
}
