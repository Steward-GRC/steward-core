// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// fakePolicyAppendixBackend answers every call with a canned appendix, and
// records what the handler passed on.
type fakePolicyAppendixBackend struct {
	result  domain.PolicyAppendix
	list    []domain.PolicyAppendix
	changed bool
	err     error

	gotPolicy   uuid.UUID
	gotID       uuid.UUID
	gotActor    uuid.UUID
	gotTitle    string
	gotContent  string
	gotRetired  bool
	gotOrder    []uuid.UUID
	calls       int
	checkpoints int
}

func (f *fakePolicyAppendixBackend) List(_ context.Context, policy uuid.UUID, includeRetired bool) ([]domain.PolicyAppendix, error) {
	f.calls++
	f.gotPolicy, f.gotRetired = policy, includeRetired
	return f.list, f.err
}

func (f *fakePolicyAppendixBackend) Create(_ context.Context, policy, actor uuid.UUID, title, content string) (domain.PolicyAppendix, error) {
	f.calls++
	f.gotPolicy, f.gotActor, f.gotTitle, f.gotContent = policy, actor, title, content
	return f.result, f.err
}

func (f *fakePolicyAppendixBackend) SaveDraft(_ context.Context, id, actor uuid.UUID, title, content string) (domain.PolicyAppendix, bool, error) {
	f.calls++
	f.gotID, f.gotActor, f.gotTitle, f.gotContent = id, actor, title, content
	return f.result, f.changed, f.err
}

func (f *fakePolicyAppendixBackend) CheckpointDraft(_ context.Context, id, policy, actor uuid.UUID, content string) (domain.PolicyAppendix, bool, error) {
	f.calls++
	f.checkpoints++
	f.gotID, f.gotPolicy, f.gotActor, f.gotContent = id, policy, actor, content
	return f.result, f.changed, f.err
}

func (f *fakePolicyAppendixBackend) Publish(_ context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, error) {
	f.calls++
	f.gotID, f.gotActor = id, actor
	return f.result, f.err
}

func (f *fakePolicyAppendixBackend) Retire(_ context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, bool, error) {
	f.calls++
	f.gotID, f.gotActor = id, actor
	return f.result, f.changed, f.err
}

func (f *fakePolicyAppendixBackend) Restore(_ context.Context, id uuid.UUID) (domain.PolicyAppendix, bool, error) {
	f.calls++
	f.gotID = id
	return f.result, f.changed, f.err
}

func (f *fakePolicyAppendixBackend) Delete(_ context.Context, id uuid.UUID) (domain.PolicyAppendix, error) {
	f.calls++
	f.gotID = id
	return f.result, f.err
}

func (f *fakePolicyAppendixBackend) ReorderAppendices(_ context.Context, policy uuid.UUID, ids []uuid.UUID) ([]domain.PolicyAppendix, error) {
	f.calls++
	f.gotPolicy, f.gotOrder = policy, ids
	return f.list, f.err
}

type appendixFixture struct {
	h      *grpcsvc.PolicyAppendixHandler
	b      *fakePolicyAppendixBackend
	audit  *auditSink
	policy uuid.UUID
	id     uuid.UUID
	bob    uuid.UUID
	alice  uuid.UUID
	at     time.Time
}

func newAppendixFixture() *appendixFixture {
	f := &appendixFixture{
		b: &fakePolicyAppendixBackend{}, audit: &auditSink{}, policy: uuid.New(), id: uuid.New(),
		bob: uuid.New(), alice: uuid.New(), at: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	f.h = grpcsvc.NewPolicyAppendixHandler(f.b, f.audit)
	f.b.result = f.appendix()
	return f
}

func (f *appendixFixture) row(no int, state domain.AppendixState, title string, editors ...uuid.UUID) domain.PolicyAppendixVersion {
	v := domain.PolicyAppendixVersion{
		ID: uuid.New(), AppendixID: f.id, VersionNo: no, State: state, Title: title, ContentJSON: `{"text":"x"}`,
		CreatedAt: f.at, CreatedBy: f.bob, UpdatedAt: f.at, UpdatedBy: f.bob, Editors: editors,
	}
	if state == domain.AppendixPublished {
		v.PublishedAt, v.PublishedBy = &f.at, &f.alice
	}
	return v
}

// appendix is a live appendix with published v1, v2 and a draft v3.
func (f *appendixFixture) appendix() domain.PolicyAppendix {
	return domain.PolicyAppendix{
		ID: f.id, PolicyID: f.policy, Letter: "B", OrderIndex: 1, CreatedAt: f.at, CreatedBy: f.bob,
		Versions: []domain.PolicyAppendixVersion{
			f.row(1, domain.AppendixPublished, "Glossary", f.bob),
			f.row(2, domain.AppendixPublished, "Glossary v2", f.bob, f.alice),
			f.row(3, domain.AppendixDraft, "Glossary v3", f.alice),
		},
	}
}

func (f *appendixFixture) events() []audit.Event { return f.audit.evs }

// appendices.test.ts "keeps showing the published appendix while a newer draft is being written"
// and appendix-titles-contributors.test.ts "lists the published versions newest first".
func TestPolicyAppendixHandler_ListShapesTheAppendix(t *testing.T) {
	f := newAppendixFixture()
	retired := f.at.Add(time.Hour)
	gone := f.appendix()
	gone.ID, gone.Letter, gone.RetiredAt = uuid.New(), "", &retired
	f.b.list = []domain.PolicyAppendix{f.appendix(), gone}

	resp, err := f.h.ListPolicyAppendices(context.Background(),
		&corev1.ListPolicyAppendicesRequest{PolicyId: f.policy.String(), IncludeRetired: true})
	require.NoError(t, err)
	require.Equal(t, f.policy, f.b.gotPolicy)
	require.True(t, f.b.gotRetired)
	require.Len(t, resp.GetAppendices(), 2)

	a := resp.GetAppendices()[0]
	require.Equal(t, f.id.String(), a.GetId())
	require.Equal(t, f.policy.String(), a.GetPolicyId())
	require.Equal(t, "B", a.GetLetter())
	require.EqualValues(t, 1, a.GetOrderIndex())
	require.Nil(t, a.GetRetiredAt())
	require.EqualValues(t, 2, a.GetPublished().GetVersionNo(), "the latest published row")
	require.Equal(t, "Glossary v2", a.GetPublished().GetTitle())
	require.Equal(t, corev1.AppendixVersionState_APPENDIX_VERSION_STATE_PUBLISHED, a.GetPublished().GetState())
	require.Equal(t, f.alice.String(), a.GetPublished().GetPublishedBy())
	require.NotNil(t, a.GetPublished().GetPublishedAt())
	require.EqualValues(t, 3, a.GetDraft().GetVersionNo())
	require.Equal(t, corev1.AppendixVersionState_APPENDIX_VERSION_STATE_DRAFT, a.GetDraft().GetState())
	require.Nil(t, a.GetDraft().GetPublishedAt())

	var all, pub []int32
	for _, v := range a.GetVersions() {
		all = append(all, v.GetVersionNo())
	}
	for _, v := range a.GetPublishedVersions() {
		pub = append(pub, v.GetVersionNo())
	}
	require.Equal(t, []int32{1, 2, 3}, all, "the table is oldest first and holds the draft")
	require.Equal(t, []int32{2, 1}, pub, "published versions are newest first without the draft")
	require.Equal(t, []string{f.bob.String(), f.alice.String()}, a.GetContributorIds())
	require.Equal(t, []string{f.bob.String(), f.alice.String()}, a.GetVersions()[1].GetContributorIds())

	r := resp.GetAppendices()[1]
	require.Equal(t, "", r.GetLetter(), "a retired appendix has no letter")
	require.NotNil(t, r.GetRetiredAt())
}

func TestPolicyAppendixHandler_ListWithoutPublishedOrDraft(t *testing.T) {
	f := newAppendixFixture()
	a := f.appendix()
	a.Versions = a.Versions[2:]
	f.b.list = []domain.PolicyAppendix{a}
	resp, err := f.h.ListPolicyAppendices(context.Background(), &corev1.ListPolicyAppendicesRequest{PolicyId: f.policy.String()})
	require.NoError(t, err)
	require.False(t, f.b.gotRetired)
	require.Nil(t, resp.GetAppendices()[0].GetPublished())
	require.NotNil(t, resp.GetAppendices()[0].GetDraft())
	require.Empty(t, resp.GetAppendices()[0].GetPublishedVersions())
}

func TestPolicyAppendixHandler_ListRejectsABadPolicyID(t *testing.T) {
	f := newAppendixFixture()
	_, err := f.h.ListPolicyAppendices(context.Background(), &corev1.ListPolicyAppendicesRequest{PolicyId: "nope"})
	requireCode(t, err, codes.InvalidArgument)
	require.Zero(t, f.b.calls)
}

// appendices.test.ts "starts a new appendix as a draft".
func TestPolicyAppendixHandler_CreateAuditsAndPassesTheTitleOn(t *testing.T) {
	f := newAppendixFixture()
	resp, err := f.h.CreatePolicyAppendix(context.Background(), &corev1.CreatePolicyAppendixRequest{
		PolicyId: f.policy.String(), Title: "  ", ContentJson: `{"text":"Terms."}`, ActorUserId: f.bob.String(),
	})
	require.NoError(t, err)
	require.Equal(t, f.id.String(), resp.GetAppendix().GetId())
	require.Equal(t, f.policy, f.b.gotPolicy)
	require.Equal(t, f.bob, f.b.gotActor)
	require.Equal(t, "  ", f.b.gotTitle, "the store decides what a blank title means")

	require.Len(t, f.events(), 1)
	ev := f.events()[0]
	require.Equal(t, "appendix.created", ev.Action)
	require.Equal(t, audit.TierAudit, ev.Tier)
	require.Equal(t, f.bob.String(), ev.ActorUserID)
	require.Equal(t, "policy_appendix:"+f.id.String(), ev.Subject)
	require.Equal(t, f.policy.String(), ev.Attributes["policy_id"])
}

func TestPolicyAppendixHandler_CreateRefusesBadInput(t *testing.T) {
	for name, req := range map[string]*corev1.CreatePolicyAppendixRequest{
		"policy id":   {PolicyId: "x", ContentJson: "{}", ActorUserId: uuid.NewString()},
		"no actor":    {PolicyId: uuid.NewString(), ContentJson: "{}"},
		"bad actor":   {PolicyId: uuid.NewString(), ContentJson: "{}", ActorUserId: "x"},
		"no content":  {PolicyId: uuid.NewString(), ActorUserId: uuid.NewString()},
		"not json":    {PolicyId: uuid.NewString(), ContentJson: "{", ActorUserId: uuid.NewString()},
		"blank space": {PolicyId: uuid.NewString(), ContentJson: "  ", ActorUserId: uuid.NewString()},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppendixFixture()
			_, err := f.h.CreatePolicyAppendix(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
			require.Zero(t, f.b.calls)
			require.Empty(t, f.events())
		})
	}
}

func TestPolicyAppendixHandler_CreateOnAMissingPolicyIsNotFound(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrAppendixPolicyNotFound
	_, err := f.h.CreatePolicyAppendix(context.Background(), &corev1.CreatePolicyAppendixRequest{
		PolicyId: f.policy.String(), ContentJson: "{}", ActorUserId: f.bob.String(),
	})
	requireCode(t, err, codes.NotFound)
	require.Empty(t, f.events())
}

// appendices.test.ts "autosaves into the draft".
func TestPolicyAppendixHandler_SaveAuditsAChange(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = true
	resp, err := f.h.SaveAppendixDraft(context.Background(), &corev1.SaveAppendixDraftRequest{
		Id: f.id.String(), Title: "Glossary of terms", ContentJson: `{"text":"More."}`, ActorUserId: f.alice.String(),
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, resp.GetAppendix().GetDraft().GetVersionNo())
	require.Equal(t, f.id, f.b.gotID)
	require.Equal(t, "Glossary of terms", f.b.gotTitle)
	require.Len(t, f.events(), 1)
	require.Equal(t, "appendix.saved", f.events()[0].Action)
	require.Equal(t, f.alice.String(), f.events()[0].ActorUserID)
	for k, v := range f.events()[0].Attributes {
		require.NotContains(t, k+v, "Glossary of terms", "no title or text in an audit event")
	}
}

// appendices.test.ts "ignores a save of the published title and content" (bug log 246).
func TestPolicyAppendixHandler_UnchangedSaveWritesNoAuditEvent(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = false
	_, err := f.h.SaveAppendixDraft(context.Background(), &corev1.SaveAppendixDraftRequest{
		Id: f.id.String(), Title: "Glossary", ContentJson: "{}", ActorUserId: f.bob.String(),
	})
	require.NoError(t, err)
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_SaveRefusesBadInput(t *testing.T) {
	for name, req := range map[string]*corev1.SaveAppendixDraftRequest{
		"bad id":      {Id: "x", Title: "t", ContentJson: "{}", ActorUserId: uuid.NewString()},
		"blank title": {Id: uuid.NewString(), Title: "   ", ContentJson: "{}", ActorUserId: uuid.NewString()},
		"no actor":    {Id: uuid.NewString(), Title: "t", ContentJson: "{}"},
		"not json":    {Id: uuid.NewString(), Title: "t", ContentJson: "{", ActorUserId: uuid.NewString()},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppendixFixture()
			_, err := f.h.SaveAppendixDraft(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
			require.Zero(t, f.b.calls)
		})
	}
}

func TestPolicyAppendixHandler_SaveOnARetiredAppendixIsFailedPrecondition(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrAppendixRetired
	_, err := f.h.SaveAppendixDraft(context.Background(), &corev1.SaveAppendixDraftRequest{
		Id: f.id.String(), Title: "t", ContentJson: "{}", ActorUserId: f.bob.String(),
	})
	requireCode(t, err, codes.FailedPrecondition)
	require.Empty(t, f.events())
}

// appendices.test.ts "publishes to the version readers see, with who and when".
func TestPolicyAppendixHandler_PublishAudits(t *testing.T) {
	f := newAppendixFixture()
	resp, err := f.h.PublishAppendix(context.Background(), &corev1.PublishAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	require.NoError(t, err)
	require.NotNil(t, resp.GetAppendix())
	require.Len(t, f.events(), 1)
	require.Equal(t, "appendix.published", f.events()[0].Action)
	require.Equal(t, f.alice.String(), f.events()[0].ActorUserID)
	require.Equal(t, f.policy.String(), f.events()[0].Attributes["policy_id"])
}

func TestPolicyAppendixHandler_PublishWithoutADraftIsFailedPrecondition(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrAppendixNoDraft
	_, err := f.h.PublishAppendix(context.Background(), &corev1.PublishAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	requireCode(t, err, codes.FailedPrecondition)
	require.Empty(t, f.events())
}

// appendices.test.ts "keeps a retired appendix, hides it from readers, and re-letters the rest"
// and "restores it to the end of the list".
func TestPolicyAppendixHandler_RetireAndRestoreAudit(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = true
	_, err := f.h.RetireAppendix(context.Background(), &corev1.RetireAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
	_, err = f.h.RestoreAppendix(context.Background(), &corev1.RestoreAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
	require.Len(t, f.events(), 2)
	require.Equal(t, "appendix.retired", f.events()[0].Action)
	require.Equal(t, "appendix.restored", f.events()[1].Action)
	require.Equal(t, f.bob.String(), f.events()[1].ActorUserID)
}

func TestPolicyAppendixHandler_RetireOrRestoreThatChangedNothingWritesNoEvent(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = false
	_, err := f.h.RetireAppendix(context.Background(), &corev1.RetireAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
	_, err = f.h.RestoreAppendix(context.Background(), &corev1.RestoreAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_UnknownAppendixIsNotFound(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrPolicyAppendixNotFound
	ctx := context.Background()
	id, actor := f.id.String(), f.bob.String()
	_, err := f.h.SaveAppendixDraft(ctx, &corev1.SaveAppendixDraftRequest{Id: id, Title: "t", ContentJson: "{}", ActorUserId: actor})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.PublishAppendix(ctx, &corev1.PublishAppendixRequest{Id: id, ActorUserId: actor})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.RetireAppendix(ctx, &corev1.RetireAppendixRequest{Id: id, ActorUserId: actor})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.RestoreAppendix(ctx, &corev1.RestoreAppendixRequest{Id: id, ActorUserId: actor})
	requireCode(t, err, codes.NotFound)
}

func TestPolicyAppendixHandler_StoreFaultIsCodedAndNotLeaked(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = errors.New("dial tcp core-pg:5432: connect: connection refused")
	_, err := f.h.ListPolicyAppendices(context.Background(), &corev1.ListPolicyAppendicesRequest{PolicyId: f.policy.String()})
	requireStoreUnavailable(t, err, "list_policy_appendices")
	require.NotContains(t, err.Error(), "connection refused")
	_, err = f.h.PublishAppendix(context.Background(), &corev1.PublishAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	requireStoreUnavailable(t, err, "publish_appendix")
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_ARefusedAuditDoesNotUndoTheWrite(t *testing.T) {
	f := newAppendixFixture()
	f.audit.err = errors.New("broker down")
	_, err := f.h.PublishAppendix(context.Background(), &corev1.PublishAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
}

func TestPolicyAppendixHandler_NoAuditorIsFine(t *testing.T) {
	f := newAppendixFixture()
	h := grpcsvc.NewPolicyAppendixHandler(f.b, nil)
	_, err := h.PublishAppendix(context.Background(), &corev1.PublishAppendixRequest{Id: f.id.String(), ActorUserId: f.bob.String()})
	require.NoError(t, err)
}

// appendices.test.ts "order" / "reorders the appendices and re-letters them".
func TestPolicyAppendixHandler_ReorderReturnsTheNewOrderAndAudits(t *testing.T) {
	f := newAppendixFixture()
	second := f.appendix()
	second.ID, second.Letter, second.OrderIndex = uuid.New(), "A", 0
	f.b.list = []domain.PolicyAppendix{second, f.appendix()}
	resp, err := f.h.ReorderPolicyAppendices(context.Background(), &corev1.ReorderPolicyAppendicesRequest{
		PolicyId: f.policy.String(), OrderedIds: []string{second.ID.String(), f.id.String()}, ActorUserId: f.bob.String(),
	})
	require.NoError(t, err)
	require.Equal(t, f.policy, f.b.gotPolicy)
	require.Equal(t, []uuid.UUID{second.ID, f.id}, f.b.gotOrder)
	require.Len(t, resp.GetAppendices(), 2)
	require.Equal(t, second.ID.String(), resp.GetAppendices()[0].GetId())
	require.Equal(t, "A", resp.GetAppendices()[0].GetLetter())
	require.Len(t, f.events(), 1)
	ev := f.events()[0]
	require.Equal(t, "appendix.reordered", ev.Action)
	require.Equal(t, f.bob.String(), ev.ActorUserID)
	require.Equal(t, "policy:"+f.policy.String(), ev.Subject)
	require.Equal(t, "2", ev.Attributes["appendix_count"])
}

// appendices.test.ts "makes no draft on a reorder, a retire or a restore": the
// handler only reorders, it never saves or publishes.
func TestPolicyAppendixHandler_ReorderTouchesNoVersions(t *testing.T) {
	f := newAppendixFixture()
	_, err := f.h.ReorderPolicyAppendices(context.Background(), &corev1.ReorderPolicyAppendicesRequest{
		PolicyId: f.policy.String(), ActorUserId: f.bob.String(),
	})
	require.NoError(t, err)
	require.Equal(t, 1, f.b.calls)
}

func TestPolicyAppendixHandler_ReorderRefusesBadInput(t *testing.T) {
	f := newAppendixFixture()
	ctx := context.Background()
	good := uuid.NewString()
	for name, req := range map[string]*corev1.ReorderPolicyAppendicesRequest{
		"bad policy": {PolicyId: "x", ActorUserId: f.bob.String()},
		"bad actor":  {PolicyId: good, ActorUserId: "x"},
		"bad id":     {PolicyId: good, OrderedIds: []string{good, "x"}, ActorUserId: f.bob.String()},
	} {
		_, err := f.h.ReorderPolicyAppendices(ctx, req)
		require.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	require.Zero(t, f.b.calls)
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_ReorderMapsStoreErrorsAndAuditsNothing(t *testing.T) {
	f := newAppendixFixture()
	req := &corev1.ReorderPolicyAppendicesRequest{PolicyId: f.policy.String(), ActorUserId: f.bob.String()}
	for err, want := range map[error]codes.Code{
		store.ErrAppendixReorderMismatch: codes.InvalidArgument,
		store.ErrAppendixPolicyNotFound:  codes.NotFound,
	} {
		f.b.err = err
		_, got := f.h.ReorderPolicyAppendices(context.Background(), req)
		require.Equal(t, want, status.Code(got), err.Error())
	}
	f.b.err = errors.New("dial tcp core-pg:5432: connect: connection refused")
	_, got := f.h.ReorderPolicyAppendices(context.Background(), req)
	requireStoreUnavailable(t, got, "reorder_policy_appendices")
	require.NotContains(t, got.Error(), "connection refused")
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_ReorderAuditFailureDoesNotUndoIt(t *testing.T) {
	f := newAppendixFixture()
	f.audit.err = errors.New("audit down")
	_, err := f.h.ReorderPolicyAppendices(context.Background(), &corev1.ReorderPolicyAppendicesRequest{
		PolicyId: f.policy.String(), ActorUserId: f.bob.String(),
	})
	require.NoError(t, err)
}

// core#85: deleting for good removes the appendix and writes one audit event
// that names the actor, the policy and the appendix, and no title or text.
func TestPolicyAppendixHandler_DeleteAuditsTheRemoval(t *testing.T) {
	f := newAppendixFixture()
	_, err := f.h.DeletePolicyAppendix(context.Background(), &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	require.NoError(t, err)
	require.Equal(t, f.id, f.b.gotID)
	require.Len(t, f.events(), 1)
	ev := f.events()[0]
	require.Equal(t, "appendix.deleted", ev.Action)
	require.Equal(t, audit.TierAudit, ev.Tier)
	require.Equal(t, f.alice.String(), ev.ActorUserID)
	require.Equal(t, "policy_appendix:"+f.id.String(), ev.Subject)
	require.Equal(t, f.policy.String(), ev.Attributes["policy_id"])
	require.Equal(t, f.id.String(), ev.Attributes["appendix_id"])
	for k, v := range ev.Attributes {
		require.NotContains(t, v, "Glossary", "attribute %s must not carry a title", k)
	}
}

func TestPolicyAppendixHandler_DeleteRefusesAnAppendixThatIsNotRetired(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrAppendixNotRetired
	_, err := f.h.DeletePolicyAppendix(context.Background(), &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	requireCode(t, err, codes.FailedPrecondition)
	require.Empty(t, f.events(), "a refused delete writes no event")
}

func TestPolicyAppendixHandler_DeleteUnknownAppendixIsNotFound(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrPolicyAppendixNotFound
	_, err := f.h.DeletePolicyAppendix(context.Background(), &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	requireCode(t, err, codes.NotFound)
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_DeleteRefusesBadIDsBeforeTheStore(t *testing.T) {
	f := newAppendixFixture()
	ctx := context.Background()
	_, err := f.h.DeletePolicyAppendix(ctx, &corev1.DeletePolicyAppendixRequest{Id: "nope", ActorUserId: f.alice.String()})
	requireCode(t, err, codes.InvalidArgument)
	_, err = f.h.DeletePolicyAppendix(ctx, &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: ""})
	requireCode(t, err, codes.InvalidArgument)
	require.Zero(t, f.b.calls, "the store is never reached")
	require.Empty(t, f.events())
}

func TestPolicyAppendixHandler_DeleteStoreFaultIsCodedAndWritesNoEvent(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = errors.New("dial tcp core-pg:5432: connect: connection refused")
	_, err := f.h.DeletePolicyAppendix(context.Background(), &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "5432")
	require.Empty(t, f.events())
}

// A failed audit emit does not undo a delete that already happened.
func TestPolicyAppendixHandler_DeleteSurvivesAFailedAuditEmit(t *testing.T) {
	f := newAppendixFixture()
	f.audit.err = errors.New("audit down")
	_, err := f.h.DeletePolicyAppendix(context.Background(), &corev1.DeletePolicyAppendixRequest{Id: f.id.String(), ActorUserId: f.alice.String()})
	require.NoError(t, err)
}
