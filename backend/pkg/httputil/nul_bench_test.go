package httputil

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A BINARY BODY MUST NOT BE REFUSED, AND MUST NOT BE WALKED BYTE BY BYTE.
//
// internal/collab decodes `Update []byte` through this funnel — a Yjs CRDT
// update, which is binary and routinely contains NUL bytes. Two things have to
// hold: the guard must not refuse it (a []byte element is a uint8, not a
// string), and it must not cost a reflect call per byte on a megabyte payload.
func TestABinaryFieldIsNeitherRefusedNorWalkedPerByte(t *testing.T) {
	var input struct {
		Update []byte `json:"update"`
	}
	// Deliberately full of NULs: this is what a real CRDT update looks like.
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
	if !bytes.Equal(input.Update, raw) {
		t.Fatal("the body did not round-trip")
	}

	// AND IT IS SKIPPED, not walked. A benchmark states the cost; this fails if
	// the skip is removed, which a benchmark nobody runs would not.
	start := time.Now()
	for i := 0; i < 50; i++ {
		if err := rejectNUL(&input); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / 50
	// Skipped is ~70ns; walked was 16.8ms for this size. Three orders of
	// magnitude of headroom, so this cannot fail on a slow machine.
	if per > 100*time.Microsecond {
		t.Errorf("rejectNUL takes %s on a 256 KiB binary field: the byte-slice "+
			"skip is gone, so every collab update pays a reflect call per byte", per)
	}
}

// The cost of the walk against the decode that produced the value. If the guard
// is a meaningful fraction of the decode it is in the wrong place.
func BenchmarkDecodeWithAndWithoutNULWalk(b *testing.B) {
	type nested struct {
		Body string            `json:"body"`
		Meta map[string]string `json:"meta"`
	}
	type payload struct {
		Name  string   `json:"name"`
		Tags  []string `json:"tags"`
		Items []nested `json:"items"`
	}

	p := payload{Name: strings.Repeat("n", 200)}
	for i := 0; i < 200; i++ {
		p.Tags = append(p.Tags, fmt.Sprintf("tag-%d", i))
		p.Items = append(p.Items, nested{
			Body: strings.Repeat("x", 400),
			Meta: map[string]string{"a": "1", "b": "2", "c": "3"},
		})
	}
	body, _ := json.Marshal(p)
	b.Logf("body is %d bytes", len(body))

	b.Run("decode only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var v payload
			if err := json.Unmarshal(body, &v); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("the walk alone", func(b *testing.B) {
		var v payload
		_ = json.Unmarshal(body, &v)
		for i := 0; i < b.N; i++ {
			if err := rejectNUL(&v); err != nil {
				b.Fatal(err)
			}
		}
	})

	// A TYPICAL body, which is what the ratio should actually be judged on.
	b.Run("a typical 400-byte body", func(b *testing.B) {
		small := payload{Name: "a channel message", Tags: []string{"x"},
			Items: []nested{{Body: strings.Repeat("m", 300), Meta: map[string]string{"k": "v"}}}}
		sb, _ := json.Marshal(small)
		b.Logf("typical body is %d bytes", len(sb))
		var v payload
		_ = json.Unmarshal(sb, &v)
		b.Run("decode", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var x payload
				_ = json.Unmarshal(sb, &x)
			}
		})
		b.Run("walk", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = rejectNUL(&v)
			}
		})
	})

	// And the case that worried me: a large binary field.
	b.Run("a 256 KiB binary field", func(b *testing.B) {
		var v struct {
			Update []byte `json:"update"`
		}
		v.Update = make([]byte, 256<<10)
		for i := 0; i < b.N; i++ {
			if err := rejectNUL(&v); err != nil {
				b.Fatal(err)
			}
		}
	})
}
