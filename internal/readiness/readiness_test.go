// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package readiness_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bugs5382/go-buildinfo/health"
	objectstore "github.com/Bugs5382/go-objectstore"
	"github.com/Bugs5382/go-objectstore/memstore"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/readiness"
)

type fakeDB struct{ down atomic.Bool }

func (f *fakeDB) Ping(context.Context) error {
	if f.down.Load() {
		return errors.New("connection refused")
	}
	return nil
}

func (*fakeDB) ServerVersion(context.Context) (string, error) { return "16.4", nil }

type fakeBroker struct{ down atomic.Bool }

func (f *fakeBroker) Healthy() bool { return !f.down.Load() }

type failingStore struct {
	objectstore.Store
	down atomic.Bool
}

func (s *failingStore) List(ctx context.Context, o objectstore.ListOptions) (objectstore.Page, error) {
	if s.down.Load() {
		return objectstore.Page{}, errors.New("connection refused")
	}
	return s.Store.List(ctx, o)
}

func checker(t *testing.T, d readiness.Deps) *health.Checker {
	t.Helper()
	c, err := readiness.New(d, health.WithTTL(time.Millisecond), health.WithTimeout(time.Second))
	require.NoError(t, err)
	return c
}

func dep(t *testing.T, r health.Report, name string) health.DependencyReport {
	t.Helper()
	for _, d := range r.Dependencies {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no %q in the report", name)
	return health.DependencyReport{}
}

func names(r health.Report) []string {
	var out []string
	for _, d := range r.Dependencies {
		out = append(out, d.Name)
	}
	return out
}

func TestRequiredOnlyWhenTheOptionalOnesAreOff(t *testing.T) {
	r := checker(t, readiness.Deps{Postgres: &fakeDB{}, Broker: &fakeBroker{}}).Report(context.Background())
	require.True(t, r.Ready)
	require.Equal(t, health.StateOK, r.Status)
	require.ElementsMatch(t, []string{readiness.Postgres, readiness.RabbitMQ}, names(r))
	require.True(t, dep(t, r, readiness.Postgres).Required)
	require.True(t, dep(t, r, readiness.RabbitMQ).Required)
	require.Equal(t, "16.4", dep(t, r, readiness.Postgres).Version)
}

func TestPostgresDownMakesCoreNotReady(t *testing.T) {
	db := &fakeDB{}
	c := checker(t, readiness.Deps{Postgres: db, Broker: &fakeBroker{}})
	db.down.Store(true)
	require.Eventually(t, func() bool { return !c.Report(context.Background()).Ready }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, health.StateDown, dep(t, c.Report(context.Background()), readiness.Postgres).State)
	db.down.Store(false)
	require.Eventually(t, func() bool { return c.Report(context.Background()).Ready }, 2*time.Second, 5*time.Millisecond)
}

func TestRabbitMQDownMakesCoreNotReady(t *testing.T) {
	b := &fakeBroker{}
	c := checker(t, readiness.Deps{Postgres: &fakeDB{}, Broker: b})
	b.down.Store(true)
	require.Eventually(t, func() bool { return !c.Report(context.Background()).Ready }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, health.StateDown, dep(t, c.Report(context.Background()), readiness.RabbitMQ).State)
}

func TestValkeyDownDegradesButStaysReady(t *testing.T) {
	var down atomic.Bool
	c := checker(t, readiness.Deps{Postgres: &fakeDB{}, Broker: &fakeBroker{}, Cache: func(context.Context) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}})
	require.Equal(t, health.StateOK, c.Report(context.Background()).Status)
	down.Store(true)
	require.Eventually(t, func() bool { return c.Report(context.Background()).Status == health.StateDegraded }, 2*time.Second, 5*time.Millisecond)
	r := c.Report(context.Background())
	require.True(t, r.Ready, "the read cache falls back to Postgres")
	require.False(t, dep(t, r, readiness.Valkey).Required)
}

func TestObjectStoreDownDegradesButStaysReady(t *testing.T) {
	s := &failingStore{Store: memstore.New()}
	c := checker(t, readiness.Deps{Postgres: &fakeDB{}, Broker: &fakeBroker{}, Objects: s})
	require.Equal(t, health.StateOK, c.Report(context.Background()).Status)
	s.down.Store(true)
	require.Eventually(t, func() bool { return c.Report(context.Background()).Status == health.StateDegraded }, 2*time.Second, 5*time.Millisecond)
	r := c.Report(context.Background())
	require.True(t, r.Ready, "only editor images need object storage")
	require.Equal(t, health.StateDegraded, dep(t, r, readiness.ObjectStore).State, "an optional dependency that is down reports degraded")
	require.False(t, dep(t, r, readiness.ObjectStore).Required)
}
