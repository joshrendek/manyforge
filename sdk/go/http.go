// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type httpTransport struct {
	base   string
	client *http.Client
	owned  *http.Transport
	config clientConfig
	public bool
}

func (t *httpTransport) close() {
	if t.owned != nil {
		t.owned.CloseIdleConnections()
	}
}
func (t *httpTransport) Do(ctx context.Context, descriptor Request, result any, options ...RequestOption) error {
	response, cancel, err := t.send(ctx, descriptor, options...)
	if err != nil {
		return err
	}
	received := time.Now()
	defer cancel()
	defer response.Body.Close()
	if result == nil {
		_, err = io.Copy(io.Discard, response.Body)
		if err != nil {
			return &TransportError{Cause: err}
		}
		return nil
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return &TransportError{Cause: err}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &InvalidResponseError{Cause: io.ErrUnexpectedEOF}
	}
	if !json.Valid(data) {
		return &InvalidResponseError{Cause: errors.New("invalid JSON")}
	}
	if err := decodeJSON(data, result); err != nil {
		return &InvalidResponseError{Cause: err}
	}
	if pair, ok := result.(*TokenPair); ok {
		pair.receivedAt = received
	}
	return nil
}

type responseStream struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (s *responseStream) Close() error { err := s.ReadCloser.Close(); s.cancel(); return err }
func (t *httpTransport) OpenStream(ctx context.Context, descriptor Request, options ...RequestOption) (io.ReadCloser, error) {
	response, cancel, err := t.send(ctx, descriptor, options...)
	if err != nil {
		return nil, err
	}
	return &responseStream{response.Body, cancel}, nil
}
func (t *httpTransport) send(ctx context.Context, descriptor Request, options ...RequestOption) (*http.Response, context.CancelFunc, error) {
	settings := RequestOptions{Timeout: t.config.timeout}
	for _, option := range options {
		if option == nil {
			return nil, nil, errors.New("nil request option")
		}
		option(&settings)
	}
	if settings.Timeout <= 0 {
		return nil, nil, errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	fail := func(err error) (*http.Response, context.CancelFunc, error) { cancel(); return nil, nil, err }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if !strings.HasPrefix(descriptor.Path, "/") || strings.HasPrefix(descriptor.Path, "//") || strings.ContainsAny(descriptor.Path, "?#") {
		return fail(errors.New("request path must be a generated relative escaped path"))
	}
	target := t.base + descriptor.Path
	if query := descriptor.Query.Encode(); query != "" {
		target += "?" + query
	}
	var token string
	if descriptor.Authenticated {
		if t.public {
			return fail(errors.New("public transport rejects management requests"))
		}
		var err error
		switch {
		case t.config.session != nil:
			token, err = t.config.session.token(ctx, t)
		case t.config.provider != nil:
			token, err = t.config.provider(ctx)
		default:
			token = t.config.accessToken
		}
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(token) == "" {
			return fail(errors.New("authenticated request requires a nonempty access token"))
		}
	}
	var body io.Reader
	var encoded []byte
	contentType := ""
	if descriptor.Form != nil {
		if t.config.secret != "" {
			return fail(errors.New("signed multipart is not a supported operation"))
		}
		var err error
		body, contentType, err = multipartBody(descriptor.Form)
		if err != nil {
			return fail(err)
		}
	} else if descriptor.Body != nil {
		var err error
		encoded, err = json.Marshal(descriptor.Body)
		if err != nil {
			return fail(err)
		}
		if descriptor.BodyKey != "" {
			if !t.public || t.config.key == "" {
				return fail(errors.New("body publishable key requires a public client"))
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(encoded, &fields); err != nil || fields == nil {
				return fail(errors.New("publishable key requires an object body"))
			}
			fields[descriptor.BodyKey], _ = json.Marshal(t.config.key)
			encoded, err = json.Marshal(fields)
			if err != nil {
				return fail(err)
			}
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, descriptor.Method, target, body)
	if err != nil {
		return fail(&TransportError{Cause: err})
	}
	// No GetBody replay hook, even for a buffer-backed mutation.
	request.GetBody = nil
	for name, value := range descriptor.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Origin") || strings.Contains(strings.ToLower(name), "signature") || strings.EqualFold(name, "X-Mailing-Timestamp") {
			return fail(errors.New("transport-owned credential header cannot be overridden"))
		}
		request.Header.Set(name, value)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if t.config.sourceOrigin != "" {
		request.Header.Set("Origin", t.config.sourceOrigin)
	}
	if t.config.secret != "" {
		if descriptor.Signing == "" {
			return fail(errors.New("operation does not support signing"))
		}
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		signingTarget := request.URL.RequestURI()
		if descriptor.Signing == "mailing" {
			signingTarget = request.URL.EscapedPath()
		}
		mac := hmac.New(sha256.New, []byte(t.config.secret))
		io.WriteString(mac, timestamp+"."+request.Method+"."+signingTarget+".")
		mac.Write(encoded)
		signature := hex.EncodeToString(mac.Sum(nil))
		switch descriptor.Signing {
		case "feedback":
			request.Header.Set("X-Feedback-Signature", "t="+timestamp+",v1="+signature)
		case "telemetry":
			request.Header.Set("X-Telemetry-Signature", "t="+timestamp+",v1="+signature)
		case "mailing":
			request.Header.Set("X-Mailing-Timestamp", timestamp)
			request.Header.Set("X-Mailing-Signature", signature)
		default:
			return fail(errors.New("unknown signing protocol"))
		}
	}
	response, err := t.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return fail(&TransportError{Cause: err})
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiErr := readAPIError(response)
		response.Body.Close()
		return fail(apiErr)
	}
	return response, cancel, nil
}
func readAPIError(response *http.Response) *APIError {
	const limit = 64 * 1024
	data, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	e := &APIError{Status: response.StatusCode, RequestID: response.Header.Get("X-Request-Id"), Headers: response.Header.Clone(), RawBody: data, Truncated: truncated || readErr != nil}
	var details map[string]any
	if !truncated && decodeJSON(data, &details) == nil {
		e.Details = details
		e.Code, _ = details["code"].(string)
		e.Message, _ = details["message"].(string)
		if e.Message == "" {
			e.Message, _ = details["error"].(string)
		}
	}
	return e
}

// multipartBody composes tiny headers and caller-owned streams without buffering
// the file, spawning a copying goroutine, or taking ownership of caller readers.
func multipartBody(form map[string]any) (io.Reader, string, error) {
	writer := multipart.NewWriter(io.Discard)
	boundary := writer.Boundary()
	names := make([]string, 0, len(form))
	for name := range form {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]io.Reader, 0, 3*len(names)+1)
	for _, name := range names {
		value := form[name]
		disposition := map[string]string{"name": name}
		contentType := ""
		var content io.Reader
		if upload, ok := value.(Upload); ok {
			if upload.Reader == nil || upload.Filename == "" {
				return nil, "", errors.New("upload requires a filename and reader")
			}
			disposition["filename"] = upload.Filename
			contentType = upload.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			if strings.ContainsAny(contentType, "\r\n") {
				return nil, "", errors.New("invalid upload content type")
			}
			content = upload.Reader
		} else {
			content = strings.NewReader(fmt.Sprint(value))
		}
		header := "--" + boundary + "\r\nContent-Disposition: " + mime.FormatMediaType("form-data", disposition) + "\r\n"
		if contentType != "" {
			header += "Content-Type: " + contentType + "\r\n"
		}
		parts = append(parts, strings.NewReader(header+"\r\n"), content, strings.NewReader("\r\n"))
	}
	parts = append(parts, strings.NewReader("--"+boundary+"--\r\n"))
	return io.MultiReader(parts...), writer.FormDataContentType(), nil
}
