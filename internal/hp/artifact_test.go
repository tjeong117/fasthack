package hp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests build real directories in t.TempDir() and really copy them.
// Mocking the filesystem here would test the mock: every hazard this file
// exists to catch — an absolute shebang, a umask eating a permission bit, a
// symlink followed instead of recreated, a rename that was not atomic — lives
// in the filesystem itself.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// artFakeVenv builds a directory shaped like a real virtualenv, embedding the
// worktree path in exactly the places uv and python -m venv embed it: a
// console-script shebang, the activate script, and an editable install's .pth
// file pointing back at the source tree.
func artFakeVenv(t *testing.T, root string) string {
	t.Helper()
	venv := filepath.Join(root, ".venv")
	site := filepath.Join(venv, "lib", "python3.11", "site-packages")
	artMkdirAll(t, filepath.Join(venv, "bin"))
	artMkdirAll(t, filepath.Join(site, "widget"))

	resolved := resolvePath(root)

	// A console script. This is the hazard: the shebang is an absolute path
	// into this specific worktree's venv.
	artWrite(t, filepath.Join(venv, "bin", "widget"), 0o755,
		"#!"+resolved+"/.venv/bin/python3\nimport widget\nprint(widget.where())\n")
	artWrite(t, filepath.Join(venv, "bin", "activate"), 0o644,
		"VIRTUAL_ENV='"+resolved+"/.venv'\nexport VIRTUAL_ENV\n")

	// An editable install. Far more dangerous than the shebang: this points at
	// the *source tree*, so a venv cloned between worktrees imports the other
	// worktree's code.
	artWrite(t, filepath.Join(site, "_editable_widget.pth"), 0o644, resolved+"/src\n")

	// pyvenv.cfg as uv writes it for a project: no worktree path at all.
	artWrite(t, filepath.Join(venv, "pyvenv.cfg"), 0o644,
		"home = /usr/local/bin\nimplementation = CPython\nversion_info = 3.11.15\nprompt = widget\n")

	artWrite(t, filepath.Join(site, "widget", "__init__.py"), 0o644,
		"def where():\n    return 'widget'\n")
	// A binary file with no worktree path in it: must survive untouched and
	// must not make the artifact unrelocatable.
	artWrite(t, filepath.Join(site, "widget", "_speedup.so"), 0o755,
		"\x7fELF\x00\x00\x00\x00binary payload\x00")

	// The base interpreter symlink: absolute, outside the worktree, correctly
	// left alone. Point it at something that really exists.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh on PATH: %v", err)
	}
	artSymlink(t, sh, filepath.Join(venv, "bin", "python3"))
	// A relative symlink, which relocates for free.
	artSymlink(t, "python3", filepath.Join(venv, "bin", "python"))

	return venv
}

func artMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func artSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func artWrite(t *testing.T, p string, mode os.FileMode, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	// Explicit: os.WriteFile's mode goes through the umask.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
}

// artCaptureFixture builds a worktree with a venv in it and snapshots it.
func artCaptureFixture(t *testing.T) (home, root string, a *Artifact) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "cache")
	root = filepath.Join(base, "wtA")
	artMkdirAll(t, root)
	artFakeVenv(t, root)

	a, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv"),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return home, root, a
}

// artNewRoot makes a sibling worktree root with a name of a deliberately
// different length, so a rewrite that assumed the two paths were the same size
// would be caught.
func artNewRoot(t *testing.T, near, name string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(near), name)
	artMkdirAll(t, p)
	return p
}

// artPrimeClone forces the extracted copy the fast path clones from into
// existence, since it is built lazily on the first restore.
func artPrimeClone(t *testing.T, home, root string, a *Artifact) string {
	t.Helper()
	warm := artNewRoot(t, root, "warm-"+randSuffix())
	if _, err := a.Materialize(home, warm, DefaultMaterializeOptions()); err != nil {
		t.Fatalf("priming the clone source: %v", err)
	}
	if err := os.RemoveAll(warm); err != nil {
		t.Fatal(err)
	}
	clone := artifactClonePath(home, a.TarSHA)
	if _, err := os.Stat(clone); err != nil {
		t.Fatalf("clone source was not created: %v", err)
	}
	return clone
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestArtifactRoundTripPreservesContentModesAndExecutableBit(t *testing.T) {
	home, root, a := artCaptureFixture(t)

	if a.Files == 0 || a.Bytes == 0 {
		t.Fatalf("capture recorded nothing: %+v", a)
	}
	if !a.Relocatable {
		t.Fatalf("fixture should be relocatable, got refusal: %s", a.Refusal)
	}

	src := filepath.Join(root, ".venv")
	before := artSnapshot(t, src)
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	res, err := a.Materialize(home, root, DefaultMaterializeOptions())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !res.Verified {
		t.Error("restore was not verified against its content address")
	}
	if res.Rewrote != 0 {
		t.Errorf("same-root restore rewrote %d files, want 0", res.Rewrote)
	}
	t.Logf("restored %d entries via %s in %v", res.Files, res.Method, res.Duration.Round(time.Millisecond))

	if diff := artSnapshotDiff(before, artSnapshot(t, src)); diff != "" {
		t.Errorf("round trip changed the tree:\n%s", diff)
	}

	// The executable bit specifically, because it is the one that turns a
	// working console script into "permission denied".
	fi, err := os.Stat(filepath.Join(src, "bin", "widget"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/widget lost its executable bit: %v", fi.Mode())
	}
}

// artSnapshot records everything about a tree that a restore must reproduce.
func artSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("dir %04o", fi.Mode().Perm())
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file %04o mtime=%d %q",
				fi.Mode().Perm(), fi.ModTime().Truncate(time.Second).Unix(), string(b))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return out
}

func artSnapshotDiff(want, got map[string]string) string {
	var b strings.Builder
	for k, v := range want {
		if g, ok := got[k]; !ok {
			fmt.Fprintf(&b, "  missing %s\n", k)
		} else if g != v {
			fmt.Fprintf(&b, "  %s:\n    want %s\n    got  %s\n", k, v, g)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&b, "  unexpected %s\n", k)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Relocation — the hazard the whole file exists for
// ---------------------------------------------------------------------------

func TestArtifactRelocationRewritesEmbeddedPaths(t *testing.T) {
	home, root, a := artCaptureFixture(t)

	want := []string{"bin/activate", "bin/widget", "lib/python3.11/site-packages/_editable_widget.pth"}
	for _, w := range want {
		if !artHas(a.Rewrites, w) {
			t.Errorf("capture did not flag %s as embedding the worktree path; got %v", w, a.Rewrites)
		}
	}
	if artHas(a.Rewrites, "pyvenv.cfg") {
		t.Error("pyvenv.cfg has no worktree path in it and should not have been flagged")
	}

	dst := artNewRoot(t, root, "worktree-B-with-a-much-longer-name")
	res, err := a.Materialize(home, dst, DefaultMaterializeOptions())
	if err != nil {
		t.Fatalf("materialize into a different root: %v", err)
	}
	if res.Rewrote != len(want) {
		t.Errorf("rewrote %d files, want %d", res.Rewrote, len(want))
	}
	if !res.Verified {
		t.Error("relocated restore was not verified")
	}

	resolvedDst, resolvedSrc := resolvePath(dst), resolvePath(root)

	// The shebang must point at the *new* worktree's interpreter.
	if got := artFirstLine(t, filepath.Join(dst, ".venv", "bin", "widget")); got != "#!"+resolvedDst+"/.venv/bin/python3" {
		t.Errorf("shebang not relocated:\n  got  %s\n  want #!%s/.venv/bin/python3", got, resolvedDst)
	}

	// The editable install must point at the *new* worktree's source. Getting
	// this wrong is how five agents silently share one agent's edits.
	pth := artRead(t, filepath.Join(dst, ".venv", "lib", "python3.11", "site-packages", "_editable_widget.pth"))
	if strings.TrimSpace(pth) != resolvedDst+"/src" {
		t.Errorf("editable .pth still points at the source worktree:\n  got  %q\n  want %q",
			strings.TrimSpace(pth), resolvedDst+"/src")
	}

	// Nothing anywhere in the restored tree may still mention the old root.
	if found := artGrep(t, filepath.Join(dst, ".venv"), resolvedSrc); len(found) > 0 {
		t.Errorf("restored tree still references the capture worktree %s in: %v", resolvedSrc, found)
	}

	// The binary that never mentioned a path must be byte-identical.
	so := artRead(t, filepath.Join(dst, ".venv", "lib", "python3.11", "site-packages", "widget", "_speedup.so"))
	if so != "\x7fELF\x00\x00\x00\x00binary payload\x00" {
		t.Errorf("binary payload was altered: %q", so)
	}

	// The absolute symlink to the base interpreter is machine-global and must
	// survive as a symlink, not be dereferenced into a copy.
	link, err := os.Readlink(filepath.Join(dst, ".venv", "bin", "python3"))
	if err != nil {
		t.Fatalf("bin/python3 is no longer a symlink: %v", err)
	}
	if strings.Contains(link, resolvedSrc) {
		t.Errorf("base interpreter symlink was rewritten into the old worktree: %s", link)
	}
}

// TestArtifactRelocationStopsAtPathComponentBoundaries is the off-by-one that
// would be a silent corruption rather than a refusal.
//
// fleet.sh names its worktrees a1, a2, a3, so /fleet/a1 is a prefix of
// /fleet/a10. A substitution without a boundary rule rewrites a reference to
// the neighbour into a path that does not exist, and the environment fails
// several commands later pointing nowhere near the cache. Caught live: before
// the rule existed, restoring a1 into b1 turned /fleet/a10 into /fleet/b10.
func TestArtifactRelocationStopsAtPathComponentBoundaries(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "a1")
	neighbour := resolvePath(filepath.Join(base, "a10"))
	artMkdirAll(t, root)
	artFakeVenv(t, root)

	artWrite(t, filepath.Join(root, ".venv", "bin", "mixed"), 0o755,
		"#!/bin/sh\nMINE="+resolvePath(root)+"/x\nNEIGHBOUR="+neighbour+"/x\nBARE="+resolvePath(root)+"\n")

	a, err := CaptureArtifact(home, root, artVenvTargetFor(root))
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	dst := artNewRoot(t, root, "b1")
	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got := artRead(t, filepath.Join(dst, ".venv", "bin", "mixed"))
	if !strings.Contains(got, "MINE="+resolvePath(dst)+"/x") {
		t.Errorf("this worktree's own path was not rewritten:\n%s", got)
	}
	// A trailing reference with nothing after it is still this worktree.
	if !strings.Contains(got, "BARE="+resolvePath(dst)+"\n") {
		t.Errorf("a reference at the end of a line was not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "NEIGHBOUR="+neighbour+"/x") {
		t.Errorf("a sibling worktree whose name merely starts with this one's was rewritten:\n%s\n"+
			"want NEIGHBOUR=%s/x", got, neighbour)
	}
}

// artVenvTargetFor is the target a Python capture takes for a worktree.
func artVenvTargetFor(root string) ArtifactTarget {
	return ArtifactTarget{Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv")}
}

// TestArtifactExcludesPycacheSoVenvsStayRelocatable is the case that decides
// whether the guard is useful at all. A .pyc bakes its source's absolute path
// into co_filename, so a pip virtualenv holds thousands of binary files
// embedding the worktree path — measured at 2,954 in a 143 MB venv against 15
// rewritable text files. Counted as content they would make every such
// environment unrelocatable, and the guard would refuse exactly the installs
// slow enough to be worth caching.
func TestArtifactExcludesPycacheSoVenvsStayRelocatable(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wtA")
	artMkdirAll(t, root)
	artFakeVenv(t, root)
	site := filepath.Join(root, ".venv", "lib", "python3.11", "site-packages", "widget")

	// Bytecode as CPython writes it: binary, carrying the absolute path of the
	// source it was compiled from.
	artWrite(t, filepath.Join(site, "__pycache__", "__init__.cpython-311.pyc"), 0o644,
		"\x00\x0d\x0d\x0a\x00\x00"+resolvePath(root)+"/.venv/lib/python3.11/site-packages/widget/__init__.py\x00")

	a, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv"),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !a.Relocatable {
		t.Fatalf("bytecode should not make a venv unrelocatable: %s", a.Refusal)
	}
	for _, r := range a.Rewrites {
		if strings.Contains(r, "__pycache__") {
			t.Errorf("__pycache__ entry %s reached the rewrite set", r)
		}
	}

	dst := artNewRoot(t, root, "wtB")
	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Held out of the snapshot rather than restored stale. CPython ignores a
	// .pyc whose source is missing, so it can never be the only copy of a
	// module, and regenerating it is what makes tracebacks in the new worktree
	// point at the new worktree.
	if _, err := os.Stat(filepath.Join(dst, ".venv", "lib", "python3.11", "site-packages", "widget", "__pycache__")); !os.IsNotExist(err) {
		t.Error("__pycache__ was restored; it should be left for the interpreter to rebuild")
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv", "lib", "python3.11", "site-packages", "widget", "__init__.py")); err != nil {
		t.Errorf("the source beside the excluded cache went missing: %v", err)
	}

	// A sourceless distribution puts its .pyc beside the package rather than
	// in __pycache__, and that one is real content.
	artWrite(t, filepath.Join(site, "compiled.pyc"), 0o644, "\x00\x0d\x0d\x0a plain bytecode, no path\x00")
	a2, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv"),
	})
	if err != nil {
		t.Fatalf("recapture: %v", err)
	}
	dst2 := artNewRoot(t, root, "wtC")
	if _, err := a2.Materialize(home, dst2, DefaultMaterializeOptions()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst2, ".venv", "lib", "python3.11", "site-packages", "widget", "compiled.pyc")); err != nil {
		t.Errorf("a sourceless .pyc outside __pycache__ must be captured: %v", err)
	}
}

// TestArtifactRefusesToRelocateBinaryEmbeddedPath is the abstention case. A
// compiled object embedding the worktree path cannot be rewritten by string
// substitution, and guessing at its structure is how you produce an
// environment that fails confusingly three commands later.
func TestArtifactRefusesToRelocateBinaryEmbeddedPath(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wtA")
	artMkdirAll(t, root)
	artFakeVenv(t, root)

	artWrite(t, filepath.Join(root, ".venv", "lib", "python3.11", "site-packages", "pinned.so"),
		0o755, "\x7fELF\x00rpath="+resolvePath(root)+"/.venv/lib\x00")

	a, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv"),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if a.Relocatable {
		t.Fatal("artifact with a path-embedding binary claimed to be relocatable")
	}
	if !strings.Contains(a.Refusal, "pinned.so") {
		t.Errorf("refusal should name the offending file, got %q", a.Refusal)
	}

	// Same root is still fine: nothing needs to move.
	if err := os.RemoveAll(filepath.Join(root, ".venv")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Materialize(home, root, DefaultMaterializeOptions()); err != nil {
		t.Errorf("same-root restore of an unrelocatable artifact should work: %v", err)
	}

	dst := artNewRoot(t, root, "wtB")
	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); !errors.Is(err, ErrArtifactNotRelocatable) {
		t.Fatalf("want ErrArtifactNotRelocatable, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv")); !os.IsNotExist(err) {
		t.Error("a refused relocation still created the target directory")
	}
}

func TestArtifactRefusesDanglingExternalSymlink(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wtA")
	artMkdirAll(t, root)
	artFakeVenv(t, root)

	// Stand in for a base interpreter that gets upgraded away after capture.
	interp := filepath.Join(base, "python-3.11")
	artWrite(t, interp, 0o755, "#!/bin/sh\n")
	if err := os.Remove(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Fatal(err)
	}
	artSymlink(t, interp, filepath.Join(root, ".venv", "bin", "python3"))

	a, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "python", RelDir: ".venv", AbsDir: filepath.Join(root, ".venv"),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !artHas(a.ExternalLinks, interp) {
		t.Fatalf("capture did not record the external interpreter link, got %v", a.ExternalLinks)
	}

	if err := os.Remove(interp); err != nil {
		t.Fatal(err)
	}
	dst := artNewRoot(t, root, "wtB")
	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); err == nil {
		t.Fatal("restoring a venv whose base interpreter is gone should refuse")
	} else if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv")); !os.IsNotExist(err) {
		t.Error("a refused restore still created the target directory")
	}
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// TestArtifactTamperedTarIsRefused corrupts the snapshot in both places it can
// be corrupted. Damage to file data is caught by the content hash; damage to a
// tar header stops the reader before the hash is ever reached. Both must land
// as the same refusal, because a caller deciding whether to serve does not
// care which byte moved.
func TestArtifactTamperedTarIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset func(b []byte) int
	}{
		{"file data", func(b []byte) int { return strings.Index(string(b), "binary payload") }},
		{"tar header", func(b []byte) int { return 4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root, a := artCaptureFixture(t)

			// Force the tar to be the source of truth by removing the
			// extracted copy the fast path would otherwise clone.
			if err := os.RemoveAll(artifactClonePath(home, a.TarSHA)); err != nil {
				t.Fatal(err)
			}
			tarPath := artifactTarPath(home, a.TarSHA)
			b, err := os.ReadFile(tarPath)
			if err != nil {
				t.Fatal(err)
			}
			i := tc.offset(b)
			if i < 0 || i >= len(b) {
				t.Fatalf("could not find a byte to corrupt (offset %d of %d)", i, len(b))
			}
			b[i] ^= 0xff
			if err := os.WriteFile(tarPath, b, 0o644); err != nil {
				t.Fatal(err)
			}

			dst := artNewRoot(t, root, "wtB")
			if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); !errors.Is(err, ErrArtifactCorrupt) {
				t.Fatalf("want ErrArtifactCorrupt, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(dst, ".venv")); !os.IsNotExist(err) {
				t.Error("a tampered snapshot still produced a target directory")
			}
			shard := filepath.Dir(artifactClonePath(home, a.TarSHA))
			if n := artGlobCount(t, filepath.Join(shard, ".unpack-*")); n != 0 {
				t.Errorf("a refused unpack left %d scratch director(ies) behind", n)
			}
		})
	}
}

// TestArtifactTamperedCloneSourceIsRefused covers the other half: the tar is
// intact but the extracted copy the fast path clones from has been altered.
// Re-deriving the hash from what was actually produced catches this;
// re-reading the tar would not.
func TestArtifactTamperedCloneSourceIsRefused(t *testing.T) {
	home, root, a := artCaptureFixture(t)
	clone := artPrimeClone(t, home, root, a)

	artWrite(t, filepath.Join(clone, "lib", "python3.11", "site-packages", "widget", "__init__.py"),
		0o644, "def where():\n    return 'HIJACKED'\n")

	dst := artNewRoot(t, root, "wtB")
	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("want ErrArtifactCorrupt, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv")); !os.IsNotExist(err) {
		t.Error("a corrupted clone source still produced a target directory")
	}
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

func TestArtifactSizeBoundFires(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wtA")
	artMkdirAll(t, filepath.Join(root, "node_modules", "big"))
	artWrite(t, filepath.Join(root, "node_modules", "big", "blob.bin"), 0o644, strings.Repeat("x", 64<<10))

	t.Setenv("HP_ARTIFACT_MAX_BYTES", "1024")
	_, err := CaptureArtifact(home, root, ArtifactTarget{
		Ecosystem: "node", RelDir: "node_modules", AbsDir: filepath.Join(root, "node_modules"),
	})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("want ErrArtifactTooLarge, got %v", err)
	}
	if !strings.Contains(err.Error(), "1.0KB") {
		t.Errorf("refusal should state the bound in human terms, got %q", err)
	}

	// Nothing may be left behind by a refused capture.
	if entries, err := os.ReadDir(filepath.Join(home, "artifacts", "tmp")); err == nil && len(entries) > 0 {
		t.Errorf("refused capture left %d temp file(s) behind", len(entries))
	}
}

func TestArtifactFileCountBoundFires(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	nm := filepath.Join(base, "wtA", "node_modules")
	artMkdirAll(t, nm)
	for i := 0; i < 40; i++ {
		artWrite(t, filepath.Join(nm, fmt.Sprintf("pkg%02d.js", i)), 0o644, "module.exports=1\n")
	}

	t.Setenv("HP_ARTIFACT_MAX_FILES", "10")
	_, err := CaptureArtifact(home, filepath.Join(base, "wtA"), ArtifactTarget{
		Ecosystem: "node", RelDir: "node_modules", AbsDir: nm,
	})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("want ErrArtifactTooLarge, got %v", err)
	}
}

func TestArtifactRefusesUnreproducibleFileType(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo")
	}
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	nm := filepath.Join(base, "wtA", "node_modules")
	artMkdirAll(t, nm)
	artWrite(t, filepath.Join(nm, "index.js"), 0o644, "module.exports=1\n")
	if err := syscall.Mkfifo(filepath.Join(nm, "sock"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	_, err := CaptureArtifact(home, filepath.Join(base, "wtA"), ArtifactTarget{
		Ecosystem: "node", RelDir: "node_modules", AbsDir: nm,
	})
	if !errors.Is(err, ErrArtifactUnsupported) {
		t.Fatalf("want ErrArtifactUnsupported for a fifo, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Never clobber, never half-install
// ---------------------------------------------------------------------------

func TestArtifactDoesNotClobberAnExistingTarget(t *testing.T) {
	home, root, a := artCaptureFixture(t)
	dst := artNewRoot(t, root, "wtB")

	// A developer's working virtualenv, already there.
	artWrite(t, filepath.Join(dst, ".venv", "bin", "mine"), 0o755, "do not delete me\n")

	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); !errors.Is(err, ErrArtifactTargetExists) {
		t.Fatalf("want ErrArtifactTargetExists, got %v", err)
	}
	if got := artRead(t, filepath.Join(dst, ".venv", "bin", "mine")); got != "do not delete me\n" {
		t.Fatalf("existing venv was disturbed: %q", got)
	}

	// With an explicit decision it is replaced — and the old contents go.
	opt := DefaultMaterializeOptions()
	opt.Overwrite = true
	if _, err := a.Materialize(home, dst, opt); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv", "bin", "mine")); !os.IsNotExist(err) {
		t.Error("overwrite left the old contents in place, producing a merged environment")
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv", "bin", "widget")); err != nil {
		t.Errorf("overwrite did not install the artifact: %v", err)
	}

	// An empty directory is not somebody's work and may be replaced silently.
	dst2 := artNewRoot(t, root, "wtC")
	artMkdirAll(t, filepath.Join(dst2, ".venv"))
	if _, err := a.Materialize(home, dst2, DefaultMaterializeOptions()); err != nil {
		t.Errorf("an empty target should not need an overwrite decision: %v", err)
	}
}

// TestArtifactFailedMaterializeLeavesNothingBehind is the atomicity claim. A
// half-populated .venv that looks installed is worse than no venv at all: the
// agent gets a confusing ImportError deep in a later command with no
// indication the cache caused it.
func TestArtifactFailedMaterializeLeavesNothingBehind(t *testing.T) {
	home, root, a := artCaptureFixture(t)
	dst := artNewRoot(t, root, "wtB")

	// Fail late — after the whole tree has been staged and rewritten, during
	// verification. This is the worst case for atomicity.
	clone := artPrimeClone(t, home, root, a)
	artWrite(t, filepath.Join(clone, "pyvenv.cfg"), 0o644, "tampered\n")

	if _, err := a.Materialize(home, dst, DefaultMaterializeOptions()); err == nil {
		t.Fatal("expected the restore to fail")
	}
	if _, err := os.Stat(filepath.Join(dst, ".venv")); !os.IsNotExist(err) {
		t.Error(".venv exists after a failed restore")
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".hs-") {
			t.Errorf("failed restore left staging debris: %s", e.Name())
		}
	}
	if len(entries) != 0 {
		t.Errorf("failed restore left %d entries in the worktree: %v", len(entries), artNames(entries))
	}
}

// TestArtifactTimeoutLeavesNothingBehind covers an interruption rather than a
// failure: the deadline expires mid-restore.
func TestArtifactTimeoutLeavesNothingBehind(t *testing.T) {
	home, root, a := artCaptureFixture(t)
	dst := artNewRoot(t, root, "wtB")

	opt := DefaultMaterializeOptions()
	opt.Timeout = time.Nanosecond
	if _, err := a.Materialize(home, dst, opt); err == nil {
		t.Fatal("expected the restore to time out")
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("timed-out restore left %v behind", artNames(entries))
	}
}

// ---------------------------------------------------------------------------
// Detection, policy and the key index
// ---------------------------------------------------------------------------

func TestArtifactTargetsFollowTheEcosystemRegistry(t *testing.T) {
	t.Setenv("VIRTUAL_ENV", "")
	root := t.TempDir()

	if got := ArtifactTargets(root); len(got) != 0 {
		t.Errorf("empty workspace should produce no targets, got %v", got)
	}

	artWrite(t, filepath.Join(root, "pyproject.toml"), 0o644, "[project]\nname='x'\n")
	artWrite(t, filepath.Join(root, "package.json"), 0o644, "{}\n")
	artWrite(t, filepath.Join(root, "Gemfile"), 0o644, "source 'x'\n")
	// Go and Rust install into caches shared by every worktree on the machine,
	// so they have nothing per-worktree to capture.
	artWrite(t, filepath.Join(root, "go.mod"), 0o644, "module x\n")
	artWrite(t, filepath.Join(root, "Cargo.toml"), 0o644, "[package]\nname='x'\n")

	got := map[string]string{}
	for _, tgt := range ArtifactTargets(root) {
		got[tgt.Ecosystem] = tgt.RelDir
	}
	for eco, dir := range map[string]string{
		"python": ".venv", "node": "node_modules", "ruby": "vendor/bundle",
	} {
		if got[eco] != dir {
			t.Errorf("%s: got %q, want %q", eco, got[eco], dir)
		}
	}
	for _, eco := range []string{"go", "rust"} {
		if d, ok := got[eco]; ok {
			t.Errorf("%s installs into a global cache and should have no artifact, got %q", eco, d)
		}
	}
}

// A $VIRTUAL_ENV outside the worktree is a real configuration, and it is not a
// per-worktree artifact. Materializing into somebody's shared virtualenv is
// not a decision this guard gets to make.
func TestArtifactTargetsSkipVenvOutsideTheWorktree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "wt")
	shared := filepath.Join(base, "shared-venv")
	artMkdirAll(t, root)
	artMkdirAll(t, shared)
	artWrite(t, filepath.Join(root, "pyproject.toml"), 0o644, "[project]\nname='x'\n")
	t.Setenv("VIRTUAL_ENV", shared)

	for _, tgt := range ArtifactTargets(root) {
		if tgt.Ecosystem == "python" {
			t.Errorf("captured a venv outside the worktree: %+v", tgt)
		}
	}
}

func TestArtifactInstallPolicyIsOptIn(t *testing.T) {
	t.Setenv("HP_ARTIFACT", "")
	if p, reason := ArtifactInstallPolicy(); p != RECORD_ONLY {
		t.Errorf("default must stay RECORD_ONLY, got %v (%s)", p, reason)
	}
	t.Setenv("HP_ARTIFACT", "1")
	if p, reason := ArtifactInstallPolicy(); p != SERVE {
		t.Errorf("with the guard on, installs should be SERVE, got %v (%s)", p, reason)
	}
}

func TestArtifactRefIndexRoundTrips(t *testing.T) {
	home := t.TempDir()
	key := "hs-v1:" + strings.Repeat("a", 64)

	if _, ok := LookupArtifact(home, key); ok {
		t.Fatal("empty cache returned an artifact")
	}
	a := &Artifact{Version: ArtifactVersion, Ecosystem: "python", RelDir: ".venv",
		TarSHA: "sha256:deadbeef", SourceRoot: "/x", Relocatable: true}
	if err := PutArtifactRef(home, key, a); err != nil {
		t.Fatal(err)
	}
	got, ok := LookupArtifact(home, key)
	if !ok {
		t.Fatal("artifact not found after being written")
	}
	if got.TarSHA != a.TarSHA || got.RelDir != a.RelDir {
		t.Errorf("round trip mangled the descriptor: %+v", got)
	}

	// A descriptor from a future format must be ignored, not interpreted.
	a.Version = ArtifactVersion + 1
	if err := PutArtifactRef(home, key, a); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookupArtifact(home, key); ok {
		t.Error("a descriptor from an unknown version was accepted")
	}
}

func TestArtifactCaptureInstallRefusesAmbiguousWorkspace(t *testing.T) {
	t.Setenv("HP_ARTIFACT", "1")
	t.Setenv("VIRTUAL_ENV", "")
	base := t.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wt")
	artMkdirAll(t, root)

	artWrite(t, filepath.Join(root, "pyproject.toml"), 0o644, "[project]\nname='x'\n")
	artWrite(t, filepath.Join(root, "package.json"), 0o644, "{}\n")

	// Nothing installed yet: nothing to capture, and that is not an error.
	if a, err := CaptureInstall(home, root, "k1"); err != nil || a != nil {
		t.Fatalf("empty workspace: got (%v, %v)", a, err)
	}

	artFakeVenv(t, root)
	a, err := CaptureInstall(home, root, "k2")
	if err != nil || a == nil {
		t.Fatalf("one installed ecosystem should capture: (%v, %v)", a, err)
	}
	if _, ok := LookupArtifact(home, "k2"); !ok {
		t.Error("CaptureInstall did not file the artifact under the key")
	}

	// Two installed ecosystems: the command string cannot say which one the
	// install touched, so restoring either would be a guess.
	artWrite(t, filepath.Join(root, "node_modules", "left-pad", "index.js"), 0o644, "x\n")
	if _, err := CaptureInstall(home, root, "k3"); err == nil {
		t.Error("an ambiguous workspace should refuse rather than capture half of it")
	}
}

func TestArtifactCaptureInstallIsOffByDefault(t *testing.T) {
	t.Setenv("HP_ARTIFACT", "")
	base := t.TempDir()
	root := filepath.Join(base, "wt")
	artMkdirAll(t, root)
	artFakeVenv(t, root)
	if a, err := CaptureInstall(filepath.Join(base, "cache"), root, "k"); err != nil || a != nil {
		t.Fatalf("guard is off, nothing should be captured: (%v, %v)", a, err)
	}
}

func TestArtifactRestoreInstallReportsAMissAsAMiss(t *testing.T) {
	home := t.TempDir()
	if _, err := RestoreInstall(home, "hs-v1:nothing", home); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("want ErrNoArtifact, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// artBuildTree lays out a few thousand small files in a shape resembling a
// populated site-packages: many packages, a handful of modules each.
func artBuildTree(tb testing.TB, dir string, files int) {
	tb.Helper()
	const perPkg = 12
	body := "# module\n" + strings.Repeat("x = 1\n", 40)
	for i := 0; i < files; i += perPkg {
		pkg := filepath.Join(dir, "lib", "python3.11", "site-packages", fmt.Sprintf("pkg%04d", i/perPkg))
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			tb.Fatal(err)
		}
		for j := 0; j < perPkg && i+j < files; j++ {
			if err := os.WriteFile(filepath.Join(pkg, fmt.Sprintf("mod%02d.py", j)), []byte(body), 0o644); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		tb.Fatal(err)
	}
	shebang := "#!" + resolvePath(filepath.Dir(dir)) + "/.venv/bin/python3\n"
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool"), []byte(shebang), 0o755); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkArtifactMaterialize measures a restore of a realistic tree, which is
// the number the whole feature trades against a cold install.
func BenchmarkArtifactMaterialize(b *testing.B) {
	artBenchMaterialize(b, true)
}

// BenchmarkArtifactMaterializeUnverified isolates the copy from the
// verification pass, so the cost of proving a restore correct is visible
// rather than buried in the total.
func BenchmarkArtifactMaterializeUnverified(b *testing.B) {
	artBenchMaterialize(b, false)
}

func artBenchMaterialize(b *testing.B, verify bool) {
	base := b.TempDir()
	home := filepath.Join(base, "cache")
	root := filepath.Join(base, "wtA")
	venv := filepath.Join(root, ".venv")
	artBuildTree(b, venv, 3000)

	a, err := CaptureArtifact(home, root, ArtifactTarget{Ecosystem: "python", RelDir: ".venv", AbsDir: venv})
	if err != nil {
		b.Fatal(err)
	}
	opt := DefaultMaterializeOptions()
	opt.Verify = verify

	// Build the clone source once, outside the measurement, so the benchmark
	// reports the steady-state restore rather than the first one.
	warm := filepath.Join(base, "warm")
	if err := os.MkdirAll(warm, 0o755); err != nil {
		b.Fatal(err)
	}
	if _, err := a.Materialize(home, warm, opt); err != nil {
		b.Fatal(err)
	}
	os.RemoveAll(warm)
	b.Logf("%d entries, %s tar", a.Files, humanBytes(a.TarBytes))

	i := 0
	for b.Loop() {
		dst := filepath.Join(base, fmt.Sprintf("wt%04d", i))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			b.Fatal(err)
		}
		res, err := a.Materialize(home, dst, opt)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if i == 0 {
			b.Logf("method=%s verified=%v rewrote=%d", res.Method, res.Verified, res.Rewrote)
		}
		os.RemoveAll(dst)
		i++
		b.StartTimer()
	}
}

func BenchmarkArtifactCapture(b *testing.B) {
	base := b.TempDir()
	root := filepath.Join(base, "wtA")
	venv := filepath.Join(root, ".venv")
	artBuildTree(b, venv, 3000)

	i := 0
	for b.Loop() {
		home := filepath.Join(base, fmt.Sprintf("cache%04d", i))
		if _, err := CaptureArtifact(home, root, ArtifactTarget{
			Ecosystem: "python", RelDir: ".venv", AbsDir: venv,
		}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		os.RemoveAll(home)
		i++
		b.StartTimer()
	}
}

// ---------------------------------------------------------------------------

func artHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func artNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func artRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func artFirstLine(t *testing.T, p string) string {
	t.Helper()
	s := artRead(t, p)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func artGlobCount(t *testing.T, pattern string) int {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// artGrep reports every file still mentioning needle, which is how the
// relocation test proves the rewrite was complete rather than merely applied.
func artGrep(t *testing.T, dir, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if strings.Contains(target, needle) {
				found = append(found, p+" (symlink)")
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			rel, _ := filepath.Rel(dir, p)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("grep %s: %v", dir, err)
	}
	return found
}
