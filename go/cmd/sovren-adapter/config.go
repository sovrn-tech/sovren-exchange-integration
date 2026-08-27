package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// validateAdminBinding enforces that the admin API — whose routes unpause
// signing/broadcast, alter scanner progression, and resolve safety gates — is
// never exposed to a network without authentication. A non-loopback Listen
// bind is refused unless AuthToken is set.
func validateAdminBinding(a AdminConfig) error {
	listen := strings.TrimSpace(a.Listen)
	if listen == "" || a.AuthToken != "" {
		return nil // empty => loopback fallback in serveHTTP; token => authenticated
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("admin.listen %q: %w", listen, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("admin.listen %q binds a non-loopback address without admin.auth_token: the admin API mutates fund-safety controls and must not be network-exposed unauthenticated", listen)
}

// isLoopbackHost reports whether host is loopback (or an empty/unspecified host,
// which serveHTTP replaces with the loopback fallback). A wildcard bind
// (0.0.0.0, ::, or empty-in-"":port) is NOT loopback.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::":
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SupportedConfigSchemaVersion is the adapter.yaml contract major
// (contracts/adapter-config-and-ops.md).
const SupportedConfigSchemaVersion = 1

// Config mirrors adapter.yaml field-for-field. Unknown keys are errors
// (fail-fast); every economic value is a string integer/decimal — nothing
// here may be hard-coded in adapter logic (FR-040).
type Config struct {
	SchemaVersion   int               `yaml:"schema_version"`
	NetworkManifest string            `yaml:"network_manifest"`
	Nodes           NodesConfig       `yaml:"nodes"`
	Storage         StorageConfig     `yaml:"storage"`
	Scanner         ScannerConfig     `yaml:"scanner"`
	Withdrawals     WithdrawalsConfig `yaml:"withdrawals"`
	Sweeps          SweepsConfig      `yaml:"sweeps"`
	Signer          SignerConfig      `yaml:"signer"`
	Reconciler      ReconcilerConfig  `yaml:"reconciler"`
	Admin           AdminConfig       `yaml:"admin"`
	Metrics         MetricsConfig     `yaml:"metrics"`

	// dir is the config file's directory; relative paths resolve against it.
	dir string
}

type NodeEndpoints struct {
	GRPC string `yaml:"grpc"`
	RPC  string `yaml:"rpc"`
	REST string `yaml:"rest"`
}

type DisagreementConfig struct {
	HeightDivergenceThreshold int    `yaml:"height_divergence_threshold"`
	CheckInterval             string `yaml:"check_interval"`
}

type NodesConfig struct {
	Primary      NodeEndpoints       `yaml:"primary"`
	Secondary    *NodeEndpoints      `yaml:"secondary"`
	Disagreement *DisagreementConfig `yaml:"disagreement"`
}

type StorageConfig struct {
	Backend string `yaml:"backend"`
	DSN     string `yaml:"dsn"`
}

// WatchEntryConfig seeds one watched address at startup (data model §9);
// runtime watch management stays in the database.
type WatchEntryConfig struct {
	Address      string `yaml:"address"`
	Kind         string `yaml:"kind"`
	MemoRequired bool   `yaml:"memo_required"`
	CustomerRef  string `yaml:"customer_ref"`
}

type ScannerConfig struct {
	Confirmations *uint64 `yaml:"confirmations"`
	StartHeight   *uint64 `yaml:"start_height"`
	// WebsocketAccelerator is accepted for contract compatibility; the kit
	// client is unary-only, so acceleration is poll-interval based (see
	// docs/deposit-processing.md — documented deviation).
	WebsocketAccelerator *bool  `yaml:"websocket_accelerator"`
	PollInterval         string `yaml:"poll_interval"`
	// MinimumDepositUsovr parks smaller deposits BELOW_MINIMUM; empty
	// disables parking.
	MinimumDepositUsovr string             `yaml:"minimum_deposit_usovr"`
	Watch               []WatchEntryConfig `yaml:"watch"`
}

type WithdrawalsConfig struct {
	MinimumWithdrawalUsovr string `yaml:"minimum_withdrawal_usovr"`
	MaxFeeUsovr            string `yaml:"max_fee_usovr"`
	GasAdjustment          string `yaml:"gas_adjustment"`
	BroadcastTimeout       string `yaml:"broadcast_timeout"`
	SimulateUnavailable    string `yaml:"simulate_unavailable"`
	// ProhibitedDestinations is the exchange-specific withdrawal blocklist,
	// MERGED with the default module-account set (never a replacement). Each
	// entry must be a valid sovr account address; the workflow rejects any
	// withdrawal whose destination is in the combined set (FR-032 item 1).
	ProhibitedDestinations []string `yaml:"prohibited_destinations"`
}

type SweepsConfig struct {
	Strategy                     string `yaml:"strategy"`
	MinimumSweepAmountUsovr      string `yaml:"minimum_sweep_amount_usovr"`
	MaximumFeePercentageForSweep string `yaml:"maximum_fee_percentage_for_sweep"`
	FeeReserveUsovr              string `yaml:"fee_reserve_usovr"`
	HotWallet                    string `yaml:"hot_wallet"`
	// FeeWalletMaxSpendUsovr caps cumulative FEE_FUND fee-wallet spend within
	// FeeWalletSpendWindowBlocks recent blocks ("" / "0" = no cap; recommended
	// for the FEE_FUND strategy — see docs/sweeping.md).
	FeeWalletMaxSpendUsovr     string `yaml:"fee_wallet_max_spend_usovr"`
	FeeWalletSpendWindowBlocks uint64 `yaml:"fee_wallet_spend_window_blocks"`
}

type SignerConfig struct {
	Kind     string `yaml:"kind"`
	Endpoint string `yaml:"endpoint"`
}

type ReconcilerConfig struct {
	WalletInterval      string `yaml:"wallet_interval"`
	FullAddressInterval string `yaml:"full_address_interval"`
}

type AdminConfig struct {
	Listen string `yaml:"listen"`
	// AuthToken, when set, is required as `Authorization: Bearer <token>` on
	// every admin request (constant-time compare). Supports ${ENV} expansion so
	// the token is never committed. A non-loopback Listen bind REQUIRES a token.
	AuthToken string `yaml:"auth_token"`
}

type MetricsConfig struct {
	Listen string `yaml:"listen"`
}

var integerRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// LoadConfig reads adapter.yaml (strict — unknown keys are errors), expands
// environment references in the storage DSN, loads the referenced network
// manifest, and cross-validates the two (unsafe-local signer is refused when
// the manifest network_type is mainnet).
func LoadConfig(path string) (*Config, *client.NetworkManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.dir = filepath.Dir(path)

	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}

	// Secrets reach the DSN via environment expansion only, never committed
	// (contract). An unresolved reference fails fast.
	dsn, err := expandEnvStrict(cfg.Storage.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: storage.dsn: %w", path, err)
	}
	cfg.Storage.DSN = dsn

	token, err := expandEnvStrict(cfg.Admin.AuthToken)
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: admin.auth_token: %w", path, err)
	}
	cfg.Admin.AuthToken = token
	if err := validateAdminBinding(cfg.Admin); err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}

	manifest, err := client.LoadManifest(cfg.ManifestPath())
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: network_manifest: %w", path, err)
	}
	if cfg.Signer.Kind == "unsafe-local" && manifest.NetworkType == "mainnet" {
		return nil, nil, fmt.Errorf("config %s: signer.kind unsafe-local is refused when the network manifest declares network_type mainnet", path)
	}
	return &cfg, manifest, nil
}

// ManifestPath resolves the manifest path against the config file directory.
func (c *Config) ManifestPath() string {
	if filepath.IsAbs(c.NetworkManifest) {
		return c.NetworkManifest
	}
	return filepath.Join(c.dir, c.NetworkManifest)
}

func (c *Config) validate() error {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.SchemaVersion != SupportedConfigSchemaVersion {
		fail("schema_version: got %d, supported is %d", c.SchemaVersion, SupportedConfigSchemaVersion)
	}
	if strings.TrimSpace(c.NetworkManifest) == "" {
		fail("network_manifest: required")
	}
	if c.Nodes.Primary.RPC == "" && c.Nodes.Primary.GRPC == "" {
		fail("nodes.primary: at least one of rpc or grpc is required")
	}
	if c.Nodes.Disagreement != nil {
		if c.Nodes.Disagreement.HeightDivergenceThreshold <= 0 {
			fail("nodes.disagreement.height_divergence_threshold: must be > 0")
		}
		requireDuration(&errs, "nodes.disagreement.check_interval", c.Nodes.Disagreement.CheckInterval)
	}

	switch c.Storage.Backend {
	case "sqlite", "postgres":
	case "":
		fail("storage.backend: required (sqlite | postgres)")
	default:
		fail("storage.backend: %q is not sqlite | postgres", c.Storage.Backend)
	}
	if strings.TrimSpace(c.Storage.DSN) == "" {
		fail("storage.dsn: required")
	}

	requireDuration(&errs, "scanner.poll_interval", c.Scanner.PollInterval)
	requireInteger(&errs, "scanner.minimum_deposit_usovr", c.Scanner.MinimumDepositUsovr)
	for i, w := range c.Scanner.Watch {
		p := fmt.Sprintf("scanner.watch[%d]", i)
		if r := address.ValidateAccountAddress(w.Address); !r.Valid {
			fail("%s.address: %s", p, r.ErrorCode)
		}
		if !storage.WatchedAddressKind(w.Kind).Valid() {
			fail("%s.kind: %q is not one of %v", p, w.Kind, storage.AllWatchedAddressKinds)
		}
	}

	requireInteger(&errs, "withdrawals.minimum_withdrawal_usovr", c.Withdrawals.MinimumWithdrawalUsovr)
	requireInteger(&errs, "withdrawals.max_fee_usovr", c.Withdrawals.MaxFeeUsovr)
	requireDecimal(&errs, "withdrawals.gas_adjustment", c.Withdrawals.GasAdjustment)
	requireDuration(&errs, "withdrawals.broadcast_timeout", c.Withdrawals.BroadcastTimeout)
	switch c.Withdrawals.SimulateUnavailable {
	case "", "queue", "static":
	default:
		fail("withdrawals.simulate_unavailable: %q is not queue | static", c.Withdrawals.SimulateUnavailable)
	}
	for i, d := range c.Withdrawals.ProhibitedDestinations {
		if res := address.ValidateAccountAddress(d); !res.Valid {
			fail("withdrawals.prohibited_destinations[%d]: %q is not a valid sovr account address (%s)", i, d, res.ErrorCode)
		}
	}

	if c.Sweeps.Strategy != "" && !storage.SweepStrategy(c.Sweeps.Strategy).Valid() {
		fail("sweeps.strategy: %q is not one of %v", c.Sweeps.Strategy, storage.AllSweepStrategies)
	}
	requireInteger(&errs, "sweeps.minimum_sweep_amount_usovr", c.Sweeps.MinimumSweepAmountUsovr)
	requireDecimal(&errs, "sweeps.maximum_fee_percentage_for_sweep", c.Sweeps.MaximumFeePercentageForSweep)
	requireInteger(&errs, "sweeps.fee_reserve_usovr", c.Sweeps.FeeReserveUsovr)
	if c.Sweeps.HotWallet != "" {
		if r := address.ValidateAccountAddress(c.Sweeps.HotWallet); !r.Valid {
			fail("sweeps.hot_wallet: %s", r.ErrorCode)
		}
	}

	switch c.Signer.Kind {
	case "", "grpc-remote", "exec", "unsafe-local":
	default:
		fail("signer.kind: %q is not grpc-remote | exec | unsafe-local", c.Signer.Kind)
	}

	requireDuration(&errs, "reconciler.wallet_interval", c.Reconciler.WalletInterval)
	requireDuration(&errs, "reconciler.full_address_interval", c.Reconciler.FullAddressInterval)

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ScannerRuntime resolves the scanner section into runtime values.
func (c *Config) ScannerRuntime() (confirmations uint64, startHeight uint64, poll time.Duration, err error) {
	confirmations = deposits.DefaultConfirmations
	if c.Scanner.Confirmations != nil {
		confirmations = *c.Scanner.Confirmations
		// Supported crediting range is 1..12 committed blocks: CometBFT is
		// single-block-final (1 = protocol finality), and 12 is the top of the
		// operational-buffer band. The exchange picks the final value.
		if confirmations < 1 || confirmations > 12 {
			return 0, 0, 0, fmt.Errorf("scanner.confirmations: must be between 1 and 12")
		}
	}
	if c.Scanner.StartHeight != nil {
		startHeight = *c.Scanner.StartHeight
	}
	if c.Scanner.PollInterval != "" {
		poll, err = time.ParseDuration(c.Scanner.PollInterval)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("scanner.poll_interval: %w", err)
		}
	}
	return confirmations, startHeight, poll, nil
}

func requireDuration(errs *[]string, path, v string) {
	if v == "" {
		return
	}
	if d, err := time.ParseDuration(v); err != nil || d <= 0 {
		*errs = append(*errs, fmt.Sprintf("%s: %q is not a positive Go duration", path, v))
	}
}

func requireInteger(errs *[]string, path, v string) {
	if v == "" {
		return
	}
	if !integerRe.MatchString(v) {
		*errs = append(*errs, fmt.Sprintf("%s: %q is not a canonical base-10 integer string", path, v))
	}
}

var decimalRe = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func requireDecimal(errs *[]string, path, v string) {
	if v == "" {
		return
	}
	if !decimalRe.MatchString(v) {
		*errs = append(*errs, fmt.Sprintf("%s: %q is not a canonical decimal string", path, v))
	}
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvStrict expands ${VAR} references; an unset variable is an error
// (never a silently empty secret).
func expandEnvStrict(s string) (string, error) {
	var missing []string
	out := envRefRe.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRefRe.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ref
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable(s) %s", strings.Join(missing, ", "))
	}
	return out, nil
}
