package httputil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchMux builds a mux the size of this product's route table so the cost
// being measured is the cost that will actually be paid. A three-route mux
// would make the second match look free.
func benchMux(routes int) *http.ServeMux {
	mux := http.NewServeMux()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < routes; i++ {
		mux.Handle(fmt.Sprintf("GET /api/v1/r%d/{thing_id}/sub", i), h)
		mux.Handle(fmt.Sprintf("POST /api/v1/r%d/{thing_id}", i), h)
	}
	mux.Handle("GET /api/v1/channels/{channel_id}/messages", h)
	return mux
}

// THE GUARD MATCHES THE ROUTE A SECOND TIME, AND THAT IS THE WHOLE COST.
//
// mux.Handler(r) runs the same matching the mux is about to run, so every
// request in the product pays for two. That is worth knowing rather than
// assuming: the alternative designs (validating inside each handler, or
// translating the SQLSTATE) pay nothing here, and if the second match were
// expensive it would be an argument against this one.
//
// Run with: go test ./pkg/httputil/ -bench 'IDPathParams' -benchmem
func BenchmarkWithoutIDPathParams(b *testing.B) {
	mux := benchMux(100)
	req := httptest.NewRequest("GET",
		"/api/v1/channels/3f2504e0-4f89-11d3-9a0c-0305e82c3301/messages", nil)
	w := httptest.NewRecorder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

func BenchmarkWithIDPathParams(b *testing.B) {
	mux := benchMux(100)
	h := ValidateIDPathParams(mux)(mux)
	req := httptest.NewRequest("GET",
		"/api/v1/channels/3f2504e0-4f89-11d3-9a0c-0305e82c3301/messages", nil)
	w := httptest.NewRecorder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(w, req)
	}
}
