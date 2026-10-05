// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package server runs core's gRPC server.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"time"

	grpcactor "github.com/Bugs5382/go-grpc-actor"
	log "github.com/Bugs5382/go-log"
	gootel "github.com/Bugs5382/go-otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// MaxMessageBytes is the message limit both ways. An editor image (up to
// 10 MiB) travels in one unary message, above gRPC's 4 MiB default.
const MaxMessageBytes = 16 << 20

// gracefulStopTimeout bounds the drain of in-flight RPCs on shutdown, so a
// hung peer can't hold the process.
const gracefulStopTimeout = 10 * time.Second

// ReadinessService is the grpc.health.v1 service name readiness probes ask
// for. The empty name is liveness: it reports the process only.
const ReadinessService = "readiness"

// Check reports whether one required dependency is usable. It must be cheap:
// it runs every CheckInterval.
type Check func(context.Context) error

// Options are the transport and probe settings. Zero serves plain gRPC,
// trusts no forwarded actor and is always ready.
type Options struct {
	CertFile, KeyFile, ClientCAFile string
	// TrustedCallers are the SPIFFE IDs whose forwarded actor is believed.
	TrustedCallers []string
	// Readiness holds the checks of the required dependencies.
	Readiness     []Check
	CheckInterval time.Duration
}

// Serve runs a gRPC server on lis with go-otel tracing, panic recovery,
// go-grpc-actor, grpc.health.v1 and reflection, plus the services register
// adds. It returns nil once ctx is cancelled and the server has stopped.
func Serve(ctx context.Context, lis net.Listener, lg log.Logger, opts Options, register func(*grpc.Server)) error {
	actorOpts := []grpcactor.ServerOption{}
	if len(opts.TrustedCallers) > 0 {
		actorOpts = append(actorOpts, grpcactor.WithTrust(grpcactor.TrustSPIFFEIDs(opts.TrustedCallers...)))
	}
	serverOpts := []grpc.ServerOption{
		grpc.StatsHandler(gootel.GRPCServerStatsHandler()),
		grpc.ChainUnaryInterceptor(recoverUnary(lg), grpcactor.UnaryServerInterceptor(actorOpts...)),
		grpc.ChainStreamInterceptor(recoverStream(lg), grpcactor.StreamServerInterceptor(actorOpts...)),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
	}
	if opts.CertFile != "" {
		creds, err := mtls(opts)
		if err != nil {
			return err
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
	}
	s := grpc.NewServer(serverOpts...)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s, hs)
	go watchReadiness(ctx, hs, opts)
	reflection.Register(s)
	register(s)

	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(lis) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() {
			s.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(gracefulStopTimeout):
			s.Stop()
		}
		<-errCh
		return nil
	}
}

func watchReadiness(ctx context.Context, hs *health.Server, opts Options) {
	interval := opts.CheckInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		st := healthpb.HealthCheckResponse_SERVING
		for _, check := range opts.Readiness {
			cctx, cancel := context.WithTimeout(ctx, interval)
			err := check(cctx)
			cancel()
			if err != nil {
				st = healthpb.HealthCheckResponse_NOT_SERVING
				break
			}
		}
		hs.SetServingStatus(ReadinessService, st)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func mtls(opts Options) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server: load certificate: %w", err)
	}
	pem, err := os.ReadFile(opts.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("server: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("server: client CA file holds no certificate")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	}), nil
}

// The panic value and stack go to the log only; the caller gets a bare
// Internal.
func recoverUnary(lg log.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				resp, err = nil, recovered(ctx, lg, r, info.FullMethod)
			}
		}()
		return handler(ctx, req)
	}
}

func recoverStream(lg log.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recovered(ss.Context(), lg, r, info.FullMethod)
			}
		}()
		return handler(srv, ss)
	}
}

func recovered(ctx context.Context, lg log.Logger, r any, method string) error {
	lg.Ctx(ctx).Error(nil, "recovered from a panic in a gRPC handler",
		log.F("method", method), log.F("panic", r), log.F("stack", string(debug.Stack())))
	return status.Error(codes.Internal, "internal error")
}
