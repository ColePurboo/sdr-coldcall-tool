package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	AnthropicAPIKey     string
	BrowserlessToken    string
	AircallAPIID        string
	AircallAPIToken     string
	AircallNumberID     string
	AircallUserID       string
	HubSpotAccessToken  string
}

func Load() (*Config, error) {
	// Load .env if present; ignore error (env vars may already be set)
	_ = godotenv.Load()

	cfg := &Config{
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		BrowserlessToken:   os.Getenv("BROWSERLESS_TOKEN"),
		AircallAPIID:       os.Getenv("AIRCALL_API_ID"),
		AircallAPIToken:    os.Getenv("AIRCALL_API_TOKEN"),
		AircallNumberID:    os.Getenv("AIRCALL_NUMBER_ID"),
		AircallUserID:      os.Getenv("AIRCALL_USER_ID"),
		HubSpotAccessToken: os.Getenv("HUBSPOT_ACCESS_TOKEN"),
	}

	var missing []string
	if cfg.AnthropicAPIKey == "" {
		missing = append(missing, "ANTHROPIC_API_KEY")
	}
	if cfg.BrowserlessToken == "" {
		missing = append(missing, "BROWSERLESS_TOKEN")
	}

	if len(missing) > 0 {
		for _, key := range missing {
			fmt.Fprintf(os.Stderr, "✗ Missing required key: %s\n  Add it to your .env file in this directory.\n  See .env.example for the full template.\n\n", key)
		}
		return nil, fmt.Errorf("missing required configuration keys")
	}

	return cfg, nil
}

// LoadPartial loads all config values without requiring Anthropic/Browserless keys.
// Use this for modes (dry-run, no-research) where API keys are optional.
func LoadPartial() *Config {
	_ = godotenv.Load()
	return &Config{
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		BrowserlessToken:   os.Getenv("BROWSERLESS_TOKEN"),
		AircallAPIID:       os.Getenv("AIRCALL_API_ID"),
		AircallAPIToken:    os.Getenv("AIRCALL_API_TOKEN"),
		AircallNumberID:    os.Getenv("AIRCALL_NUMBER_ID"),
		AircallUserID:      os.Getenv("AIRCALL_USER_ID"),
		HubSpotAccessToken: os.Getenv("HUBSPOT_ACCESS_TOKEN"),
	}
}

func (c *Config) IsAircallConfigured() bool {
	return c.AircallAPIID != "" && c.AircallAPIToken != "" && c.AircallNumberID != "" && c.AircallUserID != ""
}
