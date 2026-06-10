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
	Outcome       string         `json:"outcome"`
	QuickTag      string         `json:"quick_tag"`
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

var dispositionMap = map[string]string{
	"interested":     "CONNECTED",
	"voicemail":      "LEFT_VOICEMAIL",
	"no_answer":      "NO_ANSWER",
	"not_interested": "CONNECTED",
	"wrong_number":   "WRONG_NUMBER",
}

var outcomeLabels = map[string]string{
	"interested":     "Interested",
	"voicemail":      "Voicemail",
	"no_answer":      "No answer",
	"not_interested": "Not interested",
	"wrong_number":   "Wrong number / bad data",
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
	phone string,
	outcome, quickTag, freeText string,
	brief research.Brief,
	callStart, callEnd time.Time,
) CallLog {
	duration := callEnd.Sub(callStart).Milliseconds()
	if duration < 0 {
		duration = 0
	}

	outcomeLabel := outcomeLabels[outcome]
	if outcomeLabel == "" {
		outcomeLabel = outcome
	}

	var bodyParts []string
	bodyParts = append(bodyParts, "Outcome: "+outcomeLabel)
	if quickTag != "" {
		bodyParts = append(bodyParts, "Tag: "+quickTag)
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
		HsCallDisposition: dispositionMap[outcome],
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
			Outcome:  outcome,
			QuickTag: quickTag,
			FreeText: freeText,
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
