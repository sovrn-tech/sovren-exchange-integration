package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Deps is everything a service receives from main: shared config, the
// opened store, the (failover-wrapped) chain client, metrics, and logging.
type Deps struct {
	Config   *Config
	Manifest *client.NetworkManifest
	Store    storage.Store
	Client   client.Client
	Metrics  *metrics.Set
	Logger   *slog.Logger
}

// RunFunc runs one adapter service until ctx is done. A nil return is a
// clean stop; an error stops the whole adapter.
type RunFunc func(ctx context.Context, deps *Deps) error

// registry maps service name → runner. Services self-register from init()
// in their own file (one file per owning track — files never shared).
var registry = map[string]RunFunc{}

func register(name string, fn RunFunc) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("sovren-adapter: duplicate service registration %q", name))
	}
	registry[name] = fn
}

func registeredServices() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
