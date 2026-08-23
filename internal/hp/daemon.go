package hp

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// leaseTimeout bounds how long a peer will block waiting for the agent that
// took the lease. If the holder dies, everyone else falls through and
// executes normally rather than hanging.
const leaseTimeout = 15 * time.Minute

type lease struct {
	agent string
	at    time.Time
	done  chan struct{}
}

// Server is the shared cache. It is the only writer of the log, which is what
// keeps concurrent agents from interleaving partial lines.
type Server struct {
	store *Store

	mu     sync.Mutex
	leases map[string]*lease

	subMu sync.Mutex
	subs  map[chan []byte]struct{}

	start time.Time
}

func NewServer(store *Store) *Server {
	return &Server{
		store:  store,
		leases: map[string]*lease{},
		subs:   map[chan []byte]struct{}{},
		start:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/lookup", s.handleLookup)
	mux.HandleFunc("/record", s.handleRecord)
	mux.HandleFunc("/release", s.handleRelease)
	mux.HandleFunc("/servable", s.handleServable)
	mux.HandleFunc("/verify", s.handleVerify)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	return cors(mux)
}

// cors lets web/viewer.html connect straight from a file:// origin.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLookup is where single-flight lives.
//
// Without it, a cold fan-out gets nothing from the cache: five agents launched
// at once run the same first command simultaneously, so none of them has a
// peer's result to serve yet and all five pay in full. That is precisely the
// opening stretch where cross-agent overlap is highest, so losing it loses the
// best part of the data. Blocking the second through fifth callers on the
// first one's execution turns five installs into one.
func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	var req LookupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, LookupResp{Decision: DecisionMiss})
		return
	}

	if !req.Serve {
		// Baseline arm, or a command that is not serve-eligible. Still take a
		// lease so peers are not stampeding, but never serve a result.
		s.acquire(req.Key, req.Agent)
		s.emitDecision(&req, DecisionMiss, nil, 0)
		writeJSON(w, LookupResp{Decision: DecisionMiss})
		return
	}

	if rec, ok := s.store.Lookup(req.Key); ok {
		s.emitDecision(&req, DecisionHit, rec, 0)
		writeJSON(w, hitResp(rec, 0))
		return
	}

	waited, served := s.waitForPeer(req.Key)
	if served != nil {
		s.emitDecision(&req, DecisionLeaseWait, served, waited)
		writeJSON(w, hitResp(served, waited))
		return
	}

	s.acquire(req.Key, req.Agent)
	s.emitDecision(&req, DecisionMiss, nil, waited)
	writeJSON(w, LookupResp{Decision: DecisionMiss, WaitedMS: waited})
}

// waitForPeer blocks while another agent is executing this exact command at
// this exact state. Returns the peer's result if it arrived.
func (s *Server) waitForPeer(key string) (waitedMS int64, rec *Record) {
	s.mu.Lock()
	l, held := s.leases[key]
	s.mu.Unlock()
	if !held {
		return 0, nil
	}
	started := time.Now()
	select {
	case <-l.done:
	case <-time.After(leaseTimeout):
		// Holder died or is pathologically slow. Fall through and execute
		// rather than hanging the agent forever.
		s.mu.Lock()
		if cur, ok := s.leases[key]; ok && cur == l {
			delete(s.leases, key)
		}
		s.mu.Unlock()
		return time.Since(started).Milliseconds(), nil
	}
	waited := time.Since(started).Milliseconds()
	if r, ok := s.store.Lookup(key); ok {
		return waited, r
	}
	return waited, nil
}

func (s *Server) acquire(key, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.leases[key]; ok {
		return
	}
	s.leases[key] = &lease{agent: agent, at: time.Now(), done: make(chan struct{})}
}

func (s *Server) release(key string) {
	s.mu.Lock()
	l, ok := s.leases[key]
	if ok {
		delete(s.leases, key)
	}
	s.mu.Unlock()
	if ok {
		close(l.done)
	}
}

func hitResp(rec *Record, waited int64) LookupResp {
	return LookupResp{
		Decision:    DecisionHit,
		ExitCode:    rec.ExitCode,
		StdoutBlob:  rec.StdoutBlob,
		StderrBlob:  rec.StderrBlob,
		SourceAgent: rec.Agent,
		DurationMS:  rec.DurationMS,
		WaitedMS:    waited,
	}
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	var rec Record
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec.V = 1
	if rec.TS == 0 {
		rec.TS = float64(time.Now().UnixMilli()) / 1000
	}
	if err := s.store.Append(&rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.release(rec.Key)
	s.broadcast(map[string]any{"type": "decision", "record": rec})
	s.broadcastStats()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.release(body.Key)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleServable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.ServableRecords())
}

// VerifyVerdict is the result of re-executing a served command for real and
// diffing it against what was served.
type VerifyVerdict struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	RawMatch bool   `json:"raw_match"`
}

// handleVerify records a verdict and evicts on divergence.
//
// Divergence must be loud. A cache that quietly serves a wrong answer is worse
// than no cache, so the counter is the credibility of the whole system and it
// is never allowed to be silently green.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var v VerifyVerdict
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	src, found := s.store.Lookup(v.Key)
	rec := &Record{
		V: 1, TS: float64(time.Now().UnixMilli()) / 1000,
		Agent: "verifier", Key: v.Key, Decision: "VERIFY",
		Reason: v.Detail, Verified: &v.OK,
	}
	if found {
		rec.Cmd, rec.CmdNorm, rec.CwdRel = src.Cmd, src.CmdNorm, src.CwdRel
		rec.Policy, rec.SourceAgent = src.Policy, src.Agent
	}
	if !v.OK {
		s.store.Evict(v.Key)
		log.Printf("hindsight: CACHE_MISMATCH key=%s %s", v.Key, v.Detail)
	}
	if err := s.store.Append(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.broadcast(map[string]any{"type": "verify", "key": v.Key, "ok": v.OK, "detail": v.Detail})
	s.broadcastStats()
	writeJSON(w, map[string]bool{"ok": true})
}

// emitDecision logs and broadcasts decisions the daemon makes on its own
// (hits and lease waits). Executions are logged by `hindsight record`.
func (s *Server) emitDecision(req *LookupReq, decision string, src *Record, waited int64) {
	rec := &Record{
		V: 1, TS: float64(time.Now().UnixMilli()) / 1000,
		Agent: req.Agent, Cmd: req.Cmd, CmdNorm: req.CmdNorm, CwdRel: req.CwdRel,
		TreeBefore: req.Tree, EnvFPBefore: req.EnvFP,
		Key: req.Key, Policy: req.Policy, Reason: req.Reason, Decision: decision,
	}
	if src != nil {
		// For a hit, duration_ms is the execution time that was deleted.
		rec.DurationMS = src.DurationMS
		rec.ExitCode = src.ExitCode
		rec.StdoutBlob = src.StdoutBlob
		rec.StderrBlob = src.StderrBlob
		rec.SourceAgent = src.Agent
	}
	if decision == DecisionMiss {
		return // the record command will log the real execution
	}
	if err := s.store.Append(rec); err != nil {
		log.Printf("hindsight: append failed: %v", err)
	}
	_ = waited
	s.broadcast(map[string]any{"type": "decision", "record": rec})
	s.broadcastStats()
}

// Stats is the counter the viewer renders.
type Stats struct {
	Served         int     `json:"served"`
	Executed       int     `json:"executed"`
	SecondsDeleted float64 `json:"seconds_deleted"`
	SecondsSpent   float64 `json:"seconds_spent"`
	Verified       int     `json:"verified"`
	Divergent      int     `json:"divergent"`
	Agents         int     `json:"agents"`
	Inflight       int     `json:"inflight"`
	UptimeS        float64 `json:"uptime_s"`
}

func (s *Server) stats() Stats {
	var st Stats
	agents := map[string]struct{}{}
	for _, r := range s.store.Records() {
		if r.Agent != "" {
			agents[r.Agent] = struct{}{}
		}
		switch r.Decision {
		case DecisionHit, DecisionLeaseWait:
			st.Served++
			st.SecondsDeleted += float64(r.DurationMS) / 1000
		case DecisionMiss:
			st.Executed++
			st.SecondsSpent += float64(r.DurationMS) / 1000
		case "VERIFY":
			if r.Verified != nil && *r.Verified {
				st.Verified++
			} else {
				st.Divergent++
			}
		}
	}
	st.Agents = len(agents)
	s.mu.Lock()
	st.Inflight = len(s.leases)
	s.mu.Unlock()
	st.UptimeS = time.Since(s.start).Seconds()
	return st
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.stats()) }

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 256)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	// Prime the connection so a viewer that attaches late still shows totals.
	// Same flat shape as every other stats event; a viewer should never have
	// to special-case the first one.
	if b, err := json.Marshal(statsEvent(s.stats())); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // a slow viewer must never block the cache
		}
	}
}

func statsEvent(st Stats) map[string]any {
	return map[string]any{
		"type": "stats", "served": st.Served, "executed": st.Executed,
		"seconds_deleted": st.SecondsDeleted, "seconds_spent": st.SecondsSpent,
		"verified": st.Verified, "divergent": st.Divergent,
		"agents": st.Agents, "inflight": st.Inflight,
	}
}

func (s *Server) broadcastStats() { s.broadcast(statsEvent(s.stats())) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
