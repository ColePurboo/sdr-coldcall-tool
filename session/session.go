package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionsDir = ".sessions"

type SessionState struct {
	CSVName         string         `json:"csv_name"`
	CSVPath         string         `json:"csv_path"`
	TotalCompanies  int            `json:"total_companies"`
	CurrentPosition int            `json:"current_position"`
	LastCompanyName string         `json:"last_company_name"`
	Status          string         `json:"status"`
	StartedAt       string         `json:"started_at"`
	LastActiveAt    string         `json:"last_active_at"`
	SessionCount    int            `json:"session_count"`
	ActiveLogFile   string         `json:"active_log_file"`
	CallsMade       int            `json:"calls_made"`
	OutcomeCounts   map[string]int `json:"outcome_counts"`
}

func sessionPath(csvPath string) string {
	base := filepath.Base(csvPath)
	// Strip status prefixes
	for _, p := range []string{"[in progress] ", "[completed] "} {
		base = strings.TrimPrefix(base, p)
	}
	base = strings.TrimSuffix(base, ".csv")
	return filepath.Join(sessionsDir, base+".session.json")
}

// New creates a fresh session for a CSV.
func New(csvPath string, totalCompanies int, logFile string) (*SessionState, error) {
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, err
	}
	now := time.Now().Format(time.RFC3339)
	s := &SessionState{
		CSVName:        filepath.Base(csvPath),
		CSVPath:        csvPath,
		TotalCompanies: totalCompanies,
		Status:         "in_progress",
		StartedAt:      now,
		LastActiveAt:   now,
		SessionCount:   1,
		ActiveLogFile:  logFile,
		OutcomeCounts:  make(map[string]int),
	}
	return s, s.save()
}

// Load returns a session if one exists for the CSV. Returns (nil, false, nil) for fresh CSVs.
func Load(csvPath string) (*SessionState, bool, error) {
	p := sessionPath(csvPath)
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var s SessionState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, false, err
	}
	if s.OutcomeCounts == nil {
		s.OutcomeCounts = make(map[string]int)
	}
	return &s, s.Status == "in_progress", nil
}

// Advance moves the session forward by one company and persists.
func (s *SessionState) Advance(companyName, outcome string) error {
	s.CurrentPosition++
	s.LastCompanyName = companyName
	s.LastActiveAt = time.Now().Format(time.RFC3339)
	if outcome != "" {
		s.CallsMade++
		s.OutcomeCounts[outcome]++
	}
	return s.save()
}

// Resume increments the session count and updates the log file if resuming.
func (s *SessionState) Resume(logFile string) error {
	s.SessionCount++
	s.ActiveLogFile = logFile
	s.LastActiveAt = time.Now().Format(time.RFC3339)
	return s.save()
}

// Retreat moves the session back one company, undoing the last Advance call.
func (s *SessionState) Retreat(outcome string) error {
	if s.CurrentPosition > 0 {
		s.CurrentPosition--
	}
	if outcome != "" && s.CallsMade > 0 {
		s.CallsMade--
		if s.OutcomeCounts[outcome] > 0 {
			s.OutcomeCounts[outcome]--
		}
	}
	s.LastActiveAt = time.Now().Format(time.RFC3339)
	return s.save()
}

// Complete marks the session as done.
func (s *SessionState) Complete() error {
	s.Status = "completed"
	s.LastActiveAt = time.Now().Format(time.RFC3339)
	return s.save()
}

// Delete removes the session file (used for start-over).
func (s *SessionState) Delete() error {
	return os.Remove(sessionPath(s.CSVPath))
}

func (s *SessionState) save() error {
	p := sessionPath(s.CSVPath)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RenameCSV renames a CSV file, replacing any existing status prefix.
func RenameCSV(current string, newPrefix string) (string, error) {
	dir := filepath.Dir(current)
	base := filepath.Base(current)
	for _, p := range []string{"[in progress] ", "[completed] "} {
		base = strings.TrimPrefix(base, p)
	}
	newPath := filepath.Join(dir, newPrefix+base)
	return newPath, os.Rename(current, newPath)
}

// RenderProgressBar renders a 30-char progress bar.
func RenderProgressBar(current, total, width int) string {
	if total == 0 {
		return strings.Repeat("░", width) + "  0%   0 remaining"
	}
	pct := float64(current-1) / float64(total)
	if pct < 0 {
		pct = 0
	}
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	remaining := total - (current - 1)
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("%s  %d%%   %d remaining", bar, int(pct*100), remaining)
}
