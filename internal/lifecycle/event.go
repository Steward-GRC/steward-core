// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle publishes the events other services act on when a
// document changes, onto the "jobs" topic exchange. It is kept apart from the
// audit exchange so an audit consumer can never swallow a worker's events. The
// JSON shapes are the consumers' contract: the AI indexer and obligations
// decode them into their own structs.
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// EventType identifies what happened to a document version.
type EventType string

const (
	// EventTypePublished is emitted when a draft is published.
	EventTypePublished EventType = "policy.published"
	// EventTypeUnpublished tells the AI indexer to drop a version's chunks.
	EventTypeUnpublished EventType = "policy.unpublished"
	// EventTypeObligationChanged is emitted when a policy's acknowledgement
	// audience may have shrunk, so obligations can purge acknowledgements from
	// users who left it.
	EventTypeObligationChanged EventType = "policy.obligation_changed"
	// EventTypeRetired is emitted when a policy is retired, so obligations can
	// tell its audience.
	EventTypeRetired EventType = "policy.retired"

	// EventTypeProcedurePublished is the procedure counterpart of
	// EventTypePublished. It has its own routing key because procedures carry
	// no acknowledgement, so it must never reach the obligations pipeline.
	EventTypeProcedurePublished EventType = "procedure.published"
	// EventTypeProcedureUnpublished is the procedure counterpart of
	// EventTypeUnpublished.
	EventTypeProcedureUnpublished EventType = "procedure.unpublished"
	// EventTypeProcedureRetired is the procedure counterpart of
	// EventTypeRetired, kept out of the policy retire fan-out.
	EventTypeProcedureRetired EventType = "procedure.retired"
)

// RoutingKeyPublished carries EventTypePublished on the "jobs" exchange.
const RoutingKeyPublished = "policy.published"

// RoutingKeyObligationChanged carries EventTypeObligationChanged.
const RoutingKeyObligationChanged = "policy.obligation_changed"

// RoutingKeyRetired carries EventTypeRetired.
const RoutingKeyRetired = "policy.retired"

// RoutingKeyProcedurePublished carries EventTypeProcedurePublished.
const RoutingKeyProcedurePublished = "procedure.published"

// RoutingKeyProcedureRetired carries EventTypeProcedureRetired.
const RoutingKeyProcedureRetired = "procedure.retired"

// AIIndexQueue is the AI indexer's policy queue. A reindex publishes to the
// default exchange with this routing key, so it reaches that one queue and
// not the "jobs" fan-out, which would make obligations ask for a review
// again. Same payload as a publish; only the exchange differs.
const AIIndexQueue = "ai.policy.publish"

// AIProcedureIndexQueue is the AI indexer's procedure queue, used the same
// way as AIIndexQueue.
const AIProcedureIndexQueue = "ai.procedure.publish"

// The document kinds carried in PolicyVersionContent.DocumentType. They are
// upper case because that is the consumers' wire contract.
const (
	// DocumentTypePolicy marks a PolicyVersionContent as a policy.
	DocumentTypePolicy = "POLICY"
	// DocumentTypeProcedure marks a PolicyVersionContent as a procedure.
	DocumentTypeProcedure = "PROCEDURE"
)

// SectionContent is one section's plain text. The JSON keys are the AI
// indexer's field names.
type SectionContent struct {
	Key  string `json:"Key"`
	Text string `json:"Text"`
}

// PolicyVersionContent is everything a consumer needs about a published
// version: its identity, the access metadata and the section text. The JSON
// keys are the AI indexer's field names; extra keys are ignored by readers
// that don't use them. EffectiveDate is absent when the policy has none, and
// readers then fall back to the publish date. An empty DocumentType reads as
// a policy.
type PolicyVersionContent struct {
	PolicyID      string           `json:"PolicyID"`
	VersionID     string           `json:"VersionID"`
	VersionNo     int              `json:"VersionNo"`
	CategoryID    string           `json:"CategoryID"`
	Sensitivity   string           `json:"Sensitivity"` // "standard" or "sensitive"
	PolicyTitle   string           `json:"PolicyTitle"`
	EffectiveDate *time.Time       `json:"EffectiveDate,omitempty"`
	DocumentType  string           `json:"DocumentType,omitempty"`
	Sections      []SectionContent `json:"Sections"`
}

// ObligationChangedEvent names one policy whose acknowledgement audience may
// have shrunk.
type ObligationChangedEvent struct {
	EventType EventType `json:"event_type"`
	PolicyID  string    `json:"policy_id"`
}

// PolicyRetiredEvent is published when a document is retired.
type PolicyRetiredEvent struct {
	EventType EventType `json:"event_type"`
	RetiredAt time.Time `json:"retired_at"`
	PolicyID  string    `json:"policy_id"`
	Number    string    `json:"number"`
	Title     string    `json:"title"`
}

// PublishEvent is published when a version is published or unpublished;
// EventType tells one consumer queue which. PublishedAt is the stored
// transition time, so a redelivery or a reindex keeps the same value.
type PublishEvent struct {
	EventType   EventType            `json:"event_type"`
	PublishedAt time.Time            `json:"published_at"`
	Version     PolicyVersionContent `json:"version"`
}

// Publisher sends one message.
type Publisher interface {
	Publish(ctx context.Context, routingKey string, body []byte) error
}

// Emitter publishes lifecycle events on the "jobs" exchange. The optional
// aiPub publishes to the default exchange, for the reindex path; without it
// the reindex methods do nothing.
type Emitter struct {
	pub   Publisher
	aiPub Publisher
}

// New returns an Emitter that publishes JSON events through p.
func New(p Publisher) *Emitter { return &Emitter{pub: p} }

// WithAIPublisher sets the default-exchange publisher the reindex path uses.
func (e *Emitter) WithAIPublisher(p Publisher) *Emitter {
	if e == nil {
		return e
	}
	e.aiPub = p
	return e
}

// EmitPublished publishes a published version. publishedAt is the time the
// store stamped, not now, so a re-emit stays stable.
func (e *Emitter) EmitPublished(ctx context.Context, publishedAt time.Time, v PolicyVersionContent) error {
	if e == nil || e.pub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType:   EventTypePublished,
		PublishedAt: publishedAt.UTC(),
		Version:     v,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal publish event: %w", err)
	}
	return e.pub.Publish(ctx, RoutingKeyPublished, body)
}

// EmitProcedurePublished is EmitPublished for a procedure.
func (e *Emitter) EmitProcedurePublished(ctx context.Context, publishedAt time.Time, v PolicyVersionContent) error {
	if e == nil || e.pub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType:   EventTypeProcedurePublished,
		PublishedAt: publishedAt.UTC(),
		Version:     v,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal procedure publish event: %w", err)
	}
	return e.pub.Publish(ctx, RoutingKeyProcedurePublished, body)
}

// EmitProcedureReindex is EmitReindex for a procedure.
func (e *Emitter) EmitProcedureReindex(ctx context.Context, publishedAt time.Time, v PolicyVersionContent) error {
	if e == nil || e.aiPub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType:   EventTypeProcedurePublished,
		PublishedAt: publishedAt.UTC(),
		Version:     v,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal procedure reindex event: %w", err)
	}
	return e.aiPub.Publish(ctx, AIProcedureIndexQueue, body)
}

// EmitProcedureReindexRemove is EmitReindexRemove for a procedure.
func (e *Emitter) EmitProcedureReindexRemove(ctx context.Context, policyID, versionID string) error {
	if e == nil || e.aiPub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType: EventTypeProcedureUnpublished,
		Version:   PolicyVersionContent{PolicyID: policyID, VersionID: versionID},
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal procedure reindex-remove event: %w", err)
	}
	return e.aiPub.Publish(ctx, AIProcedureIndexQueue, body)
}

// EmitReindex re-sends a published version to the AI indexer only (see
// AIIndexQueue).
func (e *Emitter) EmitReindex(ctx context.Context, publishedAt time.Time, v PolicyVersionContent) error {
	if e == nil || e.aiPub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType:   EventTypePublished,
		PublishedAt: publishedAt.UTC(),
		Version:     v,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal reindex event: %w", err)
	}
	return e.aiPub.Publish(ctx, AIIndexQueue, body)
}

// EmitReindexRemove tells the AI indexer to drop one version's chunks. The
// indexer deletes by version id, so a version with no chunks is a no-op.
func (e *Emitter) EmitReindexRemove(ctx context.Context, policyID, versionID string) error {
	if e == nil || e.aiPub == nil {
		return nil
	}
	evt := PublishEvent{
		EventType: EventTypeUnpublished,
		Version:   PolicyVersionContent{PolicyID: policyID, VersionID: versionID},
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal reindex-remove event: %w", err)
	}
	return e.aiPub.Publish(ctx, AIIndexQueue, body)
}

// EmitObligationChanged publishes one event per affected policy.
func (e *Emitter) EmitObligationChanged(ctx context.Context, policyID string) error {
	if e == nil || e.pub == nil {
		return nil
	}
	evt := ObligationChangedEvent{
		EventType: EventTypeObligationChanged,
		PolicyID:  policyID,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal obligation-changed event: %w", err)
	}
	return e.pub.Publish(ctx, RoutingKeyObligationChanged, body)
}

// EmitRetired publishes a retired policy. retiredAt is the time the store
// stamped.
func (e *Emitter) EmitRetired(ctx context.Context, retiredAt time.Time, policyID, number, title string) error {
	if e == nil || e.pub == nil {
		return nil
	}
	evt := PolicyRetiredEvent{
		EventType: EventTypeRetired,
		RetiredAt: retiredAt.UTC(),
		PolicyID:  policyID,
		Number:    number,
		Title:     title,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal retired event: %w", err)
	}
	return e.pub.Publish(ctx, RoutingKeyRetired, body)
}

// EmitProcedureRetired is EmitRetired for a procedure.
func (e *Emitter) EmitProcedureRetired(ctx context.Context, retiredAt time.Time, policyID, number, title string) error {
	if e == nil || e.pub == nil {
		return nil
	}
	evt := PolicyRetiredEvent{
		EventType: EventTypeProcedureRetired,
		RetiredAt: retiredAt.UTC(),
		PolicyID:  policyID,
		Number:    number,
		Title:     title,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal procedure retired event: %w", err)
	}
	return e.pub.Publish(ctx, RoutingKeyProcedureRetired, body)
}
