package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tenantscaler/internal/envfile"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/k8s"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
)

const (
	// gatewayPoolPoll is how often a local pool's ready endpoints are read.
	gatewayPoolPoll = 500 * time.Millisecond
	// gatewayK8sPoll is how often the pool Service's EndpointSlices are read.
	gatewayK8sPoll = 2 * time.Second
	// gatewayTenantPoll is how often TenantScaler resources are re-read.
	gatewayTenantPoll = 30 * time.Second
	// gatewayShutdownGrace bounds the wait for active requests on shutdown.
	gatewayShutdownGrace = 15 * time.Second
)

func init() {
	register("gateway", "run the tenant-fair gateway (the fast loop) in front of the replica pool", runGateway)
}

// gatewayFlags are the parsed flags of the gateway subcommand.
type gatewayFlags struct {
	addr, cfgPath, mode, poolURL, endpoints string
	k8sService, namespace, kubeconfig       string
	k8sPort, slots                          int
	endpointsSet                            bool
}

func parseGatewayFlags(args []string) (gatewayFlags, error) {
	var f gatewayFlags
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.StringVar(&f.addr, "addr", "127.0.0.1:8080", "listen address")
	fs.StringVar(&f.cfgPath, "config", "", "YAML config: tenants and SLOs, pool name, dispatch window (empty = built-in default)")
	fs.StringVar(&f.mode, "mode", gateway.ModeFair, "fair | roundrobin | passthrough | partition")
	fs.StringVar(&f.poolURL, "pool-url", "", "local pool manager URL; its ready replicas are polled from GET /endpoints")
	fs.StringVar(&f.endpoints, "endpoints", envfile.Get("TS_UPSTREAM_ENDPOINTS", ""),
		"comma-separated upstream base URLs of any OpenAI-compatible servers, with or without a trailing /v1 (default $TS_UPSTREAM_ENDPOINTS)")
	fs.StringVar(&f.k8sService, "k8s-service", "",
		"Kubernetes mode: dispatch to the ready pods behind this Service (EndpointSlices); tenants come from TenantScaler resources")
	fs.IntVar(&f.k8sPort, "k8s-port", 8000, "replica container port used with -k8s-service")
	fs.StringVar(&f.namespace, "namespace", envfile.Get("TS_NAMESPACE", ""), "Kubernetes namespace (default $TS_NAMESPACE; empty = the pod's own namespace in-cluster)")
	fs.StringVar(&f.kubeconfig, "kubeconfig", envfile.Get("TS_KUBECONFIG", ""), "kubeconfig path (default $TS_KUBECONFIG; empty = in-cluster config)")
	fs.IntVar(&f.slots, "slots", 0, "dispatch window per replica (0 = gateway.slotsPerReplica from the config, else the backend's concurrent sequences)")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	fs.Visit(func(fl *flag.Flag) { f.endpointsSet = f.endpointsSet || fl.Name == "endpoints" })
	if f.slots < 0 {
		return f, fmt.Errorf("-slots must be >= 0, got %d", f.slots)
	}
	return f, nil
}

func runGateway(args []string) error {
	f, err := parseGatewayFlags(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(f.cfgPath)
	if err != nil {
		return err
	}
	mode, err := gateway.ParseMode(f.mode)
	if err != nil {
		return err
	}
	slots := f.slots
	if slots == 0 {
		slots = cfg.SlotsPerReplica()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// bg runs discovery and tenant refresh until the gateway has stopped.
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()

	disc, kc, upstream, err := gatewayDiscovery(bg, f, log)
	if err != nil {
		return err
	}
	tenants := cfg.Tenants
	if kc != nil {
		tenants = crTenants(ctx, kc, cfg.Pool.Name, tenants, log)
	}
	keys, err := gateway.ParseTenantKeys(envfile.Get("TS_TENANT_KEYS", ""))
	if err != nil {
		return fmt.Errorf("$TS_TENANT_KEYS: %w", err)
	}
	g, err := gateway.New(gateway.Config{
		Mode: mode, Tenants: tenants, SlotsPerReplica: slots, Discovery: disc,
		UpstreamAPIKey: envfile.Get("TS_UPSTREAM_API_KEY", ""), Seed: 1, Logger: log,
		TenantKeys: keys, AdminKey: envfile.Get("TS_ADMIN_KEY", ""),
	})
	if err != nil {
		return err
	}
	log.Info("gateway access", "tenantKeys", len(keys), "adminKey", envfile.Get("TS_ADMIN_KEY", "") != "")
	defer g.Close()
	if kc != nil {
		list := func(ctx context.Context) ([]model.TenantSpec, error) { return listCRTenants(ctx, kc, cfg.Pool.Name) }
		go g.FollowTenants(bg, list, gatewayTenantPoll)
	}

	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.addr, err)
	}
	// No write timeout: completions stream for as long as generation runs.
	srv := &http.Server{Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("gateway (%s mode) on http://%s: %d tenants, dispatch window %d per replica, upstream %s\n",
		mode, ln.Addr(), len(tenants), slots, upstream)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var serveErr error
	select {
	case err := <-errc:
		serveErr = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		stop() // a second signal terminates immediately
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), gatewayShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		serveErr = errors.Join(serveErr, fmt.Errorf("shutdown: %w", err))
	}
	return serveErr
}

// gatewayDiscovery builds the endpoint discovery from the flags: Kubernetes
// EndpointSlices, a local pool manager, or a static list (one of them). It
// returns the Kubernetes client in Kubernetes mode and a description of the
// upstream for the startup banner.
func gatewayDiscovery(ctx context.Context, f gatewayFlags, log *slog.Logger) (gateway.Discovery, *k8s.Client, string, error) {
	static := strings.TrimSpace(f.endpoints) != ""
	switch {
	case f.k8sService != "":
		if f.poolURL != "" || f.endpointsSet {
			return nil, nil, "", errors.New("-k8s-service cannot be combined with -pool-url or -endpoints")
		}
		kc, err := k8s.NewClient(f.kubeconfig, f.namespace)
		if err != nil {
			return nil, nil, "", err
		}
		es := k8s.NewEndpointSliceDiscovery(kc, f.k8sService, f.k8sPort)
		pd := gateway.NewPollingDiscovery(func(ctx context.Context) ([]string, error) {
			if err := es.Refresh(ctx); err != nil {
				return nil, err
			}
			return es.Endpoints(), nil
		}, gatewayK8sPoll)
		pd.Logger = log
		pd.Start(ctx)
		return pd, kc, fmt.Sprintf("Service %s/%s port %d", kc.Namespace, f.k8sService, f.k8sPort), nil
	case f.poolURL != "":
		if f.endpointsSet {
			return nil, nil, "", errors.New("-pool-url cannot be combined with -endpoints")
		}
		pd := gateway.NewPollingDiscovery(pool.Client{BaseURL: f.poolURL}.Endpoints, gatewayPoolPoll)
		pd.Logger = log
		pd.Start(ctx)
		return pd, nil, "pool " + f.poolURL, nil
	case static:
		sd := gateway.StaticDiscovery(strings.Split(f.endpoints, ","))
		return sd, nil, strings.Join(sd.Endpoints(), ", "), nil
	}
	return nil, nil, "", errors.New("no upstream: pass -pool-url, -endpoints (or set $TS_UPSTREAM_ENDPOINTS) or -k8s-service")
}

// crTenants returns the tenants of the TenantScaler resources of the pool,
// or fallback (the -config tenants) when they cannot be listed (e.g. the
// gateway's Role does not grant it) or there are none.
func crTenants(ctx context.Context, kc *k8s.Client, pool string, fallback []model.TenantSpec, log *slog.Logger) []model.TenantSpec {
	specs, err := listCRTenants(ctx, kc, pool)
	switch {
	case err != nil:
		log.Warn("cannot list TenantScaler resources; using the -config tenants", "err", err)
		return fallback
	case len(specs) == 0:
		log.Warn("no TenantScaler resources for this pool; using the -config tenants", "pool", pool)
		return fallback
	}
	return specs
}

func listCRTenants(ctx context.Context, kc *k8s.Client, pool string) ([]model.TenantSpec, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	crs, err := kc.ListTenants(ctx, pool)
	if err != nil {
		return nil, err
	}
	specs := make([]model.TenantSpec, len(crs))
	for i, cr := range crs {
		specs[i] = cr.TenantSpec()
	}
	return specs, nil
}
