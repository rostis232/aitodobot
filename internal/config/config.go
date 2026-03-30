package config

import (
	"errors"
	"os"
)

type Config struct {
	TelegramToken         string
	DatabaseURL           string
	MasterKeyBase64       string
	OpenAIDefaultModel    string
	OpenAITranscribeModel string
}

func FromEnv() (Config, error) {
	cfg := Config{
		TelegramToken:         os.Getenv("TELEGRAM_BOT_TOKEN"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		MasterKeyBase64:       os.Getenv("MASTER_KEY"),
		OpenAIDefaultModel:    os.Getenv("OPENAI_DEFAULT_MODEL"),
		OpenAITranscribeModel: os.Getenv("OPENAI_TRANSCRIBE_MODEL"),
	}

	if cfg.OpenAIDefaultModel == "" {
		cfg.OpenAIDefaultModel = "gpt-4o"
	}
	if cfg.OpenAITranscribeModel == "" {
		cfg.OpenAITranscribeModel = "gpt-4o-mini-transcribe"
	}

	if cfg.TelegramToken == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.MasterKeyBase64 == "" {
		return Config{}, errors.New("MASTER_KEY is required (base64, 32 bytes decoded)")
	}

	return cfg, nil
}
