package httputil

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
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

// The Encode errors below are deliberately discarded. WriteHeader has already
// been called, so the status line and headers are on the wire and there is no
// way to signal a failure to the client; the only realistic cause is the peer
// having gone away, which the server learns about anyway. Logging here would
// need a logger in every response helper for no actionable signal.

func JSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Data: data})
}

func JSONList(w http.ResponseWriter, status int, data interface{}, cursor string, hasMore bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{
		Data: data,
		Meta: &Meta{Cursor: cursor, HasMore: hasMore},
	})
}

func JSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{
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

	// U+0000 IS LEGAL JSON AND NO POSTGRES COLUMN CAN STORE IT.
	//
	// `\u0000` is a valid escape, encoding/json decodes it to a real NUL, and
	// every text and jsonb column then refuses the INSERT — 22021 for text,
	// 22P05 for jsonb. Handlers did not check for it, so the answer was
	// `500 internal server error` for a body that is simply unstorable.
	//
	// Measured on seven routes, one byte apart in the same request: posting a
	// channel message (the hottest write in the product) and editing one,
	// creating a channel, a Drive folder, a workspace, an invitation, and
	// PATCHing your own display name. All 500 on the NUL and succeed on the
	// control.
	//
	// It belongs HERE rather than in each handler because the invariant is a
	// property of the storage layer, not of any one resource — and because a
	// route written next year gets it without anyone remembering.
	if v != nil {
		stripNUL(v)
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
	_ = json.NewEncoder(w.ResponseWriter).Encode(Response{
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

// stripNUL removes U+0000 from every string in a decoded request body.
//
// IT STRIPS RATHER THAN REFUSES, and the reason is that this codebase had
// already decided that question twice before the guard existed.
// `mailbox.safeFilename` and `webhook.sanitizeName` both map every rune below
// 0x20 to -1, deliberately, because their input is attacker-supplied. Refusing
// at the funnel ran BEFORE either of them and turned two working paths into
// failures: a webhook whose display name carried a NUL went from posting to
// `400 "text is required"`, and — far worse — an inbound customer email with a
// NUL in an attachment filename went from being filed to being rejected
// outright, so a sender could make a customer's message disappear by naming a
// file carefully.
//
// Stripping fixes what refusing fixed and keeps both of those working. The seven
// routes that answered `500 internal server error` for an unstorable body now
// answer normally with the byte gone, which is a better outcome than a 400 the
// caller cannot act on: U+0000 renders as nothing, so a user who somehow sent
// one cannot see what they would be asked to remove.
//
// It works on the DECODED value, not the raw body. Scanning the bytes for the
// escape is the obvious cheap approach and it is wrong in a way that was
// measured twice: a body carrying the six ordinary characters of that escape —
// a Windows path, a regex, JSON round-tripped as a string — encodes the
// backslash as two, so a naive search matches text with no NUL in it. Getting
// that right from the bytes means counting preceding backslashes; the decoded
// value has no such ambiguity.
func stripNUL(v any) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return
	}
	stripValue(rv, 0)
}

// maxStripDepth bounds the walk. encoding/json refuses past 10,000 levels and
// each level here costs at most three frames, so this cannot be reached by a
// body the decoder accepted — it is a backstop, not a limit on request shapes.
// Unlike a refusal, stopping early here is safe: the strings below are simply
// left alone, and Postgres refuses them as it did before this existed.
const maxStripDepth = 40000

// stripValue removes NULs in place where it can, and returns a replacement plus
// true where it cannot.
//
// The split exists because a map's values and an interface's contents are not
// addressable: they have to be rebuilt and assigned back through SetMapIndex or
// Set. Struct fields and slice elements are addressable and are edited directly.
func stripValue(v reflect.Value, depth int) (reflect.Value, bool) {
	if depth > maxStripDepth || !v.IsValid() {
		return v, false
	}
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if !strings.ContainsRune(s, 0) {
			return v, false
		}
		cleaned := removeNUL(s)
		if v.CanSet() {
			v.SetString(cleaned)
			return v, false
		}
		out := reflect.New(v.Type()).Elem()
		out.SetString(cleaned)
		return out, true

	case reflect.Pointer:
		if !v.IsNil() {
			stripValue(v.Elem(), depth+1)
		}

	case reflect.Interface:
		if v.IsNil() {
			return v, false
		}
		inner := v.Elem()
		// An interface's contents are never addressable, so whatever comes back
		// has to be assigned onto the interface itself.
		if repl, changed := stripValue(inner, depth+1); changed {
			if v.CanSet() {
				v.Set(repl)
				return v, false
			}
			return repl, true
		}
		// A map or slice inside the interface was edited in place; nothing to
		// assign, because the header the interface holds still points at it.

	case reflect.Slice, reflect.Array:
		// A byte slice holds numbers, not strings — nothing to find, and walking
		// a 256 KiB CRDT update byte by byte cost 16.8ms before this arm.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v, false
		}
		for i := 0; i < v.Len(); i++ {
			if repl, changed := stripValue(v.Index(i), depth+1); changed {
				// An ARRAY inside a map is not addressable, so the element the
				// recursion rebuilt has to be assigned back — discarding it here
				// silently dropped the strip and left the NUL for Postgres.
				if v.Index(i).CanSet() {
					v.Index(i).Set(repl)
				} else {
					return rebuiltSequence(v, i, repl, depth), true
				}
			}
		}

	case reflect.Map:
		stripMap(v, depth)

	case reflect.Struct:
		t := v.Type()
		rebuilt := reflect.Value{}
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			// An EMBEDDED unexported struct type still has exported fields that
			// encoding/json fills and promotes, so skipping the whole field left
			// them unwalked. Descend into it — but only when it is ADDRESSABLE,
			// which is the condition under which the recursion can write in
			// place.
			//
			// Without that clause this PANICKED. reflect marks an embedded field
			// of unexported type read-only, and while that flag is not inherited
			// by the struct's own fields — which is why encoding/json can fill
			// them and why the addressable case works — it is carried by the
			// field itself. So when the parent is a map value the promoted field
			// is unsettable, the rebuild path below runs, and `rebuilt.Set(v)`
			// hits `reflect: reflect.Value.Set using value obtained using
			// unexported field`. Through RecoveryMiddleware that is a 500 with a
			// stack — the exact answer this funnel exists to prevent.
			//
			// The alternative is copying every non-addressable struct BEFORE
			// descending, since a fresh reflect.New value has settable promoted
			// fields. That is a copy on every such struct to reach a shape no
			// request type in this repo has, so the promoted field of an
			// embedded unexported type inside a map is left alone instead.
			embedded := f.Anonymous && v.Field(i).Kind() == reflect.Struct &&
				v.Field(i).CanAddr()
			if !f.IsExported() && !embedded {
				continue
			}
			repl, changed := stripValue(v.Field(i), depth+1)
			if !changed {
				continue
			}
			// A STRUCT inside a map is not addressable either.
			if v.Field(i).CanSet() {
				v.Field(i).Set(repl)
				continue
			}
			if !rebuilt.IsValid() {
				rebuilt = reflect.New(v.Type()).Elem()
				rebuilt.Set(v)
			}
			if rebuilt.Field(i).CanSet() {
				rebuilt.Field(i).Set(repl)
			}
		}
		if rebuilt.IsValid() {
			return rebuilt, true
		}
	}
	return v, false
}

// stripMap rewrites entries whose KEY or value carries a NUL.
//
// A key is as unstorable as a value — a jsonb object's keys go into the same
// column — and rewriting one means deleting the old entry, so the mutations are
// collected first rather than made while ranging.
func stripMap(v reflect.Value, depth int) {
	type rewrite struct{ oldKey, newKey, val reflect.Value }
	var pending []rewrite

	it := v.MapRange()
	for it.Next() {
		k, val := it.Key(), it.Value()

		newKey := k
		keyChanged := false
		if k.Kind() == reflect.String && strings.ContainsRune(k.String(), 0) {
			newKey = reflect.New(k.Type()).Elem()
			newKey.SetString(removeNUL(k.String()))
			keyChanged = true
		}

		newVal, valChanged := stripValue(val, depth+1)
		if keyChanged || valChanged {
			if !valChanged {
				newVal = val
			}
			pending = append(pending, rewrite{oldKey: k, newKey: newKey, val: newVal})
		}
	}

	// Applied after the range, so a rewritten key that collides with one already
	// present WINS: `{"ab":"legit","a\u0000b":"attacker"}` ends as
	// `{"ab":"attacker"}`, deterministically, regardless of JSON order. Both
	// values are the caller's own and no route treats a body map key as an
	// identity, so this decides nothing today — it is written down because the
	// ordering is not obvious and the opposite would be just as arbitrary.
	for _, r := range pending {
		if r.oldKey.String() != r.newKey.String() {
			v.SetMapIndex(r.oldKey, reflect.Value{})
		}
		v.SetMapIndex(r.newKey, r.val)
	}
}

// removeNUL is strings.Map's job, written out so the common case allocates
// nothing: the caller has already established there is a NUL to remove.
func removeNUL(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r != 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rebuiltSequence copies a non-addressable array so one element can be replaced,
// and finishes stripping the rest.
//
// Only arrays reach it: a slice's elements are always addressable, even when the
// slice itself sits in a map, because the header points at backing storage the
// map does not own.
//
// That invariant is asserted rather than assumed. `reflect.New(sliceType).Elem()`
// is a NIL slice, so reflect.Copy would move nothing and the Index below would
// panic with an index out of range — a load-bearing assumption failing as a
// crash rather than as a wrong answer.
func rebuiltSequence(v reflect.Value, at int, repl reflect.Value, depth int) reflect.Value {
	if v.Kind() != reflect.Array {
		// A slice here means the invariant above is wrong. Leaving the value
		// alone is the safe answer: the NUL survives to Postgres, which is the
		// behaviour before this function existed, rather than a panic.
		return v
	}
	out := reflect.New(v.Type()).Elem()
	reflect.Copy(out, v)
	out.Index(at).Set(repl)
	for i := at + 1; i < out.Len(); i++ {
		if r, changed := stripValue(out.Index(i), depth+1); changed {
			out.Index(i).Set(r)
		}
	}
	return out
}
