package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/zawnk/later/internal/api"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/ntfy"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/scheduler"
	"github.com/zawnk/later/internal/service"
	"github.com/zawnk/later/internal/store"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/data/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	s, err := store.New(cfg.Server.DataDir)
	if err != nil {
		slog.Error("failed to initialize store", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := service.New(s)
	ntfyClient := ntfy.New(cfg)

	var wg sync.WaitGroup

	if len(cfg.Inbound) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ntfyClient.Run(ctx, func(msg ntfy.ParsedInboundMessage) (*reminder.Reminder, error) {
				return svc.CreateReminder(service.CreateInput{
					Text:           msg.Text,
					OutboundTopics: msg.Outbound,
					Tags:           msg.Tags,
					Priority:       msg.Priority,
				})
			})
		}()
	}

	// start scheduler
	sched := scheduler.New(s, ntfyClient.Send)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.Run(ctx)
	}()

	// start API
	a := api.New(cfg, svc)

	var handler http.Handler = a.Routes()
	if os.Getenv("LATER_HTTP_LOG") == "1" {
		slog.Info("HTTP request logger enabled - /healthz probes suppressed")
		handler = api.LogRequests(handler)
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		slog.Info("shutdown signal received - shutting down http server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("http shutdown error", "err", err)
		}
	}()

	slog.Info("starting server", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "err", err)
		stop()
		wg.Wait()
		os.Exit(1)
	}

	wg.Wait()
	slog.Info("shutdown complete")
}
