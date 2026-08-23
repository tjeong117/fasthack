package hp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tier-2 promotion: the same disjointness argument as scope.go, made against a
// measured read set instead of the command line.
//
// The tests below are mostly refusals, which is the correct ratio. The one
// promotion is the whole point of the feature and every refusal is a way the
// promotion could have been a wrong answer instead.

// readSetOf builds a set that is valid in every respect except its contents,
// so that a test asserting one refusal is not accidentally passing because of
// another.
func readSetOf(paths ...string) *ReadSet {
	return &ReadSet{
		Method:    ReadSetPythonImports,
		Policy:    SERVE.String(),
		Tool:      "pytest",
		Processes: 1,
		Paths:     paths,
		TestGlobs: []string{"test_*.py", "*_test.py"},
	}
}

// pyRepoSeed is a small but honest Python project: a package under test, a
// test that imports one module of it, a module the test does not touch, and a
// data file the read set structurally cannot see.
func pyRepoSeed() map[string]string {
	return map[string]string{
		".gitignore":            ".venv/\n__pycache__/\n.pytest_cache/\n",
		"conftest.py":           "",
		"src/__init__.py":       "",
		"src/billing.py":        "def total():\n    return 1\n",
		"src/auth.py":           "def login():\n    return True\n",
		"tests/data/rates.json": "{\"vat\": 20}\n",
		"tests/test_billing.py": "from src.billing import total\n\n\n" +
			"def test_total():\n    assert total() == 1\n",
	}
}

// The observed set for pyRepoSeed, as a real pytest run reports it. The
// end-to-end test below asserts that this is what the plugin actually
// produces, so the unit tests are working from a measurement rather than a
// convenient fiction.
func pyObservedSet() *ReadSet {
	return readSetOf("conftest.py", "src/__init__.py", "src/billing.py", "tests/test_billing.py")
}

// TestReadSetPromotesPeerEditOutsideTheObservedSet is the case the feature
// exists for, and the case scope.go cannot decide.
//
// A peer edited src/auth.py. Our recorded pytest run never imported it, which
// we know because sys.modules would have said so. Nothing else moved, so the
// recorded result is still the result.
func TestReadSetPromotesPeerEditOutsideTheObservedSet(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login():\n    return False\n")
	})

	d := ScopeMatchObserved(root, recorded, current, pyObservedSet())
	scopeCheck(t, d, true)
	if len(d.ChangedPaths) != 1 || d.ChangedPaths[0] != "src/auth.py" {
		t.Fatalf("expected the diff to be exactly src/auth.py, got %v", d.ChangedPaths)
	}
}

// TestReadSetRefusesPeerEditInsideTheObservedSet is the same shape with the
// edit moved one file across. This is the boundary the whole design turns on.
func TestReadSetRefusesPeerEditInsideTheObservedSet(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "src/billing.py", "def total():\n    return 999\n")
	})

	d := ScopeMatchObserved(root, recorded, current, pyObservedSet())
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "src/billing.py") {
		t.Fatalf("the refusal must name the file that collided: %q", d.Reason)
	}
}

// TestReadSetAcrossAnImportEdge is scope_import_test.go, re-run against an
// observed set.
//
// That test documents why Tier-1 had to be crippled: pytest tests/test_billing.py
// reads src/billing.py through an import, the path appears nowhere in argv, so
// argv-based disjointness serves a stale pass for changed code. An observed
// set gets both directions right for the same diff, which is the entire claim
// of this file.
func TestReadSetAcrossAnImportEdge(t *testing.T) {
	const cmd = "pytest tests/test_billing.py"
	seed := map[string]string{
		"src/billing.py": "def total(): return 1\n",
		"src/auth.py":    "def login(): return True\n",
		"tests/test_billing.py": "from src.billing import total\n" +
			"def test_total(): assert total() == 1\n",
	}

	t.Run("changed module is in the read set: refuse", func(t *testing.T) {
		root, recorded, current := scopeRepo(t, seed, func(root string) {
			scopeWrite(t, root, "src/billing.py", "def total(): return 999\n")
		})
		// Tier 1 refuses this one too, but only by refusing pytest outright.
		scopeCheck(t, ScopeMatch(root, recorded, current, cmd), false)
		scopeCheck(t, ScopeMatchObserved(root, recorded, current,
			readSetOf("src/billing.py", "tests/test_billing.py")), false)
	})

	t.Run("changed module is not in the read set: promote", func(t *testing.T) {
		root, recorded, current := scopeRepo(t, seed, func(root string) {
			scopeWrite(t, root, "src/auth.py", "def login(): return False\n")
		})
		// Tier 1 still refuses, and this is the hit it is leaving on the table:
		// it cannot tell this diff from the one above.
		d1 := ScopeMatch(root, recorded, current, cmd)
		scopeCheck(t, d1, false)
		if !strings.Contains(d1.Reason, "pytest") {
			t.Fatalf("expected tier 1 to refuse on the command head, got %q", d1.Reason)
		}
		scopeCheck(t, ScopeMatchObserved(root, recorded, current,
			readSetOf("src/billing.py", "tests/test_billing.py")), true)
	})
}

// TestReadSetRefusesAnAddedConftest is part 3: the addition the set is
// structurally blind to.
//
// conftest.py is not in the read set and cannot be, because it did not exist
// when the run was observed. Nothing imports it; pytest finds it by name. So
// "absent from the read set" carries no information whatsoever here, and
// disjointness would be a lie.
func TestReadSetRefusesAnAddedConftest(t *testing.T) {
	cases := []struct{ name, added string }{
		// In a directory the run read from, and in one it did not. Both refuse,
		// and the second is the one that proves the name rule is doing the work
		// rather than the directory rule.
		{"beside the tests", "tests/conftest.py"},
		{"somewhere else entirely", "other/pkg/conftest.py"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
				scopeWrite(t, root, tc.added, "import pytest\n")
			})
			d := ScopeMatchObserved(root, recorded, current, pyObservedSet())
			scopeCheck(t, d, false)
			if !strings.Contains(d.Reason, tc.added) {
				t.Fatalf("the refusal must name the added file: %q", d.Reason)
			}
			if !strings.Contains(d.Reason, "by name rather than by reference") {
				t.Fatalf("expected the auto-discovery argument, got %q", d.Reason)
			}
		})
	}
}

// TestReadSetRefusesAnAddedTestFile uses the project's own discovery patterns
// rather than a guess at them. A new test_*.py means the next run collects a
// strictly larger suite than the one we recorded.
func TestReadSetRefusesAnAddedTestFile(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "tests/test_refunds.py", "def test_refund():\n    assert True\n")
	})
	d := ScopeMatchObserved(root, recorded, current, pyObservedSet())
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "tests/test_refunds.py") {
		t.Fatalf("the refusal must name the added test: %q", d.Reason)
	}
}

// TestReadSetRefusesAnAddedFileInADirectoryTheRunRead covers the addition that
// no name rule catches: a new fixture under a directory the run collected
// tests from. Collection walks that subtree, and a test that globs its own
// data directory reads what the walk turns up; sys.modules sees neither.
func TestReadSetRefusesAnAddedFileInADirectoryTheRunRead(t *testing.T) {
	// A read set with no root-level file, so the repo root is not implicated
	// and the directory rule has to fire on tests/ specifically.
	rs := readSetOf("src/billing.py", "tests/test_billing.py")
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "tests/data/refunds.json", "{\"rate\": 1}\n")
	})
	d := ScopeMatchObserved(root, recorded, current, rs)
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "tests/data/refunds.json") || !strings.Contains(d.Reason, "tests") {
		t.Fatalf("expected the directory-containment argument, got %q", d.Reason)
	}
}

// TestReadSetAdditions is the boundary between an addition the recorded run
// can be shown not to reach and one it cannot.
//
// The argument for every promotion below is the same, and it is the one this
// file's addition rule now rests on: getting here at all requires that no file
// in the read set changed, so the import graph is byte-identical to the one
// sys.modules reported, and an unchanged graph cannot reach a file that did
// not exist when it was measured. The refusals are the ways a file is read
// without being imported -- scanned out of a directory, or resolved by name
// against the import search path.
//
// The rule this replaced refused on every added .py and on anything sharing a
// directory with anything the run read. pyRepoSeed has a root-level
// conftest.py, which made the repo root a read directory, which refused every
// addition anywhere in the tree. Half the rows below are that bug.
func TestReadSetAdditions(t *testing.T) {
	// A read set with no root-level entry, for the rows that need the repo
	// root not to be implicated by conftest.py.
	nested := readSetOf("src/billing.py", "tests/test_billing.py")

	cases := []struct {
		name    string
		added   map[string]string
		rs      *ReadSet
		promote bool
		want    string
	}{{
		name:    "a document nothing could import",
		added:   map[string]string{"README.md": "# notes\n"},
		rs:      pyObservedSet(),
		promote: true,
	}, {
		// The row the feature is worth having for. Agents add modules
		// constantly, and src/ is a package: the addition creates the name
		// src.newmodule, which no unchanged import statement mentions.
		name:    "a new module in a package nothing imports",
		added:   map[string]string{"src/newmodule.py": "def f():\n    return 1\n"},
		rs:      pyObservedSet(),
		promote: true,
	}, {
		name:    "a new fixture in a directory unrelated to the tests",
		added:   map[string]string{"assets/logo.svg": "<svg/>\n"},
		rs:      pyObservedSet(),
		promote: true,
	}, {
		// Not promotable, and not because of the addition: a file inside the
		// read set changed. This is the precondition the whole loosening
		// argument depends on, so it gets a row of its own.
		name: "an unreachable addition alongside an edit inside the read set",
		added: map[string]string{
			"src/newmodule.py": "def f():\n    return 1\n",
			"src/billing.py":   "def total():\n    return 999\n",
		},
		rs:      pyObservedSet(),
		promote: false,
		want:    "src/billing.py",
	}, {
		name:    "a conftest in a directory the run never touched",
		added:   map[string]string{"other/pkg/conftest.py": "import pytest\n"},
		rs:      pyObservedSet(),
		promote: false,
		want:    "by name rather than by reference",
	}, {
		name:    "an __init__.py that changes package resolution",
		added:   map[string]string{"other/__init__.py": ""},
		rs:      pyObservedSet(),
		promote: false,
		want:    "by name rather than by reference",
	}, {
		name:    "a .pth file the interpreter executes at startup",
		added:   map[string]string{"vendor/inject.pth": "import vendor\n"},
		rs:      pyObservedSet(),
		promote: false,
		want:    "by name rather than by reference",
	}, {
		// The default globs would not collect this one. The project's own
		// configuration does, and the plugin reports it, so we use it.
		name:    "a file matching the project's own discovery pattern",
		added:   map[string]string{"src/check_refunds.py": "def test_x():\n    assert True\n"},
		rs:      withGlobs(pyObservedSet(), "check_*.py"),
		promote: false,
		want:    "discovery pattern",
	}, {
		name:    "a plain file under a directory the run collected tests from",
		added:   map[string]string{"tests/data/refunds.json": "{}\n"},
		rs:      nested,
		promote: false,
		want:    "collected tests from",
	}, {
		// The case the loosening argument does not cover. `import json`
		// resolved to the stdlib when we watched the run; the repo root is on
		// sys.path, so afterwards it resolves here.
		name:    "a top-level module that can shadow an installed one",
		added:   map[string]string{"json.py": "VALUE = 1\n"},
		rs:      nested,
		promote: false,
		want:    "import search path",
	}, {
		// Same file name, one directory down, in a directory nothing in the
		// read set was imported from. It is not on any search path we can see,
		// so it shadows nothing and is unreachable from an unchanged graph.
		name:    "the same name where it is not on a search path",
		added:   map[string]string{"vendor/json.py": "VALUE = 1\n"},
		rs:      nested,
		promote: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
				for rel, body := range tc.added {
					scopeWrite(t, root, rel, body)
				}
			})
			d := ScopeMatchObserved(root, recorded, current, tc.rs)
			scopeCheck(t, d, tc.promote)
			if tc.want != "" && !strings.Contains(d.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", d.Reason, tc.want)
			}
			if !tc.promote {
				return
			}
			for rel := range tc.added {
				if !strings.Contains(strings.Join(d.ChangedPaths, " "), rel) {
					t.Fatalf("the promotion did not see %s in the diff: %v", rel, d.ChangedPaths)
				}
			}
		})
	}
}

func withGlobs(rs *ReadSet, globs ...string) *ReadSet {
	out := *rs
	out.TestGlobs = globs
	return &out
}

// TestReadSetRefusesAChangedPathTheMethodCannotSee is the second half of part
// 3, and the reason ReadSet carries a method at all.
//
// The changed file is disjoint from the read set by inspection. It is still
// unsafe, because a test that does open("tests/data/rates.json") never puts
// that path into sys.modules. Absence from the set is only evidence when the
// method would have reported a presence.
func TestReadSetRefusesAChangedPathTheMethodCannotSee(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "tests/data/rates.json", "{\"vat\": 25}\n")
	})
	d := ScopeMatchObserved(root, recorded, current, pyObservedSet())
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "proves nothing") {
		t.Fatalf("expected the unobservable-kind argument, got %q", d.Reason)
	}
}

// TestReadSetRefusesAChangedToolchainFile: a changed pyproject.toml re-points
// testpaths and addopts without anything importing it.
func TestReadSetRefusesAChangedToolchainFile(t *testing.T) {
	seed := pyRepoSeed()
	seed["pyproject.toml"] = "[tool.pytest.ini_options]\naddopts = \"-q\"\n"
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "pyproject.toml", "[tool.pytest.ini_options]\naddopts = \"-v\"\n")
	})
	scopeCheck(t, ScopeMatchObserved(root, recorded, current, pyObservedSet()), false)
}

// TestReadSetRefusesWithoutAUsableSet covers every way the set itself can fail
// to license a promotion. All of them are cheap, and all of them cost a hit
// rather than correctness.
func TestReadSetRefusesWithoutAUsableSet(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login():\n    return False\n")
	})
	// Precondition: this exact diff promotes with a good set, so each failure
	// below is attributable to the set and not to the diff.
	scopeCheck(t, ScopeMatchObserved(root, recorded, current, pyObservedSet()), true)

	noPolicy := pyObservedSet()
	noPolicy.Policy = ""
	recordOnly := pyObservedSet()
	recordOnly.Policy = RECORD_ONLY.String()
	unknownMethod := pyObservedSet()
	unknownMethod.Method = "strace/openat"

	cases := []struct {
		name string
		rs   *ReadSet
		want string
	}{
		{"never captured", nil, "no read set was captured"},
		{"captured but empty", readSetOf(), "the read set is empty"},
		{"policy not recorded", noPolicy, "unrecorded"},
		{"policy is RECORD_ONLY", recordOnly, "RECORD_ONLY"},
		{"method this binary does not know", unknownMethod, "unknown method"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ScopeMatchObserved(root, recorded, current, tc.rs)
			scopeCheck(t, d, false)
			if !strings.Contains(d.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", d.Reason, tc.want)
			}
		})
	}
}

// TestReadSetRefusesMalformedInputs guards the arguments handed to git and the
// case tier 0 already owns.
func TestReadSetRefusesMalformedInputs(t *testing.T) {
	root, recorded, current := scopeRepo(t, pyRepoSeed(), func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login():\n    return False\n")
	})
	rs := pyObservedSet()

	scopeCheck(t, ScopeMatchObserved("", recorded, current, rs), false)
	scopeCheck(t, ScopeMatchObserved(root, recorded, recorded, rs), false)
	scopeCheck(t, ScopeMatchObserved(root, "--output=/tmp/pwned", current, rs), false)
	scopeCheck(t, ScopeMatchObserved(root, recorded, "not-a-tree", rs), false)
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

func readSetJSONLine(t *testing.T, root string, mutate func(m map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"v": 1, "method": ReadSetPythonImports, "tool": "pytest", "pid": 4711,
		"root": root, "rootdir": root, "exit_status": 0, "complete": true,
		"test_globs": []string{"test_*.py", "*_test.py"},
		"paths":      []string{"src/billing.py", "tests/test_billing.py"},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// TestReadSetCaptureUnionsProcessesAndCleansUp: one line per Python process,
// so xdist workers and a chained command union rather than overwrite. The file
// must not survive, or the next command collects this command's observations.
func TestReadSetCaptureUnionsProcessesAndCleansUp(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "rs.jsonl")
	t.Setenv(readSetOutOverride, out)

	body := readSetJSONLine(t, root, nil) +
		readSetJSONLine(t, root, func(m map[string]any) {
			m["pid"] = 4712
			m["paths"] = []string{"src/billing.py", "src/tax.py"}
		})
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	rs, ok := CaptureReadSet(root)
	if !ok {
		t.Fatal("a well-formed capture must be collected")
	}
	want := []string{"src/billing.py", "src/tax.py", "tests/test_billing.py"}
	if !scopeEqual(rs.Paths, want) {
		t.Fatalf("paths = %v, want the union %v", rs.Paths, want)
	}
	if rs.Processes != 2 {
		t.Fatalf("processes = %d, want 2", rs.Processes)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("the capture file must be removed; a survivor is read as the next command's read set")
	}
	if _, ok := CaptureReadSet(root); ok {
		t.Fatal("a second capture with no file must report nothing observed")
	}
}

// TestReadSetCaptureRefusesPartialObservations.
//
// Every case here is a set that might be missing a path, and a set missing a
// path is the one input that turns disjointness into a wrong answer. Refusing
// the whole capture costs a hit; salvaging the readable part does not.
func TestReadSetCaptureRefusesPartialObservations(t *testing.T) {
	root := t.TempDir()
	good := readSetJSONLine(t, root, nil)

	cases := []struct{ name, body string }{
		{"no file content at all", ""},
		{"torn line from an interleaved append", good + `{"v":1,"method":"python/sys.mo`},
		{"self-reported incomplete", readSetJSONLine(t, root, func(m map[string]any) { m["complete"] = false })},
		{"newer schema version", readSetJSONLine(t, root, func(m map[string]any) { m["v"] = 2 })},
		{"unknown capture method", readSetJSONLine(t, root, func(m map[string]any) { m["method"] = "strace/openat" })},
		// pytest 2 is interrupted, 3 internal error, 4 usage error, 5 nothing
		// collected. None of them imported the whole graph.
		{"interrupted session", readSetJSONLine(t, root, func(m map[string]any) { m["exit_status"] = 2 })},
		{"usage error", readSetJSONLine(t, root, func(m map[string]any) { m["exit_status"] = 4 })},
		{"no tests collected", readSetJSONLine(t, root, func(m map[string]any) { m["exit_status"] = 5 })},
		{"stale capture from another repo", readSetJSONLine(t, root, func(m map[string]any) { m["root"] = t.TempDir() })},
		{"absolute path", readSetJSONLine(t, root, func(m map[string]any) { m["paths"] = []string{"/etc/passwd"} })},
		{"path escaping the repo", readSetJSONLine(t, root, func(m map[string]any) { m["paths"] = []string{"../peer/x.py"} })},
		{"one bad line among good ones", good + readSetJSONLine(t, root, func(m map[string]any) { m["complete"] = false })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "rs.jsonl")
			t.Setenv(readSetOutOverride, out)
			if err := os.WriteFile(out, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if rs, ok := CaptureReadSet(root); ok {
				t.Fatalf("collected a partial observation: %+v", rs)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("a rejected capture file must still be removed")
			}
		})
	}

	// A tests-failed session is not a partial observation: pytest imports the
	// whole collected graph before it runs anything, so exit 1 is a complete
	// read set of a run that happened to be red.
	out := filepath.Join(t.TempDir(), "rs.jsonl")
	t.Setenv(readSetOutOverride, out)
	body := readSetJSONLine(t, root, func(m map[string]any) { m["exit_status"] = 1 })
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := CaptureReadSet(root); !ok {
		t.Fatal("a failing test run is a complete observation of a red run")
	}
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

// TestReadSetEnvArmsThePluginWithoutInstalling: no pip install, no entry point,
// no conftest edit, and nothing written inside the workspace.
func TestReadSetEnvArmsThePluginWithoutInstalling(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HP_HOME", home)
	t.Setenv("PYTHONPATH", "/preexisting/path")
	t.Setenv("PYTEST_ADDOPTS", "-q")
	t.Setenv(readSetOutOverride, "")

	env := map[string]string{}
	for _, kv := range ReadSetEnv(root) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if len(env) != 4 {
		t.Fatalf("expected exactly four additions, got %v", env)
	}

	pluginDir := filepath.Join(home, "pyplugin")
	if got := env["PYTHONPATH"]; got != pluginDir+string(os.PathListSeparator)+"/preexisting/path" {
		t.Fatalf("PYTHONPATH = %q; the plugin directory must be prepended and the existing value kept", got)
	}
	if got := env["PYTEST_ADDOPTS"]; got != "-q -p "+readSetPluginModule {
		t.Fatalf("PYTEST_ADDOPTS = %q; the existing value must be kept", got)
	}
	if env[readSetRootVar] != root {
		t.Fatalf("%s = %q, want %q", readSetRootVar, env[readSetRootVar], root)
	}
	if !strings.HasPrefix(env[readSetOutVar], home) {
		t.Fatalf("the capture must land under HP_HOME, outside the tree; got %q", env[readSetOutVar])
	}

	// The module must exist before the flag naming it does, because pytest
	// treats "-p <unimportable>" as a fatal usage error rather than a warning.
	b, err := os.ReadFile(filepath.Join(pluginDir, readSetPluginModule+".py"))
	if err != nil {
		t.Fatalf("plugin was not materialized: %v", err)
	}
	if string(b) != readSetPluginSource {
		t.Fatal("materialized plugin does not match the embedded source")
	}

	// Nothing may be written inside the workspace: a cache write inside the
	// tree changes the tree hash that keys the cache.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("arming wrote into the workspace: %v", entries)
	}
}

// TestReadSetEnvStaysSilentWhenItCannotArm. Emitting "-p hindsight_pytest_plugin"
// without the module on the path would break every Python command in the repo.
// Failing to arm has to mean emitting nothing, not emitting half of it.
func TestReadSetEnvStaysSilentWhenItCannotArm(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HP_HOME", filepath.Join(blocker, "home"))
	if env := ReadSetEnv(t.TempDir()); env != nil {
		t.Fatalf("armed against an unusable HP_HOME: %v", env)
	}
	before := os.Getenv("PYTHONPATH")
	ArmReadSet(t.TempDir())()
	if os.Getenv("PYTHONPATH") != before {
		t.Fatal("a failed arming must not leave PYTHONPATH modified")
	}
}

// TestReadSetArmingIsReversedBeforeTheAfterState is the regression test for the
// one way this feature could silently destroy the cache it is meant to
// improve.
//
// PYTHONPATH is in envAllow, so arming moves the environment fingerprint. The
// purity gate compares the fingerprint the hook measured before the command
// with the one hindsight record measures after it, and marks the record
// unservable when they differ. Arm and forget to disarm and every command in
// every repo becomes unservable -- no error, no warning, just a cache that
// never serves anything again.
func TestReadSetArmingIsReversedBeforeTheAfterState(t *testing.T) {
	root := newRepo(t)
	t.Setenv("HP_HOME", t.TempDir())
	t.Setenv(readSetOutOverride, "")
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	before, _ := ws.EnvFingerprint()
	disarm := ArmReadSet(root)
	armed, _ := ws.EnvFingerprint()
	disarm()
	after, _ := ws.EnvFingerprint()

	if armed == before {
		t.Skip("PYTHONPATH is no longer part of the environment fingerprint; " +
			"this test is guarding a hazard that no longer exists")
	}
	if after != before {
		t.Fatalf("disarming did not restore the environment fingerprint: %s -> %s -> %s\n"+
			"every record made by this process would fail the purity gate", before, armed, after)
	}
	if os.Getenv(readSetOutVar) != "" || os.Getenv(readSetRootVar) != "" {
		t.Fatal("disarming left the wrapper variables set; a later command would collect a stale capture")
	}
}

// TestReadSetPluginSourceMatchesScript keeps the readable copy and the copy
// that actually ships from drifting.
func TestReadSetPluginSourceMatchesScript(t *testing.T) {
	p := filepath.Join("..", "..", "scripts", readSetPluginModule+".py")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("%s not present in this checkout: %v", p, err)
	}
	if string(b) != readSetPluginSource {
		t.Fatalf("%s and the embedded readSetPluginSource have diverged; "+
			"re-splice the script into internal/hp/readset.go", p)
	}
}

// ---------------------------------------------------------------------------
// End to end, against a real pytest
// ---------------------------------------------------------------------------

// readSetPytest finds a pytest that actually runs. Skips rather than fails,
// because the Go side has to build and test on a machine with no Python at all.
//
// Each candidate is probed rather than trusted. A pytest on PATH can be
// unusable -- one broken entry-point plugin anywhere in its site-packages
// makes every invocation raise before it parses arguments -- and that is a
// property of the machine, not of this feature.
func readSetPytest(t *testing.T) []string {
	t.Helper()
	var candidates [][]string
	if p := os.Getenv("HINDSIGHT_TEST_PYTEST"); p != "" {
		candidates = append(candidates, []string{p})
	}
	if p, err := exec.LookPath("pytest"); err == nil {
		candidates = append(candidates, []string{p})
	}
	if p, err := exec.LookPath("python3"); err == nil {
		candidates = append(candidates, []string{p, "-m", "pytest"})
	}
	for _, c := range candidates {
		probe := exec.Command(c[0], append(append([]string{}, c[1:]...), "--version")...)
		if probe.Run() == nil {
			return c
		}
	}
	t.Skip("no working pytest available; set HINDSIGHT_TEST_PYTEST to one to run this")
	return nil
}

func runPytest(t *testing.T, root string, env []string, args ...string) (string, int) {
	t.Helper()
	argv := append(append([]string{}, readSetPytest(t)...), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v\n%s", argv, err, out)
	}
	return string(out), code
}

// TestReadSetEndToEndAgainstRealPytest is the demonstration, run as a test.
//
// A real repo, a real pytest run producing a real read set, a peer edit
// outside it, and ScopeMatchObserved promoting exactly where ScopeMatch
// refuses. Nothing here is stubbed except the peer.
func TestReadSetEndToEndAgainstRealPytest(t *testing.T) {
	readSetPytest(t)

	root := newRepo(t)
	for rel, body := range pyRepoSeed() {
		scopeWrite(t, root, rel, body)
	}
	t.Setenv("HP_HOME", t.TempDir())
	t.Setenv(readSetOutOverride, "")

	env := ReadSetEnv(root)
	if env == nil {
		t.Fatal("could not arm the plugin")
	}
	out, code := runPytest(t, root, env, "-q", "tests/test_billing.py")
	if code != 0 {
		t.Fatalf("fixture suite did not pass (exit %d):\n%s", code, out)
	}

	rs, ok := CaptureReadSet(root)
	if !ok {
		t.Fatalf("no read set captured from a successful pytest run\npytest said:\n%s", out)
	}
	rs.Policy = SERVE.String()

	// The plugin must report the import edge argv does not name, and must not
	// report the interpreter's own stdlib or the venv pytest came from.
	for _, want := range []string{"src/billing.py", "tests/test_billing.py", "conftest.py"} {
		if !readSetHas(rs, want) {
			t.Fatalf("observed set %v is missing %s", rs.Paths, want)
		}
	}
	if readSetHas(rs, "src/auth.py") {
		t.Fatalf("observed set %v contains a module the test never imports", rs.Paths)
	}
	for _, p := range rs.Paths {
		if strings.Contains(p, "site-packages") || strings.HasPrefix(p, "/") {
			t.Fatalf("observed set leaked something outside the repo: %s", p)
		}
	}
	// The gap this method has, stated as a test so it cannot be forgotten: the
	// data file is a real input and is not in the set. ScopeMatchObserved
	// compensates by refusing on changed paths of a kind sys.modules cannot
	// report, which is covered above.
	if readSetHas(rs, "tests/data/rates.json") {
		t.Fatal("sys.modules unexpectedly reported a non-imported file; the observability rule assumes it cannot")
	}

	recorded := scopeTree(t, root)
	scopeWrite(t, root, "src/auth.py", "def login():\n    return False\n")
	current := scopeTree(t, root)
	if recorded == current {
		t.Fatal("the peer edit did not move the tree")
	}

	const cmd = "pytest tests/test_billing.py"
	tier1 := ScopeMatch(root, recorded, current, cmd)
	scopeCheck(t, tier1, false)
	tier2 := ScopeMatchObserved(root, recorded, current, rs)
	scopeCheck(t, tier2, true)

	t.Logf("command      : %s", cmd)
	t.Logf("observed set : %s, %d paths, %d process(es) -> %v",
		rs.Method, len(rs.Paths), rs.Processes, rs.Paths)
	t.Logf("guarantee    : %s", rs.Guarantee())
	t.Logf("peer diff    : %v", tier2.ChangedPaths)
	t.Logf("tier 1 REFUSE: %s", tier1.Reason)
	t.Logf("tier 2 PROMOTE: %s", tier2.Reason)
}

// TestReadSetPluginDoesNotChangeCommandOutput is the invariant that makes the
// whole approach admissible.
//
// The plugin is loaded into the very command whose output is being cached. If
// it changed that output by so much as a line, every recorded blob would carry
// it, and shadow verification -- which re-executes without the plugin -- would
// report a divergence on every single Python hit.
func TestReadSetPluginDoesNotChangeCommandOutput(t *testing.T) {
	readSetPytest(t)

	root := newRepo(t)
	for rel, body := range pyRepoSeed() {
		scopeWrite(t, root, rel, body)
	}
	t.Setenv("HP_HOME", t.TempDir())
	t.Setenv(readSetOutOverride, "")

	bare, bareCode := runPytest(t, root, nil, "tests/test_billing.py")
	armed, armedCode := runPytest(t, root, ReadSetEnv(root), "tests/test_billing.py")
	if bareCode != armedCode {
		t.Fatalf("exit code changed: %d without the plugin, %d with it", bareCode, armedCode)
	}

	home, _ := os.UserHomeDir()
	gotBare := string(Normalize([]byte(bare), root, home))
	gotArmed := string(Normalize([]byte(armed), root, home))
	if gotBare != gotArmed {
		t.Fatalf("the plugin changed the command's output, which would poison every cached blob:\n"+
			"without:\n%s\nwith:\n%s", gotBare, gotArmed)
	}
}

func readSetHas(rs *ReadSet, p string) bool {
	for _, got := range rs.Paths {
		if got == p {
			return true
		}
	}
	return false
}
