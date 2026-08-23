package hp

import (
	"bytes"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		root, home string
		want       string
	}{
		{
			name: "root path",
			in:   "FAILED /Users/a/wt1/tests/test_x.py::test_ok",
			root: "/Users/a/wt1", home: "/home/other",
			want: "FAILED {{ROOT}}/tests/test_x.py::test_ok",
		},
		{
			name: "home path",
			in:   "cache at /Users/a/.cache/uv",
			root: "/nowhere", home: "/Users/a",
			want: "cache at {{HOME}}/.cache/uv",
		},
		{
			name: "every occurrence is replaced",
			in:   "/w/a.go /w/b.go /w/c.go",
			root: "/w", home: "",
			want: "{{ROOT}}/a.go {{ROOT}}/b.go {{ROOT}}/c.go",
		},
		{
			name: "var folders tmpdir",
			in:   "tmp=/var/folders/qz/9x7k/T/pytest-of-a done",
			want: "tmp={{TMP}} done",
		},
		{
			name: "random tmp path",
			in:   "wrote /tmp/pytest-of-a-1a2b3c4d/report.xml",
			want: "wrote {{TMP}}",
		},
		{
			name: "stable tmp path is kept",
			in:   "wrote /tmp/out",
			want: "wrote /tmp/out",
		},
		{
			name: "hex address",
			in:   "<object at 0x7fa1b2c3d4>",
			want: "<object at {{ADDR}}>",
		},
		{
			name: "short hex is not an address",
			in:   "mask 0x1f2e",
			want: "mask 0x1f2e",
		},
		{
			name: "durations",
			in:   "4.52s 1.2 ms 3.0 seconds 12.5 sec 0.01s",
			want: "{{DUR}} {{DUR}} {{DUR}} {{DUR}} {{DUR}}",
		},
		{
			name: "version numbers are not durations",
			in:   "python 3.12.1 and ruff 0.4.2",
			want: "python 3.12.1 and ruff 0.4.2",
		},
		{
			name: "pids",
			in:   "pid=1234 PID 5678 Pid=90",
			want: "{{PID}} {{PID}} {{PID}}",
		},
		{
			name: "empty input",
			in:   "", root: "/w", home: "/h",
			want: "",
		},
		{
			name: "nothing to scrub",
			in:   "3 passed", root: "/w", home: "/h",
			want: "3 passed",
		},
		{
			name: "a whole pytest tail",
			in: "==== /Users/a/wt1/tests/test_x.py::test_ok PASSED in 4.52s (pid=8123) " +
				"tmp=/var/folders/qz/9x7k/T/pytest-0 addr=0x7fa1b2c3d4 home=/Users/a/.cache",
			root: "/Users/a/wt1", home: "/Users/a",
			want: "==== {{ROOT}}/tests/test_x.py::test_ok PASSED in {{DUR}} ({{PID}}) " +
				"tmp={{TMP}} addr={{ADDR}} home={{HOME}}/.cache",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Normalize([]byte(tc.in), tc.root, tc.home)); got != tc.want {
				t.Errorf("Normalize(%q, %q, %q)\n got %q\nwant %q", tc.in, tc.root, tc.home, got, tc.want)
			}
		})
	}
}

// A worktree lives under $HOME, so home is a prefix of root. Substituting the
// shorter one first would leave {{HOME}}/wt1, which root could never match.
func TestNormalizeRootHomeOverlap(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		root, home string
		want       string
	}{
		{
			name: "home is a prefix of root",
			in:   "/Users/a/wt1/main.go and /Users/a/.cache/x",
			root: "/Users/a/wt1", home: "/Users/a",
			want: "{{ROOT}}/main.go and {{HOME}}/.cache/x",
		},
		{
			name: "root is a prefix of home",
			in:   "/srv/main.go and /srv/home/a/x",
			root: "/srv", home: "/srv/home/a",
			want: "{{ROOT}}/main.go and {{HOME}}/x",
		},
		{
			name: "root equals home",
			in:   "/Users/a/main.go",
			root: "/Users/a", home: "/Users/a",
			want: "{{ROOT}}/main.go",
		},
		{
			// Ordering also matters against the tmp pattern: a worktree cut
			// under /tmp must read as the root, not as scratch space.
			name: "root under tmp is the root",
			in:   "/tmp/hs-wt-1a2b3c4d/main.go",
			root: "/tmp/hs-wt-1a2b3c4d", home: "/Users/a",
			want: "{{ROOT}}/main.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Normalize([]byte(tc.in), tc.root, tc.home)); got != tc.want {
				t.Errorf("Normalize(%q, %q, %q)\n got %q\nwant %q", tc.in, tc.root, tc.home, got, tc.want)
			}
		})
	}
}

// An empty root or home must be skipped, not spliced in between every byte.
func TestNormalizeEmptyRootHome(t *testing.T) {
	const in = "ok /Users/a/wt1/main.go"
	cases := []struct {
		root, home, want string
	}{
		{"", "", "ok /Users/a/wt1/main.go"},
		{"/Users/a/wt1", "", "ok {{ROOT}}/main.go"},
		{"", "/Users/a", "ok {{HOME}}/wt1/main.go"},
	}
	for _, tc := range cases {
		got := string(Normalize([]byte(in), tc.root, tc.home))
		if got != tc.want {
			t.Errorf("Normalize(%q, %q, %q) = %q, want %q", in, tc.root, tc.home, got, tc.want)
		}
	}
	if got := string(Normalize(nil, "", "")); got != "" {
		t.Errorf("Normalize(nil, \"\", \"\") = %q, want empty", got)
	}
}

func TestNormalizeLeavesInputAlone(t *testing.T) {
	in := []byte("/w/main.go took 4.52s pid=99 at 0x7fa1b2c3d4")
	keep := append([]byte(nil), in...)
	out := Normalize(in, "/w", "/h")
	if !bytes.Equal(in, keep) {
		t.Errorf("Normalize modified its input: %q", in)
	}
	if bytes.Equal(out, in) {
		t.Error("Normalize returned the input unchanged, expected substitutions")
	}
}

// pytestWarningTail is the shape of output that breaks cross-worktree
// verification: pytest prints the warning's source location as an absolute
// path, so two correct runs of the same command in two worktrees produce
// byte-different output that differs only in the worktree prefix.
//
// Taken verbatim from demo-runs/20260823-154831, where nine of twelve
// shadow-verifications reported a divergence that was not one.
func pytestWarningTail(root string) []byte {
	return []byte("=============================== warnings summary ===============================\n" +
		"sympy/core/tests/test_arit.py:1699\n" +
		"  " + root + "/sympy/core/tests/test_arit.py:1699: PytestUnknownMarkWarning: " +
		"Unknown pytest.mark.thread_unsafe - is this a typo?\n" +
		"    @pytest.mark.thread_unsafe(\n\n" +
		"101 passed, 2 xfailed, 1 warning in 2.79s\n")
}

// A record produced in one worktree, verified in another.
//
// cmd/hindsight/verify.go normalizes both the freshly re-executed output and
// the recorded output with the VERIFIER's root. That is correct for the fresh
// output and wrong for the recorded output, which carries the producing
// agent's root and therefore never matches the string being substituted.
//
// This test pins that the defect is in the caller, not here: given the root
// the output was actually produced under, Normalize already agrees. There is
// no repair available inside Normalize, because its frozen signature accepts
// exactly one root and the recorded root is not recoverable from the bytes.
// The proposed one-line caller fix is written up in NORM_CROSS_WORKTREE.md.
func TestNormalizeCrossWorktreeRoots(t *testing.T) {
	const (
		producer = "/private/tmp/fleet-cached-20260823-154831/a3"
		verifier = "/private/tmp/fleet-cached-20260823-154831/verify"
		home     = "/Users/a"
	)
	recorded := pytestWarningTail(producer)
	fresh := pytestWarningTail(verifier)

	// What verify.go does today.
	wantN := Normalize(recorded, verifier, home)
	gotN := Normalize(fresh, verifier, home)
	if bytes.Equal(gotN, wantN) {
		t.Fatal("expected the verifier's root to mis-normalize the recorded output; " +
			"if this now passes, the caller was fixed and this test should assert equality")
	}

	// And why. The fresh output loses its prefix to {{ROOT}}; the recorded
	// output falls through to the temp-dir pattern, which swallows the whole
	// path tail and leaves the /private that the pattern is not anchored to.
	if !bytes.Contains(gotN, []byte("{{ROOT}}/sympy/core/tests/test_arit.py")) {
		t.Errorf("fresh output should carry {{ROOT}}, got:\n%s", gotN)
	}
	if !bytes.Contains(wantN, []byte("/private{{TMP}}")) {
		t.Errorf("recorded output should fall through to {{TMP}}, got:\n%s", wantN)
	}

	// Handed the root each blob was actually produced under, the same two
	// runs agree. This is the whole fix.
	if fixed, want := Normalize(fresh, verifier, home), Normalize(recorded, producer, home); !bytes.Equal(fixed, want) {
		t.Errorf("normalizing each blob against its own root should agree\n got %q\nwant %q", fixed, want)
	}
}

// The three commands in the demo split 1 clean / 2 divergent for a reason:
// output with no absolute path in it has nothing for the root substitution to
// get wrong, so it verifies cleanly even with the roots mismatched. Anything
// that prints a path does not. This is the control for the test above.
func TestNormalizeCrossWorktreeRelativeOutputIsUnaffected(t *testing.T) {
	const (
		producer = "/private/tmp/fleet-cached-20260823-154831/a3"
		verifier = "/private/tmp/fleet-cached-20260823-154831/verify"
	)
	out := []byte("................................x....................................... [ 64%]\n" +
		"..................x....................                                  [100%]\n\n" +
		"109 passed, 1 deselected, 2 xfailed in 1.81s\n")
	if !bytes.Equal(Normalize(out, verifier, ""), Normalize(out, producer, "")) {
		t.Error("output with no absolute paths must normalize identically under any root")
	}
}

// Output is bytes, not text. Invalid UTF-8 must survive the round trip.
func TestNormalizeBinarySafe(t *testing.T) {
	in := []byte{0x00, 0xff, 0xfe, '/', 'w', '/', 'a', 0x80}
	want := []byte{0x00, 0xff, 0xfe, '{', '{', 'R', 'O', 'O', 'T', '}', '}', '/', 'a', 0x80}
	if got := Normalize(in, "/w", ""); !bytes.Equal(got, want) {
		t.Errorf("Normalize(% x) = % x, want % x", in, got, want)
	}
}
