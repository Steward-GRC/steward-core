// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// Tests for the version a new draft copies its appendices from (issue #53).
// The mock carries a rejected version's links and template over to the next
// draft ("rejected ?? published"); core does the same for appendices.

func (f *statusFixture) assertSource(want uuid.UUID, label string) {
	f.t.Helper()
	got, err := f.ps.AppendixSource(f.ctx, f.p.ID)
	if err != nil {
		f.t.Fatalf("%s: AppendixSource: %v", label, err)
	}
	if got != want {
		f.t.Fatalf("%s: AppendixSource = %s, want %s", label, got, want)
	}
}

func (f *statusFixture) reject(content string) domain.PolicyVersion {
	f.t.Helper()
	d, _ := f.save(content)
	f.setStatus(d.ID, domain.PolicyVersionStatusInReview)
	return f.setStatus(d.ID, domain.PolicyVersionStatusRejected)
}

func TestAppendixSourceNoneWhenThePolicyHasNoVersion(t *testing.T) {
	f := newStatusFixture(t)
	f.assertSource(uuid.Nil, "no versions")
	f.save(reviewedText)
	f.assertSource(uuid.Nil, "only a draft")
}

func TestAppendixSourceIsThePublishedVersion(t *testing.T) {
	f := newStatusFixture(t)
	d, _ := f.save(reviewedText)
	pub := f.setStatus(d.ID, domain.PolicyVersionStatusPublished)
	f.assertSource(pub.ID, "published only")
}

func TestAppendixSourceIsTheRejectedVersionWhenNothingIsPublished(t *testing.T) {
	f := newStatusFixture(t)
	rej := f.reject(reviewedText)
	f.assertSource(rej.ID, "only rejected")
}

func TestAppendixSourceIsTheRejectedVersionWhenNewerThanPublished(t *testing.T) {
	f := newStatusFixture(t)
	d, _ := f.save(reviewedText)
	f.setStatus(d.ID, domain.PolicyVersionStatusPublished)
	rej := f.reject(reviewedText)
	f.assertSource(rej.ID, "published, then rejected")
}

func TestAppendixSourceIsThePublishedVersionWhenNewerThanARejection(t *testing.T) {
	f := newStatusFixture(t)
	f.reject(reviewedText)
	d, _ := f.save(reviewedText)
	pub := f.setStatus(d.ID, domain.PolicyVersionStatusPublished)
	f.assertSource(pub.ID, "rejected, then published")
}

func TestAppendixSourceIsTheLatestRejectedVersion(t *testing.T) {
	f := newStatusFixture(t)
	f.reject(reviewedText)
	second := f.reject(reviewedText)
	f.assertSource(second.ID, "two rejections")
}

// A new draft after a rejection starts with the rejected version's appendices,
// not the published version's.
func TestAppendicesCarryOverFromTheRejectedVersion(t *testing.T) {
	f := newStatusFixture(t)
	as := store.NewAppendixStore(f.db)

	// v1 is published with one appendix.
	d, _ := f.save(reviewedText)
	if _, err := as.Add(f.ctx, mustAppendix(t, d.ID, "Published Form", `{"a":1}`)); err != nil {
		t.Fatalf("Add published appendix: %v", err)
	}
	f.setStatus(d.ID, domain.PolicyVersionStatusPublished)

	// The next draft has two appendices of its own and is rejected.
	d2, _ := f.save(reviewedText)
	for _, title := range []string{"Rejected First", "Rejected Second"} {
		if _, err := as.Add(f.ctx, mustAppendix(t, d2.ID, title, `{"b":1}`)); err != nil {
			t.Fatalf("Add rejected appendix: %v", err)
		}
	}
	f.setStatus(d2.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d2.ID, domain.PolicyVersionStatusRejected)

	// The next save is a new draft, which copies from the source.
	d3, res := f.save(reviewedText)
	if !res.Inserted {
		t.Fatal("expected a new draft row")
	}
	src, err := f.ps.AppendixSource(f.ctx, f.p.ID)
	if err != nil {
		t.Fatalf("AppendixSource: %v", err)
	}
	if err := as.CopyForward(f.ctx, src, d3.ID); err != nil {
		t.Fatalf("CopyForward: %v", err)
	}
	got, err := as.ListByVersion(f.ctx, d3.ID)
	if err != nil {
		t.Fatalf("ListByVersion: %v", err)
	}
	if len(got) != 2 || got[0].Title != "Rejected First" || got[1].Title != "Rejected Second" {
		t.Fatalf("new draft appendices = %+v, want the rejected version's two in order", got)
	}
}
