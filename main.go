package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tg-tasks-bot/internal/bot"
	"tg-tasks-bot/internal/config"
	"tg-tasks-bot/internal/crypto"
	"tg-tasks-bot/internal/db"
	"tg-tasks-bot/internal/openai"
	"tg-tasks-bot/internal/repo"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}

	cryptor, err := crypto.NewAESGCMFromBase64(cfg.MasterKeyBase64)
	if err != nil {
		slog.Error("crypto init error", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("db connect error", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.EnsureSchema(ctx, pool); err != nil {
		slog.Error("db schema error", "err", err)
		os.Exit(1)
	}

	r := repo.New(pool, cryptor)
	oa := openai.NewClient()

	tg, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		slog.Error("telegram init error", "err", err)
		os.Exit(1)
	}
	tg.Debug = false

	app := bot.New(bot.Dependencies{
		TG:               tg,
		Repo:             r,
		OpenAI:           oa,
		DefaultChatModel: cfg.OpenAIDefaultModel,
		TranscribeModel:  cfg.OpenAITranscribeModel,
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run(ctx)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = shutdownCtx
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("bot stopped", "err", err)
			os.Exit(1)
		}
	}
}
