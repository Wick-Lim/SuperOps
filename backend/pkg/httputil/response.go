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
	"unicode/utf8"
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
	// 22P05 for jsonb. Handlers do not check for it, so the answer was
	// `500 internal server error` for a body that is simply unstorable.
	//
	// Measured on seven routes before this existed, one byte apart in the same
	// request: posting a channel message (the hottest write in the product) and
	// editing one, creating a channel, a Drive folder, a workspace, an
	// invitation, and PATCHing your own display name. All 500 on the NUL and
	// succeed on the control.
	//
	// It belongs HERE rather than in each handler because the invariant is a
	// property of the storage layer, not of any one resource — and because a
	// route written next year gets it without anyone remembering. Packages with
	// something more specific to say still say it: they run before this is
	// reached, and name the field.
	if v != nil {
		if err := rejectNUL(v); err != nil {
			return err
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

// rejectNUL walks a decoded request body and refuses U+0000 in any string.
//
// It inspects the DECODED value rather than scanning the raw body for the
// escape. Scanning is the obvious cheap approach and it is wrong: a body
// carrying the six ordinary characters of that escape — a Windows path, a
// regex, JSON round-tripped as a string — encodes the backslash as two, so a
// naive search matches and refuses a payload with no NUL in it. Telling a user
// their input contains a character it does not is worse than the 500 this
// replaces. Getting it right from the bytes means counting preceding
// backslashes; the decoded value has no such ambiguity.
//
// Cost is one reflect walk over a body MaxBytesReader has already bounded.
// Measured (BenchmarkDecodeWithAndWithoutNULWalk): 183ns against a 1.9µs decode
// for a typical 400-byte body, 39.7µs against 413µs for a 100 KiB one, and 29ns
// for a 256 KiB binary field. Under a tenth of the decode in each case.
//
// It got there by not building the path on the way DOWN. Passing it as an
// argument meant `fmt.Sprintf` per element for a string discarded unless the
// walk finds something: on a 5,000-key config that was +480µs and +20,000
// allocations. Each frame now prepends its own segment on the way out.
func rejectNUL(v any) error {
	found, tooDeep, path := findNUL(reflect.ValueOf(v), 0)
	if !found {
		return nil
	}
	// The depth refusal gets its own sentence. Its marker is appended at the
	// DEEPEST point, so every ancestor is prefixed in front of it and the path
	// bound then cuts it off — leaving "a.a.a.a…" and no explanation of a
	// refusal the caller cannot otherwise account for.
	//
	// It is a distinct RESULT, not a string to look for. Matching on the text
	// let a caller forge it: a map key spelling the marker made an ordinary
	// refusal claim the body was too deeply nested. Keys are caller-supplied, so
	// anything derived from them is caller-supplied too.
	if tooDeep {
		return &AppError{
			Status: http.StatusBadRequest,
			Code:   "INVALID_BODY",
			Message: "the request body is nested too deeply to check for NUL " +
				"characters (U+0000), which cannot be stored",
		}
	}
	where := boundPath(strings.TrimPrefix(path, "."))
	if where == "" {
		where = "the request body"
	}
	return &AppError{
		Status: http.StatusBadRequest,
		Code:   "INVALID_BODY",
		Message: fmt.Sprintf("%s contains a NUL character (U+0000), which cannot be stored",
			where),
	}
}

// maxNULWalkDepth bounds the walk, and exceeding it REFUSES.
//
// It used to be 64 and to return "nothing found", which made it a bypass rather
// than a bound: two units are spent per JSON object level (Interface, then Map),
// so a NUL 32 objects deep passed as clean. encoding/json's own limit is 10,000,
// not 64 — an earlier comment here claimed the decoder made this a formality and
// was wrong by two orders of magnitude — so this now sits above it, where only a
// value the decoder would itself refuse can reach it.
//
// And it fails closed. "I could not finish checking" is not "there is nothing
// here", and returning the same answer for both is what made the old limit a
// hole.
const maxNULWalkDepth = 20000

// findNUL reports whether v contains U+0000, and where.
//
// The path is returned as a SUFFIX and each frame prepends its own segment, so
// nothing is built on the way down.
//
// EACH ARM STOPS PREPENDING once the suffix is already maxPath bytes, because
// boundPath keeps only the tail and nothing added in front of that can survive
// it. Without the short-circuit the assembly is quadratic in depth — every frame
// copies a suffix two bytes longer than the last, so Σ2i — and a 54 KB request
// nested 9,000 deep allocated 86 MB to produce a 261-byte message. Measured;
// five times the depth was twenty-five times the bytes. With it, the work is
// bounded by maxPath rather than by anything the caller sends. On the no-NUL path that means nothing is
// allocated for the path — but not nothing at all: reflect.MapIter's Key and
// Value each box a value, so a map costs about two allocations per entry, and
// there is no way around that without unsafe. Measured on a clean 5,000-key
// config: 10,049 allocations against the decode's 20,048.
func findNUL(v reflect.Value, depth int) (found, tooDeep bool, path string) {
	if !v.IsValid() {
		return false, false, ""
	}
	if depth > maxNULWalkDepth {
		return true, true, ""
	}
	switch v.Kind() {
	case reflect.String:
		if strings.ContainsRune(v.String(), 0) {
			return true, false, ""
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return findNUL(v.Elem(), depth+1)
		}
	case reflect.Slice, reflect.Array:
		// A BYTE SLICE HOLDS NUMBERS, NOT STRINGS, so there is nothing here to
		// find and walking it is pure cost. internal/collab decodes a Yjs CRDT
		// update as `Update []byte` through this funnel — binary, routinely full
		// of NUL bytes, and large. Measured before this arm existed: 16.8ms per
		// request for a 256 KiB update, against 119µs for a large ordinary body.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false, false, ""
		}
		for i := 0; i < v.Len(); i++ {
			if found, deep, suffix := findNUL(v.Index(i), depth+1); found {
				if len(suffix) >= maxPath {
					return true, deep, markTruncated(suffix)
				}
				return true, deep, fmt.Sprintf("[%d]%s", i, suffix)
			}
		}
	case reflect.Map:
		// MapRange rather than MapKeys: the latter materialises every key into a
		// fresh slice before the first comparison, which on a 5,000-key config
		// is 5,000 allocations spent to look at the first one.
		it := v.MapRange()
		for it.Next() {
			k := it.Key()
			// The KEY too: a jsonb object's keys are as unstorable as its values.
			if k.Kind() == reflect.String && strings.ContainsRune(k.String(), 0) {
				return true, false, ""
			}
			if found, deep, suffix := findNUL(it.Value(), depth+1); found {
				if len(suffix) >= maxPath {
					return true, deep, markTruncated(suffix)
				}
				return true, deep, "." + pathSegment(keyString(k)) + suffix
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			if found, deep, suffix := findNUL(v.Field(i), depth+1); found {
				if len(suffix) >= maxPath {
					return true, deep, markTruncated(suffix)
				}
				return true, deep, "." + fieldName(t.Field(i)) + suffix
			}
		}
	}
	return false, false, ""
}

// markTruncated says that what follows is a TAIL, not a whole path.
//
// The short-circuit stops prepending once the suffix fills maxPath, and the
// result is then usually under maxPath — so boundPath, which adds the ellipsis
// when IT truncates, returns the string unchanged and says nothing. A 743-byte
// true path was reported as a 200-byte tail with no marker: `lvl91.lvl92.…`
// reads as a complete path to a top-level field that does not exist, and the
// caller goes looking for it.
//
// IT IS NOT REDUNDANT WITH boundPath, and the window is narrow enough to invite
// exactly that conclusion. rejectNUL trims one leading "." before boundPath
// sees the path, so a raw path of 200 or 201 bytes arrives at 199 or 200 — at or
// under the bound, so boundPath declines to truncate and says nothing. Both are
// constructible: 201 is the minimum first short-circuit with a one-character
// key, and exactly 200 with an empty one. Marking at the short-circuit makes the
// predicate's exact value irrelevant; tuning the threshold instead would work
// today and break the moment anyone changes the trim, maxPath, or the ellipsis.
//
// Idempotent because every frame above the first short-circuit takes the same
// branch — and unforgeable, because only this function ever produces a suffix
// beginning with the ellipsis: the map arm emits "." and the slice arm "[".
func markTruncated(suffix string) string {
	if strings.HasPrefix(suffix, "…") {
		return suffix
	}
	return "…" + suffix
}

// maxPath bounds the reported path. Nesting is the axis that needed it:
// `{"config":{"a":{"a":{…}}}}` ten thousand deep — a 60 KB request the decoder
// accepts — produced a 20 KB path of `config.a.a.a…`, an amplification primitive
// on the error path.
const maxPath = 200

// boundPath keeps the TAIL, not the head.
//
// The leaf is the informative half — it names the field the caller has to change
// — and its ancestors are not. Truncating from the left gives `…KKKK.leaf`;
// truncating from the right gave `config.KKK…` and dropped the only part they
// can act on.
//
// This also removed a coupling between two constants. With head truncation the
// leaf survived only while `ancestors × (segmentCap + 4) + leaf <= maxPath`, so
// a per-segment cap of 64 was not a principled number — it was the largest value
// for which TWO long ancestors still left room, and a third lost the leaf again.
// Keeping the tail makes that guarantee unconditional and leaves maxPath as the
// only bound.
func boundPath(p string) string {
	if len(p) <= maxPath {
		return p
	}
	// Cut on a rune boundary so the message never carries a broken sequence.
	cut := len(p) - maxPath
	for cut < len(p) && !utf8.RuneStart(p[cut]) {
		cut++
	}
	return "…" + p[cut:]
}

// keyString names a map key without copying it.
//
// `fmt.Sprint(k.Interface())` boxes the key and formats it, which for a string
// materialises a full copy BEFORE pathSegment gets the chance to truncate — a
// 100 KB key cost 214 KB on the error path even with the cap in place. A string
// key needs neither: reflect.Value.String returns the header.
//
// The Kind guard is essential, not defensive: reflect.Value.String on an int key
// returns "<int Value>" rather than panicking, which would be silent garbage in
// the message. And the fallback is genuinely reachable — encoding/json decodes
// into map[int]string and into maps keyed by an encoding.TextUnmarshaler — it is
// just that no request type in this repo uses one.
func keyString(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprint(k.Interface())
}

// maxPathSegment bounds the INTERMEDIATE, and nothing else.
//
// It is deliberately not the old 64. That number decided which segments survived
// into the message, which coupled it to maxPath — the leaf lived only while
// `ancestors × (cap + 4) + leaf <= maxPath`, so two long ancestors fit and three
// did not. boundPath keeping the tail settled that question for good, and this
// constant has nothing to do with it.
//
// What it does is bound the garbage. Without it each frame concatenates the FULL
// key before boundPath trims the result: measured against a no-NUL control on
// the same body, a 100 KB key cost +428 KB of allocation and eight nested ones
// +703 KB, to build a 261-byte message. With MaxRequestBodyBytes at 1 MiB that
// ceiling is megabytes per refusal — modest, self-inflicted by a caller who is
// already being refused, and rate limited since the guard moved inside the
// chain, but there is no reason to pay it.
const maxPathSegment = 128

// pathSegment scrubs one component of the reported path, and bounds its length.
//
// The scrubbing is not cosmetic. A map key is caller-controlled, and the message
// is safe inside a JSON body today only because the encoder escapes control
// characters. That is not a property this function should depend on: the same
// string could reach a log line tomorrow.
//
// The fast path returns s unchanged, which for an invalid UTF-8 byte differs
// from what the loop below would do — `range` decodes it to RuneError and
// WriteRune writes the replacement character. Unreachable: this is only ever
// called on map keys from encoding/json, which substitutes U+FFFD at decode
// time, and on struct field names, which are ours.
func pathSegment(s string) string {
	if len(s) > maxPathSegment {
		cut := maxPathSegment
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControl(r) {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// fieldName prefers the JSON name, because that is what the caller sent and the
// only name they can act on.
func fieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
