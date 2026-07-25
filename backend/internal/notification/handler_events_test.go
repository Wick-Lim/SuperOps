package notification

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

func testService() *Service {
	// Every case below must be decided from the payload or the subject alone,
	// before anything touches Postgres — hence the nil pool. A nil-pointer panic
	// here means the handler started doing work on an event it should have
	// rejected outright.
	return &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func event(subject, data string) *nats.Msg {
	return &nats.Msg{Subject: subject, Data: []byte(data)}
}

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

const subject = "superops.2f1d9c4e-0f9a-4a1e-8f66-9a8bd2c4e111.message.created"

// A handler that returns nil is telling the worker to ack. For an event it can
// never process that has to be an explicit permanent error instead, or the
// worker naks it four more times before dropping it anyway.
func TestEventHandlersRejectUnprocessableEvents(t *testing.T) {
	tests := []struct {
		name    string
		handler func(context.Context, *nats.Msg) error
		subject string
		data    string
	}{
		{"message: envelope is not json", testService().HandleMessage, subject, `{`},
		{"message: payload is not json", testService().HandleMessage, subject, `{"type":"message.new","data":5}`},
		{"message: no id, channel or user", testService().HandleMessage, subject, `{"type":"message.new","data":{}}`},
		{
			"message: subject has no workspace",
			testService().HandleMessage,
			"superops",
			`{"type":"message.new","data":{"channel_id":"c","user_id":"u"}}`,
		},
		{
			"message: subject has an empty workspace",
			testService().HandleMessage,
			"superops..message.created",
			`{"type":"message.new","data":{"channel_id":"c","user_id":"u"}}`,
		},
		{"reaction: envelope is not json", testService().HandleReaction, subject, `{`},
		{"reaction: payload is not json", testService().HandleReaction, subject, `{"type":"reaction.added","data":5}`},
		{"reaction: empty payload", testService().HandleReaction, subject, `{"type":"reaction.added","data":{}}`},
		{"invite: envelope is not json", testService().HandleChannelMemberAdded, subject, `{`},
		{
			"invite: payload is not json",
			testService().HandleChannelMemberAdded,
			subject,
			`{"type":"channel.member_added","data":5}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.handler(context.Background(), event(tt.subject, tt.data))
			if err == nil {
				t.Fatal("expected an error, got nil (the event would have been acked and lost)")
			}
			if !isPermanent(err) {
				t.Fatalf("error %v is retryable; a permanently bad event must be terminated", err)
			}
		})
	}
}

// Every consumer sees events it does not care about; those are a clean ack.
func TestEventHandlersIgnoreForeignEventTypes(t *testing.T) {
	body := `{"type":"message.updated","data":{"channel_id":"c","user_id":"u"}}`
	s := testService()
	for name, err := range map[string]error{
		"message":  s.HandleMessage(context.Background(), event(subject, body)),
		"reaction": s.HandleReaction(context.Background(), event(subject, body)),
		"invite":   s.HandleChannelMemberAdded(context.Background(), event(subject, body)),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Durable consumers are at-least-once, so the id has to be a function of the
// event: the second delivery must collide with the first row rather than add a
// duplicate to the recipient's list.
func TestNotificationIDIsStableAndDistinguishing(t *testing.T) {
	const (
		user  = "1c1b6e7a-2f3d-4c5b-8a9e-0d1f2a3b4c5d"
		other = "9e8d7c6b-5a4f-4e3d-2c1b-0a9f8e7d6c5b"
		msgID = "44444444-4444-4444-4444-444444444444"
	)

	base := notificationID(TypeMention, user, msgID)
	if base != notificationID(TypeMention, user, msgID) {
		t.Fatal("the same event must derive the same id on every delivery")
	}
	if _, err := uuid.Parse(base); err != nil {
		t.Fatalf("derived id is not a uuid: %v", err)
	}

	distinct := map[string]string{
		"other recipient": notificationID(TypeMention, other, msgID),
		"other type":      notificationID(TypeThreadReply, user, msgID),
		"other subject":   notificationID(TypeMention, user, "other"),
	}
	for name, id := range distinct {
		if id == base {
			t.Errorf("%s collided with the base id", name)
		}
	}

	// The separator must not let two different tuples render to the same string.
	if notificationID(TypeDM, "a", "b\x00c") == notificationID(TypeDM, "a\x00b", "c") {
		t.Error("the id key is ambiguous across field boundaries")
	}
}
