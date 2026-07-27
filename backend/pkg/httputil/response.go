package httputil

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
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
	return decodeJSON(r, v, limit, true)
}

func decodeJSON(r *http.Request, v interface{}, limit int64, strip bool) error {
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
	if strip && v != nil {
		stripNUL(v)
	}

	return nil
}

// DecodeJSONVerbatim is DecodeJSONLimit for a body whose strings are KEYS
// rather than prose, and it is the exception to everything the block above
// argues for.
//
// Stripping repairs a NAME, a message or a filename: U+0000 renders as nothing,
// so removing it gives the caller what they meant to send. It does not repair a
// map key, because a key's identity decides which code runs. A workflow step
// config sent as `channel_id` with a trailing NUL is not a channel_id — but
// strip it and it becomes one, and the workflow posts to that channel. Measured: the same body
// answered 400 "the config of step 0 cannot contain a NUL character" before the
// funnel existed and 201 after, having invented a key the caller never spelled.
//
// A route reaching for this must refuse the NUL itself; nothing downstream will
// now. That is the point — the refusal it writes can name the step or the field,
// which a generic answer from here could not.
func DecodeJSONVerbatim(r *http.Request, v interface{}, limit int64) error {
	return decodeJSON(r, v, limit, false)
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

// stripValue removes NULs, and reports whether the value it was given is no
// longer what it was.
//
// `changed` MEANS "MODIFIED", NOT "ASSIGN MY RETURN" — the doc here said the
// latter for several rounds, and misreading it is what produced a snapshot that
// silently reverted an already-repaired field. The String arm returns true after
// writing in place. Callers that can ignore the returned value already do;
// assigning a value to itself is a no-op, and the two are told apart by identity
// where it matters (see the `newVal != val` comparison in stripMap).
//
// A rebuild is needed where the value is not ADDRESSABLE — a map's values, an
// interface's contents, and an array or struct sitting inside either. Struct
// fields and slice elements are addressable when their container is, which is
// most of the time and not always: a struct in a map is not, and that is what
// the rebuild path below exists for.
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
			// TRUE EVEN THOUGH IT WAS WRITTEN IN PLACE. `changed` means "this
			// value is not what it was", not "you must assign my return".
			// Returning false here lost every strip made inside a COPY: the
			// array arm walks a copy it has to hand back, and its caller only
			// hands it back when something changed. A caller that can ignore
			// the replacement already does — assigning a value to itself is a
			// no-op.
			return v, true
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
			// A REBUILD IS A DIFFERENT VALUE; an in-place edit hands the same
			// one back. Only the first needs assigning — treating both as a
			// rebuild made every level of a deeply nested body allocate an
			// interface box on the way out, +600 KB on a body that costs 2.1 MB
			// to decode.
			if repl == inner {
				return v, true
			}
			if v.CanSet() {
				v.Set(repl)
				return v, true
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
		// A SLICE'S ELEMENTS ARE ALWAYS SETTABLE, even inside a map: the header
		// points at backing storage the map does not own. An ARRAY inside a map
		// is not, so it is copied and walked addressably.
		//
		// It used to copy at the first element that needed a rebuild and re-walk
		// only what came after. That made one array answer three ways: elements
		// before the pivot kept their NULs (their recursion had already reported
		// "nothing I can do"), the pivot got whatever the rebuild produced, and
		// elements after it were stripped because the copy had made them
		// addressable. Two byte-identical elements came out different, and
		// changing element 1 decided whether element 0 was stripped.
		// THE ADDRESSABILITY GATE IS FOR CORRECTNESS, NOT COST — containsNUL
		// below already keeps a clean array from being copied, addressable or
		// not, so removing this changes no allocation. What it stops is an
		// ADDRESSABLE array taking the rebuild path at all: the Pointer arm
		// discards its recursion's return, so a rebuild made under a `*[2]string`
		// is thrown away and the NUL survives. Measured — without this,
		// `struct{P *[2]string}`, a top-level `[2]string` and
		// `map[string]*[2]string` all keep their NULs while the whole suite
		// stays green.
		//
		// `v.Index(0).CanSet()` and `v.CanSet()` are the same test — Value.Index
		// propagates the array's flags verbatim — so the choice between them is
		// style, not behaviour.
		if v.Kind() == reflect.Array && v.Len() > 0 && !v.Index(0).CanSet() {
			// COPIED ONLY ONCE SOMETHING NEEDS IT, and then re-walked FROM THE
			// START. Copying up front cost every clean array in a map a copy it
			// never used: 200 clean four-element arrays went from 400 to 600
			// allocations. Copying lazily was never the bug — re-walking from
			// `at+1` was, because it left elements before that point untouched
			// while later ones were stripped, so one array answered two ways.
			for i := 0; i < v.Len(); i++ {
				if !containsNUL(v.Index(i), depth+1) {
					continue
				}
				// Set, NOT reflect.Copy.
				//
				// `v` arrives here only from a non-addressable position — a map
				// value or an interface's contents. When the array type is
				// POINTER-SHAPED (exactly one pointer word: [1]map, [1]*T,
				// [1]struct{map}) reflect stores it DIRECTLY in the Value rather
				// than behind a pointer, and reflect.Copy assumes an array
				// source is indirect: it copies the first word of the pointed-to
				// object instead of the pointer itself. Measured on
				// `map[string][1]map[string]int`: the element's pointer becomes
				// 0x1, the map's `used` count read as an address.
				//
				// The consequences are not equal. A [1]map yields a garbage
				// pointer and a nil dereference — recoverable, a 500. A [1]*T
				// builds an address out of adjacent bytes and dies in
				// runtime.throw with "unexpected fault address", which
				// RecoveryMiddleware cannot catch: the process goes down and
				// takes every in-flight request with it.
				//
				// Value.Set handles the non-indirect case explicitly. It is also
				// the more honest call: Copy is for element-wise copying between
				// sequences of possibly different length, and this is a
				// whole-value copy between identical types.
				out := reflect.New(v.Type()).Elem()
				out.Set(v)
				for j := 0; j < out.Len(); j++ {
					if repl, changed := stripValue(out.Index(j), depth+1); changed {
						out.Index(j).Set(repl)
					}
				}
				return out, true
			}
			return v, false
		}
		modified := false
		for i := 0; i < v.Len(); i++ {
			if repl, changed := stripValue(v.Index(i), depth+1); changed {
				// Reachable for a slice whose element needed a rebuild — the
				// element is settable, so the value goes straight back.
				v.Index(i).Set(repl)
				modified = true
			}
		}
		return v, modified

	case reflect.Map:
		return v, stripMap(v, depth)

	case reflect.Struct:
		t := v.Type()
		rebuilt := reflect.Value{}
		modified := false
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			// An EMBEDDED unexported struct type still has exported fields that
			// encoding/json fills and promotes, so skipping the whole field left
			// them unwalked.
			embedded := f.Anonymous && v.Field(i).Kind() == reflect.Struct
			if !f.IsExported() && !embedded {
				continue
			}
			repl, changed := stripValue(v.Field(i), depth+1)
			if !changed {
				continue
			}
			modified = true
			// A STRUCT inside a map is not addressable either.
			//
			// Written to the REBUILD as well once one exists. It is a snapshot
			// taken at the first field that needed it, so a later field edited
			// in place was written to `v` and lost when `rebuilt` was returned
			// instead — an array of structs with an embedded unexported type
			// came back with the promoted field stripped and the plain one not,
			// which is neither of the two answers this is supposed to give.
			if v.Field(i).CanSet() {
				v.Field(i).Set(repl)
				if rebuilt.IsValid() && rebuilt.Field(i).CanSet() {
					rebuilt.Field(i).Set(repl)
				}
				continue
			}
			// THE REBUILD IS WHAT CANNOT SURVIVE A READ-ONLY VALUE, and it is
			// the only statement in the walk that cannot.
			//
			// reflect marks an embedded field of unexported type read-only. The
			// flag is not inherited by that struct's own fields — Value.Field
			// drops it, which is why encoding/json fills them — so it exists at
			// exactly one place: the embedded field itself, always Kind struct.
			// `rebuilt.Set(v)` reads it and panics with "reflect.Value.Set using
			// value obtained using unexported field", which through
			// RecoveryMiddleware is a 500 with a stack, from the guard that
			// exists to stop 500s.
			//
			// An earlier fix gated the DESCENT on addressability instead. That
			// stopped the panic and threw away far more: a map, a slice or a
			// pointee inside the embedded struct is mutated through
			// SetMapIndex or an element write, needs no addressability and never
			// reaches this line — yet all of them kept their NULs.
			//
			// WHAT STILL DEGRADES IS EVERY KIND THAT NEEDS A REBUILD, not just a
			// plain string as an earlier comment claimed. Measured with a NUL in
			// each: string, array, `any`, a nested struct and an array of
			// structs all keep it; map, slice and pointer do not, because those
			// three mutate in place. `any` is the one worth noticing — it is the
			// ordinary shape of decoded JSON.
			//
			// The rule underneath is "a field degrades iff repairing it needed a
			// rebuild", so array and nested-struct degrade only when what is
			// INSIDE them does: a `[1]map[string]string` or a
			// `struct{S []string}` in the same position is stripped fine.
			//
			// TWO CONSEQUENCES OF THAT DEGRADATION. `map[string]T` and
			// `map[string]*T` give different answers for the same T, because a
			// pointee is addressable. And within ONE array the answer can differ
			// PER ELEMENT the answer used to differ too: the array was copied at the
			// first element needing a rebuild and re-walked only after it, so
			// earlier elements kept their NULs while later ones lost them. The
			// copy is taken for the whole array now, so every element gets the
			// same treatment — see TestEveryElementOfAnArrayGetsTheSameTreatment.
			//
			// NO REBUILD FOR AN ADDRESSABLE v: every edit already landed in it,
			// so a snapshot has no job — and taking one caused the loss it was
			// meant to prevent. The only field that reaches here with v
			// addressable is an embedded unexported struct, and
			// `reflect.New(T).Elem()` carries the same read-only flag on that
			// field, so the snapshot is created by the one field kind that can
			// never be written into it. With two of them, the second edited v
			// after the snapshot and was reverted when the snapshot came back:
			// `{"a":"p\u0000q","b":"r\u0000s"}` gave A="pq" B="r\x00s", a value
			// that was correct in memory and then overwritten.
			//
			// !CanInterface is the read-only case, which cannot be copied at
			// all. CanAddr is the case where copying is pointless.
			if !v.CanInterface() || v.CanAddr() {
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
		return v, modified
	}
	return v, false
}

// stripMap rewrites entries whose KEY or value carries a NUL.
//
// A key is as unstorable as a value — a jsonb object's keys go into the same
// column — and rewriting one means deleting the old entry, so the mutations are
// collected first rather than made while ranging.
func stripMap(v reflect.Value, depth int) bool {
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

		// `changed` means "this value is not what it was", which includes the
		// case where the recursion edited it IN PLACE and handed the same value
		// back. Re-writing that through SetMapIndex is pure cost, and at depth
		// it is not small: every level of a 5,000-deep body paid one, +600 KB
		// for a body that allocates 2.1 MB to decode.
		//
		// Comparing the returned Value against what the iterator held
		// distinguishes the two: a rebuild is a different Value, an in-place
		// edit is the same one.
		//
		// This compares IDENTITY, not the values represented — reflect.Value's
		// own documentation warns that `==` does not do the latter, and identity
		// is exactly what is wanted. It is sound structurally rather than by
		// luck: every rebuild in this walk is `reflect.New(T).Elem()`, which
		// carries flagAddr, while `MapIter.Value()` and `Value.Elem()` never do,
		// so a rebuild can never compare equal to what was handed in.
		newVal, valChanged := stripValue(val, depth+1)
		rebuiltVal := valChanged && newVal != val
		if keyChanged || rebuiltVal {
			if !rebuiltVal {
				newVal = val
			}
			pending = append(pending, rewrite{oldKey: k, newKey: newKey, val: newVal})
		}
	}

	// SORTED, so the outcome is a function of the request rather than of Go's
	// randomised map iteration order.
	//
	// Which entry survives a collision is WHICHEVER ORIGINAL KEY SORTS LAST among
	// those in `pending` — not "the stripped one", which is what an earlier
	// comment here said and what the pre-sort code happened to do.
	// `{"ab":"pre\u0000sent","a\u0000b":"stripped"}` keeps "present", because
	// "a\u0000b" sorts first; `{"ab":"present","a\u0000b":"stripped"}` keeps
	// "stripped", because the clean entry is not pending at all.
	//
	// Unsorted, this was a coin flip: `{"a\u0000b":"first","ab\u0000":"second"}`
	// stored "first" 55 times and "second" 345 over 400 runs of a byte-identical
	// request. Which key wins is arbitrary either way — arbitrary and stable is
	// what makes a bug report actionable.
	//
	// Order matters for exactly one interaction: two entries writing the same
	// newKey, where the last write wins. Everything else is order-free — a
	// delete happens only when the key changed, a changed key's new form never
	// carries a NUL, so deletes and writes touch disjoint keys and a delete can
	// never remove an earlier write.
	//
	// That is also why sorting on Value.String is enough even though it renders
	// every non-string key as "<int Value>". Only a string key is ever rewritten,
	// so only string keys can collide; non-string entries are in `pending`
	// because their VALUE was stripped, they each write their own distinct key,
	// and their relative order cannot change the resulting map. Measured: with
	// every int key comparing equal, a `map[int]string` is stable over 300 runs.
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].oldKey.String() < pending[j].oldKey.String()
	})
	for _, r := range pending {
		if r.oldKey.String() != r.newKey.String() {
			v.SetMapIndex(r.oldKey, reflect.Value{})
		}
		v.SetMapIndex(r.newKey, r.val)
	}
	return len(pending) > 0
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

// containsNUL reports whether a value holds U+0000 anywhere, without modifying
// it.
//
// It exists so a NON-ADDRESSABLE array is copied only when the copy will be
// used. Every other container is walked directly, because it can be edited in
// place; an array in a map cannot, and paying a copy per clean array is a cost
// every request with one would carry.
//
// The read-only cases it cannot inspect answer false, which is the same answer
// the walk gives them: they are left alone either way.
//
// IT MUST AGREE WITH stripValue, and that is the one silent failure mode of this
// design: a shape where this answers false and the walk would have changed
// something means the array is never copied and the NUL survives, with nothing
// erroring anywhere. The two mirror each other by hand, which is the arrangement
// that drifts, so TestContainsNULAgreesWithTheWalk drives both over the same
// bodies and requires the same answer.
//
// It costs a SECOND traversal of a dirty array's subtree — linear, and paid only
// by arrays that turn out to need the copy. A clean one is traversed once and
// copied never, which is the case worth optimising: 200 clean four-element
// arrays in a map went from 400 allocations to 600 when the copy was
// unconditional.
func containsNUL(v reflect.Value, depth int) bool {
	if depth > maxStripDepth || !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return strings.ContainsRune(v.String(), 0)
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && containsNUL(v.Elem(), depth+1)
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if containsNUL(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			k := it.Key()
			if k.Kind() == reflect.String && strings.ContainsRune(k.String(), 0) {
				return true
			}
			if containsNUL(it.Value(), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			embedded := f.Anonymous && v.Field(i).Kind() == reflect.Struct
			if !f.IsExported() && !embedded {
				continue
			}
			if containsNUL(v.Field(i), depth+1) {
				return true
			}
		}
	}
	return false
}
