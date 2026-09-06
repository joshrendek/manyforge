package mailing

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestProfileVerificationRequiresSendRouteGroup(t *testing.T) {
	writeRouter := chi.NewRouter()
	sendRouter := chi.NewRouter()
	handler := NewHandler(nil)
	handler.WriteRoutes(writeRouter)
	handler.SendRoutes(sendRouter)

	path := "/businesses/00000000-0000-0000-0000-000000000001/mailing/sending-profile/verify"
	if writeRouter.Match(chi.NewRouteContext(), http.MethodPost, path) {
		t.Fatalf("write routes unexpectedly match profile verification path %s", path)
	}
	if !sendRouter.Match(chi.NewRouteContext(), http.MethodPost, path) {
		t.Fatalf("send routes do not match profile verification path %s", path)
	}
}
