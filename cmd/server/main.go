package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"

	"zabbix-exporter/internal/config"
	"zabbix-exporter/internal/log"
	"zabbix-exporter/internal/pipeline"
	"zabbix-exporter/internal/prometheus/exporter"
	"zabbix-exporter/internal/zabbix"
)

const shutdownTimeout = 5 * time.Second

var configPath string

func init() {
	pflag.StringVarP(&configPath, "config", "c", "config.yaml", "Path to configuration file")
	pflag.Parse()
}

func main() {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Error("Failed to load config: %v", err)
		os.Exit(1)
	}
	log.SetLevel(cfg.Log.Level)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	if err := run(context.Background(), cfg, sigCh); err != nil {
		log.Error("Server stopped with error: %v", err)
		os.Exit(1)
	}
}

func run(parent context.Context, cfg *config.Config, sigCh <-chan os.Signal) error {
	rootCtx, rootCancel := context.WithCancel(parent)
	defer rootCancel()
	signalReceived := make(chan struct{})
	if sigCh != nil {
		go func() {
			select {
			case <-sigCh:
				close(signalReceived)
				rootCancel()
			case <-rootCtx.Done():
			}
		}()
	}

	var zabbixClient *zabbix.Client
	if cfg.Zabbix.APIKey != "" {
		zabbixClient = zabbix.NewAPIKeyClientWithTLS(cfg.Zabbix.URL, cfg.Zabbix.APIKey, cfg.Zabbix.TLSSkipVerify)
	} else {
		zabbixClient = zabbix.NewClientWithTLS(cfg.Zabbix.URL, cfg.Zabbix.Username, cfg.Zabbix.Password, cfg.Zabbix.TLSSkipVerify)
	}
	tokenManager := zabbix.NewTokenManager(
		zabbixClient,
		time.Duration(cfg.Zabbix.RefreshTokenInterval)*time.Second,
		5*time.Minute,
	)
	if err := tokenManager.Start(rootCtx); err != nil {
		if rootCtx.Err() != nil {
			return startupCancellationError(parent, signalReceived)
		}
		return fmt.Errorf("start token manager: %w", err)
	}
	log.Debug("Zabbix authentication initialized successfully")
	if rootCtx.Err() != nil {
		return errors.Join(startupCancellationError(parent, signalReceived), stopTokenManager(tokenManager))
	}

	scheduled, err := pipeline.NewScheduledFromApp(zabbixClient, cfg)
	if err != nil {
		_ = stopTokenManager(tokenManager)
		return fmt.Errorf("create scheduled pipeline: %w", err)
	}
	if err := scheduled.Start(rootCtx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		cleanupErr := errors.Join(scheduled.Stop(stopCtx), tokenManager.StopContext(stopCtx))
		cancel()
		if rootCtx.Err() != nil {
			return errors.Join(startupCancellationError(parent, signalReceived), cleanupErr)
		}
		return errors.Join(fmt.Errorf("start scheduled pipeline: %w", err), cleanupErr)
	}
	businessCollector, err := exporter.NewValueExporter(scheduled.Values(), scheduled.Store(), cfg.Prometheus.Push.PageSize)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = scheduled.Stop(stopCtx)
		_ = tokenManager.StopContext(stopCtx)
		return fmt.Errorf("create scheduled exporter: %w", err)
	}

	registry := prometheus.NewRegistry()
	if err := registry.Register(businessCollector); err != nil {
		return fmt.Errorf("register business exporter: %w", err)
	}
	gatherer := prometheus.Gatherers{prometheus.DefaultGatherer, registry}
	ready := &readiness{token: zabbixClient, scheduled: scheduled}
	mux := http.NewServeMux()
	mux.Handle(cfg.Prometheus.Pull.Path, promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.Handle(cfg.Prometheus.Pull.InternalPath, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/ready", ready.ServeHTTP)
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Prometheus.Pull.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Debug("Starting HTTP server on port %d", cfg.Prometheus.Pull.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var runErr error
	select {
	case <-rootCtx.Done():
		runErr = startupCancellationError(parent, signalReceived)
	case err := <-serverErr:
		runErr = fmt.Errorf("HTTP server: %w", err)
	}
	ready.shuttingDown.Store(true)
	log.Debug("Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	var shutdownErr error
	shutdownErr = errors.Join(shutdownErr, server.Shutdown(shutdownCtx))
	shutdownErr = errors.Join(shutdownErr, scheduled.Stop(shutdownCtx))
	shutdownErr = errors.Join(shutdownErr, tokenManager.StopContext(shutdownCtx))
	rootCancel()
	log.Debug("Shutdown complete")
	return errors.Join(runErr, shutdownErr)
}

func startupCancellationError(parent context.Context, signalReceived <-chan struct{}) error {
	select {
	case <-signalReceived:
		return nil
	default:
		return parent.Err()
	}
}

func stopTokenManager(manager *zabbix.TokenManager) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return manager.StopContext(ctx)
}

type readiness struct {
	token        interface{ GetToken() string }
	scheduled    interface{ Ready() error }
	shuttingDown atomic.Bool
}

func (r *readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if r.shuttingDown.Load() {
		http.Error(w, "Not ready: shutting down", http.StatusServiceUnavailable)
		return
	}
	if r.token == nil || r.token.GetToken() == "" {
		http.Error(w, "Not ready: no token", http.StatusServiceUnavailable)
		return
	}
	if r.scheduled != nil {
		if err := r.scheduled.Ready(); err != nil {
			http.Error(w, "Not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ready"))
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
