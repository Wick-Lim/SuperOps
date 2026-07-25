package audit

import "testing"

// audit_logs.ip_address is INET; anything that is not a real address has to
// become NULL rather than blow up the insert (and take the audit record with it).
func TestNilIfNotIP(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
	}{
		{"ipv4", "203.0.113.7", false},
		{"ipv6", "2001:db8::1", false},
		{"loopback", "127.0.0.1", false},
		{"empty", "", true},
		{"host and port", "203.0.113.7:52344", true},
		{"hostname", "localhost", true},
		{"garbage", "not-an-ip", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nilIfNotIP(tt.in)
			if (got == nil) != tt.wantNil {
				t.Errorf("nilIfNotIP(%q) = %v, wantNil=%v", tt.in, got, tt.wantNil)
			}
			if !tt.wantNil && got != tt.in {
				t.Errorf("nilIfNotIP(%q) = %v, want the address back", tt.in, got)
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("empty string must become NULL")
	}
	if got := nilIfEmpty("abc"); got != "abc" {
		t.Errorf("nilIfEmpty(\"abc\") = %v", got)
	}
}
