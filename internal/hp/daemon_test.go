package hp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func post(t *testing.T, url string, in, out any) {
	t.Helper()
	body, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSingleFlightCoalesces is the test for the behaviour that makes a cold
// fan-out worth anything.
//
// Five agents launched at once ask for the same command at the same state.
// Exactly one may be told to execute; the rest must block until that one
// finishes and then be served its result. Without this the highest-overlap
// stretch of a run, where every agent is still in lockstep, produces no hits
// at all because nobody has finished yet.
func TestSingleFlightCoalesces(t *testing.T) {
	srv, ts := newTestServer(t)
	const key = "hs-v1:coalesce"
	const n = 5

	req := LookupReq{Key: key, Cmd: "pytest -q", Policy: "SERVE", Serve: true}

	// The first caller takes the lease.
	var first LookupResp
	req.Agent = "a1"
	post(t, ts.URL+"/lookup", req, &first)
	if first.Decision != DecisionMiss {
		t.Fatalf("first caller should miss and take the lease, got %s", first.Decision)
	}

	// Peers arrive while the lease is held and must block, not duplicate.
	var wg sync.WaitGroup
	decisions := make([]string, n-1)
	sources := make([]string, n-1)
	for i := 0; i < n-1; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := req
			r.Agent = "peer"
			var resp LookupResp
			post(t, ts.URL+"/lookup", r, &resp)
			decisions[i] = resp.Decision
			sources[i] = resp.SourceAgent
		}(i)
	}

	// Give the peers time to actually be blocked rather than racing ahead.
	time.Sleep(150 * time.Millisecond)
	if got := srv.stats().Inflight; got != 1 {
		t.Fatalf("expected exactly 1 in-flight lease, got %d", got)
	}

	// The lease holder finishes and publishes its result.
	blob, err := srv.store.PutBlob([]byte("42\n"))
	if err != nil {
		t.Fatal(err)
	}
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: key, Cmd: "pytest -q", Decision: DecisionMiss,
		Servable: true, ExitCode: 0, DurationMS: 9000,
		StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	wg.Wait()
	for i, d := range decisions {
		if d != DecisionHit {
			t.Fatalf("peer %d got %s; every blocked peer must be served the holder's result", i, d)
		}
		if sources[i] != "a1" {
			t.Fatalf("peer %d served from %q, expected a1", i, sources[i])
		}
	}

	st := srv.stats()
	if st.Inflight != 0 {
		t.Fatalf("lease should be released after recording, %d still held", st.Inflight)
	}
	if st.Served != n-1 {
		t.Fatalf("expected %d served, got %d", n-1, st.Served)
	}
	// Four peers each avoided a nine-second execution.
	if st.SecondsDeleted != 36 {
		t.Fatalf("expected 36s deleted, got %v", st.SecondsDeleted)
	}
}

// TestBaselineArmNeverServes: the control arm must be instrumented identically
// and differ only in whether hits are served. A control measured differently
// from the treatment is not a control.
func TestBaselineArmNeverServes(t *testing.T) {
	srv, ts := newTestServer(t)
	const key = "hs-v1:baseline"

	blob, _ := srv.store.PutBlob([]byte("cached\n"))
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: key, Decision: DecisionMiss, Servable: true,
		DurationMS: 5000, StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	var served, baseline LookupResp
	post(t, ts.URL+"/lookup", LookupReq{Key: key, Agent: "a2", Serve: true}, &served)
	if served.Decision != DecisionHit {
		t.Fatalf("treatment arm should hit, got %s", served.Decision)
	}
	post(t, ts.URL+"/lookup", LookupReq{Key: key, Agent: "a3", Serve: false}, &baseline)
	if baseline.Decision != DecisionMiss {
		t.Fatalf("baseline arm must never be served, got %s", baseline.Decision)
	}
}

// TestVerifyEvictsOnDivergence: a divergent result must stop being served and
// must be counted loudly.
func TestVerifyEvictsOnDivergence(t *testing.T) {
	srv, ts := newTestServer(t)
	const key = "hs-v1:diverge"

	blob, _ := srv.store.PutBlob([]byte("out\n"))
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: key, Decision: DecisionMiss, Servable: true,
		DurationMS: 100, StdoutBlob: blob, StderrBlob: blob,
	}, nil)
	if _, ok := srv.store.Lookup(key); !ok {
		t.Fatal("record should be servable before verification")
	}

	post(t, ts.URL+"/verify", VerifyVerdict{Key: key, OK: false, Detail: "stdout diverged"}, nil)

	if _, ok := srv.store.Lookup(key); ok {
		t.Fatal("a divergent record must be evicted, not kept")
	}
	if got := srv.stats().Divergent; got != 1 {
		t.Fatalf("divergence must be counted, got %d", got)
	}
}

// TestEventsStreamShape pins the SSE contract the viewer depends on.
func TestEventsStreamShape(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("wrong content type %q", ct)
	}

	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	line := string(buf[:n])
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("expected an SSE data frame, got %q", line)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line[6:])), &ev); err != nil {
		t.Fatalf("priming event is not valid JSON: %v", err)
	}
	if ev["type"] != "stats" {
		t.Fatalf("expected a priming stats event, got %v", ev["type"])
	}
	// Flat, not nested. A viewer must not have to special-case the first event.
	for _, k := range []string{"served", "executed", "seconds_deleted", "divergent"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("stats event missing %q; shape is %v", k, ev)
		}
	}
}

// TestLookupFailsSoftOnGarbage: a malformed request must degrade to a miss,
// which means normal execution, rather than an error the agent sees.
func TestLookupFailsSoftOnGarbage(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/lookup", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out LookupResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Decision != DecisionMiss {
		t.Fatalf("garbage should yield a miss, got %s", out.Decision)
	}
}
