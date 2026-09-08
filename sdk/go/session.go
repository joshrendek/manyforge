// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// OnRotate must durably save the new pair before returning. Failure invalidates
// this owner: the consumed refresh token is never reused. Owners are in-process,
// not a coordination protocol for independent processes or browser tabs.
type OnRotate func(context.Context, TokenPair) error
type Session struct {
	mu       sync.Mutex
	pair     TokenPair
	expires  time.Time
	lifetime time.Duration
	pending  chan struct{}
	invalid  error
	onRotate OnRotate
	origin   string
}

func SessionFromTokenPair(pair TokenPair, onRotate OnRotate) (*Session, error) {
	if err := validPair(pair); err != nil {
		return nil, err
	}
	lifetime := time.Duration(pair.ExpiresIn) * time.Second
	received := pair.receivedAt
	if received.IsZero() {
		// Caller-constructed pairs are received by this owner now.
		received = time.Now()
	}
	return &Session{pair: pair, lifetime: lifetime, expires: received.Add(lifetime), onRotate: onRotate}, nil
}
func validPair(pair TokenPair) error {
	if strings.TrimSpace(pair.AccessToken) == "" || strings.TrimSpace(pair.RefreshToken) == "" || pair.ExpiresIn <= 0 {
		return errors.New("session requires nonempty tokens and a positive lifetime")
	}
	return nil
}
func (s *Session) token(ctx context.Context, transport *httpTransport) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s.mu.Lock()
		if s.origin == "" {
			s.origin = transport.base
		}
		if s.origin != transport.base {
			s.mu.Unlock()
			return "", &SessionError{Cause: errors.New("session cannot be shared across instance origins")}
		}
		if s.invalid != nil {
			err := s.invalid
			s.mu.Unlock()
			return "", &SessionError{Cause: err}
		}
		if s.pending != nil {
			pending := s.pending
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		margin := min(30*time.Second, s.lifetime/10)
		if time.Until(s.expires) >= margin {
			token := s.pair.AccessToken
			s.mu.Unlock()
			return token, nil
		}
		pending := make(chan struct{})
		s.pending = pending
		refresh := s.pair.RefreshToken
		s.mu.Unlock()
		// The initiating request owns cancellation; cancelling a waiter does not
		// cancel rotation. Any failed attempt invalidates the owner conservatively.
		pair, err := NewRootResources(transport, "").Auth.Refresh(ctx, RefreshRequest{RefreshToken: refresh})
		received := pair.receivedAt
		if received.IsZero() {
			received = time.Now()
		}
		if err == nil {
			err = validPair(pair)
		}
		if err == nil && s.onRotate != nil {
			err = callOnRotate(ctx, s.onRotate, pair)
		}
		s.mu.Lock()
		if err != nil {
			s.invalid = err
			s.pair = TokenPair{}
		} else {
			s.pair = pair
			s.lifetime = time.Duration(pair.ExpiresIn) * time.Second
			s.expires = received.Add(s.lifetime)
		}
		s.pending = nil
		close(pending)
		s.mu.Unlock()
		if err != nil {
			return "", &SessionError{Cause: err}
		}
		return pair.AccessToken, nil
	}
}
func callOnRotate(ctx context.Context, callback OnRotate, pair TokenPair) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("session rotation persistence callback panicked")
		}
	}()
	return callback(ctx, pair)
}
