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

// TestAdvisoryLockKeysAreDistinct: every singleton job takes a session-scoped
// advisory lock so exactly one replica runs it per tick. Two jobs sharing a key
// does not fail — it silently means one of them never runs on a multi-replica
// deployment, and the only symptom is work that quietly stops happening.
func TestAdvisoryLockKeysAreDistinct(t *testing.T) {
	keys := map[string]int64{
		jobSessionCleanup: lockSessionCleanup,
		jobRetention:      lockRetention,
		jobObjectGC:       lockObjectGC,
		jobACLDrift:       lockACLDrift,
	}
	seen := map[int64]string{}
	for name, key := range keys {
		if other, dup := seen[key]; dup {
			t.Errorf("jobs %q and %q share advisory lock key %#x; one of them will never run", name, other, key)
		}
		seen[key] = name
	}
}
