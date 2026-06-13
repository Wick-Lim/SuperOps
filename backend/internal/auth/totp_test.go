package auth

import (
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

func TestProvisioningURI(t *testing.T) {
	uri := TOTPProvisioningURI("ABCDEF", "alice@example.com", "SuperOps")
	if uri == "" || uri[:len("otpauth://totp/")] != "otpauth://totp/" {
		t.Fatalf("unexpected uri: %s", uri)
	}
}
