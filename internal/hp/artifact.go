package hp

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The artifact guard.
//
// Hindsight caches a command's transcript: stdout, stderr, exit code. For
// pytest or grep that is the entire product. For `uv sync` or `pip install` it
// is not — their real product is a directory on disk — so serving the
// transcript alone would leave the next command dying with an ImportError,
// which is exactly the wrong-answer failure the purity gate exists to prevent.
// The gate refuses installs correctly today: the environment fingerprint
// moves, the record is unservable, and every agent in a fan-out installs
// independently.
//
// That is the largest uncaptured pool in the system. A cold install is tens of
// seconds, and it is the one command class where all N agents are provably at
// an identical state and running simultaneously — the case single-flight
// already coalesces but cannot serve.
//
// This file captures the produced directory alongside the transcript so both
// can be replayed. It changes nothing about the guarantee. A materialized
// directory is a verified replay of one that really existed, and every path
// that cannot prove that refuses instead. A restore that is subtly wrong is
// far worse than an install that just runs: the agent then fails deep inside a
// later command with no indication the cache caused it.

// ArtifactVersion is the descriptor format version. A descriptor from a
// version this build does not understand is refused, not interpreted.
const ArtifactVersion = 1

const (
	// DefaultArtifactMaxBytes bounds the uncompressed size of one artifact. A
	// .venv carrying torch runs to several gigabytes, and refusing that is the
	// right answer: the copy would cost more than the install.
	DefaultArtifactMaxBytes int64 = 2 << 30 // 2 GiB
	// DefaultArtifactMaxFiles bounds the entry count. node_modules routinely
	// holds a hundred thousand files and occasionally far more.
	DefaultArtifactMaxFiles = 200_000
	// DefaultArtifactTimeout bounds capture and materialize. Overrunning it
	// aborts and cleans up, which costs a hit and never correctness.
	DefaultArtifactTimeout = 120 * time.Second
)

// Refusals. Each one is a case where serving would be a guess.
var (
	ErrArtifactTooLarge       = errors.New("artifact exceeds the configured bound")
	ErrArtifactTargetExists   = errors.New("target directory exists and is not empty")
	ErrArtifactNotRelocatable = errors.New("artifact cannot be moved to a different worktree root")
	ErrArtifactCorrupt        = errors.New("artifact does not match its recorded hash")
	ErrArtifactUnsupported    = errors.New("artifact contains a file type that cannot be reproduced")
	ErrNoArtifact             = errors.New("no artifact recorded for this key")
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// ArtifactEnabled reports whether installs may be served.
//
// Default off, deliberately. Flipping installs from RECORD_ONLY to SERVE
// re-enables the lease path for them, so a bug here does not merely cost a hit
// — it makes four peers block for the length of an install and then install
// anyway. Opting in keeps the existing measured arms unchanged.
func ArtifactEnabled() bool { return os.Getenv("HP_ARTIFACT") == "1" }

// ArtifactVerifyOnMaterialize reports whether a restored directory is
// re-hashed against its content address before being swapped into place. On by
// default: this is what makes a served directory a verified replay rather than
// a hopeful one.
func ArtifactVerifyOnMaterialize() bool { return os.Getenv("HP_ARTIFACT_VERIFY") != "0" }

func ArtifactMaxBytes() int64 {
	if n, err := strconv.ParseInt(os.Getenv("HP_ARTIFACT_MAX_BYTES"), 10, 64); err == nil && n > 0 {
		return n
	}
	return DefaultArtifactMaxBytes
}

func ArtifactMaxFiles() int {
	if n, err := strconv.Atoi(os.Getenv("HP_ARTIFACT_MAX_FILES")); err == nil && n > 0 {
		return n
	}
	return DefaultArtifactMaxFiles
}

func ArtifactTimeout() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("HP_ARTIFACT_TIMEOUT")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return DefaultArtifactTimeout
}

// ArtifactInstallPolicy is what policy.go asks once it has recognised an
// install. It lives here because whether an install is servable is a property
// of this file, not of the classifier.
//
// Classify stays a pure function of the command string for any fixed process:
// this reads no argument and consults nothing that changes mid-run.
func ArtifactInstallPolicy() (Policy, string) {
	if ArtifactEnabled() {
		return SERVE, "install: servable, the artifact guard replays the produced directory"
	}
	return RECORD_ONLY, "install: never servable without the artifact guard"
}

// ---------------------------------------------------------------------------
// Which directory does an install produce?
// ---------------------------------------------------------------------------

// ArtifactTarget names a directory that an install fills in.
type ArtifactTarget struct {
	Ecosystem string // matches Ecosystem.Name()
	RelDir    string // slash-separated, relative to the worktree root
	AbsDir    string
}

// ArtifactProducer is the optional half of Ecosystem. An ecosystem that
// installs into the worktree can say where, and the guard picks it up without
// this file needing to know about it.
type ArtifactProducer interface {
	Ecosystem
	ArtifactDirs(root string) []string
}

// installDirs is the fallback for the ecosystems that shipped before
// ArtifactProducer existed.
//
// Three entries, and the omissions are the interesting part. Go, Rust and the
// JVM are absent because their caches are global — $GOMODCACHE,
// ~/.cargo/registry, ~/.m2 — so N worktrees on one machine already share one
// installed copy and there is nothing per-worktree to capture. The artifact
// guard is needed precisely for the ecosystems that install *into the
// worktree*, and that is Python, Node and Ruby.
var installDirs = map[string][]string{
	"node": {"node_modules"},
	"ruby": {"vendor/bundle"},
}

// ArtifactTargets lists the directories an install could have produced in this
// workspace, considering only ecosystems actually in use.
//
// Reuses the registry and the detectors from envfp.go rather than restating
// them, so the guard captures exactly the directories the fingerprint watches.
// Python goes through venvPath for the same reason: if $VIRTUAL_ENV redirected
// the fingerprint, it has to redirect the capture too.
func ArtifactTargets(root string) []ArtifactTarget {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var out []ArtifactTarget
	for _, e := range ecosystems {
		if !e.Detect(abs) {
			continue
		}
		var dirs []string
		if p, ok := e.(ArtifactProducer); ok {
			dirs = p.ArtifactDirs(abs)
		} else if e.Name() == "python" {
			dirs = []string{venvPath(abs)}
		} else {
			for _, rel := range installDirs[e.Name()] {
				dirs = append(dirs, filepath.Join(abs, filepath.FromSlash(rel)))
			}
		}
		for _, d := range dirs {
			rel, err := filepath.Rel(abs, d)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				// $VIRTUAL_ENV pointing outside the worktree is a real
				// configuration. It is also not a per-worktree artifact, and
				// materializing into someone's shared virtualenv is not a
				// decision this guard gets to make.
				Debugf("artifact: %s dir %s is outside the worktree; not capturable", e.Name(), d)
				continue
			}
			out = append(out, ArtifactTarget{
				Ecosystem: e.Name(), RelDir: filepath.ToSlash(rel), AbsDir: d,
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The descriptor
// ---------------------------------------------------------------------------

// Artifact describes one captured directory. It is small, JSON-serializable,
// and it is the only thing that has to travel with a cache key.
type Artifact struct {
	Version   int    `json:"version"`
	Ecosystem string `json:"ecosystem"`
	RelDir    string `json:"rel_dir"`

	// TarSHA content-addresses the snapshot. Two identical captures share one
	// file on disk, and any restore can be checked against it.
	TarSHA   string `json:"tar_sha256"`
	TarBytes int64  `json:"tar_bytes"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`

	// SourceRoot is the symlink-resolved worktree root the capture came from.
	// Everything about relocation is a question about this string.
	SourceRoot string `json:"source_root"`

	// Relocatable says whether this artifact may be materialized at a root
	// other than SourceRoot. False is a refusal, and Refusal says why.
	Relocatable bool   `json:"relocatable"`
	Refusal     string `json:"refusal,omitempty"`

	// Rewrites are paths inside the artifact, relative to RelDir, whose text
	// contents embed SourceRoot and must be substituted on relocation.
	Rewrites []string `json:"rewrites,omitempty"`
	// SymlinkRewrites are symlinks whose target embeds SourceRoot.
	SymlinkRewrites []string `json:"symlink_rewrites,omitempty"`
	// ExternalLinks are absolute symlink targets outside the worktree — in
	// practice .venv/bin/python pointing at the base interpreter. They are
	// machine-global and correct to keep, and they are checked on materialize
	// because an artifact that outlived an interpreter upgrade would restore a
	// virtualenv whose every command fails.
	ExternalLinks []string `json:"external_links,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	GOOS      string    `json:"goos"`
	GOARCH    string    `json:"goarch"`
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// Artifacts live in a tree parallel to blobs/ rather than inside it.
//
// Store.PutBlob takes a []byte, which means holding the whole snapshot in
// memory. A .venv is tens to hundreds of megabytes and node_modules is worse,
// and `hindsight record` runs alongside the agent, so that is not a cost we
// can impose. Streaming to a temp file and renaming it to its own hash gives
// the same content-addressing and the same write-then-rename atomicity without
// the memory. The lifecycles differ too: transcripts are kilobytes and worth
// keeping indefinitely, snapshots are gigabytes and want their own eviction.

func artifactRoot(home string) string { return filepath.Join(home, "artifacts") }

func artifactTarPath(home, sha string) string {
	h := strings.TrimPrefix(sha, "sha256:")
	if len(h) < 2 {
		return filepath.Join(artifactRoot(home), h+".tar")
	}
	return filepath.Join(artifactRoot(home), h[:2], h+".tar")
}

// artifactClonePath is the extracted copy kept beside the tar so a restore can
// be a copy-on-write clone instead of an extraction.
func artifactClonePath(home, sha string) string {
	return strings.TrimSuffix(artifactTarPath(home, sha), ".tar") + ".d"
}

// artifactRefPath maps a cache key to its descriptor.
//
// A side table keyed by cache key, rather than a new field on Record. The log
// line is a frozen contract and a portable corpus; a tar of somebody's
// virtualenv is neither. Keeping them apart means the artifact guard needs no
// change to store.go, daemon.go or the wire format.
func artifactRefPath(home, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(artifactRoot(home), "refs", h[:2], h+".json")
}

// PutArtifactRef records which artifact belongs to a cache key.
func PutArtifactRef(home, key string, a *Artifact) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return writeFileAtomic(artifactRefPath(home, key), b, 0o644)
}

// LookupArtifact returns the artifact recorded for a key, if any.
func LookupArtifact(home, key string) (*Artifact, bool) {
	b, err := os.ReadFile(artifactRefPath(home, key))
	if err != nil {
		return nil, false
	}
	var a Artifact
	if json.Unmarshal(b, &a) != nil {
		return nil, false
	}
	if a.Version != ArtifactVersion {
		Debugf("artifact: descriptor v%d, this build understands v%d", a.Version, ArtifactVersion)
		return nil, false
	}
	return &a, true
}

func writeFileAtomic(dst string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

// CaptureArtifact snapshots one produced directory into the cache.
//
// The snapshot is a plain uncompressed tar. These trees are already-compressed
// wheels and minified JavaScript, so gzip would spend CPU on the hot path to
// win a few percent.
func CaptureArtifact(home, root string, t ArtifactTarget) (*Artifact, error) {
	started := time.Now()
	deadline := started.Add(ArtifactTimeout())

	srcRoot := resolvePath(root)
	if fi, err := os.Stat(t.AbsDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("artifact: %s is not a directory", t.AbsDir)
	}

	if err := os.MkdirAll(filepath.Join(artifactRoot(home), "tmp"), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Join(artifactRoot(home), "tmp"), "capture-*.tar")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	// Every failure below leaves nothing behind. A half-written tar under a
	// content-addressed name would be a permanent wrong answer.
	defer func() { tmp.Close(); os.Remove(tmpName) }()

	h := sha256.New()
	st, err := tarTree(io.MultiWriter(tmp, h), t.AbsDir, tarOpts{
		maxBytes: ArtifactMaxBytes(),
		maxFiles: ArtifactMaxFiles(),
		scanFor:  rootNeedles(root, srcRoot),
		deadline: deadline,
	})
	if err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	sha := "sha256:" + hex.EncodeToString(h.Sum(nil))
	dst := artifactTarPath(home, sha)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(dst); err != nil {
		if err := os.Rename(tmpName, dst); err != nil {
			return nil, err
		}
	}
	var tarBytes int64
	if fi, err := os.Stat(dst); err == nil {
		tarBytes = fi.Size()
	}

	a := &Artifact{
		Version: ArtifactVersion, Ecosystem: t.Ecosystem, RelDir: t.RelDir,
		TarSHA: sha, TarBytes: tarBytes, Files: st.entries, Bytes: st.bytes,
		SourceRoot: srcRoot, Relocatable: true,
		Rewrites: st.textHits, SymlinkRewrites: st.linkHits,
		ExternalLinks: dedupe(st.externalLinks),
		CreatedAt:     time.Now().UTC(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}

	// The one thing text substitution cannot fix. A compiled extension or a
	// database that embeds the worktree path is not safely rewritable, and
	// guessing at its structure is how you produce an environment that imports
	// the wrong source tree. Keep it for the same-path case, refuse to move it.
	if len(st.binaryHits) > 0 {
		a.Relocatable = false
		a.Refusal = fmt.Sprintf(
			"%d binary file(s) embed the worktree path and cannot be safely rewritten (first: %s)",
			len(st.binaryHits), st.binaryHits[0])
	}

	// No clone source is built here. It is tempting — a copy-on-write clone of
	// the live directory is nearly free — but the live directory is not what
	// was captured: excludedFromArtifact holds __pycache__ out of the tar, and
	// a clone source taken from disk would carry it back in, restoring
	// thousands of bytecode files still pointing at the capture worktree. The
	// clone source is derived from the tar on first restore instead, which
	// makes it equal to the snapshot by construction rather than by anyone
	// remembering to keep two walks in step.

	Debugf("artifact: captured %s (%d entries, %s) in %s, relocatable=%v",
		t.RelDir, st.entries, humanBytes(st.bytes),
		time.Since(started).Round(time.Millisecond), a.Relocatable)
	return a, nil
}

// CaptureInstall is the whole record.go integration in one call: work out what
// this workspace's install produced, snapshot it, and file it under the key.
//
// Returns nil with no error when there is nothing to capture, which is the
// ordinary case for every command that is not an install.
func CaptureInstall(home, root, key string) (*Artifact, error) {
	if !ArtifactEnabled() {
		return nil, nil
	}
	var present []ArtifactTarget
	for _, t := range ArtifactTargets(root) {
		if fi, err := os.Stat(t.AbsDir); err == nil && fi.IsDir() && !dirIsEmpty(t.AbsDir) {
			present = append(present, t)
		}
	}
	switch len(present) {
	case 0:
		return nil, nil
	case 1:
	default:
		// A workspace that is both Python and Node has two produced
		// directories, and which one an install touched is not knowable from
		// the command string. Capturing both would mean tarring node_modules
		// on every pip install; capturing one would restore half an
		// environment. Refuse.
		names := make([]string, 0, len(present))
		for _, t := range present {
			names = append(names, t.RelDir)
		}
		return nil, fmt.Errorf(
			"artifact: %s each hold installed dependencies and the command does not say which it changed; "+
				"refusing to capture a partial environment", strings.Join(names, " and "))
	}

	a, err := CaptureArtifact(home, root, present[0])
	if err != nil {
		return nil, err
	}
	if err := PutArtifactRef(home, key, a); err != nil {
		return nil, err
	}
	return a, nil
}

func dirIsEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err != nil || len(names) == 0
}

// ---------------------------------------------------------------------------
// Materialize
// ---------------------------------------------------------------------------

// MaterializeOptions controls one restore.
type MaterializeOptions struct {
	// Overwrite permits replacing an existing non-empty directory. Off by
	// default: clobbering a developer's working .venv is not a tradeoff the
	// cache gets to make on its own.
	Overwrite bool
	// Verify re-derives the content hash from the tree that was actually
	// produced and refuses on mismatch.
	Verify bool
	// Timeout bounds the whole operation.
	Timeout time.Duration
}

// DefaultMaterializeOptions is the safe configuration: never clobber, always
// verify, bounded.
func DefaultMaterializeOptions() MaterializeOptions {
	return MaterializeOptions{
		Overwrite: false,
		Verify:    ArtifactVerifyOnMaterialize(),
		Timeout:   ArtifactTimeout(),
	}
}

// MaterializeResult reports what happened, including which copy strategy was
// available. Cloned versus copied is a two- to three-fold difference in cost
// and it should never have to be guessed at from a stopwatch.
type MaterializeResult struct {
	Dir      string
	Method   string // clonefile | reflink | copy
	Files    int
	Bytes    int64
	Rewrote  int
	Verified bool
	Duration time.Duration
}

// Materialize reconstructs the captured directory inside targetRoot.
//
// The sequence is: decide whether relocation is legal, build the whole tree
// beside the target under a temporary name, rewrite the embedded paths,
// re-derive the content hash from what was built, and only then rename it into
// place. An interrupt anywhere before the rename leaves the workspace exactly
// as it was, which is the difference between a cache miss and a half-populated
// .venv that looks installed.
func (a *Artifact) Materialize(home, targetRoot string, opt MaterializeOptions) (MaterializeResult, error) {
	started := time.Now()
	var res MaterializeResult
	if opt.Timeout <= 0 {
		opt.Timeout = ArtifactTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	if a.Version != ArtifactVersion {
		return res, fmt.Errorf("artifact: descriptor v%d, this build understands v%d",
			a.Version, ArtifactVersion)
	}
	if a.GOOS != runtime.GOOS || a.GOARCH != runtime.GOARCH {
		return res, fmt.Errorf("artifact: captured on %s/%s, cannot be restored on %s/%s",
			a.GOOS, a.GOARCH, runtime.GOOS, runtime.GOARCH)
	}

	dstRoot := resolvePath(targetRoot)
	target := filepath.Join(dstRoot, filepath.FromSlash(a.RelDir))
	res.Dir = target

	// Relocation. This is the hazard that makes the feature non-trivial: a
	// virtualenv embeds its own absolute path in every console script's
	// shebang, and an editable install embeds the absolute path of the source
	// tree in a .pth file. Cloned unchanged into another worktree, the scripts
	// fail with a confusing interpreter error and — far worse — Python imports
	// the *other* worktree's source, so five agents silently share one agent's
	// edits.
	relocating := dstRoot != a.SourceRoot
	var fwd, rev *rootReplacer
	if relocating {
		if !a.Relocatable {
			return res, fmt.Errorf("%w: %s", ErrArtifactNotRelocatable, a.Refusal)
		}
		fwd, rev = relocationReplacers(a.SourceRoot, dstRoot)
	}

	if err := checkTargetFree(target, opt.Overwrite); err != nil {
		return res, err
	}

	// A verbatim copy of the captured tree, verified against its content
	// address when it was created, kept in the cache.
	clone := artifactClonePath(home, a.TarSHA)
	if _, err := os.Stat(clone); err != nil {
		if err := a.ensureCloneSource(home, clone, deadline); err != nil {
			return res, err
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return res, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".hs-*")
	if err != nil {
		return res, err
	}
	// MkdirTemp creates the directory; the copy wants to create it itself so a
	// clone of the whole subtree is one operation.
	if err := os.Remove(stage); err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(stage)
		}
	}()

	method, err := cloneOrCopyMethod(ctx, clone, stage)
	if err != nil {
		return res, fmt.Errorf("artifact: could not stage the restore: %w", err)
	}
	res.Method = method

	if relocating {
		n, err := applyRewrites(stage, a.Rewrites, a.SymlinkRewrites, fwd)
		if err != nil {
			return res, err
		}
		res.Rewrote = n
	}

	// The base interpreter is machine-global and correctly shared, so
	// .venv/bin/python stays an absolute symlink out of the tree. That is only
	// right while the interpreter is still there.
	for _, l := range a.ExternalLinks {
		if _, err := os.Stat(l); err != nil {
			return res, fmt.Errorf("artifact: %s points outside the worktree at %s, "+
				"which no longer exists; refusing to restore a broken environment", a.RelDir, l)
		}
	}

	// Re-derive the content hash from the tree that was actually produced,
	// with the relocation substituted back out. Stronger than re-reading the
	// tar: it proves the bytes on disk are the captured bytes, modulo exactly
	// the substitutions we intended, rather than proving the archive we copied
	// from was intact.
	if opt.Verify {
		sum, st, err := hashTree(stage, rev, setOf(a.Rewrites), setOf(a.SymlinkRewrites), deadline)
		if err != nil {
			return res, err
		}
		if sum != a.TarSHA {
			return res, fmt.Errorf("%w: restored %s hashes to %s, recorded %s",
				ErrArtifactCorrupt, a.RelDir, short(sum), short(a.TarSHA))
		}
		res.Files, res.Bytes, res.Verified = st.entries, st.bytes, true
	} else {
		res.Files, res.Bytes = a.Files, a.Bytes
	}

	// Swap. Rename is atomic within a filesystem, and stage is a sibling of
	// target, so there is no instant at which the workspace holds a partial
	// directory.
	var displaced string
	if opt.Overwrite {
		if _, err := os.Stat(target); err == nil {
			displaced = target + ".hs-old-" + randSuffix()
			if err := os.Rename(target, displaced); err != nil {
				return res, err
			}
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if displaced != "" {
			os.Rename(displaced, target)
		}
		return res, err
	}
	committed = true
	if displaced != "" {
		os.RemoveAll(displaced)
	}

	res.Duration = time.Since(started)
	Debugf("artifact: restored %s via %s in %s (%d entries, %d rewritten, verified=%v)",
		a.RelDir, res.Method, res.Duration.Round(time.Millisecond),
		res.Files, res.Rewrote, res.Verified)
	return res, nil
}

// ensureCloneSource extracts the tar into the cache, verifying the content
// hash in the same pass. The directory is renamed into its final name only if
// the hash matched, so its existence is the proof.
func (a *Artifact) ensureCloneSource(home, clone string, deadline time.Time) error {
	tarPath := artifactTarPath(home, a.TarSHA)
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("artifact: snapshot missing: %w", err)
	}
	defer f.Close()

	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(clone), ".unpack-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()

	h := sha256.New()
	if err := extractTar(io.TeeReader(f, h), tmp, deadline); err != nil {
		// Corruption lands wherever it lands. Damage to a header stops the
		// reader before the hash is ever compared, damage to file data does
		// not, and a caller deciding whether to serve should not have to care
		// which: a snapshot that will not unpack is not servable.
		return fmt.Errorf("%w: snapshot %s would not unpack: %v",
			ErrArtifactCorrupt, short(a.TarSHA), err)
	}
	if sum := "sha256:" + hex.EncodeToString(h.Sum(nil)); sum != a.TarSHA {
		return fmt.Errorf("%w: snapshot %s hashes to %s",
			ErrArtifactCorrupt, short(a.TarSHA), short(sum))
	}
	if err := os.Rename(tmp, clone); err != nil {
		// A peer materializing the same artifact won the race. Its copy passed
		// the same check, so it is the same bytes.
		if _, statErr := os.Stat(clone); statErr == nil {
			return nil
		}
		return err
	}
	ok = true
	return nil
}

// checkTargetFree refuses to write over somebody's working directory.
func checkTargetFree(target string, overwrite bool) error {
	fi, err := os.Lstat(target)
	if err != nil {
		return nil // absent, which is the ordinary case
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s exists and is not a directory", ErrArtifactTargetExists, target)
	}
	if dirIsEmpty(target) {
		return os.Remove(target)
	}
	if !overwrite {
		return fmt.Errorf("%w: %s (pass Overwrite to replace it)", ErrArtifactTargetExists, target)
	}
	return nil
}

// RestoreInstall is the hook.go integration in one call.
func RestoreInstall(home, key, targetRoot string) (MaterializeResult, error) {
	a, ok := LookupArtifact(home, key)
	if !ok {
		return MaterializeResult{}, ErrNoArtifact
	}
	return a.Materialize(home, targetRoot, DefaultMaterializeOptions())
}

// ---------------------------------------------------------------------------
// Relocation
// ---------------------------------------------------------------------------

// relocationReplacers builds the forward and reverse substitutions between two
// worktree roots.
//
// Both the raw and the symlink-resolved spelling of each root are substituted,
// because macOS resolves /tmp to /private/tmp and tools write whichever they
// were handed. Longest needle first, so resolving one prefix never eats the
// other's match.
func relocationReplacers(src, dst string) (fwd, rev *rootReplacer) {
	var pairs []rootPair
	seen := map[string]bool{}
	for _, s := range []struct{ a, b string }{
		{src, dst},
		{unresolvePath(src), unresolvePath(dst)},
	} {
		if s.a == "" || s.b == "" || s.a == s.b || seen[s.a] {
			continue
		}
		seen[s.a] = true
		pairs = append(pairs, rootPair{s.a, s.b})
	}

	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	f := append([]rootPair(nil), pairs...)
	// The reverse replacer matches on the destination side, so it is ordered
	// by the length of that side.
	r := make([]rootPair, len(pairs))
	for i, p := range pairs {
		r[i] = rootPair{p.to, p.from}
	}
	sort.SliceStable(r, func(i, j int) bool { return len(r[i].from) > len(r[j].from) })
	return &rootReplacer{pairs: f}, &rootReplacer{pairs: r}
}

type rootPair struct{ from, to string }

// rootReplacer substitutes one worktree root for another, and only where the
// match ends a path component.
//
// The boundary rule is the whole point, and leaving it out is a silent
// corruption rather than a refusal. fleet.sh names its worktrees a1, a2, a3;
// with ten agents, a plain substitution of /fleet/a1 also rewrites every
// mention of /fleet/a10, into a /fleet/b10 that does not exist. The same
// applies to any neighbouring path that merely starts with the root's
// spelling, and the environment it produces fails several commands later with
// nothing pointing back at the cache.
type rootReplacer struct{ pairs []rootPair }

// Replace rewrites s in a single pass, so a substitution's output is never
// itself rescanned. Pairs are tried longest-first at each position.
func (r *rootReplacer) Replace(s string) string {
	if r == nil || len(r.pairs) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		hit := false
		for _, p := range r.pairs {
			if p.from == "" || !strings.HasPrefix(s[i:], p.from) {
				continue
			}
			// /fleet/a1 inside /fleet/a10 is a different directory.
			if end := i + len(p.from); end < len(s) && isPathNameByte(s[end]) {
				continue
			}
			b.WriteString(p.to)
			i += len(p.from)
			hit = true
			break
		}
		if !hit {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// isPathNameByte reports whether c could continue a path component. Anything
// else — a separator, a quote, a newline, the end of the string — ends the
// name and makes the match a real reference to this root.
func isPathNameByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// rootNeedles is what capture scans file contents for.
func rootNeedles(raw, resolved string) []string {
	out := []string{resolved}
	if abs, err := filepath.Abs(raw); err == nil && abs != resolved {
		out = append(out, abs)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// applyRewrites substitutes the worktree path in the files capture flagged.
//
// Only those files. The capture pass already read every byte and recorded
// exactly which entries embed the old root, so a restore touches a handful of
// small scripts rather than re-reading the tree. On a copy-on-write clone that
// is the difference between ten file writes and several thousand.
func applyRewrites(stage string, files, links []string, rep *rootReplacer) (int, error) {
	n := 0
	for _, rel := range files {
		p := filepath.Join(stage, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil {
			return n, fmt.Errorf(
				"artifact: %s was recorded as needing a path rewrite but is missing: %w", rel, err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return n, err
		}
		out := []byte(rep.Replace(string(b)))
		if bytes.Equal(out, b) {
			continue
		}
		// Written in place rather than through a temp file: the whole staging
		// directory is the atomic unit and it is not visible to anyone yet.
		if err := os.WriteFile(p, out, fi.Mode().Perm()); err != nil {
			return n, err
		}
		if err := os.Chmod(p, fi.Mode().Perm()); err != nil {
			return n, err
		}
		if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
			return n, err
		}
		n++
	}
	for _, rel := range links {
		p := filepath.Join(stage, filepath.FromSlash(rel))
		old, err := os.Readlink(p)
		if err != nil {
			return n, fmt.Errorf(
				"artifact: %s was recorded as a symlink needing a rewrite: %w", rel, err)
		}
		updated := rep.Replace(old)
		if updated == old {
			continue
		}
		if err := os.Remove(p); err != nil {
			return n, err
		}
		if err := os.Symlink(updated, p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Copying
// ---------------------------------------------------------------------------

// cloneOrCopyMethod copies src to dst and reports how.
//
// Copy-on-write cloning is what makes a restore cheap: the kernel duplicates
// metadata and shares the blocks until one side writes, so a virtualenv lands
// in a fraction of the time a real copy takes and costs almost no disk.
// Support is detected by trying it, never assumed — the same binary runs on
// APFS, on btrfs and xfs with reflink, and on filesystems with neither.
func cloneOrCopyMethod(ctx context.Context, src, dst string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		// -c is clonefile(2). BSD cp fails rather than falling back, which is
		// what makes this a real detection.
		if err := runCopy(ctx, dst, "cp", "-Rc", src, dst); err == nil {
			return "clonefile", nil
		}
	case "linux":
		// --reflink=always, so a filesystem without support errors instead of
		// silently doing a full copy and letting us report a clone.
		if err := runCopy(ctx, dst, "cp", "-a", "--reflink=always", src, dst); err == nil {
			return "reflink", nil
		}
	}
	os.RemoveAll(dst)
	if err := copyTree(ctx, src, dst); err != nil {
		return "", err
	}
	return "copy", nil
}

func runCopy(ctx context.Context, dst, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dst)
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// copyTree is the portable fallback. Symlinks are recreated, never followed,
// and modes and modification times are preserved so the restored tree hashes
// identically to the captured one.
//
// Modes are set with an explicit chmod rather than through the create mode,
// because the process umask silently clears bits on the way past, and a
// restored file whose group-write bit went missing would fail verification for
// a reason nothing in the logs would explain.
func copyTree(ctx context.Context, src, dst string) error {
	var dirs []pathMode
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, pathMode{out, fi.Mode().Perm()})
			return nil
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, out)
		case fi.Mode().IsRegular():
			if err := copyFile(p, out, fi.Mode().Perm()); err != nil {
				return err
			}
			mt := fi.ModTime().Truncate(time.Second)
			return os.Chtimes(out, mt, mt)
		default:
			return fmt.Errorf("%w: %s", ErrArtifactUnsupported, rel)
		}
	})
	if err != nil {
		return err
	}
	return chmodDeepestFirst(dirs)
}

type pathMode struct {
	path string
	mode os.FileMode
}

// chmodDeepestFirst applies directory modes after their contents are in place.
// A directory restored read-only on the way down would make its own children
// unwritable.
func chmodDeepestFirst(dirs []pathMode) error {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// ---------------------------------------------------------------------------
// The tar: one deterministic serialization, used for capture and for checking
// ---------------------------------------------------------------------------

// excludedFromArtifact names directories that are pure derived caches, held
// out of the snapshot entirely.
//
// __pycache__ is the one that decides whether this feature works at all. A
// .pyc bakes the absolute path of its source into co_filename, so a pip
// virtualenv holds thousands of binary files embedding the worktree path —
// measured at 2,954 of them in a 143 MB venv, against 15 rewritable text
// files. Counted as content they would make every such environment
// unrelocatable, which is to say the guard would refuse precisely the installs
// slow enough to be worth caching.
//
// Holding them out is not a shortcut. PEP 3147 makes __pycache__ a cache by
// contract: CPython ignores a .pyc there whose source is missing, so it can
// never be the only copy of a module. Sourceless distributions put their .pyc
// beside the package instead, and those are captured normally. What is
// restored is a virtualenv that recompiles on first import — the same one pip
// would have produced — rather than one carrying another worktree's paths in
// every traceback.
func excludedFromArtifact(name string) bool { return name == "__pycache__" }

type tarOpts struct {
	maxBytes int64
	maxFiles int
	// scanFor marks entries whose contents or link target contain one of these
	// strings, splitting them into text (rewritable) and binary (refused).
	scanFor []string
	// transform is applied to the contents of the entries named in
	// transformFiles before they are serialized. Used to substitute a
	// relocation back out so a restored tree can be compared to its origin.
	transform      *rootReplacer
	transformFiles map[string]bool
	transformLinks map[string]bool
	deadline       time.Time
}

type tarStats struct {
	entries int
	bytes   int64
	// textHits are rewritable; binaryHits are the refusal.
	textHits      []string
	binaryHits    []string
	linkHits      []string
	externalLinks []string
}

// tarTree writes a deterministic, uncompressed tar of dir.
//
// Determinism matters more than it looks: the tar's hash is the artifact's
// identity, so anything varying between two byte-identical trees would make a
// restore unverifiable. Ownership is dropped, because restoring another user's
// uid is neither possible nor meaningful. Directory and symlink timestamps are
// dropped, because Go cannot portably set a symlink's mtime and a directory's
// mtime changes as it is populated.
//
// Regular files keep their modification time, truncated to a whole second.
// Timestamps are state that tools act on — CPython validates bytecode against
// the whole-second mtime of its source, and every incremental build tool
// compares them — so a snapshot that flattened them would restore a tree that
// behaves differently from the one captured. A second of resolution survives a
// clone, an extraction and a chtimes identically, which is what the content
// hash needs.
func tarTree(w io.Writer, dir string, opt tarOpts) (tarStats, error) {
	var st tarStats
	tw := tar.NewWriter(w)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !opt.deadline.IsZero() && time.Now().After(opt.deadline) {
			return fmt.Errorf("artifact: timed out walking %s", dir)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && excludedFromArtifact(d.Name()) {
			return fs.SkipDir
		}
		name := filepath.ToSlash(rel)

		st.entries++
		if opt.maxFiles > 0 && st.entries > opt.maxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrArtifactTooLarge, opt.maxFiles)
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode()

		switch {
		case mode.IsDir():
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(mode.Perm()),
				ModTime: time.Unix(0, 0), Format: tar.FormatPAX,
			})

		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if containsAny(link, opt.scanFor) {
				st.linkHits = append(st.linkHits, name)
			} else if filepath.IsAbs(link) && len(opt.scanFor) > 0 {
				st.externalLinks = append(st.externalLinks, link)
			}
			if opt.transform != nil && opt.transformLinks[name] {
				link = opt.transform.Replace(link)
			}
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink, Name: name, Linkname: link,
				Mode: int64(mode.Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX,
			})

		case mode.IsRegular():
			return writeRegular(tw, p, name, fi, &st, &opt)

		default:
			// A socket, fifo or device cannot be faithfully reproduced, and a
			// directory we cannot reproduce exactly is not one we may serve.
			return fmt.Errorf("%w: %s is a %s", ErrArtifactUnsupported, name, mode.Type())
		}
	})
	if err != nil {
		tw.Close()
		return st, err
	}
	return st, tw.Close()
}

// writeRegular streams one file into the tar, scanning it on the way past.
//
// Chunked with an overlap window rather than read whole: node_modules holds
// bundled sources tens of megabytes long, and the scan has to catch a match
// straddling a chunk boundary.
func writeRegular(tw *tar.Writer, p, name string, fi os.FileInfo, st *tarStats, opt *tarOpts) error {
	// A rewritten file changes length, so its content has to be settled before
	// the header is written. Only entries capture already flagged take this
	// path, and those are shebang lines and .pth files.
	if opt.transform != nil && opt.transformFiles[name] {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := []byte(opt.transform.Replace(string(b)))
		st.bytes += int64(len(out))
		if opt.maxBytes > 0 && st.bytes > opt.maxBytes {
			return fmt.Errorf("%w: more than %s", ErrArtifactTooLarge, humanBytes(opt.maxBytes))
		}
		if err := tw.WriteHeader(regularHeader(name, int64(len(out)), fi)); err != nil {
			return err
		}
		_, err = tw.Write(out)
		return err
	}

	size := fi.Size()
	st.bytes += size
	if opt.maxBytes > 0 && st.bytes > opt.maxBytes {
		return fmt.Errorf("%w: more than %s", ErrArtifactTooLarge, humanBytes(opt.maxBytes))
	}
	if err := tw.WriteHeader(regularHeader(name, size, fi)); err != nil {
		return err
	}

	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()

	maxOverlap := 0
	for _, n := range opt.scanFor {
		if len(n) > maxOverlap {
			maxOverlap = len(n)
		}
	}
	var (
		buf     = make([]byte, 64<<10)
		overlap []byte
		first   = true
		hit     bool
		binary  bool
		written int64
	)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := tw.Write(chunk); err != nil {
				return err
			}
			written += int64(n)
			if len(opt.scanFor) > 0 {
				if first {
					// git's own heuristic: a NUL in the first block means
					// binary. Good enough, and being wrong only ever moves an
					// artifact from rewritable to refused.
					head := chunk
					if len(head) > 8<<10 {
						head = head[:8<<10]
					}
					binary = bytes.IndexByte(head, 0) >= 0
					first = false
				}
				if !hit {
					window := chunk
					if len(overlap) > 0 {
						window = append(append([]byte{}, overlap...), chunk...)
					}
					hit = containsAnyBytes(window, opt.scanFor)
				}
				if keep := maxOverlap - 1; keep > 0 {
					if len(chunk) < keep {
						keep = len(chunk)
					}
					overlap = append([]byte{}, chunk[len(chunk)-keep:]...)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	// The header committed to a size. A file that grew or shrank while we read
	// it was being written concurrently, and the snapshot is of nothing.
	if written != size {
		return fmt.Errorf("artifact: %s changed while it was being captured", name)
	}
	if hit {
		if binary {
			st.binaryHits = append(st.binaryHits, name)
		} else {
			st.textHits = append(st.textHits, name)
		}
	}
	return nil
}

func regularHeader(name string, size int64, fi os.FileInfo) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: size,
		Mode:    int64(fi.Mode().Perm()),
		ModTime: fi.ModTime().Truncate(time.Second),
		Format:  tar.FormatPAX,
	}
}

// hashTree re-derives an artifact's content address from a directory on disk.
func hashTree(dir string, rev *rootReplacer, files, links map[string]bool, deadline time.Time) (string, tarStats, error) {
	h := sha256.New()
	st, err := tarTree(h, dir, tarOpts{
		transform: rev, transformFiles: files, transformLinks: links, deadline: deadline,
	})
	if err != nil {
		return "", st, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), st, nil
}

// extractTar writes an archive into dir, restoring modes and file times.
func extractTar(r io.Reader, dir string, deadline time.Time) error {
	tr := tar.NewReader(r)
	var dirs []pathMode
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("artifact: timed out extracting into %s", dir)
		}
		clean := path.Clean(hdr.Name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("artifact: snapshot contains an unsafe path %q", hdr.Name)
		}
		out := filepath.Join(dir, filepath.FromSlash(clean))
		mode := os.FileMode(hdr.Mode).Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, pathMode{out, mode})
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			os.Remove(out)
			if err := os.Symlink(hdr.Linkname, out); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			// Explicit, because the create mode above went through the umask.
			if err := os.Chmod(out, mode); err != nil {
				return err
			}
			if err := os.Chtimes(out, hdr.ModTime, hdr.ModTime); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s", ErrArtifactUnsupported, clean)
		}
	}
	return chmodDeepestFirst(dirs)
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func containsAnyBytes(b []byte, needles []string) bool {
	for _, n := range needles {
		if n != "" && bytes.Contains(b, []byte(n)) {
			return true
		}
	}
	return false
}

func dedupe(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func setOf(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// resolvePath is the canonical spelling of a path: absolute, with every
// symlink resolved. Relocation is a string substitution, so the two roots have
// to be spelled the same way or nothing matches.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// unresolvePath undoes the one symlink that matters in practice: macOS
// resolves /tmp to /private/tmp, and tools write whichever spelling they were
// handed.
func unresolvePath(p string) string {
	if runtime.GOOS == "darwin" && strings.HasPrefix(p, "/private/") {
		return strings.TrimPrefix(p, "/private")
	}
	return p
}

func randSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func short(sha string) string {
	h := strings.TrimPrefix(sha, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}
