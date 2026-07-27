package httputil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
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
	// 1.08ms without, twenty thousand times.
	start := time.Now()
	const runs = 20
	for i := 0; i < runs; i++ {
		stripNUL(&input)
	}
	per := time.Since(start) / runs
	// Three orders of magnitude of headroom, so a slow machine cannot trip it
	// and the regression cannot hide under it.
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

	if stripped > clean+(1<<20) {
		t.Errorf("stripping at depth %d allocated %d B against a clean %d B: the "+
			"walk is quadratic in depth", deep, stripped, clean)
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
// rebuiltSequence replaces one element and must finish stripping the rest.
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
			t.Errorf("m.k = %q, want %q — rebuiltSequence stopped at the first "+
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
	t.Run("a clean body takes no rebuild", func(t *testing.T) {
		type item struct {
			A string `json:"a"`
			B string `json:"b"`
		}
		v := struct {
			M map[string]item `json:"m"`
		}{M: map[string]item{"k": {A: "plain", B: "also plain"}}}
		if n := testing.AllocsPerRun(20, func() { stripNUL(&v) }); n > 10 {
			t.Errorf("a clean body allocates %.0f times: a rebuild is being taken "+
				"when nothing changed", n)
		}
	})
}

// ExportedBase is embedded by the case above; a distinct type so the unexported
// case keeps testing what it names.
type ExportedBase struct {
	Title string `json:"title"`
}
