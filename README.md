# steward-core 📚

> 🧭 Categories, templates, policies and procedures with versions for Steward

The core service holds Steward's documents and everything they're built from: the category tree and
its governance, templates, policies and procedures with their versions and numbers, and the shared
libraries documents attach.

- **Categories:** a tree up to three levels deep. Owners, the acknowledgement audience, review
  cadence and the default template inherit down the tree; access rules are ordered and stored as
  steward-authz rules.
- **Documents:** policies and procedures share one model: drafts, publish, versions, appendices,
  sensitivity, a number derived from the category, and a staged rename for a published document.
- **Libraries:** contact blocks, definitions (scoped to a category) and references, attached by id
  and rendered live; related links between documents.
- **Settings:** the site-wide announcement and maintenance notices, and the outbound email-service
  configuration, with its key encrypted at rest.
- **Editor images:** stored once in object storage, served by the gateway.

Core calls no other service. It publishes lifecycle events and steward-audit's `AuditEvent`.

## 🚀 Run

```bash
cp .env.example .env   # a local Postgres and RabbitMQ; set CORE_SETTINGS_KEY
task run
```

Or build the image with `docker build --build-arg VERSION=dev --build-arg COMMIT=$(git rev-parse HEAD) -t steward-core .`.
Settings are in [configuration](docs/configuration.md). The version and commit show up in the
`steward-version` and `steward-commit` health headers and on `/readyz`; the probes are in the
[runbook](docs/runbook.md#probes).

## 📚 Docs

- [API](docs/api.md): the gRPC services, the events out, and calling other services.
- [Configuration](docs/configuration.md).
- [Runbook](docs/runbook.md).
- [Error codes](docs/error-codes.md).

## 🛠 Develop

```bash
task build       # go build ./...
task test        # go test ./... (the store tests start Postgres with testcontainers)
task test-race   # the same with the race detector
task lint        # gofmt check + golangci-lint + yamllint
task proto       # fetch the pinned callee protos, buf lint, regenerate gen/
task license     # check Apache-2.0 headers (golic)
```

Set `DATABASE_TEST_DSN` to run the store tests against an existing Postgres instead of a container.

## 🙏 Acknowledgements

Steward was originally written by [@Bugs5382](https://github.com/Bugs5382).

## ⚖️ License

Apache-2.0 (c) 2026 The Steward Authors
