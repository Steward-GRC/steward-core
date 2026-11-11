// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// versionLinkBackend is the part of *store.VersionLinkStore the handler uses.
type versionLinkBackend interface {
	GetVersionLinks(ctx context.Context, versionID uuid.UUID) (domain.VersionLinks, error)
	SetReferences(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error)
	SetContactBlocks(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error)
	SetDefinitions(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error)
	SetRelated(ctx context.Context, versionID uuid.UUID, links []domain.RelatedLinkInput) (domain.VersionLinks, error)
}

// VersionLinkHandler implements corev1.VersionLinkServiceServer: the library
// links of a policy version (issue #70). It adds no access rules of its own;
// whether the caller may edit the policy is the gateway's to decide.
type VersionLinkHandler struct {
	corev1.UnimplementedVersionLinkServiceServer
	store   versionLinkBackend
	auditor auditEmitter
}

// NewVersionLinkHandler returns a handler over s. auditor may be nil.
func NewVersionLinkHandler(s versionLinkBackend, auditor auditEmitter) *VersionLinkHandler {
	return &VersionLinkHandler{store: s, auditor: auditor}
}

// versionLinkError maps a store or domain refusal to its gRPC code. Anything
// unlisted is Internal and its cause is logged, not sent.
func versionLinkError(ctx context.Context, op string, err error) error {
	l := logger.Ctx(ctx)
	switch {
	case errors.Is(err, store.ErrVersionNotFound), errors.Is(err, store.ErrLinkItemNotFound),
		errors.Is(err, store.ErrRelatedPolicyNotFound):
		l.Debug("version link op refused: not found", log.F("op", op), log.F("reason", err.Error()))
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrVersionNotEditable):
		l.Debug("version link op refused: version not editable", log.F("op", op))
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domain.ErrRepeatedLink), errors.Is(err, domain.ErrUnknownRelationshipType),
		errors.Is(err, domain.ErrRelatedToItself):
		l.Debug("version link op refused: invalid", log.F("op", op), log.F("reason", err.Error()))
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		l.Error(err, "version link op failed", log.F("op", op))
		return status.Errorf(codes.Internal, "%s failed", op)
	}
}

func parseUUIDs(field string, in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(in))
	for i, s := range in {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s[%d]: %v", field, i, err)
		}
		out = append(out, id)
	}
	return out, nil
}

func parseVersionID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "policy_version_id: %v", err)
	}
	return id, nil
}

func relatedDirectionToProto(d domain.RelatedDirection) corev1.RelatedDirection {
	switch d {
	case domain.RelatedOutgoing:
		return corev1.RelatedDirection_RELATED_DIRECTION_OUTGOING
	case domain.RelatedIncoming:
		return corev1.RelatedDirection_RELATED_DIRECTION_INCOMING
	default:
		return corev1.RelatedDirection_RELATED_DIRECTION_UNSPECIFIED
	}
}

func versionLinksToProto(v domain.VersionLinks) *corev1.VersionLinks {
	out := &corev1.VersionLinks{
		PolicyVersionId: v.PolicyVersionID.String(),
		Editable:        v.Editable,
		Definitions:     definitionEntriesToProto(v.Definitions),
		Related:         make([]*corev1.RelatedVersionLink, len(v.Related)),
		References:      make([]*corev1.Reference, len(v.References)),
		Contacts:        make([]*corev1.ContactBlock, len(v.Contacts)),
	}
	for i, r := range v.Related {
		out.Related[i] = &corev1.RelatedVersionLink{
			PolicyId: r.PolicyID.String(), Number: r.Number, Title: r.Title,
			RelationType: r.Type, Label: r.Label, Direction: relatedDirectionToProto(r.Direction),
		}
	}
	for i, r := range v.References {
		out.References[i] = referenceToProto(r)
	}
	for i, c := range v.Contacts {
		out.Contacts[i] = contactToProto(c)
	}
	return out
}

// ListRelationshipTypes handles VersionLinkService.ListRelationshipTypes.
func (h *VersionLinkHandler) ListRelationshipTypes(ctx context.Context, _ *corev1.ListRelationshipTypesRequest) (*corev1.ListRelationshipTypesResponse, error) {
	types := domain.RelationshipTypes()
	out := make([]*corev1.RelationshipType, len(types))
	for i, t := range types {
		out[i] = &corev1.RelationshipType{Id: t.ID, Label: t.Label, InverseId: t.InverseID, InverseLabel: t.InverseLabel}
	}
	logger.Ctx(ctx).Debug("relationship types listed", log.F("count", len(out)))
	return &corev1.ListRelationshipTypesResponse{Types: out}, nil
}

// GetVersionLinks handles VersionLinkService.GetVersionLinks.
func (h *VersionLinkHandler) GetVersionLinks(ctx context.Context, req *corev1.GetVersionLinksRequest) (*corev1.GetVersionLinksResponse, error) {
	vid, err := parseVersionID(req.GetPolicyVersionId())
	if err != nil {
		logger.Ctx(ctx).Debug("get version links refused: bad policy_version_id")
		return nil, err
	}
	logger.Ctx(ctx).Debug("get version links", log.F("version_id", vid.String()))
	links, err := h.store.GetVersionLinks(ctx, vid)
	if err != nil {
		return nil, versionLinkError(ctx, "get version links", err)
	}
	logger.Ctx(ctx).Debug("get version links done", log.F("version_id", vid.String()), log.F("editable", links.Editable),
		log.F("definitions", len(links.Definitions)), log.F("references", len(links.References)),
		log.F("contacts", len(links.Contacts)), log.F("related", len(links.Related)))
	return &corev1.GetVersionLinksResponse{Links: versionLinksToProto(links)}, nil
}

// applySet is the shared tail of the Set calls: run the change, audit it, log
// it and shape the answer.
func (h *VersionLinkHandler) applySet(ctx context.Context, op, action, actor string, vid uuid.UUID, count int,
	run func() (domain.VersionLinks, error)) (*corev1.VersionLinks, error) {
	logger.Ctx(ctx).Debug("set version links", log.F("op", op), log.F("version_id", vid.String()), log.F("count", count))
	links, err := run()
	if err != nil {
		return nil, versionLinkError(ctx, op, err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: action,
			Subject:     "policy_version:" + vid.String(),
			ActorUserID: actor,
		})
	}
	logger.Ctx(ctx).Debug("set version links done", log.F("op", op), log.F("version_id", vid.String()), log.F("count", count))
	return versionLinksToProto(links), nil
}

// SetVersionReferences handles VersionLinkService.SetVersionReferences.
func (h *VersionLinkHandler) SetVersionReferences(ctx context.Context, req *corev1.SetVersionReferencesRequest) (*corev1.SetVersionReferencesResponse, error) {
	vid, ids, err := parseSetRequest(req.GetPolicyVersionId(), "reference_ids", req.GetReferenceIds())
	if err != nil {
		return nil, err
	}
	links, err := h.applySet(ctx, "set version references", "policy.version_references_changed", req.GetActorUserId(), vid, len(ids),
		func() (domain.VersionLinks, error) { return h.store.SetReferences(ctx, vid, ids) })
	if err != nil {
		return nil, err
	}
	return &corev1.SetVersionReferencesResponse{Links: links}, nil
}

// SetVersionContactBlocks handles VersionLinkService.SetVersionContactBlocks.
func (h *VersionLinkHandler) SetVersionContactBlocks(ctx context.Context, req *corev1.SetVersionContactBlocksRequest) (*corev1.SetVersionContactBlocksResponse, error) {
	vid, ids, err := parseSetRequest(req.GetPolicyVersionId(), "contact_block_ids", req.GetContactBlockIds())
	if err != nil {
		return nil, err
	}
	links, err := h.applySet(ctx, "set version contact blocks", "policy.version_contacts_changed", req.GetActorUserId(), vid, len(ids),
		func() (domain.VersionLinks, error) { return h.store.SetContactBlocks(ctx, vid, ids) })
	if err != nil {
		return nil, err
	}
	return &corev1.SetVersionContactBlocksResponse{Links: links}, nil
}

// SetVersionDefinitions handles VersionLinkService.SetVersionDefinitions.
func (h *VersionLinkHandler) SetVersionDefinitions(ctx context.Context, req *corev1.SetVersionDefinitionsRequest) (*corev1.SetVersionDefinitionsResponse, error) {
	vid, ids, err := parseSetRequest(req.GetPolicyVersionId(), "definition_ids", req.GetDefinitionIds())
	if err != nil {
		return nil, err
	}
	links, err := h.applySet(ctx, "set version definitions", "policy.version_definitions_changed", req.GetActorUserId(), vid, len(ids),
		func() (domain.VersionLinks, error) { return h.store.SetDefinitions(ctx, vid, ids) })
	if err != nil {
		return nil, err
	}
	return &corev1.SetVersionDefinitionsResponse{Links: links}, nil
}

// SetVersionRelated handles VersionLinkService.SetVersionRelated.
func (h *VersionLinkHandler) SetVersionRelated(ctx context.Context, req *corev1.SetVersionRelatedRequest) (*corev1.SetVersionRelatedResponse, error) {
	vid, err := parseVersionID(req.GetPolicyVersionId())
	if err != nil {
		logger.Ctx(ctx).Debug("set version related refused: bad policy_version_id")
		return nil, err
	}
	in := make([]domain.RelatedLinkInput, 0, len(req.GetLinks()))
	for i, l := range req.GetLinks() {
		pid, perr := uuid.Parse(l.GetPolicyId())
		if perr != nil {
			logger.Ctx(ctx).Debug("set version related refused: bad policy_id", log.F("index", i))
			return nil, status.Errorf(codes.InvalidArgument, "links[%d].policy_id: %v", i, perr)
		}
		in = append(in, domain.RelatedLinkInput{PolicyID: pid, Type: l.GetRelationType()})
	}
	links, err := h.applySet(ctx, "set version related", "policy.version_related_changed", req.GetActorUserId(), vid, len(in),
		func() (domain.VersionLinks, error) { return h.store.SetRelated(ctx, vid, in) })
	if err != nil {
		return nil, err
	}
	return &corev1.SetVersionRelatedResponse{Links: links}, nil
}

// parseSetRequest reads the version id and the item ids of a Set call.
func parseSetRequest(versionID, field string, ids []string) (uuid.UUID, []uuid.UUID, error) {
	vid, err := parseVersionID(versionID)
	if err != nil {
		return uuid.Nil, nil, err
	}
	parsed, err := parseUUIDs(field, ids)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return vid, parsed, nil
}
