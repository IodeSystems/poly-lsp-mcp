package mcp

// Per-session mutation isolation. One daemon hosts a single *Server per
// workspace root, shared across every connected client, so the open
// commit:false batch can no longer be a single field: two clients editing
// one root would otherwise commit or revert each other's staged edits.
//
// A sessionID names the owner of a batch. In a stdio server there is one
// implicit session (localSession, the zero value — so an un-threaded call
// is correct by default); the daemon passes the client's X-Poly-Session id
// so each client's batch and file claims stand alone. All state here is
// touched only under editMu (the per-root edit serializer), including
// RollbackSession, which the daemon calls from its disconnect watcher.

// sessionID identifies a client session that owns an open edit batch.
type sessionID string

// localSession is the implicit single session of a stdio server. It is the
// zero value on purpose: any edit path that never learns a real session
// (stdio, the legacy surface) transparently uses this one bucket.
const localSession sessionID = ""

// normSession maps a wire session string to a sessionID; empty stays
// localSession (a daemon client that sends no X-Poly-Session shares the
// implicit bucket rather than erroring).
func normSession(s string) sessionID { return sessionID(s) }

// currentBatch returns the open batch for the ACTIVE session, or nil.
// Assumes editMu held (the edit handler holds it for the whole operation);
// a nil map reads as no batch.
func (s *Server) currentBatch() *editBatch { return s.batches[s.activeSession] }

// setBatch records the active session's open batch (lazy-init).
func (s *Server) setBatch(b *editBatch) {
	if s.batches == nil {
		s.batches = map[sessionID]*editBatch{}
	}
	s.batches[s.activeSession] = b
}

// closeBatch drops the active session's batch and releases its file
// claims. Assumes editMu held.
func (s *Server) closeBatch() {
	sess := s.activeSession
	if b := s.batches[sess]; b != nil {
		s.releaseClaims(sess, b)
	}
	delete(s.batches, sess)
}

// claimFile records that sess has staged uri. A second session staging the
// same file is refused (returns the current holder, ok=false) — a per-file
// claim, not a per-root lease, so two sessions with disjoint file sets stay
// independent while a genuine overlap is caught before a naive revert could
// restore one session's original over the other's staged edit. Assumes
// editMu held.
func (s *Server) claimFile(uri string, sess sessionID) (holder sessionID, ok bool) {
	if h, taken := s.claims[uri]; taken && h != sess {
		return h, false
	}
	if s.claims == nil {
		s.claims = map[string]sessionID{}
	}
	s.claims[uri] = sess
	return sess, true
}

// releaseClaims drops every claim sess holds for the files in b.
func (s *Server) releaseClaims(sess sessionID, b *editBatch) {
	for uri := range b.originals {
		if s.claims[uri] == sess {
			delete(s.claims, uri)
		}
	}
}

// RollbackSession discards a session's open batch, reverting every staged
// file to its pre-batch bytes. The daemon calls it when a client's watch
// connection drops (auto-rollback on disconnect) so a vanished client can't
// strand a broken intermediate on disk. Returns the number of staged edits
// discarded and whether a batch was actually open. Takes editMu, so it
// serializes with in-flight edits from OTHER sessions on the same root.
func (s *Server) RollbackSession(sess string) (discarded int, ok bool) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	sid := normSession(sess)
	b := s.batches[sid]
	if b == nil {
		return 0, false
	}
	oc := editOutcome{}
	n := b.count
	b.revertAll(&oc)
	s.releaseClaims(sid, b)
	delete(s.batches, sid)
	return n, true
}
