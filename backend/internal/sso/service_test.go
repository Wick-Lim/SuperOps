package sso

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
)

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestCallbackProvisionsAccountOnFirstLogin(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("newcomer") + "@example.com"

	result, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub":   "subject-jit",
		"email": email,
		"name":  "Jit Newcomer",
	}})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if result.Tokens == nil || result.Tokens.AccessToken == "" {
		t.Fatalf("expected a session, got step %q", result.Step)
	}

	userID := env.userIDForEmail(email)
	if got := env.roleOf(userID); got != authz.RoleMember {
		t.Errorf("workspace role = %q, want member", got)
	}

	// A provisioned account has no password: that is what makes it
	// unguessable at the password prompt.
	var hasPassword bool
	if err := env.pool.QueryRow(env.ctx,
		`SELECT COALESCE(password_hash,'') <> '' FROM users WHERE id = $1`, userID).Scan(&hasPassword); err != nil {
		t.Fatalf("read password state: %v", err)
	}
	if hasPassword {
		t.Error("a just-in-time provisioned account must not have a password")
	}

	identity, err := env.repo.GetIdentity(env.ctx, env.provider.ID, "subject-jit")
	if err != nil || identity == nil {
		t.Fatalf("identity not stored: %v", err)
	}
	if identity.UserID != userID {
		t.Errorf("identity points at %s, want %s", identity.UserID, userID)
	}

	if env.sessionMethod(result.Tokens.RefreshToken) != auth.MethodSSO {
		t.Error("the session must be recorded as an SSO session")
	}
	if n := env.countAudit(actionSSOLogin, userID); n != 1 {
		t.Errorf("audit entries for %s = %d, want 1", actionSSOLogin, n)
	}
	if n := env.countAudit(actionSSOProvisioned, userID); n != 1 {
		t.Errorf("audit entries for %s = %d, want 1", actionSSOProvisioned, n)
	}
}

func TestSecondLoginReusesTheSameAccount(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("returning") + "@example.com"
	spec := issueSpec{Claims: map[string]any{"sub": "subject-returning", "email": email}}

	if _, err := env.signIn(spec); err != nil {
		t.Fatalf("first sign in: %v", err)
	}
	first := env.userIDForEmail(email)

	if _, err := env.signIn(spec); err != nil {
		t.Fatalf("second sign in: %v", err)
	}

	var accounts int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("a repeat sign-in created %d accounts", accounts)
	}
	if env.userIDForEmail(email) != first {
		t.Error("the second sign-in resolved to a different account")
	}
}

// ---------------------------------------------------------------------------
// Assertion verification
// ---------------------------------------------------------------------------

// Every case here is a genuinely invalid token produced by the test's own
// signing key, not a string a mock was told to reject.
func TestCallbackRejectsInvalidAssertions(t *testing.T) {
	env := newEnv(t, nil)
	otherKey := env.idp.addKey("attacker-key")

	tests := []struct {
		name string
		spec issueSpec
	}{
		{
			name: "tampered signature",
			spec: issueSpec{Corrupt: true, Claims: map[string]any{"sub": "tampered"}},
		},
		{
			// Signed correctly, by the wrong key, and announced under the kid of
			// the right one. Nothing but the signature check catches this.
			name: "signed by a key that is not the provider's",
			spec: issueSpec{SignKey: otherKey, SignKID: "key-1", Claims: map[string]any{"sub": "wrongkey"}},
		},
		{
			name: "audience is another client",
			spec: issueSpec{Claims: map[string]any{"aud": "some-other-application", "sub": "wrongaud"}},
		},
		{
			name: "audience list without us, azp set to us",
			spec: issueSpec{Claims: map[string]any{
				"aud": []string{"another-client", "a-third-client"},
				"azp": "superops-test-client",
				"sub": "wrongaudlist",
			}},
		},
		{
			name: "expired",
			spec: issueSpec{Claims: map[string]any{
				"exp": time.Now().Add(-10 * time.Minute).Unix(),
				"sub": "expired",
			}},
		},
		{
			name: "issued by another provider",
			spec: issueSpec{Claims: map[string]any{"iss": "https://evil.example.com", "sub": "wrongiss"}},
		},
		{
			name: "nonce absent",
			spec: issueSpec{Claims: map[string]any{"nonce": omit{}, "sub": "nononce"}},
		},
		{
			name: "nonce from a different sign-in",
			spec: issueSpec{Claims: map[string]any{"nonce": "a-nonce-we-never-issued", "sub": "badnonce"}},
		},
		{
			name: "no subject",
			spec: issueSpec{Claims: map[string]any{"sub": omit{}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := env.signIn(tt.spec)
			if err == nil {
				t.Fatalf("an invalid assertion produced a result: %+v", result)
			}
			if !errors.Is(err, ErrAssertionInvalid) {
				t.Fatalf("error = %v, want ErrAssertionInvalid", err)
			}
		})
	}
}

// A replayed nonce is only reachable through a replayed callback, and the state
// is what makes that impossible: it is deleted the first time it is spent.
func TestCallbackStateIsSingleUse(t *testing.T) {
	env := newEnv(t, nil)

	start, err := env.svc.Start(env.ctx, env.workspaceSlug)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	nonce, challenge := env.parseAuthorizeURL(start.AuthorizationURL)
	code := env.idp.authorize(nonce, challenge, issueSpec{Claims: map[string]any{
		"sub":   "subject-replay",
		"email": unique("replay") + "@example.com",
	}})

	if _, err := env.svc.Callback(env.ctx, start.State, code, "agent", "203.0.113.10:1"); err != nil {
		t.Fatalf("first callback: %v", err)
	}

	// Same state, same code: the assertion that worked a moment ago.
	_, err = env.svc.Callback(env.ctx, start.State, code, "agent", "203.0.113.10:1")
	if !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("replayed callback error = %v, want ErrStateInvalid", err)
	}

	_, err = env.svc.Callback(env.ctx, "a-state-nobody-issued", code, "agent", "203.0.113.10:1")
	if !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("forged state error = %v, want ErrStateInvalid", err)
	}
}

// The other half of nonce replay: a token captured from one sign-in, presented
// against a second, fresh state.
func TestNonceBindsTheTokenToOneSignIn(t *testing.T) {
	env := newEnv(t, nil)

	firstStart, err := env.svc.Start(env.ctx, env.workspaceSlug)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	capturedNonce, _ := env.parseAuthorizeURL(firstStart.AuthorizationURL)

	secondStart, err := env.svc.Start(env.ctx, env.workspaceSlug)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, challenge := env.parseAuthorizeURL(secondStart.AuthorizationURL)

	// A provider (or anything that can make one issue a token) replaying the
	// earlier nonce into the later sign-in.
	code := env.idp.authorize(capturedNonce, challenge, issueSpec{Claims: map[string]any{"sub": "subject-nonce"}})

	_, err = env.svc.Callback(env.ctx, secondStart.State, code, "agent", "203.0.113.10:1")
	if !errors.Is(err, ErrAssertionInvalid) {
		t.Fatalf("error = %v, want ErrAssertionInvalid", err)
	}
}

// PKCE: the code is worthless without the verifier bound to that one request.
func TestCodeIsBoundToThePKCEVerifier(t *testing.T) {
	env := newEnv(t, nil)

	start, err := env.svc.Start(env.ctx, env.workspaceSlug)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	nonce, _ := env.parseAuthorizeURL(start.AuthorizationURL)

	// The provider recorded a challenge from somebody else's verifier, so the
	// one this process replays cannot match.
	code := env.idp.authorize(nonce, "3fCC9Xr7lQ0aQhVAWWv2mSMDgrLZAgFTFrlNZuh2Xy8", issueSpec{})

	_, err = env.svc.Callback(env.ctx, start.State, code, "agent", "203.0.113.10:1")
	if !errors.Is(err, ErrExchangeFailed) {
		t.Fatalf("error = %v, want ErrExchangeFailed", err)
	}
}

// ---------------------------------------------------------------------------
// Key rotation
// ---------------------------------------------------------------------------

func TestUnknownKeyIDTriggersAJWKSRefetch(t *testing.T) {
	env := newEnv(t, nil)
	env.idp.publishOnly("key-1")

	if _, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "rotate-1", "email": unique("rotate") + "@example.com",
	}}); err != nil {
		t.Fatalf("sign in before rotation: %v", err)
	}
	fetchesBefore := env.idp.fetchCount()

	// The provider rotates: a new key, signing immediately, published alongside
	// the old one. The first token carrying the new kid is the only notice we
	// get.
	env.idp.addKey("key-2")
	env.idp.publishOnly("key-1", "key-2")

	if _, err := env.signIn(issueSpec{SignKID: "key-2", Claims: map[string]any{
		"sub": "rotate-2", "email": unique("rotate") + "@example.com",
	}}); err != nil {
		t.Fatalf("sign in after rotation: %v", err)
	}
	if env.idp.fetchCount() <= fetchesBefore {
		t.Error("an unknown kid did not cause the key set to be refetched")
	}

	// And a kid that no rotation will ever produce must still fail, without
	// falling back to "try every key".
	_, err := env.signIn(issueSpec{SignKID: "key-1", SignKey: env.idp.addKey("ghost"), Claims: map[string]any{"sub": "ghost"}})
	if !errors.Is(err, ErrAssertionInvalid) {
		t.Fatalf("error = %v, want ErrAssertionInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Account linking — the part a reviewer should look at hardest
// ---------------------------------------------------------------------------

// The core claim of this package: an identity provider asserting an address
// that already belongs to a local account does NOT get that account.
func TestExistingAccountIsNeverLinkedSilently(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("victim") + "@example.com"
	victimID := env.createUser(email, "victim-password")
	env.addMember(env.workspaceID, victimID, authz.RoleAdmin)

	result, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub":            "attacker-subject",
		"email":          email,
		"email_verified": true, // even verified. Verified by *them*, not by us.
	}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if result.Tokens != nil {
		t.Fatal("an unproven identity was handed a session for an existing account")
	}
	if result.Step != PendingLink {
		t.Fatalf("step = %q, want %q", result.Step, PendingLink)
	}
	if result.PendingToken == "" {
		t.Fatal("no challenge token was issued")
	}

	identity, err := env.repo.GetIdentity(env.ctx, env.provider.ID, "attacker-subject")
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	if identity != nil {
		t.Fatal("an identity binding was created without proof of the local credential")
	}
}

// The address comparison is case-insensitive, so the takeover cannot be
// arranged by asserting a differently-cased address and getting a second
// account for it.
func TestLinkChallengeIgnoresAddressCase(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("MixedCase") + "@Example.com"
	env.createUser(email, "victim-password")

	result, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub":   "case-subject",
		"email": strings.ToLower(email),
	}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if result.Step != PendingLink {
		t.Fatalf("step = %q, want a link challenge", result.Step)
	}
}

func TestLinkRequiresTheLocalPassword(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("linker") + "@example.com"
	userID := env.createUser(email, "correct-horse-battery")
	env.addMember(env.workspaceID, userID, authz.RoleMember)

	challenge, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "link-subject", "email": email}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}

	// Wrong password: refused, and the challenge is spent so guessing costs a
	// full round trip through the provider.
	if _, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "not-the-password", "", "agent", "203.0.113.10:1"); !errors.Is(err, ErrInvalidLocalCredential) {
		t.Fatalf("wrong password error = %v, want ErrInvalidLocalCredential", err)
	}
	if _, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "correct-horse-battery", "", "agent", "203.0.113.10:1"); !errors.Is(err, ErrPendingInvalid) {
		t.Fatalf("reused challenge error = %v, want ErrPendingInvalid", err)
	}

	// Fresh challenge, correct password: linked.
	challenge, err = env.signIn(issueSpec{Claims: map[string]any{"sub": "link-subject", "email": email}})
	if err != nil {
		t.Fatalf("second callback: %v", err)
	}
	result, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "correct-horse-battery", "", "agent", "203.0.113.10:1")
	if err != nil {
		t.Fatalf("complete link: %v", err)
	}
	if result.Tokens == nil {
		t.Fatal("a completed link produced no session")
	}

	identity, err := env.repo.GetIdentity(env.ctx, env.provider.ID, "link-subject")
	if err != nil || identity == nil {
		t.Fatalf("identity not stored after linking: %v", err)
	}
	if identity.UserID != userID {
		t.Errorf("identity bound to %s, want %s", identity.UserID, userID)
	}

	// And from here on it is an ordinary sign-in.
	next, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "link-subject", "email": email}})
	if err != nil {
		t.Fatalf("sign in after linking: %v", err)
	}
	if next.Tokens == nil {
		t.Fatalf("a linked identity still got step %q", next.Step)
	}
}

// A local account with a second factor keeps it during linking: the password
// alone must not be enough to bind an identity to a 2FA-protected account.
func TestLinkRequiresTheSecondFactorToo(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("linker2fa") + "@example.com"
	userID := env.createUser(email, "correct-horse-battery")
	backupCode := env.enrolTOTP(userID)

	challenge, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "link-2fa", "email": email}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if _, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "correct-horse-battery", "", "agent", "203.0.113.10:1"); !errors.Is(err, ErrSecondFactorRequired) {
		t.Fatalf("error = %v, want ErrSecondFactorRequired", err)
	}

	challenge, err = env.signIn(issueSpec{Claims: map[string]any{"sub": "link-2fa", "email": email}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if _, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "correct-horse-battery", "000000", "agent", "203.0.113.10:1"); !errors.Is(err, ErrSecondFactorInvalid) {
		t.Fatalf("error = %v, want ErrSecondFactorInvalid", err)
	}

	challenge, err = env.signIn(issueSpec{Claims: map[string]any{"sub": "link-2fa", "email": email}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	result, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "correct-horse-battery", backupCode, "agent", "203.0.113.10:1")
	if err != nil {
		t.Fatalf("complete link with backup code: %v", err)
	}
	if result.Tokens == nil {
		t.Fatal("no session after a fully proven link")
	}
}

func TestLinkingCanBeDisabledEntirely(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) { in.AllowLinking = false })
	email := unique("nolink") + "@example.com"
	env.createUser(email, "some-password")

	_, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "nolink-subject", "email": email}})
	if !errors.Is(err, ErrLinkNotAllowed) {
		t.Fatalf("error = %v, want ErrLinkNotAllowed", err)
	}
}

// ---------------------------------------------------------------------------
// email_verified
// ---------------------------------------------------------------------------

func TestProvisioningRequiresAVerifiedAddress(t *testing.T) {
	tests := []struct {
		name    string
		claim   any
		require bool
		wantErr error
	}{
		{"unverified", false, true, ErrEmailNotVerified},
		{"claim absent", omit{}, true, ErrEmailNotVerified},
		{"verified", true, true, nil},
		{"string true", "true", true, nil},
		// Entra ID does not emit the claim at all, which is the only reason the
		// requirement is switchable.
		{"claim absent but the directory is trusted", omit{}, false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t, func(in *SaveProviderInput) { in.RequireVerifiedEmail = tt.require })
			email := unique("verify") + "@example.com"

			result, err := env.signIn(issueSpec{Claims: map[string]any{
				"sub":            unique("subject"),
				"email":          email,
				"email_verified": tt.claim,
			}})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("sign in: %v", err)
			}
			if result.Tokens == nil {
				t.Fatalf("expected a session, got step %q", result.Step)
			}
		})
	}
}

func TestProvisioningRequiresAnAddress(t *testing.T) {
	env := newEnv(t, nil)
	_, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "no-email", "email": omit{}}})
	if !errors.Is(err, ErrEmailMissing) {
		t.Fatalf("error = %v, want ErrEmailMissing", err)
	}
}

func TestProvisioningCanBeDisabled(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) { in.AllowJIT = false })
	_, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "nojit", "email": unique("nojit") + "@example.com",
	}})
	if !errors.Is(err, ErrJITNotAllowed) {
		t.Fatalf("error = %v, want ErrJITNotAllowed", err)
	}
}

// ---------------------------------------------------------------------------
// Second factors and deactivation: the two things SSO must not bypass
// ---------------------------------------------------------------------------

func TestSSODoesNotBypassTwoFactor(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("twofactor") + "@example.com"

	// Provision through SSO, then enrol a second factor, as a user would.
	if _, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "2fa-subject", "email": email}}); err != nil {
		t.Fatalf("first sign in: %v", err)
	}
	userID := env.userIDForEmail(email)
	backupCode := env.enrolTOTP(userID)

	result, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "2fa-subject", "email": email}})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if result.Tokens != nil {
		t.Fatal("SSO issued a session for a 2FA-protected account without a second factor")
	}
	if result.Step != PendingTOTP {
		t.Fatalf("step = %q, want %q", result.Step, PendingTOTP)
	}

	if _, err := env.svc.CompleteSecondFactor(env.ctx, result.PendingToken, "000000", "agent", "203.0.113.10:1"); !errors.Is(err, ErrSecondFactorInvalid) {
		t.Fatalf("wrong code error = %v, want ErrSecondFactorInvalid", err)
	}

	result, err = env.signIn(issueSpec{Claims: map[string]any{"sub": "2fa-subject", "email": email}})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	done, err := env.svc.CompleteSecondFactor(env.ctx, result.PendingToken, backupCode, "agent", "203.0.113.10:1")
	if err != nil {
		t.Fatalf("complete second factor: %v", err)
	}
	if done.Tokens == nil {
		t.Fatal("no session after a valid second factor")
	}

	// The backup code is spent; a replay of it must not authenticate again.
	result, err = env.signIn(issueSpec{Claims: map[string]any{"sub": "2fa-subject", "email": email}})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if _, err := env.svc.CompleteSecondFactor(env.ctx, result.PendingToken, backupCode, "agent", "203.0.113.10:1"); !errors.Is(err, ErrSecondFactorInvalid) {
		t.Fatalf("replayed backup code error = %v, want ErrSecondFactorInvalid", err)
	}
}

func TestIDPAssertedMFAOnlyCountsWithEvidence(t *testing.T) {
	tests := []struct {
		name      string
		trust     bool
		acr       string
		claims    map[string]any
		wantStep  string
		wantToken bool
	}{
		{
			name:     "not trusted, amr says mfa",
			trust:    false,
			claims:   map[string]any{"amr": []string{"pwd", "mfa"}},
			wantStep: PendingTOTP,
		},
		{
			name:     "trusted but the token shows no second factor",
			trust:    true,
			claims:   map[string]any{"amr": []string{"pwd"}},
			wantStep: PendingTOTP,
		},
		{
			name:     "trusted and no amr at all",
			trust:    true,
			claims:   map[string]any{},
			wantStep: PendingTOTP,
		},
		{
			name:      "trusted with amr evidence",
			trust:     true,
			claims:    map[string]any{"amr": []string{"pwd", "otp"}},
			wantToken: true,
		},
		{
			name:      "trusted with the required acr",
			trust:     true,
			acr:       "urn:acme:loa:strong",
			claims:    map[string]any{"acr": "urn:acme:loa:strong"},
			wantToken: true,
		},
		{
			name:     "trusted with the wrong acr",
			trust:    true,
			acr:      "urn:acme:loa:strong",
			claims:   map[string]any{"acr": "urn:acme:loa:weak", "amr": []string{"mfa"}},
			wantStep: PendingTOTP,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t, func(in *SaveProviderInput) {
				in.TrustIDPMFA = tt.trust
				in.RequiredACR = tt.acr
			})
			email := unique("mfa") + "@example.com"
			if _, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "mfa-subject", "email": email}}); err != nil {
				t.Fatalf("first sign in: %v", err)
			}
			env.enrolTOTP(env.userIDForEmail(email))

			claims := map[string]any{"sub": "mfa-subject", "email": email}
			for k, v := range tt.claims {
				claims[k] = v
			}
			result, err := env.signIn(issueSpec{Claims: claims})
			if err != nil {
				t.Fatalf("sign in: %v", err)
			}
			if tt.wantToken {
				if result.Tokens == nil {
					t.Fatalf("expected a session, got step %q", result.Step)
				}
				return
			}
			if result.Tokens != nil {
				t.Fatal("the local second factor was skipped without evidence")
			}
			if result.Step != tt.wantStep {
				t.Fatalf("step = %q, want %q", result.Step, tt.wantStep)
			}
		})
	}
}

func TestDeactivatedAccountCannotSignInThroughSSO(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("disabled") + "@example.com"

	if _, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "disabled-subject", "email": email}}); err != nil {
		t.Fatalf("first sign in: %v", err)
	}
	env.deactivate(env.userIDForEmail(email))

	_, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "disabled-subject", "email": email}})
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("error = %v, want ErrAccountInactive", err)
	}
}

func TestDeactivatedAccountCannotBeLinked(t *testing.T) {
	env := newEnv(t, nil)
	email := unique("disabledlocal") + "@example.com"
	env.deactivate(env.createUser(email, "some-password"))

	_, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "disabled-link", "email": email}})
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("error = %v, want ErrAccountInactive", err)
	}
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

func TestRoleIsTakenFromTheProviderWhenConfigured(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) {
		in.RoleClaim = "groups"
		in.RoleMapping = map[string]string{
			"acme-admins":  authz.RoleAdmin,
			"acme-staff":   authz.RoleMember,
			"acme-vendors": authz.RoleGuest,
			// An IdP group that tries to claim ownership must not be able to.
			"acme-owners": authz.RoleOwner,
		}
		in.DefaultRole = authz.RoleGuest
	})

	email := unique("roled") + "@example.com"
	if _, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "role-subject", "email": email,
		"groups": []string{"acme-staff", "acme-admins"},
	}}); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	userID := env.userIDForEmail(email)
	if got := env.roleOf(userID); got != authz.RoleAdmin {
		t.Errorf("role = %q, want admin (the most privileged mapped group)", got)
	}

	// A later login with a narrower group demotes, because the provider is
	// authoritative for the roles it asserts.
	if _, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "role-subject", "email": email, "groups": []string{"acme-vendors"},
	}}); err != nil {
		t.Fatalf("second sign in: %v", err)
	}
	if got := env.roleOf(userID); got != authz.RoleGuest {
		t.Errorf("role after demotion = %q, want guest", got)
	}

	// An unmapped group is not an assertion, so it must not overwrite a role an
	// administrator set by hand.
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE workspace_members SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`,
		env.workspaceID, userID); err != nil {
		t.Fatalf("promote by hand: %v", err)
	}
	if _, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "role-subject", "email": email, "groups": []string{"some-unrelated-group"},
	}}); err != nil {
		t.Fatalf("third sign in: %v", err)
	}
	if got := env.roleOf(userID); got != authz.RoleAdmin {
		t.Errorf("role = %q, want the hand-set admin to survive an unmapped group", got)
	}
}

// Ownership never comes from a directory group.
func TestOwnerRoleIsNeverAssignedOrOverwritten(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) {
		in.RoleClaim = "groups"
		in.RoleMapping = map[string]string{"acme-owners": authz.RoleOwner, "acme-staff": authz.RoleMember}
	})

	// The workspace owner signs in through SSO and is asserted as a plain
	// member. Their ownership must survive.
	ownerEmail := unique("wsowner") + "@example.com"
	if _, err := env.pool.Exec(env.ctx, `UPDATE users SET email = $2 WHERE id = $1`, env.ownerID, ownerEmail); err != nil {
		t.Fatalf("rename owner: %v", err)
	}

	challenge, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "owner-subject", "email": ownerEmail, "groups": []string{"acme-staff"},
	}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if challenge.Step != PendingLink {
		t.Fatalf("step = %q, want a link challenge for the existing owner account", challenge.Step)
	}
	if _, err := env.svc.CompleteLink(env.ctx, challenge.PendingToken, "owner-password", "", "agent", "203.0.113.10:1"); err != nil {
		t.Fatalf("complete link: %v", err)
	}
	if _, err := env.signIn(issueSpec{Claims: map[string]any{
		"sub": "owner-subject", "email": ownerEmail, "groups": []string{"acme-staff"},
	}}); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if got := env.roleOf(env.ownerID); got != authz.RoleOwner {
		t.Errorf("owner role = %q, want it untouched by the provider's assertion", got)
	}
}

// ---------------------------------------------------------------------------
// Enforcement, and not being locked out by it
// ---------------------------------------------------------------------------

func TestEnforcementDisablesPasswordLoginForMembers(t *testing.T) {
	env := newEnv(t, nil)

	memberEmail := unique("member") + "@example.com"
	memberID := env.createUser(memberEmail, "member-password")
	env.addMember(env.workspaceID, memberID, authz.RoleMember)

	// Before enforcement, the password works.
	if _, err := env.auth.Login(env.ctx, auth.LoginInput{Email: memberEmail, Password: "member-password"}, "agent", "203.0.113.10:1"); err != nil {
		t.Fatalf("login before enforcement: %v", err)
	}

	// The administrator must have their own identity linked first.
	if _, err := env.svc.SetEnforced(env.ctx, env.workspaceID, env.ownerID, true, "203.0.113.10:1"); !errors.Is(err, ErrEnforceNeedsIdentity) {
		t.Fatalf("error = %v, want ErrEnforceNeedsIdentity", err)
	}
	if err := env.repo.LinkIdentity(env.ctx, env.provider.ID, env.ownerID, "owner-subject", "owner@example.com"); err != nil {
		t.Fatalf("link owner identity: %v", err)
	}
	if _, err := env.svc.SetEnforced(env.ctx, env.workspaceID, env.ownerID, true, "203.0.113.10:1"); err != nil {
		t.Fatalf("set enforced: %v", err)
	}

	// The member is now SSO-only.
	_, err := env.auth.Login(env.ctx, auth.LoginInput{Email: memberEmail, Password: "member-password"}, "agent", "203.0.113.10:1")
	if !errors.Is(err, auth.ErrSSORequired) {
		t.Fatalf("member login error = %v, want auth.ErrSSORequired", err)
	}

	// The owner is not, so there is always a way back to the switch.
	if _, err := env.auth.Login(env.ctx, auth.LoginInput{Email: env.ownerEmail(), Password: "owner-password"}, "agent", "203.0.113.10:1"); err != nil {
		t.Fatalf("owner login under enforcement: %v", err)
	}

	// ...unless the deployment explicitly gives that up.
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE sso_providers SET allow_owner_password_login = FALSE WHERE id = $1`, env.provider.ID); err != nil {
		t.Fatalf("disable owner exemption: %v", err)
	}
	_, err = env.auth.Login(env.ctx, auth.LoginInput{Email: env.ownerEmail(), Password: "owner-password"}, "agent", "203.0.113.10:1")
	if !errors.Is(err, auth.ErrSSORequired) {
		t.Fatalf("owner login error = %v, want auth.ErrSSORequired", err)
	}
}

// Enforcement has to bite an existing password session too, or it takes effect
// only when the refresh token happens to expire.
func TestEnforcementStopsPasswordSessionsFromRefreshing(t *testing.T) {
	env := newEnv(t, nil)

	memberEmail := unique("refresher") + "@example.com"
	memberID := env.createUser(memberEmail, "member-password")
	env.addMember(env.workspaceID, memberID, authz.RoleMember)

	tokens, err := env.auth.Login(env.ctx, auth.LoginInput{Email: memberEmail, Password: "member-password"}, "agent", "203.0.113.10:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if env.sessionMethod(tokens.RefreshToken) != auth.MethodPassword {
		t.Fatal("a password login must be recorded as a password session")
	}

	if err := env.repo.LinkIdentity(env.ctx, env.provider.ID, env.ownerID, "owner-subject", "owner@example.com"); err != nil {
		t.Fatalf("link owner identity: %v", err)
	}
	if _, err := env.svc.SetEnforced(env.ctx, env.workspaceID, env.ownerID, true, "203.0.113.10:1"); err != nil {
		t.Fatalf("set enforced: %v", err)
	}

	_, err = env.auth.RefreshTokens(env.ctx, tokens.RefreshToken, "agent", "203.0.113.10:1")
	if !errors.Is(err, auth.ErrSSORequired) {
		t.Fatalf("refresh error = %v, want auth.ErrSSORequired", err)
	}

	// An SSO session of the same user keeps working: enforcement is asking for
	// exactly that.
	ssoEmail := unique("ssouser") + "@example.com"
	result, err := env.signIn(issueSpec{Claims: map[string]any{"sub": "sso-refresh", "email": ssoEmail}})
	if err != nil {
		t.Fatalf("sso sign in: %v", err)
	}
	if _, err := env.auth.RefreshTokens(env.ctx, result.Tokens.RefreshToken, "agent", "203.0.113.10:1"); err != nil {
		t.Fatalf("refresh of an sso session: %v", err)
	}
}

// An invitation into an enforced workspace is refused before it is consumed —
// otherwise the invite would be burned to produce a session that cannot be
// refreshed.
func TestInviteIntoAnEnforcedWorkspaceIsRefusedIntact(t *testing.T) {
	env := newEnv(t, nil)
	if err := env.repo.LinkIdentity(env.ctx, env.provider.ID, env.ownerID, "owner-subject", "owner@example.com"); err != nil {
		t.Fatalf("link owner identity: %v", err)
	}
	if _, err := env.svc.SetEnforced(env.ctx, env.workspaceID, env.ownerID, true, "203.0.113.10:1"); err != nil {
		t.Fatalf("set enforced: %v", err)
	}

	token := unique("invite")
	inviteEmail := unique("invitee") + "@example.com"
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO invitations (workspace_id, email, role, token_hash, invited_by, status, expires_at)
		 VALUES ($1, $2, 'member', $3, $4, 'pending', NOW() + INTERVAL '1 day')`,
		env.workspaceID, inviteEmail, hashForTest(token), env.ownerID); err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	_, err := env.auth.AcceptInvite(env.ctx, auth.AcceptInviteInput{
		Token: token, Username: unique("inv"), Password: "a-good-password", IPAddress: "203.0.113.10:1",
	})
	if !errors.Is(err, auth.ErrSSORequired) {
		t.Fatalf("accept invite error = %v, want auth.ErrSSORequired", err)
	}

	var status string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT status FROM invitations WHERE token_hash = $1`, hashForTest(token)).Scan(&status); err != nil {
		t.Fatalf("read invitation: %v", err)
	}
	if status != "pending" {
		t.Errorf("invitation status = %q, want it left pending", status)
	}
}

func TestEnforcementCannotBeTurnedOnForADisabledProvider(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) { in.Enabled = false })
	if err := env.repo.LinkIdentity(env.ctx, env.provider.ID, env.ownerID, "owner-subject", "owner@example.com"); err != nil {
		t.Fatalf("link owner identity: %v", err)
	}
	if _, err := env.svc.SetEnforced(env.ctx, env.workspaceID, env.ownerID, true, "203.0.113.10:1"); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("error = %v, want ErrProviderDisabled", err)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestSaveProviderNeverExposesTheClientSecret(t *testing.T) {
	env := newEnv(t, nil)
	secret := "s3cr3t-from-the-idp"

	saved, err := env.svc.SaveProvider(env.ctx, env.workspaceID, env.ownerID, SaveInput{
		Name:         "Acme",
		Issuer:       env.idp.issuer(),
		ClientID:     env.idp.clientID,
		ClientSecret: &secret,
		RedirectURI:  "https://app.example.com/sso/callback",
		Enabled:      boolPtr(true),
		DefaultRole:  authz.RoleMember,
	}, "203.0.113.10:1")
	if err != nil {
		t.Fatalf("save provider: %v", err)
	}

	view := saved.View()
	if !view.ClientSecretSet {
		t.Error("client_secret_set should be true")
	}
	body := renderJSON(t, view)
	if strings.Contains(body, secret) {
		t.Fatalf("the client secret reached the API representation: %s", body)
	}

	// Sealed at rest, and openable only with the configured key.
	var stored []byte
	if err := env.pool.QueryRow(env.ctx,
		`SELECT client_secret_enc FROM sso_providers WHERE id = $1`, saved.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if strings.Contains(string(stored), secret) {
		t.Fatal("the client secret is stored in plaintext")
	}
	opened, err := saved.ClientSecret(testSecretKey)
	if err != nil || opened != secret {
		t.Fatalf("open secret = %q, %v", opened, err)
	}
	if _, err := saved.ClientSecret([]byte("ffffffffffffffffffffffffffffffff")); err == nil {
		t.Fatal("a different key must not open the secret")
	}

	// A later save that sends no secret keeps the stored one.
	again, err := env.svc.SaveProvider(env.ctx, env.workspaceID, env.ownerID, SaveInput{
		Name: "Acme Renamed", Issuer: env.idp.issuer(), ClientID: env.idp.clientID,
		RedirectURI: "https://app.example.com/sso/callback", Enabled: boolPtr(true), DefaultRole: authz.RoleMember,
	}, "203.0.113.10:1")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !again.HasClientSecret() {
		t.Fatal("omitting client_secret erased the stored one")
	}
}

func TestSaveProviderRejectsAnUnreachableIssuer(t *testing.T) {
	env := newEnv(t, nil)
	_, err := env.svc.SaveProvider(env.ctx, env.workspaceID, env.ownerID, SaveInput{
		Issuer:      env.idp.issuer() + "/not-an-issuer",
		ClientID:    "x",
		RedirectURI: "https://app.example.com/sso/callback",
		Enabled:     boolPtr(true),
		DefaultRole: authz.RoleMember,
	}, "203.0.113.10:1")
	if err == nil {
		t.Fatal("enabling a provider that does not answer discovery must fail at save time")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestSaveProviderValidatesRoles(t *testing.T) {
	env := newEnv(t, nil)
	_, err := env.svc.SaveProvider(env.ctx, env.workspaceID, env.ownerID, SaveInput{
		Issuer: env.idp.issuer(), ClientID: "x", RedirectURI: "https://app.example.com/cb",
		DefaultRole: authz.RoleOwner,
	}, "203.0.113.10:1")
	if err == nil {
		t.Fatal("default_role owner must be refused")
	}

	_, err = env.svc.SaveProvider(env.ctx, env.workspaceID, env.ownerID, SaveInput{
		Issuer: env.idp.issuer(), ClientID: "x", RedirectURI: "https://app.example.com/cb",
		DefaultRole: authz.RoleMember, RoleClaim: "groups",
		RoleMapping: map[string]string{"g": authz.RoleOwner},
	}, "203.0.113.10:1")
	if err == nil {
		t.Fatal("a role mapping onto owner must be refused")
	}
}

func TestVerifyReportsWhatTheProviderActuallySupports(t *testing.T) {
	env := newEnv(t, nil)
	out, err := env.svc.Verify(env.ctx, env.workspaceID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.Issuer != env.idp.issuer() {
		t.Errorf("issuer = %q, want %q", out.Issuer, env.idp.issuer())
	}
	if out.SigningKeys < 1 {
		t.Error("verify reported no signing keys")
	}
	if !out.SupportsS256 {
		t.Error("the fake provider advertises S256 and verify did not see it")
	}
}

func TestDisabledProviderCannotStartASignIn(t *testing.T) {
	env := newEnv(t, func(in *SaveProviderInput) { in.Enabled = false })
	if _, err := env.svc.Start(env.ctx, env.workspaceSlug); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("error = %v, want ErrProviderDisabled", err)
	}
	if _, err := env.svc.PublicInfo(env.ctx, env.workspaceSlug); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("error = %v, want ErrProviderNotFound", err)
	}
	if _, err := env.svc.Start(env.ctx, "a-workspace-that-does-not-exist"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("error = %v, want ErrProviderNotFound", err)
	}
}

func TestExpiredAuthRequestCannotBeCompleted(t *testing.T) {
	env := newEnv(t, nil)

	start, err := env.svc.Start(env.ctx, env.workspaceSlug)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	nonce, challenge := env.parseAuthorizeURL(start.AuthorizationURL)
	code := env.idp.authorize(nonce, challenge, issueSpec{})

	if _, err := env.pool.Exec(env.ctx,
		`UPDATE sso_auth_requests SET expires_at = NOW() - INTERVAL '1 minute' WHERE state_hash = $1`,
		hashForTest(start.State)); err != nil {
		t.Fatalf("expire auth request: %v", err)
	}

	if _, err := env.svc.Callback(env.ctx, start.State, code, "agent", "203.0.113.10:1"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("error = %v, want ErrStateInvalid", err)
	}

	// And the sweep the worker runs removes it.
	if _, err := env.repo.CleanExpiredAuthRequests(env.ctx); err != nil {
		t.Fatalf("clean expired: %v", err)
	}
}
