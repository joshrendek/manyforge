// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// Transport is implemented by the native HTTP/session layer or a caller-owned
// transport. It must not redirect or replay ambiguously processed requests.
// Do decodes successful JSON into result (nil means an empty response), preserving
// json.Number for arbitrary attributes. OpenStream transfers close ownership to
// the caller. Paths are already escaped; transports must not encode them again.
type Transport interface {
	Do(context.Context, Request, any, ...RequestOption) error
	OpenStream(context.Context, Request, ...RequestOption) (io.ReadCloser, error)
}

type Request struct {
	OperationID   string
	Method        string
	Path          string
	Query         url.Values
	Headers       map[string]string
	Body          any
	Form          map[string]any
	Authenticated bool
	Audience      string
	Signing       string
	// BodyKey names a transport-owned publishable-key field; empty means none.
	BodyKey string
}

// Upload allows byte buffers (bytes.NewReader) and streaming multipart input.
// The caller retains ownership of Reader; the transport constructs boundaries.
type Upload struct {
	Filename    string
	Reader      io.Reader
	ContentType string
}
type RequestOptions struct{ Timeout time.Duration }
type RequestOption func(*RequestOptions)

func WithTimeout(timeout time.Duration) RequestOption {
	return func(o *RequestOptions) { o.Timeout = timeout }
}

// Parameter carries the OpenAPI serialization style into the transport-neutral
// URL builder, rather than guessing collection encodings from Go slice types.
func addQuery(query url.Values, name string, value any, style string, explode bool) {
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = fmt.Sprint(v.Index(i).Interface())
		}
		if explode {
			for _, part := range parts {
				query.Add(name, part)
			}
			return
		}
		separator := ","
		if style == "spaceDelimited" {
			separator = " "
		}
		if style == "pipeDelimited" {
			separator = "|"
		}
		query.Add(name, strings.Join(parts, separator))
		return
	}
	query.Add(name, fmt.Sprint(value))
}

// Iterator lazily requests cursor pages. It owns no connection or background task.
type Iterator[T any] struct {
	fetch   func(string) ([]T, string, error)
	cursor  string
	seen    map[string]bool
	items   []T
	index   int
	current T
	err     error
	done    bool
}

func newIterator[T any](cursor string, fetch func(string) ([]T, string, error)) *Iterator[T] {
	seen := make(map[string]bool)
	if cursor != "" {
		seen[cursor] = true
	}
	return &Iterator[T]{fetch: fetch, cursor: cursor, seen: seen}
}
func (i *Iterator[T]) Next() bool {
	for i.index == len(i.items) {
		if i.done || i.err != nil {
			return false
		}
		items, next, err := i.fetch(i.cursor)
		if err != nil {
			i.err = err
			return false
		}
		if next != "" && i.seen[next] {
			i.err = &PaginationError{}
			return false
		}
		if next != "" {
			i.seen[next] = true
		}
		i.items, i.index, i.cursor, i.done = items, 0, next, next == ""
	}
	i.current = i.items[i.index]
	i.index++
	return true
}
func (i *Iterator[T]) Current() T { return i.current }
func (i *Iterator[T]) Err() error { return i.err }

type PaginationError struct{}

func (*PaginationError) Error() string { return "server repeated a pagination cursor" }
