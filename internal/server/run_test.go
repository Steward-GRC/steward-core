// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	grpcactor "github.com/Bugs5382/go-grpc-actor"
	log "github.com/Bugs5382/go-log"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
)

type probe struct {
	corev1.UnimplementedSettingsServiceServer
	sawActor chan bool
}

func (p probe) GetGlobalSettings(ctx context.Context, _ *corev1.GetGlobalSettingsRequest) (*corev1.GetGlobalSettingsResponse, error) {
	_, ok := grpcactor.FromContext(ctx)
	p.sawActor <- ok
	return &corev1.GetGlobalSettingsResponse{}, nil
}

func (probe) SetGlobalSettings(context.Context, *corev1.SetGlobalSettingsRequest) (*corev1.SetGlobalSettingsResponse, error) {
	panic("boom: secret detail")
}

func serve(t *testing.T, opts Options) (*grpc.ClientConn, chan bool, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	saw := make(chan bool, 1)
	go func() {
		done <- Serve(ctx, lis, log.Nop(), opts, func(s *grpc.Server) { corev1.RegisterSettingsServiceServer(s, probe{sawActor: saw}) })
	}()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(grpcactor.UnaryClientInterceptor()))
	require.NoError(t, err)
	return conn, saw, func() {
		_ = conn.Close()
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "Serve returns nil after shutdown")
		case <-time.After(15 * time.Second):
			t.Fatal("Serve did not stop")
		}
	}
}

func TestServeServesHealthRecoversPanicsAndStops(t *testing.T) {
	conn, _, stop := serve(t, Options{})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hc, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, hc.GetStatus())

	_, err = corev1.NewSettingsServiceClient(conn).SetGlobalSettings(ctx, &corev1.SetGlobalSettingsRequest{})
	st := status.Convert(err)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "internal error", st.Message(), "a panic surfaces as a bare Internal")
}

func TestServeDropsAnActorFromAnUntrustedCaller(t *testing.T) {
	conn, saw, stop := serve(t, Options{})
	defer stop()
	ctx := grpcactor.WithActor(context.Background(), grpcactor.Actor{Subject: "erin", Impersonator: "alice"})
	_, err := corev1.NewSettingsServiceClient(conn).GetGlobalSettings(ctx, &corev1.GetGlobalSettingsRequest{})
	require.NoError(t, err)
	require.False(t, <-saw, "without a trusted, verified caller the actor is ignored")
}

func TestServeAcceptsAnEditorImageAboveTheDefaultLimit(t *testing.T) {
	require.GreaterOrEqual(t, MaxMessageBytes, 10<<20+1<<20, "a 10 MiB image plus framing fits one message")
}

func TestReadinessFollowsTheChecksAndLivenessDoesNot(t *testing.T) {
	var up atomic.Bool
	conn, _, stop := serve(t, Options{
		Readiness: []Check{func(context.Context) error {
			if up.Load() {
				return nil
			}
			return errors.New("postgres down")
		}},
		CheckInterval: 20 * time.Millisecond,
	})
	defer stop()
	hc := healthpb.NewHealthClient(conn)
	status := func(svc string) healthpb.HealthCheckResponse_ServingStatus {
		r, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{Service: svc})
		require.NoError(t, err)
		return r.GetStatus()
	}
	require.Eventually(t, func() bool { return status(ReadinessService) == healthpb.HealthCheckResponse_NOT_SERVING }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, status(""), "liveness never follows a dependency")
	up.Store(true)
	require.Eventually(t, func() bool { return status(ReadinessService) == healthpb.HealthCheckResponse_SERVING }, 2*time.Second, 10*time.Millisecond)
}
