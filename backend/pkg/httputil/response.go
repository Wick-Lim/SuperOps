package httputil

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// MaxRequestBodyBytes caps every JSON request body. Without it an
// unauthenticated POST /api/v1/auth/login streams into json.Decoder until the
// client stops or the process dies — a single connection can pin arbitrary
// memory before any credential is checked. File uploads do not go through
// DecodeJSON, so 1 MiB is generous for every JSON endpoint in the API.
const MaxRequestBodyBytes int64 = 1 << 20

type Response struct {
	Data  interface{} `json:"data"`
	Meta  *Meta       `json:"meta,omitempty"`
	Error *ErrorBody  `json:"error,omitempty"`
}

type Meta struct {
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"has_more"`
}

type ErrorBody struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

func JSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{Data: data})
}

func JSONList(w http.ResponseWriter, status int, data interface{}, cursor string, hasMore bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{
		Data: data,
		Meta: &Meta{Cursor: cursor, HasMore: hasMore},
	})
}

func JSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{
		Error: &ErrorBody{Code: code, Message: message},
	})
}

// DecodeJSON reads a bounded JSON body into v with unknown fields rejected.
// Every error it returns is an *AppError, so a handler may forward it straight
// to HandleError and get the right status without leaking parser internals.
func DecodeJSON(r *http.Request, v interface{}) error {
	return DecodeJSONLimit(r, v, MaxRequestBodyBytes)
}

// DecodeJSONLimit is DecodeJSON with an explicit byte cap, for the rare
// endpoint whose payload is legitimately larger than the default.
func DecodeJSONLimit(r *http.Request, v interface{}, limit int64) error {
	defer r.Body.Close()

	body := http.MaxBytesReader(nil, r.Body, limit)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return NewPayloadTooLarge(fmt.Sprintf("request body exceeds %d bytes", limit))
		}
		return &AppError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID_BODY",
			Message: "malformed JSON body",
			Err:     err,
		}
	}

	return nil
}

// NotFoundHandler renders an unmatched route in the standard envelope.
// http.ServeMux's built-in fallback writes plain text "404 page not found",
// which every client that unconditionally parses {data,error} chokes on.
func NotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		JSONError(w, http.StatusNotFound, "NOT_FOUND", "endpoint not found")
	}
}

// MethodNotAllowedHandler renders a method mismatch in the standard envelope,
// preserving the Allow header the mux computed.
func MethodNotAllowedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		JSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

// EnvelopeMuxErrors wraps the root mux so its plain-text 404/405 responses
// become JSON envelopes.
//
// It is a wrapper rather than a mux.Handle("/", ...) catch-all on purpose:
// registering "/" makes every request match a pattern, which permanently
// disables ServeMux's 405 detection (and its Allow header) — a POST to a
// GET-only route would report 404. Interception keeps both statuses correct.
//
// Only responses the stdlib itself produced are rewritten: status 404/405 with
// the "text/plain; charset=utf-8" Content-Type that http.Error sets. Handlers
// in this codebase always answer application/json, so they pass through
// untouched.
func EnvelopeMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ew := &envelopeWriter{ResponseWriter: w}
		next.ServeHTTP(ew, r)
	})
}

type envelopeWriter struct {
	http.ResponseWriter
	rewriting bool // stdlib 404/405 detected; swallow its plain-text body
	written   bool
}

func (w *envelopeWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.written = true

	isStdlibError := (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) &&
		w.Header().Get("Content-Type") == "text/plain; charset=utf-8"

	if !isStdlibError {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	w.rewriting = true
	code, message := "NOT_FOUND", "endpoint not found"
	if status == http.StatusMethodNotAllowed {
		code, message = "METHOD_NOT_ALLOWED", "method not allowed"
	}
	// Keep any Allow header the mux set; replace only the body contract.
	w.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	json.NewEncoder(w.ResponseWriter).Encode(Response{
		Error: &ErrorBody{Code: code, Message: message},
	})
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	if w.rewriting {
		// The envelope has already been written; discard the stdlib's text.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Hijack delegates so the WebSocket upgrade survives this wrapper.
func (w *envelopeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

func (w *envelopeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
