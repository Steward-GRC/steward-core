// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	grpcactor "github.com/Bugs5382/go-grpc-actor"
	log "github.com/Bugs5382/go-log"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/workloadauth"
)

const testNS = "steward"

// localIssuer is an OIDC issuer on a local TLS test server: discovery and a
// JWKS with one P-256 key generated in the test.
type localIssuer struct {
	url, caFile string
	key         *ecdsa.PrivateKey
}

func newLocalIssuer(t *testing.T) *localIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	iss := &localIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss.url, "jwks_uri": iss.url + "/openid/v1/jwks"})
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub, err := key.PublicKey.ECDH()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		raw := pub.Bytes()
		b64 := base64.RawURLEncoding.EncodeToString
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "kid": "k1", "use": "sig", "crv": "P-256", "x": b64(raw[1:33]), "y": b64(raw[33:]),
		}}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	iss.url = srv.URL
	iss.caFile = filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(iss.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	return iss
}

func (i *localIssuer) token(t *testing.T, sa, audience string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": i.url, "aud": []string{audience}, "sub": "system:serviceaccount:" + testNS + ":" + sa,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	require.NoError(t, err)
	return s
}

var getSettings = corev1.SettingsService_GetGlobalSettings_FullMethodName

// authServe serves the probe with workload auth on: steward-gateway,
// steward-delivery and steward-reporting hold valid identities, but the
// policy lists only the gateway (on behalf) and delivery (as itself).
func authServe(t *testing.T) (*localIssuer, *grpc.ClientConn, chan bool, func()) {
	t.Helper()
	iss := newLocalIssuer(t)
	v, err := workloadauth.NewVerifier(workloadauth.Config{
		Issuer: iss.url, CAFile: iss.caFile, Audience: "steward",
		AllowedServiceAccounts: []string{testNS + "/steward-gateway", testNS + "/steward-delivery", testNS + "/steward-reporting"},
	}, log.Nop())
	require.NoError(t, err)
	require.NoError(t, v.Refresh(context.Background()))
	conn, saw, stop := serve(t, Options{Auth: &Auth{
		Verifier: v,
		Policy:   workloadauth.Policy{getSettings: {"gateway": workloadauth.OnBehalf, "delivery": workloadauth.Self}},
	}})
	return iss, conn, saw, stop
}

func bearerCtx(tok string) context.Context {
	ctx := grpcactor.WithActor(context.Background(), grpcactor.Actor{Subject: "erin", Impersonator: "alice"})
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func callSettings(conn *grpc.ClientConn, ctx context.Context) codes.Code {
	_, err := corev1.NewSettingsServiceClient(conn).GetGlobalSettings(ctx, &corev1.GetGlobalSettingsRequest{})
	return status.Code(err)
}

func TestWorkloadAuthLetsAnOnBehalfCallerForwardTheActor(t *testing.T) {
	iss, conn, saw, stop := authServe(t)
	defer stop()
	require.Equal(t, codes.OK, callSettings(conn, bearerCtx(iss.token(t, "steward-gateway", "steward"))))
	require.True(t, <-saw, "the gateway's forwarded actor is believed")
}

func TestWorkloadAuthLetsASelfCallerActOnlyAsItself(t *testing.T) {
	iss, conn, saw, stop := authServe(t)
	defer stop()
	require.Equal(t, codes.OK, callSettings(conn, bearerCtx(iss.token(t, "steward-delivery", "steward"))))
	require.False(t, <-saw, "delivery's forwarded actor is dropped")
}

func TestWorkloadAuthRefusesAValidTokenFromAnUnlistedCaller(t *testing.T) {
	iss, conn, _, stop := authServe(t)
	defer stop()
	require.Equal(t, codes.PermissionDenied, callSettings(conn, bearerCtx(iss.token(t, "steward-reporting", "steward"))),
		"a verified identity the method doesn't list")
	require.Equal(t, codes.Unauthenticated, callSettings(conn, bearerCtx(iss.token(t, "steward-ai", "steward"))),
		"a valid token from a service account outside WORKLOAD_ALLOWED_SERVICEACCOUNTS")
}

func TestWorkloadAuthRefusesAMissingOrInvalidToken(t *testing.T) {
	iss, conn, _, stop := authServe(t)
	defer stop()
	require.Equal(t, codes.Unauthenticated, callSettings(conn, context.Background()), "no token")
	require.Equal(t, codes.Unauthenticated, callSettings(conn, bearerCtx("not-a-jwt")), "a malformed token")
	require.Equal(t, codes.Unauthenticated, callSettings(conn, bearerCtx(iss.token(t, "steward-gateway", "other"))), "another audience")
	other := newLocalIssuer(t)
	require.Equal(t, codes.Unauthenticated, callSettings(conn, bearerCtx(other.token(t, "steward-gateway", "steward"))), "another issuer")
}

func TestWorkloadAuthLeavesHealthAndReflectionOpen(t *testing.T) {
	_, conn, _, stop := authServe(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, hc.GetStatus())
	rs, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	require.NoError(t, err)
	require.NoError(t, rs.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{}}))
	res, err := rs.Recv()
	require.NoError(t, err)
	require.NotEmpty(t, res.GetListServicesResponse().GetService())
}

func TestWorkloadAuthDisabledLetsCallsThrough(t *testing.T) {
	conn, saw, stop := serve(t, Options{})
	defer stop()
	require.Equal(t, codes.OK, callSettings(conn, context.Background()), "WORKLOAD_AUTH=disabled serves a call with no token")
	require.False(t, <-saw, "and still believes no forwarded actor")
}

func TestTrustOnBehalfNeedsAnOnBehalfGrant(t *testing.T) {
	grant := func(a workloadauth.Access) context.Context {
		return workloadauth.ContextWithGrant(context.Background(), workloadauth.Grant{Caller: workloadauth.Caller{Name: "x"}, Access: a})
	}
	require.True(t, TrustOnBehalf(grant(workloadauth.OnBehalf), "/m"))
	require.False(t, TrustOnBehalf(grant(workloadauth.Self), "/m"))
	require.False(t, TrustOnBehalf(context.Background(), "/m"), "no verified caller, no trust")
}
