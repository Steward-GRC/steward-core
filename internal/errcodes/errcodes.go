// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package errcodes holds the core service's coded errors (band 4) and turns
// them into gRPC statuses through go-apperr.
package errcodes

import (
	"context"
	"errors"
	"strconv"
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
	CodeCategoryRefusesType  = 4007
	CodeCategoryHoldsDocs    = 4008
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
		{Code: CodeCategoryRefusesType, Symbol: "CATEGORY_REFUSES_TYPE", Category: apperr.CategoryFailedPrecondition,
			Title: "category document types", Cause: "a document was created in, or moved into, a category that does not accept its type",
			UserSafe: true, Message: "Category {category} does not accept {types}. {hint}"},
		{Code: CodeCategoryHoldsDocs, Symbol: "CATEGORY_HOLDS_DOCUMENTS", Category: apperr.CategoryFailedPrecondition,
			Title: "category document types", Cause: "narrowing the accepted document types would strand documents already in the category",
			UserSafe: true, Message: "Can't change what {category} accepts: it holds {count} documents the new setting would not accept ({policies} policies, {procedures} procedures). Move them to another category first."},
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

var (
	errCategoryRefusesType    = errors.New("core: category does not accept this document type")
	errCategoryHoldsDocuments = errors.New("core: category holds documents the new setting would not accept")
)

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

// CategoryRefusesType codes a create or move into a category that does not accept the document.
// name is the resolved category name, docType is "policy" or "procedure" and accepts is the
// category's setting ("policies", "procedures" or "none"). A container's message says to pick a
// child category.
func CategoryRefusesType(name, docType, accepts string) error {
	plural := docType + "s"
	if docType == "policy" {
		plural = "policies"
	}
	hint := "It holds " + accepts + " only."
	if accepts == "none" {
		hint = "It only holds other categories. Pick a child category."
	}
	return apperr.WithMeta(apperr.Coded(CodeCategoryRefusesType, errCategoryRefusesType),
		apperr.Meta("category", name), apperr.Meta("type", docType), apperr.Meta("types", plural),
		apperr.Meta("accepts", accepts), apperr.Meta("hint", hint))
}

// CategoryHoldsDocuments codes a narrowing refused because documents of a removed type are in the
// category; the counts say which.
func CategoryHoldsDocuments(name string, policies, procedures int) error {
	return apperr.WithMeta(apperr.Coded(CodeCategoryHoldsDocs, errCategoryHoldsDocuments),
		apperr.Meta("category", name), apperr.Meta("count", strconv.Itoa(policies+procedures)),
		apperr.Meta("policies", strconv.Itoa(policies)), apperr.Meta("procedures", strconv.Itoa(procedures)))
}

type logSink struct{ l log.Logger }

func (s logSink) LogCoded(ctx context.Context, code int, err error) {
	s.l.Ctx(ctx).Debug("coded error", log.F("code", code), log.F("error", err.Error()))
}
