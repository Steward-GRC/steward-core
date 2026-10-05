// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package readiness registers core's dependencies with go-buildinfo's health
// checker. Postgres and RabbitMQ are required: without them core can neither
// read nor write a policy, nor audit the write. Valkey and object storage are
// optional: the read cache falls back to Postgres, and only editor images need
// the object store, so an outage there degrades core instead of draining it.
package readiness

import (
	"context"
	"errors"
	"strings"

	"github.com/Bugs5382/go-buildinfo/health"
	objectstore "github.com/Bugs5382/go-objectstore"
	postgres "github.com/Bugs5382/go-postgres"
)

// Dependency names, as they appear in the report and the
// steward-depstate-<name> headers.
const (
	Postgres    = "postgres"
	RabbitMQ    = "rabbitmq"
	Valkey      = "valkey"
	ObjectStore = "objectstore"
)

// Database is the Postgres the service runs on.
type Database interface {
	Ping(ctx context.Context) error
	ServerVersion(ctx context.Context) (string, error)
}

// Broker is the RabbitMQ connection; go-rabbitmq's Conn reports it.
type Broker interface{ Healthy() bool }

// Deps are the dependencies to report. A nil Cache or Objects is a feature
// that is off, and is not reported.
type Deps struct {
	Postgres Database
	Broker   Broker
	Cache    func(ctx context.Context) error
	Objects  objectstore.Store
}

var errBrokerDown = errors.New("rabbitmq connection is down")

// New returns a checker with deps registered.
func New(d Deps, opts ...health.Option) (*health.Checker, error) {
	deps := []health.Dependency{
		{Name: Postgres, Required: true, Check: d.Postgres.Ping, Version: d.Postgres.ServerVersion},
		{Name: RabbitMQ, Required: true, Check: func(context.Context) error {
			if !d.Broker.Healthy() {
				return errBrokerDown
			}
			return nil
		}},
	}
	if d.Cache != nil {
		deps = append(deps, health.Dependency{Name: Valkey, Check: d.Cache})
	}
	if d.Objects != nil {
		deps = append(deps, health.Dependency{Name: ObjectStore, Check: func(ctx context.Context) error {
			_, err := d.Objects.List(ctx, objectstore.ListOptions{Limit: 1})
			return err
		}})
	}
	c := health.New(opts...)
	return c, c.Register(deps...)
}

// PostgresDB adapts go-postgres's DB.
func PostgresDB(db *postgres.DB) Database { return pgDB{db} }

type pgDB struct{ db *postgres.DB }

func (p pgDB) Ping(ctx context.Context) error { return p.db.Ping(ctx) }

// ServerVersion drops the build suffix ("16.4 (Debian 16.4-1)"), which the
// header would redact.
func (p pgDB) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := p.db.Pool().QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", err
	}
	if f := strings.Fields(v); len(f) > 0 {
		return f[0], nil
	}
	return v, nil
}
