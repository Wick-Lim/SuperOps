package httputil

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// THE DECODE FUNNEL REFUSES U+0000, AND SAYS WHICH FIELD.
//
// It is legal JSON and no Postgres text or jsonb column can store it, so every
// route that wrote a caller's string answered 500 for a body that is simply
// unstorable — measured on seven of them, including posting a channel message.
//
// The guard lives here rather than in each handler because the invariant is a
// property of the storage layer, and because a route written next year gets it
// without anyone remembering.
func TestDecodeRejectsNUL(t *testing.T) {
	type nested struct {
		Deep string `json:"deep"`
	}
	type body struct {
		Name    string         `json:"name"`
		Tags    []string       `json:"tags"`
		Config  map[string]any `json:"config"`
		Child   *nested        `json:"child"`
		Ignored string         `json:"-"`
	}

	// The escape, assembled so this source file holds no NUL of its own.
	esc := "\\u" + "0000"

	cases := []struct {
		name      string
		json      string
		wantField string // "" means it must be accepted
	}{
		{"a plain string field", `{"name":"a` + esc + `b"}`, "name"},
		{"an element of a slice", `{"tags":["ok","a` + esc + `b"]}`, "tags[1]"},
		{"a value inside a map", `{"config":{"k":"a` + esc + `b"}}`, "config.k"},
		{"a KEY inside a map", `{"config":{"a` + esc + `b":1}}`, "config"},
		{"a value nested two deep", `{"config":{"a":{"b":"x` + esc + `"}}}`, "config.a.b"},
		{"a field behind a pointer", `{"child":{"deep":"a` + esc + `b"}}`, "child.deep"},

		// MUST BE ACCEPTED: the six ordinary characters of the escape, which is
		// what a Windows path or a regex carries. An earlier version of this
		// check elsewhere searched the ENCODED form and refused these, telling
		// the caller their input held a character that was not in it.
		{"a literal backslash-u-0000", `{"name":"C:\\u0000-not-a-nul"}`, ""},
		{"an ordinary body", `{"name":"hello","tags":["a"],"config":{"k":1}}`, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v body
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.json))
			err := DecodeJSON(req, &v)

			if c.wantField == "" {
				if err != nil {
					t.Fatalf("refused a body with no NUL in it: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a body containing U+0000; the INSERT would answer 500")
			}
			appErr, ok := err.(*AppError)
			if !ok {
				t.Fatalf("error is %T, want *AppError", err)
			}
			if appErr.Status != 400 {
				t.Errorf("status = %d, want 400", appErr.Status)
			}
			if !strings.Contains(appErr.Message, c.wantField) {
				t.Errorf("message %q does not name the field %q", appErr.Message, c.wantField)
			}
			if !strings.Contains(appErr.Message, "NUL") {
				t.Errorf("message %q does not say what is wrong", appErr.Message)
			}
			if strings.ContainsRune(appErr.Message, 0) {
				t.Errorf("the message echoes the raw NUL back: %q", appErr.Message)
			}
		})
	}
}

// DEEP NESTING WAS A BYPASS, NOT A BOUND.
//
// The walk stopped at depth 64 and returned "nothing found", so a NUL far enough
// down passed as clean. Two units go per JSON object level (Interface, then
// Map), which put the real ceiling at 32 objects — and the comment justifying it
// claimed encoding/json refuses deeply nested input, which it does at 10,000,
// not 64. So there were ~9,968 levels the decoder accepted and the walk declined
// to look at.
func TestADeeplyNestedNULIsStillFound(t *testing.T) {
	for _, depth := range []int{5, 32, 40, 200, 1000} {
		t.Run(fmt.Sprintf("%d levels", depth), func(t *testing.T) {
			esc := "\\u" + "0000"
			body := `"a` + esc + `b"`
			for i := 0; i < depth; i++ {
				body = `{"d":` + body + `}`
			}
			var v struct {
				Config map[string]any `json:"config"`
			}
			req := httptest.NewRequest("POST", "/", strings.NewReader(`{"config":`+body+`}`))
			err := DecodeJSON(req, &v)
			if err == nil {
				t.Fatalf("a NUL %d levels deep was accepted; the INSERT would answer 500", depth)
			}
			if !strings.Contains(err.Error(), "NUL") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// EXCEEDING THE BOUND REFUSES. "I could not finish checking" is not "there is
// nothing here", and returning the same answer for both is what made the old
// limit a hole. Driven below the funnel because encoding/json will not build a
// value this deep.
func TestExceedingTheWalkDepthRefuses(t *testing.T) {
	var v any = "leaf"
	for i := 0; i < maxNULWalkDepth+10; i++ {
		v = map[string]any{"d": v}
	}
	if err := rejectNUL(v); err == nil {
		t.Error("a value too deep to check was reported clean")
	} else if !strings.Contains(err.Error(), "nested too deeply") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// THE PATH IS BUILT ON THE WAY OUT, so a clean body allocates nothing for it. It was built eagerly for every element and discarded: four allocations per
// map key, +480µs and +20,000 allocations on a 5,000-key config — the exact
// payload the workflow size test posts.
func TestTheWalkCostsLessThanTheDecodeItGuards(t *testing.T) {
	cfg := make(map[string]any, 5000)
	for i := 0; i < 5000; i++ {
		cfg[fmt.Sprintf("k%d", i)] = i
	}
	v := struct {
		Config map[string]any `json:"config"`
	}{Config: cfg}

	walk := testing.AllocsPerRun(20, func() { _ = rejectNUL(&v) })

	// The bar is the DECODE of the same body, which is the work the guard is
	// attached to. A guard that allocates more than the thing it guards is in
	// the wrong place; one that allocates less is proportionate. An absolute
	// number would only encode this machine.
	//
	// Reflect map iteration is not free — each Value costs a boxing allocation —
	// so zero was never available. What was available is not ALSO building a
	// path string per element, which the eager version did: four allocations per
	// key, +480µs and +20,000 allocations on this exact body.
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	decode := testing.AllocsPerRun(20, func() {
		var x struct {
			Config map[string]any `json:"config"`
		}
		_ = json.Unmarshal(body, &x)
	})

	t.Logf("walk %.0f allocs, decode %.0f allocs", walk, decode)
	if walk > decode {
		t.Errorf("the walk allocates %.0f times against the decode's %.0f: it is "+
			"building a path per element again", walk, decode)
	}
}

// A CALLER-CONTROLLED KEY IS BOUNDED AND SCRUBBED IN THE MESSAGE.
//
// A 100,000-character map key produced a 100 KB error body — an amplification
// primitive, and a large allocation on the path least worth spending one on.
// Control characters landed in it verbatim; safe only because the JSON encoder
// escapes them, which is not a property this should rely on.
func TestTheRefusalDoesNotEchoAnUnboundedKey(t *testing.T) {
	esc := "\\u" + "0000"
	long := strings.Repeat("K", 100000)
	req := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"config":{"`+long+`":"a`+esc+`b"}}`))
	var v struct {
		Config map[string]any `json:"config"`
	}
	err := DecodeJSON(req, &v)
	if err == nil {
		t.Fatal("accepted a body containing U+0000")
	}
	if n := len(err.Error()); n > 500 {
		t.Errorf("the refusal is %d bytes; a caller sets its size", n)
	}

	// THE OTHER AXIS. Capping each segment at 64 did not cap how many segments
	// there are: `config.a.a.a…` nested ten thousand deep — a 60 KB body the
	// decoder accepts — produced a 20 KB path. Same primitive, different shape.
	deep := `"a` + esc + `b"`
	for i := 0; i < 5000; i++ {
		deep = `{"aaaaaaaa":` + deep + `}`
	}
	// A FRESH VALUE. Reusing `v` merged this body into the map the failed decode
	// above had already populated, so the walk could hit the long-key entry
	// first — map iteration order is random — and report a short path. The
	// assertion then passed for a reason that had nothing to do with the bound,
	// and the mutation that removes the bound did not fail it.
	var deepV struct {
		Config map[string]any `json:"config"`
	}
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"config":`+deep+`}`))
	err = DecodeJSON(req, &deepV)
	if err == nil {
		t.Fatal("accepted a body containing U+0000")
	}
	if n := len(err.Error()); n > 500 {
		t.Errorf("a %d-level path produced a %d-byte refusal; the assembled path "+
			"is not bounded, only its segments are", 5000, n)
	}
	if !strings.ContainsRune(err.Error(), '…') {
		t.Errorf("the truncated path does not say it was truncated: %q", err.Error())
	}

	// And a key carrying control characters does not put them in the message.
	//
	// A FRESH VALUE for the same reason as above, and it was measured: reusing
	// `v` left the 100,000-"K" entry in the map, encoding/json merged this body
	// into it rather than replacing it, and with two NUL-carrying entries map
	// order picked the winner. Removing the control-character scrubbing failed
	// this assertion 5 times in 50 — 90% ineffective.
	var ctrlV struct {
		Config map[string]any `json:"config"`
	}
	req = httptest.NewRequest("POST", "/", strings.NewReader(
		`{"config":{"a\r\nX-Injected: 1":"a`+esc+`b"}}`))
	err = DecodeJSON(req, &ctrlV)
	if err == nil {
		t.Fatal("accepted a body containing U+0000")
	}
	if strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("control characters from a map key are in the message: %q", err.Error())
	}
}

// THE FAIL-CLOSED BOUND MUST NOT BE REACHABLE THROUGH THE DECODER, AND MUST NOT
// BLOW THE STACK BEFORE IT IS REACHED.
//
// The bound is 20,000 and encoding/json refuses past 10,000, so a body the
// decoder accepts can never trip the refusal — but 10,000 frames of recursion is
// real, and a goroutine stack that overflows is a crash, not a 400.
func TestTheDecoderCannotReachTheFailClosedBound(t *testing.T) {
	// Deepest the decoder will build, minus a margin.
	const deep = 9000
	body := `"leaf"`
	for i := 0; i < deep; i++ {
		body = `{"d":` + body + `}`
	}
	var v struct {
		Config map[string]any `json:"config"`
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"config":`+body+`}`))
	if err := DecodeJSON(req, &v); err != nil {
		t.Fatalf("a %d-deep body the decoder accepts was refused: %v", deep, err)
	}

	// And the same body with a NUL at the bottom is caught, not passed.
	esc := "\\u" + "0000"
	body = `"a` + esc + `b"`
	for i := 0; i < deep; i++ {
		body = `{"d":` + body + `}`
	}
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"config":`+body+`}`))
	err := DecodeJSON(req, &v)
	if err == nil {
		t.Fatalf("a NUL %d levels deep was accepted", deep)
	}
	if strings.Contains(err.Error(), "deeply") {
		t.Errorf("the bound fired on a body the decoder accepted: %v", err)
	}
}

// THE DEPTH MARKER MUST NOT BE FORGEABLE FROM A MAP KEY.
//
// rejectNUL distinguishes "I stopped looking" from "I found one here" by the
// suffix findNUL returns. A caller controls map keys, so a key spelling that
// suffix would make an ordinary refusal claim the body was too deeply nested —
// a wrong explanation for a request the caller can otherwise account for.
func TestTheDepthMarkerCannotBeForged(t *testing.T) {
	esc := "\\u" + "0000"
	// The key is the marker's own text, and the NUL is in its value.
	body := `{"config":{"(nested too deeply to check)":"a` + esc + `b"}}`
	var v struct {
		Config map[string]any `json:"config"`
	}
	err := DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v)
	if err == nil {
		t.Fatal("accepted a body containing U+0000")
	}
	// The phrase itself is expected here — it is the caller's own key, echoed
	// back as the field name. What must not happen is the DEDICATED depth
	// sentence, which claims something about the body's shape.
	if strings.HasPrefix(err.Error(), "the request body is nested too deeply") {
		t.Errorf("a map key forged the depth refusal: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "config.(nested too deeply to check) contains a NUL") {
		t.Errorf("the refusal does not name the field: %q", err.Error())
	}
}

// THE FIELD THE CALLER MUST CHANGE SURVIVES ANY NUMBER OF LONG ANCESTORS.
//
// The message is bounded, so something has to go when the path is long — and it
// must not be the leaf, which is the only part the caller can act on.
//
// Truncating from the head made that a function of two coupled constants: with a
// 64-rune per-segment cap and a 200-byte total, the leaf survived one long
// ancestor and two, and was lost at three. 64 was not a principled number, it
// was the largest value for which two still fit. Keeping the TAIL instead makes
// the guarantee unconditional — which is what this asserts, including at the
// count where the old shape broke.
func TestTheLeafSurvivesAnyNumberOfLongAncestors(t *testing.T) {
	esc := "\\u" + "0000"
	long := strings.Repeat("K", 300)

	for _, ancestors := range []int{1, 2, 3, 10} {
		t.Run(fmt.Sprintf("%d long ancestors", ancestors), func(t *testing.T) {
			body := `{"leaf":"a` + esc + `b"}`
			for i := 0; i < ancestors; i++ {
				body = `{"` + long + fmt.Sprint(i) + `":` + body + `}`
			}
			var v struct {
				Config map[string]any `json:"config"`
			}
			err := DecodeJSON(httptest.NewRequest("POST", "/",
				strings.NewReader(`{"config":`+body+`}`)), &v)
			if err == nil {
				t.Fatal("accepted a body containing U+0000")
			}
			if !strings.Contains(err.Error(), ".leaf ") {
				t.Errorf("the leaf is not named, so the ancestors ate the path: %q",
					err.Error())
			}
			if n := len(err.Error()); n > 500 {
				t.Errorf("the refusal is %d bytes; the path is not bounded", n)
			}
		})
	}
}

// THE ERROR PATH MUST NOT ALLOCATE IN PROPORTION TO A KEY THE CALLER CHOSE.
//
// boundPath bounds the MESSAGE, not the work of building it: without a segment
// cap each frame concatenates the full key before the trim, so a 100 KB key cost
// +428 KB and eight nested ones +703 KB to produce 261 bytes. Bounded intermediate
// now — and the cap is not what decides which segments survive, so this cannot
// drift back into the coupling it replaced.
func TestTheRefusalDoesNotAllocateInProportionToTheKey(t *testing.T) {
	esc := "\\u" + "0000"
	long := strings.Repeat("K", 100000)

	build := func(withNUL bool) string {
		leaf := `"ab"`
		if withNUL {
			leaf = `"a` + esc + `b"`
		}
		return `{"config":{"` + long + `":{"leaf":` + leaf + `}}}`
	}

	// BYTES, NOT COUNT. The first version of this measured AllocsPerRun and was
	// useless: removing the cap left the count identical at 59 either way, since
	// the same handful of concatenations happen — they are just enormous. What
	// changes is how much each one copies.
	measure := func(body string) uint64 {
		const runs = 5
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			var v struct {
				Config map[string]any `json:"config"`
			}
			_ = DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v)
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}

	clean := measure(build(false))
	refused := measure(build(true))
	t.Logf("clean %d B/op, refused %d B/op (key is %d bytes)", clean, refused, len(long))

	// The refusal may cost a little more than the decode it follows — it builds a
	// message. What it must not do is scale with the key: unbounded, a 100 KB key
	// added ~428 KB. Half the key is a generous line no bounded implementation
	// approaches and no machine-specific noise crosses.
	if refused > clean+uint64(len(long)/2) {
		t.Errorf("a refusal on a %d-byte key allocated %d B against a clean decode's "+
			"%d B: the path is being assembled from full-length keys again",
			len(long), refused, clean)
	}
}

// NOR IN PROPORTION TO HOW DEEPLY IT IS NESTED — the other axis, and the one
// that was 200x worse.
//
// Each frame prepends its own segment to the suffix below it, so without a
// short-circuit every level copies a string two bytes longer than the last and
// the total is quadratic in depth. Measured: a 54 KB body nested 9,000 deep
// allocated 86 MB to produce a 261-byte message, and five times the depth cost
// twenty-five times the bytes.
//
// boundPath keeps the tail, so once the suffix is already maxPath bytes nothing
// prepended in front of it can survive — which is what makes the short-circuit
// correct rather than a truncation of the answer.
//
// The sibling test above measures the key-SIZE axis with a single level and
// would not have caught this. Keys here are one character so no segment cap can
// help; only the short-circuit can.
func TestTheRefusalDoesNotAllocateInProportionToDepth(t *testing.T) {
	esc := "\\u" + "0000"

	measure := func(depth int, withNUL bool) uint64 {
		leaf := `"ab"`
		if withNUL {
			leaf = `"a` + esc + `b"`
		}
		body := leaf
		for i := 0; i < depth; i++ {
			body = `{"d":` + body + `}`
		}
		body = `{"config":` + body + `}`

		const runs = 5
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			var v struct {
				Config map[string]any `json:"config"`
			}
			_ = DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v)
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}

	const deep = 5000
	clean := measure(deep, false)
	refused := measure(deep, true)
	t.Logf("at depth %d: clean %d B/op, refused %d B/op", deep, clean, refused)

	// Quadratic, this was +26.6 MB. Bounded, it is a few hundred bytes. 1 MB
	// sits far above any bounded implementation and far below the old cost, so
	// it cannot flake and cannot pass a regression.
	if refused > clean+(1<<20) {
		t.Errorf("a refusal at depth %d allocated %d B against a clean decode's %d B: "+
			"the path assembly is quadratic in depth again", deep, refused, clean)
	}
}

// THE PATH MUST STILL POINT AT THE NUL, not merely be short.
//
// The short-circuit stops prepending once the suffix fills maxPath, so the
// reported path is a TAIL of the true one. That is only acceptable while the
// tail it keeps is genuinely the innermost part — a path naming a field the NUL
// is not in would be worse than a slow one, because the caller would go and look
// at the wrong place.
//
// WHAT THIS DOES NOT GUARD: boundPath's direction. With the short-circuit in
// place the assembled path lands just under maxPath for ordinary segments, so
// boundPath is a no-op here and reversing it changes nothing — measured, this
// test passes under head truncation alone. It fails only when BOTH are wrong,
// which is right: the property is held by the short-circuit, and the
// short-circuit alone is enough to hold it. Tail preservation has its own test,
// TestTheLeafSurvivesAnyNumberOfLongAncestors, whose 300-byte keys are what make
// boundPath actually act.
func TestTheReportedPathIsAlwaysASuffixOfTheRealOne(t *testing.T) {
	esc := "\\u" + "0000"

	for _, depth := range []int{0, 1, 5, 50, 500, 5000} {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			// A distinctive innermost field so the tail is identifiable, and
			// distinctive ancestors so a wrong tail is obvious.
			body := `{"needle":"a` + esc + `b"}`
			want := ".needle"
			for i := 0; i < depth; i++ {
				body = fmt.Sprintf(`{"anc%d":%s}`, i, body)
			}
			var v struct {
				Config map[string]any `json:"config"`
			}
			err := DecodeJSON(httptest.NewRequest("POST", "/",
				strings.NewReader(`{"config":`+body+`}`)), &v)
			if err == nil {
				t.Fatal("accepted a body containing U+0000")
			}
			msg := err.Error()

			// The path is everything before " contains a NUL".
			idx := strings.Index(msg, " contains a NUL")
			if idx < 0 {
				t.Fatalf("unexpected message shape: %q", msg)
			}
			got := msg[:idx]

			// A TRUNCATED PATH MUST SAY SO. The short-circuit leaves the
			// result under maxPath, so boundPath — which is what adds the
			// ellipsis when IT truncates — returns the string unchanged and
			// says nothing. `lvl91.lvl92.…target` then reads as a complete
			// path, and the caller goes looking for a field that is not there.
			// The true path always starts at "config"; anything shorter than
			// that plus the leaf has had ancestors dropped.
			if !strings.HasPrefix(got, "config") && !strings.HasPrefix(got, "…") {
				t.Errorf("the path %q is a tail of the real one but carries no "+
					"ellipsis, so it reads as complete", got)
			}
			if !strings.HasSuffix(got, want) {
				t.Errorf("the path is %q; it does not end at the field holding the "+
					"NUL (%q), so it points somewhere the caller would look in vain",
					got, want)
			}
			if len(msg) > 500 {
				t.Errorf("the message is %d bytes", len(msg))
			}
		})
	}
}
