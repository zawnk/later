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

	"github.com/zawnk/later/internal/api"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/ntfy"
	"github.com/zawnk/later/internal/scheduler"
	"github.com/zawnk/later/internal/service"
	"github.com/zawnk/later/internal/store"
)

func main() {
	configPath := flag.String("config", "/data/config.yaml", "path to config file")
	flag.Parse()

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
	// wire ntfy inbound subscriber
	if len(cfg.Inbound) > 0 {
		msgs := make(chan ntfy.SubscriptionMessage, 32)

		wg.Add(1)
		go func() {
			defer wg.Done()
			ntfyClient.Run(ctx, msgs)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for msg := range msgs {
				rem, err := svc.CreateReminder(msg.Text, msg.Outbound)
				if err != nil {
					slog.Error("failed to create reminder from ntfy", "err", err)
					continue
				}
				slog.Info("reminder created via ntfy", "topic", rem.OutboundTopics, "id", rem.ID, "due", rem.DueAt)

				if err := ntfyClient.SendConfirmation(msg.Inbound, rem); err != nil {
					slog.Error("failed to send confirmation", "err", err)
				}
			}
		}()
	}

	// start scheduler
	sched := scheduler.New(s, ntfyClient.Send)
	wg.Add(1)
	go func() {
		defer wg.Done()
		go sched.Run(ctx)
	}()

	// start API
	a := api.New(cfg, svc)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           a.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		slog.Info("shutting down http server")
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
	}

	wg.Wait()
	slog.Info("shutdown complete")
}
