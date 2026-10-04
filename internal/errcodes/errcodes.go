// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package errcodes holds the core service's coded errors (band 4) and turns
// them into gRPC statuses through go-apperr.
package errcodes

import (
	"context"
	"errors"
	"sync"

	apperr "github.com/Bugs5382/go-apperr"
	"github.com/Bugs5382/go-apperr/apperrgrpc"
	log "github.com/Bugs5382/go-log"
)

// Domain is the ErrorInfo domain every core error carries.
const Domain = "core"

// The core service's codes.
const (
	CodeInternal             = 4000
	CodeStoreUnavailable     = 4001
	CodeCategoryNotDeletable = 4002
	CodeInvalidCategoryRule  = 4003
)

// Entries returns the registry entries.
func Entries() []apperr.Entry {
	return []apperr.Entry{
		{Code: CodeInternal, Symbol: "INTERNAL", Category: apperr.CategoryInternal,
			Title: "core", Cause: "an uncoded failure inside the core service"},
		{Code: CodeStoreUnavailable, Symbol: "STORE_UNAVAILABLE", Category: apperr.CategoryInternal,
			Title: "core store", Cause: "a core Postgres read or write failed; the op metadata names it, the cause is only logged"},
		{Code: CodeCategoryNotDeletable, Symbol: "CATEGORY_NOT_DELETABLE", Category: apperr.CategoryFailedPrecondition,
			Title: "delete category", Cause: "the category or one of its subcategories still holds documents",
			UserSafe: true, Message: "Can't delete category {category}: it or one of its subcategories still has documents. Move or delete those documents first, then delete the category."},
		{Code: CodeInvalidCategoryRule, Symbol: "INVALID_CATEGORY_RULE", Category: apperr.CategoryInvalid,
			Title: "category rules", Cause: "a category ruleset failed the steward-authz validation",
			UserSafe: true, Message: "Access rule {rule} in {category} is not valid ({problem})."},
	}
}

var (
	regOnce sync.Once
	reg     *apperr.Registry
)

// Registry returns the service registry. Coded errors are logged through
// go-log with the trace of the request they failed.
func Registry() *apperr.Registry {
	regOnce.Do(func() {
		r, err := apperr.NewRegistry(Entries(), apperr.WithService(4), apperr.WithCodeDigits(4),
			apperr.WithLogger(logSink{log.NewLogger("core")}))
		if err != nil {
			panic(err)
		}
		reg = r
	})
	return reg
}

// Error turns err into the gRPC error a handler returns.
func Error(ctx context.Context, err error) error {
	return apperrgrpc.Error(ctx, Registry(), err, CodeInternal, Domain)
}

// Doc is the Markdown body of docs/error-codes.md.
func Doc() string {
	return "# Error codes\n\nEvery coded gRPC error from the core service carries an `ErrorInfo` with the symbol as its\n" +
		"reason, the domain `" + Domain + "` and the code in `codeNum`. Only user-safe messages reach the\n" +
		"caller; every other code is sent as `Code N: Internal Error`.\n\n" + Registry().Markdown()
}

// StoreUnavailable codes a failed store call; op names it.
func StoreUnavailable(op string, cause error) error {
	return apperr.WithMeta(apperr.Coded(CodeStoreUnavailable, cause), apperr.Meta("op", op))
}

var errCategoryHasDocuments = errors.New("core: category still has documents")

// CategoryNotDeletable codes a refused category delete. name is the resolved
// category name, never its id, so the message can be shown as it is.
func CategoryNotDeletable(name string, cause error) error {
	if cause == nil {
		cause = errCategoryHasDocuments
	}
	return apperr.WithMeta(apperr.Coded(CodeCategoryNotDeletable, cause), apperr.Meta("category", name))
}

// InvalidCategoryRule codes a ruleset steward-authz refused. The category,
// rule and problem metadata it attached are kept.
func InvalidCategoryRule(cause error) error {
	return apperr.Coded(CodeInvalidCategoryRule, cause)
}

type logSink struct{ l log.Logger }

func (s logSink) LogCoded(ctx context.Context, code int, err error) {
	s.l.Ctx(ctx).Debug("coded error", log.F("code", code), log.F("error", err.Error()))
}
