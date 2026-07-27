package leads

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Phone is a single dialable number with a human-readable type tag
// (e.g. "mobile", "office", or a custom HubSpot property label).
type Phone struct {
	Number string // normalized, typically E.164 (+1XXXXXXXXXX)
	Label  string
}

type Contact struct {
	FirstName    string
	LastName     string
	Title        string
	Email        string
	CompanyPhone string
	MobilePhone  string
	Phones       []Phone // all tagged numbers, mobile-first; source of truth for the dialer
	HubSpotID    string   // populated when loaded from HubSpot; empty for CSV contacts
}

type Company struct {
	Name      string
	City      string
	Province  string
	Employees string
	Industry  string
	Website   string
	Contacts  []Contact
	Primary   Contact
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// financeFirstRank ranks a title for scroll order within a company:
// 0 = senior finance / the finance decision-maker (CFO, Controller, VP Finance…),
// 1 = other finance & accounting roles, 2 = top executives, 3 = everyone else.
// This is the order the SDR wants to work contacts in — the finance DM first,
// then the rest of finance, then the CEO/owner, then everyone else.
func financeFirstRank(title string) int {
	t := strings.ToLower(title)
	hasFinance := strings.Contains(t, "financ")
	senior := containsAny(t, []string{"chief", "vp", "vice president", "svp", "evp", "head of", "director"})

	// Tier 0: the finance decision-maker.
	if containsAny(t, []string{"cfo", "chief financial", "controller", "comptroller", "treasur"}) ||
		(hasFinance && senior) {
		return 0
	}
	// Tier 1: other finance / accounting roles.
	if containsAny(t, []string{"financ", "accountant", "accounting", "accounts", "bookkeep", "payroll", "cpa"}) {
		return 1
	}
	// Tier 2: top executives.
	if containsAny(t, []string{"ceo", "chief executive", "president", "owner", "founder", "co-founder", "co founder"}) {
		return 2
	}
	return 3
}

// SortContacts orders a company's contacts finance-first, then executives, then
// the rest, preserving original order within each bucket. Mutates in place.
func SortContacts(contacts []Contact) {
	sort.SliceStable(contacts, func(i, j int) bool {
		return financeFirstRank(contacts[i].Title) < financeFirstRank(contacts[j].Title)
	})
}

// DedupePhones drops numbers that are empty or repeat an earlier number
// (first label wins), preserving order.
func DedupePhones(in []Phone) []Phone {
	seen := make(map[string]bool)
	var out []Phone
	for _, p := range in {
		if p.Number == "" || seen[p.Number] {
			continue
		}
		seen[p.Number] = true
		out = append(out, p)
	}
	return out
}

var nonDigit = regexp.MustCompile(`\D`)

// normalizePhone converts a phone string to E.164 (+1XXXXXXXXXX) format.
// Handles multi-phone cells by taking the first number only.
func normalizePhone(raw string) string {
	// Take first phone if comma-separated
	raw = strings.Split(raw, ",")[0]
	raw = strings.TrimSpace(raw)
	// Strip extensions like "ext 123"
	if idx := strings.Index(strings.ToLower(raw), "ext"); idx != -1 {
		raw = raw[:idx]
	}
	digits := nonDigit.ReplaceAllString(raw, "")
	if digits == "" {
		return ""
	}
	// Strip leading 1 if 11 digits
	if len(digits) == 11 && digits[0] == '1' {
		digits = digits[1:]
	}
	if len(digits) != 10 {
		return raw // can't normalize, return as-is
	}
	return "+1" + digits
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

// Load parses a CSV file and returns grouped, prioritized companies.
func Load(path string) ([]*Company, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("CSV has no data rows")
	}

	// Build column index from header
	header := rows[0]
	col := make(map[string]int)
	for i, h := range header {
		col[strings.TrimSpace(strings.ToLower(h))] = i
	}

	get := func(row []string, key string) string {
		idx, ok := col[key]
		if !ok || idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}

	companyMap := make(map[string]*Company)
	var order []string // preserve insertion order

	for _, row := range rows[1:] {
		companyName := get(row, "company name")
		if companyName == "" {
			continue
		}
		key := normalizeKey(companyName)

		co, exists := companyMap[key]
		if !exists {
			website := get(row, "website")
			website = strings.TrimPrefix(website, "http://")
			website = strings.TrimPrefix(website, "https://")
			website = strings.TrimSuffix(website, "/")

			co = &Company{
				Name:      companyName,
				City:      get(row, "company city"),
				Province:  get(row, "company state"),
				Employees: get(row, "# employees"),
				Industry:  get(row, "industry"),
				Website:   website,
			}
			companyMap[key] = co
			order = append(order, key)
		}

		mobile := normalizePhone(get(row, "mobile phone"))
		office := normalizePhone(get(row, "company phone"))
		contact := Contact{
			FirstName:    get(row, "first name"),
			LastName:     get(row, "last name"),
			Title:        get(row, "job title"),
			Email:        get(row, "email"),
			CompanyPhone: office,
			MobilePhone:  mobile,
			Phones: DedupePhones([]Phone{
				{Number: mobile, Label: "mobile"},
				{Number: office, Label: "office"},
			}),
		}
		co.Contacts = append(co.Contacts, contact)
	}

	// Sort each company's contacts finance-first and treat the top one as primary.
	companies := make([]*Company, 0, len(order))
	for _, key := range order {
		co := companyMap[key]
		SortContacts(co.Contacts)
		co.Primary = co.Contacts[0]
		companies = append(companies, co)
	}

	return companies, nil
}

// PhonesFor returns the ordered, de-duplicated list of tagged numbers the dialer
// cycles through. Uses the explicit Phones list when present, otherwise falls
// back to the two legacy fields (mobile-first).
func PhonesFor(c Contact) []Phone {
	if len(c.Phones) > 0 {
		return c.Phones
	}
	return DedupePhones([]Phone{
		{Number: c.MobilePhone, Label: "mobile"},
		{Number: c.CompanyPhone, Label: "office"},
	})
}

// BestPhone returns the default number to dial (first in PhonesFor — mobile preferred).
func BestPhone(c Contact) string {
	if ph := PhonesFor(c); len(ph) > 0 {
		return ph[0].Number
	}
	return ""
}

// PhoneLabel returns the type tag of the default number ("mobile", "office", …).
func PhoneLabel(c Contact) string {
	if ph := PhonesFor(c); len(ph) > 0 {
		return ph[0].Label
	}
	return "none"
}

// ScanCSVFiles scans dir for *.csv files and categorizes them.
type CSVFile struct {
	Path     string
	Base     string
	Status   string // "fresh", "in_progress", "completed"
	Display  string // name without prefix
}

func ScanCSVFiles(dir string) ([]CSVFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []CSVFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		cf := CSVFile{
			Path:    filepath.Join(dir, name),
			Base:    name,
			Display: name,
			Status:  "fresh",
		}
		if strings.HasPrefix(name, "[in progress] ") {
			cf.Status = "in_progress"
			cf.Display = strings.TrimPrefix(name, "[in progress] ")
		} else if strings.HasPrefix(name, "[completed] ") {
			cf.Status = "completed"
			cf.Display = strings.TrimPrefix(name, "[completed] ")
		}
		files = append(files, cf)
	}
	return files, nil
}
