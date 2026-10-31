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

	"sandbox-cloud/internal/runner"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address; use :8090 only inside a restricted Pod network")
	workdir := flag.String("workdir", "/workspace", "existing workspace directory")
	flag.Parse()
	r, err := runner.New(*workdir)
	if err != nil {
		return err
	}
	s := &http.Server{Addr: *listen, Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe() }()
	log.Printf("runner listening on %s, workspace %s", *listen, *workdir)
	select {
	case <-ctx.Done():
	case err := <-done:
		if err != http.ErrServerClosed {
			closeErr := r.Close()
			return fmt.Errorf("serve runner: %w (cleanup: %v)", err, closeErr)
		}
	}
	if err := r.Close(); err != nil {
		_ = s.Close()
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(shutdown); err != nil {
		return err
	}
	return nil
}
