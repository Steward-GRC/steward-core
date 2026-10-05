// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lifecycleCapture records every lifecycle publish, so tests can see the jobs-exchange event beside
// the audit one.
type lifecycleCapture struct {
	calls []lifecycleCall
}

type lifecycleCall struct {
	routingKey string
	body       []byte
}

func (c *lifecycleCapture) Publish(_ context.Context, rk string, body []byte) error {
	c.calls = append(c.calls, lifecycleCall{routingKey: rk, body: append([]byte(nil), body...)})
	return nil
}

// capturePublisher records each audit message with its routing key and decoded event.
type capturePublisher struct {
	calls []capturedCall
}

type capturedCall struct {
	routingKey string
	event      audit.Event
}

func (c *capturePublisher) Publish(_ context.Context, rk string, body []byte) error {
	e, err := audit.Decode(body)
	if err != nil {
		return err
	}
	c.calls = append(c.calls, capturedCall{routingKey: rk, event: e})
	return nil
}

// fakePolicyStore satisfies grpcsvc.PolicyStorer without a real DB.
type fakePolicyStore struct {
	policies      map[uuid.UUID]domain.Policy
	versions      map[uuid.UUID]domain.PolicyVersion
	draftByPolicy map[uuid.UUID]domain.PolicyVersion
	// templateUpdateAvailable is the canned answer for
	// IsTemplateUpdateAvailable; templateUpdateErr forces an error path.
	templateUpdateAvailable bool
	templateUpdateErr       error
	// deleteErr, when non-nil, is returned by DeletePolicy to exercise the
	// handler's error mapping (e.g. store.ErrHasPublishedVersion).
	deleteErr error
	// reassignAuthorGrants is the author-grant count ReassignUserPolicies reports; the fake holds
	// no rules.
	reassignAuthorGrants int
}

func newFakePolicyStore() *fakePolicyStore {
	return &fakePolicyStore{
		policies:      map[uuid.UUID]domain.Policy{},
		versions:      map[uuid.UUID]domain.PolicyVersion{},
		draftByPolicy: map[uuid.UUID]domain.PolicyVersion{},
	}
}

func (f *fakePolicyStore) CreatePolicy(_ context.Context, p domain.Policy) (domain.Policy, error) {
	// The real store numbers from the category's code, tested in the store tests; a constant is
	// enough here.
	p.Number = "POL-NEW-000001"
	// Like the real store: seed the first freeform draft with the policy and return its id.
	draft := domain.PolicyVersion{
		ID:          uuid.New(),
		PolicyID:    p.ID,
		Status:      domain.PolicyVersionStatusDraft,
		ContentJSON: "{}",
		CreatedBy:   p.OwnerUserID,
	}
	p.CurrentDraftVersionID = draft.ID
	f.policies[p.ID] = p
	f.versions[draft.ID] = draft
	f.draftByPolicy[p.ID] = draft
	return p, nil
}

func (f *fakePolicyStore) GetPolicy(_ context.Context, id uuid.UUID) (domain.Policy, error) {
	p, ok := f.policies[id]
	if !ok {
		return domain.Policy{}, fmt.Errorf("not found")
	}
	return p, nil
}

func (f *fakePolicyStore) ListPolicies(_ context.Context, categoryID uuid.UUID, includeDescendants bool, docType domain.DocumentType) ([]domain.Policy, error) {
	var out []domain.Policy
	for _, p := range f.policies {
		if !includeDescendants && p.HomeCategoryID != categoryID {
			continue
		}
		// Like the real store: a zero document type is a policy, and the filter is an exact match.
		stored := p.DocumentType
		if stored == "" {
			stored = domain.DocumentTypePolicy
		}
		if stored != docType {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakePolicyStore) GetPolicyVersion(_ context.Context, id uuid.UUID) (domain.PolicyVersion, error) {
	pv, ok := f.versions[id]
	if !ok {
		return domain.PolicyVersion{}, fmt.Errorf("not found")
	}
	return pv, nil
}

func (f *fakePolicyStore) UpsertDraft(_ context.Context, pv domain.PolicyVersion) (domain.PolicyVersion, store.UpsertDraftResult, error) {
	prev, existed := f.draftByPolicy[pv.PolicyID]
	res := store.UpsertDraftResult{
		Inserted:       !existed,
		ContentChanged: !existed || prev.ContentJSON != pv.ContentJSON,
	}
	pv.Status = domain.PolicyVersionStatusDraft
	f.draftByPolicy[pv.PolicyID] = pv
	f.versions[pv.ID] = pv
	return pv, res, nil
}

func (f *fakePolicyStore) UpdateDraftContent(_ context.Context, policyVersionID uuid.UUID, contentJSON string) error {
	pv, ok := f.versions[policyVersionID]
	if !ok {
		return fmt.Errorf("not found")
	}
	if pv.Status != domain.PolicyVersionStatusDraft {
		return fmt.Errorf("not draft")
	}
	pv.ContentJSON = contentJSON
	f.versions[policyVersionID] = pv
	f.draftByPolicy[pv.PolicyID] = pv
	return nil
}

func (f *fakePolicyStore) PublishDraft(_ context.Context, policyID, _ uuid.UUID) (domain.PolicyVersion, error) {
	pv, ok := f.draftByPolicy[policyID]
	if !ok {
		return domain.PolicyVersion{}, fmt.Errorf("no draft")
	}
	pv.Status = domain.PolicyVersionStatusPublished
	pv.VersionNo = 1
	f.versions[pv.ID] = pv
	delete(f.draftByPolicy, policyID)
	return pv, nil
}

func (f *fakePolicyStore) DiscardDraft(_ context.Context, policyID uuid.UUID) error {
	pv, ok := f.draftByPolicy[policyID]
	if !ok {
		return store.ErrNoDraft
	}
	delete(f.draftByPolicy, policyID)
	delete(f.versions, pv.ID)
	return nil
}

func (f *fakePolicyStore) DeletePolicy(_ context.Context, policyID uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.policies[policyID]; !ok {
		return pgx.ErrNoRows
	}
	// Cascade: remove the policy, its draft, and any versions.
	if d, ok := f.draftByPolicy[policyID]; ok {
		delete(f.versions, d.ID)
		delete(f.draftByPolicy, policyID)
	}
	for id, pv := range f.versions {
		if pv.PolicyID == policyID {
			delete(f.versions, id)
		}
	}
	delete(f.policies, policyID)
	return nil
}

func (f *fakePolicyStore) RetirePolicy(_ context.Context, id uuid.UUID) (domain.Policy, error) {
	p, ok := f.policies[id]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	// Idempotent, like the store's WHERE retired_at IS NULL.
	if p.RetiredAt == nil {
		now := time.Now().UTC()
		p.RetiredAt = &now
		f.policies[id] = p
	}
	return p, nil
}

func (f *fakePolicyStore) SetVersionStatus(_ context.Context, policyVersionID uuid.UUID, target domain.PolicyVersionStatus) (domain.PolicyVersion, error) {
	pv, ok := f.versions[policyVersionID]
	if !ok {
		return domain.PolicyVersion{}, fmt.Errorf("not found")
	}
	if err := domain.ValidateVersionStatusTransition(pv.Status, target); err != nil {
		return domain.PolicyVersion{}, err
	}
	pv.Status = target
	f.versions[policyVersionID] = pv
	if pol, ok := f.policies[pv.PolicyID]; ok {
		switch target {
		case domain.PolicyVersionStatusPublished:
			pol.CurrentPublishedVersionID = pv.ID
		case domain.PolicyVersionStatusDraft:
			if pol.CurrentPublishedVersionID == pv.ID {
				pol.CurrentPublishedVersionID = uuid.Nil
			}
		}
		f.policies[pv.PolicyID] = pol
	}
	return pv, nil
}

func (f *fakePolicyStore) SetTitle(_ context.Context, policyID uuid.UUID, title string) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	p.Title = title
	f.policies[policyID] = p
	return p, nil
}

func (f *fakePolicyStore) SetProposedTitle(_ context.Context, policyVersionID uuid.UUID, proposedTitle *string) error {
	pv, ok := f.versions[policyVersionID]
	if !ok {
		return fmt.Errorf("not found")
	}
	if pv.Status != domain.PolicyVersionStatusDraft {
		return fmt.Errorf("not draft")
	}
	pv.ProposedTitle = proposedTitle
	f.versions[policyVersionID] = pv
	f.draftByPolicy[pv.PolicyID] = pv
	return nil
}

func (f *fakePolicyStore) IsTemplateUpdateAvailable(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return f.templateUpdateAvailable, f.templateUpdateErr
}

func (f *fakePolicyStore) ListPublishedVersions(_ context.Context, policyID uuid.UUID) ([]domain.PolicyVersion, error) {
	var out []domain.PolicyVersion
	for _, pv := range f.versions {
		if pv.PolicyID == policyID && pv.Status != domain.PolicyVersionStatusDraft {
			out = append(out, pv)
		}
	}
	return out, nil
}

// fakeTemplateStore satisfies grpcsvc.TemplateStorer without a real DB. Tests seed versions by id;
// an unknown id gets a generic published version.
type fakeTemplateStore struct {
	versions map[uuid.UUID]domain.TemplateVersion
	// getVersionCalls records every id passed to GetTemplateVersion so tests
	// can assert that the free-form path never looks a template version up.
	getVersionCalls []uuid.UUID
}

func newFakeTemplateStore() *fakeTemplateStore {
	return &fakeTemplateStore{versions: map[uuid.UUID]domain.TemplateVersion{}}
}

func (f *fakeTemplateStore) CreateTemplate(_ context.Context, t domain.Template) (domain.Template, error) {
	return t, nil
}

func (f *fakeTemplateStore) CreateTemplateVersion(_ context.Context, tv domain.TemplateVersion) (domain.TemplateVersion, error) {
	return tv, nil
}

func (f *fakeTemplateStore) PublishTemplateVersion(_ context.Context, id uuid.UUID) (domain.TemplateVersion, error) {
	return domain.TemplateVersion{ID: id, Status: domain.TemplateVersionStatusPublished}, nil
}

func (f *fakeTemplateStore) GetTemplateVersion(_ context.Context, id uuid.UUID) (domain.TemplateVersion, error) {
	if f != nil {
		f.getVersionCalls = append(f.getVersionCalls, id)
	}
	if f != nil && f.versions != nil {
		if tv, ok := f.versions[id]; ok {
			return tv, nil
		}
	}
	return domain.TemplateVersion{ID: id, Status: domain.TemplateVersionStatusPublished}, nil
}

func (f *fakeTemplateStore) GetLatestPublishedVersion(_ context.Context, templateID uuid.UUID) (domain.TemplateVersion, error) {
	return domain.TemplateVersion{ID: uuid.New(), TemplateID: templateID, Status: domain.TemplateVersionStatusPublished}, nil
}

func (f *fakeTemplateStore) ListTemplates(_ context.Context, _ *uuid.UUID) ([]domain.Template, error) {
	return nil, nil
}

func (f *fakeTemplateStore) UpdateTemplateVersionSections(_ context.Context, id uuid.UUID, sections []domain.Section) (domain.TemplateVersion, error) {
	tv := f.versions[id]
	tv.Sections = sections
	f.versions[id] = tv
	return tv, nil
}

func (f *fakeTemplateStore) ListTemplateVersions(_ context.Context, templateID uuid.UUID) ([]domain.TemplateVersion, error) {
	var out []domain.TemplateVersion
	for _, v := range f.versions {
		if v.TemplateID == templateID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeTemplateStore) DeleteTemplateVersion(_ context.Context, id uuid.UUID) error {
	delete(f.versions, id)
	return nil
}

func (f *fakeTemplateStore) RetireTemplate(_ context.Context, _ uuid.UUID) (domain.Template, error) {
	return domain.Template{}, nil
}

func (f *fakeTemplateStore) RenameTemplate(_ context.Context, id uuid.UUID, name string) (domain.Template, error) {
	return domain.Template{ID: id, Name: name}, nil
}

func (f *fakeTemplateStore) DeleteTemplate(_ context.Context, _ uuid.UUID) error {
	return nil
}

// seedDraftForUpdate sets up a fake template store with a section + block
// structure, a fake policy store with a draft pinned to that template version,
// and returns the IDs the caller needs to drive an UpdateDraftContent request.
func seedDraftForUpdate(t *testing.T, fakePS *fakePolicyStore, fakeTS *fakeTemplateStore) (draftID, tvID uuid.UUID, policyID uuid.UUID) {
	t.Helper()
	tvID = uuid.New()
	// Block.ContentJSON stores the boilerplate content for boilerplate blocks
	// and the regionKey for editable blocks — see templateSectionsForValidate.
	fakeTS.versions[tvID] = domain.TemplateVersion{
		ID:     tvID,
		Status: domain.TemplateVersionStatusPublished,
		Sections: []domain.Section{
			{
				Key:   "purpose",
				Order: 0,
				Blocks: []domain.Block{
					{Type: domain.BlockTypeBoilerplate, ContentJSON: "This policy applies to all staff."},
					{Type: domain.BlockTypeEditable, ContentJSON: "purpose-body"},
				},
			},
		},
	}
	policyID = uuid.New()
	draftID = uuid.New()
	fakePS.versions[draftID] = domain.PolicyVersion{
		ID:                draftID,
		PolicyID:          policyID,
		Status:            domain.PolicyVersionStatusDraft,
		TemplateVersionID: tvID,
		ContentJSON:       `{}`,
	}
	fakePS.draftByPolicy[policyID] = fakePS.versions[draftID]
	return draftID, tvID, policyID
}

// validLexicalContent returns a JSON document that satisfies the section
// structure produced by seedDraftForUpdate.
func validLexicalContent() string {
	doc := map[string]any{
		"root": map[string]any{
			"type": "root",
			"children": []any{
				map[string]any{
					"type":       "policy-section",
					"sectionKey": "purpose",
					"order":      0,
					"children": []any{
						map[string]any{"type": "policy-boilerplate", "content": "This policy applies to all staff."},
						map[string]any{
							"type":      "policy-editable-region",
							"regionKey": "purpose-body",
							"children":  []any{},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// TestUpdateDraftContentAcceptsValidContent verifies the happy path: a valid
// Lexical snapshot is stored, the audit event is emitted, and Accepted=true.
func TestUpdateDraftContentAcceptsValidContent(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	content := validLexicalContent()
	actor := uuid.New().String()

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       content,
		TemplateVersionId: tvID.String(),
		ActorUserId:       actor,
	})
	if err != nil {
		t.Fatalf("UpdateDraftContent: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected Accepted=true, got false (reason: %s)", resp.GetRejectReason())
	}
	if resp.GetDraftId() != draftID.String() {
		t.Fatalf("draft_id: got %q want %q", resp.GetDraftId(), draftID.String())
	}
	// Persistence: the fake store should now hold the new content for this draft.
	if got := fakePS.versions[draftID].ContentJSON; got != content {
		t.Fatalf("content not persisted; got %q", got)
	}
	// Audit: exactly one event, action=policy.draft_content_updated.
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	if got := cap.calls[0].event.Action; got != "policy.draft_content_updated" {
		t.Fatalf("audit action: got %q", got)
	}
	if got := cap.calls[0].event.ActorUserID; got != actor {
		t.Fatalf("audit actor: got %q want %q", got, actor)
	}
}

// uiAuthoredContent is the shape the editor serializes: a flat root of heading and paragraph nodes,
// which SaveDraft accepts for the same drafts.
func uiAuthoredContent(bodyText string) string {
	doc := map[string]any{
		"root": map[string]any{
			"type": "root", "version": 1,
			"children": []any{
				map[string]any{
					"type": "heading", "tag": "h1", "version": 1,
					"children": []any{map[string]any{
						"type": "text", "text": "Purpose", "version": 1,
					}},
				},
				map[string]any{
					"type": "paragraph", "version": 1,
					"children": []any{map[string]any{
						"type": "text", "text": bodyText, "version": 1,
					}},
				},
			},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// TestUpdateDraftContentAcceptsUIAuthoredContent: the strict section validator refused everything
// the editor produces (`root[0]: expected policy-section, got "heading"`), so no collaborative edit
// could persist. The snapshot must be accepted and stored.
func TestUpdateDraftContentAcceptsUIAuthoredContent(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	content := uiAuthoredContent("An edit made in the collaborative editor.")

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       content,
		TemplateVersionId: tvID.String(),
		ActorUserId:       uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("UpdateDraftContent rejected editor-authored content: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected Accepted=true, got false (reason: %s)", resp.GetRejectReason())
	}
	if got := fakePS.versions[draftID].ContentJSON; got != content {
		t.Fatalf("content not persisted; got %q", got)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentRefusesBlankingADraftWithText pins the data-loss guard:
// a checkpoint carrying no author text must never replace a draft that has
// text. This is the failure mode where an editor cannot parse its initial state,
// renders an empty document, and the debounced checkpoint writes that emptiness
// over the live draft.
func TestUpdateDraftContentRefusesBlankingADraftWithText(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)

	// The draft already holds real authored text.
	existing := uiAuthoredContent("Real authored policy text that must survive.")
	pv := fakePS.versions[draftID]
	pv.ContentJSON = existing
	fakePS.versions[draftID] = pv

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	// Structurally well-formed but textually empty — exactly what a Lexical
	// instance renders when it fails to parse its input.
	blank := `{"root":{"type":"root","children":[{"type":"paragraph","children":[]}]}}`

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       blank,
		TemplateVersionId: tvID.String(),
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatalf("expected the blank checkpoint to be refused, got resp=%+v", resp)
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("status code: got %v want FailedPrecondition", got)
	}
	// The authored draft must be untouched.
	if got := fakePS.versions[draftID].ContentJSON; got != existing {
		t.Fatalf("draft content was overwritten by a blank checkpoint: %q", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events on refusal, got %d", len(cap.calls))
	}
}

// A blank checkpoint over an already-textless draft is harmless and must still
// be accepted, so a genuinely new draft is not wedged by the guard.
func TestUpdateDraftContentAllowsBlankOverTextlessDraft(t *testing.T) {
	em := audit.New(&capturePublisher{})
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	blank := `{"root":{"type":"root","children":[{"type":"paragraph","children":[]}]}}`

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       blank,
		TemplateVersionId: tvID.String(),
		ActorUserId:       uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("blank checkpoint over a textless draft must be accepted: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatal("expected Accepted=true")
	}
}

// Section-shaped content is still held to the full strict contract on this path.
func TestUpdateDraftContentStillEnforcesStrictSectionShape(t *testing.T) {
	em := audit.New(&capturePublisher{})
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	// Declares itself section-shaped but edits the locked boilerplate.
	tampered := `{"root":{"type":"root","children":[
	  {"type":"policy-section","sectionKey":"purpose","order":0,"children":[
	    {"type":"policy-boilerplate","content":"I rewrote the locked text."}]}]}}`

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       tampered,
		TemplateVersionId: tvID.String(),
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("edited boilerplate in section-shaped content must still be rejected")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
}

// TestUpdateDraftContentRejectsInvalidStructure verifies that a structurally
// invalid snapshot is rejected with codes.InvalidArgument, that no DB write
// occurs, and that no audit event is emitted.
func TestUpdateDraftContentRejectsInvalidStructure(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)
	priorContent := fakePS.versions[draftID].ContentJSON

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	// Empty root.children violates rule 1 (must equal template section count).
	badContent := `{"root":{"type":"root","children":[]}}`

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       badContent,
		TemplateVersionId: tvID.String(),
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatalf("expected error, got nil (resp=%+v)", resp)
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	// No DB write: content unchanged.
	if got := fakePS.versions[draftID].ContentJSON; got != priorContent {
		t.Fatalf("content was modified on validation failure: %q", got)
	}
	// No audit emission.
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events on rejection, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentRejectsTemplateVersionMismatch verifies that when the
// request's template_version_id differs from the one pinned on the draft, the
// handler returns InvalidArgument and does not touch the store.
func TestUpdateDraftContentRejectsTemplateVersionMismatch(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, _, policyID := seedDraftForUpdate(t, fakePS, fakeTS)
	priorContent := fakePS.versions[draftID].ContentJSON

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	wrongTV := uuid.New().String()

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       validLexicalContent(),
		TemplateVersionId: wrongTV,
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	if got := fakePS.versions[draftID].ContentJSON; got != priorContent {
		t.Fatalf("content was modified on mismatch: %q", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentUnauthenticated verifies that a missing actor_user_id
// is rejected with codes.Unauthenticated before any DB or audit work.
func TestUpdateDraftContentUnauthenticated(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, tvID, policyID := seedDraftForUpdate(t, fakePS, fakeTS)
	priorContent := fakePS.versions[draftID].ContentJSON

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       validLexicalContent(),
		TemplateVersionId: tvID.String(),
		// ActorUserId intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("status code: got %v want Unauthenticated", got)
	}
	if got := fakePS.versions[draftID].ContentJSON; got != priorContent {
		t.Fatalf("content was modified on unauthenticated call: %q", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}

// TestCreatePolicySeedsInitialDraft: a new policy already has a draft (current_draft_version_id is
// set), so it can be submitted without a separate save, and that draft is audited
// (policy.draft_saved) beside policy.created.
func TestCreatePolicySeedsInitialDraft(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	// A home category must exist (CreatePolicy resolves it for the number code).
	g, err := fakeGS.Create(context.Background(), domain.Category{Name: "IT", Slug: "it"})
	if err != nil {
		t.Fatalf("seed category: %v", err)
	}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	owner := uuid.New().String()
	resp, err := h.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
		HomeCategoryId: g.ID.String(),
		Title:          "IT Security Policy",
		OwnerUserId:    owner,
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// The response's policy must carry a non-empty current_draft_version_id:
	// this is exactly the field the UI's submit-for-approval flow requires.
	draftID := resp.GetPolicy().GetCurrentDraftVersionId()
	if draftID == "" {
		t.Fatal("expected current_draft_version_id to be set on a freshly created policy")
	}

	// The store must actually hold a draft version for the policy.
	pid, err := uuid.Parse(resp.GetPolicy().GetId())
	if err != nil {
		t.Fatalf("parse policy id: %v", err)
	}
	dv, ok := fakePS.draftByPolicy[pid]
	if !ok {
		t.Fatal("expected an initial draft version to exist for the created policy")
	}
	if dv.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("initial version status: got %q want draft", dv.Status)
	}
	if dv.ID.String() != draftID {
		t.Fatalf("response draft id %q does not match stored draft %q", draftID, dv.ID)
	}

	// Audit: policy.created AND policy.draft_saved for the seeded draft.
	if len(cap.calls) != 2 {
		t.Fatalf("expected 2 audit events (created + draft_saved), got %d", len(cap.calls))
	}
	if got := cap.calls[0].event.Action; got != "policy.created" {
		t.Fatalf("first audit action: got %q want policy.created", got)
	}
	if got := cap.calls[1].event.Action; got != "policy.draft_saved" {
		t.Fatalf("second audit action: got %q want policy.draft_saved", got)
	}
	if got := cap.calls[1].event.Subject; got != "policy_version:"+draftID {
		t.Fatalf("draft_saved subject: got %q want policy_version:%s", got, draftID)
	}
}

// TestCreatePolicyDocumentType: document_type reaches the store and comes back on the response;
// PROCEDURE creates a procedure and an unset type creates a policy.
func TestCreatePolicyDocumentType(t *testing.T) {
	newHandler := func() (*grpcsvc.PolicyHandler, *fakePolicyStore, domain.Category) {
		t.Helper()
		em := audit.New(&capturePublisher{})
		fakePS := newFakePolicyStore()
		fakeGS := newFakeCategoryStore()
		fakeTS := newFakeTemplateStore()
		g, err := fakeGS.Create(context.Background(), domain.Category{Name: "IT", Slug: "it"})
		if err != nil {
			t.Fatalf("seed category: %v", err)
		}
		return grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em), fakePS, g
	}

	t.Run("procedure", func(t *testing.T) {
		h, fakePS, g := newHandler()
		resp, err := h.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
			HomeCategoryId: g.ID.String(),
			Title:          "Onboarding Procedure",
			OwnerUserId:    uuid.New().String(),
			DocumentType:   corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE,
		})
		if err != nil {
			t.Fatalf("CreatePolicy: %v", err)
		}
		if got := resp.GetPolicy().GetDocumentType(); got != corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE {
			t.Fatalf("response document_type: got %v want PROCEDURE", got)
		}
		pid, _ := uuid.Parse(resp.GetPolicy().GetId())
		if got := fakePS.policies[pid].DocumentType; got != domain.DocumentTypeProcedure {
			t.Fatalf("persisted document_type: got %q want %q", got, domain.DocumentTypeProcedure)
		}
	})

	t.Run("unset defaults to policy", func(t *testing.T) {
		h, fakePS, g := newHandler()
		resp, err := h.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
			HomeCategoryId: g.ID.String(),
			Title:          "Security Policy",
			OwnerUserId:    uuid.New().String(),
			// DocumentType intentionally unset (UNSPECIFIED).
		})
		if err != nil {
			t.Fatalf("CreatePolicy: %v", err)
		}
		if got := resp.GetPolicy().GetDocumentType(); got != corev1.DocumentType_DOCUMENT_TYPE_POLICY {
			t.Fatalf("response document_type: got %v want POLICY", got)
		}
		pid, _ := uuid.Parse(resp.GetPolicy().GetId())
		if got := fakePS.policies[pid].DocumentType; got != domain.DocumentTypePolicy {
			t.Fatalf("persisted document_type: got %q want %q", got, domain.DocumentTypePolicy)
		}
	})
}

// TestPublishDraftEmitsAuditEvent verifies that the PublishDraft handler emits
// a single audit event with action="policy.published" on success, and that the
// emitter's routing key is "audit.audit" (tier=audit).
func TestPublishDraftEmitsAuditEvent(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	// Pre-seed a policy and an in-progress draft so PublishDraft has something
	// to promote.
	p, err := domain.NewPolicy("Test Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	fakePS.draftByPolicy[p.ID] = draftPV

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	actor := uuid.New().String()
	_, err = h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	got := cap.calls[0]
	if got.routingKey != "audit.audit" {
		t.Fatalf("routing key: got %q, want %q", got.routingKey, "audit.audit")
	}
	if got.event.Action != "policy.published" {
		t.Fatalf("action: got %q, want %q", got.event.Action, "policy.published")
	}
	if got.event.ActorUserID != actor {
		t.Fatalf("actor: got %q, want %q", got.event.ActorUserID, actor)
	}
}

// TestDiscardDraftDeletesDraftAndEmitsAudit: the draft goes, policy.draft_discarded is audited, and
// the policy itself stays.
func TestDiscardDraftDeletesDraftAndEmitsAudit(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	p, err := domain.NewPolicy("Discard Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	fakePS.draftByPolicy[p.ID] = draftPV
	fakePS.versions[draftPV.ID] = draftPV

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	actor := uuid.New().String()
	if _, err := h.DiscardDraft(context.Background(), &corev1.DiscardDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: actor,
	}); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}

	// Draft gone, policy survives.
	if _, ok := fakePS.draftByPolicy[p.ID]; ok {
		t.Fatal("expected draft removed")
	}
	if _, ok := fakePS.policies[p.ID]; !ok {
		t.Fatal("policy must NOT be deleted by discard draft")
	}

	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	if got := cap.calls[0].event.Action; got != "policy.draft_discarded" {
		t.Fatalf("action: got %q, want %q", got, "policy.draft_discarded")
	}
	if got := cap.calls[0].event.ActorUserID; got != actor {
		t.Fatalf("actor: got %q, want %q", got, actor)
	}
}

// TestDiscardDraftNoDraftReturnsNotFound verifies discarding when there is no
// draft surfaces NotFound (not Internal), and emits no audit event.
func TestDiscardDraftNoDraftReturnsNotFound(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	p, err := domain.NewPolicy("No Draft Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	_, err = h.DiscardDraft(context.Background(), &corev1.DiscardDraftRequest{PolicyId: p.ID.String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v (code %s)", err, status.Code(err))
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected no audit event on failure, got %d", len(cap.calls))
	}
}

// TestDeletePolicyHardDeletesAndEmitsAudit verifies the delete-policy handler
// hard-deletes a never-published policy, returns Deleted=true, and emits a
// policy.deleted audit event.
func TestDeletePolicyHardDeletesAndEmitsAudit(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	p, err := domain.NewPolicy("Delete Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	fakePS.draftByPolicy[p.ID] = draftPV
	fakePS.versions[draftPV.ID] = draftPV

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	actor := uuid.New().String()
	resp, err := h.DeletePolicy(context.Background(), &corev1.DeletePolicyRequest{
		PolicyId: p.ID.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}
	if !resp.GetDeleted() {
		t.Fatal("expected Deleted=true")
	}

	// Policy and its draft are gone.
	if _, ok := fakePS.policies[p.ID]; ok {
		t.Fatal("expected policy removed")
	}
	if _, ok := fakePS.draftByPolicy[p.ID]; ok {
		t.Fatal("expected draft removed")
	}

	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	if got := cap.calls[0].event.Action; got != "policy.deleted" {
		t.Fatalf("action: got %q, want %q", got, "policy.deleted")
	}
	if got := cap.calls[0].event.ActorUserID; got != actor {
		t.Fatalf("actor: got %q, want %q", got, actor)
	}
}

// TestDeletePolicyWithPublishedReturnsFailedPrecondition verifies the handler
// maps store.ErrHasPublishedVersion to FailedPrecondition and emits no audit.
func TestDeletePolicyWithPublishedReturnsFailedPrecondition(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	p, err := domain.NewPolicy("Published Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	fakePS.deleteErr = store.ErrHasPublishedVersion

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	_, err = h.DeletePolicy(context.Background(), &corev1.DeletePolicyRequest{PolicyId: p.ID.String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (code %s)", err, status.Code(err))
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected no audit event on failure, got %d", len(cap.calls))
	}
}

// TestDeletePolicyBadIDReturnsInvalidArgument verifies a malformed policy_id
// yields InvalidArgument before the store is touched.
func TestDeletePolicyBadIDReturnsInvalidArgument(t *testing.T) {
	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	_, err := h.DeletePolicy(context.Background(), &corev1.DeletePolicyRequest{PolicyId: "not-a-uuid"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v (code %s)", err, status.Code(err))
	}
}

// TestRetirePolicyStampsEmitsAuditAndLifecycle verifies the retire-policy
// handler stamps retired_at (surfaced on the returned proto), emits a
// policy.retired audit event, and emits a policy.retired lifecycle event on the
// jobs exchange carrying the policy id/number/title.
func TestRetirePolicyStampsEmitsAuditAndLifecycle(t *testing.T) {
	auditCap := &capturePublisher{}
	em := audit.New(auditCap)
	lifeCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(lifeCap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	homeGID := uuid.New()
	p, err := domain.NewPolicy("Retire Me", homeGID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.Number = "POL-HR-000007"
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
		WithLifecycleEmitter(lifeEm)
	actor := uuid.New().String()
	resp, err := h.RetirePolicy(context.Background(), &corev1.RetirePolicyRequest{
		PolicyId: p.ID.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("RetirePolicy: %v", err)
	}

	// Store stamped retired_at.
	if got := fakePS.policies[p.ID].RetiredAt; got == nil {
		t.Fatal("expected store retired_at stamped")
	}
	// Returned proto carries a non-empty RFC3339 retired_at.
	if resp.GetPolicy().GetRetiredAt() == "" {
		t.Fatal("expected proto retired_at to be set")
	}
	if _, perr := time.Parse(time.RFC3339, resp.GetPolicy().GetRetiredAt()); perr != nil {
		t.Fatalf("proto retired_at not RFC3339: %v", perr)
	}

	// Audit: exactly one policy.retired event with the actor + subject.
	if len(auditCap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(auditCap.calls))
	}
	if got := auditCap.calls[0].event.Action; got != "policy.retired" {
		t.Fatalf("action: got %q want policy.retired", got)
	}
	if got := auditCap.calls[0].event.ActorUserID; got != actor {
		t.Fatalf("actor: got %q want %q", got, actor)
	}
	if got, want := auditCap.calls[0].event.Subject, "policy:"+p.ID.String(); got != want {
		t.Fatalf("subject: got %q want %q", got, want)
	}

	// Lifecycle: exactly one policy.retired event carrying id/number/title.
	if len(lifeCap.calls) != 1 {
		t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
	}
	if got, want := lifeCap.calls[0].routingKey, "policy.retired"; got != want {
		t.Fatalf("lifecycle routing key: got %q want %q", got, want)
	}
	var evt lifecycle.PolicyRetiredEvent
	if uerr := json.Unmarshal(lifeCap.calls[0].body, &evt); uerr != nil {
		t.Fatalf("unmarshal lifecycle body: %v", uerr)
	}
	if evt.EventType != lifecycle.EventTypeRetired {
		t.Fatalf("event_type: got %q want %q", evt.EventType, lifecycle.EventTypeRetired)
	}
	if evt.PolicyID != p.ID.String() {
		t.Fatalf("policy_id: got %q want %q", evt.PolicyID, p.ID)
	}
	if evt.Number != "POL-HR-000007" {
		t.Fatalf("number: got %q want POL-HR-000007", evt.Number)
	}
	if evt.Title != "Retire Me" {
		t.Fatalf("title: got %q want Retire Me", evt.Title)
	}
	if evt.RetiredAt.IsZero() {
		t.Fatal("expected non-zero retired_at in lifecycle event")
	}
}

// TestPublishDraftRoutesByDocumentType: publishing a PROCEDURE emits procedure.published with
// DocumentType=PROCEDURE, never policy.published, which keeps procedures out of the acknowledgement
// fan-out; a POLICY still emits policy.published with DocumentType=POLICY.
func TestPublishDraftRoutesByDocumentType(t *testing.T) {
	publish := func(t *testing.T, docType domain.DocumentType) (routingKey, wireDocType, eventType string) {
		t.Helper()
		auditCap := &capturePublisher{}
		em := audit.New(auditCap)
		lifeCap := &lifecycleCapture{}
		lifeEm := lifecycle.New(lifeCap)

		fakeGS := newFakeCategoryStore()
		fakeTS := &fakeTemplateStore{}
		fakePS := newFakePolicyStore()

		homeGID := uuid.New()
		if _, err := fakeGS.Create(context.Background(), domain.Category{
			ID: homeGID, Name: "HR", Slug: "hr",
		}); err != nil {
			t.Fatalf("seed category: %v", err)
		}

		p, err := domain.NewPolicy("Onboarding", homeGID, domain.SensitivityStandard, uuid.New())
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		p.DocumentType = docType
		fakePS.policies[p.ID] = p

		draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(),
			`{"sections":{"purpose":"Define the program."}}`)
		if err != nil {
			t.Fatalf("NewPolicyVersionDraft: %v", err)
		}
		fakePS.draftByPolicy[p.ID] = draftPV

		h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
			WithLifecycleEmitter(lifeEm)

		if _, err := h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{
			PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
		}); err != nil {
			t.Fatalf("PublishDraft: %v", err)
		}
		if len(lifeCap.calls) != 1 {
			t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
		}
		var evt map[string]any
		if err := json.Unmarshal(lifeCap.calls[0].body, &evt); err != nil {
			t.Fatalf("unmarshal lifecycle body: %v", err)
		}
		et, _ := evt["event_type"].(string)
		v, ok := evt["version"].(map[string]any)
		if !ok {
			t.Fatalf("version object missing: %+v", evt)
		}
		dt, _ := v["DocumentType"].(string)
		return lifeCap.calls[0].routingKey, dt, et
	}

	t.Run("procedure emits procedure.published", func(t *testing.T) {
		rk, dt, et := publish(t, domain.DocumentTypeProcedure)
		if rk != "procedure.published" {
			t.Fatalf("routing key: got %q want procedure.published", rk)
		}
		if et != "procedure.published" {
			t.Fatalf("event_type: got %q want procedure.published", et)
		}
		if dt != "PROCEDURE" {
			t.Fatalf("DocumentType: got %q want PROCEDURE", dt)
		}
	})

	t.Run("policy still emits policy.published", func(t *testing.T) {
		rk, dt, et := publish(t, domain.DocumentTypePolicy)
		if rk != "policy.published" {
			t.Fatalf("routing key: got %q want policy.published", rk)
		}
		if et != "policy.published" {
			t.Fatalf("event_type: got %q want policy.published", et)
		}
		if dt != "POLICY" {
			t.Fatalf("DocumentType: got %q want POLICY", dt)
		}
	})
}

// TestRetirePolicyRoutesByDocumentType: retiring a PROCEDURE emits procedure.retired, a POLICY
// policy.retired.
func TestRetirePolicyRoutesByDocumentType(t *testing.T) {
	retire := func(t *testing.T, docType domain.DocumentType) (routingKey string, eventType lifecycle.EventType) {
		t.Helper()
		auditCap := &capturePublisher{}
		em := audit.New(auditCap)
		lifeCap := &lifecycleCapture{}
		lifeEm := lifecycle.New(lifeCap)

		fakeGS := newFakeCategoryStore()
		fakeTS := &fakeTemplateStore{}
		fakePS := newFakePolicyStore()

		p, err := domain.NewPolicy("Retire Me", uuid.New(), domain.SensitivityStandard, uuid.New())
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		p.DocumentType = docType
		p.Number = "PRC-HR-000009"
		fakePS.policies[p.ID] = p

		h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
			WithLifecycleEmitter(lifeEm)

		if _, err := h.RetirePolicy(context.Background(), &corev1.RetirePolicyRequest{
			PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
		}); err != nil {
			t.Fatalf("RetirePolicy: %v", err)
		}
		if len(lifeCap.calls) != 1 {
			t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
		}
		var evt lifecycle.PolicyRetiredEvent
		if uerr := json.Unmarshal(lifeCap.calls[0].body, &evt); uerr != nil {
			t.Fatalf("unmarshal lifecycle body: %v", uerr)
		}
		return lifeCap.calls[0].routingKey, evt.EventType
	}

	t.Run("procedure emits procedure.retired", func(t *testing.T) {
		rk, et := retire(t, domain.DocumentTypeProcedure)
		if rk != "procedure.retired" {
			t.Fatalf("routing key: got %q want procedure.retired", rk)
		}
		if et != lifecycle.EventTypeProcedureRetired {
			t.Fatalf("event_type: got %q want %q", et, lifecycle.EventTypeProcedureRetired)
		}
	})

	t.Run("policy still emits policy.retired", func(t *testing.T) {
		rk, et := retire(t, domain.DocumentTypePolicy)
		if rk != "policy.retired" {
			t.Fatalf("routing key: got %q want policy.retired", rk)
		}
		if et != lifecycle.EventTypeRetired {
			t.Fatalf("event_type: got %q want %q", et, lifecycle.EventTypeRetired)
		}
	})
}

// TestRetirePolicyBadIDReturnsInvalidArgument verifies a malformed policy_id
// yields InvalidArgument before the store is touched.
func TestRetirePolicyBadIDReturnsInvalidArgument(t *testing.T) {
	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	_, err := h.RetirePolicy(context.Background(), &corev1.RetirePolicyRequest{PolicyId: "not-a-uuid"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v (code %s)", err, status.Code(err))
	}
}

// TestRetirePolicyUnknownIDReturnsNotFound verifies an unknown policy_id maps to
// NotFound (the store's pgx.ErrNoRows convention).
func TestRetirePolicyUnknownIDReturnsNotFound(t *testing.T) {
	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	_, err := h.RetirePolicy(context.Background(), &corev1.RetirePolicyRequest{PolicyId: uuid.New().String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v (code %s)", err, status.Code(err))
	}
}

// TestPublishDraftEmitsLifecycleEventOnJobsExchange: with a lifecycle emitter wired, PublishDraft
// also publishes policy.published on the jobs exchange the AI re-index consumer binds to.
func TestPublishDraftEmitsLifecycleEventOnJobsExchange(t *testing.T) {
	auditCap := &capturePublisher{}
	em := audit.New(auditCap)
	lifeCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(lifeCap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	// Seed a category so the handler can resolve the policy's home category_id.
	homeGID := uuid.New()
	if _, err := fakeGS.Create(context.Background(), domain.Category{
		ID: homeGID, Name: "HR", Slug: "hr",
	}); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	// Seed a sensitive policy + draft so the published event carries the
	// access-control metadata AI's chunk filter relies on.
	p, err := domain.NewPolicy("Test Policy", homeGID, domain.SensitivitySensitive, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(),
		`{"sections":{"purpose":"Define the program.","scope":"All staff."}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	fakePS.draftByPolicy[p.ID] = draftPV

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
		WithLifecycleEmitter(lifeEm)

	if _, err := h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	// Audit emission must still happen — we are *adding* a publish, not
	// replacing the audit one.
	if len(auditCap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(auditCap.calls))
	}
	if got := auditCap.calls[0].routingKey; got != "audit.audit" {
		t.Fatalf("audit routing key: got %q", got)
	}

	// Lifecycle emission: exactly one, on the "policy.published" routing key.
	if len(lifeCap.calls) != 1 {
		t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
	}
	if got, want := lifeCap.calls[0].routingKey, "policy.published"; got != want {
		t.Fatalf("lifecycle routing key: got %q want %q", got, want)
	}

	// Decode into a loose map and assert wire-key shape matches what AI's
	// consumer expects to unmarshal (PolicyVersionContent has no json tags
	// so keys are the uppercase Go field names).
	var evt map[string]any
	if err := json.Unmarshal(lifeCap.calls[0].body, &evt); err != nil {
		t.Fatalf("unmarshal lifecycle body: %v", err)
	}
	if evt["event_type"] != "policy.published" {
		t.Fatalf("event_type: %v", evt["event_type"])
	}
	v, ok := evt["version"].(map[string]any)
	if !ok {
		t.Fatalf("version object missing: %+v", evt)
	}
	if v["PolicyID"] != p.ID.String() {
		t.Fatalf("PolicyID: got %v want %s", v["PolicyID"], p.ID)
	}
	if v["CategoryID"] != homeGID.String() {
		t.Fatalf("CategoryID: got %v want %s", v["CategoryID"], homeGID)
	}
	if v["Sensitivity"] != "sensitive" {
		t.Fatalf("Sensitivity: got %v want sensitive", v["Sensitivity"])
	}
	if v["PolicyTitle"] != "Test Policy" {
		t.Fatalf("PolicyTitle: got %v", v["PolicyTitle"])
	}
	// VersionNo round-trips through JSON as float64.
	if got, want := v["VersionNo"], float64(1); got != want {
		t.Fatalf("VersionNo: got %v want %v", got, want)
	}
	sections, ok := v["Sections"].([]any)
	if !ok || len(sections) != 2 {
		t.Fatalf("Sections: %v", v["Sections"])
	}
}

// TestPublishDraftLifecycleEventCarriesEffectiveDate: policy.published carries the policy's own
// effective date when it has one, so obligations can show it, and leaves the key out when it has
// none (obligations then shows the publish date).
func TestPublishDraftLifecycleEventCarriesEffectiveDate(t *testing.T) {
	publishAndDecodeVersion := func(t *testing.T, effectiveDate *time.Time) map[string]any {
		t.Helper()
		auditCap := &capturePublisher{}
		em := audit.New(auditCap)
		lifeCap := &lifecycleCapture{}
		lifeEm := lifecycle.New(lifeCap)

		fakeGS := newFakeCategoryStore()
		fakeTS := &fakeTemplateStore{}
		fakePS := newFakePolicyStore()

		homeGID := uuid.New()
		if _, err := fakeGS.Create(context.Background(), domain.Category{
			ID: homeGID, Name: "HR", Slug: "hr",
		}); err != nil {
			t.Fatalf("seed category: %v", err)
		}

		p, err := domain.NewPolicy("Test Policy", homeGID, domain.SensitivityStandard, uuid.New())
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		// The store's GetPolicy populates Policy.EffectiveDate from the
		// effective_date column; the fake returns whatever we seed here.
		p.EffectiveDate = effectiveDate
		fakePS.policies[p.ID] = p

		draftPV, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(),
			`{"sections":{"purpose":"Define the program."}}`)
		if err != nil {
			t.Fatalf("NewPolicyVersionDraft: %v", err)
		}
		fakePS.draftByPolicy[p.ID] = draftPV

		h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
			WithLifecycleEmitter(lifeEm)

		if _, err := h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{
			PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
		}); err != nil {
			t.Fatalf("PublishDraft: %v", err)
		}
		if len(lifeCap.calls) != 1 {
			t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
		}
		var evt map[string]any
		if err := json.Unmarshal(lifeCap.calls[0].body, &evt); err != nil {
			t.Fatalf("unmarshal lifecycle body: %v", err)
		}
		v, ok := evt["version"].(map[string]any)
		if !ok {
			t.Fatalf("version object missing: %+v", evt)
		}
		return v
	}

	t.Run("carries the real effective date when set", func(t *testing.T) {
		ed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
		v := publishAndDecodeVersion(t, &ed)
		raw, ok := v["EffectiveDate"].(string)
		if !ok || raw == "" {
			t.Fatalf("EffectiveDate: got %v, want an RFC3339 date string", v["EffectiveDate"])
		}
		got, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatalf("EffectiveDate not RFC3339: %q (%v)", raw, err)
		}
		if !got.Equal(ed) {
			t.Fatalf("EffectiveDate: got %s, want %s", got, ed)
		}
	})

	t.Run("omits the effective date when unset", func(t *testing.T) {
		v := publishAndDecodeVersion(t, nil)
		if raw, present := v["EffectiveDate"]; present {
			t.Fatalf("EffectiveDate must be absent when the policy has none, got %v", raw)
		}
	})
}

// TestPublishDraftLifecycleEmitFailureDoesNotFailRPC: a jobs-exchange outage doesn't break the
// publish; the audit event still goes out and the RPC succeeds.
func TestPublishDraftLifecycleEmitFailureDoesNotFailRPC(t *testing.T) {
	auditCap := &capturePublisher{}
	em := audit.New(auditCap)
	// failingPublisher always errors. The handler should swallow.
	lifeEm := lifecycle.New(failingPublisher{})

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	homeGID := uuid.New()
	_, _ = fakeGS.Create(context.Background(), domain.Category{ID: homeGID, Slug: "hr"})
	p, _ := domain.NewPolicy("P", homeGID, domain.SensitivityStandard, uuid.New())
	fakePS.policies[p.ID] = p
	d, _ := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(), `{"sections":{}}`)
	fakePS.draftByPolicy[p.ID] = d

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em).
		WithLifecycleEmitter(lifeEm)

	if _, err := h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("PublishDraft must succeed even when lifecycle emit fails, got %v", err)
	}
	if len(auditCap.calls) != 1 {
		t.Fatalf("expected audit event still emitted, got %d", len(auditCap.calls))
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, _ string, _ []byte) error {
	return fmt.Errorf("simulated broker down")
}

func (f *fakePolicyStore) SetAck(_ context.Context, policyID uuid.UUID, ack *domain.AckTrigger, override []uuid.UUID) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, fmt.Errorf("not found")
	}
	p.AckTriggers = ack
	p.AckAudienceOverride = override
	f.policies[policyID] = p
	return p, nil
}

func (f *fakePolicyStore) SetTemplate(_ context.Context, policyID, templateID uuid.UUID, templateNone bool) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, fmt.Errorf("not found")
	}
	if templateNone {
		p.TemplateID = uuid.Nil
		p.TemplateNone = true
	} else {
		p.TemplateID = templateID
		p.TemplateNone = false
	}
	f.policies[policyID] = p
	return p, nil
}

func (f *fakePolicyStore) ListAllPolicies(_ context.Context) ([]domain.Policy, error) {
	out := make([]domain.Policy, 0, len(f.policies))
	for _, p := range f.policies {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakePolicyStore) ListPoliciesByOwner(_ context.Context, ownerUserID uuid.UUID, includeRetired bool) ([]domain.Policy, error) {
	out := make([]domain.Policy, 0)
	for _, p := range f.policies {
		if p.OwnerUserID != ownerUserID {
			continue
		}
		if !includeRetired && p.RetiredAt != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakePolicyStore) ReassignUserPolicies(_ context.Context, fromUserID, toUserID uuid.UUID) ([]uuid.UUID, int, int, error) {
	var ids []uuid.UUID
	for id, p := range f.policies {
		if p.OwnerUserID == fromUserID {
			p.OwnerUserID = toUserID
			f.policies[id] = p
			ids = append(ids, id)
		}
	}
	return ids, len(ids), f.reassignAuthorGrants, nil
}

func (f *fakePolicyStore) SetOwner(_ context.Context, policyID, ownerUserID uuid.UUID) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	p.OwnerUserID = ownerUserID
	f.policies[policyID] = p
	return p, nil
}

func (f *fakePolicyStore) SetSensitivity(_ context.Context, policyID uuid.UUID, sensitivity domain.Sensitivity) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	p.Sensitivity = sensitivity
	f.policies[policyID] = p
	return p, nil
}

func (f *fakePolicyStore) SetHomeCategory(_ context.Context, policyID, targetCategoryID uuid.UUID) (domain.Policy, error) {
	p, ok := f.policies[policyID]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	p.HomeCategoryID = targetCategoryID
	// The real store renumbers from the target category's code, tested in the store tests; a
	// constant is enough here.
	p.Number = "POL-MOVED-000001"
	f.policies[policyID] = p
	return p, nil
}

// TestSetPolicyOwner verifies the happy path: owner is updated, returned, and a
// policy.owner_changed audit event is emitted with the actor.
func TestSetPolicyOwner(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	gid := uuid.New()
	pid := uuid.New()
	fakePS.policies[pid] = domain.Policy{ID: pid, HomeCategoryID: gid, OwnerUserID: uuid.New()}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	owner := uuid.New()
	actor := uuid.New().String()
	resp, err := h.SetPolicyOwner(context.Background(), &corev1.SetPolicyOwnerRequest{
		PolicyId: pid.String(), OwnerUserId: owner.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("SetPolicyOwner: %v", err)
	}
	if resp.GetPolicy().GetOwnerUserId() != owner.String() {
		t.Fatalf("owner not returned: got %q", resp.GetPolicy().GetOwnerUserId())
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.owner_changed" {
		t.Fatalf("audit action: %+v", cap.calls)
	}
	if cap.calls[0].event.ActorUserID != actor {
		t.Fatalf("audit actor: got %q", cap.calls[0].event.ActorUserID)
	}
}

// TestListPoliciesByOwner verifies the owned set is returned and the retired filter is honoured.
func TestListPoliciesByOwner(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	owner := uuid.New()
	other := uuid.New()
	gid := uuid.New()
	active := uuid.New()
	retired := uuid.New()
	notOwned := uuid.New()
	retiredAt := time.Now()
	fakePS.policies[active] = domain.Policy{ID: active, HomeCategoryID: gid, OwnerUserID: owner}
	fakePS.policies[retired] = domain.Policy{ID: retired, HomeCategoryID: gid, OwnerUserID: owner, RetiredAt: &retiredAt}
	fakePS.policies[notOwned] = domain.Policy{ID: notOwned, HomeCategoryID: gid, OwnerUserID: other}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)

	resp, err := h.ListPoliciesByOwner(context.Background(), &corev1.ListPoliciesByOwnerRequest{OwnerUserId: owner.String()})
	if err != nil {
		t.Fatalf("ListPoliciesByOwner: %v", err)
	}
	if len(resp.GetPolicies()) != 1 || resp.GetPolicies()[0].GetId() != active.String() {
		t.Fatalf("active-only: got %d policies %+v", len(resp.GetPolicies()), resp.GetPolicies())
	}

	respAll, err := h.ListPoliciesByOwner(context.Background(), &corev1.ListPoliciesByOwnerRequest{OwnerUserId: owner.String(), IncludeRetired: true})
	if err != nil {
		t.Fatalf("ListPoliciesByOwner include_retired: %v", err)
	}
	if len(respAll.GetPolicies()) != 2 {
		t.Fatalf("include_retired: got %d policies", len(respAll.GetPolicies()))
	}

	if _, err := h.ListPoliciesByOwner(context.Background(), &corev1.ListPoliciesByOwnerRequest{OwnerUserId: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad uuid: want InvalidArgument, got %v", err)
	}
}

// TestReassignUserPolicies verifies ownership is moved, counts and ids are returned, a
// policy.owner_changed event is emitted per policy plus a summary category.ruleset_changed for the
// author-grant sweep, and from==to is rejected.
func TestReassignUserPolicies(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	from := uuid.New()
	to := uuid.New()
	gid := uuid.New()
	p1 := uuid.New()
	p2 := uuid.New()
	fakePS.policies[p1] = domain.Policy{ID: p1, HomeCategoryID: gid, OwnerUserID: from}
	fakePS.policies[p2] = domain.Policy{ID: p2, HomeCategoryID: gid, OwnerUserID: from}
	fakePS.reassignAuthorGrants = 3

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	actor := uuid.New().String()
	resp, err := h.ReassignUserPolicies(context.Background(), &corev1.ReassignUserPoliciesRequest{
		FromUserId: from.String(), ToUserId: to.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("ReassignUserPolicies: %v", err)
	}
	if resp.GetReassignedOwnerCount() != 2 || len(resp.GetReassignedPolicyIds()) != 2 {
		t.Fatalf("owner count/ids: %+v", resp)
	}
	if resp.GetReassignedAuthorGrants() != 3 {
		t.Fatalf("author grants: got %d", resp.GetReassignedAuthorGrants())
	}
	if fakePS.policies[p1].OwnerUserID != to || fakePS.policies[p2].OwnerUserID != to {
		t.Fatalf("owner not moved to target")
	}
	// 2 policy.owner_changed + 1 category.ruleset_changed summary.
	var ownerChanged, rulesetChanged int
	for _, c := range cap.calls {
		switch c.event.Action {
		case "policy.owner_changed":
			ownerChanged++
			if c.event.ActorUserID != actor {
				t.Fatalf("owner_changed actor: got %q", c.event.ActorUserID)
			}
		case "category.ruleset_changed":
			rulesetChanged++
		}
	}
	if ownerChanged != 2 || rulesetChanged != 1 {
		t.Fatalf("audit events: owner=%d ruleset=%d", ownerChanged, rulesetChanged)
	}

	if _, err := h.ReassignUserPolicies(context.Background(), &corev1.ReassignUserPoliciesRequest{
		FromUserId: from.String(), ToUserId: from.String(),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("from==to: want InvalidArgument, got %v", err)
	}
}

// TestSetPolicySensitivity verifies the happy path: the classification is
// flipped in place, returned on the policy, and a policy.sensitivity_changed
// audit event is emitted carrying the actor and the old->new value. It also
// confirms the reverse direction (sensitive->standard) works.
func TestSetPolicySensitivity(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	gid := uuid.New()
	pid := uuid.New()
	fakePS.policies[pid] = domain.Policy{ID: pid, HomeCategoryID: gid, Sensitivity: domain.SensitivityStandard}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	// standard -> sensitive
	actor := uuid.New().String()
	resp, err := h.SetPolicySensitivity(context.Background(), &corev1.SetPolicySensitivityRequest{
		PolicyId: pid.String(), Sensitivity: corev1.Sensitivity_SENSITIVITY_SENSITIVE, ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("SetPolicySensitivity: %v", err)
	}
	if resp.GetPolicy().GetSensitivity() != corev1.Sensitivity_SENSITIVITY_SENSITIVE {
		t.Fatalf("sensitivity not returned: got %v", resp.GetPolicy().GetSensitivity())
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.sensitivity_changed" {
		t.Fatalf("audit action: %+v", cap.calls)
	}
	if cap.calls[0].event.ActorUserID != actor {
		t.Fatalf("audit actor: got %q", cap.calls[0].event.ActorUserID)
	}
	if got := cap.calls[0].event.Attributes["from_sensitivity"]; got != "standard" {
		t.Fatalf("from_sensitivity: got %q", got)
	}
	if got := cap.calls[0].event.Attributes["to_sensitivity"]; got != "sensitive" {
		t.Fatalf("to_sensitivity: got %q", got)
	}

	// sensitive -> standard (reverse direction)
	resp, err = h.SetPolicySensitivity(context.Background(), &corev1.SetPolicySensitivityRequest{
		PolicyId: pid.String(), Sensitivity: corev1.Sensitivity_SENSITIVITY_STANDARD, ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("SetPolicySensitivity reverse: %v", err)
	}
	if resp.GetPolicy().GetSensitivity() != corev1.Sensitivity_SENSITIVITY_STANDARD {
		t.Fatalf("reverse sensitivity: got %v", resp.GetPolicy().GetSensitivity())
	}
	if len(cap.calls) != 2 || cap.calls[1].event.Attributes["from_sensitivity"] != "sensitive" ||
		cap.calls[1].event.Attributes["to_sensitivity"] != "standard" {
		t.Fatalf("reverse audit: %+v", cap.calls[1].event)
	}
}

// TestMovePolicyRenumbersAndAudits verifies the move renumbers the policy under
// the target category and emits a policy.moved audit event.
func TestMovePolicyRenumbersAndAudits(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	src := uuid.New()
	dst := uuid.New()
	fakeGS.categories[dst] = domain.Category{ID: dst, Slug: "fin"}
	pid := uuid.New()
	fakePS.policies[pid] = domain.Policy{ID: pid, HomeCategoryID: src, Number: "POL-IT-000003"}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	resp, err := h.MovePolicy(context.Background(), &corev1.MovePolicyRequest{
		PolicyId: pid.String(), HomeCategoryId: dst.String(), ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("MovePolicy: %v", err)
	}
	if resp.GetPolicy().GetNumber() != "POL-MOVED-000001" {
		t.Fatalf("number not renumbered: got %q", resp.GetPolicy().GetNumber())
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.moved" {
		t.Fatalf("audit action: %+v", cap.calls)
	}
}

// TestMovePolicyNoOpSameCategory verifies moving to the current home category is a
// no-op: unchanged policy, no audit.
func TestMovePolicyNoOpSameCategory(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	gid := uuid.New()
	pid := uuid.New()
	fakePS.policies[pid] = domain.Policy{ID: pid, HomeCategoryID: gid, Number: "POL-IT-000003"}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	resp, err := h.MovePolicy(context.Background(), &corev1.MovePolicyRequest{
		PolicyId: pid.String(), HomeCategoryId: gid.String(), ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("MovePolicy no-op: %v", err)
	}
	if resp.GetPolicy().GetNumber() != "POL-IT-000003" {
		t.Fatalf("number changed on no-op: got %q", resp.GetPolicy().GetNumber())
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected no audit on no-op, got %+v", cap.calls)
	}
}

// TestMovePolicyBadPolicyID verifies id validation.
func TestMovePolicyBadPolicyID(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	_, err := h.MovePolicy(context.Background(), &corev1.MovePolicyRequest{
		PolicyId: "nope", HomeCategoryId: uuid.New().String(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// TestSetAckTriggersSet verifies that ack_triggers_set=true with ON_PUBLISH persists
// and the returned proto reflects ack_triggers_set=true + correct enum.
func TestSetAckTriggersSet(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	// Seed a policy.
	homeGID := uuid.New()
	_, _ = fakeGS.Create(context.Background(), domain.Category{ID: homeGID, Name: "HR", Slug: "hr"})
	p, err := domain.NewPolicy("Test Policy", homeGID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	resp, err := h.SetAck(context.Background(), &corev1.SetAckRequest{
		PolicyId:       p.ID.String(),
		AckTriggers:    corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH,
		AckTriggersSet: true,
	})
	if err != nil {
		t.Fatalf("SetAck: %v", err)
	}
	if !resp.Policy.AckTriggersSet {
		t.Fatalf("expected ack_triggers_set=true, got false")
	}
	if resp.Policy.AckTriggers != corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH {
		t.Fatalf("ack_triggers: got %v want ON_PUBLISH", resp.Policy.AckTriggers)
	}
	// Audit event.
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	if cap.calls[0].event.Action != "policy.ack_config_updated" {
		t.Fatalf("audit action: got %q", cap.calls[0].event.Action)
	}
}

// TestSetAckTriggersNotSet verifies that ack_triggers_set=false clears the
// policy-level override (nil AckTriggers) and the returned proto has
// ack_triggers_set=false.
func TestSetAckTriggersNotSet(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	homeGID := uuid.New()
	_, _ = fakeGS.Create(context.Background(), domain.Category{ID: homeGID, Name: "HR", Slug: "hr"})
	p, _ := domain.NewPolicy("Test Policy", homeGID, domain.SensitivityStandard, uuid.New())
	// Pre-set an ack trigger so we can verify it gets cleared.
	trig := domain.AckOnChange
	p.AckTriggers = &trig
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.SetAck(context.Background(), &corev1.SetAckRequest{
		PolicyId:       p.ID.String(),
		AckTriggers:    corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE, // value ignored when not set
		AckTriggersSet: false,
	})
	if err != nil {
		t.Fatalf("SetAck: %v", err)
	}
	if resp.Policy.AckTriggersSet {
		t.Fatalf("expected ack_triggers_set=false")
	}
	// Store should now have nil AckTriggers.
	if fakePS.policies[p.ID].AckTriggers != nil {
		t.Fatalf("expected nil AckTriggers in store, got %v", fakePS.policies[p.ID].AckTriggers)
	}
}

// seedPolicyWithEffectiveTemplate creates a category with a default template, a
// policy homed in it, and a published version pinned to a template version. The
// store's IsTemplateUpdateAvailable answer is controlled by the fake's
// templateUpdateAvailable field, so these tests exercise the GetPolicy wiring
// rather than the SQL comparison (which is covered by the store's own tests).
func seedPolicyWithEffectiveTemplate(t *testing.T, fakePS *fakePolicyStore, fakeGS *fakeCategoryStore) uuid.UUID {
	t.Helper()
	categoryID := uuid.New()
	fakeGS.categories[categoryID] = domain.Category{
		ID: categoryID, Name: "IT", Slug: "it", DefaultTemplateID: uuid.New(),
	}
	p, err := domain.NewPolicy("Pol", categoryID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	pubID := uuid.New()
	p.CurrentPublishedVersionID = pubID
	fakePS.policies[p.ID] = p
	fakePS.versions[pubID] = domain.PolicyVersion{
		ID: pubID, PolicyID: p.ID,
		Status: domain.PolicyVersionStatusPublished, VersionNo: 1,
	}
	return p.ID
}

// TestGetPolicyTemplateUpdateAvailableTrue verifies the GetPolicy detail read
// surfaces template_update_available=true when the store reports a newer
// published template version exists.
func TestGetPolicyTemplateUpdateAvailableTrue(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	fakePS.templateUpdateAvailable = true

	policyID := seedPolicyWithEffectiveTemplate(t, fakePS, fakeGS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.GetPolicy(context.Background(), &corev1.GetPolicyRequest{Id: policyID.String()})
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if !resp.GetPolicy().GetTemplateUpdateAvailable() {
		t.Fatalf("expected template_update_available=true")
	}
}

// TestGetPolicyTemplateUpdateAvailableFalse verifies the flag is false when the
// store reports the policy is on the latest template version.
func TestGetPolicyTemplateUpdateAvailableFalse(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	fakePS.templateUpdateAvailable = false

	policyID := seedPolicyWithEffectiveTemplate(t, fakePS, fakeGS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.GetPolicy(context.Background(), &corev1.GetPolicyRequest{Id: policyID.String()})
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if resp.GetPolicy().GetTemplateUpdateAvailable() {
		t.Fatalf("expected template_update_available=false")
	}
}

// TestGetPolicyTemplateUpdateAvailableNoPublishedVersion verifies that a policy
// with no published version never complains and never hits the store query.
func TestGetPolicyTemplateUpdateAvailableNoPublishedVersion(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	// Even if the store would say true, no published version => false.
	fakePS.templateUpdateAvailable = true

	categoryID := uuid.New()
	fakeGS.categories[categoryID] = domain.Category{ID: categoryID, Slug: "it", DefaultTemplateID: uuid.New()}
	p, _ := domain.NewPolicy("Pol", categoryID, domain.SensitivityStandard, uuid.New())
	// CurrentPublishedVersionID left as uuid.Nil (draft-only policy).
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.GetPolicy(context.Background(), &corev1.GetPolicyRequest{Id: p.ID.String()})
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if resp.GetPolicy().GetTemplateUpdateAvailable() {
		t.Fatalf("expected false when no published version")
	}
}

// seedObligationEnv builds:
//   - owning category (rootID) with AckTriggers=AckOnChange, AudienceGroupIDs=["AD-A"]
//   - child category (childID) with AudienceGroupIDs=["AD-B"]
//   - a policy homed in rootID with a published version
//
// Returns all the IDs so tests can drive against them.
func seedObligationEnv(t *testing.T, fakePS *fakePolicyStore, fakeGS *fakeCategoryStore) (policyID, rootID, childID, publishedVersionID uuid.UUID) {
	t.Helper()
	rootID = uuid.New()
	childID = uuid.New()

	rootADs := []string{"AD-A"}
	childADs := []string{"AD-B"}
	rootG := domain.Category{ID: rootID, Name: "Root", Slug: "root", AckTriggers: domain.AckOnChange, AudienceGroupIDs: &rootADs}
	childG := domain.Category{ID: childID, ParentID: rootID, Name: "Child", Slug: "child", AudienceGroupIDs: &childADs}
	fakeGS.categories[rootID] = rootG
	fakeGS.categories[childID] = childG

	p, err := domain.NewPolicy("Pol1", rootID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	publishedVersionID = uuid.New()
	p.CurrentPublishedVersionID = publishedVersionID
	p.Number = "root-1"
	fakePS.policies[p.ID] = p
	fakePS.versions[publishedVersionID] = domain.PolicyVersion{
		ID: publishedVersionID, PolicyID: p.ID,
		Status: domain.PolicyVersionStatusPublished, VersionNo: 1,
	}
	policyID = p.ID
	return
}

// TestResolvePolicyObligationOnChange verifies the category's on-change trigger and the audience
// groups of the root and child are collected; on_change=true when published.
func TestResolvePolicyObligationOnChange(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, _, _, publishedVersionID := seedObligationEnv(t, fakePS, fakeGS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: policyID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if !resp.RequiresAck {
		t.Fatalf("expected requires_ack=true")
	}
	if !resp.OnChange {
		t.Fatalf("expected on_change=true")
	}
	if resp.PublishedVersionId != publishedVersionID.String() {
		t.Fatalf("published_version_id: got %q want %q", resp.PublishedVersionId, publishedVersionID)
	}
	// Audience should contain AD-A and AD-B (from root and child subtree).
	adSet := map[string]bool{}
	for _, ad := range resp.AudienceGroups {
		adSet[ad] = true
	}
	if !adSet["AD-A"] || !adSet["AD-B"] {
		t.Fatalf("audience_groups: got %v want {AD-A, AD-B}", resp.AudienceGroups)
	}
}

// TestResolvePolicyObligationEveryoneInherited verifies the "Everyone" flag is
// resolved through the owning category's ancestor chain (nearest-ancestor-wins):
// a true value set on the root propagates to a policy homed in the child, and
// surfaces as everyone=true on the obligation.
func TestResolvePolicyObligationEveryoneInherited(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	rootID := uuid.New()
	childID := uuid.New()
	everyone := true
	// Root sets ack_everyone=true; the child (where the policy is homed) carries
	// the trigger. The everyone flag must be inherited from the root even though
	// the child does not set it.
	fakeGS.categories[rootID] = domain.Category{ID: rootID, Name: "Root", Slug: "root", AckEveryone: &everyone}
	fakeGS.categories[childID] = domain.Category{ID: childID, ParentID: rootID, Name: "Child", Slug: "child", AckTriggers: domain.AckOnChange}

	p, err := domain.NewPolicy("Pol", childID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.CurrentPublishedVersionID = uuid.New()
	p.Number = "child-1"
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: p.ID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if !resp.RequiresAck {
		t.Fatalf("expected requires_ack=true")
	}
	if !resp.Everyone {
		t.Fatalf("expected everyone=true (inherited from root)")
	}
}

// TestResolvePolicyObligationNoTrigger verifies that when neither category nor
// policy sets a trigger, the obligation is empty.
func TestResolvePolicyObligationNoTrigger(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	homeGID := uuid.New()
	// Category with no AckTriggers (default "").
	fakeGS.categories[homeGID] = domain.Category{ID: homeGID, Name: "Neutral", Slug: "neutral"}
	p, _ := domain.NewPolicy("P", homeGID, domain.SensitivityStandard, uuid.New())
	p.CurrentPublishedVersionID = uuid.New()
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: p.ID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if resp.RequiresAck {
		t.Fatalf("expected requires_ack=false for no-trigger category")
	}
}

// TestResolvePolicyObligationPolicyOverrideNone verifies that a policy-level
// override of AckNone beats the category's on-change setting.
func TestResolvePolicyObligationPolicyOverrideNone(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, _, _, _ := seedObligationEnv(t, fakePS, fakeGS)

	// Override the policy to set AckNone.
	p := fakePS.policies[policyID]
	none := domain.AckNone
	p.AckTriggers = &none
	fakePS.policies[policyID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: policyID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if resp.RequiresAck {
		t.Fatalf("expected requires_ack=false: policy override=none beats category on-change")
	}
}

// TestResolvePolicyObligationNotPublished verifies that an unpublished policy
// returns requires_ack=false (only live versions trigger obligations).
func TestResolvePolicyObligationNotPublished(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, _, _, _ := seedObligationEnv(t, fakePS, fakeGS)
	// Clear the published version ID.
	p := fakePS.policies[policyID]
	p.CurrentPublishedVersionID = uuid.Nil
	fakePS.policies[policyID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: policyID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if resp.RequiresAck {
		t.Fatalf("expected requires_ack=false for unpublished policy")
	}
}

// TestResolvePolicyObligationAckAudienceOverride verifies that with AckAudienceOverride set only
// that category's audience groups are used, not the subtree.
func TestResolvePolicyObligationAckAudienceOverride(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, rootID, _, _ := seedObligationEnv(t, fakePS, fakeGS)

	// Set audience override to the root category only.
	p := fakePS.policies[policyID]
	p.AckAudienceOverride = []uuid.UUID{rootID}
	fakePS.policies[policyID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: policyID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if !resp.RequiresAck {
		t.Fatalf("expected requires_ack=true")
	}
	// Should only have AD-A (root category only, not child).
	if len(resp.AudienceGroups) != 1 || resp.AudienceGroups[0] != "AD-A" {
		t.Fatalf("audience_groups: got %v want [AD-A]", resp.AudienceGroups)
	}
}

// TestListObligatingPoliciesOnlyPublishedWithAck verifies that
// ListObligatingPolicies returns only policies that are published AND have a
// non-none ack trigger.
func TestListObligatingPoliciesOnlyPublishedWithAck(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, _, _, publishedVersionID := seedObligationEnv(t, fakePS, fakeGS)

	// Also create a policy with no trigger (should NOT be in result).
	neutralGID := uuid.New()
	fakeGS.categories[neutralGID] = domain.Category{ID: neutralGID, Name: "Neutral", Slug: "neutral"}
	p2, _ := domain.NewPolicy("P2", neutralGID, domain.SensitivityStandard, uuid.New())
	p2.CurrentPublishedVersionID = uuid.New()
	p2.Number = "neutral-1"
	fakePS.policies[p2.ID] = p2
	fakePS.versions[p2.CurrentPublishedVersionID] = domain.PolicyVersion{
		ID: p2.CurrentPublishedVersionID, PolicyID: p2.ID,
		Status: domain.PolicyVersionStatusPublished, VersionNo: 1,
	}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.ListObligatingPolicies(context.Background(), &corev1.ListObligatingPoliciesRequest{})
	if err != nil {
		t.Fatalf("ListObligatingPolicies: %v", err)
	}
	if len(resp.Policies) != 1 {
		t.Fatalf("expected 1 obligating policy, got %d", len(resp.Policies))
	}
	op := resp.Policies[0]
	if op.PolicyId != policyID.String() {
		t.Fatalf("policy_id: got %q want %q", op.PolicyId, policyID)
	}
	if op.PublishedVersionId != publishedVersionID.String() {
		t.Fatalf("published_version_id: got %q want %q", op.PublishedVersionId, publishedVersionID)
	}
	if op.VersionNo != 1 {
		t.Fatalf("version_no: got %d want 1", op.VersionNo)
	}
	// Procedures are excluded at the source, so the document_type is always POLICY, never
	// UNSPECIFIED.
	if op.GetDocumentType() != corev1.DocumentType_DOCUMENT_TYPE_POLICY {
		t.Fatalf("document_type: got %v want POLICY", op.GetDocumentType())
	}
	if !op.OnChange {
		t.Fatalf("on_change: expected true")
	}
	adSet := map[string]bool{}
	for _, ad := range op.AudienceGroups {
		adSet[ad] = true
	}
	if !adSet["AD-A"] || !adSet["AD-B"] {
		t.Fatalf("audience_groups: got %v", op.AudienceGroups)
	}
}

// TestProcedureObligatesNoOne: a PROCEDURE obligates no one even when published in a category with
// an on-change trigger. It gives requires_ack=false and is left out of ListObligatingPolicies, so
// nothing downstream can raise an acknowledgement, reminder or escalation for it. A sibling POLICY
// in the same category still obligates, so the gate keys on document_type alone.
func TestProcedureObligatesNoOne(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	// Seed the standard on-change ack env; the seeded document is a POLICY.
	procID, rootID, _, _ := seedObligationEnv(t, fakePS, fakeGS)

	// Flip the seeded document to a PROCEDURE (published, on-change trigger).
	proc := fakePS.policies[procID]
	proc.DocumentType = domain.DocumentTypeProcedure
	fakePS.policies[procID] = proc

	// Add a sibling POLICY homed in the SAME (on-change) root category so the
	// regression assertion proves policies still obligate exactly as before.
	sibling, err := domain.NewPolicy("Sibling Policy", rootID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	sibling.DocumentType = domain.DocumentTypePolicy
	sibling.CurrentPublishedVersionID = uuid.New()
	sibling.Number = "root-2"
	fakePS.policies[sibling.ID] = sibling
	fakePS.versions[sibling.CurrentPublishedVersionID] = domain.PolicyVersion{
		ID: sibling.CurrentPublishedVersionID, PolicyID: sibling.ID,
		Status: domain.PolicyVersionStatusPublished, VersionNo: 1,
	}

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)

	// ResolvePolicyObligation(procedure): requires_ack=false, empty audience.
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: procID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation(procedure): %v", err)
	}
	if resp.RequiresAck {
		t.Fatalf("procedure must yield requires_ack=false")
	}
	if resp.OnChange {
		t.Fatalf("procedure must yield on_change=false")
	}
	if len(resp.AudienceGroups) != 0 {
		t.Fatalf("procedure must yield empty audience, got %v", resp.AudienceGroups)
	}

	// ResolvePolicyObligation(sibling policy): still obligates (regression).
	polResp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: sibling.ID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation(policy): %v", err)
	}
	if !polResp.RequiresAck || !polResp.OnChange {
		t.Fatalf("sibling policy must still obligate (requires_ack && on_change)")
	}

	// ListObligatingPolicies: the procedure is excluded; the sibling policy is
	// present (and is the ONLY entry).
	list, err := h.ListObligatingPolicies(context.Background(), &corev1.ListObligatingPoliciesRequest{})
	if err != nil {
		t.Fatalf("ListObligatingPolicies: %v", err)
	}
	for _, op := range list.Policies {
		if op.PolicyId == procID.String() {
			t.Fatalf("procedure %s must be excluded from obligating policies", procID)
		}
	}
	if len(list.Policies) != 1 || list.Policies[0].PolicyId != sibling.ID.String() {
		t.Fatalf("expected only the sibling policy to obligate, got %+v", list.Policies)
	}
}

// TestRetiredPolicyObligatesNoOne verifies that a retired policy (RetiredAt set)
// yields requires_ack=false from ResolvePolicyObligation and is EXCLUDED from
// ListObligatingPolicies, even though it is published with an on-change trigger.
// This is how retiring a policy cancels every user's outstanding obligation
// (obligations are computed, so dropping the policy from this set removes it
// from all users) while completed acks are kept as history.
func TestRetiredPolicyObligatesNoOne(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	policyID, _, _, _ := seedObligationEnv(t, fakePS, fakeGS)

	// Mark the (published, on-change) policy retired.
	p := fakePS.policies[policyID]
	now := time.Now().UTC()
	p.RetiredAt = &now
	fakePS.policies[policyID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)

	// ResolvePolicyObligation: requires_ack=false.
	resp, err := h.ResolvePolicyObligation(context.Background(), &corev1.ResolvePolicyObligationRequest{
		PolicyId: policyID.String(),
	})
	if err != nil {
		t.Fatalf("ResolvePolicyObligation: %v", err)
	}
	if resp.RequiresAck {
		t.Fatalf("expected requires_ack=false for a retired policy")
	}

	// ListObligatingPolicies: excluded entirely.
	list, err := h.ListObligatingPolicies(context.Background(), &corev1.ListObligatingPoliciesRequest{})
	if err != nil {
		t.Fatalf("ListObligatingPolicies: %v", err)
	}
	for _, op := range list.Policies {
		if op.PolicyId == policyID.String() {
			t.Fatalf("retired policy %s must be excluded from obligating policies", policyID)
		}
	}
}

// seedVersion creates a policy + a version in the given status and returns both.
func seedVersion(t *testing.T, fakePS *fakePolicyStore, st domain.PolicyVersionStatus) (domain.Policy, domain.PolicyVersion) {
	t.Helper()
	p, err := domain.NewPolicy("Test Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	pv := domain.PolicyVersion{
		ID:                uuid.New(),
		PolicyID:          p.ID,
		VersionNo:         1,
		Status:            st,
		TemplateVersionID: uuid.New(),
		ContentJSON:       `{"sections":{}}`,
	}
	fakePS.versions[pv.ID] = pv
	return p, pv
}

func TestSetVersionStatusPublishEmitsAuditWithActor(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, em)
	actor := uuid.New().String()
	resp, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("SetVersionStatus: %v", err)
	}
	if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED {
		t.Fatalf("status: got %v", resp.GetVersion().GetStatus())
	}
	// Policy pointer should now point at this version.
	if fakePS.policies[p.ID].CurrentPublishedVersionID != pv.ID {
		t.Fatalf("current_published_version_id not updated")
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	got := cap.calls[0]
	if got.event.Action != "policy.status_set" {
		t.Fatalf("action: got %q", got.event.Action)
	}
	if got.event.ActorUserID != actor {
		t.Fatalf("actor: got %q want %q", got.event.ActorUserID, actor)
	}
	if got.event.Attributes["status"] != "published" || got.event.Attributes["from"] != "draft" {
		t.Fatalf("attributes: got %v", got.event.Attributes)
	}
}

// TestSetVersionStatusAcceptsSystemActor: workflow publishes with actor_user_id="system", not a
// UUID. Validating it as a UUID left the publish failing with InvalidArgument and the version stuck
// in draft with no audit event. A system actor must be accepted and the publish completed with a
// policy.status_set event.
func TestSetVersionStatusAcceptsSystemActor(t *testing.T) {
	for _, actor := range []string{"system", "system:scheduled-publish"} {
		t.Run(actor, func(t *testing.T) {
			cap := &capturePublisher{}
			em := audit.New(cap)
			fakePS := newFakePolicyStore()
			p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)

			h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, em)
			resp, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
				PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: actor,
			})
			if err != nil {
				t.Fatalf("SetVersionStatus(actor=%q): %v", actor, err)
			}
			if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED {
				t.Fatalf("status: got %v, want PUBLISHED", resp.GetVersion().GetStatus())
			}
			if fakePS.policies[p.ID].CurrentPublishedVersionID != pv.ID {
				t.Fatalf("current_published_version_id not updated")
			}
			if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.status_set" {
				t.Fatalf("expected one policy.status_set audit event, got %+v", cap.calls)
			}
			if got := cap.calls[0].event.ActorUserID; got != actor {
				t.Fatalf("audit actor: got %q want %q", got, actor)
			}
		})
	}
}

// TestSetVersionStatusRejectsGarbageActor ensures a non-empty, non-UUID,
// non-system actor is still rejected (the system-actor exception is narrow).
func TestSetVersionStatusRejectsGarbageActor(t *testing.T) {
	fakePS := newFakePolicyStore()
	_, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil)
	_, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: "not-a-uuid",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for garbage actor, got %v", err)
	}
}

func TestSetVersionStatusWithdrawClearsPointer(t *testing.T) {
	fakePS := newFakePolicyStore()
	p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusPublished)
	pol := fakePS.policies[p.ID]
	pol.CurrentPublishedVersionID = pv.ID
	fakePS.policies[p.ID] = pol

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil)
	resp, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "draft", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("SetVersionStatus(withdraw): %v", err)
	}
	if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT {
		t.Fatalf("status: got %v", resp.GetVersion().GetStatus())
	}
	if fakePS.policies[p.ID].CurrentPublishedVersionID != uuid.Nil {
		t.Fatalf("expected current_published_version_id cleared")
	}
}

func TestSetVersionStatusRejectsInvalidTransition(t *testing.T) {
	fakePS := newFakePolicyStore()
	_, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusArchived) // terminal

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil)
	_, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: uuid.New().String(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestSetVersionStatusRejectsNonStoredStatus(t *testing.T) {
	fakePS := newFakePolicyStore()
	_, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil)
	// "in_review" is an approval-lifecycle status, not a stored version status.
	_, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "in_review", ActorUserId: uuid.New().String(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

// TestSetVersionStatusPublishEmitsLifecycleEvent: workflow publishes through SetVersionStatus, not
// PublishDraft, so a real draft-to-published move here must also emit policy.published on the jobs
// exchange, or the AI index never sees versions published through approval.
func TestSetVersionStatusPublishEmitsLifecycleEvent(t *testing.T) {
	fakePS := newFakePolicyStore()
	p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)

	lifeCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(lifeCap)
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	resp, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: "system",
	})
	if err != nil {
		t.Fatalf("SetVersionStatus: %v", err)
	}
	if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED {
		t.Fatalf("status: got %v", resp.GetVersion().GetStatus())
	}
	if len(lifeCap.calls) != 1 {
		t.Fatalf("expected 1 lifecycle event, got %d", len(lifeCap.calls))
	}
	if got, want := lifeCap.calls[0].routingKey, "policy.published"; got != want {
		t.Fatalf("lifecycle routing key: got %q want %q", got, want)
	}
	var evt map[string]any
	if err := json.Unmarshal(lifeCap.calls[0].body, &evt); err != nil {
		t.Fatalf("unmarshal lifecycle body: %v", err)
	}
	v, ok := evt["version"].(map[string]any)
	if !ok {
		t.Fatalf("version object missing: %+v", evt)
	}
	if v["PolicyID"] != p.ID.String() {
		t.Fatalf("PolicyID: got %v want %s", v["PolicyID"], p.ID)
	}
}

// TestSetVersionStatusNonPublishTargetDoesNotEmitLifecycle ensures the
// lifecycle emit is scoped to transitions INTO published: withdraw
// (published→draft) and retire (draft→archived) must not fire it.
func TestSetVersionStatusNonPublishTargetDoesNotEmitLifecycle(t *testing.T) {
	lifeCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(lifeCap)

	t.Run("withdraw published to draft", func(t *testing.T) {
		fakePS := newFakePolicyStore()
		p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusPublished)
		pol := fakePS.policies[p.ID]
		pol.CurrentPublishedVersionID = pv.ID
		fakePS.policies[p.ID] = pol

		h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil).
			WithLifecycleEmitter(lifeEm)
		if _, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
			PolicyVersionId: pv.ID.String(), Status: "draft", ActorUserId: uuid.New().String(),
		}); err != nil {
			t.Fatalf("SetVersionStatus(withdraw): %v", err)
		}
		if len(lifeCap.calls) != 0 {
			t.Fatalf("expected no lifecycle event on withdraw, got %d", len(lifeCap.calls))
		}
	})

	t.Run("draft to archived", func(t *testing.T) {
		fakePS := newFakePolicyStore()
		_, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusDraft)

		h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil).
			WithLifecycleEmitter(lifeEm)
		if _, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
			PolicyVersionId: pv.ID.String(), Status: "archived", ActorUserId: uuid.New().String(),
		}); err != nil {
			t.Fatalf("SetVersionStatus(archive): %v", err)
		}
		if len(lifeCap.calls) != 0 {
			t.Fatalf("expected no lifecycle event on archive, got %d", len(lifeCap.calls))
		}
	})
}

// TestSetVersionStatusRepublishSelfTransitionDoesNotEmitLifecycle guards the
// published→published no-op (allowed as idempotent by
// domain.ValidateVersionStatusTransition): since the version is not actually
// changing status, it must not re-trigger a lifecycle publish/re-index.
func TestSetVersionStatusRepublishSelfTransitionDoesNotEmitLifecycle(t *testing.T) {
	fakePS := newFakePolicyStore()
	p, pv := seedVersion(t, fakePS, domain.PolicyVersionStatusPublished)
	pol := fakePS.policies[p.ID]
	pol.CurrentPublishedVersionID = pv.ID
	fakePS.policies[p.ID] = pol

	lifeCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(lifeCap)
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	if _, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: pv.ID.String(), Status: "published", ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetVersionStatus(republish): %v", err)
	}
	if len(lifeCap.calls) != 0 {
		t.Fatalf("expected no lifecycle event on published→published self-transition, got %d", len(lifeCap.calls))
	}
}

type fakeAppendixCopier struct{ calls [][2]string }

func (f *fakeAppendixCopier) CopyForward(_ context.Context, from, to uuid.UUID) error {
	f.calls = append(f.calls, [2]string{from.String(), to.String()})
	return nil
}

// TestSaveDraft_CopiesAppendicesForwardOnce verifies that SaveDraft fires
// CopyForward exactly once — on the first create — using the published
// version as the source. A second SaveDraft (autosave) must not trigger it.
func TestSaveDraft_CopiesAppendicesForwardOnce(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()
	copier := &fakeAppendixCopier{}

	// Build a policy with a published version seeded as CurrentPublishedVersionID.
	pubID := uuid.New()
	p, err := domain.NewPolicy("Appendix Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.CurrentPublishedVersionID = pubID
	fakePS.policies[p.ID] = p
	fakePS.versions[pubID] = domain.PolicyVersion{
		ID:        pubID,
		PolicyID:  p.ID,
		Status:    domain.PolicyVersionStatusPublished,
		VersionNo: 1,
	}

	tvID := uuid.New()
	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil).
		WithAppendixCopier(copier)

	// 1st SaveDraft → should INSERT → copier called once with [pubID, newDraftID].
	resp1, err := h.SaveDraft(context.Background(), &corev1.SaveDraftRequest{
		PolicyId:          p.ID.String(),
		TemplateVersionId: tvID.String(),
		ContentJson:       `{"sections":{}}`,
		ActorUserId:       uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("1st SaveDraft: %v", err)
	}
	newDraftID := resp1.GetVersion().GetId()
	if len(copier.calls) != 1 {
		t.Fatalf("expected 1 copy-forward call after 1st SaveDraft, got %d", len(copier.calls))
	}
	if copier.calls[0][0] != pubID.String() {
		t.Fatalf("copy-forward from: got %q want %q", copier.calls[0][0], pubID.String())
	}
	if copier.calls[0][1] != newDraftID {
		t.Fatalf("copy-forward to: got %q want %q", copier.calls[0][1], newDraftID)
	}

	// 2nd SaveDraft (autosave update) → should UPDATE → copier NOT called again.
	_, err = h.SaveDraft(context.Background(), &corev1.SaveDraftRequest{
		PolicyId:          p.ID.String(),
		TemplateVersionId: tvID.String(),
		ContentJson:       `{"sections":{"purpose":"updated"}}`,
		ActorUserId:       uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("2nd SaveDraft: %v", err)
	}
	if len(copier.calls) != 1 {
		t.Fatalf("expected still 1 copy-forward call after 2nd SaveDraft (autosave), got %d", len(copier.calls))
	}
}

// TestSaveDraft_AutosaveDoesNotDuplicateAudit: SaveDraft is the autosave endpoint. A real save (a
// new draft, or changed content) is audited once as policy.draft_saved; an idle tick with identical
// content isn't audited at all.
func TestSaveDraft_AutosaveDoesNotDuplicateAudit(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	fakeTS := newFakeTemplateStore()

	p, err := domain.NewPolicy("Autosave Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	countDraftSaved := func() int {
		n := 0
		for _, c := range cap.calls {
			if c.event.Action == "policy.draft_saved" {
				n++
			}
		}
		return n
	}

	save := func(content string) {
		t.Helper()
		if _, serr := h.SaveDraft(context.Background(), &corev1.SaveDraftRequest{
			PolicyId:    p.ID.String(),
			ContentJson: content,
			ActorUserId: uuid.New().String(),
		}); serr != nil {
			t.Fatalf("SaveDraft(%q): %v", content, serr)
		}
	}

	// 1st save (INSERT) → one genuine save → one audit record.
	save(`{"sections":{"purpose":"v1"}}`)
	if got := countDraftSaved(); got != 1 {
		t.Fatalf("after first save: got %d draft_saved audits, want 1", got)
	}

	// 2nd save with IDENTICAL content (an idle autosave tick) → no change →
	// still exactly one audit record.
	save(`{"sections":{"purpose":"v1"}}`)
	if got := countDraftSaved(); got != 1 {
		t.Fatalf("after identical autosave: got %d draft_saved audits, want 1 (no duplicate)", got)
	}

	// 3rd save with CHANGED content → a genuine save → a second audit record.
	save(`{"sections":{"purpose":"v2"}}`)
	if got := countDraftSaved(); got != 2 {
		t.Fatalf("after real content change: got %d draft_saved audits, want 2", got)
	}
}

// renamePendingHandler wires a PolicyHandler over the in-memory fake store so a
// test can put the policy's working version into an ARBITRARY status. The real
// store derives current_draft_version_id from status='draft', so a "current
// draft" that is no longer an editable draft is only expressible through the
// fake. Returns the handler, the fake store, the audit capture, and the seeded
// policy id and working-version id.
func renamePendingHandler(t *testing.T, workingStatus domain.PolicyVersionStatus) (*grpcsvc.PolicyHandler, *fakePolicyStore, *capturePublisher, uuid.UUID, uuid.UUID) {
	t.Helper()
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()

	policyID := uuid.New()
	pubID := uuid.New()
	workingID := uuid.New()

	fakePS.versions[pubID] = domain.PolicyVersion{ID: pubID, PolicyID: policyID, Status: domain.PolicyVersionStatusPublished, VersionNo: 1}
	fakePS.versions[workingID] = domain.PolicyVersion{ID: workingID, PolicyID: policyID, Status: workingStatus}
	fakePS.policies[policyID] = domain.Policy{
		ID:                        policyID,
		HomeCategoryID:            uuid.New(),
		Title:                     "Live Title",
		CurrentPublishedVersionID: pubID,
		CurrentDraftVersionID:     workingID,
	}
	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	return h, fakePS, cap, policyID, workingID
}

// A published policy whose working version is pending approval (any status other than the editable
// 'draft'; core doesn't store in_review, workflow owns it) must refuse the rename with
// FailedPrecondition, leave proposed_title unset and emit no policy.rename_staged.
func TestRenamePolicyPendingApprovalBlocked(t *testing.T) {
	h, fakePS, cap, policyID, workingID := renamePendingHandler(t, domain.PolicyVersionStatus("in_review"))

	resp, err := h.RenamePolicy(context.Background(), &corev1.RenamePolicyRequest{
		PolicyId: policyID.String(), NewTitle: "Should Not Stage", ActorUserId: uuid.New().String(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got resp=%v err=%v", resp, err)
	}
	if got := status.Convert(err).Message(); got != "A change is pending approval. Resolve or withdraw that draft before renaming." {
		t.Fatalf("unexpected message: %q", got)
	}
	if pv := fakePS.versions[workingID]; pv.ProposedTitle != nil {
		t.Fatalf("proposed_title must stay unset on the in-review version, got %q", *pv.ProposedTitle)
	}
	for _, c := range cap.calls {
		if c.event.Action == "policy.rename_staged" {
			t.Fatalf("no policy.rename_staged must be emitted on a blocked rename")
		}
	}
}

// A published policy whose working version is an editable draft stages the rename on it and returns
// staged=true with its id; staged is reported only when a stage happened.
func TestRenamePolicyEditableDraftStages(t *testing.T) {
	h, fakePS, _, policyID, draftID := renamePendingHandler(t, domain.PolicyVersionStatusDraft)

	resp, err := h.RenamePolicy(context.Background(), &corev1.RenamePolicyRequest{
		PolicyId: policyID.String(), NewTitle: "Staged Title", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	if !resp.GetStaged() {
		t.Fatalf("expected staged=true for an editable draft")
	}
	if resp.GetDraftVersionId() != draftID.String() {
		t.Fatalf("expected stage on existing draft %s, got %s", draftID, resp.GetDraftVersionId())
	}
	if pv := fakePS.versions[draftID]; pv.ProposedTitle == nil || *pv.ProposedTitle != "Staged Title" {
		t.Fatalf("proposed_title: got %v want %q", pv.ProposedTitle, "Staged Title")
	}
}

// seedPublishedPolicy adds a policy with a current published version (and an
// optional prior superseded version) to the fake store and returns the policy
// and its published version. content drives domain.ExtractSections.
func seedPublishedPolicy(t *testing.T, fakePS *fakePolicyStore, homeGID uuid.UUID, title, content string, withSuperseded bool) (domain.Policy, domain.PolicyVersion) {
	t.Helper()
	p, err := domain.NewPolicy(title, homeGID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	pub := domain.PolicyVersion{
		ID:          uuid.New(),
		PolicyID:    p.ID,
		VersionNo:   2,
		Status:      domain.PolicyVersionStatusPublished,
		ContentJSON: content,
	}
	p.CurrentPublishedVersionID = pub.ID
	fakePS.policies[p.ID] = p
	fakePS.versions[pub.ID] = pub
	if withSuperseded {
		old := domain.PolicyVersion{
			ID:          uuid.New(),
			PolicyID:    p.ID,
			VersionNo:   1,
			Status:      domain.PolicyVersionStatusSuperseded,
			ContentJSON: `{"sections":{"purpose":"old"}}`,
		}
		fakePS.versions[old.ID] = old
	}
	return p, pub
}

// TestReindexPolicyEmitsAIOnlyEvent: ReindexPolicy sends the current published version to the AI
// indexer queue only (default exchange, "ai.policy.publish"), never the jobs exchange, so the
// review email isn't sent again, and queues the removal of the superseded version's chunks.
func TestReindexPolicyEmitsAIOnlyEvent(t *testing.T) {
	jobsCap := &lifecycleCapture{}
	aiCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(jobsCap).WithAIPublisher(aiCap)

	fakeGS := newFakeCategoryStore()
	fakeTS := &fakeTemplateStore{}
	fakePS := newFakePolicyStore()

	homeGID := uuid.New()
	if _, err := fakeGS.Create(context.Background(), domain.Category{ID: homeGID, Name: "HR", Slug: "hr"}); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	p, pub := seedPublishedPolicy(t, fakePS, homeGID,
		"DR Policy", `{"sections":{"purpose":"Define recovery.","scope":"All staff."}}`, true)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	resp, err := h.ReindexPolicy(context.Background(), &corev1.ReindexPolicyRequest{
		PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("ReindexPolicy: %v", err)
	}
	if resp.GetVersionId() != pub.ID.String() {
		t.Fatalf("version_id: got %q want %q", resp.GetVersionId(), pub.ID)
	}
	if resp.GetSections() != 2 {
		t.Fatalf("sections: got %d want 2", resp.GetSections())
	}
	if resp.GetRemovedPrior() != 1 {
		t.Fatalf("removed_prior: got %d want 1", resp.GetRemovedPrior())
	}

	// Nothing may go to the jobs exchange: that would send the review email again.
	if len(jobsCap.calls) != 0 {
		t.Fatalf("jobs exchange must NOT be used for re-index, got %d calls", len(jobsCap.calls))
	}
	// AI queue: one remove (superseded) + one publish (current) = 2 calls, all
	// on the direct-to-queue routing key.
	if len(aiCap.calls) != 2 {
		t.Fatalf("expected 2 AI-queue calls, got %d", len(aiCap.calls))
	}
	var publishCount, removeCount int
	for _, c := range aiCap.calls {
		if c.routingKey != lifecycle.AIIndexQueue {
			t.Fatalf("AI routing key: got %q want %q", c.routingKey, lifecycle.AIIndexQueue)
		}
		var evt map[string]any
		if err := json.Unmarshal(c.body, &evt); err != nil {
			t.Fatalf("unmarshal AI body: %v", err)
		}
		switch evt["event_type"] {
		case "policy.published":
			publishCount++
			v := evt["version"].(map[string]any)
			if v["VersionID"] != pub.ID.String() {
				t.Fatalf("publish VersionID: got %v want %s", v["VersionID"], pub.ID)
			}
			secs, ok := v["Sections"].([]any)
			if !ok || len(secs) != 2 {
				t.Fatalf("publish Sections: %v", v["Sections"])
			}
		case "policy.unpublished":
			removeCount++
		default:
			t.Fatalf("unexpected event_type %v", evt["event_type"])
		}
	}
	if publishCount != 1 || removeCount != 1 {
		t.Fatalf("want 1 publish + 1 remove, got %d publish %d remove", publishCount, removeCount)
	}
}

// TestReindexPolicyNoPublishedVersionRejected asserts a policy with no published
// version is rejected with FailedPrecondition.
func TestReindexPolicyNoPublishedVersionRejected(t *testing.T) {
	aiCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(&lifecycleCapture{}).WithAIPublisher(aiCap)

	fakeGS := newFakeCategoryStore()
	fakePS := newFakePolicyStore()
	p, err := domain.NewPolicy("Draft-only", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p // CurrentPublishedVersionID is uuid.Nil

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, &fakeTemplateStore{}, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	_, err = h.ReindexPolicy(context.Background(), &corev1.ReindexPolicyRequest{PolicyId: p.ID.String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (code %s)", err, status.Code(err))
	}
	if len(aiCap.calls) != 0 {
		t.Fatalf("no AI event should be emitted for a policy with no published version, got %d", len(aiCap.calls))
	}
}

// TestReindexPolicyVersionRejectsDraft asserts a draft version cannot be
// re-indexed (drafts are never in the corpus).
func TestReindexPolicyVersionRejectsDraft(t *testing.T) {
	aiCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(&lifecycleCapture{}).WithAIPublisher(aiCap)

	fakePS := newFakePolicyStore()
	draft := domain.PolicyVersion{
		ID:          uuid.New(),
		PolicyID:    uuid.New(),
		Status:      domain.PolicyVersionStatusDraft,
		ContentJSON: `{"sections":{"purpose":"wip"}}`,
	}
	fakePS.versions[draft.ID] = draft

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	_, err := h.ReindexPolicyVersion(context.Background(), &corev1.ReindexPolicyVersionRequest{
		PolicyVersionId: draft.ID.String(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for draft, got %v (code %s)", err, status.Code(err))
	}
	if len(aiCap.calls) != 0 {
		t.Fatalf("no AI event should be emitted for a draft, got %d", len(aiCap.calls))
	}
}

// TestReindexAllPublishedIteratesPublishedVersions asserts the bulk variant
// re-indexes every policy's current published version, skips policies with none,
// and returns the counts.
func TestReindexAllPublishedIteratesPublishedVersions(t *testing.T) {
	jobsCap := &lifecycleCapture{}
	aiCap := &lifecycleCapture{}
	lifeEm := lifecycle.New(jobsCap).WithAIPublisher(aiCap)

	fakeGS := newFakeCategoryStore()
	fakePS := newFakePolicyStore()
	homeGID := uuid.New()
	if _, err := fakeGS.Create(context.Background(), domain.Category{ID: homeGID, Slug: "hr"}); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	seedPublishedPolicy(t, fakePS, homeGID, "P1", `{"sections":{"purpose":"one"}}`, false)
	seedPublishedPolicy(t, fakePS, homeGID, "P2", `{"sections":{"purpose":"two"}}`, false)
	// A never-published policy that must be skipped.
	unp, _ := domain.NewPolicy("Draft-only", homeGID, domain.SensitivityStandard, uuid.New())
	fakePS.policies[unp.ID] = unp

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, &fakeTemplateStore{}, domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifeEm)

	resp, err := h.ReindexAllPublished(context.Background(), &corev1.ReindexAllPublishedRequest{})
	if err != nil {
		t.Fatalf("ReindexAllPublished: %v", err)
	}
	if resp.GetPoliciesReindexed() != 2 {
		t.Fatalf("policies_reindexed: got %d want 2", resp.GetPoliciesReindexed())
	}
	if resp.GetSkipped() != 1 {
		t.Fatalf("skipped: got %d want 1", resp.GetSkipped())
	}
	if resp.GetFailures() != 0 {
		t.Fatalf("failures: got %d want 0", resp.GetFailures())
	}
	if len(jobsCap.calls) != 0 {
		t.Fatalf("jobs exchange must NOT be used for re-index, got %d", len(jobsCap.calls))
	}
	// Two published policies, no superseded versions → exactly 2 publish events.
	if len(aiCap.calls) != 2 {
		t.Fatalf("expected 2 AI-queue publish calls, got %d", len(aiCap.calls))
	}
}

// TestListPoliciesDocumentTypeFilter: document_type reaches the store. Unset lists policies only,
// PROCEDURE procedures only, POLICY policies only.
func TestListPoliciesDocumentTypeFilter(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, nil)
	ctx := context.Background()
	gid := uuid.New()

	pol, _ := domain.NewPolicy("A Policy", gid, domain.SensitivityStandard, uuid.New())
	if _, err := fakePS.CreatePolicy(ctx, pol); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	proc, _ := domain.NewPolicy("A Procedure", gid, domain.SensitivityStandard, uuid.New())
	proc.DocumentType = domain.DocumentTypeProcedure
	if _, err := fakePS.CreatePolicy(ctx, proc); err != nil {
		t.Fatalf("seed procedure: %v", err)
	}

	idsFor := func(t *testing.T, dt corev1.DocumentType) map[string]bool {
		t.Helper()
		resp, err := h.ListPolicies(ctx, &corev1.ListPoliciesRequest{
			CategoryId:   gid.String(),
			DocumentType: dt,
		})
		if err != nil {
			t.Fatalf("ListPolicies(%v): %v", dt, err)
		}
		got := map[string]bool{}
		for _, p := range resp.GetPolicies() {
			got[p.GetId()] = true
		}
		return got
	}

	// Unset filter: policies only.
	unset := idsFor(t, corev1.DocumentType_DOCUMENT_TYPE_UNSPECIFIED)
	if !unset[pol.ID.String()] {
		t.Fatal("unset filter must include the policy")
	}
	if unset[proc.ID.String()] {
		t.Fatal("unset filter must NOT include the procedure")
	}

	// Explicit POLICY → policies only.
	pols := idsFor(t, corev1.DocumentType_DOCUMENT_TYPE_POLICY)
	if !pols[pol.ID.String()] || pols[proc.ID.String()] {
		t.Fatalf("POLICY filter wrong set: %v", pols)
	}

	// Explicit PROCEDURE → procedures only.
	procs := idsFor(t, corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE)
	if !procs[proc.ID.String()] {
		t.Fatal("PROCEDURE filter must include the procedure")
	}
	if procs[pol.ID.String()] {
		t.Fatal("PROCEDURE filter must NOT include the policy")
	}
}

// seedFreeFormDraftForUpdate sets up a draft pinned to no template version, the usual case. Its
// TemplateVersionID is uuid.Nil, as a NULL column scans.
func seedFreeFormDraftForUpdate(t *testing.T, fakePS *fakePolicyStore) (draftID, policyID uuid.UUID) {
	t.Helper()
	policyID = uuid.New()
	draftID = uuid.New()
	fakePS.versions[draftID] = domain.PolicyVersion{
		ID:                draftID,
		PolicyID:          policyID,
		Status:            domain.PolicyVersionStatusDraft,
		TemplateVersionID: uuid.Nil,
		ContentJSON:       `{}`,
	}
	fakePS.draftByPolicy[policyID] = fakePS.versions[draftID]
	return draftID, policyID
}

// TestUpdateDraftContentAcceptsFreeFormDraft: a freeform draft sends an empty template_version_id,
// which strict UUID parsing refused, so no collaborative edit on a freeform document could persist.
// The snapshot must be accepted and stored, with no template version lookup.
func TestUpdateDraftContentAcceptsFreeFormDraft(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, policyID := seedFreeFormDraftForUpdate(t, fakePS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)
	content := uiAuthoredContent("An edit made on a free-form policy.")
	actor := uuid.New().String()

	resp, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       content,
		TemplateVersionId: "",
		ActorUserId:       actor,
	})
	if err != nil {
		t.Fatalf("UpdateDraftContent rejected a free-form draft: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected Accepted=true, got false (reason: %s)", resp.GetRejectReason())
	}
	if got := fakePS.versions[draftID].ContentJSON; got != content {
		t.Fatalf("content not persisted; got %q", got)
	}
	// No pinned template means nothing to fetch: a lookup of uuid.Nil would be
	// a pointless round trip and, against the real store, an error.
	if len(fakeTS.getVersionCalls) != 0 {
		t.Fatalf("expected no TemplateVersion lookup for a free-form draft, got %v", fakeTS.getVersionCalls)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(cap.calls))
	}
	if got := cap.calls[0].event.Action; got != "policy.draft_content_updated" {
		t.Fatalf("audit action: got %q", got)
	}
}

// TestUpdateDraftContentFreeFormRejectsAnyTemplateVersion pins the other half
// of the pin check. "Free-form" means NO template, not ANY template: a request
// that claims a real template_version_id against a draft pinned to none must
// still be refused, because uuid.Nil does not match a real id.
func TestUpdateDraftContentFreeFormRejectsAnyTemplateVersion(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, policyID := seedFreeFormDraftForUpdate(t, fakePS)
	priorContent := fakePS.versions[draftID].ContentJSON

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       uiAuthoredContent("smuggled in under someone else's template"),
		TemplateVersionId: uuid.New().String(),
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error for a template claim against a free-form draft, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	// It must fail on the pin check, not incidentally on UUID parsing.
	if !strings.Contains(err.Error(), "does not match draft's pinned version") {
		t.Fatalf("expected the pin-mismatch error, got: %v", err)
	}
	if got := fakePS.versions[draftID].ContentJSON; got != priorContent {
		t.Fatalf("content was modified on mismatch: %q", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentTemplatedRejectsEmptyTemplateVersion is the mirror
// case: a draft that IS pinned must not be updatable by a caller that omits
// the template_version_id. Allowing an empty value to parse to uuid.Nil must
// not become a way to bypass the pin.
func TestUpdateDraftContentTemplatedRejectsEmptyTemplateVersion(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, _, policyID := seedDraftForUpdate(t, fakePS, fakeTS)
	priorContent := fakePS.versions[draftID].ContentJSON

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       uiAuthoredContent("dropping the pin to dodge validation"),
		TemplateVersionId: "",
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error when a templated draft is sent an empty template_version_id, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	// The empty value parses, so the pin check itself must refuse it, not UUID parsing.
	if !strings.Contains(err.Error(), "does not match draft's pinned version") {
		t.Fatalf("expected the pin-mismatch error, got: %v", err)
	}
	if got := fakePS.versions[draftID].ContentJSON; got != priorContent {
		t.Fatalf("content was modified: %q", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentRejectsMalformedTemplateVersion: allowing an empty value doesn't allow a
// malformed one; a non-empty non-UUID is still InvalidArgument.
func TestUpdateDraftContentRejectsMalformedTemplateVersion(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, policyID := seedFreeFormDraftForUpdate(t, fakePS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       uiAuthoredContent("body"),
		TemplateVersionId: "not-a-uuid",
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error for a malformed template_version_id, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}

// TestUpdateDraftContentFreeFormStillEnforcesEnvelopeInvariants verifies the
// free-form path is validated, not waved through: an emptied root is the
// concrete corruption mode of the snapshot path and must still be refused even
// with no template to validate against.
func TestUpdateDraftContentFreeFormStillEnforcesEnvelopeInvariants(t *testing.T) {
	cap := &capturePublisher{}
	em := audit.New(cap)
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fakeGS := newFakeCategoryStore()
	draftID, policyID := seedFreeFormDraftForUpdate(t, fakePS)

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, fakeTS, domain.NoopValidator{}, em)

	_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
		PolicyId:          policyID.String(),
		DraftId:           draftID.String(),
		ContentJson:       `{"root":{"type":"root","children":[]}}`,
		TemplateVersionId: "",
		ActorUserId:       uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected an emptied root to be rejected on the free-form path, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("status code: got %v want InvalidArgument", got)
	}
	if len(cap.calls) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(cap.calls))
	}
}
