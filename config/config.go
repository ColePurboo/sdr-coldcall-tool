package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	AnthropicAPIKey  string
	BrowserlessToken string
	AircallAPIID     string
	AircallAPIToken  string
}

func Load() (*Config, error) {
	// Load .env if present; ignore error (env vars may already be set)
	_ = godotenv.Load()

	cfg := &Config{
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		BrowserlessToken: os.Getenv("BROWSERLESS_TOKEN"),
		AircallAPIID:     os.Getenv("AIRCALL_API_ID"),
		AircallAPIToken:  os.Getenv("AIRCALL_API_TOKEN"),
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
