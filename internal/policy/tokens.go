package policy

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// tokenTTL is the proposal_token expiry (dev_docs/safety-architecture.md
// section 3.3: 120s for proposal_token; the 10 min human-class expiry does
// not apply, D3 removed the human confirmation class from this package
// entirely).
const tokenTTL = 120 * time.Second

// proposal is everything a proposal_token binds
// (dev_docs/safety-architecture.md section 3.3: printer identity, action,
// parameters, job identity, gating state, snapshot time).
type proposal struct {
	identity     string
	action       ActionName
	params       Params
	job          *printerstate.JobIdentity
	bucket       printerstate.Bucket
	class        printerstate.GatingClass
	snapshotTime time.Time
	issuedAt     time.Time
}

// tokenStore holds outstanding proposals in memory only: a server restart
// forgets every token, which is exactly why the "unknown token" error
// (notFoundToken) below names that possibility explicitly
// (dev_docs/safety-architecture.md section 3.3 point 2).
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]proposal
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: map[string]proposal{}}
}

// issue stores p under a fresh random token and returns it, garbage
// collecting expired entries first so the store cannot grow unbounded
// across a long-running process.
func (s *tokenStore) issue(p proposal) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(p.issuedAt)
	tok := randomToken()
	s.tokens[tok] = p
	return tok
}

// consume looks up token, removing it unconditionally the moment it is
// found (single use: 3.3 point 2, "tokens are single use"). ok is false for
// a token this store has never seen, or one that has expired; both cases
// are reported identically by the caller (notFoundToken's exact wording),
// since neither can be told apart from the caller's side.
func (s *tokenStore) consume(token string, now time.Time) (proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.tokens[token]
	if !ok {
		return proposal{}, false
	}
	delete(s.tokens, token)
	if now.Sub(p.issuedAt) > tokenTTL {
		return proposal{}, false
	}
	return p, true
}

// gcLocked drops every entry older than tokenTTL relative to now. Callers
// must hold s.mu.
func (s *tokenStore) gcLocked(now time.Time) {
	for tok, p := range s.tokens {
		if now.Sub(p.issuedAt) > tokenTTL {
			delete(s.tokens, tok)
		}
	}
}

// randomToken returns a fresh, unguessable opaque token: 24 random bytes,
// hex-encoded.
func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read only fails if the OS entropy source itself is
		// broken, a condition this process cannot meaningfully recover
		// from; a token collision here would be a worse safety failure
		// than a panic that surfaces the problem immediately.
		panic("policy: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// notFoundError is the exact distinct not_found wording
// dev_docs/safety-architecture.md section 3.3 point 2 requires for both an
// unknown and an expired/used token.
func notFoundTokenError(action ActionName) *Error {
	return &Error{
		Action:  action,
		Code:    CodeNotFound,
		Message: "no such proposal (expired, already used, or the server restarted); request a new proposal",
	}
}
