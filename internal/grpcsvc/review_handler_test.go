// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
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

// fakeReviewBackend is an in-memory reviewBackend with the store's behaviour.
type fakeReviewBackend struct {
	mu       sync.Mutex
	versions map[uuid.UUID]bool
	threads  []domain.ReviewThread
	fail     error
}

func newFakeReviewBackend(versions ...uuid.UUID) *fakeReviewBackend {
	f := &fakeReviewBackend{versions: map[uuid.UUID]bool{}}
	for _, v := range versions {
		f.versions[v] = true
	}
	return f
}

func (f *fakeReviewBackend) find(id uuid.UUID) int {
	return slices.IndexFunc(f.threads, func(t domain.ReviewThread) bool { return t.ID == id })
}

func (f *fakeReviewBackend) CreateThread(_ context.Context, t domain.ReviewThread) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return domain.ReviewThread{}, f.fail
	}
	if !f.versions[t.PolicyVersionID] {
		return domain.ReviewThread{}, store.ErrReviewVersionNotFound
	}
	t.ID = uuid.New()
	t.CreatedAt = time.Now()
	for i := range t.Messages {
		t.Messages[i].ID = uuid.New()
		t.Messages[i].CreatedAt = t.CreatedAt
	}
	f.threads = append(f.threads, t)
	return t, nil
}

func (f *fakeReviewBackend) GetThread(_ context.Context, id uuid.UUID) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return domain.ReviewThread{}, f.fail
	}
	if i := f.find(id); i >= 0 {
		return f.threads[i], nil
	}
	return domain.ReviewThread{}, store.ErrReviewThreadNotFound
}

func (f *fakeReviewBackend) ListThreads(_ context.Context, v, approver uuid.UUID) ([]domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	out := []domain.ReviewThread{}
	for _, t := range f.threads {
		if t.PolicyVersionID == v && (approver == uuid.Nil || t.ApproverUserID == approver) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeReviewBackend) AddMessage(_ context.Context, id uuid.UUID, m domain.ReviewMessage) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		return domain.ReviewThread{}, store.ErrReviewThreadNotFound
	}
	m.ID, m.CreatedAt = uuid.New(), time.Now()
	f.threads[i].Messages = append(f.threads[i].Messages, m)
	return f.threads[i], nil
}

func (f *fakeReviewBackend) SetResolved(_ context.Context, id uuid.UUID, resolved bool) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		return domain.ReviewThread{}, store.ErrReviewThreadNotFound
	}
	f.threads[i].Resolved = resolved
	return f.threads[i], nil
}

func (f *fakeReviewBackend) DeleteThread(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		return store.ErrReviewThreadNotFound
	}
	f.threads = slices.Delete(f.threads, i, i+1)
	return nil
}

type auditSink struct {
	mu  sync.Mutex
	evs []audit.Event
	err error
}

func (a *auditSink) Emit(_ context.Context, ev audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return a.err
}

type reviewFixture struct {
	h       *grpcsvc.ReviewHandler
	backend *fakeReviewBackend
	audit   *auditSink
	version string
	// the people: bob drafts, charlie and alice hold seats
	bob, charlie, alice string
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	v := uuid.New()
	f := &reviewFixture{
		backend: newFakeReviewBackend(v), audit: &auditSink{}, version: v.String(),
		bob: uuid.NewString(), charlie: uuid.NewString(), alice: uuid.NewString(),
	}
	f.h = grpcsvc.NewReviewHandler(f.backend, f.audit, domain.ReviewLimits{BodyMax: 50, QuoteMax: 30})
	return f
}

func (f *reviewFixture) start(t *testing.T, actor string, side corev1.ReviewSide, approver, body string) *corev1.ReviewThread {
	t.Helper()
	resp, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: actor, ActorSide: side, PolicyVersionId: f.version,
		Quote: "seven years", Occurrence: 2, Body: body, ApproverUserId: approver,
	})
	require.NoError(t, err)
	return resp.GetThread()
}

func (f *reviewFixture) charlieStarts(t *testing.T) *corev1.ReviewThread {
	return f.start(t, f.charlie, corev1.ReviewSide_REVIEW_SIDE_REVIEWER, f.charlie, "What is this?")
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, want, status.Code(err), "%v", err)
}

// mock: inline review threads / lets an approver start a thread that anchors on a quote and which match of it
func TestAddReviewThreadAnchorsOnQuoteAndOccurrence(t *testing.T) {
	f := newReviewFixture(t)
	th := f.charlieStarts(t)
	require.NotEmpty(t, th.GetId())
	require.Equal(t, "seven years", th.GetQuote())
	require.Equal(t, int32(2), th.GetOccurrence())
	require.False(t, th.GetResolved())
	require.Equal(t, f.version, th.GetPolicyVersionId())
	require.Equal(t, f.charlie, th.GetApproverUserId())
	require.Equal(t, f.charlie, th.GetCreatedBy())
	require.NotNil(t, th.GetCreatedAt())
	require.Len(t, th.GetMessages(), 1)
	require.Equal(t, "What is this?", th.GetMessages()[0].GetBody())
	require.Equal(t, f.charlie, th.GetMessages()[0].GetAuthorUserId())
}

// mock: lists a version's threads for its drafter
func TestListReviewThreadsForTheAuthorSeesEveryThread(t *testing.T) {
	f := newReviewFixture(t)
	th := f.charlieStarts(t)
	resp, err := f.h.ListReviewThreads(context.Background(), &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	require.NoError(t, err)
	require.Len(t, resp.GetThreads(), 1)
	require.Equal(t, th.GetId(), resp.GetThreads()[0].GetId())
}

// mock: an unknown policy version is NotFound, not an empty list
func TestListReviewThreadsOnUnknownVersionIsNotFound(t *testing.T) {
	f := newReviewFixture(t)
	f.backend.fail = store.ErrReviewVersionNotFound
	_, err := f.h.ListReviewThreads(context.Background(), &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// mock: lets the drafter reply, resolve and reopen
func TestReplyResolveAndReopen(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	th := f.charlieStarts(t)

	reply, err := f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(), Body: "Fixed in the next draft.",
	})
	require.NoError(t, err)
	require.Len(t, reply.GetThread().GetMessages(), 2)
	require.Equal(t, "Fixed in the next draft.", reply.GetThread().GetMessages()[1].GetBody())
	require.Equal(t, f.bob, reply.GetThread().GetMessages()[1].GetAuthorUserId())

	done, err := f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(), Resolved: true,
	})
	require.NoError(t, err)
	require.True(t, done.GetThread().GetResolved())
	again, err := f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(), Resolved: false,
	})
	require.NoError(t, err)
	require.False(t, again.GetThread().GetResolved())
}

// mock: refuses an empty comment and an empty quote
func TestAddReviewThreadRefusesEmptyAndInvalidInput(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	req := func(mod func(*corev1.AddReviewThreadRequest)) *corev1.AddReviewThreadRequest {
		r := &corev1.AddReviewThreadRequest{
			ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, PolicyVersionId: f.version,
			Quote: "x", Occurrence: 1, Body: "Why?", ApproverUserId: f.charlie,
		}
		mod(r)
		return r
	}
	for name, r := range map[string]*corev1.AddReviewThreadRequest{
		"empty comment":        req(func(r *corev1.AddReviewThreadRequest) { r.Body = "  " }),
		"empty quote":          req(func(r *corev1.AddReviewThreadRequest) { r.Quote = " " }),
		"occurrence zero":      req(func(r *corev1.AddReviewThreadRequest) { r.Occurrence = 0 }),
		"body over the limit":  req(func(r *corev1.AddReviewThreadRequest) { r.Body = strings.Repeat("a", 51) }),
		"quote over the limit": req(func(r *corev1.AddReviewThreadRequest) { r.Quote = strings.Repeat("a", 31) }),
		"no side":              req(func(r *corev1.AddReviewThreadRequest) { r.ActorSide = corev1.ReviewSide_REVIEW_SIDE_UNSPECIFIED }),
		"bad version id":       req(func(r *corev1.AddReviewThreadRequest) { r.PolicyVersionId = "nope" }),
		"bad actor id":         req(func(r *corev1.AddReviewThreadRequest) { r.ActorUserId = "" }),
		"bad approver id":      req(func(r *corev1.AddReviewThreadRequest) { r.ApproverUserId = "nope" }),
	} {
		_, err := f.h.AddReviewThread(ctx, r)
		requireCode(t, err, codes.InvalidArgument)
		_ = name
	}
	require.Empty(t, f.backend.threads, "nothing was stored")
	require.Empty(t, f.audit.evs, "nothing was audited")
}

func TestAddReviewThreadOnUnknownVersion(t *testing.T) {
	f := newReviewFixture(t)
	_, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, PolicyVersionId: uuid.NewString(),
		Quote: "x", Occurrence: 1, Body: "Hi", ApproverUserId: f.charlie,
	})
	requireCode(t, err, codes.NotFound)
}

// mock: keeps someone outside the review from starting or answering a thread
// (the seat check is the gateway's; core refuses a reviewer who is not the thread's approver)
func TestReviewerWhoIsNotTheApproverIsRefused(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_, err := f.h.AddReviewThread(ctx, &corev1.AddReviewThreadRequest{
		ActorUserId: f.alice, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, PolicyVersionId: f.version,
		Quote: "x", Occurrence: 1, Body: "Hi", ApproverUserId: f.charlie,
	})
	requireCode(t, err, codes.PermissionDenied)
	require.Empty(t, f.backend.threads)

	th := f.charlieStarts(t)
	_, err = f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{
		ActorUserId: uuid.NewString(), ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(), Body: "Hi",
	})
	requireCode(t, err, codes.PermissionDenied)
}

// mock: lets only the person who started a thread delete it
func TestOnlyTheStarterDeletes(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	th := f.charlieStarts(t)

	_, err := f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(),
	})
	requireCode(t, err, codes.PermissionDenied)
	require.Len(t, f.backend.threads, 1)

	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(),
	})
	require.NoError(t, err)
	left, err := f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	require.NoError(t, err)
	require.Empty(t, left.GetThreads())

	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(),
	})
	requireCode(t, err, codes.NotFound)
}

// mock: whose thread it is / is with the approver who started it, and the author sees it with who wrote it
func TestThreadIsWithTheApproverWhoStartedItAndTheAuthorSeesWhoWrote(t *testing.T) {
	f := newReviewFixture(t)
	th := f.charlieStarts(t)
	require.Equal(t, f.charlie, th.GetApproverUserId())
	resp, err := f.h.ListReviewThreads(context.Background(), &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	require.NoError(t, err)
	require.Len(t, resp.GetThreads(), 1)
	require.Equal(t, f.charlie, resp.GetThreads()[0].GetMessages()[0].GetAuthorUserId())
}

// mock: whose thread it is / is not shown to another approver on the same step
func TestListFilterKeepsAnotherApproversThreadsOut(t *testing.T) {
	f := newReviewFixture(t)
	f.charlieStarts(t)
	ctx := context.Background()
	other, err := f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version, ApproverUserId: f.alice})
	require.NoError(t, err)
	require.Empty(t, other.GetThreads())
	own, err := f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version, ApproverUserId: f.charlie})
	require.NoError(t, err)
	require.Len(t, own.GetThreads(), 1)

	_, err = f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: "nope"})
	requireCode(t, err, codes.InvalidArgument)
	_, err = f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version, ApproverUserId: "nope"})
	requireCode(t, err, codes.InvalidArgument)
}

// mock: whose thread it is / cannot be answered, resolved or deleted by another approver
func TestAnotherApproverCannotReplyResolveOrDelete(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	th := f.charlieStarts(t)
	reviewer := corev1.ReviewSide_REVIEW_SIDE_REVIEWER

	_, err := f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{ActorUserId: f.alice, ActorSide: reviewer, ThreadId: th.GetId(), Body: "Me too."})
	requireCode(t, err, codes.PermissionDenied)
	_, err = f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{ActorUserId: f.alice, ActorSide: reviewer, ThreadId: th.GetId(), Resolved: true})
	requireCode(t, err, codes.PermissionDenied)
	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{ActorUserId: f.alice, ActorSide: reviewer, ThreadId: th.GetId()})
	requireCode(t, err, codes.PermissionDenied)

	require.Len(t, f.backend.threads, 1)
	require.Len(t, f.backend.threads[0].Messages, 1)
	require.False(t, f.backend.threads[0].Resolved)
	require.Empty(t, f.audit.evs[1:], "only the creation was audited; the refusals changed nothing")
}

// mock: whose thread it is / lets the author write to one approver, who alone sees it
func TestAuthorWritesToOneApproverWhoAloneSeesIt(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	th := f.start(t, f.bob, corev1.ReviewSide_REVIEW_SIDE_AUTHOR, f.charlie, "Can you check this wording?")
	require.Equal(t, f.charlie, th.GetApproverUserId())
	require.Equal(t, f.bob, th.GetCreatedBy())

	charlie, err := f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version, ApproverUserId: f.charlie})
	require.NoError(t, err)
	require.Len(t, charlie.GetThreads(), 1)
	alice, err := f.h.ListReviewThreads(ctx, &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version, ApproverUserId: f.alice})
	require.NoError(t, err)
	require.Empty(t, alice.GetThreads())

	// the approver it is with may answer it, but only the author who started it may delete it
	_, err = f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(), Body: "Looks fine.",
	})
	require.NoError(t, err)
	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(),
	})
	requireCode(t, err, codes.PermissionDenied)
}

// mock: whose thread it is / makes the author say which approver (core requires approver_user_id; the seat check is the gateway's)
func TestAuthorMustNameTheApprover(t *testing.T) {
	f := newReviewFixture(t)
	_, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, PolicyVersionId: f.version,
		Quote: "x", Occurrence: 1, Body: "Check?",
	})
	requireCode(t, err, codes.InvalidArgument)
}

func TestThreadCallsOnAnUnknownOrMalformedThread(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	author := corev1.ReviewSide_REVIEW_SIDE_AUTHOR
	_, err := f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: uuid.NewString(), Body: "Hi"})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: uuid.NewString(), Resolved: true})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: uuid.NewString()})
	requireCode(t, err, codes.NotFound)
	_, err = f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: "nope", Body: "Hi"})
	requireCode(t, err, codes.InvalidArgument)
}

func TestReplyRefusesEmptyOrTooLongBody(t *testing.T) {
	f := newReviewFixture(t)
	th := f.charlieStarts(t)
	for _, body := range []string{" ", strings.Repeat("a", 51)} {
		_, err := f.h.ReplyReviewThread(context.Background(), &corev1.ReplyReviewThreadRequest{
			ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(), Body: body,
		})
		requireCode(t, err, codes.InvalidArgument)
	}
	require.Len(t, f.backend.threads[0].Messages, 1)
}

func TestReviewStoreFaultIsCodedNotLeaked(t *testing.T) {
	f := newReviewFixture(t)
	f.backend.fail = errors.New("dial tcp core-pg:5432: connection refused")
	_, err := f.h.ListReviewThreads(context.Background(), &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	requireCode(t, err, codes.Internal)
	require.NotContains(t, err.Error(), "core-pg")
}

// Audit is the activity tier with ids and outcomes only, never a body or a quote.
func TestReviewAuditCarriesIdsNeverBodiesOrQuotes(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	secret := "SECRET-BODY-TEXT"
	th := f.start(t, f.charlie, corev1.ReviewSide_REVIEW_SIDE_REVIEWER, f.charlie, secret)
	author := corev1.ReviewSide_REVIEW_SIDE_AUTHOR
	_, err := f.h.ReplyReviewThread(ctx, &corev1.ReplyReviewThreadRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: th.GetId(), Body: secret})
	require.NoError(t, err)
	_, err = f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: th.GetId(), Resolved: true})
	require.NoError(t, err)
	_, err = f.h.SetReviewThreadResolved(ctx, &corev1.SetReviewThreadResolvedRequest{ActorUserId: f.bob, ActorSide: author, ThreadId: th.GetId(), Resolved: false})
	require.NoError(t, err)
	_, err = f.h.DeleteReviewThread(ctx, &corev1.DeleteReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, ThreadId: th.GetId(),
	})
	require.NoError(t, err)

	var actions []string
	for _, ev := range f.audit.evs {
		actions = append(actions, ev.Action)
		require.Equal(t, audit.TierActivity, ev.Tier)
		require.Equal(t, "review_thread:"+th.GetId(), ev.Subject)
		require.Equal(t, th.GetId(), ev.Attributes["thread_id"])
		require.Equal(t, f.version, ev.Attributes["policy_version_id"])
		require.Equal(t, "ok", ev.Attributes["outcome"])
		for k, v := range ev.Attributes {
			require.NotContains(t, v, secret, k)
			require.NotContains(t, v, "seven years", k)
		}
		require.NotContains(t, ev.Subject+ev.ActorUserID+ev.GroupID, secret)
	}
	require.Equal(t, []string{
		"review.thread.created", "review.thread.replied", "review.thread.resolved", "review.thread.reopened", "review.thread.deleted",
	}, actions)
	require.Equal(t, f.charlie, f.audit.evs[0].ActorUserID)
	require.Equal(t, f.bob, f.audit.evs[1].ActorUserID)
	require.Equal(t, f.charlie, f.audit.evs[0].Attributes["approver_user_id"])
}

func TestReviewAuditFailureDoesNotUndoTheWrite(t *testing.T) {
	f := newReviewFixture(t)
	f.audit.err = errors.New("broker down")
	th := f.charlieStarts(t)
	require.NotEmpty(t, th.GetId())
	require.Len(t, f.backend.threads, 1)
}

func TestReviewHandlerWithoutAuditor(t *testing.T) {
	v := uuid.New()
	h := grpcsvc.NewReviewHandler(newFakeReviewBackend(v), nil, domain.DefaultReviewLimits)
	a := uuid.NewString()
	_, err := h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: a, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, PolicyVersionId: v.String(),
		Quote: "x", Occurrence: 1, Body: "Hi", ApproverUserId: a,
	})
	require.NoError(t, err)
}

// GetReviewThread reads one thread by id so the gateway can check a thread-only mutation.
func TestGetReviewThreadReturnsTheThreadWithItsMessages(t *testing.T) {
	f := newReviewFixture(t)
	th := f.charlieStarts(t)
	_, err := f.h.ReplyReviewThread(context.Background(), &corev1.ReplyReviewThreadRequest{
		ActorUserId: f.bob, ActorSide: corev1.ReviewSide_REVIEW_SIDE_AUTHOR, ThreadId: th.GetId(), Body: "Seven is the rule.",
	})
	require.NoError(t, err)
	before := len(f.audit.evs)

	resp, err := f.h.GetReviewThread(context.Background(), &corev1.GetReviewThreadRequest{ThreadId: th.GetId()})
	require.NoError(t, err)
	got := resp.GetThread()
	require.Equal(t, th.GetId(), got.GetId())
	require.Equal(t, f.version, got.GetPolicyVersionId())
	require.Equal(t, f.charlie, got.GetApproverUserId())
	require.Equal(t, f.charlie, got.GetCreatedBy())
	require.Equal(t, "seven years", got.GetQuote())
	require.Equal(t, int32(2), got.GetOccurrence())
	require.Len(t, got.GetMessages(), 2)
	require.Equal(t, "What is this?", got.GetMessages()[0].GetBody())
	require.Equal(t, "Seven is the rule.", got.GetMessages()[1].GetBody())
	require.Equal(t, before, len(f.audit.evs), "a read emits no audit event")
}

func TestGetReviewThreadOnAnUnknownThreadIsNotFound(t *testing.T) {
	f := newReviewFixture(t)
	_, err := f.h.GetReviewThread(context.Background(), &corev1.GetReviewThreadRequest{ThreadId: uuid.NewString()})
	requireCode(t, err, codes.NotFound)
}

func TestGetReviewThreadRefusesABlankOrMalformedId(t *testing.T) {
	f := newReviewFixture(t)
	for _, id := range []string{"", "   ", "nope", "1234"} {
		_, err := f.h.GetReviewThread(context.Background(), &corev1.GetReviewThreadRequest{ThreadId: id})
		requireCode(t, err, codes.InvalidArgument)
	}
}

func TestGetReviewThreadStoreFaultIsCodedNotLeaked(t *testing.T) {
	f := newReviewFixture(t)
	f.backend.fail = errors.New("pq: secret detail")
	_, err := f.h.GetReviewThread(context.Background(), &corev1.GetReviewThreadRequest{ThreadId: uuid.NewString()})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret detail")
}
