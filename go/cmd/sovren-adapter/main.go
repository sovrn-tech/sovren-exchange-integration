// Command sovren-adapter is the runnable reference adapter (contract:
// go-client-api.md §cmd/sovren-adapter): one binary hosting the scanner /
// withdrawals / sweeper / reconciler services plus the Prometheus and admin
// listeners. Services self-register via init() in their own files.
//
// Usage:
//
//	sovren-adapter --config adapter.yaml <service ...|all>
//
// Exit codes: 0 clean stop, 1 config error, 2 storage error,
// 3 chain-connectivity error at startup.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/postgres"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

// Version / Commit are stamped via -ldflags at release build time.
var (
	Version = "dev"
	Commit  = "none"
)

const (
	exitOK           = 0
	exitConfigError  = 1
	exitStorageError = 2
	exitChainError   = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("sovren-adapter", flag.ContinueOnError)
	configPath := fs.String("config", "adapter.yaml", "path to adapter.yaml")
	if err := fs.Parse(args); err != nil {
		return exitConfigError
	}
	log := logging.New("sovren-adapter")

	services := fs.Args()
	if len(services) == 0 {
		fmt.Fprintf(os.Stderr, "usage: sovren-adapter --config adapter.yaml <service ...|all>\nregistered services: %s\n",
			strings.Join(registeredServices(), ", "))
		return exitConfigError
	}
	runners := map[string]RunFunc{}
	for _, name := range services {
		if name == "all" {
			for n, fn := range registry {
				runners[n] = fn
			}
			continue
		}
		fn, ok := registry[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown service %q; registered services: %s\n",
				name, strings.Join(registeredServices(), ", "))
			return exitConfigError
		}
		runners[name] = fn
	}

	cfg, manifest, err := LoadConfig(*configPath)
	if err != nil {
		log.Error("configuration error", logging.FieldErrorCode, "CONFIG_INVALID", "error", err.Error())
		return exitConfigError
	}

	store, err := openStore(cfg)
	if err != nil {
		log.Error("storage error", logging.FieldErrorCode, "STORAGE_OPEN_FAILED", "error", err.Error())
		return exitStorageError
	}
	defer store.Close()

	m := metrics.NewSet()
	m.BuildInfo.WithLabelValues(Version, Commit).Set(1)

	chainClient, err := buildClient(cfg, manifest, m)
	if err != nil {
		log.Error("chain client error", logging.FieldErrorCode, "CHAIN_CLIENT_FAILED", "error", err.Error())
		return exitChainError
	}
	defer chainClient.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Startup connectivity + chain-identity check (exit 3 on failure).
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	status, err := chainClient.NodeStatus(probeCtx)
	cancel()
	if err != nil {
		log.Error("chain unreachable at startup", logging.FieldErrorCode, "CHAIN_UNREACHABLE", "error", err.Error())
		return exitChainError
	}
	if status.ChainID != manifest.ChainID {
		log.Error("node chain_id does not match manifest",
			logging.FieldErrorCode, "WRONG_CHAIN_ID",
			"node_chain_id", status.ChainID, "manifest_chain_id", manifest.ChainID)
		return exitChainError
	}

	if err := seedWatchSet(ctx, store, cfg, manifest.ChainID); err != nil {
		log.Error("watch-set seed failed", logging.FieldErrorCode, "WATCH_SEED_FAILED", "error", err.Error())
		return exitStorageError
	}

	deps := &Deps{
		Config:   cfg,
		Manifest: manifest,
		Store:    store,
		Client:   chainClient,
		Metrics:  m,
		Logger:   log,
	}

	// Bind the metrics + admin listeners before any worker starts: these are
	// required operational surfaces (a custody adapter must not run blind),
	// so a bind failure is fatal here rather than a goroutine-only log.
	metricsSrv, err := serveHTTP(cfg.Metrics.Listen, ":9464", metricsMux(m), log, "metrics")
	if err != nil {
		log.Error("metrics listener bind failed", logging.FieldErrorCode, "LISTENER_BIND_FAILED", "error", err.Error())
		return exitConfigError
	}
	adminSrv, err := serveHTTP(cfg.Admin.Listen, "127.0.0.1:9465", adminMux(deps), log, "admin")
	if err != nil {
		log.Error("admin listener bind failed", logging.FieldErrorCode, "LISTENER_BIND_FAILED", "error", err.Error())
		_ = metricsSrv.Close()
		return exitConfigError
	}

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	runCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	for name, fn := range runners {
		wg.Add(1)
		go func(name string, fn RunFunc) {
			defer wg.Done()
			slog := log.With(logging.FieldService, name)
			slog.Info("service starting", "version", Version)
			if err := fn(runCtx, deps); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("service failed", "error", err.Error())
				errOnce.Do(func() { firstErr = err })
				cancelAll()
			}
			slog.Info("service stopped")
		}(name, fn)
	}
	wg.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}

	if firstErr != nil {
		return exitStorageError
	}
	return exitOK
}

func openStore(cfg *Config) (storage.Store, error) {
	switch cfg.Storage.Backend {
	case "sqlite":
		return sqlite.Open(cfg.Storage.DSN)
	case "postgres":
		return postgres.Open(cfg.Storage.DSN)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Storage.Backend)
	}
}

// buildClient wires the RPC-transport client (BlockResults — required by the
// scanner — is served by CometBFT RPC only) with automatic failover to the
// secondary node when configured.
func buildClient(cfg *Config, manifest *client.NetworkManifest, m *metrics.Set) (client.Client, error) {
	opts := []client.Option{
		client.WithChainID(manifest.ChainID),
		client.WithMetrics(m),
		client.WithTimeout(15 * time.Second),
	}
	primary, err := nodeClient(cfg.Nodes.Primary, opts)
	if err != nil {
		return nil, fmt.Errorf("nodes.primary: %w", err)
	}
	if cfg.Nodes.Secondary == nil {
		return primary, nil
	}
	secondary, err := nodeClient(*cfg.Nodes.Secondary, opts)
	if err != nil {
		return nil, fmt.Errorf("nodes.secondary: %w", err)
	}
	return client.NewFailover(primary, secondary, client.FailoverPolicy{}), nil
}

func nodeClient(ep NodeEndpoints, opts []client.Option) (client.Client, error) {
	if ep.RPC != "" {
		return client.NewCometRPC(ep.RPC, opts...)
	}
	return client.NewGRPC(ep.GRPC, opts...)
}

// seedWatchSet upserts config-declared watched addresses (startup
// convenience; the database remains the runtime source of truth).
func seedWatchSet(ctx context.Context, store storage.Store, cfg *Config, chainID string) error {
	for _, w := range cfg.Scanner.Watch {
		err := store.Watch().Upsert(ctx, storage.WatchedAddress{
			ChainID:      chainID,
			Address:      w.Address,
			Kind:         storage.WatchedAddressKind(w.Kind),
			CustomerRef:  w.CustomerRef,
			MemoRequired: w.MemoRequired,
			Active:       true,
		})
		if err != nil {
			return fmt.Errorf("watch %s: %w", w.Address, err)
		}
	}
	return nil
}

func metricsMux(m *metrics.Set) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	return mux
}

// serveHTTP binds the listener SYNCHRONOUSLY before returning: a bind
// failure surfaces as an error to the caller (fatal at startup) rather than
// being logged from inside a goroutine while custody workers start anyway
// with no metrics/admin (pause, reconcile) surface. Serve() still runs in a
// goroutine, but on the already-bound listener. An empty listen falls back
// to the loopback default; admin auth/non-loopback validation happens
// earlier at config load (validateAdminBinding).
func serveHTTP(listen, fallback string, handler http.Handler, log interface {
	Error(msg string, args ...any)
}, name string) (*http.Server, error) {
	if listen == "" {
		listen = fallback
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("%s listener: bind %q: %w", name, listen, err)
	}
	// Addr records the CONCRETE bound address (a ":0" request resolves to an
	// assigned port here); Serve() uses ln, so Addr is informational.
	srv := &http.Server{Addr: ln.Addr().String(), Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(name+" listener failed", "error", err.Error(), "listen", listen)
		}
	}()
	return srv, nil
}
