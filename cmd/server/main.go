// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Command server runs the core service: categories, templates, policies and
// procedures with their versions, the content libraries, settings and editor
// images, over gRPC. It calls no other service; it publishes lifecycle events
// and steward-audit's AuditEvent.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	buildinfo "github.com/Bugs5382/go-buildinfo"
	"github.com/Bugs5382/go-buildinfo/health"
	log "github.com/Bugs5382/go-log"
	objectstore "github.com/Bugs5382/go-objectstore"
	"github.com/Bugs5382/go-objectstore/s3store"
	gootel "github.com/Bugs5382/go-otel"
	postgres "github.com/Bugs5382/go-postgres"
	pgotel "github.com/Bugs5382/go-postgres/otel"
	"github.com/Bugs5382/go-rabbitmq"
	rmqotel "github.com/Bugs5382/go-rabbitmq/otel"
	redis "github.com/Bugs5382/go-redis"
	"google.golang.org/grpc"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/cache"
	"github.com/Steward-GRC/steward-core/internal/config"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
	"github.com/Steward-GRC/steward-core/internal/readiness"
	"github.com/Steward-GRC/steward-core/internal/server"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/Steward-GRC/steward-core/internal/workloadauth"
)

const serviceName = "core"

// jwksRecheck is how long a good JWKS fetch keeps readiness up before the
// next probe fetches again.
const jwksRecheck = time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := log.NewLogger(serviceName)
	if err := run(ctx, logger); err != nil {
		logger.Fatal(err, "core service stopped")
	}
}

func run(ctx context.Context, logger log.Logger) error {
	bi := buildinfo.Get()
	logger.Info("starting", log.F("version", bi.Version), log.F("commit", bi.Commit), log.F("go_version", bi.GoVersion))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	otelShutdown, err := gootel.Init(ctx, serviceName, cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("otel: %w", err)
	}
	defer func() {
		if err := otelShutdown(context.Background()); err != nil {
			logger.Warn("otel shutdown", log.F("error", err.Error()))
		}
	}()

	if err := pgotel.InstrumentMigrate(ctx, serviceName, func() error {
		return postgres.Migrate(cfg.MigrateDSN, cfg.MigrationsDir)
	}); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	db, err := postgres.New(ctx, cfg.DatabaseDSN, pgotel.WithTracing())
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer db.Close()

	conn, err := rabbitmq.Connect(ctx, cfg.RabbitURL, append(rmqotel.Instrument(), rabbitmq.WithLogger(rabbitLogger{logger}))...)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	defer func() { _ = conn.Close() }()
	auditPub := conn.NewPublisher(audit.Exchange,
		rabbitmq.WithExchangeDeclare(rabbitmq.ExchangeConfig{Name: audit.Exchange, Kind: "topic", Durable: true}),
		rabbitmq.WithDefaultContentType(audit.ContentType))
	auditor := grpcsvc.NewImpersonationEmitter(audit.New(publisher{auditPub}))
	// Lifecycle events go to their own exchange so an audit consumer can never
	// swallow a worker's events, and the reindex path goes straight to the AI
	// indexer's queue through the default exchange, so a reindex doesn't fan
	// out to the other "jobs" consumers.
	jobsPub := conn.NewPublisher("jobs", rabbitmq.WithExchangeDeclare(rabbitmq.ExchangeConfig{Name: "jobs", Kind: "topic", Durable: true}))
	lifecycleEmitter := lifecycle.New(publisher{jobsPub}).WithAIPublisher(publisher{conn.NewPublisher("")})

	box, err := store.NewSecretBox(cfg.SettingsKey)
	if err != nil {
		return err
	}
	categories := store.NewCategoryStore(db)
	templates := store.NewTemplateStore(db)
	policies := store.NewPolicyStore(db)
	appendices := store.NewAppendixStore(db)
	emailService := store.NewEmailServiceStore(db, box)

	policyH := grpcsvc.NewPolicyHandler(policies, categories, templates, domain.NoopValidator{}, auditor).
		WithLifecycleEmitter(lifecycleEmitter).WithAppendixCopier(appendices)
	settingsH := grpcsvc.NewSettingsHandler(store.NewSettingsStore(db), auditor).WithEmailServiceStore(emailService)
	deps := readiness.Deps{Postgres: readiness.PostgresDB(db), Broker: conn}
	if cfg.RedisAddr != "" {
		rc, err := redis.Connect(ctx, redis.WithAddr(cfg.RedisAddr), redis.WithPassword(cfg.RedisPassword),
			redis.WithTimeouts(300*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond))
		if err != nil {
			logger.Warn("redis unreachable: the read cache is off", log.F("error", err.Error()))
			// The cache is only wired at boot, so it stays off until a restart;
			// readiness reports it degraded for that long.
			bootErr := err
			deps.Cache = func(context.Context) error { return bootErr }
		} else {
			deps.Cache = func(ctx context.Context) error { return rc.Redis().Ping(ctx).Err() }
			defer func() { _ = rc.Close() }()
			c := cache.New(rc, cfg.CacheTTL)
			policyH = policyH.WithVersionCache(c)
			settingsH = settingsH.WithCache(c)
			logger.Info("read cache on", log.F("ttl", cfg.CacheTTL.String()))
		}
	}

	var objects objectstore.Store
	if cfg.S3.Endpoint != "" {
		s3, err := s3store.New(s3store.Config{
			Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket,
			AccessKeyID: cfg.S3.AccessKey, SecretAccessKey: cfg.S3.SecretKey, PathStyle: cfg.S3.PathStyle,
			MaxObjectSize: domain.MaxAssetBytes,
		})
		if err != nil {
			return fmt.Errorf("object store: %w", err)
		}
		if err := s3.EnsureBucket(ctx); err != nil {
			return fmt.Errorf("object store: %w", err)
		}
		objects = s3
		deps.Objects = s3
		logger.Info("editor images on", log.F("bucket", cfg.S3.Bucket))
	} else {
		logger.Info("S3_ENDPOINT is not set: editor images are off")
	}
	assetH := grpcsvc.NewAssetHandler(store.NewAssetStore(db), nil, auditor)
	if objects != nil {
		assetH = grpcsvc.NewAssetHandler(store.NewAssetStore(db), objects, auditor)
	}
	var auth *server.Auth
	if cfg.WorkloadAuthEnabled {
		v, err := workloadauth.NewVerifier(cfg.WorkloadAuth, logger)
		if err != nil {
			return fmt.Errorf("workload auth: %w", err)
		}
		go v.Run(ctx)
		deps.JWKS = readiness.RecheckEvery(v.Refresh, jwksRecheck, time.Now)
		auth = &server.Auth{Verifier: v, Policy: grpcsvc.CallerPolicy(), Options: []workloadauth.Option{
			workloadauth.WithDenyHook(grpcsvc.AuditDenial(audit.New(publisher{auditPub}), logger)),
		}}
		logger.Info("service-to-service authentication on",
			log.F("issuer", cfg.WorkloadAuth.Issuer), log.F("audience", cfg.WorkloadAuth.Audience),
			log.F("jwks_override", cfg.WorkloadAuth.JWKSURL != ""), log.F("ca_file", cfg.WorkloadAuth.CAFile != ""),
			log.F("bearer_file", cfg.WorkloadAuth.BearerFile != ""),
			log.F("allowed_serviceaccounts", strings.Join(cfg.WorkloadAuth.AllowedServiceAccounts, ",")))
	} else {
		deps.WorkloadAuthDisabled = true
		go workloadauth.WarnDisabled(ctx, logger, workloadauth.DisabledWarnInterval)
	}

	checker, err := readiness.New(deps, health.WithTTL(5*time.Second), health.WithTimeout(2*time.Second), health.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("readiness: %w", err)
	}

	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", ":"+cfg.GRPCPort)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	probeLis, err := lc.Listen(ctx, "tcp", ":"+cfg.ProbePort)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info("serving", log.F("port", cfg.GRPCPort), log.F("probe_port", cfg.ProbePort))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	probesDone := make(chan error, 1)
	go func() {
		probesDone <- server.ServeProbes(ctx, probeLis, checker)
		cancel()
	}()
	opts := server.Options{
		CertFile: cfg.TLS.CertFile, KeyFile: cfg.TLS.KeyFile, ClientCAFile: cfg.TLS.ClientCAFile, Auth: auth,
		Checker: checker,
	}
	err = server.Serve(ctx, lis, logger, opts, func(s *grpc.Server) {
		corev1.RegisterCategoryServiceServer(s, grpcsvc.NewCategoryHandler(categories, auditor).WithObligationEmitter(lifecycleEmitter, policies))
		corev1.RegisterTemplateServiceServer(s, grpcsvc.NewTemplateHandler(templates, auditor))
		corev1.RegisterPolicyServiceServer(s, policyH)
		corev1.RegisterSettingsServiceServer(s, settingsH)
		corev1.RegisterAppendixServiceServer(s, grpcsvc.NewAppendixHandler(appendices))
		corev1.RegisterRelationServiceServer(s, grpcsvc.NewRelationHandler(store.NewRelationStore(db)))
		corev1.RegisterContactServiceServer(s, grpcsvc.NewContactHandler(store.NewContactStore(db)))
		corev1.RegisterReferenceServiceServer(s, grpcsvc.NewReferenceHandler(store.NewReferenceStore(db), auditor))
		corev1.RegisterDefinitionLibraryServiceServer(s, grpcsvc.NewDefinitionLibraryHandler(store.NewDefinitionLibraryStore(db), auditor))
		// Internal only: the gateway never serves this one, so the key can't
		// reach a browser.
		corev1.RegisterEmailServiceSecretServiceServer(s, grpcsvc.NewEmailServiceSecretHandler(emailService))
		corev1.RegisterAssetServiceServer(s, assetH)
	})
	cancel()
	return errors.Join(err, <-probesDone)
}

// publisher narrows a go-rabbitmq publisher to the Publish the emitters use.
type publisher struct{ p *rabbitmq.Publisher }

func (p publisher) Publish(ctx context.Context, routingKey string, body []byte) error {
	return p.p.Publish(ctx, routingKey, body)
}

type rabbitLogger struct{ l log.Logger }

func (r rabbitLogger) Debugf(f string, a ...any) { r.l.Debug(fmt.Sprintf(f, a...)) }
func (r rabbitLogger) Infof(f string, a ...any)  { r.l.Info(fmt.Sprintf(f, a...)) }
func (r rabbitLogger) Warnf(f string, a ...any)  { r.l.Warn(fmt.Sprintf(f, a...)) }
func (r rabbitLogger) Errorf(f string, a ...any) { r.l.Error(nil, fmt.Sprintf(f, a...)) }
