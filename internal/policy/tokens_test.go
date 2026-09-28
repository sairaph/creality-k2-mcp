package policy

import (
	"testing"
	"time"
)

func TestTokenStore_IssueAndConsume(t *testing.T) {
	s := newTokenStore()
	now := time.Now()
	tok := s.issue(proposal{identity: "p1", action: ActionCancelPrint, issuedAt: now})

	got, ok := s.consume(tok, now)
	if !ok {
		t.Fatal("consume() ok = false, want true")
	}
	if got.identity != "p1" || got.action != ActionCancelPrint {
		t.Errorf("consume() = %+v, want identity p1 / action cancel_print", got)
	}

	if _, ok := s.consume(tok, now); ok {
		t.Fatal("second consume() of the same token ok = true, want false (single use)")
	}
}

func TestTokenStore_UnknownTokenNotOK(t *testing.T) {
	s := newTokenStore()
	if _, ok := s.consume("does-not-exist", time.Now()); ok {
		t.Fatal("consume() of an unknown token ok = true, want false")
	}
}

// TestTokenStore_ExpiryAt120Seconds pins the 120s proposal_token expiry
// (dev_docs/safety-architecture.md section 3.3) without a real 120s sleep,
// by issuing the token as if it were already 121s old.
func TestTokenStore_ExpiryAt120Seconds(t *testing.T) {
	s := newTokenStore()
	issuedAt := time.Now().Add(-121 * time.Second)
	tok := s.issue(proposal{identity: "p1", action: ActionCancelPrint, issuedAt: issuedAt})

	if _, ok := s.consume(tok, time.Now()); ok {
		t.Fatal("consume() of a 121s-old token ok = true, want false (past the 120s expiry)")
	}
}

func TestTokenStore_StillValidJustUnderExpiry(t *testing.T) {
	s := newTokenStore()
	issuedAt := time.Now().Add(-119 * time.Second)
	tok := s.issue(proposal{identity: "p1", action: ActionCancelPrint, issuedAt: issuedAt})

	if _, ok := s.consume(tok, time.Now()); !ok {
		t.Fatal("consume() of a 119s-old token ok = false, want true (still inside the 120s expiry)")
	}
}

func TestNotFoundTokenError_ExactWording(t *testing.T) {
	err := notFoundTokenError(ActionResumePrint)
	if err.Code != CodeNotFound {
		t.Fatalf("Code = %q, want %q", err.Code, CodeNotFound)
	}
	const want = "no such proposal (expired, already used, or the server restarted); request a new proposal"
	if err.Message != want {
		t.Fatalf("Message = %q, want %q", err.Message, want)
	}
}
