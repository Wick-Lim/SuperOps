package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
)

// The ack decision hinges on this: a permanent error must survive the wrapping
// each handler does on its way out, or a poison message gets naked five times
// instead of being terminated once.
func TestIsPermanent(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("meilisearch unreachable"), false},
		{"search permanent", &search.PermanentError{Reason: "malformed payload"}, true},
		{"notification permanent", &notification.PermanentError{Reason: "no workspace id"}, true},
		// The mail consumer's verdicts have to be legible here too, or a 550 from
		// a relay would be naked five times instead of terminated once.
		{"mail permanent", &mail.PermanentError{Reason: "550 user unknown"}, true},
		{
			"mail transient",
			fmt.Errorf("smtp: dial relay: %w", errors.New("connection refused")),
			false,
		},
		{
			"wrapped permanent",
			fmt.Errorf("index message %s: %w", "abc", &search.PermanentError{Reason: "rejected"}),
			true,
		},
		{
			"wrapped transient",
			fmt.Errorf("index message: %w", errors.New("connection refused")),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanent(tt.err); got != tt.want {
				t.Fatalf("isPermanent(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNakDelayFollowsTheConsumerBackoff(t *testing.T) {
	if len(consumerBackOff) > consumerMaxDeliver {
		t.Fatal("NATS rejects a consumer whose BackOff is longer than MaxDeliver")
	}
	for delivery, want := range map[int]time.Duration{
		0: consumerBackOff[0], // defensive: metadata unavailable
		1: consumerBackOff[0],
		2: consumerBackOff[1],
		4: consumerBackOff[3],
		9: consumerBackOff[len(consumerBackOff)-1], // clamped
	} {
		if got := nakDelay(delivery); got != want {
			t.Errorf("nakDelay(%d) = %s, want %s", delivery, got, want)
		}
	}
}

func TestTermReasonIsOneBoundedLine(t *testing.T) {
	got := termReason("malformed payload:\n\tunexpected end of JSON input")
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("term reason must be a single line, got %q", got)
	}
	if got := termReason(strings.Repeat("x", 500)); len(got) > termReasonMax+3 {
		t.Errorf("term reason not bounded: %d chars", len(got))
	}
}

func TestHandlerTimeoutLeavesRoomBeforeRedelivery(t *testing.T) {
	if handlerTimeout >= consumerAckWait {
		t.Fatalf("handlerTimeout %s must expire before AckWait %s, or the server hands out a second copy "+
			"of a message that is still being worked on", handlerTimeout, consumerAckWait)
	}
}

func TestObjectDay(t *testing.T) {
	tests := []struct {
		key  string
		want string // empty = unparseable
	}{
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/abc.png", "2026-07-25"},
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/nested/abc.png", "2026-07-25"},
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/abc.png", ""}, // too few segments
		{"ws/2026/13/40/abc.png", ""},                        // not a date
		{"", ""},
	}
	for _, tt := range tests {
		day, ok := objectDay(tt.key)
		if ok != (tt.want != "") {
			t.Errorf("objectDay(%q) ok = %v, want %v", tt.key, ok, tt.want != "")
			continue
		}
		if ok && day.Format("2006-01-02") != tt.want {
			t.Errorf("objectDay(%q) = %s, want %s", tt.key, day.Format("2006-01-02"), tt.want)
		}
	}
}

// The bucket sweep deletes objects whose files row is gone. An object that was
// PUT seconds ago and whose INSERT has not committed yet looks exactly like
// one, so nothing may be swept until the grace period has certainly elapsed —
// and a storage key only carries a date, not a time.
func TestObjectSweepHonoursTheGracePeriod(t *testing.T) {
	const key = "7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/abc.png"

	// Worst case for the old start-of-day comparison: uploaded at 23:59:59 on
	// the 25th, swept at 00:00:01 on the 26th, two seconds old.
	freshUpload := time.Date(2026, 7, 26, 0, 0, 1, 0, time.UTC)
	if olderThan(key, freshUpload.Add(-objectGCGrace)) {
		t.Error("an object that may be seconds old was eligible for deletion")
	}

	// Even a full day later it is still inside the slack that covers the date's
	// granularity and the deployment's timezone.
	if olderThan(key, time.Date(2026, 7, 27, 0, 0, 1, 0, time.UTC).Add(-objectGCGrace)) {
		t.Error("swept before the grace period could have elapsed in every timezone")
	}

	// But it must not become immortal: a few days on, it is collectable.
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !olderThan(key, old.Add(-objectGCGrace)) {
		t.Error("an object a week old was never collected")
	}

	// Anything whose age cannot be established from the key is left alone.
	if olderThan("no-date-here.png", old) {
		t.Error("an unparseable key must never be swept")
	}
}

// Whatever the slack is, it has to be at least a day (the key's granularity) on
// top of the grace period, or TestObjectSweepHonoursTheGracePeriod passes by
// coincidence.
func TestObjectKeyDateSlackCoversDateGranularity(t *testing.T) {
	if objectKeyDateSlack < 24*time.Hour {
		t.Fatalf("objectKeyDateSlack %s is shorter than the one-day granularity of a storage key",
			objectKeyDateSlack)
	}
}

func TestSplitN(t *testing.T) {
	got := splitN("a/b/c/d/e/f", '/', 5)
	want := []string{"a", "b", "c", "d", "e/f"}
	if len(got) != len(want) {
		t.Fatalf("splitN = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitN = %v, want %v", got, want)
		}
	}
	if got := splitN("abc", '/', 5); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("splitN = %v, want [abc]", got)
	}
}

func TestUnionDeduplicatesAndKeepsOrder(t *testing.T) {
	got := union([]string{"a", "b", "a"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("union = %v, want %v", got, want)
		}
	}
}
