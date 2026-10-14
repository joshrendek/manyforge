package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	syncTimeout  = 10 * time.Second // per request
	primeTimeout = 3 * time.Second  // per request, and the whole prime sync budget
	maxSyncBody  = 32 << 20

	// The server caps a sync at 1000 ops and a 1 MiB body; stay well under both.
	maxOpsPerSync = 1000
	maxOpsBytes   = 512 << 10
	// maxPullRounds bounds how many `more: true` pages one sync follows.
	maxPullRounds = 20
)

var (
	errTokenRejected = errors.New("token rejected — run `mf login --token mfw_…`")
	errRateLimited   = errors.New("rate limited by the server (429); changes kept locally, retry later")
)

type syncRequest struct {
	Since int64 `json:"since"`
	Ops   []Op  `json:"ops"`
}

type opResult struct {
	Key   string `json:"key"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type syncResponse struct {
	Rev     int64      `json:"rev"`
	Project *Project   `json:"project"`
	Tasks   []Task     `json:"tasks"`
	Results []opResult `json:"results"`
	More    bool       `json:"more"`
}

type syncSummary struct {
	sent, failed, pulled, requests int
	cursor                         int64
}

// canSync reports whether r has a server and a token; the reason explains why not.
func (a *app) canSync(r *repo) (tok string, reason string) {
	if r.cfg.Server == "" {
		return "", "no server configured (local-only)"
	}
	tok, _ = a.token(r)
	if tok == "" {
		return "", fmt.Sprintf("no token for %s (run `mf login --token mfw_…`)", r.cfg.Server)
	}
	return tok, ""
}

// autoSync is the best-effort sync after a mutation: failures warn on stderr
// and leave the outbox for the next attempt.
func (a *app) autoSync(r *repo) {
	if a.noSync || r.cfg.Server == "" {
		return
	}
	tok, reason := a.canSync(r)
	if tok == "" {
		fprintf(a.stderr, "mf: not synced: %s\n", reason)
		return
	}
	if _, err := a.sync(context.Background(), r, tok, syncTimeout); err != nil {
		fprintf(a.stderr, "mf: sync failed, changes kept locally: %v\n", err)
	}
}

// sync pushes the outbox in capped batches and pulls every task changed since
// the cursor, following `more` pages. Each request is bounded by timeout and
// the whole sync by ctx. The state lock is held throughout so two concurrent
// mf processes never send the same ops twice; progress from completed rounds
// is saved even if a later round fails.
func (a *app) sync(ctx context.Context, r *repo, tok string, timeout time.Duration) (syncSummary, error) {
	var sum syncSummary
	endpoint, err := syncURL(r.cfg.Server)
	if err != nil {
		return sum, err
	}
	var roundErr error
	err = r.withState(func(st *State) error {
		roundErr = a.syncRounds(ctx, endpoint, tok, timeout, st, &sum)
		return nil
	})
	if err != nil {
		return sum, err
	}
	return sum, roundErr
}

func (a *app) syncRounds(ctx context.Context, endpoint, tok string, timeout time.Duration, st *State, sum *syncSummary) error {
	for pulls := 0; ; {
		batch := nextBatch(st.Outbox)
		resp, err := a.post(ctx, endpoint, tok, syncRequest{Since: st.Cursor, Ops: batch}, timeout)
		if err != nil {
			return err
		}
		sum.requests++
		// 2xx: every sent op was applied or rejected by the server; drop them all.
		st.Outbox = st.Outbox[len(batch):]
		a.applyResponse(st, batch, resp, sum)
		if len(st.Outbox) > 0 {
			continue
		}
		if pulls++; !resp.More || pulls >= maxPullRounds {
			return nil // anything left arrives on the next sync
		}
	}
}

// nextBatch is the longest outbox prefix within the per-request op-count and
// byte caps; a single oversized op still goes alone.
func nextBatch(outbox []Op) []Op {
	size := 0
	for i, op := range outbox {
		if i == maxOpsPerSync {
			return outbox[:i]
		}
		b, _ := json.Marshal(op)
		if size += len(b) + 1; size > maxOpsBytes && i > 0 {
			return outbox[:i]
		}
	}
	if outbox == nil {
		return []Op{} // "ops": [] rather than null
	}
	return outbox
}

func (a *app) applyResponse(st *State, sent []Op, resp *syncResponse, sum *syncSummary) {
	sum.sent += len(sent)
	created := map[string]bool{}
	for _, op := range sent {
		if op.Op == "create" {
			created[op.Key] = true
		}
	}
	for _, res := range resp.Results {
		if res.OK {
			continue
		}
		sum.failed++
		if created[res.Key] && looksLikeConflict(res.Error) {
			fprintf(a.stderr, "mf: sync: %s: key already exists on the server; local create not applied (%s)\n", res.Key, res.Error)
			continue
		}
		fprintf(a.stderr, "mf: sync: %s: %s\n", res.Key, res.Error)
	}
	for i := range resp.Tasks {
		t := resp.Tasks[i] // server snapshot wins
		st.Tasks[t.Key] = &t
	}
	sum.pulled += len(resp.Tasks)
	if resp.Project != nil {
		st.Project = resp.Project
	}
	if resp.Rev > st.Cursor {
		st.Cursor = resp.Rev
	}
	sum.cursor = st.Cursor
}

func looksLikeConflict(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "conflict") || strings.Contains(m, "exist")
}

func (a *app) post(ctx context.Context, endpoint, tok string, body syncRequest, timeout time.Duration) (*syncResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mf/"+version)
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxSyncBody))
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return nil, errTokenRejected
	case http.StatusTooManyRequests:
		return nil, errRateLimited
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var env struct{ Code, Message string }
		if json.Unmarshal(raw, &env) == nil && env.Code != "" {
			return nil, fmt.Errorf("server returned %d %s: %s", res.StatusCode, env.Code, env.Message)
		}
		return nil, fmt.Errorf("server returned %d", res.StatusCode)
	}
	var out syncResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode sync response: %w", err)
	}
	return &out, nil
}

// syncURL builds the sync endpoint. The bearer token must not cross the
// network in clear text, so plain http is only allowed to loopback hosts.
func syncURL(server string) (string, error) {
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil || u.Host == "" {
		return "", usagef("invalid server URL %q", server)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", usagef("refusing to send the token over plain http to %s; use https", host)
		}
	default:
		return "", usagef("invalid server URL %q (want https://…)", server)
	}
	return u.String() + "/api/v1/work/sync", nil
}
