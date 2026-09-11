package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"jobcompass/internal/app"
)

func main() {
	envFile := flag.String("env-file", "", "optional environment file")
	flag.Parse()
	if *envFile != "" {
		if err := app.LoadEnvFile(*envFile); err != nil {
			slog.Error("cannot load environment file", "error", err)
			os.Exit(1)
		}
	}
	config, err := app.LoadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	application, err := app.New(config, nil)
	if err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer application.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := application.Run(ctx); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
