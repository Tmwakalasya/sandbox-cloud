package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sandbox-cloud/internal/cloud"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", ":8080", "HTTP address inside the cluster")
	namespace := flag.String("namespace", "sandbox-workspaces", "namespace for workspace Pods")
	image := flag.String("workspace-image", "", "required immutable workspace image reference")
	flag.Parse()
	if *image == "" {
		return errors.New("--workspace-image is required")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cfg.Timeout = 15 * time.Second
	kube, err := typed.NewForConfig(cfg)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 15 * time.Second
	api := &cloud.Server{Kube: &cloud.Kubernetes{Client: kube, Namespace: *namespace, Image: *image}, Transport: transport}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go api.Reconcile(ctx)
	server := &http.Server{Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("control API on %s; workspace namespace %s", *listen, *namespace)
	select {
	case <-ctx.Done():
	case err := <-done:
		if err != http.ErrServerClosed {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
