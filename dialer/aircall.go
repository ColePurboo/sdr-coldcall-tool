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
	To       string `json:"to"`
	NumberID int    `json:"number_id"`
}

// dialAircall fires a POST /v1/users/{user_id}/calls and initiates an outbound call.
// Returns an error on failure. The caller is responsible for UX.
func dialAircall(cfg *config.Config, toPhone string) error {
	var userID int
	fmt.Sscanf(cfg.AircallUserID, "%d", &userID)
	var numberID int
	fmt.Sscanf(cfg.AircallNumberID, "%d", &numberID)

	payload := aircallDialRequest{To: toPhone, NumberID: numberID}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/users/%d/calls", aircallBaseURL, userID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(cfg.AircallAPIID, cfg.AircallAPIToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Aircall API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

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
	return fmt.Errorf("Aircall API error %d: %s", resp.StatusCode, msg)
}
