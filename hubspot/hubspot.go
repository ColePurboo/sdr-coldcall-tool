package hubspot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	ContactID      string
	Title          string
	Disposition    string // HubSpot disposition GUID
	DispositionKey string // internal key, used to set the contact's Disposition property
	SentimentKey   string // internal key, used to set the contact's Sentiments property
	Notes          string // free-text notes written to the contact's Quick Notes field
	Duration       int64  // milliseconds
	Timestamp      string // RFC3339
	Body           string // formatted notes
}

// contactDispositionLabel maps internal disposition keys to the exact enum values
// of the HubSpot contact "disposition" property.
var contactDispositionLabel = map[string]string{
	"no_answer":             "No Answer",
	"number_not_in_service": "Number not in Service",
	"contact_retired":       "Contact Retired",
	"instant_hangup_dm":     "Instant hangup w/DM",
	"connected_dm":          "Connected w/DM",
	"connected_reception":   "Connected w/Reception",
	"need_to_enrich":        "Need to enrich",
	"other":                 "Other",
}

// contactSentimentLabel maps internal sentiment keys to the exact enum values
// of the HubSpot contact "sentiments" property.
var contactSentimentLabel = map[string]string{
	"call_back_later":      "Call back later",
	"pitch_bad_fit":        "Pitch - Bad Fit",
	"pitch_not_interested": "Pitch - Not Interested",
	"pitch_1_2_months":     "Pitch - 1-5 Months",
	"pitch_3_5_months":     "Pitch - 3-5 Months",
	"pitch_6_12_months":    "Pitch 6-12 Months",
	"demo_scheduled":       "Demo Scheduled",
	"hang_up":              "Hang Up",
	"wrong_dm_name":        "Wrong DM Name",
	"dq_lead":              "DQ this lead",
	"not_dm":               "Not the DM",
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
	// Discover every phone-number property (including custom fields) so all of a
	// contact's numbers can be surfaced and tagged. Fall back to the two standard
	// fields if discovery isn't available (e.g. missing properties scope).
	phoneProps, err := fetchPhoneProperties(token)
	if err != nil || len(phoneProps) == 0 {
		phoneProps = standardPhoneProps()
	}

	baseProps := []string{
		"firstname", "lastname", "jobtitle", "email", "company", "associatedcompanyid",
		"city", "state", "industry", "website", "numberofemployees",
	}
	propSet := make(map[string]bool)
	var properties []string
	addProp := func(n string) {
		if n == "" || propSet[n] {
			return
		}
		propSet[n] = true
		properties = append(properties, n)
	}
	for _, p := range baseProps {
		addProp(p)
	}
	for _, p := range phoneProps {
		addProp(p.Name)
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

			companyID := prop("associatedcompanyid")
			companyName := prop("company")
			firstName := prop("firstname")
			lastName := prop("lastname")
			if companyName == "" {
				// Fall back to contact's full name as company key
				companyName = strings.TrimSpace(firstName + " " + lastName)
			}
			if companyName == "" && companyID == "" {
				continue
			}

			// Group by the HubSpot company object when available (reliably keeps
			// the CEO + CFO of the same company together), else by company name.
			key := normalizeKey(companyName)
			if companyID != "" {
				key = "id:" + companyID
			}
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

			// Build the tagged phone list from every discovered phone property.
			var phones []leads.Phone
			for _, pp := range phoneProps {
				phones = append(phones, leads.Phone{
					Number: normalizePhone(prop(pp.Name)),
					Label:  pp.Label,
				})
			}
			phones = leads.DedupePhones(phones)

			contact := leads.Contact{
				FirstName:    firstName,
				LastName:     lastName,
				Title:        prop("jobtitle"),
				Email:        prop("email"),
				CompanyPhone: normalizePhone(prop("phone")),
				MobilePhone:  normalizePhone(prop("mobilephone")),
				Phones:       phones,
				HubSpotID:    fmt.Sprintf("%d", c.VID),
			}
			co.Contacts = append(co.Contacts, contact)
		}

		if !resp.HasMore {
			break
		}
		vidOffset = resp.VidOffset
	}

	// Sort each company's contacts finance-first and treat the top one as primary.
	companies := make([]*leads.Company, 0, len(order))
	for _, key := range order {
		co := companyMap[key]
		leads.SortContacts(co.Contacts)
		co.Primary = co.Contacts[0]
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

	// 3. Update the contact's Disposition and Sentiments properties.
	contactProps := map[string]string{}
	if label, ok := contactDispositionLabel[eng.DispositionKey]; ok {
		contactProps["disposition"] = label
	}
	if label, ok := contactSentimentLabel[eng.SentimentKey]; ok {
		contactProps["sentiments"] = label
	}
	if eng.Notes != "" {
		contactProps["quick_notes"] = eng.Notes
	}
	if len(contactProps) > 0 {
		contactURL := fmt.Sprintf("%s/crm/v3/objects/contacts/%s", baseURL, eng.ContactID)
		patchData, _ := json.Marshal(map[string]any{"properties": contactProps})
		if err := patch(token, contactURL, patchData); err != nil {
			return fmt.Errorf("update contact properties: %w", err)
		}
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

// patch performs an authenticated PATCH with a JSON body.
func patch(token, url string, data []byte) error {
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

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

// phoneProp is a discovered HubSpot contact property that holds a phone number.
type phoneProp struct {
	Name  string // internal property name to request
	Label string // friendly tag shown to the SDR ("mobile", "office", or the HubSpot label)
	rank  int    // sort order: mobile (0), office (1), everything else (2)
}

// phoneNameHints are substrings that mark a property as a phone number when the
// HubSpot fieldType isn't explicitly "phonenumber".
var phoneNameHints = []string{"phone", "mobile", "cell", "direct", "tel"}

// isPhoneProperty reports whether a contact property holds a phone number, based
// on its HubSpot field type, internal name, or label.
func isPhoneProperty(name, fieldType, label string) bool {
	if fieldType == "phonenumber" {
		return true
	}
	hay := strings.ToLower(name + " " + label)
	for _, kw := range phoneNameHints {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// phoneLabelFor returns the friendly tag for a phone property.
func phoneLabelFor(name, label string) string {
	switch name {
	case "mobilephone":
		return "mobile"
	case "phone":
		return "office"
	}
	if label != "" {
		return label
	}
	return name
}

// phoneRank orders phone properties: mobile first, the standard office phone
// next, then all custom fields.
func phoneRank(name string) int {
	switch name {
	case "mobilephone":
		return 0
	case "phone":
		return 1
	}
	n := strings.ToLower(name)
	if strings.Contains(n, "mobile") || strings.Contains(n, "cell") {
		return 0
	}
	return 2
}

// standardPhoneProps is the fallback set used when property discovery is
// unavailable (e.g. missing scope) — the two standard HubSpot phone fields.
func standardPhoneProps() []phoneProp {
	return []phoneProp{
		{Name: "mobilephone", Label: "mobile", rank: 0},
		{Name: "phone", Label: "office", rank: 1},
	}
}

// fetchPhoneProperties discovers every phone-number contact property in the
// portal (including custom fields) so their numbers can be surfaced and tagged.
func fetchPhoneProperties(token string) ([]phoneProp, error) {
	body, err := get(token, baseURL+"/crm/v3/properties/contacts")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			FieldType string `json:"fieldType"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse contact properties: %w", err)
	}
	var props []phoneProp
	for _, p := range resp.Results {
		if !isPhoneProperty(p.Name, p.FieldType, p.Label) {
			continue
		}
		props = append(props, phoneProp{
			Name:  p.Name,
			Label: phoneLabelFor(p.Name, p.Label),
			rank:  phoneRank(p.Name),
		})
	}
	sort.SliceStable(props, func(i, j int) bool {
		if props[i].rank != props[j].rank {
			return props[i].rank < props[j].rank
		}
		return props[i].Name < props[j].Name
	})
	return props, nil
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
