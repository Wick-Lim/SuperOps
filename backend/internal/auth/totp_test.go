package auth

import (
	"math"
	"strings"
	"testing"
	"time"
)

// RFC 4226 Appendix D test vectors (secret = ASCII "12345678901234567890").
func TestHOTPVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	want := []string{
		"755224", "287082", "359152", "969429", "338314",
		"254676", "287922", "162583", "399871", "520489",
	}
	for i, w := range want {
		if got := hotp(key, uint64(i)); got != w {
			t.Errorf("counter %d: got %s want %s", i, got, w)
		}
	}
}

func TestValidateTOTPRoundTrip(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	code, err := totpAt(secret, time.Now())
	if err != nil {
		t.Fatalf("totpAt: %v", err)
	}
	if !ValidateTOTP(secret, code) {
		t.Fatalf("expected current code %q to validate", code)
	}
	// A code from one step ago is still accepted (skew window).
	prev, _ := totpAt(secret, time.Now().Add(-30*time.Second))
	if !ValidateTOTP(secret, prev) {
		t.Errorf("expected previous-step code %q to validate within skew", prev)
	}
	// A code from 5 minutes ago must be rejected.
	old, _ := totpAt(secret, time.Now().Add(-5*time.Minute))
	if old != code && ValidateTOTP(secret, old) {
		t.Errorf("stale code %q should not validate", old)
	}
	if ValidateTOTP(secret, "12345") || ValidateTOTP(secret, "abcdef") {
		t.Errorf("malformed codes should not validate")
	}
}

func TestMatchTOTPStep(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	now := time.Unix(1_700_000_000, 0) // fixed so the step arithmetic is exact
	current := now.Unix() / totpPeriodSecs

	codeAt := func(step int64) string {
		c, err := totpAtStep(secret, step)
		if err != nil {
			t.Fatalf("totpAtStep: %v", err)
		}
		return c
	}

	tests := []struct {
		name     string
		code     string
		wantStep int64
		wantOK   bool
	}{
		{"current step", codeAt(current), current, true},
		{"one step behind", codeAt(current - 1), current - 1, true},
		{"one step ahead", codeAt(current + 1), current + 1, true},
		{"two steps behind is outside the window", codeAt(current - 2), 0, false},
		{"two steps ahead is outside the window", codeAt(current + 2), 0, false},
		{"too short", "12345", 0, false},
		{"not digits", "abcdef", 0, false},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step, ok := matchTOTPStep(secret, tt.code, now)
			if ok != tt.wantOK || step != tt.wantStep {
				t.Errorf("matchTOTPStep(%q) = (%d, %v), want (%d, %v)", tt.code, step, ok, tt.wantStep, tt.wantOK)
			}
		})
	}
}

// A matched code is spent by advancing users.totp_last_step, and acceptance is
// guarded by "... WHERE totp_last_step < $step". This exercises that rule:
// once a step has been consumed, neither it nor any earlier step inside the
// ±1 skew window may authenticate again.
func TestTOTPReplayRejected(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	current := now.Unix() / totpPeriodSecs

	codeAt := func(step int64) string {
		c, _ := totpAtStep(secret, step)
		return c
	}

	// Mirrors the guarded UPDATE: returns the new watermark and whether the
	// code authenticated.
	var lastStep int64
	consume := func(code string) bool {
		step, ok := matchTOTPStep(secret, code, now)
		if !ok || step <= lastStep {
			return false
		}
		lastStep = step
		return true
	}

	tests := []struct {
		name string
		code string
		want bool
	}{
		{"first use of the current code", codeAt(current), true},
		{"immediate replay of the same code", codeAt(current), false},
		{"replay again", codeAt(current), false},
		{"an older in-window code cannot be used after it", codeAt(current - 1), false},
		{"the next step is still accepted", codeAt(current + 1), true},
		{"replay of the next step", codeAt(current + 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := consume(tt.code); got != tt.want {
				t.Errorf("consume(%q) = %v, want %v (watermark %d)", tt.code, got, tt.want, lastStep)
			}
		})
	}
}

func TestGenerateBackupCode(t *testing.T) {
	const samples = 500

	// >= 40 bits of entropy per code is the floor; the alphabet/length pair
	// must keep clearing it.
	bits := float64(backupCodeLen) * math.Log2(float64(len(backupCodeAlphabet)))
	if bits < 40 {
		t.Fatalf("backup codes carry only %.1f bits, want >= 40", bits)
	}

	seen := make(map[string]bool, samples)
	freq := make(map[rune]int)
	for i := 0; i < samples; i++ {
		c, err := GenerateBackupCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(c) != backupCodeLen {
			t.Fatalf("code %q has length %d, want %d", c, len(c), backupCodeLen)
		}
		for _, r := range c {
			if !strings.ContainsRune(backupCodeAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside the alphabet", c, r)
			}
			freq[r]++
		}
		if seen[c] {
			t.Fatalf("duplicate code %q within %d samples", c, samples)
		}
		seen[c] = true
		if !looksLikeBackupCode(c) {
			t.Fatalf("generated code %q fails the shape test", c)
		}
	}

	// Every symbol should show up; a folded or truncated alphabet would leave
	// holes (this is what lowercasing base64url used to do).
	if len(freq) != len(backupCodeAlphabet) {
		t.Errorf("only %d of %d alphabet symbols appeared across %d codes",
			len(freq), len(backupCodeAlphabet), samples)
	}
	for _, r := range "ilo01" {
		if strings.ContainsRune(backupCodeAlphabet, r) {
			t.Errorf("ambiguous character %q must not be in the alphabet", r)
		}
	}
}

func TestLooksLikeBackupCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want bool
	}{
		{"current format", "abcdefghjk", true},
		{"legacy 8-char base64url", "a1b2-c3_", true},
		{"six digit totp code", "123456", false},
		{"empty", "", false},
		{"too long", "abcdefghjkm", false},
		{"uppercase", "ABCDEFGHJK", false},
		{"punctuation", "abcdefg!jk", false},
		{"whitespace", "abcdefg jk", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeBackupCode(tt.code); got != tt.want {
				t.Errorf("looksLikeBackupCode(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := TOTPProvisioningURI("ABCDEF", "alice@example.com", "SuperOps")
	if uri == "" || uri[:len("otpauth://totp/")] != "otpauth://totp/" {
		t.Fatalf("unexpected uri: %s", uri)
	}
}
