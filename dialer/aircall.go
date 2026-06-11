package dialer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"sdr-dialer/config"
)

const aircallBaseURL = "https://api.aircall.io/v1"

type aircallDialRequest struct {
	To     string `json:"to"`
	UserID int    `json:"user_id,omitempty"`
}

type aircallDialResponse struct {
	Call struct {
		ID int `json:"id"`
	} `json:"call"`
}

// dialAircall fires a POST /v1/numbers/{number_id}/calls and returns the call ID.
// Returns (0, err) on failure. The caller is responsible for UX.
func dialAircall(cfg *config.Config, toPhone string) (callID int, err error) {
	payload := aircallDialRequest{To: toPhone}

	if cfg.AircallUserID != "" {
		fmt.Sscanf(cfg.AircallUserID, "%d", &payload.UserID)
	}

	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/numbers/%s/calls", aircallBaseURL, cfg.AircallNumberID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(cfg.AircallAPIID, cfg.AircallAPIToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("Aircall API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errBody struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		msg := errBody.Message
		if msg == "" {
			msg = errBody.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return 0, fmt.Errorf("Aircall API error %d: %s", resp.StatusCode, msg)
	}

	var result aircallDialResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, nil
	}
	return result.Call.ID, nil
}
