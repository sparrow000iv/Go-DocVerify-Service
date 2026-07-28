// Command server runs the DocVerify service, exposing a REST API, a gRPC
// API and a Prometheus /metrics endpoint, with graceful shutdown on SIGTERM
// so Kubernetes rolling updates drain cleanly.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/sparrow000iv/go-docverify-service/internal/grpcapi"
	"github.com/sparrow000iv/go-docverify-service/internal/httpapi"
	"github.com/sparrow000iv/go-docverify-service/internal/store"
	pb "github.com/sparrow000iv/go-docverify-service/proto/docverify/v1"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	httpAddr := flag.String("http-addr", envOr("HTTP_ADDR", ":8080"), "HTTP listen address")
	grpcAddr := flag.String("grpc-addr", envOr("GRPC_ADDR", ":9090"), "gRPC listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	st := store.New()

	// --- HTTP server -------------------------------------------------------
	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           httpapi.New(st, logger).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// --- gRPC server -------------------------------------------------------
	grpcSrv := grpc.NewServer()
	pb.RegisterDocVerifyServer(grpcSrv, grpcapi.New(st))

	hs := health.NewServer()
	hs.SetServingStatus("docverify.v1.DocVerify", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, hs)
	reflection.Register(grpcSrv) // enables grpcurl against the running pod

	errCh := make(chan error, 2)

	go func() {
		logger.Info("http server starting", "addr", *httpAddr, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		lis, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			errCh <- err
			return
		}
		logger.Info("grpc server starting", "addr", *grpcAddr, "version", version)
		if err := grpcSrv.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	// --- graceful shutdown -------------------------------------------------
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case sig := <-stop:
		logger.Info("shutdown signal received", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Error("http shutdown error", "error", err)
	}

	done := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		grpcSrv.Stop()
	}

	logger.Info("shutdown complete")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
