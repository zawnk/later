package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

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

	svc := service.New(s)
	ntfyClient := ntfy.NewNtfyClient(cfg)

	// wire ntfy inbound to service
	if len(cfg.Inbound) > 0 {
		ch := ntfyClient.SubscribeAllWithReconnect()
		go func() {
			for msg := range ch {
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
	go sched.Start()

	// start API
	a := api.New(cfg, svc)
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("starting server", "addr", addr)
	if err := http.ListenAndServe(addr, a.Routes()); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
