// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package cache is the optional read cache for published versions and the
// global settings, on go-redis. A cache that can't be reached reads as a
// miss, so an outage only costs speed.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	redis "github.com/Bugs5382/go-redis"
	rediscache "github.com/Bugs5382/go-redis/cache"
)

// Redis caches byte values for one TTL.
type Redis struct {
	c   *rediscache.Cache[[]byte]
	ttl time.Duration
	br  breaker
}

// New returns a cache on client with entries kept for ttl.
func New(client *redis.Client, ttl time.Duration) *Redis {
	return &Redis{c: rediscache.New[[]byte](client, rediscache.WithCodec(rawCodec{})), ttl: ttl}
}

// TTL is how long entries are kept.
func (r *Redis) TTL() time.Duration { return r.ttl }

// GetBytes returns the cached value and whether there was one.
func (r *Redis) GetBytes(ctx context.Context, key string) ([]byte, bool) {
	if !r.br.allow() {
		return nil, false
	}
	b, err := r.c.Get(ctx, key)
	if errors.Is(err, rediscache.ErrMiss) {
		r.br.record(nil)
		return nil, false
	}
	r.br.record(err)
	return b, err == nil
}

// SetBytes stores val. A failure is only counted against the breaker.
func (r *Redis) SetBytes(ctx context.Context, key string, val []byte) {
	if r.br.allow() {
		r.br.record(r.c.Set(ctx, key, val, r.ttl))
	}
}

// Del removes key.
func (r *Redis) Del(ctx context.Context, key string) {
	if r.br.allow() {
		r.br.record(r.c.Delete(ctx, key))
	}
}

// rawCodec stores the bytes as they are; the callers already hold encoded
// protobuf or JSON.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }

func (rawCodec) Unmarshal(data []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), data...)
	return nil
}

// breaker skips Redis for a few seconds after three failures in a row, so an
// outage doesn't add a timeout to every request.
type breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}

func (b *breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.fails = 0
		b.openUntil = time.Time{}
		return
	}
	b.fails++
	if b.fails >= 3 {
		b.openUntil = time.Now().Add(3 * time.Second)
	}
}
