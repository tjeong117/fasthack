package hp

import "testing"

func fleetServer(t *testing.T) *Server {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store)
}

func at(t *testing.T, s *Server, agent, tree, cmd string, decision string) {
	t.Helper()
	if err := s.store.Append(&Record{
		V: 1, Agent: agent, TreeBefore: tree, Cmd: cmd, CmdNorm: cmd,
		Decision: decision, Key: "k-" + tree + "-" + cmd,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFleetDetectsCollapse. Five agents at one tree is five agents' cost
// buying one agent's search. It happened in a real run — every agent produced
// a byte-identical fix — and was only noticed by reading a log afterwards.
func TestFleetDetectsCollapse(t *testing.T) {
	s := fleetServer(t)
	for _, a := range []string{"a1", "a2", "a3", "a4", "a5"} {
		at(t, s, a, "tree-same", "pytest -q", DecisionMiss)
	}
	v := s.Fleet()
	if !v.Converged {
		t.Fatalf("five agents at one tree is a collapse; assessment was %q", v.Assessment)
	}
	if len(v.Clusters) != 1 || len(v.Clusters[0]) != 5 {
		t.Fatalf("expected one cluster of five, got %v", v.Clusters)
	}
	for _, a := range v.Agents {
		if len(a.Peers) != 4 {
			t.Fatalf("%s should see four peers at its state, saw %v", a.Agent, a.Peers)
		}
	}
}

// TestFleetDetectsFullDivergence: the opposite failure. Nothing can be shared
// and nothing is comparable.
func TestFleetDetectsFullDivergence(t *testing.T) {
	s := fleetServer(t)
	for _, a := range []string{"a1", "a2", "a3"} {
		at(t, s, a, "tree-"+a, "pytest -q", DecisionMiss)
	}
	v := s.Fleet()
	if !v.FullyApart {
		t.Fatalf("three agents at three trees is full divergence; got %q", v.Assessment)
	}
	for _, a := range v.Agents {
		if len(a.Peers) != 0 {
			t.Fatalf("%s should have no peers, has %v", a.Agent, a.Peers)
		}
	}
}

// TestFleetReportsPartialExploration is the healthy middle: some agents
// together, some apart.
func TestFleetReportsPartialExploration(t *testing.T) {
	s := fleetServer(t)
	at(t, s, "a1", "tree-x", "pytest", DecisionMiss)
	at(t, s, "a2", "tree-x", "pytest", DecisionHit)
	at(t, s, "a3", "tree-y", "pytest", DecisionMiss)
	v := s.Fleet()
	if v.Converged || v.FullyApart {
		t.Fatalf("this fleet is exploring, not collapsed or apart: %q", v.Assessment)
	}
	if len(v.Clusters) != 2 {
		t.Fatalf("expected two clusters, got %v", v.Clusters)
	}
	// The larger cluster sorts first so a reader sees the biggest group.
	if len(v.Clusters[0]) != 2 {
		t.Fatalf("clusters should be ordered largest first, got %v", v.Clusters)
	}
}

// TestFleetTracksTheLatestPositionOnly: an agent that moved is where it is
// now, not where it has been.
func TestFleetTracksTheLatestPositionOnly(t *testing.T) {
	s := fleetServer(t)
	at(t, s, "a1", "tree-old", "ls", DecisionMiss)
	at(t, s, "a1", "tree-new", "pytest", DecisionMiss)
	at(t, s, "a2", "tree-new", "pytest", DecisionHit)

	v := s.Fleet()
	if len(v.Agents) != 2 {
		t.Fatalf("expected two agents, got %d", len(v.Agents))
	}
	for _, a := range v.Agents {
		if a.Agent == "a1" {
			if a.Tree != "tree-new" {
				t.Fatalf("a1 moved to tree-new, fleet says %s", a.Tree)
			}
			if a.Commands != 2 || a.Executed != 2 {
				t.Fatalf("a1 ran two commands, fleet says %d (%d executed)", a.Commands, a.Executed)
			}
		}
	}
	if !v.Converged {
		t.Fatalf("both agents ended at tree-new, so this is a collapse: %q", v.Assessment)
	}
}

// TestFleetIgnoresTheVerifier: shadow verification runs under its own agent
// id and is not a member of the fan-out.
func TestFleetIgnoresTheVerifier(t *testing.T) {
	s := fleetServer(t)
	at(t, s, "a1", "tree-x", "pytest", DecisionMiss)
	at(t, s, "verifier", "tree-x", "pytest", "VERIFY")
	v := s.Fleet()
	for _, a := range v.Agents {
		if a.Agent == "verifier" {
			t.Fatal("the verifier is not an agent in the fan-out and must not appear in the map")
		}
	}
	if len(v.Agents) != 1 {
		t.Fatalf("expected one real agent, got %d", len(v.Agents))
	}
}
