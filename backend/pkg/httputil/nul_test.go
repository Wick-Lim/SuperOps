package httputil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
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

	allocs := testing.AllocsPerRun(5, func() { stripNUL(&input) })
	if allocs > 50 {
		t.Errorf("stripping a 256 KiB binary field allocates %.0f times: the "+
			"byte-slice skip is gone, so every collab update pays per byte", allocs)
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
