// service exposes a small synchronous HTTP API for CPU point-cloud workflows.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pcw "github.com/ajavanma/pcw-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	output := flag.String("output", "work/service", "private directory retaining workflow artifacts")
	datasets := flag.String("datasets", "", "operator dataset registry JSON file (optional)")
	executable := flag.String("pcw", "pcw", "PCW executable path")
	python := flag.String("python", os.Getenv("PCW_PYTHON"), "Python interpreter with PCW installed (overrides -pcw)")
	maxConcurrent := flag.Int("max-concurrent", 1, "maximum simultaneous workflows (no queue)")
	timeout := flag.Duration("timeout", 5*time.Minute, "deadline for the entire workflow request")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	// Keep addition below from overflowing the HTTP write deadline.
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("timeout must be positive and at most 24h")
	}
	config := pcw.Config{Executable: *executable, MaxConcurrent: *maxConcurrent}
	if *python != "" {
		config.Executable = *python
		config.PrefixArgs = []string{"-m", "pointcloud_workflows"}
	}
	client, err := pcw.NewClient(config)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "pcw-service ", log.LstdFlags)
	registry, err := loadDatasets(*datasets)
	if err != nil {
		return err
	}
	handler, err := newService(pcwWorkflow(client), *output, *maxConcurrent, *timeout, logger, registry)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: *timeout + 15*time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 8 << 10, ErrorLog: logger,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	logger.Printf("listening on %s; artifacts: %s", listener.Addr(), handler.root)
	return serve(ctx, server, listener)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// BaseContext cancels active workers before Shutdown waits for handlers.
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		if err := <-finished; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
