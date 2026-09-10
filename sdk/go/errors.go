// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"fmt"
	"net/http"
)

// APIError retains server diagnostics for explicit inspection. Error deliberately
// excludes server-controlled strings, URLs, bodies and credential-bearing paths.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Headers   http.Header
	Details   any
	RawBody   []byte
	Truncated bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ManyForge HTTP response error (status %d)", e.Status)
}

// TransportError preserves errors.Is/context cancellation without rendering URLs.
type TransportError struct{ Cause error }

func (*TransportError) Error() string   { return "ManyForge transport failed" }
func (e *TransportError) Unwrap() error { return e.Cause }

type InvalidResponseError struct{ Cause error }

func (*InvalidResponseError) Error() string {
	return "ManyForge successful response has invalid payload"
}
func (e *InvalidResponseError) Unwrap() error { return e.Cause }

type SessionError struct{ Cause error }

func (*SessionError) Error() string   { return "ManyForge session requires new authentication" }
func (e *SessionError) Unwrap() error { return e.Cause }
