// Package main 为 cmdc2api 程序入口：Anthropic Messages → cmdc 单跳直转反向代理。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/B1anYu/cmdc2api/internal/config"
	"github.com/B1anYu/cmdc2api/internal/masq"
	"github.com/B1anYu/cmdc2api/internal/server"
)

func main() {
	cfg := config.Load()

	state := masq.LoadState(cfg.StateFile)
	ver := masq.NewCCVersion()
	ver.Start()

	srv := server.New(cfg, state, ver)
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			srv.CleanupSessions()
		}
	}()

	go func() {
		slog.Info("cmdc2api started",
			"addr", httpSrv.Addr,
			"apiBase", cfg.APIBase,
			"sessionStrategy", cfg.SessionStrategy,
			"zdr", cfg.ZDR,
			"assistantReasoning", cfg.AssistantReasoning,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
