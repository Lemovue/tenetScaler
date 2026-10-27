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
	"syscall"
	"time"

	"tenantscaler/internal/demo"
)

func init() {
	register("mirror", "serve teammates' traffic to two gateways: TenantScaler answers, the baseline gets a copy (side by side)", runMirror)
}

func runMirror(args []string) error {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	addr := fs.String("addr", "0.0.0.0:8081", "public listen address for teammates' laptops (tenantscaler loadgen -gateway)")
	primary := fs.String("primary", "http://127.0.0.1:18081", "TenantScaler gateway base URL: its answers go back to the client")
	copyTo := fs.String("copy", "http://127.0.0.1:18082", "baseline gateway base URL: gets a copy of every completion")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("mirror: listen on %s: %w", *addr, err)
	}
	srv := &http.Server{Handler: demo.NewMirror(ctx, *primary, *copyTo, logger), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	fmt.Printf("mirror on http://%s: TenantScaler %s answers, baseline %s gets a copy; Ctrl+C stops\n", ln.Addr(), *primary, *copyTo)
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutdown); serr != nil {
		err = errors.Join(err, serr)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
