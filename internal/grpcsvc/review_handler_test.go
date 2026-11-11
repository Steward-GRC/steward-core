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
	if t.Suggestion != nil {
		sg := *t.Suggestion
		sg.Rounds = append([]domain.SuggestionRound(nil), sg.Rounds...)
		for i := range sg.Rounds {
			sg.Rounds[i].CreatedAt = t.CreatedAt
		}
		t.Suggestion = &sg
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

// AppendRound and CloseSuggestion mirror the store: the round must be the next
// one, and an ended suggestion takes nothing more.
func (f *fakeReviewBackend) suggestionOf(id uuid.UUID) (int, *domain.ReviewSuggestion, error) {
	i := f.find(id)
	if i < 0 {
		return 0, nil, store.ErrReviewThreadNotFound
	}
	sg := f.threads[i].Suggestion
	if sg == nil {
		return 0, nil, store.ErrSuggestionMissing
	}
	if !sg.Open() {
		return 0, nil, store.ErrSuggestionEnded
	}
	return i, sg, nil
}

func (f *fakeReviewBackend) AppendRound(_ context.Context, id uuid.UUID, r domain.SuggestionRound) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, sg, err := f.suggestionOf(id)
	if err != nil {
		return domain.ReviewThread{}, err
	}
	if len(sg.Rounds) != r.Seq-1 {
		return domain.ReviewThread{}, store.ErrSuggestionChanged
	}
	r.CreatedAt = time.Now()
	sg.Rounds = append(sg.Rounds, r)
	return f.threads[i], nil
}

func (f *fakeReviewBackend) CloseSuggestion(_ context.Context, id uuid.UUID, c domain.SuggestionClose) (domain.ReviewThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, sg, err := f.suggestionOf(id)
	if err != nil {
		return domain.ReviewThread{}, err
	}
	if len(sg.Rounds) != c.Rounds {
		return domain.ReviewThread{}, store.ErrSuggestionChanged
	}
	sg.Outcome, sg.Reason, sg.ClosedBy, sg.ClosedText, sg.ClosedAt = c.Outcome, c.Reason, c.ClosedBy, c.ClosedText, time.Now()
	f.threads[i].Resolved = true
	return f.threads[i], nil
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

// ---- suggestions and counters (core#54) ----

func (f *reviewFixture) propose(t *testing.T, text string) *corev1.ReviewThread {
	t.Helper()
	resp, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: corev1.ReviewSide_REVIEW_SIDE_REVIEWER, PolicyVersionId: f.version,
		Quote: "seven years", Occurrence: 1, Body: "Shorter?", ApproverUserId: f.charlie, SuggestedText: text,
	})
	require.NoError(t, err)
	return resp.GetThread()
}

func (f *reviewFixture) counter(actor string, side corev1.ReviewSide, id, text string) (*corev1.ReviewThread, error) {
	resp, err := f.h.CounterReviewSuggestion(context.Background(), &corev1.CounterReviewSuggestionRequest{
		ActorUserId: actor, ActorSide: side, ThreadId: id, Text: text,
	})
	return resp.GetThread(), err
}

func (f *reviewFixture) closeAs(actor string, side corev1.ReviewSide, id string, o corev1.SuggestionOutcome, reason string) (*corev1.ReviewThread, error) {
	resp, err := f.h.CloseReviewSuggestion(context.Background(), &corev1.CloseReviewSuggestionRequest{
		ActorUserId: actor, ActorSide: side, ThreadId: id, Outcome: o, Reason: reason,
	})
	return resp.GetThread(), err
}

const (
	authorSide   = corev1.ReviewSide_REVIEW_SIDE_AUTHOR
	reviewerSide = corev1.ReviewSide_REVIEW_SIDE_REVIEWER
)

// mock: starting a suggestion / opens it on the author's turn when a reviewer proposes
func TestAddReviewThreadWithASuggestionOpensOnTheAuthorsTurn(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	sg := th.GetSuggestion()
	require.NotNil(t, sg)
	require.Equal(t, "six years", sg.GetText())
	require.Equal(t, authorSide, sg.GetTurn())
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_UNSPECIFIED, sg.GetOutcome())
	require.Len(t, sg.GetRounds(), 1)
	require.Equal(t, reviewerSide, sg.GetRounds()[0].GetSide())
	require.Equal(t, f.charlie, sg.GetRounds()[0].GetUserId())
	require.Equal(t, int32(1), sg.GetRounds()[0].GetSeq())
	require.NotNil(t, sg.GetRounds()[0].GetCreatedAt())
}

// mock: starting a suggestion / opens it on the reviewer's turn when the author proposes
func TestAddReviewThreadWithAnAuthorSuggestionOpensOnTheReviewersTurn(t *testing.T) {
	f := newReviewFixture(t)
	resp, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: f.bob, ActorSide: authorSide, PolicyVersionId: f.version,
		Quote: "seven years", Occurrence: 1, Body: "How about this?", ApproverUserId: f.charlie, SuggestedText: "five years",
	})
	require.NoError(t, err)
	require.Equal(t, reviewerSide, resp.GetThread().GetSuggestion().GetTurn())
}

// mock: starting a suggestion / is a plain comment when no text, or the same text, is proposed
func TestAddReviewThreadWithNoOrSameTextIsAPlainComment(t *testing.T) {
	f := newReviewFixture(t)
	require.Nil(t, f.propose(t, "").GetSuggestion())
	require.Nil(t, f.propose(t, "seven years").GetSuggestion())
	require.Nil(t, f.propose(t, "   ").GetSuggestion())
}

func TestAddReviewThreadRefusesSuggestedTextOverTheLimit(t *testing.T) {
	f := newReviewFixture(t)
	_, err := f.h.AddReviewThread(context.Background(), &corev1.AddReviewThreadRequest{
		ActorUserId: f.charlie, ActorSide: reviewerSide, PolicyVersionId: f.version,
		Quote: "x", Occurrence: 1, Body: "Why?", ApproverUserId: f.charlie, SuggestedText: strings.Repeat("a", 31),
	})
	requireCode(t, err, codes.InvalidArgument)
	require.Empty(t, f.backend.threads, "nothing is stored for a refused proposal")
}

// mock: starting a suggestion / comes back on the version's thread list
func TestListReviewThreadsCarriesTheSuggestion(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	resp, err := f.h.ListReviewThreads(context.Background(), &corev1.ListReviewThreadsRequest{PolicyVersionId: f.version})
	require.NoError(t, err)
	require.Equal(t, th.GetId(), resp.GetThreads()[0].GetId())
	require.Equal(t, "six years", resp.GetThreads()[0].GetSuggestion().GetText())
}

// mock: countering / lets the side whose turn it is counter, and flips the turn
func TestCounterFlipsTheTurnAndKeepsTheRounds(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	got, err := f.counter(f.bob, authorSide, th.GetId(), "five years")
	require.NoError(t, err)
	sg := got.GetSuggestion()
	require.Equal(t, "five years", sg.GetText())
	require.Equal(t, reviewerSide, sg.GetTurn())
	require.Len(t, sg.GetRounds(), 2)
	require.Equal(t, "six years", sg.GetRounds()[0].GetText())
	require.Equal(t, authorSide, sg.GetRounds()[1].GetSide())
	require.Equal(t, f.bob, sg.GetRounds()[1].GetUserId())

	got, err = f.counter(f.charlie, reviewerSide, th.GetId(), "four years")
	require.NoError(t, err)
	require.Equal(t, authorSide, got.GetSuggestion().GetTurn())
	require.Len(t, got.GetSuggestion().GetRounds(), 3)
}

// mock: countering / refuses a counter out of turn, an empty one, and one on a plain comment
func TestCounterRefusedOutOfTurnEmptyAndOnAPlainComment(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.counter(f.charlie, reviewerSide, th.GetId(), "four years")
	requireCode(t, err, codes.FailedPrecondition)
	_, err = f.counter(f.bob, authorSide, th.GetId(), "  ")
	requireCode(t, err, codes.InvalidArgument)
	_, err = f.counter(f.bob, authorSide, th.GetId(), strings.Repeat("a", 31))
	requireCode(t, err, codes.InvalidArgument)
	plain := f.propose(t, "")
	_, err = f.counter(f.bob, authorSide, plain.GetId(), "x")
	requireCode(t, err, codes.FailedPrecondition)
	_, err = f.counter(f.bob, authorSide, uuid.NewString(), "x")
	requireCode(t, err, codes.NotFound)
	_, err = f.counter("nope", authorSide, th.GetId(), "x")
	requireCode(t, err, codes.InvalidArgument)
	_, err = f.counter(f.bob, corev1.ReviewSide_REVIEW_SIDE_UNSPECIFIED, th.GetId(), "x")
	requireCode(t, err, codes.InvalidArgument)
	require.Len(t, f.backend.threads[0].Suggestion.Rounds, 1, "no refused counter is stored")
}

// mock: countering / keeps someone outside the review out (core: a reviewer must be the thread's approver)
func TestCounterAndCloseByAnotherReviewerAreDenied(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.counter(f.bob, authorSide, th.GetId(), "five years")
	require.NoError(t, err)
	_, err = f.counter(f.alice, reviewerSide, th.GetId(), "four years")
	requireCode(t, err, codes.PermissionDenied)
	_, err = f.closeAs(f.alice, reviewerSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_AGREED, "")
	requireCode(t, err, codes.PermissionDenied)
}

// mock: closing / lets the author change it, which resolves the thread and records the text
func TestAuthorChangesItResolvesAndRecordsTheTextAndCloser(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	got, err := f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_CHANGED, "")
	require.NoError(t, err)
	require.True(t, got.GetResolved())
	sg := got.GetSuggestion()
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_CHANGED, sg.GetOutcome())
	require.Equal(t, "six years", sg.GetClosedText())
	require.Equal(t, f.bob, sg.GetClosedBy())
	require.NotNil(t, sg.GetClosedAt())
}

// mock: closing / lets the author remove the note, and the reviewer agree on their turn
func TestAuthorRemovesAndReviewerAgrees(t *testing.T) {
	f := newReviewFixture(t)
	first := f.propose(t, "six years")
	got, err := f.closeAs(f.bob, authorSide, first.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, "")
	require.NoError(t, err)
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, got.GetSuggestion().GetOutcome())

	second := f.propose(t, "six years")
	_, err = f.counter(f.bob, authorSide, second.GetId(), "five years")
	require.NoError(t, err)
	got, err = f.closeAs(f.charlie, reviewerSide, second.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_AGREED, "")
	require.NoError(t, err)
	require.Equal(t, "five years", got.GetSuggestion().GetClosedText())
	require.Equal(t, f.charlie, got.GetSuggestion().GetClosedBy())
}

// mock: closing / does not let the wrong side use an outcome that is not theirs
func TestWrongSideCannotUseAnOutcomeThatIsNotTheirs(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.closeAs(f.charlie, reviewerSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_AGREED, "")
	requireCode(t, err, codes.FailedPrecondition)
	_, err = f.counter(f.bob, authorSide, th.GetId(), "five years")
	require.NoError(t, err)
	_, err = f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_CHANGED, "")
	requireCode(t, err, codes.FailedPrecondition)
	_, err = f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_UNSPECIFIED, "")
	requireCode(t, err, codes.InvalidArgument)
	require.True(t, f.backend.threads[0].Suggestion.Open())
}

// mock: closing / needs a reason for not agreed, and records it
func TestNotAgreedNeedsAReasonAndRecordsIt(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_NOT_AGREED, "")
	requireCode(t, err, codes.InvalidArgument)
	got, err := f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_NOT_AGREED, "Law says seven.")
	require.NoError(t, err)
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_NOT_AGREED, got.GetSuggestion().GetOutcome())
	require.Equal(t, "Law says seven.", got.GetSuggestion().GetReason())
}

// mock: closing / lets the side that proposed first withdraw while it is not their turn
func TestFirstProposerWithdrawsWhileNotTheirTurn(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	got, err := f.closeAs(f.charlie, reviewerSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_WITHDRAWN, "")
	require.NoError(t, err)
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_WITHDRAWN, got.GetSuggestion().GetOutcome())
	other := f.propose(t, "six years")
	_, err = f.closeAs(f.bob, authorSide, other.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_WITHDRAWN, "")
	requireCode(t, err, codes.FailedPrecondition)
}

// mock: closing / refuses to close twice, or to counter a closed suggestion
func TestNoSecondCloseAndNoCounterAfterClose(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, "")
	require.NoError(t, err)
	_, err = f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, "")
	requireCode(t, err, codes.FailedPrecondition)
	_, err = f.counter(f.bob, authorSide, th.GetId(), "x")
	requireCode(t, err, codes.FailedPrecondition)
}

// Go-only (mock silent): the mock does not say what a reopened closed suggestion shows.
func TestReopeningAClosedSuggestionsThreadKeepsItsOutcome(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	_, err := f.closeAs(f.bob, authorSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, "")
	require.NoError(t, err)
	re, err := f.h.SetReviewThreadResolved(context.Background(), &corev1.SetReviewThreadResolvedRequest{
		ActorUserId: f.bob, ActorSide: authorSide, ThreadId: th.GetId(), Resolved: false,
	})
	require.NoError(t, err)
	require.False(t, re.GetThread().GetResolved())
	require.Equal(t, corev1.SuggestionOutcome_SUGGESTION_OUTCOME_REMOVED, re.GetThread().GetSuggestion().GetOutcome())
	_, err = f.counter(f.bob, authorSide, th.GetId(), "x")
	requireCode(t, err, codes.FailedPrecondition)
}

// Audit is ids and outcomes only: never the proposed text, a counter, or a reason.
func TestSuggestionAuditCarriesIDsAndOutcomeButNeverText(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years secret")
	_, err := f.counter(f.bob, authorSide, th.GetId(), "five years secret")
	require.NoError(t, err)
	_, err = f.closeAs(f.charlie, reviewerSide, th.GetId(), corev1.SuggestionOutcome_SUGGESTION_OUTCOME_NOT_AGREED, "reason secret")
	require.NoError(t, err)

	var actions []string
	for _, ev := range f.audit.evs {
		actions = append(actions, ev.Action)
		for k, v := range ev.Attributes {
			require.NotContains(t, v, "secret", "attribute %s of %s", k, ev.Action)
		}
		require.NotContains(t, ev.Subject, "secret")
	}
	require.Equal(t, []string{"review.thread.created", "review.suggestion.countered", "review.suggestion.closed"}, actions)
	closed := f.audit.evs[2]
	require.Equal(t, audit.TierActivity, closed.Tier)
	require.Equal(t, "not_agreed", closed.Attributes["suggestion_outcome"])
	require.Equal(t, th.GetId(), closed.Attributes["thread_id"])
	require.Equal(t, f.charlie, closed.ActorUserID)
	counter := f.audit.evs[1]
	require.Equal(t, "2", counter.Attributes["round"])
}

func TestSuggestionStoreFaultIsUnavailableAndLeaksNothing(t *testing.T) {
	f := newReviewFixture(t)
	th := f.propose(t, "six years")
	f.backend.fail = errors.New("boom with six years")
	_, err := f.counter(f.bob, authorSide, th.GetId(), "x")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "six years")
}
