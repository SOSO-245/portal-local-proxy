package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	configPath := flag.String("config", "", "JSON configuration file (omitted: loopback defaults)")
	check := flag.Bool("check", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("portal-local-proxy", version)
		return
	}
	logger := log.New(os.Stdout, "portal-local-proxy ", log.Ldate|log.Ltime|log.LUTC)
	cfg := DefaultConfig()
	var err error
	if *configPath != "" {
		cfg, err = LoadConfig(*configPath)
		if err != nil {
			logger.Fatal("configuration rejected: ", err)
		}
	}
	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		logger.Fatal("configuration rejected: ", err)
	}
	defer proxy.Close()
	if *check {
		fmt.Printf("configuration valid; listen=%s probe_mode=%s upstream_configured=%t\n", cfg.Listen, cfg.ProbeMode, cfg.Upstream != "")
		return
	}
	srv := &http.Server{
		Addr: cfg.Listen, Handler: proxy,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
		ErrorLog: logger,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		proxy.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Printf("version=%s listen=%s probe_mode=%s upstream_configured=%t", version, cfg.Listen, cfg.ProbeMode, cfg.Upstream != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal("listener failed")
	}
}
