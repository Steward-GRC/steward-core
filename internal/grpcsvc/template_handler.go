// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strings"
	"time"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TemplateStorer is the store interface the TemplateHandler depends on.
type TemplateStorer interface {
	CreateTemplate(ctx context.Context, t domain.Template) (domain.Template, error)
	CreateTemplateVersion(ctx context.Context, tv domain.TemplateVersion) (domain.TemplateVersion, error)
	PublishTemplateVersion(ctx context.Context, id uuid.UUID) (domain.TemplateVersion, error)
	GetTemplateVersion(ctx context.Context, id uuid.UUID) (domain.TemplateVersion, error)
	GetLatestPublishedVersion(ctx context.Context, templateID uuid.UUID) (domain.TemplateVersion, error)
	UpdateTemplateVersionSections(ctx context.Context, id uuid.UUID, sections []domain.Section) (domain.TemplateVersion, error)
	ListTemplateVersions(ctx context.Context, templateID uuid.UUID) ([]domain.TemplateVersion, error)
	DeleteTemplateVersion(ctx context.Context, id uuid.UUID) error
	ListTemplates(ctx context.Context, ownerCategoryID *uuid.UUID) ([]domain.Template, error)
	RetireTemplate(ctx context.Context, id uuid.UUID) (domain.Template, error)
	RenameTemplate(ctx context.Context, id uuid.UUID, name string) (domain.Template, error)
	DeleteTemplate(ctx context.Context, id uuid.UUID) error
}

// TemplateHandler implements corev1.TemplateServiceServer.
type TemplateHandler struct {
	corev1.UnimplementedTemplateServiceServer
	store   TemplateStorer
	auditor auditEmitter
}

// NewTemplateHandler returns a new TemplateHandler. The auditor may be nil.
func NewTemplateHandler(s TemplateStorer, auditor auditEmitter) *TemplateHandler {
	return &TemplateHandler{store: s, auditor: auditor}
}

// normalizeLevel clamps the heading depth to 1-5, defaulting 0/unset to 1.
func normalizeLevel(l int) int {
	if l < 1 {
		return 1
	}
	if l > 5 {
		return 5
	}
	return l
}

func sectionsFromProto(in []*corev1.Section) []domain.Section {
	out := make([]domain.Section, 0, len(in))
	for _, s := range in {
		sec := domain.Section{
			Key:      s.GetKey(),
			Title:    s.GetTitle(),
			Order:    int(s.GetOrder()),
			Level:    normalizeLevel(int(s.GetLevel())),
			Required: s.GetRequired(),
		}
		for _, b := range s.GetBlocks() {
			bt := domain.BlockTypeEditable
			if b.GetType() == corev1.BlockType_BLOCK_TYPE_BOILERPLATE {
				bt = domain.BlockTypeBoilerplate
			}
			sec.Blocks = append(sec.Blocks, domain.Block{Type: bt, ContentJSON: b.GetContentJson()})
		}
		out = append(out, sec)
	}
	return out
}

func sectionsToProto(in []domain.Section) []*corev1.Section {
	out := make([]*corev1.Section, 0, len(in))
	for _, s := range in {
		ps := &corev1.Section{
			Key:      s.Key,
			Title:    s.Title,
			Order:    toInt32(s.Order),
			Level:    toInt32(normalizeLevel(s.Level)),
			Required: s.Required,
		}
		for _, b := range s.Blocks {
			bt := corev1.BlockType_BLOCK_TYPE_EDITABLE
			if b.Type == domain.BlockTypeBoilerplate {
				bt = corev1.BlockType_BLOCK_TYPE_BOILERPLATE
			}
			ps.Blocks = append(ps.Blocks, &corev1.Block{Type: bt, ContentJson: b.ContentJSON})
		}
		out = append(out, ps)
	}
	return out
}

func templateToProto(t domain.Template) *corev1.Template {
	retiredAt := ""
	if t.RetiredAt != nil {
		retiredAt = t.RetiredAt.UTC().Format(time.RFC3339)
	}
	return &corev1.Template{
		Id:              t.ID.String(),
		Code:            t.Code,
		Name:            t.Name,
		OwnerCategoryId: nilableUUID(t.OwnerCategoryID),
		RetiredAt:       retiredAt,
	}
}

func templateVersionToProto(tv domain.TemplateVersion) *corev1.TemplateVersion {
	return &corev1.TemplateVersion{
		Id:         tv.ID.String(),
		TemplateId: tv.TemplateID.String(),
		VersionNo:  toInt32(tv.VersionNo),
		Status:     templateVersionStatusToProto(tv.Status),
		Sections:   sectionsToProto(tv.Sections),
	}
}

func templateVersionStatusToProto(s domain.TemplateVersionStatus) corev1.TemplateVersionStatus {
	switch s {
	case domain.TemplateVersionStatusDraft:
		return corev1.TemplateVersionStatus_TEMPLATE_VERSION_STATUS_DRAFT
	case domain.TemplateVersionStatusPublished:
		return corev1.TemplateVersionStatus_TEMPLATE_VERSION_STATUS_PUBLISHED
	case domain.TemplateVersionStatusArchived:
		return corev1.TemplateVersionStatus_TEMPLATE_VERSION_STATUS_ARCHIVED
	default:
		return corev1.TemplateVersionStatus_TEMPLATE_VERSION_STATUS_UNSPECIFIED
	}
}

// CreateTemplate handles TemplateService.CreateTemplate.
func (h *TemplateHandler) CreateTemplate(ctx context.Context, req *corev1.CreateTemplateRequest) (*corev1.CreateTemplateResponse, error) {
	ownerID, err := parseOptionalUUID(req.GetOwnerCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid owner_category_id: %v", err)
	}
	t, err := domain.NewTemplate(req.GetName(), ownerID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	t, err = h.store.CreateTemplate(ctx, t)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create template: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template.created",
			Subject:     "template:" + t.ID.String(),
			GroupID:     nilableUUID(t.OwnerCategoryID),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.CreateTemplateResponse{Template: templateToProto(t)}, nil
}

// CreateTemplateVersion handles TemplateService.CreateTemplateVersion.
func (h *TemplateHandler) CreateTemplateVersion(ctx context.Context, req *corev1.CreateTemplateVersionRequest) (*corev1.CreateTemplateVersionResponse, error) {
	tv, err := domain.NewTemplateVersion(req.GetTemplateId(), sectionsFromProto(req.GetSections()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	tv, err = h.store.CreateTemplateVersion(ctx, tv)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create template version: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template_version.created",
			Subject:     "template_version:" + tv.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.CreateTemplateVersionResponse{Version: templateVersionToProto(tv)}, nil
}

// PublishTemplateVersion handles TemplateService.PublishTemplateVersion.
func (h *TemplateHandler) PublishTemplateVersion(ctx context.Context, req *corev1.PublishTemplateVersionRequest) (*corev1.PublishTemplateVersionResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	tv, err := h.store.PublishTemplateVersion(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrEmptyTemplateVersion) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "publish: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template_version.published",
			Subject:     "template_version:" + tv.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.PublishTemplateVersionResponse{Version: templateVersionToProto(tv)}, nil
}

// GetLatestTemplateVersion handles TemplateService.GetLatestTemplateVersion.
func (h *TemplateHandler) GetLatestTemplateVersion(ctx context.Context, req *corev1.GetLatestTemplateVersionRequest) (*corev1.GetLatestTemplateVersionResponse, error) {
	tid, err := uuid.Parse(req.GetTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_id: %v", err)
	}
	tv, err := h.store.GetLatestPublishedVersion(ctx, tid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "template version not found")
		}
		logger.Ctx(ctx).Error(err, "get latest template version failed", log.F("template_id", tid.String()))
		return nil, status.Error(codes.Internal, "get latest template version failed")
	}
	return &corev1.GetLatestTemplateVersionResponse{Version: templateVersionToProto(tv)}, nil
}

// UpdateTemplateVersionSections handles TemplateService.UpdateTemplateVersionSections.
func (h *TemplateHandler) UpdateTemplateVersionSections(ctx context.Context, req *corev1.UpdateTemplateVersionSectionsRequest) (*corev1.UpdateTemplateVersionSectionsResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	tv, err := h.store.UpdateTemplateVersionSections(ctx, id, sectionsFromProto(req.GetSections()))
	if err != nil {
		if errors.Is(err, store.ErrTemplateVersionNotEditable) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "update sections: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template_version.updated",
			Subject:     "template_version:" + tv.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.UpdateTemplateVersionSectionsResponse{Version: templateVersionToProto(tv)}, nil
}

// DeleteTemplateVersion handles TemplateService.DeleteTemplateVersion (discard draft).
func (h *TemplateHandler) DeleteTemplateVersion(ctx context.Context, req *corev1.DeleteTemplateVersionRequest) (*corev1.DeleteTemplateVersionResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	if err := h.store.DeleteTemplateVersion(ctx, id); err != nil {
		if errors.Is(err, store.ErrTemplateVersionNotEditable) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "discard version: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template_version.discarded",
			Subject:     "template_version:" + id.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.DeleteTemplateVersionResponse{}, nil
}

// ListTemplateVersions handles TemplateService.ListTemplateVersions.
func (h *TemplateHandler) ListTemplateVersions(ctx context.Context, req *corev1.ListTemplateVersionsRequest) (*corev1.ListTemplateVersionsResponse, error) {
	tid, err := uuid.Parse(req.GetTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_id: %v", err)
	}
	vs, err := h.store.ListTemplateVersions(ctx, tid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list template versions: %v", err)
	}
	out := make([]*corev1.TemplateVersion, 0, len(vs))
	for _, v := range vs {
		out = append(out, templateVersionToProto(v))
	}
	return &corev1.ListTemplateVersionsResponse{Versions: out}, nil
}

// ListTemplates handles TemplateService.ListTemplates.
func (h *TemplateHandler) ListTemplates(ctx context.Context, req *corev1.ListTemplatesRequest) (*corev1.ListTemplatesResponse, error) {
	var owner *uuid.UUID
	if req.GetOwnerCategoryId() != "" {
		id, err := uuid.Parse(req.GetOwnerCategoryId())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid owner_category_id: %v", err)
		}
		owner = &id
	}
	ts, err := h.store.ListTemplates(ctx, owner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list templates: %v", err)
	}
	out := make([]*corev1.Template, 0, len(ts))
	for _, t := range ts {
		out = append(out, templateToProto(t))
	}
	return &corev1.ListTemplatesResponse{Templates: out}, nil
}

// RetireTemplate handles TemplateService.RetireTemplate (soft-delete).
func (h *TemplateHandler) RetireTemplate(ctx context.Context, req *corev1.RetireTemplateRequest) (*corev1.RetireTemplateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	t, err := h.store.RetireTemplate(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrTemplateNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "retire template: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template.retired",
			Subject: "template:" + t.ID.String(),
			GroupID: nilableUUID(t.OwnerCategoryID),
		})
	}
	return &corev1.RetireTemplateResponse{Template: templateToProto(t)}, nil
}

// RenameTemplate handles TemplateService.RenameTemplate. It makes no new version.
func (h *TemplateHandler) RenameTemplate(ctx context.Context, req *corev1.RenameTemplateRequest) (*corev1.RenameTemplateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	t, err := h.store.RenameTemplate(ctx, id, name)
	if err != nil {
		if errors.Is(err, store.ErrTemplateNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "rename template: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template.renamed",
			Subject:    "template:" + t.ID.String(),
			GroupID:    nilableUUID(t.OwnerCategoryID),
			Attributes: map[string]string{"to_name": t.Name},
		})
	}
	return &corev1.RenameTemplateResponse{Template: templateToProto(t)}, nil
}

// DeleteTemplate handles TemplateService.DeleteTemplate. A template a
// document uses is FailedPrecondition, so the caller can retire it instead.
func (h *TemplateHandler) DeleteTemplate(ctx context.Context, req *corev1.DeleteTemplateRequest) (*corev1.DeleteTemplateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	if err := h.store.DeleteTemplate(ctx, id); err != nil {
		switch {
		case errors.Is(err, store.ErrTemplateNotFound):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case errors.Is(err, store.ErrTemplateReferenced):
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		default:
			return nil, status.Errorf(codes.Internal, "delete template: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "template.deleted",
			Subject: "template:" + id.String(),
		})
	}
	return &corev1.DeleteTemplateResponse{}, nil
}
