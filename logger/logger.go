package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sdr-dialer/leads"
	"sdr-dialer/research"
)

type ContactFields struct {
	FirstName    string `json:"firstname"`
	LastName     string `json:"lastname"`
	JobTitle     string `json:"jobtitle"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	Company      string `json:"company"`
	City         string `json:"city"`
	State        string `json:"state"`
	Industry     string `json:"industry"`
	Website      string `json:"website"`
	NumEmployees string `json:"numberofemployees"`
}

type ResearchFields struct {
	WhatTheyDo        string `json:"what_they_do"`
	WhoTheyServe      string `json:"who_they_serve"`
	PaymentComplexity string `json:"payment_complexity"`
	VennFit           string `json:"venn_fit"`
	Hook              string `json:"hook"`
}

type SdrNotes struct {
	Disposition   string         `json:"disposition"`
	Sentiment     string         `json:"sentiment,omitempty"`
	FreeText      string         `json:"free_text"`
	ResearchBrief ResearchFields `json:"research_brief"`
	CallStarted   string         `json:"call_started"`
	CallEnded     string         `json:"call_ended"`
}

type CallLog struct {
	HsCallTitle       string        `json:"hs_call_title"`
	HsCallDirection   string        `json:"hs_call_direction"`
	HsCallDisposition string        `json:"hs_call_disposition"`
	HsCallDuration    int64         `json:"hs_call_duration"`
	HsTimestamp       string        `json:"hs_timestamp"`
	HsCallBody        string        `json:"hs_call_body"`
	Contact           ContactFields `json:"contact"`
	SdrNotes          SdrNotes      `json:"sdr_notes"`
}

// dispositionMap maps internal keys to HubSpot call disposition GUIDs.
var dispositionMap = map[string]string{
	"connected_dm":          "f240bbac-87c9-4f6e-bf70-924b57d47db7", // CONNECTED
	"connected_reception":   "f240bbac-87c9-4f6e-bf70-924b57d47db7", // CONNECTED
	"instant_hangup_dm":     "f240bbac-87c9-4f6e-bf70-924b57d47db7", // CONNECTED
	"no_answer":             "73a0d17f-1163-4015-bdd5-ec830791da20", // NO_ANSWER
	"contact_retired":       "73a0d17f-1163-4015-bdd5-ec830791da20", // NO_ANSWER
	"need_to_enrich":        "73a0d17f-1163-4015-bdd5-ec830791da20", // NO_ANSWER
	"other":                 "73a0d17f-1163-4015-bdd5-ec830791da20", // NO_ANSWER
	"number_not_in_service": "17b47fee-58de-441e-a44c-c6300d46f273", // WRONG_NUMBER
}

var dispositionLabels = map[string]string{
	"no_answer":             "No Answer",
	"number_not_in_service": "Number not in service",
	"contact_retired":       "Contact retired",
	"instant_hangup_dm":     "Instant hangup w/DM",
	"connected_dm":          "Connected w/DM",
	"connected_reception":   "Connected w/Reception",
	"need_to_enrich":        "Need to enrich",
	"other":                 "Other",
}

var sentimentLabels = map[string]string{
	"call_back_later":      "Call back later",
	"pitch_bad_fit":        "Pitch - Bad Fit",
	"pitch_not_interested": "Pitch - Not Interested",
	"pitch_1_2_months":     "Pitch - 1-2 Months",
	"pitch_3_5_months":     "Pitch - 3-5 Months",
	"pitch_6_12_months":    "Pitch - 6-12 Months",
	"demo_scheduled":       "Demo Scheduled",
	"hang_up":              "Hang up",
	"wrong_dm_name":        "Wrong DM Name",
	"dq_lead":              "DQ this lead",
	"not_dm":               "Not the DM",
}

// LogFileName generates the output path for a session's log file.
func LogFileName(csvName string) string {
	base := strings.TrimSuffix(filepath.Base(csvName), ".csv")
	// Strip status prefixes
	for _, p := range []string{"[in progress] ", "[completed] "} {
		base = strings.TrimPrefix(base, p)
	}
	ts := time.Now().Format("2006-01-02_15-04")
	return filepath.Join("logs", fmt.Sprintf("call_log_%s_%s.json", base, ts))
}

// Logger accumulates call logs and atomically writes them to disk.
type Logger struct {
	path string
	logs []CallLog
}

// New creates or reopens a log file at the given path.
func New(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	l := &Logger{path: path}

	// If file exists, load existing entries so we can append
	if data, err := os.ReadFile(path); err == nil && len(data) > 2 {
		_ = json.Unmarshal(data, &l.logs)
	}

	return l, nil
}

// Append records a call log entry and writes to disk atomically.
func (l *Logger) Append(entry CallLog) error {
	l.logs = append(l.logs, entry)
	return l.flush()
}

func (l *Logger) flush() error {
	data, err := json.MarshalIndent(l.logs, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := l.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, l.path)
}

// BuildEntry constructs a CallLog from call components.
func BuildEntry(
	co *leads.Company,
	contact leads.Contact,
	phone, phoneLabel string,
	disposition, sentiment, freeText string,
	brief research.Brief,
	callStart, callEnd time.Time,
) CallLog {
	duration := max(callEnd.Sub(callStart).Milliseconds(), 0)

	dispLabel := dispositionLabels[disposition]
	if dispLabel == "" {
		dispLabel = disposition
	}
	sentLabel := sentimentLabels[sentiment]
	if sentLabel == "" {
		sentLabel = sentiment
	}

	var bodyParts []string
	bodyParts = append(bodyParts, "Disposition: "+dispLabel)
	if sentLabel != "" {
		bodyParts = append(bodyParts, "Sentiment: "+sentLabel)
	}
	if phone != "" {
		num := phone
		if phoneLabel != "" && phoneLabel != "none" {
			num += " (" + phoneLabel + ")"
		}
		bodyParts = append(bodyParts, "Number dialed: "+num)
	}
	if freeText != "" {
		bodyParts = append(bodyParts, "Notes: "+freeText)
	}
	if brief.WhatTheyDo != "" {
		bodyParts = append(bodyParts, "\n[Research] "+brief.WhatTheyDo)
		if brief.VennFit != "" {
			bodyParts = append(bodyParts, "Strong fit: "+brief.VennFit)
		}
	} else if brief.Raw != "" {
		bodyParts = append(bodyParts, "\n[Research] "+brief.Raw)
	}

	return CallLog{
		HsCallTitle:       "SDR Call — " + co.Name,
		HsCallDirection:   "OUTBOUND",
		HsCallDisposition: dispositionMap[disposition],
		HsCallDuration:    duration,
		HsTimestamp:       callStart.UTC().Format(time.RFC3339),
		HsCallBody:        strings.Join(bodyParts, "\n"),
		Contact: ContactFields{
			FirstName:    contact.FirstName,
			LastName:     contact.LastName,
			JobTitle:     contact.Title,
			Email:        contact.Email,
			Phone:        phone,
			Company:      co.Name,
			City:         co.City,
			State:        co.Province,
			Industry:     co.Industry,
			Website:      co.Website,
			NumEmployees: co.Employees,
		},
		SdrNotes: SdrNotes{
			Disposition: disposition,
			Sentiment:   sentiment,
			FreeText:    freeText,
			ResearchBrief: ResearchFields{
				WhatTheyDo:        brief.WhatTheyDo,
				WhoTheyServe:      brief.WhoTheyServe,
				PaymentComplexity: brief.PaymentComplexity,
				VennFit:           brief.VennFit,
				Hook:              brief.Hook,
			},
			CallStarted: callStart.Format(time.RFC3339),
			CallEnded:   callEnd.Format(time.RFC3339),
		},
	}
}

// RemoveLast removes the most recently appended entry and rewrites the file.
func (l *Logger) RemoveLast() error {
	if len(l.logs) == 0 {
		return nil
	}
	l.logs = l.logs[:len(l.logs)-1]
	return l.flush()
}

func (l *Logger) Path() string { return l.path }

// DispositionLabel returns the human-readable label for an internal disposition key.
func DispositionLabel(key string) string {
	if l := dispositionLabels[key]; l != "" {
		return l
	}
	return key
}

// SentimentLabel returns the human-readable label for an internal sentiment key.
func SentimentLabel(key string) string {
	if l := sentimentLabels[key]; l != "" {
		return l
	}
	return key
}

// DispositionGUID returns the HubSpot GUID for a disposition key.
func DispositionGUID(key string) string {
	return dispositionMap[key]
}
