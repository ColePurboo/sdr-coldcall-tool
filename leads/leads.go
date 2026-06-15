package leads

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

type Contact struct {
	FirstName    string
	LastName     string
	Title        string
	Email        string
	CompanyPhone string
	MobilePhone  string
	HubSpotID    string // populated when loaded from HubSpot; empty for CSV contacts
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

		contact := Contact{
			FirstName:    get(row, "first name"),
			LastName:     get(row, "last name"),
			Title:        get(row, "job title"),
			Email:        get(row, "email"),
			CompanyPhone: normalizePhone(get(row, "company phone")),
			MobilePhone:  normalizePhone(get(row, "mobile phone")),
		}
		co.Contacts = append(co.Contacts, contact)
	}

	// Resolve primary contact for each company
	companies := make([]*Company, 0, len(order))
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

// BestPhone returns the best phone number for a contact (mobile preferred).
func BestPhone(c Contact) string {
	if c.MobilePhone != "" {
		return c.MobilePhone
	}
	return c.CompanyPhone
}

// PhoneLabel returns "mobile" or "office" label.
func PhoneLabel(c Contact) string {
	if c.MobilePhone != "" {
		return "mobile"
	}
	return "office"
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
