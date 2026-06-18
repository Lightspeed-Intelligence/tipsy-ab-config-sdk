package tipsyabconfig

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// defaultNamespaceEnvVar is the environment variable the SDK reads ONCE at
// Init to discover the project default namespace (decision A-3 / design 04
// §B.1). The SDK never hard-codes a default; if this env is empty/unset the
// defaultNamespace stays "" and ns-optional entry points return
// ErrNamespaceRequired.
const defaultNamespaceEnvVar = "PROJECT_DEFAULT_NAMESPACE"

// Config holds startup parameters for Init. All fields except Namespaces are
// optional; the SDK fills in safe defaults that match config-platform-sdk.md
// §3.
type Config struct {
	// Namespaces is the list of business namespaces this process subscribes
	// to. Required; empty list ⇒ Init returns an error.
	Namespaces []string

	// ConfigServiceAddr is the gRPC target for ConfigService. Required.
	//
	// In gRPC mode (Transport=="grpc"/default) the address is parsed by the
	// 方案 Y scheme grammar:
	//   - bare "host:port" (e.g. "ab-config-grpc:50051") → plaintext h2c
	//     (backward compatible).
	//   - native grpc-go resolver targets ("passthrough:///...", "dns:///...",
	//     "unix:...", "xds:///...") → passed through verbatim, plaintext.
	//   - "grpc://host:port" → plaintext h2c (explicit form; query parameters
	//     are rejected).
	//   - "grpcs://host:port[?authority=<domain>&insecure=true]" → TLS.
	//     `authority` overrides the HTTP/2 :authority AND the TLS SNI /
	//     ServerName; `insecure=true` skips certificate verification (Dev /
	//     Origin-Cert direct-IP only — never in production). A missing port is a
	//     parameter error.
	//   - "http://" / "https://" → parameter error in gRPC mode (use
	//     Transport=http for HTTP base URLs).
	// Parameter errors are returned from Init before dialing and are never
	// absorbed by StartupFailOpen.
	ConfigServiceAddr string

	// AbtestServiceAddr is the gRPC target for AbtestService. Optional —
	// if empty, GetConfigStatic still works but GetConfig will behave as if
	// abtest is permanently unavailable (degraded mode).
	AbtestServiceAddr string

	// PullInterval is the fallback PullAll period (default 10 s).
	PullInterval time.Duration

	// PullTimeout is the per-ns PullAll deadline (default 5 s).
	PullTimeout time.Duration

	// PullRetries is the number of exponential-backoff retries used at
	// startup per ns (default 3).
	PullRetries int

	// AbtestTimeout is the per-Compute deadline (default 1.5 s).
	AbtestTimeout time.Duration

	// StartupFailOpen, when true, lets the SDK start with an empty cache if
	// startup PullAll fails. When false (default) Init returns
	// ErrStartupPullFailed and the host should treat that as fatal.
	//
	// This switch ONLY governs the startup PullAll outcome (dial succeeded but
	// the per-namespace PullAll RPCs failed, including auth failures surfaced
	// via TokenProvider). It does NOT absorb parameter-validation errors or
	// gRPC dial (target-resolution) failures — those always return an error
	// from Init regardless of this switch. See Init for the full table.
	//
	// When true and a startup failure is absorbed, Client.Health reports
	// StartupCacheEmpty=true and an OnBackgroundError event with
	// Phase "startup_pull" is fired.
	StartupFailOpen bool

	// Token is the static JWT used for both ConfigService and AbtestService
	// per-RPC credentials. Required unless TokenProvider is set.
	Token string

	// TokenProvider, when non-nil, is consulted on every RPC for a fresh
	// token. Useful for short-lived tokens. Takes precedence over Token.
	TokenProvider func(ctx context.Context) (string, error)

	// Logger is the structured logger; defaults to slog.Default().
	Logger *slog.Logger

	// MaxRecvMessageSize / MaxSendMessageSize override the 512 MB defaults
	// (backend §4.4.1).
	MaxRecvMessageSize int
	MaxSendMessageSize int

	// DialOptions allows callers to inject extra grpc.DialOption (for
	// tests: bufconn dialer + WithContextDialer). Appended after the SDK's
	// own defaults; later options win for the same field.
	DialOptions []grpc.DialOption

	// OnBackgroundError, when non-nil, is invoked on every background pull /
	// subscribe failure for degradation observability (throttled alerting,
	// Prometheus, etc.). It is invoked SYNCHRONOUSLY on the SDK's background
	// goroutine, so it MUST be lightweight and non-blocking: a slow callback
	// will slow the pull/subscribe loop. The SDK recovers panics raised by the
	// callback (so a buggy callback can't kill the background goroutine), but
	// the callback should not rely on that. ev.Phase is one of "startup_pull",
	// "periodic_pull", or "subscribe". The same failure information is also
	// reflected in Client.Health for poll-based consumers.
	OnBackgroundError func(ev BackgroundErrorEvent)

	// DefaultNamespace, when non-empty, overrides the value read from the
	// `PROJECT_DEFAULT_NAMESPACE` environment variable at Init. The normal
	// production path leaves this empty and relies on the env var (decision
	// A-3); the override exists mainly for tests and hosts that prefer to
	// inject the default namespace programmatically. An empty DefaultNamespace
	// AND an empty env var leaves the SDK with no default namespace, in which
	// case ns-optional entry points return ErrNamespaceRequired.
	DefaultNamespace string

	// Transport selects the wire transport: TransportGRPC ("grpc", the
	// default) or TransportHTTP ("http"). When empty, the SDK reads the
	// TIPSY_SDK_TRANSPORT environment variable; if that is also empty it
	// defaults to gRPC. The value is trimmed and lower-cased before comparison;
	// any value other than "grpc"/"http" makes Init return a parameter error
	// (never absorbed by StartupFailOpen).
	//
	// gRPC mode semantics:
	//   - ConfigServiceAddr / AbtestServiceAddr follow the 方案 Y address
	//     grammar (see ConfigServiceAddr): bare host:port and "grpc://" are
	//     plaintext h2c; "grpcs://host:port[?authority=&insecure=true]" is TLS
	//     with optional :authority/SNI override and skip-verify.
	//
	// HTTP mode semantics:
	//   - ConfigServiceAddr / AbtestServiceAddr are interpreted as base URLs
	//     and must start with "http://" or "https://" (trailing "/" trimmed).
	//   - Subscribe streaming is NOT used; the SDK relies solely on periodic
	//     PullAll polling, so config-change propagation latency is bounded by
	//     PullInterval. Consequently Health.SubscribeConnected is always false,
	//     Health.LastSubscribeErr is always nil, and no BackgroundErrorEvent
	//     with Phase "subscribe" is ever fired.
	//   - DialOptions and MaxSendMessageSize are ignored.
	Transport string

	// HTTPClient is an optional *http.Client used in HTTP mode (a test /
	// custom-proxy / custom-TLS injection seam). When nil the SDK builds a
	// default client (no global Timeout — per-call deadlines come from ctx).
	// An injected client's lifecycle is the caller's responsibility: Close only
	// calls CloseIdleConnections on an SDK-built client. Ignored in gRPC mode.
	HTTPClient *http.Client
}

func (c *Config) applyDefaults() {
	if c.PullInterval <= 0 {
		c.PullInterval = 10 * time.Second
	}
	if c.PullTimeout <= 0 {
		c.PullTimeout = 5 * time.Second
	}
	if c.PullRetries <= 0 {
		c.PullRetries = 3
	}
	if c.AbtestTimeout <= 0 {
		c.AbtestTimeout = 1500 * time.Millisecond
	}
	if c.MaxRecvMessageSize <= 0 {
		c.MaxRecvMessageSize = 512 * 1024 * 1024
	}
	if c.MaxSendMessageSize <= 0 {
		c.MaxSendMessageSize = 512 * 1024 * 1024
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	// Default namespace (decision A-3): explicit Config.DefaultNamespace wins;
	// otherwise read the env var ONCE here. Either may be empty, in which case
	// the SDK has no default namespace.
	if c.DefaultNamespace == "" {
		c.DefaultNamespace = os.Getenv(defaultNamespaceEnvVar)
	}
}

// Client is the SDK handle. Construct via Init; tear down via Close.
//
// Client is safe for concurrent use; all exported methods may be called from
// any goroutine.
type Client struct {
	cfg     Config
	metrics *Metrics
	cache   *configCache
	logger  *slog.Logger

	configConn *grpc.ClientConn
	abtestConn *grpc.ClientConn
	configCli  configv1.ConfigServiceClient
	abtestCli  abtestv1.AbtestServiceClient

	// configTr / abtestTr are the transport abstraction the RPC call sites use.
	// In gRPC mode they thinly wrap configCli / abtestCli (byte-for-byte
	// forwarding). In HTTP mode they are the net/http implementations. abtestTr
	// is nil when no AbtestService address was configured (degraded mode).
	configTr configTransport
	abtestTr abtestTransport

	// httpClient is the *http.Client used in HTTP mode. ownsHTTPClient records
	// whether the SDK built it (and therefore should CloseIdleConnections on
	// Close) versus an injected Config.HTTPClient (owned by the caller). Both
	// are zero/false in gRPC mode.
	httpClient     *http.Client
	ownsHTTPClient bool

	subscribedNamespaces []string

	// defaultNamespace is the project default namespace resolved once at Init
	// (Config.DefaultNamespace override > `PROJECT_DEFAULT_NAMESPACE` env >
	// ""). It is the fallback for ns-optional dynamic getConfig and the eager
	// pre-request ns. May be empty (decision A-3 — never hard-coded).
	defaultNamespace string

	// defaultNsSubscribed records whether defaultNamespace is in
	// subscribedNamespaces. When false (or defaultNamespace == "") the eager
	// pre-request in NewAbtestContext is skipped; ns-optional getConfig still
	// resolves to defaultNamespace and surfaces a validation error to the
	// caller via resolveNamespace.
	defaultNsSubscribed bool

	// health is the mutex-protected background-link health state surfaced via
	// Health and updated by fireBackgroundError.
	health healthState

	// Lifecycle.
	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup
	closedOnce sync.Once
}

// Init constructs the SDK, performs startup PullAll for every subscribed
// namespace, and starts the Subscribe stream + 10 s fallback PullAll loop.
//
// Failure-return contract:
//
//   - Parameter errors (empty Namespaces, empty ConfigServiceAddr, neither
//     Token nor TokenProvider set) ALWAYS return an error, regardless of
//     StartupFailOpen.
//   - gRPC dial failures for ConfigService / AbtestService (i.e. target
//     resolution errors; grpc.NewClient connects lazily) ALWAYS return an
//     error and are NOT absorbed by StartupFailOpen.
//   - Startup PullAll failures (dial succeeded but the RPCs failed, including
//     auth failures) are governed by StartupFailOpen: when false (default)
//     Init returns ErrStartupPullFailed and the caller is expected to abort
//     the process; when true the failure is absorbed and Init returns a usable
//     client (empty cache) with a nil error.
//   - Runtime PullAll / Subscribe / Compute failures NEVER abort: the client
//     keeps serving the previous cache (or the caller-supplied default) and
//     reports failures via logs, Metrics, OnBackgroundError, and Health.
//
// TokenProvider errors are NOT returned synchronously from Init: they surface
// during the per-RPC credentials phase of each call, so they appear as PullAll
// failures (absorbed at startup only when StartupFailOpen=true; never aborting
// at runtime).
func Init(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	if len(cfg.Namespaces) == 0 {
		return nil, errors.New("tipsyabconfig: Namespaces must be non-empty")
	}

	// Resolve the transport mode (Config.Transport > TIPSY_SDK_TRANSPORT env >
	// "grpc"). An invalid value is a parameter error, never absorbed by
	// StartupFailOpen.
	transport, err := resolveTransport(cfg.Transport)
	if err != nil {
		return nil, err
	}

	if cfg.ConfigServiceAddr == "" {
		return nil, errors.New("tipsyabconfig: ConfigServiceAddr must be set")
	}
	if cfg.Token == "" && cfg.TokenProvider == nil {
		return nil, errors.New("tipsyabconfig: Token or TokenProvider must be set")
	}

	// In HTTP mode the addresses are base URLs. Validate (and normalise) the
	// non-empty ones; an empty AbtestServiceAddr keeps the existing degraded
	// semantics (validation applies only to non-empty addresses).
	configBaseURL := cfg.ConfigServiceAddr
	abtestBaseURL := cfg.AbtestServiceAddr
	if transport == TransportHTTP {
		configBaseURL, err = validateHTTPBaseURL("ConfigServiceAddr", cfg.ConfigServiceAddr)
		if err != nil {
			return nil, err
		}
		if cfg.AbtestServiceAddr != "" {
			abtestBaseURL, err = validateHTTPBaseURL("AbtestServiceAddr", cfg.AbtestServiceAddr)
			if err != nil {
				return nil, err
			}
		}
	}

	// Sort + de-dup subscribed namespaces (PullAll is ns-serial in
	// dictionary order per §5.1).
	subs := make([]string, 0, len(cfg.Namespaces))
	seen := map[string]struct{}{}
	for _, ns := range cfg.Namespaces {
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		subs = append(subs, ns)
	}
	sort.Strings(subs)
	if len(subs) == 0 {
		return nil, errors.New("tipsyabconfig: Namespaces must contain at least one non-empty value")
	}

	metrics := newMetrics()
	cache := newConfigCache(metrics)

	rootCtx, rootCancel := context.WithCancel(context.Background())

	cli := &Client{
		cfg:                  cfg,
		metrics:              metrics,
		cache:                cache,
		logger:               cfg.Logger,
		subscribedNamespaces: subs,
		defaultNamespace:     cfg.DefaultNamespace,
		rootCtx:              rootCtx,
		rootCancel:           rootCancel,
	}
	if cfg.DefaultNamespace != "" {
		if _, ok := seen[cfg.DefaultNamespace]; ok {
			cli.defaultNsSubscribed = true
		} else {
			// Default ns is not subscribed: warn so misconfiguration is
			// visible. ns-optional getConfig will still resolve to it and
			// then surface ErrNamespaceNotSubscribed from resolveNamespace.
			cli.logger.Warn("tipsyabconfig: project default namespace is not in subscribed Namespaces; eager pre-request disabled",
				"default_namespace", cfg.DefaultNamespace, "namespaces", subs)
		}
	}

	// Build transports. gRPC mode dials gRPC connections and wraps the
	// generated clients; HTTP mode builds net/http transports and dials no gRPC
	// connection.
	switch transport {
	case TransportHTTP:
		// Reuse the injected client or build a default one. A default client
		// has no global Timeout (per-call deadlines come from ctx) and uses the
		// default connection pool.
		httpClient := cfg.HTTPClient
		if httpClient == nil {
			httpClient = &http.Client{}
			cli.ownsHTTPClient = true
		}
		cli.httpClient = httpClient
		ts := bearerCredentialsFromConfig(cfg)
		cli.configTr = newHTTPConfigTransport(httpClient, ts, cfg.MaxRecvMessageSize, configBaseURL)
		if cfg.AbtestServiceAddr != "" {
			cli.abtestTr = newHTTPAbtestTransport(httpClient, ts, cfg.MaxRecvMessageSize, abtestBaseURL)
		}
	default: // TransportGRPC
		// Parse the gRPC target address(es) first (design 方案 Y). Parse errors
		// are parameter errors surfaced BEFORE dialing and are never absorbed by
		// StartupFailOpen.
		configTarget, err := parseGRPCTarget(cfg.ConfigServiceAddr)
		if err != nil {
			rootCancel()
			return nil, err
		}
		cli.warnInsecureSkipVerify("ConfigServiceAddr", configTarget)

		var abtestTarget grpcTarget
		if cfg.AbtestServiceAddr != "" {
			abtestTarget, err = parseGRPCTarget(cfg.AbtestServiceAddr)
			if err != nil {
				rootCancel()
				return nil, err
			}
			cli.warnInsecureSkipVerify("AbtestServiceAddr", abtestTarget)
		}

		// Build gRPC connections.
		cfgConn, err := cli.dial(configTarget)
		if err != nil {
			rootCancel()
			return nil, fmt.Errorf("tipsyabconfig: dial ConfigService: %w", err)
		}
		cli.configConn = cfgConn
		cli.configCli = configv1.NewConfigServiceClient(cfgConn)
		cli.configTr = newGRPCConfigTransport(cli.configCli)

		if cfg.AbtestServiceAddr != "" {
			abConn, err := cli.dial(abtestTarget)
			if err != nil {
				_ = cfgConn.Close()
				rootCancel()
				return nil, fmt.Errorf("tipsyabconfig: dial AbtestService: %w", err)
			}
			cli.abtestConn = abConn
			cli.abtestCli = abtestv1.NewAbtestServiceClient(abConn)
			cli.abtestTr = newGRPCAbtestTransport(cli.abtestCli)
		}
	}

	// Startup PullAll: ns-serial, with retries.
	if err := cli.startupPullAll(ctx); err != nil {
		if !cfg.StartupFailOpen {
			cli.shutdownConns()
			rootCancel()
			return nil, fmt.Errorf("%w: %v", ErrStartupPullFailed, err)
		}
		metrics.cacheEmpty.Add(1)
		cli.logger.Error("tipsyabconfig: startup PullAll failed; running with empty cache (fail-open)", "err", err)
		// Record the absorbed startup failure for observability. This is the
		// single aggregate startup_pull fire (Namespace="") — startupPullAll
		// only logs per-ns and intentionally does not fire, to avoid noisy
		// double-firing of the same startup failure.
		cli.fireBackgroundError(BackgroundErrorEvent{
			Phase: "startup_pull",
			Err:   err,
			Time:  time.Now(),
		})
	}

	// Start background loops. Subscribe is gRPC-only; HTTP mode relies solely
	// on periodic PullAll polling, so runSubscribe is not started there (which
	// is why Health.SubscribeConnected stays false and no "subscribe" phase
	// BackgroundErrorEvent is ever fired in HTTP mode).
	if transport != TransportHTTP {
		cli.wg.Add(1)
		go cli.runSubscribe()
	}
	cli.wg.Add(1)
	go cli.runPullLoop()

	return cli, nil
}

// Namespaces returns the sorted list of namespaces this client subscribed to.
func (c *Client) Namespaces() []string {
	out := make([]string, len(c.subscribedNamespaces))
	copy(out, c.subscribedNamespaces)
	return out
}

// DefaultNamespace returns the project default namespace resolved once at Init
// (Config.DefaultNamespace override > `PROJECT_DEFAULT_NAMESPACE` env var).
// Returns "" when no default namespace was configured (decision A-3 — the SDK
// never hard-codes one).
func (c *Client) DefaultNamespace() string { return c.defaultNamespace }

// isSubscribed reports whether ns is one of the namespaces this client
// subscribed to at Init. subscribedNamespaces is sorted + de-duped and small,
// so a linear scan is fine on the resolve path.
func (c *Client) isSubscribed(ns string) bool {
	for _, s := range c.subscribedNamespaces {
		if s == ns {
			return true
		}
	}
	return false
}

// resolveNamespace applies the design 04 §B.1 namespace-resolution rules:
// explicit ns argument > defaultNamespace > ErrNamespaceRequired. The resolved
// ns is then validated against the subscription set; an unsubscribed ns yields
// ErrNamespaceNotSubscribed (the SDK only consumes subscribed namespaces).
func (c *Client) resolveNamespace(ns string) (string, error) {
	if ns == "" {
		ns = c.defaultNamespace
	}
	if ns == "" {
		return "", ErrNamespaceRequired
	}
	if !c.isSubscribed(ns) {
		return "", fmt.Errorf("%w: %q", ErrNamespaceNotSubscribed, ns)
	}
	return ns, nil
}

// Metrics returns the per-process counter handle. Safe for concurrent use.
func (c *Client) Metrics() *Metrics { return c.metrics }

// Close stops background loops, closes gRPC connections, and releases any
// SDK-owned HTTP client idle connections.
func (c *Client) Close() error {
	c.closedOnce.Do(func() {
		c.rootCancel()
		c.wg.Wait()
		c.shutdownConns()
	})
	return nil
}

func (c *Client) shutdownConns() {
	if c.configConn != nil {
		_ = c.configConn.Close()
		c.configConn = nil
	}
	if c.abtestConn != nil {
		_ = c.abtestConn.Close()
		c.abtestConn = nil
	}
	// HTTP mode has no gRPC connections. Only release idle connections on a
	// client the SDK built; an injected Config.HTTPClient is the caller's to
	// manage.
	if c.ownsHTTPClient && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}

// resolveTransport applies the transport-selection rules: an explicit non-empty
// Config.Transport wins; otherwise the TIPSY_SDK_TRANSPORT env var is consulted;
// an empty result defaults to gRPC. The value is trimmed and lower-cased before
// comparison. Any value other than "grpc"/"http" is a parameter error.
func resolveTransport(configTransport string) (string, error) {
	raw := configTransport
	if strings.TrimSpace(raw) == "" {
		raw = os.Getenv(transportEnvVarName)
	}
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return TransportGRPC, nil
	case TransportGRPC, TransportHTTP:
		return v, nil
	default:
		return "", fmt.Errorf("tipsyabconfig: invalid transport %q (must be %q or %q)", raw, TransportGRPC, TransportHTTP)
	}
}

// validateHTTPBaseURL validates an HTTP-mode base URL: it must start with
// "http://" or "https://". The returned value has any trailing "/" trimmed. The
// field argument names the offending Config field in the error.
func validateHTTPBaseURL(field, addr string) (string, error) {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return "", fmt.Errorf("tipsyabconfig: %s must start with http:// or https:// in HTTP transport mode, got %q", field, addr)
	}
	return strings.TrimRight(addr, "/"), nil
}

// warnInsecureSkipVerify logs a WARN, once per service address, when a parsed
// gRPC target disables TLS certificate verification (grpcs://...?insecure=true).
// Init calls it for each configured service address, so a deployment that sets
// insecure=true on both ConfigServiceAddr and AbtestServiceAddr WARNs twice (no
// cross-address dedup, by design — each misconfigured address is worth flagging).
// This makes accidental production use of the Dev-only skip-verify switch visible
// in logs.
func (c *Client) warnInsecureSkipVerify(field string, tgt grpcTarget) {
	if tgt.useTLS && tgt.insecureSkipVerify {
		c.logger.Warn("tipsyabconfig: TLS certificate verification DISABLED (insecure=true); Dev / Origin-Cert direct-IP only — never use in production",
			"field", field, "authority", tgt.authority)
	}
}

// dial builds a gRPC client connection for the parsed target. tgt carries the
// transport decision (plaintext h2c vs TLS) and TLS details (authority / SNI /
// skip-verify) produced by parseGRPCTarget.
//
// Backward compatibility: when tgt.useTLS is false (bare host:port, grpc://, or
// a native grpc-go resolver target) this is byte-for-byte the legacy plaintext
// path — insecure transport credentials, and cfg.DialOptions still appended last
// so the bufconn test seam keeps overriding the dialer.
func (c *Client) dial(tgt grpcTarget) (*grpc.ClientConn, error) {
	creds := bearerCredentialsFromConfig(c.cfg)

	var transportCreds credentials.TransportCredentials
	if tgt.useTLS {
		// grpc-go v1.80 NewClient note: WithAuthority and the TLS ServerName are
		// taken from the SAME value (authority) so the HTTP/2 :authority and the
		// SNI / certificate-name target agree (Dev: both = the gRPC domain),
		// avoiding any authority/ServerName mismatch validation.
		tlsCfg := &tls.Config{
			InsecureSkipVerify: tgt.insecureSkipVerify, //nolint:gosec // gated by grpcs://?insecure=true (Dev / Origin-Cert direct-IP only)
		}
		if tgt.authority != "" {
			// ServerName drives SNI and the certificate-name target. It is sent
			// in the ClientHello even when InsecureSkipVerify is true, which is
			// required for Traefik SNI routing.
			tlsCfg.ServerName = tgt.authority
		}
		transportCreds = credentials.NewTLS(tlsCfg)
	} else {
		transportCreds = insecure.NewCredentials()
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithPerRPCCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(c.cfg.MaxRecvMessageSize),
			grpc.MaxCallSendMsgSize(c.cfg.MaxSendMessageSize),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	if tgt.useTLS && tgt.authority != "" {
		opts = append(opts, grpc.WithAuthority(tgt.authority))
	}
	// Headless Service + client-side round_robin opt-in: when the dial target
	// uses the gRPC "dns:///" name-resolver scheme, inject a default service
	// config that selects the round_robin load-balancing policy. Every other
	// address form falls through to grpc-go's default pick_first to preserve
	// backwards compatibility (design §Acceptance Criteria #10). The injection
	// goes BEFORE cfg.DialOptions so a test seam can still override the
	// default service config if needed.
	if sc := serviceConfigFor(tgt.dialTarget); sc != "" {
		opts = append(opts, grpc.WithDefaultServiceConfig(sc))
	}
	opts = append(opts, c.cfg.DialOptions...)
	return grpc.NewClient(tgt.dialTarget, opts...)
}

// serviceConfigFor returns the gRPC service-config JSON to inject for a
// given dial target, or "" to leave grpc-go on its defaults.
//
// Headless Service + round_robin opt-in: when the dial target starts with
// "dns:///" (gRPC name-resolver scheme that resolves to all backend pod
// IPs), we inject a round_robin loadBalancingConfig. Every other scheme
// — bare host:port, "grpc://", "grpcs://[?...]", "passthrough:///",
// "unix:" — falls through to grpc-go's default pick_first, preserving
// backwards compatibility (design §Acceptance Criteria #10).
func serviceConfigFor(dialTarget string) string {
	if strings.HasPrefix(dialTarget, "dns:///") {
		return `{"loadBalancingConfig":[{"round_robin":{}}]}`
	}
	return ""
}

// bearerCredentialsFromConfig returns the grpc.PerRPCCredentials matching
// the static-token / dynamic-provider config knobs.
func bearerCredentialsFromConfig(cfg Config) tokenSource {
	if cfg.TokenProvider != nil {
		return tokenSource{dynamic: cfg.TokenProvider}
	}
	return tokenSource{static: cfg.Token}
}

// tokenSource implements grpc.PerRPCCredentials. We deliberately do not
// reuse auth.BearerCredentials (the server-side helper from
// internal/auth): TokenProvider needs the per-call ctx for token rotation
// and auth.BearerCredentials carries only a fixed string.
type tokenSource struct {
	static  string
	dynamic func(ctx context.Context) (string, error)
}

func (t tokenSource) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token := t.static
	if t.dynamic != nil {
		v, err := t.dynamic(ctx)
		if err != nil {
			return nil, err
		}
		token = v
	}
	// auth uses the lower-case header key per grpc-metadata convention.
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (t tokenSource) RequireTransportSecurity() bool { return false }
