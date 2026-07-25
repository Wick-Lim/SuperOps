package ws

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

const testChannelID = "6f1e3d2c-8b7a-4c5d-9e0f-1a2b3c4d5e6f"

// drain returns every frame currently queued for the client.
func drain(t *testing.T, c *Client) []OutboundMessage {
	t.Helper()
	var out []OutboundMessage
	for {
		select {
		case b := <-c.send:
			var m OutboundMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal frame: %v", err)
			}
			out = append(out, m)
		default:
			return out
		}
	}
}

func inbound(t *testing.T, msgType string, data interface{}) InboundMessage {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal inbound data: %v", err)
	}
	return InboundMessage{Type: msgType, Data: b}
}

func errCode(m OutboundMessage) string {
	var payload struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(m.Data, &payload)
	return payload.Code
}

func TestHandleMessage(t *testing.T) {
	tests := []struct {
		name         string
		msgType      string
		data         interface{}
		member       bool
		memberErr    error
		subscribed   bool
		wantTypes    []string
		wantErrCode  string
		wantChecked  bool
		wantSubbed   bool
		wantUnsubbed bool
	}{
		{
			name:        "subscribe by a member is acknowledged",
			msgType:     TypeSubscribe,
			data:        SubscribeData{ChannelID: testChannelID},
			member:      true,
			wantTypes:   []string{TypeSubscribed},
			wantChecked: true,
			wantSubbed:  true,
		},
		{
			name:        "subscribe by a non-member is refused",
			msgType:     TypeSubscribe,
			data:        SubscribeData{ChannelID: testChannelID},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
			wantChecked: true,
		},
		{
			name:        "subscribe surfaces a lookup failure as an internal error",
			msgType:     TypeSubscribe,
			data:        SubscribeData{ChannelID: testChannelID},
			member:      true,
			memberErr:   errors.New("boom"),
			wantTypes:   []string{TypeError},
			wantErrCode: "INTERNAL_ERROR",
			wantChecked: true,
		},
		{
			name:        "subscribe rejects a non-uuid channel id before any lookup",
			msgType:     TypeSubscribe,
			data:        SubscribeData{ChannelID: "../../etc/passwd"},
			member:      true,
			wantTypes:   []string{TypeError},
			wantErrCode: "BAD_REQUEST",
			wantChecked: false,
		},
		{
			name:        "typing.start is membership gated",
			msgType:     TypeTypingStart,
			data:        TypingData{ChannelID: testChannelID},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
			wantChecked: true,
		},
		{
			name:        "typing.start rejects a non-uuid channel id before it reaches a subject",
			msgType:     TypeTypingStart,
			data:        TypingData{ChannelID: "chan.*"},
			member:      true,
			wantTypes:   []string{TypeError},
			wantErrCode: "BAD_REQUEST",
			wantChecked: false,
		},
		{
			name:        "typing.start by a member broadcasts nothing back to itself",
			msgType:     TypeTypingStart,
			data:        TypingData{ChannelID: testChannelID},
			member:      true,
			wantTypes:   nil,
			wantChecked: true,
		},
		{
			name:        "typing.start skips the query when already subscribed",
			msgType:     TypeTypingStart,
			data:        TypingData{ChannelID: testChannelID},
			subscribed:  true,
			wantTypes:   nil,
			wantChecked: false,
		},
		{
			name:        "typing.stop is handled, not rejected as unknown",
			msgType:     TypeTypingStop,
			data:        TypingData{ChannelID: testChannelID},
			member:      true,
			wantTypes:   nil,
			wantChecked: true,
		},
		{
			name:      "presence.update accepts an enum value",
			msgType:   TypePresence,
			data:      PresenceData{Status: "away"},
			wantTypes: nil,
		},
		{
			name:        "presence.update rejects an arbitrary string",
			msgType:     TypePresence,
			data:        PresenceData{Status: "<script>alert(1)</script>"},
			wantTypes:   []string{TypeError},
			wantErrCode: "BAD_REQUEST",
		},
		{
			name:        "unknown types still error",
			msgType:     "totally.made.up",
			data:        map[string]string{},
			wantTypes:   []string{TypeError},
			wantErrCode: "UNKNOWN_TYPE",
		},
		{
			name:         "unsubscribe is acknowledged",
			msgType:      TypeUnsubscribe,
			data:         SubscribeData{ChannelID: testChannelID},
			subscribed:   true,
			wantTypes:    []string{TypeUnsubscribed},
			wantUnsubbed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(testLogger())
			checked := false
			c := NewClient(context.Background(), hub, nil, "u1", []string{"ws-1"}, nil,
				func(context.Context, string, string) (bool, error) {
					checked = true
					return tt.member, tt.memberErr
				}, nil, testLogger())
			if tt.subscribed {
				c.Subscribe(testChannelID)
			}

			c.handleMessage(context.Background(), inbound(t, tt.msgType, tt.data))

			frames := drain(t, c)
			if len(frames) != len(tt.wantTypes) {
				t.Fatalf("got %d frames %v, want %v", len(frames), frames, tt.wantTypes)
			}
			for i, want := range tt.wantTypes {
				if frames[i].Type != want {
					t.Errorf("frame %d type = %q, want %q", i, frames[i].Type, want)
				}
			}
			if tt.wantErrCode != "" && errCode(frames[0]) != tt.wantErrCode {
				t.Errorf("error code = %q, want %q", errCode(frames[0]), tt.wantErrCode)
			}
			if checked != tt.wantChecked {
				t.Errorf("membership checked = %v, want %v", checked, tt.wantChecked)
			}
			if tt.wantSubbed && !c.IsSubscribed(testChannelID) {
				t.Error("client is not subscribed after a successful subscribe")
			}
			if !tt.wantSubbed && !tt.subscribed && c.IsSubscribed(testChannelID) {
				t.Error("client subscribed despite a refused subscribe")
			}
			if tt.wantUnsubbed && c.IsSubscribed(testChannelID) {
				t.Error("client still subscribed after unsubscribe")
			}
		})
	}
}

// TestMissingMemberCheckerFailsClosed: an unwired authorization dependency must
// not mean "everyone is a member".
func TestMissingMemberCheckerFailsClosed(t *testing.T) {
	hub := NewHub(testLogger())
	c := NewClient(context.Background(), hub, nil, "u1", nil, nil, nil, nil, testLogger())

	c.handleMessage(context.Background(), inbound(t, TypeSubscribe, SubscribeData{ChannelID: testChannelID}))

	frames := drain(t, c)
	if len(frames) != 1 || frames[0].Type != TypeError || errCode(frames[0]) != "FORBIDDEN" {
		t.Fatalf("frames = %v, want a single FORBIDDEN error", frames)
	}
	if c.IsSubscribed(testChannelID) {
		t.Fatal("subscribed with no membership checker wired")
	}
}
