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

// Output is bytes, not text. Invalid UTF-8 must survive the round trip.
func TestNormalizeBinarySafe(t *testing.T) {
	in := []byte{0x00, 0xff, 0xfe, '/', 'w', '/', 'a', 0x80}
	want := []byte{0x00, 0xff, 0xfe, '{', '{', 'R', 'O', 'O', 'T', '}', '}', '/', 'a', 0x80}
	if got := Normalize(in, "/w", ""); !bytes.Equal(got, want) {
		t.Errorf("Normalize(% x) = % x, want % x", in, got, want)
	}
}
