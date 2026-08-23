package hp

import (
	"net/http"
	"sort"
	"time"
)

// The daemon already knows where every agent stands. It has to: a lease says
// which agent is executing what right now, and every lookup carries that
// agent's current tree hash. Built for single-flight, that same state answers
// a different question — is this fan-out actually exploring?
//
// Two failure modes matter and both are computable from it.
//
// Collapse is when agents share a tree hash. Five agents at one tree is five
// agents' cost buying one agent's search. We measured exactly this and only
// noticed by reading a log afterwards: every agent in a live run produced a
// byte-identical fix, so they converged to one state.
//
// Divergence is the inverse — no two agents anywhere near each other, so
// nothing they produce is comparable and the cache never fires.
//
// This is reported to whoever is running the fleet, never fed back to the
// agents. Telling an agent what its peers are doing makes it inherit their
// decisions, which collapses the search the fan-out was paying for. The same
// tree hash that decides whether a result can be shared decides whether two
// agents are in the same place; only the first of those belongs in an agent's
// context.

// AgentState is where one agent stands.
type AgentState struct {
	Agent    string `json:"agent"`
	Tree     string `json:"tree"`
	LastCmd  string `json:"last_cmd"`
	Commands int    `json:"commands"`
	Served   int    `json:"served"`
	Executed int    `json:"executed"`
	// InFlight is the command this agent currently holds a lease on, if any.
	InFlight string `json:"in_flight,omitempty"`
	// Peers are the other agents sharing this agent's tree right now.
	Peers    []string `json:"peers"`
	LastSeen float64  `json:"last_seen"`
}

// FleetView is the whole fan-out at one instant.
type FleetView struct {
	Agents []AgentState `json:"agents"`
	// Clusters groups agents by tree. One cluster containing everyone means
	// the fleet has collapsed; as many clusters as agents means it has fully
	// diverged. Neither extreme is what you want from a fan-out.
	Clusters   [][]string `json:"clusters"`
	Converged  bool       `json:"converged"`
	FullyApart bool       `json:"fully_apart"`
	Assessment string     `json:"assessment"`
}

// Fleet reconstructs per-agent position from the log and the lease table.
func (s *Server) Fleet() FleetView {
	latest := map[string]*AgentState{}
	order := []string{}

	for _, r := range s.store.Records() {
		if r.Agent == "" || r.Agent == "verifier" {
			continue
		}
		st, ok := latest[r.Agent]
		if !ok {
			st = &AgentState{Agent: r.Agent}
			latest[r.Agent] = st
			order = append(order, r.Agent)
		}
		st.Commands++
		switch r.Decision {
		case DecisionHit, DecisionLeaseWait:
			st.Served++
		case DecisionMiss:
			st.Executed++
		}
		// Records arrive in log order, so the last one wins.
		if r.TreeBefore != "" {
			st.Tree = r.TreeBefore
		}
		if r.Cmd != "" {
			st.LastCmd = r.Cmd
		}
		if r.TS > st.LastSeen {
			st.LastSeen = r.TS
		}
	}

	s.mu.Lock()
	for _, l := range s.leases {
		if st, ok := latest[l.agent]; ok {
			st.InFlight = st.LastCmd
			_ = l
		}
	}
	s.mu.Unlock()

	byTree := map[string][]string{}
	for _, a := range order {
		st := latest[a]
		if st.Tree != "" {
			byTree[st.Tree] = append(byTree[st.Tree], a)
		}
	}
	for _, a := range order {
		st := latest[a]
		for _, peer := range byTree[st.Tree] {
			if peer != a {
				st.Peers = append(st.Peers, peer)
			}
		}
	}

	view := FleetView{}
	for _, a := range order {
		view.Agents = append(view.Agents, *latest[a])
	}
	for _, group := range byTree {
		sort.Strings(group)
		view.Clusters = append(view.Clusters, group)
	}
	sort.Slice(view.Clusters, func(i, j int) bool { return len(view.Clusters[i]) > len(view.Clusters[j]) })

	n := len(view.Agents)
	switch {
	case n < 2:
		view.Assessment = "not a fan-out"
	case len(view.Clusters) == 1:
		view.Converged = true
		view.Assessment = "collapsed: every agent is at the same state, so this fan-out " +
			"is paying N times for one search"
	case len(view.Clusters) == n:
		view.FullyApart = true
		view.Assessment = "fully diverged: no two agents share a state, so nothing can be " +
			"shared and results are not comparable"
	default:
		view.Assessment = "exploring: " + plural(len(view.Clusters), "distinct state") +
			" across " + plural(n, "agent")
	}
	return view
}

func plural(n int, noun string) string {
	s := noun
	if n != 1 {
		s += "s"
	}
	return itoa(n) + " " + s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	view := s.Fleet()
	s.broadcast(map[string]any{"type": "fleet", "fleet": view, "at": time.Now().Unix()})
	writeJSON(w, view)
}
