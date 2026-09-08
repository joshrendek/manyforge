// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Optional distinguishes an absent field, an explicit JSON null, and a value.
// Its zero value is absent. Generated fields use Go 1.25's omitzero semantics.
type Optional[T any] struct {
	value T
	set   bool
	null  bool
}

func Unset[T any]() Optional[T]        { return Optional[T]{} }
func Null[T any]() Optional[T]         { return Optional[T]{set: true, null: true} }
func Value[T any](value T) Optional[T] { return Optional[T]{value: value, set: true} }
func (o Optional[T]) IsZero() bool     { return !o.set }
func (o Optional[T]) IsSet() bool      { return o.set }
func (o Optional[T]) IsNull() bool     { return o.set && o.null }
func (o Optional[T]) Get() (T, bool)   { return o.value, o.set && !o.null }
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.set || o.null {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*o = Null[T]()
		return nil
	}
	var value T
	if err := decodeJSON(data, &value); err != nil {
		return err
	}
	*o = Value(value)
	return nil
}

// Date is a calendar date, not a timestamp. JSON preserves YYYY-MM-DD exactly.
type Date string

func (d Date) MarshalJSON() ([]byte, error) {
	if _, err := time.Parse("2006-01-02", string(d)); err != nil {
		return nil, fmt.Errorf("invalid calendar date: %w", err)
	}
	return json.Marshal(string(d))
}
func (d *Date) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if len(value) != 10 {
		return fmt.Errorf("invalid calendar date")
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("invalid calendar date: %w", err)
	}
	*d = Date(value)
	return nil
}

// UseNumber also preserves arbitrary JSON integer tokens in open attributes.
func decodeJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(value)
}

func objectFields(data []byte, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("missing required field %s", name)
		}
	}
	return fields, nil
}

func unionShape(data []byte, required []string, literals map[string][]string) bool {
	fields, err := objectFields(data, required...)
	if err != nil {
		return false
	}
	for name, allowed := range literals {
		raw, present := fields[name]
		if !present {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		found := false
		for _, candidate := range allowed {
			if value == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func nonNullFields(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return fmt.Errorf("field %s is not nullable", name)
		}
	}
	return nil
}
