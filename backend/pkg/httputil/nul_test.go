package httputil

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// THE DECODE FUNNEL REMOVES U+0000 FROM EVERY STRING IN A REQUEST BODY.
//
// It is legal JSON and no Postgres text or jsonb column can store it, so every
// route that wrote a caller's string answered 500 for a body that is simply
// unstorable — measured on seven of them, including posting a channel message,
// which is the hottest write in the product.
//
// The guard lives here rather than in each handler because the invariant is a
// property of the storage layer, and because a route written next year gets it
// without anyone remembering.
//
// It STRIPS rather than refuses, which is what this codebase had already decided
// twice: mailbox.safeFilename and webhook.sanitizeName both map every rune below
// 0x20 to -1 because their input is attacker-supplied. Refusing ran before both
// of them — see TestStrippingKeepsSanitisedPathsWorking.
func TestDecodeStripsNUL(t *testing.T) {
	type nested struct {
		Deep string `json:"deep"`
	}
	type body struct {
		Name   string         `json:"name"`
		Tags   []string       `json:"tags"`
		Config map[string]any `json:"config"`
		Child  *nested        `json:"child"`
	}

	// The escape, assembled so this source file holds no NUL of its own.
	esc := "\\u" + "0000"

	cases := []struct {
		name  string
		json  string
		check func(*testing.T, body)
	}{
		{"a plain string field", `{"name":"a` + esc + `b"}`, func(t *testing.T, v body) {
			if v.Name != "ab" {
				t.Errorf("name = %q, want %q", v.Name, "ab")
			}
		}},
		{"an element of a slice", `{"tags":["ok","a` + esc + `b"]}`, func(t *testing.T, v body) {
			if len(v.Tags) != 2 || v.Tags[1] != "ab" {
				t.Errorf("tags = %q", v.Tags)
			}
		}},
		{"a value inside a map", `{"config":{"k":"a` + esc + `b"}}`, func(t *testing.T, v body) {
			if v.Config["k"] != "ab" {
				t.Errorf("config[k] = %v", v.Config["k"])
			}
		}},
		{"a KEY inside a map", `{"config":{"a` + esc + `b":1}}`, func(t *testing.T, v body) {
			if _, ok := v.Config["ab"]; !ok {
				t.Errorf("config = %v, want the key rewritten to \"ab\"", v.Config)
			}
			for k := range v.Config {
				if strings.ContainsRune(k, 0) {
					t.Errorf("a key still carries a NUL: %q", k)
				}
			}
		}},
		{"a value nested two deep", `{"config":{"a":{"b":"x` + esc + `"}}}`, func(t *testing.T, v body) {
			inner, _ := v.Config["a"].(map[string]any)
			if inner["b"] != "x" {
				t.Errorf("config.a.b = %v, want %q", inner["b"], "x")
			}
		}},
		{"an element of an array inside a map", `{"config":{"a":["p` + esc + `q"]}}`,
			func(t *testing.T, v body) {
				arr, _ := v.Config["a"].([]any)
				if len(arr) != 1 || arr[0] != "pq" {
					t.Errorf("config.a = %v", arr)
				}
			}},
		{"a field behind a pointer", `{"child":{"deep":"a` + esc + `b"}}`, func(t *testing.T, v body) {
			if v.Child == nil || v.Child.Deep != "ab" {
				t.Errorf("child = %+v", v.Child)
			}
		}},

		// MUST BE LEFT ALONE: the six ordinary characters of the escape, which
		// is what a Windows path or a regex carries. A check that searched the
		// ENCODED form matched these, because encoding/json escapes a backslash
		// too — as a refusal it told callers their input held a character that
		// was not in it, and as a stripper it would silently delete text.
		{"a literal backslash-u-0000", `{"name":"C:\\u0000-not-a-nul"}`, func(t *testing.T, v body) {
			if v.Name != `C:\u0000-not-a-nul` {
				t.Errorf("name = %q; a literal escape was mangled", v.Name)
			}
		}},
		{"an ordinary body", `{"name":"hello","tags":["a"],"config":{"k":1}}`,
			func(t *testing.T, v body) {
				if v.Name != "hello" || len(v.Tags) != 1 || v.Config["k"] != float64(1) {
					t.Errorf("an ordinary body was altered: %+v", v)
				}
			}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v body
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.json))
			if err := DecodeJSON(req, &v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			c.check(t, v)
		})
	}
}

// STRIPPING IS WHAT KEEPS TWO SANITISED PATHS WORKING.
//
// An earlier version REFUSED, which ran before the handler-level sanitisers that
// already strip every rune below 0x20 — so a webhook whose display name carried
// a NUL went from posting to `400 "text is required"`, and an inbound customer
// email with a NUL in an attachment filename went from being filed to being
// rejected, which let a sender make a customer's message disappear by naming a
// file carefully.
//
// Both fields are decoded through this funnel, so this is what they see: the NUL
// goes, everything else survives, and the handler's own sanitiser then does its
// job on a value it recognises.
func TestStrippingKeepsSanitisedPathsWorking(t *testing.T) {
	esc := "\\u" + "0000"
	var v struct {
		Filename string `json:"filename"`
		Username string `json:"username"`
		Text     string `json:"text"`
	}
	body := `{"filename":"../../etc/passwd` + esc + `\r\nX-Injected: yes",` +
		`"username":"build` + esc + `bot","text":"deploy finished"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	if err := DecodeJSON(req, &v); err != nil {
		t.Fatalf("a body a sanitiser would have handled was refused: %v", err)
	}
	if strings.ContainsRune(v.Filename, 0) || strings.ContainsRune(v.Username, 0) {
		t.Fatal("a NUL survived the funnel")
	}
	// Everything the sanitisers exist to handle must still reach them.
	if !strings.Contains(v.Filename, "../../etc/passwd") ||
		!strings.Contains(v.Filename, "X-Injected") {
		t.Errorf("filename = %q: the funnel removed more than the NUL, so the "+
			"sanitiser is no longer tested against what it is for", v.Filename)
	}
	if v.Username != "buildbot" {
		t.Errorf("username = %q, want %q", v.Username, "buildbot")
	}
	if v.Text != "deploy finished" {
		t.Errorf("text = %q: an untouched field was altered", v.Text)
	}
}

// DEEP NESTING IS NOT A BYPASS.
//
// An earlier version stopped at depth 64 and reported "nothing found", so a NUL
// far enough down survived: two units go per JSON object level, which put the
// real ceiling at 32 objects, and the comment justifying it claimed
// encoding/json refuses deeply nested input — which it does at 10,000, not 64.
func TestADeeplyNestedNULIsStillRemoved(t *testing.T) {
	for _, depth := range []int{5, 32, 40, 200, 1000, 9000} {
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
			if err := DecodeJSON(req, &v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			cur := any(v.Config)
			for i := 0; i < depth; i++ {
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("level %d is %T", i, cur)
				}
				cur = m["d"]
			}
			if cur != "ab" {
				t.Errorf("the leaf %d levels down is %v, want %q — the walk stopped "+
					"short, so the NUL reaches Postgres and the route answers 500",
					depth, cur, "ab")
			}
		})
	}
}

// A BINARY FIELD IS NEITHER ALTERED NOR WALKED BYTE BY BYTE.
//
// internal/collab decodes `Update []byte` through this funnel — a Yjs CRDT
// update, binary and routinely full of NUL bytes. Two things must hold: the
// bytes survive exactly, because stripping them corrupts the document; and it
// does not cost a reflect call per byte, which measured 16.8ms per 256 KiB.
func TestABinaryFieldIsNeitherAlteredNorWalkedPerByte(t *testing.T) {
	var input struct {
		Update []byte `json:"update"`
	}
	raw := make([]byte, 256<<10)
	for i := range raw {
		if i%3 == 0 {
			raw[i] = 0
		} else {
			raw[i] = byte(i)
		}
	}
	body := fmt.Sprintf(`{"update":%q}`, base64.StdEncoding.EncodeToString(raw))

	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	if err := DecodeJSONLimit(req, &input, 4<<20); err != nil {
		t.Fatalf("a binary field was refused: %v", err)
	}
	if len(input.Update) != len(raw) {
		t.Fatalf("the binary field lost %d bytes: NULs were stripped from a CRDT "+
			"update, which corrupts the document", len(raw)-len(input.Update))
	}
	for i := range raw {
		if input.Update[i] != raw[i] {
			t.Fatalf("byte %d changed from %d to %d", i, raw[i], input.Update[i])
		}
	}

	// TIME, NOT ALLOCATIONS. Walking a []byte element by element allocates
	// NOTHING — it is reflect.Value.Index in a loop — so an allocation count is
	// the one quantity this regression does not move: with the skip deleted the
	// whole package still passed. Measured on this payload: 54ns with the skip,
	// and between 861µs and 1.03ms without across runs and machines — four
	// orders of magnitude either way.
	start := time.Now()
	const runs = 20
	for i := 0; i < runs; i++ {
		stripNUL(&input)
	}
	per := time.Since(start) / runs
	// The headroom is not symmetric, and the pass side is the generous one:
	// ~1850x on a clean walk (54ns against the bound) and ~9x on the regression
	// (measured between 861µs and 1.03ms across runs). A slower machine pushes
	// the regression further above the bound, not below it, so the asymmetry is
	// in the safe direction. Verified quiet, under load and with -race.
	if per > 100*time.Microsecond {
		t.Errorf("stripping a 256 KiB binary field takes %s: the byte-slice skip "+
			"is gone, so every collab update pays a reflect visit per byte", per)
	}
}

// A CLEAN BODY IS NOT COPIED — the overwhelmingly common case, and the one the
// cost has to be judged on.
func TestACleanBodyIsNotCopied(t *testing.T) {
	cfg := make(map[string]any, 5000)
	for i := 0; i < 5000; i++ {
		cfg[fmt.Sprintf("k%d", i)] = fmt.Sprintf("value-%d", i)
	}
	v := struct {
		Config map[string]any `json:"config"`
	}{Config: cfg}

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
	walk := testing.AllocsPerRun(20, func() { stripNUL(&v) })
	t.Logf("walk %.0f allocs, decode %.0f allocs", walk, decode)

	// The bar is the DECODE it is attached to: a guard that allocates more than
	// the thing it guards is in the wrong place. An absolute number would only
	// encode this machine.
	if walk > decode {
		t.Errorf("the walk allocates %.0f times against the decode's %.0f", walk, decode)
	}
}

// STRIPPING DOES NOT COPY TEXT IT LEAVES ALONE.
//
// Measured in BYTES, not allocation count: a size regression changes how big
// each allocation is and not how many there are, and reaching for AllocsPerRun
// here let exactly such a regression through once.
func TestStrippingDoesNotCopyUntouchedText(t *testing.T) {
	esc := "\\u" + "0000"
	long := strings.Repeat("K", 100000)

	measure := func(withNUL bool) uint64 {
		leaf := `"ab"`
		if withNUL {
			leaf = `"a` + esc + `b"`
		}
		body := `{"config":{"` + long + `":{"leaf":` + leaf + `}}}`
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

	clean := measure(false)
	stripped := measure(true)
	t.Logf("clean %d B/op, stripped %d B/op (the untouched key is %d bytes)",
		clean, stripped, len(long))

	if stripped > clean+uint64(len(long)/2) {
		t.Errorf("stripping allocated %d B against %d B for the same body with "+
			"nothing to strip: text that is not being changed is being copied",
			stripped, clean)
	}
}

// AND DOES NOT SCALE WITH DEPTH.
func TestStrippingDoesNotScaleWithDepth(t *testing.T) {
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
	stripped := measure(deep, true)
	t.Logf("at depth %d: clean %d B/op, stripped %d B/op", deep, clean, stripped)

	// A MEGABYTE OF SLACK WAS TOO MUCH TO SEE THE REGRESSION THAT ACTUALLY
	// HAPPENED. Redefining `changed` to mean "modified" — necessary, so that
	// edits made inside a copy are not discarded — made every level of a nested
	// body report a rebuild, and the interface arm boxed a fresh value at each
	// one: +600 KB, comfortably inside a 1 MiB bound. The cost of stripping must
	// not scale with depth at all, so the bound is a fraction of what the decode
	// itself costs rather than an absolute number.
	if stripped > clean+clean/10 {
		t.Errorf("stripping at depth %d allocated %d B against a clean %d B, over "+
			"a tenth more: the walk is paying per level again", deep, stripped, clean)
	}
}

// THREE SHAPES THE WALK USED TO SKIP SILENTLY.
//
// The Struct and Slice/Array arms discarded the (replacement, changed) return
// that the Map and Interface arms honour. When the container itself is not
// addressable — a struct or an array sitting inside a map — the rebuilt string
// was thrown away: no strip, no error, and Postgres 22021 downstream. And an
// embedded UNEXPORTED struct type was skipped whole, though encoding/json fills
// and promotes its exported fields.
//
// None of these is in a request type today. They are here because the funnel's
// justification is "a route written next year gets this without anyone
// remembering", and for these three it did not.
func TestTheWalkReachesShapesItUsedToSkip(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("a struct inside a map", func(t *testing.T) {
		var v struct {
			M map[string]struct {
				S string `json:"s"`
			} `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":{"s":"a`+esc+`b"}}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if got := v.M["k"].S; got != "ab" {
			t.Errorf("m.k.s = %q, want %q", got, "ab")
		}
	})

	t.Run("an array inside a map", func(t *testing.T) {
		var v struct {
			M map[string][2]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":["p`+esc+`q","ok"]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if got := v.M["k"][0]; got != "pq" {
			t.Errorf("m.k[0] = %q, want %q", got, "pq")
		}
		if got := v.M["k"][1]; got != "ok" {
			t.Errorf("m.k[1] = %q: an untouched element was altered", got)
		}
	})

	t.Run("an embedded unexported struct", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"title":"a`+esc+`b","body":"c`+esc+`d"}`))
		var v outerWithEmbedded
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.Title != "ab" {
			t.Errorf("the promoted field of an embedded unexported struct = %q, "+
				"want %q — the whole field was skipped", v.Title, "ab")
		}
		if v.Body != "cd" {
			t.Errorf("body = %q, want %q", v.Body, "cd")
		}
	})
}

type embeddedBase struct {
	Title string `json:"title"`
}

type outerWithEmbedded struct {
	embeddedBase
	Body string `json:"body"`
}

// THE REBUILT CONTAINERS MUST NOT LOSE THE EDITS MADE BEFORE THEM.
//
// The Struct arm sets a changed field in place when it can and takes a copy on
// the first field it cannot — so a struct with a settable field BEFORE a
// non-settable one has to carry both edits. The Array arm has the same shape:
// the array copy replaces one element and must finish stripping the rest.
func TestRebuiltContainersKeepEveryEdit(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("later array elements are still stripped", func(t *testing.T) {
		var v struct {
			M map[string][3]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":["a`+esc+`b","c`+esc+`d","e`+esc+`f"]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		want := [3]string{"ab", "cd", "ef"}
		if v.M["k"] != want {
			t.Errorf("m.k = %q, want %q — the array copy stopped at the first "+
				"element it replaced", v.M["k"], want)
		}
	})

	t.Run("an array of structs inside a map", func(t *testing.T) {
		type item struct {
			S string `json:"s"`
		}
		var v struct {
			M map[string][2]item `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[{"s":"a`+esc+`b"},{"s":"c`+esc+`d"}]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.M["k"][0].S != "ab" || v.M["k"][1].S != "cd" {
			t.Errorf("m.k = %+v, want both stripped", v.M["k"])
		}
	})

	t.Run("an array of arrays inside a map", func(t *testing.T) {
		var v struct {
			M map[string][2][2]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[["a`+esc+`b","c"],["d","e`+esc+`f"]]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		got := v.M["k"]
		if got[0][0] != "ab" || got[1][1] != "ef" || got[0][1] != "c" || got[1][0] != "d" {
			t.Errorf("m.k = %q", got)
		}
	})

	t.Run("a struct in a map with several changed fields", func(t *testing.T) {
		type item struct {
			A string `json:"a"`
			B string `json:"b"`
			C string `json:"c"`
		}
		var v struct {
			M map[string]item `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":{"a":"p`+esc+`q","b":"plain","c":"r`+esc+`s"}}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		got := v.M["k"]
		if got.A != "pq" || got.B != "plain" || got.C != "rs" {
			t.Errorf("m.k = %+v: the lazy rebuild dropped an edit", got)
		}
	})

	t.Run("an embedded exported struct still works", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"title":"a`+esc+`b","body":"c`+esc+`d"}`))
		var v struct {
			ExportedBase
			Body string `json:"body"`
		}
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.Title != "ab" || v.Body != "cd" {
			t.Errorf("got %+v", v)
		}
	})

	// AND AN ORDINARY BODY MUST NOT ALLOCATE A REBUILD. reflect.New on every
	// request would be a cost paid by everyone for a shape almost nobody sends.
	//
	// The bound is derived from the payload rather than picked: reflect's map
	// iteration costs about two allocations per entry and nothing else here
	// allocates, so anything above that is a rebuild. A one-entry map with a
	// bound of 10 could not see this — an unconditional reflect.New on every
	// struct passed it, because there was only one struct and 2.5x of slack.
	t.Run("a clean body takes no rebuild", func(t *testing.T) {
		type item struct {
			A string `json:"a"`
			B string `json:"b"`
		}
		const entries = 200
		m := make(map[string]item, entries)
		for i := 0; i < entries; i++ {
			m[fmt.Sprintf("k%d", i)] = item{A: "plain", B: "also plain"}
		}
		v := struct {
			M map[string]item `json:"m"`
		}{M: m}

		got := testing.AllocsPerRun(20, func() { stripNUL(&v) })
		// Two per entry for MapIter, plus a tenth of that for the walk itself.
		// A rebuild per struct would add another `entries`, so the slack is a
		// fifth of the smallest regression this can see — wide enough that a Go
		// release changing MapIter's cost by a little does not go red for no
		// reason, narrow enough that it cannot hide one.
		const bound = 2*entries + entries/5
		t.Logf("%d entries: %.0f allocs", entries, got)
		if got > bound {
			t.Errorf("a clean %d-entry body allocates %.0f times, over %d: a "+
				"rebuild is being taken when nothing changed", entries, got, bound)
		}
	})
}

// ExportedBase is embedded by the case above; a distinct type so the unexported
// case keeps testing what it names.
type ExportedBase struct {
	Title string `json:"title"`
}

// AN EMBEDDED UNEXPORTED TYPE INSIDE A MAP MUST NOT PANIC.
//
// reflect marks an embedded field of unexported type read-only. That flag is not
// inherited by the struct's own fields — which is why encoding/json fills them
// and why the addressable case above works — but the FIELD carries it. So when
// the parent is a map value the promoted field is unsettable, the struct arm
// takes its rebuild path, and `rebuilt.Set(v)` panics with "reflect.Value.Set
// using value obtained using unexported field".
//
// Through RecoveryMiddleware that is a 500 with a logged stack — the exact
// answer this funnel exists to prevent, arrived at by the funnel itself. The
// descent is gated on addressability now, so this shape degrades to what it did
// before: the promoted field is left alone, the rest is stripped.
func TestAnEmbeddedUnexportedTypeInAMapDoesNotPanic(t *testing.T) {
	esc := "\\u" + "0000"

	for _, c := range []struct {
		name   string
		body   string
		decode func() (any, func(*testing.T))
	}{
		{
			name: "a struct with an embedded unexported type",
			body: `{"m":{"k":{"title":"a` + esc + `b","body":"c` + esc + `d"}}}`,
			decode: func() (any, func(*testing.T)) {
				v := &struct {
					M map[string]outerWithEmbedded `json:"m"`
				}{}
				return v, func(t *testing.T) {
					if v.M["k"].Body != "cd" {
						t.Errorf("body = %q, want %q — the reachable half was not "+
							"stripped either", v.M["k"].Body, "cd")
					}
				}
			},
		},
		{
			name: "an array of them",
			body: `{"m":{"k":[{"title":"a` + esc + `b","body":"c` + esc + `d"}]}}`,
			decode: func() (any, func(*testing.T)) {
				v := &struct {
					M map[string][1]outerWithEmbedded `json:"m"`
				}{}
				return v, func(t *testing.T) {
					if v.M["k"][0].Body != "cd" {
						t.Errorf("body = %q, want %q", v.M["k"][0].Body, "cd")
					}
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, check := c.decode()
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			// A panic here is the regression; t.Fatal on it rather than letting
			// the whole package die.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("stripping panicked: %v\nthis reaches a caller as a "+
						"500 with a stack, through the guard meant to stop 500s", r)
				}
			}()
			if err := DecodeJSON(req, v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			check(t)
		})
	}
}

// THE SAME BYTES MUST PRODUCE THE SAME MAP.
//
// When two keys strip to the same thing they are both pending, and applying them
// in Go's randomised map-iteration order made the survivor a coin flip: a
// byte-identical request stored "first" 55 times and "second" 345 times over 400
// runs. Which key wins is arbitrary either way; irreproducible is the part that
// is not acceptable, because it makes a bug report impossible to act on.
func TestCollidingKeysResolveTheSameWayEveryTime(t *testing.T) {
	esc := "\\u" + "0000"

	for _, c := range []struct{ name, body, want string }{
		{"two dirty keys colliding", `{"m":{"a` + esc + `b":"first","ab` + esc + `":"second"}}`, "second"},
		{"a dirty key colliding with a clean one", `{"m":{"ab":"clean","a` + esc + `b":"dirty"}}`, "dirty"},
		// The winner is whichever ORIGINAL key sorts last among those being
		// rewritten — not "the stripped one", which is what the pre-sort code
		// happened to do and what the comment said for a while. Both of these
		// have two entries in the rewrite list, so both are decided by the sort.
		{"a clean key whose value is dirty", `{"m":{"ab":"cl` + esc + `ean","a` + esc + `b":"dirty"}}`, "clean"},
		{"the clean key sorts last", `{"m":{"ab":"cl` + esc + `ean","` + esc + `ab":"dirty"}}`, "clean"},
		{"three keys colliding", `{"m":{"a` + esc + `b":"one","ab` + esc + `":"two","` + esc + `ab":"three"}}`, "two"},
	} {
		t.Run(c.name, func(t *testing.T) {
			seen := map[string]int{}
			const runs = 200
			for i := 0; i < runs; i++ {
				var v struct {
					M map[string]any `json:"m"`
				}
				req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
				if err := DecodeJSON(req, &v); err != nil {
					t.Fatal(err)
				}
				got, _ := v.M["ab"].(string)
				seen[got]++
				if len(v.M) != 1 {
					t.Fatalf("the map has %d entries, want 1: %v", len(v.M), v.M)
				}
			}
			if len(seen) != 1 {
				t.Errorf("over %d runs of a byte-identical request the survivor "+
					"varied: %v — the outcome depends on map iteration order",
					runs, seen)
			}
			// AND WHICH ONE, not merely that it is always the same. The rule
			// is "whichever original key sorts last among those being
			// rewritten" — reversing the comparator leaves every subtest of the
			// determinism half green while flipping all five outcomes.
			for got := range seen {
				if got != c.want {
					t.Errorf("the survivor is %q, want %q: the winner is whichever "+
						"ORIGINAL key sorts last among those being rewritten",
						got, c.want)
				}
			}
		})
	}
}

// A NON-STRING-KEYED MAP IS STRIPPED, AND STABLE.
//
// `map[int]string` is a shape encoding/json decodes happily, and such an entry
// reaches the rewrite list whenever its VALUE carried a NUL — the key does not
// have to. It is stable for a reason worth knowing rather than by luck: only a
// string key is ever rewritten, so only string keys can collide, and two entries
// writing distinct keys cannot depend on the order they are written in.
//
// This asserts only what is observable. An earlier version also checked what a
// helper returned for an int key, which passed or failed with the helper rather
// than with anything a caller sees — the same shape as an assertion the
// regression does not move, which this branch has produced three times.
func TestANonStringKeyedMapIsStrippedAndStable(t *testing.T) {
	esc := "\\u" + "0000"
	seen := map[string]int{}
	const runs = 100
	for i := 0; i < runs; i++ {
		var v struct {
			M map[int]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/",
			strings.NewReader(`{"m":{"7":"a`+esc+`b","3":"c`+esc+`d"}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.M[7] != "ab" || v.M[3] != "cd" {
			t.Fatalf("values wrong: %v", v.M)
		}
		seen[fmt.Sprintf("%v", v.M)]++
	}
	if len(seen) != 1 {
		t.Errorf("a byte-identical body produced %d different maps: %v", len(seen), seen)
	}
}

// GATING THE REBUILD, NOT THE DESCENT, KEEPS MOST OF THE EMBEDDED SUBTREE.
//
// Only one statement in the walk cannot survive a read-only value: the copy
// taken when a changed field is not settable. An earlier fix gated the whole
// DESCENT on addressability, which stopped the panic and abandoned everything
// inside the embedded struct — a map, a slice and a pointee all kept their NULs,
// though none of them needs addressability to be mutated.
//
// What still degrades is every field whose repair needed a rebuild — string,
// array, `any`, a nested struct, an array of structs — not "a plain string", as
// this comment said while the code it documents was corrected. The three that
// survive are the three that mutate in place.
func TestTheEmbeddedSubtreeIsStrippedExceptWhereACopyIsNeeded(t *testing.T) {
	esc := "\\u" + "0000"
	var v struct {
		K map[string]embeddedOuter `json:"k"`
	}
	body := `{"k":{"x":{"m":{"a` + esc + `":"v` + esc + `w"},"s":["p` + esc + `q"],` +
		`"p":{"title":"t` + esc + `u"},"title":"y` + esc + `z","body":"c` + esc + `d"}}}`
	if err := DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v); err != nil {
		t.Fatal(err)
	}
	got := v.K["x"]

	// Reachable without a copy — all of these must be clean.
	if len(got.S) != 1 || got.S[0] != "pq" {
		t.Errorf("a slice inside the embedded struct = %q, want [pq]", got.S)
	}
	if got.P == nil || got.P.Title != "tu" {
		t.Errorf("a pointee inside the embedded struct = %+v, want title tu", got.P)
	}
	// The length check is not decoration: without it this loop passes on an
	// empty map, which is what a bug that deletes rewritten keys and never
	// re-adds them produces.
	if len(got.M) != 1 {
		t.Errorf("the map inside the embedded struct has %d entries, want 1: %v",
			len(got.M), got.M)
	}
	for k, val := range got.M {
		if strings.ContainsRune(k, 0) || strings.ContainsRune(val, 0) {
			t.Errorf("a map inside the embedded struct kept a NUL: %q=%q", k, val)
		}
	}
	if got.Body != "cd" {
		t.Errorf("body = %q, want cd", got.Body)
	}

	// THE DOCUMENTED DEGRADATION, ASSERTED IN BOTH DIRECTIONS. A plain string in
	// the embedded struct needs the copy this refuses to take, so it keeps its
	// NUL. Left as a t.Logf, the only record of it was a comment — and a change
	// that silently started stripping it would be as unnoticed as one that
	// silently stopped.
	//
	// If this fails because the field IS stripped, that is not automatically
	// wrong: it means a copy is being taken. Check that it is deliberate and
	// what it costs, then move this assertion.
	if !strings.ContainsRune(got.Title, 0) {
		t.Errorf("the promoted string field is stripped (%q): a copy of every "+
			"non-addressable struct is being taken, which is the cost the "+
			"CanInterface guard exists to avoid", got.Title)
	}
}

type embeddedInner struct {
	Title string `json:"title"`
}

type embeddedPrivate struct {
	M     map[string]string `json:"m"`
	S     []string          `json:"s"`
	P     *embeddedInner    `json:"p"`
	Title string            `json:"title"`
}

type embeddedOuter struct {
	embeddedPrivate
	Body string `json:"body"`
}

// ONE ARRAY, ONE ANSWER — the same input must not come out two ways.
//
// The array arm used to copy at the FIRST element that needed a rebuild and
// re-walk only what came after, so one array answered three ways: elements
// before that point kept their NULs, the pivot got whatever the rebuild
// produced, and elements after it were stripped because the copy had made them
// addressable. Two byte-identical elements came out different, and changing
// element 1 decided whether element 0 was stripped. It copies once, up front.
func TestEveryElementOfAnArrayGetsTheSameTreatment(t *testing.T) {
	esc := "\\u" + "0000"
	type item struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	dirty := `{"a":"t` + esc + `x","b":"clean"}`
	pivot := `{"a":"ok","b":"b` + esc + `y"}`

	var v struct {
		M map[string][3]item `json:"m"`
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"m":{"k":[`+dirty+`,`+pivot+`,`+dirty+`]}}`))
	if err := DecodeJSON(req, &v); err != nil {
		t.Fatal(err)
	}
	got := v.M["k"]
	if got[0] != got[2] {
		t.Errorf("two byte-identical elements came out different: [0]=%+v [2]=%+v",
			got[0], got[2])
	}
	for i, el := range got {
		if strings.ContainsRune(el.A, 0) || strings.ContainsRune(el.B, 0) {
			t.Errorf("element %d kept a NUL: %+v", i, el)
		}
	}
}

// A REBUILD MUST NOT LOSE AN EDIT MADE AFTER IT WAS TAKEN.
//
// `rebuilt` is a snapshot of the struct at the first field that needed one. A
// later field edited IN PLACE was written to the original and lost when the
// snapshot was returned instead — so an array of structs with an embedded
// unexported type came back with the promoted field stripped and the plain one
// not, which is neither of the two answers this is supposed to give.
func TestARebuildKeepsEditsMadeAfterIt(t *testing.T) {
	esc := "\\u" + "0000"
	var v struct {
		M map[string][1]outerWithEmbedded `json:"m"`
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"m":{"k":[{"title":"a`+esc+`b","body":"c`+esc+`d"}]}}`))
	if err := DecodeJSON(req, &v); err != nil {
		t.Fatal(err)
	}
	got := v.M["k"][0]
	if got.Body != "cd" {
		t.Errorf("body = %q, want %q: an in-place edit made after the rebuild was "+
			"snapshotted did not survive it", got.Body, "cd")
	}
	// And the promoted field, which the copy makes reachable here.
	if strings.ContainsRune(got.Title, 0) {
		t.Errorf("title = %q: the array copy makes the promoted field settable, "+
			"so it should be stripped", got.Title)
	}
}

// EDGE SHAPES OF THE ARRAY COPY.
//
// The up-front copy is gated on `v.Len() > 0 && !v.Index(0).CanSet()`. Both
// halves of that gate are worth a case: an empty array must not index element
// zero, and an array that is already addressable must not be copied at all.
func TestTheArrayCopyGateHandlesItsEdges(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("an empty array in a map", func(t *testing.T) {
		var v struct {
			M map[string][0]string `json:"m"`
			N string               `json:"n"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[]},"n":"a`+esc+`b"}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatalf("an empty array panicked or was refused: %v", err)
		}
		if v.N != "ab" {
			t.Errorf("n = %q: the walk stopped at the empty array", v.N)
		}
	})

	t.Run("an addressable array is edited in place", func(t *testing.T) {
		var v struct {
			A [2]string `json:"a"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"a":["p`+esc+`q","r`+esc+`s"]}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.A[0] != "pq" || v.A[1] != "rs" {
			t.Errorf("a = %q, want [pq rs]", v.A)
		}

		// AND NOT COPIED, which is the half this asserted only in its name
		// while the copy was unconditional.
		//
		// It is no longer the addressability gate that carries this: containsNUL
		// runs first, so a CLEAN array is never copied whether it is addressable
		// or not, and removing the gate now changes nothing measurable. The
		// assertion is still worth making — it is the property, and the next
		// change to that ordering would be caught by it rather than by nothing.
		var clean struct {
			A [512]string `json:"a"`
		}
		for i := range clean.A {
			clean.A[i] = "ok"
		}
		if n := testing.AllocsPerRun(20, func() { stripNUL(&clean) }); n > 0 {
			t.Errorf("an addressable array allocates %.0f times: it is being "+
				"copied when it could be written in place", n)
		}
	})

	// The OUTER array is the one copied — it is the non-addressable map value.
	// The inner arrays live inside that copy, are addressable, and take the
	// in-place path.
	t.Run("an array of arrays, where the outer is the one copied", func(t *testing.T) {
		var v struct {
			M map[string][2][2]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[["a`+esc+`b","c`+esc+`d"],["e`+esc+`f","g"]]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		want := [2][2]string{{"ab", "cd"}, {"ef", "g"}}
		if v.M["k"] != want {
			t.Errorf("m.k = %q, want %q", v.M["k"], want)
		}
	})
}

// TWO EMBEDDED UNEXPORTED STRUCTS: the second must not be reverted.
//
// The rebuild is a snapshot, and the ONLY field that can reach it when `v` is
// addressable is an embedded unexported struct — everything exported is settable
// there. `reflect.New(T).Elem()` carries the same read-only flag on that field,
// so the snapshot is created by the one field kind that can never be written
// into it. With two of them the second edited the original after the snapshot
// and was reverted when the snapshot came back: a value correct in memory,
// overwritten on the way out.
//
// An addressable value needs no snapshot at all, which is the fix.
func TestASecondEmbeddedStructIsNotReverted(t *testing.T) {
	esc := "\\u" + "0000"
	body := `{"a":"p` + esc + `q","b":"r` + esc + `s"}`

	t.Run("as a struct field", func(t *testing.T) {
		var v struct {
			F twoEmbedded `json:"f"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"f":`+body+`}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.F.A != "pq" || v.F.B != "rs" {
			t.Errorf("A=%q B=%q, want pq/rs: the snapshot reverted a field edited "+
				"after it was taken", v.F.A, v.F.B)
		}
	})

	t.Run("as a slice element", func(t *testing.T) {
		var v struct {
			F []twoEmbedded `json:"f"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"f":[`+body+`]}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.F[0].A != "pq" || v.F[0].B != "rs" {
			t.Errorf("A=%q B=%q, want pq/rs", v.F[0].A, v.F[0].B)
		}
	})

	t.Run("three embedded", func(t *testing.T) {
		var v struct {
			F threeEmbedded `json:"f"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"f":{"a":"p`+esc+`q","b":"r`+esc+`s","c":"t`+esc+`u"}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.F.A != "pq" || v.F.B != "rs" || v.F.C != "tu" {
			t.Errorf("A=%q B=%q C=%q, want pq/rs/tu", v.F.A, v.F.B, v.F.C)
		}
	})
}

// A CLEAN ARRAY IN A MAP MUST NOT BE COPIED.
//
// The copy was taken up front, so every clean array paid for one it never used:
// 200 four-element arrays went from 400 allocations to 600. Copying lazily was
// never the bug — re-walking from the element after the copy was, which left
// earlier elements untouched while later ones were stripped.
func TestACleanArrayInAMapIsNotCopied(t *testing.T) {
	const entries = 200
	m := make(map[string][4]string, entries)
	for i := 0; i < entries; i++ {
		m[fmt.Sprintf("k%d", i)] = [4]string{"p", "q", "r", "s"}
	}
	v := struct {
		M map[string][4]string `json:"m"`
	}{M: m}

	got := testing.AllocsPerRun(20, func() { stripNUL(&v) })
	// Two per entry for MapIter and nothing else. A copy per entry adds one
	// allocation each, so the slack is a fifth of the smallest regression.
	const bound = 2*entries + entries/5
	t.Logf("%d clean arrays: %.0f allocs", entries, got)
	if got > bound {
		t.Errorf("a clean map of %d arrays allocates %.0f times, over %d: a copy "+
			"is being taken for arrays with nothing to strip", entries, got, bound)
	}
}

type embA struct {
	A string `json:"a"`
}

type embB struct {
	B string `json:"b"`
}

type embC struct {
	C string `json:"c"`
}

type twoEmbedded struct {
	embA
	embB
}

type threeEmbedded struct {
	embA
	embB
	embC
}

// containsNUL AND stripValue MUST AGREE, OR AN ARRAY IS NEVER COPIED.
//
// The lazy copy asks containsNUL first, so a shape where containsNUL answers
// false and stripValue would have changed something is a NUL that survives —
// silently, with no error and no test failing anywhere else. The two walks
// mirror each other by hand, which is exactly the arrangement that drifts.
//
// This drives both over the same values and requires them to agree. It is a
// differential test rather than a list of shapes, so a shape nobody thought of
// is still covered as long as it is in the generator.
func TestContainsNULAgreesWithTheWalk(t *testing.T) {
	esc := "\\u" + "0000"

	type leaf struct {
		S string            `json:"s"`
		M map[string]string `json:"m"`
		P *string           `json:"p"`
	}
	type mid struct {
		L   leaf      `json:"l"`
		A   [2]leaf   `json:"a"`
		Sl  []leaf    `json:"sl"`
		Any any       `json:"any"`
		Str [2]string `json:"str"`
	}

	// Each body puts the NUL in exactly one place, so a disagreement names it.
	bodies := map[string]string{
		"a string":              `{"l":{"s":"a` + esc + `b"}}`,
		"a map value":           `{"l":{"m":{"k":"a` + esc + `b"}}}`,
		"a map key":             `{"l":{"m":{"a` + esc + `b":"v"}}}`,
		"behind a pointer":      `{"l":{"p":"a` + esc + `b"}}`,
		"in an array of struct": `{"a":[{"s":"a` + esc + `b"},{"s":"ok"}]}`,
		"in a slice of struct":  `{"sl":[{"s":"a` + esc + `b"}]}`,
		"in an any":             `{"any":{"k":"a` + esc + `b"}}`,
		"in a string array":     `{"str":["a` + esc + `b","ok"]}`,
		"deep in an any":        `{"any":[{"k":["a` + esc + `b"]}]}`,
		// NOT IN ELEMENT ZERO. Every other array case here and elsewhere puts
		// the NUL first, so scanning only element 0 left the whole suite green
		// while `["clean","a\u0000b"]` kept its NUL — the same shape as the bug
		// the array rewrite was about, reintroduced by the lazy scan.
		"in a later array element":  `{"str":["clean","a` + esc + `b"]}`,
		"in a later struct element": `{"a":[{"s":"ok"},{"s":"a` + esc + `b"}]}`,
		"nothing at all":            `{"l":{"s":"clean"},"str":["p","q"]}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var probe mid
			if err := json.Unmarshal([]byte(body), &probe); err != nil {
				t.Fatal(err)
			}
			saw := containsNUL(reflect.ValueOf(&probe), 0)

			// DEEP EQUALITY, not %+v. Formatting a struct holding a *string
			// prints the ADDRESS, so a pointee that was stripped compares equal
			// to one that was not — the first version of this reported a
			// disagreement that was its own blindness, not the code's.
			var walked, untouched mid
			if err := json.Unmarshal([]byte(body), &walked); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(body), &untouched); err != nil {
				t.Fatal(err)
			}
			stripNUL(&walked)
			changed := !reflect.DeepEqual(walked, untouched)

			if saw != changed {
				t.Errorf("containsNUL=%v but the walk changed=%v — the two disagree, "+
					"so a lazily copied array is never copied and the NUL survives\n"+
					"body: %s", saw, changed, body)
			}
		})
	}
}

// A POINTER-SHAPED ARRAY MUST NOT BE COPIED WITH reflect.Copy.
//
// An array that occupies exactly one pointer word — [1]map, [1]*T,
// [1]struct{map} — is stored DIRECTLY in the reflect.Value when it comes from a
// non-addressable position. reflect.Copy assumes an array source is indirect and
// copies the first word of the pointed-to object instead of the pointer, so the
// element comes out as a garbage address.
//
// The two failure modes are not equally survivable. A [1]map dereferences nil,
// which RecoveryMiddleware turns into a 500. A [1]*T builds an address from
// adjacent bytes and dies in runtime.throw — unrecoverable, taking every
// in-flight request with it. Nothing routes to these shapes today; a fatal
// process kill earns a test anyway.
func TestAPointerShapedArrayInAMapSurvivesTheCopy(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("an array of maps", func(t *testing.T) {
		var v struct {
			M map[string][1]map[string]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[{"x":"a`+esc+`b"}]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if got := v.M["k"][0]["x"]; got != "ab" {
			t.Errorf("m.k[0].x = %q, want %q", got, "ab")
		}
	})

	t.Run("an array of pointers", func(t *testing.T) {
		type inner struct {
			S string `json:"s"`
		}
		var v struct {
			M map[string][1]*inner `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[{"s":"a`+esc+`b"}]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		p := v.M["k"][0]
		if p == nil || p.S != "ab" {
			t.Errorf("m.k[0] = %+v, want s=ab", p)
		}
	})

	t.Run("an array of structs holding a map", func(t *testing.T) {
		type inner struct {
			M map[string]string `json:"m"`
		}
		var v struct {
			M map[string][1]inner `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[{"m":{"x":"a`+esc+`b"}}]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if got := v.M["k"][0].M["x"]; got != "ab" {
			t.Errorf("m.k[0].m.x = %q, want %q", got, "ab")
		}
	})

	t.Run("a pointer-shaped array inside an interface", func(t *testing.T) {
		var v struct {
			A any `json:"a"`
		}
		// Built in Go: encoding/json would decode this as []any.
		v.A = [1]map[string]string{{"x": "a\x00b"}}
		stripNUL(&v)
		arr, ok := v.A.([1]map[string]string)
		if !ok {
			t.Fatalf("the interface no longer holds the array: %T", v.A)
		}
		if got := arr[0]["x"]; got != "ab" {
			t.Errorf("a[0].x = %q, want %q", got, "ab")
		}
	})

	t.Run("nested pointer-shaped arrays", func(t *testing.T) {
		var v struct {
			M map[string][1][1]map[string]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[[{"x":"a`+esc+`b"}]]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if got := v.M["k"][0][0]["x"]; got != "ab" {
			t.Errorf("m.k[0][0].x = %q, want %q", got, "ab")
		}
	})
}

// THE ADDRESSABILITY GATE IS LOAD-BEARING FOR CORRECTNESS, NOT COST.
//
// containsNUL keeps a clean array from being copied whether it is addressable or
// not, so removing the gate changes no allocation — and the whole suite stayed
// green without it. What it actually stops is an ADDRESSABLE array taking the
// rebuild path: the Pointer arm discards its recursion's return, so a rebuild
// made under a `*[2]string` is thrown away and the NUL survives.
func TestAnAddressableArrayIsNotRebuilt(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("behind a pointer in a struct", func(t *testing.T) {
		var v struct {
			P *[2]string `json:"p"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"p":["p`+esc+`q","r`+esc+`s"]}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.P == nil || v.P[0] != "pq" || v.P[1] != "rs" {
			t.Errorf("p = %q: the rebuild was discarded by the Pointer arm", v.P)
		}
	})

	t.Run("a pointer to an array in a map", func(t *testing.T) {
		var v struct {
			M map[string]*[2]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":["p`+esc+`q","r"]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		got := v.M["k"]
		if got == nil || got[0] != "pq" || got[1] != "r" {
			t.Errorf("m.k = %q", got)
		}
	})

	t.Run("a top-level array", func(t *testing.T) {
		var v [2]string
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`["p`+esc+`q","r`+esc+`s"]`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v[0] != "pq" || v[1] != "rs" {
			t.Errorf("v = %q: the rebuild was discarded by the Pointer arm", v)
		}
	})
}

// EVERY ARRAY SHAPE THAT REACHES THE COPY, not only the pointer-shaped ones.
//
// `out.Set(v)` replaced `reflect.Copy(out, v)` because Copy assumes an array
// source is indirect. Set is correct for the direct case; this checks it is
// correct for the rest too, since the bug it replaced was invisible on exactly
// the shapes that happened to be tested.
//
// NONE OF THESE FAILS UNDER reflect.Copy — measured — because none is one
// pointer word wide. That is the point: they are here to show Set did not break
// the ordinary shapes, and TestAPointerShapedArrayInAMapSurvivesTheCopy is what
// pins the bug. Two tests, two jobs; neither substitutes for the other.
func TestTheArrayCopyIsCorrectForEveryShape(t *testing.T) {
	esc := "\\u" + "0000"

	t.Run("a large array", func(t *testing.T) {
		var v struct {
			M map[string][64]string `json:"m"`
		}
		elems := make([]string, 64)
		for i := range elems {
			elems[i] = `"e"`
		}
		elems[0] = `"a` + esc + `b"`
		elems[63] = `"c` + esc + `d"`
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[`+strings.Join(elems, ",")+`]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		got := v.M["k"]
		if got[0] != "ab" || got[63] != "cd" || got[1] != "e" {
			t.Errorf("first=%q last=%q middle=%q", got[0], got[63], got[1])
		}
	})

	t.Run("an array of arrays", func(t *testing.T) {
		var v struct {
			M map[string][2][2]string `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[["a`+esc+`b","c"],["d","e`+esc+`f"]]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		if v.M["k"] != [2][2]string{{"ab", "c"}, {"d", "ef"}} {
			t.Errorf("m.k = %q", v.M["k"])
		}
	})

	t.Run("a struct with a pointer in the middle", func(t *testing.T) {
		type mid struct {
			A string  `json:"a"`
			P *string `json:"p"`
			B string  `json:"b"`
		}
		var v struct {
			M map[string][1]mid `json:"m"`
		}
		req := httptest.NewRequest("POST", "/", strings.NewReader(
			`{"m":{"k":[{"a":"a`+esc+`b","p":"p`+esc+`q","b":"c`+esc+`d"}]}}`))
		if err := DecodeJSON(req, &v); err != nil {
			t.Fatal(err)
		}
		got := v.M["k"][0]
		if got.A != "ab" || got.B != "cd" || got.P == nil || *got.P != "pq" {
			t.Errorf("got %+v with p=%v", got, got.P)
		}
	})

	// The empty array is covered by
	// TestTheArrayCopyGateHandlesItsEdges/an_empty_array_in_a_map — same
	// struct, same body, same assertion — so it is not repeated here.
}

// A DIRTY ELEMENT THAT IS NOT THE FIRST.
//
// The lazy copy scans elements until one needs work. Every other array case in
// this file puts the NUL in element ZERO, so narrowing that scan to element 0
// alone left the whole suite green while a later element kept its NUL — the same
// shape as the bug the array rewrite was about, reintroduced by the scan that
// replaced it.
//
// It has to be inside a MAP: an addressable array never reaches the copy.
func TestADirtyElementThatIsNotTheFirstIsFound(t *testing.T) {
	esc := "\\u" + "0000"

	for _, c := range []struct {
		name string
		body string
		want [3]string
	}{
		{"the last element", `{"m":{"k":["clean","also clean","a` + esc + `b"]}}`,
			[3]string{"clean", "also clean", "ab"}},
		{"the middle element", `{"m":{"k":["clean","a` + esc + `b","also clean"]}}`,
			[3]string{"clean", "ab", "also clean"}},
		{"the first, still", `{"m":{"k":["a` + esc + `b","clean","also clean"]}}`,
			[3]string{"ab", "clean", "also clean"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var v struct {
				M map[string][3]string `json:"m"`
			}
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			if err := DecodeJSON(req, &v); err != nil {
				t.Fatal(err)
			}
			if v.M["k"] != c.want {
				t.Errorf("m.k = %q, want %q: the scan does not reach every element",
					v.M["k"], c.want)
			}
		})
	}
}

// containsNUL MUST APPLY THE SAME EMBEDDED RULE AS THE WALK.
//
// The walk descends into an embedded unexported struct because encoding/json
// fills its promoted fields. If containsNUL does not, an array whose ONLY NUL is
// in such a field is never copied and keeps it. The existing embedded tests put
// a NUL in a plain field too, so containsNUL fired through that one regardless
// of what it did about the embedded struct.
func TestContainsNULAppliesTheEmbeddedRule(t *testing.T) {
	esc := "\\u" + "0000"
	var v struct {
		M map[string][1]outerWithEmbedded `json:"m"`
	}
	// The NUL is ONLY in the promoted field; body is clean.
	req := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"m":{"k":[{"title":"a`+esc+`b","body":"clean"}]}}`))
	if err := DecodeJSON(req, &v); err != nil {
		t.Fatal(err)
	}
	got := v.M["k"][0]
	if got.Title != "ab" {
		t.Errorf("title = %q, want %q: containsNUL did not look inside the "+
			"embedded unexported struct, so the array was never copied", got.Title, "ab")
	}
	if got.Body != "clean" {
		t.Errorf("body = %q, want clean", got.Body)
	}
}

// THE TWO DECODERS DIFFER IN EXACTLY ONE WAY.
//
// DecodeJSONVerbatim exists so a route whose strings are KEYS can refuse a NUL
// instead of having one silently removed. Sharing an implementation with
// DecodeJSONLimit is what keeps the size cap, the unknown-field rejection and
// the error shapes identical between them — but it also means one misplaced
// condition makes the pair behave the same, and a `strip` flag that is never
// read fails silently: the verbatim route goes back to being guessed at, and
// nothing in the funnel's own tests notices.
func TestTheVerbatimDecoderDiffersOnlyInStripping(t *testing.T) {
	esc := "\\u" + "0000"
	body := `{"name":"a` + esc + `b","n":7}`

	type payload struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}

	var stripped payload
	if err := DecodeJSONLimit(
		httptest.NewRequest("POST", "/", strings.NewReader(body)), &stripped, 1<<20); err != nil {
		t.Fatal(err)
	}
	if stripped.Name != "ab" {
		t.Errorf("DecodeJSONLimit left %q, want %q", stripped.Name, "ab")
	}

	var verbatim payload
	if err := DecodeJSONVerbatim(
		httptest.NewRequest("POST", "/", strings.NewReader(body)), &verbatim, 1<<20); err != nil {
		t.Fatal(err)
	}
	if verbatim.Name != "a\x00b" {
		t.Errorf("DecodeJSONVerbatim returned %q, want the NUL kept: a route that "+
			"opts out of the funnel is now being guessed at instead of refusing",
			verbatim.Name)
	}

	// Everything else has to match, or "verbatim" quietly means "and also
	// without the body cap" or "and also accepting unknown fields".
	if verbatim.N != stripped.N {
		t.Errorf("N = %d and %d: the decoders disagree beyond stripping",
			verbatim.N, stripped.N)
	}
	for _, c := range []struct {
		name, body string
		limit      int64
		wantStatus int
	}{
		{"an unknown field", `{"name":"a","nope":1}`, 1 << 20, http.StatusBadRequest},
		{"malformed JSON", `{"name":`, 1 << 20, http.StatusBadRequest},
		{"a body over the cap", `{"name":"aaaaaaaaaaaaaaaa"}`, 4, http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a, b payload
			errLimit := DecodeJSONLimit(
				httptest.NewRequest("POST", "/", strings.NewReader(c.body)), &a, c.limit)
			errVerbatim := DecodeJSONVerbatim(
				httptest.NewRequest("POST", "/", strings.NewReader(c.body)), &b, c.limit)

			var ae, be *AppError
			if !errors.As(errLimit, &ae) || !errors.As(errVerbatim, &be) {
				t.Fatalf("errors = %v and %v, want *AppError from both", errLimit, errVerbatim)
			}
			if ae.Status != c.wantStatus || be.Status != c.wantStatus {
				t.Errorf("status = %d and %d, want %d from both",
					ae.Status, be.Status, c.wantStatus)
			}
			if ae.Code != be.Code {
				t.Errorf("code = %q and %q: the decoders disagree beyond stripping",
					ae.Code, be.Code)
			}
		})
	}
}
