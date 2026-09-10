package manyforge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelayedSessionOwnerUsesTokenReceiptTime(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			fmt.Fprint(w, `{"access_token":"expired","refresh_token":"refresh","expires_in":1}`)
		case "/api/v1/auth/refresh":
			refreshes.Add(1)
			fmt.Fprint(w, `{"access_token":"replacement","refresh_token":"rotated","expires_in":300}`)
		default:
			if r.Header.Get("Authorization") != "Bearer replacement" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"code":"TOKEN_EXPIRED","message":"expired"}`)
				return
			}
			fmt.Fprint(w, `{"items":[],"next_cursor":null}`)
		}
	}))
	defer server.Close()
	bootstrap, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer bootstrap.Close()
	pair, err := bootstrap.Auth.Login(context.Background(), LoginRequest{Email: "fixture@example.test", Password: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	// Receiving a pair does not force callers to construct its owner immediately.
	time.Sleep(1100 * time.Millisecond)
	session, err := SessionFromTokenPair(pair, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, WithSession(session))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Businesses.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("delayed owner refreshed %d times, want 1 before protected request", refreshes.Load())
	}
}

func TestEmptySuccessRequiresBodylessOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/logout" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithAccessToken("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Businesses.List(context.Background())
	var invalid *InvalidResponseError
	if !errors.As(err, &invalid) {
		t.Fatalf("empty typed success must fail decoding, got %v", err)
	}
	if err := client.Auth.Logout(context.Background(), LogoutRequest{RefreshToken: "fixture"}); err != nil {
		t.Fatal(err)
	}
}
