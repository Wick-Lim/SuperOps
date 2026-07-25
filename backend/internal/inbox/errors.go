package inbox

// PermanentError marks an event that redelivery can never fix: a payload that
// does not parse, a kind that fails the column's CHECK, a recipient list past
// MaxRecipients.
//
// cmd/worker discovers it structurally — it looks for an error implementing
// `Permanent() bool` rather than importing this package — and terminates the
// JetStream message with a reason instead of naking it back onto the stream
// until MaxDeliver runs out. Every domain package in this tree carries its own
// copy of this type for that reason.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent identifies this error to the consumer's ack policy. The method, not
// the concrete type, is the contract.
func (e *PermanentError) Permanent() bool { return true }
