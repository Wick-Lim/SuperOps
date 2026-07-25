package inbox

import "testing"

// The ladder is the whole preference model, and it is evaluated in exactly one
// function. A precedence table test is what stops a later edit from making
// '<resource>.*' win over an exact kind, which nobody would notice until a user
// swore they had turned mentions on.
func TestPrefResolutionPrecedence(t *testing.T) {
	const user = "u"
	set := PrefSet{byUser: map[string]map[string]Pref{
		user: {
			"message.mention": {InApp: true, Push: true, Email: EmailImmediate},
			"message.*":       {InApp: true, Push: false, Email: EmailDigest},
			"*":               {InApp: false, Push: false, Email: EmailNever},
		},
	}}

	tests := []struct {
		name string
		kind string
		want Pref
	}{
		{"exact kind wins over the resource wildcard", "message.mention",
			Pref{InApp: true, Push: true, Email: EmailImmediate}},
		{"resource wildcard wins over the global wildcard", "message.dm",
			Pref{InApp: true, Push: false, Email: EmailDigest}},
		{"global wildcard catches an unrelated resource", "issue.assigned",
			Pref{InApp: false, Push: false, Email: EmailNever}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := set.Resolve(user, tt.kind); got != tt.want {
				t.Errorf("Resolve(%q) = %+v, want %+v", tt.kind, got, tt.want)
			}
		})
	}

	// A user with no rows at all falls through to the built-in, and so does a
	// kind that matches no pattern when there is no '*'.
	if got := set.Resolve("nobody", "message.dm"); got != DefaultPref {
		t.Errorf("a user with no rows = %+v, want the built-in default", got)
	}
	partial := PrefSet{byUser: map[string]map[string]Pref{
		user: {"message.*": {InApp: true, Push: true, Email: EmailNever}},
	}}
	if got := partial.Resolve(user, "issue.assigned"); got != DefaultPref {
		t.Errorf("an unmatched kind = %+v, want the built-in default", got)
	}
}

// The zero PrefSet is what LoadPrefs returns for an empty recipient set, and the
// fan-out must not treat it as "everything is off".
func TestZeroPrefSetIsTheDefault(t *testing.T) {
	var set PrefSet
	if got := set.Resolve("u", "message.dm"); got != DefaultPref {
		t.Fatalf("zero PrefSet resolved to %+v, want the built-in default", got)
	}
}

// The Go validators mirror migration 020's CHECK constraints. They exist so a
// bad value is a 400 naming the field rather than a 500 carrying a constraint
// name, which means they have to agree with the regex exactly.
func TestKindValidation(t *testing.T) {
	valid := []string{"message.dm", "a.b", "issue.status_changed", "m9.v9"}
	for _, k := range valid {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false, want true", k)
		}
		if !ValidPrefKind(k) {
			t.Errorf("ValidPrefKind(%q) = false, want true", k)
		}
	}

	invalid := []string{"", "message", "Message.dm", "message.DM", ".dm", "message.",
		"message-dm", "9message.dm", "message.dm.extra", "aaaaaaaaaaaaaaaaaaaaaaaaa.b"}
	for _, k := range invalid {
		if ValidKind(k) {
			t.Errorf("ValidKind(%q) = true, want false", k)
		}
	}

	// Wildcards are legal in a preference and never on an event.
	for _, k := range []string{"*", "message.*"} {
		if !ValidPrefKind(k) {
			t.Errorf("ValidPrefKind(%q) = false, want true", k)
		}
		if ValidKind(k) {
			t.Errorf("ValidKind(%q) = true — a wildcard is not an event kind", k)
		}
	}
	if ValidPrefKind("*.dm") {
		t.Error("'*.dm' is not a supported pattern: the ladder wildcards the verb, not the resource")
	}
}

func TestObjectTypeValidation(t *testing.T) {
	for _, s := range []string{"message", "channel", "collab_document", "a"} {
		if !validObjectType(s) {
			t.Errorf("validObjectType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Message", "9message", "message-type",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if validObjectType(s) {
			t.Errorf("validObjectType(%q) = true, want false", s)
		}
	}
}

func TestEmailModeValidation(t *testing.T) {
	for _, m := range []string{EmailNever, EmailDigest, EmailImmediate} {
		if !ValidEmailMode(m) {
			t.Errorf("ValidEmailMode(%q) = false", m)
		}
	}
	for _, m := range []string{"", "daily", "DIGEST"} {
		if ValidEmailMode(m) {
			t.Errorf("ValidEmailMode(%q) = true", m)
		}
	}
}
