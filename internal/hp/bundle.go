package hp

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A cache is only useful to the machine that produced it until it can move.
// Making it portable unlocks three things at once, which is why it is worth
// more than it looks: a team can share one engineer's test run, CI can start
// warm from a developer's cache, and an evaluation harness can ship a warm
// cache alongside a benchmark and replay an agent against it with no sandbox
// and — crucially — nothing generated.
//
// A bundle is a gzipped tar of a manifest, the log lines worth carrying, and
// the content-addressed blobs those lines reference.

// BundleVersion is the on-disk format version. Bump it when the layout
// changes; an importer that does not recognise a version refuses rather than
// guessing at the contents.
const BundleVersion = 1

// BundleManifest records where a bundle came from. Provenance is the point: a
// cache is a set of claims about what commands produce, and accepting one is
// trusting whoever produced it.
type BundleManifest struct {
	Version    int       `json:"version"`
	KeyVersion string    `json:"key_version"`
	CreatedAt  time.Time `json:"created_at"`
	SourceGOOS string    `json:"source_goos"`
	SourceArch string    `json:"source_arch"`
	Records    int       `json:"records"`
	Blobs      int       `json:"blobs"`
	BlobBytes  int64     `json:"blob_bytes"`
	// ServableOnly says whether non-servable records were dropped. A bundle
	// exists to be served from, so that is the default.
	ServableOnly bool   `json:"servable_only"`
	Note         string `json:"note,omitempty"`
}

// BundleStats summarises a pack or unpack.
type BundleStats struct {
	Records   int
	Blobs     int
	BlobBytes int64
	Skipped   int
	// Rejected counts blobs whose contents did not match the hash they were
	// filed under.
	Rejected int
}

// PackCache writes a portable bundle of a cache directory.
//
// servableOnly keeps only records the cache would actually serve, which is
// almost always what a recipient wants and is dramatically smaller.
func PackCache(dir string, w io.Writer, servableOnly bool, note string) (BundleStats, error) {
	var st BundleStats

	store, err := OpenStore(dir)
	if err != nil {
		return st, err
	}

	records := store.Records()
	keep := make([]*Record, 0, len(records))
	needed := map[string]bool{}
	for _, r := range records {
		if servableOnly && !r.Servable {
			st.Skipped++
			continue
		}
		keep = append(keep, r)
		for _, b := range []string{r.StdoutBlob, r.StderrBlob} {
			if b != "" {
				needed[b] = true
			}
		}
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// Blobs first, so a truncated bundle fails on a missing manifest rather
	// than appearing complete but under-populated.
	for id := range needed {
		data, err := os.ReadFile(store.BlobPath(id))
		if err != nil {
			st.Skipped++
			continue // a referenced blob we cannot read is a lost hit, not a failure
		}
		if err := writeTarFile(tw, "blobs/"+strings.TrimPrefix(id, "sha256:"), data); err != nil {
			return st, err
		}
		st.Blobs++
		st.BlobBytes += int64(len(data))
	}

	var logBuf strings.Builder
	for _, r := range keep {
		line, err := json.Marshal(r)
		if err != nil {
			continue
		}
		logBuf.Write(line)
		logBuf.WriteByte('\n')
		st.Records++
	}
	if err := writeTarFile(tw, "log.jsonl", []byte(logBuf.String())); err != nil {
		return st, err
	}

	manifest := BundleManifest{
		Version: BundleVersion, KeyVersion: KeyVersion, CreatedAt: time.Now().UTC(),
		SourceGOOS: runtime.GOOS, SourceArch: runtime.GOARCH,
		Records: st.Records, Blobs: st.Blobs, BlobBytes: st.BlobBytes,
		ServableOnly: servableOnly, Note: note,
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return st, err
	}
	if err := writeTarFile(tw, "manifest.json", mb); err != nil {
		return st, err
	}

	if err := tw.Close(); err != nil {
		return st, err
	}
	return st, gz.Close()
}

// UnpackCache merges a bundle into a local cache.
//
// Every blob is re-hashed and rejected if its contents do not match the name
// it was filed under. That is cheap and it matters: a cache is a set of claims
// about what commands produce, so a corrupted or tampered bundle is a
// mechanism for serving a wrong answer. Hash checking catches corruption and
// raises the cost of tampering, but it cannot tell you whether the producer
// was honest — importing a bundle is trusting whoever made it.
func UnpackCache(dir string, r io.Reader, verifyOnly bool) (BundleStats, BundleManifest, error) {
	var st BundleStats
	var manifest BundleManifest

	gz, err := gzip.NewReader(r)
	if err != nil {
		return st, manifest, err
	}
	defer gz.Close()

	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return st, manifest, err
	}

	var logLines []byte
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, manifest, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Reject anything trying to escape the cache directory.
		name := path.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || path.IsAbs(name) {
			return st, manifest, fmt.Errorf("bundle contains an unsafe path: %q", hdr.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxOutputBytes+1))
		if err != nil {
			return st, manifest, err
		}

		switch {
		case name == "manifest.json":
			if err := json.Unmarshal(data, &manifest); err != nil {
				return st, manifest, fmt.Errorf("unreadable manifest: %w", err)
			}
			if manifest.Version != BundleVersion {
				return st, manifest, fmt.Errorf(
					"bundle format v%d, this build understands v%d", manifest.Version, BundleVersion)
			}
			if manifest.KeyVersion != KeyVersion {
				return st, manifest, fmt.Errorf(
					"bundle was built with key version %s, this build uses %s; "+
						"none of its keys could match", manifest.KeyVersion, KeyVersion)
			}
		case name == "log.jsonl":
			logLines = data
		case strings.HasPrefix(name, "blobs/"):
			want := strings.TrimPrefix(name, "blobs/")
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != want {
				st.Rejected++
				continue
			}
			st.Blobs++
			st.BlobBytes += int64(len(data))
			if verifyOnly {
				continue
			}
			dst := StorePaths(dir).BlobPath("sha256:" + want)
			if _, err := os.Stat(dst); err == nil {
				continue // content-addressed, so an existing blob is the same blob
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return st, manifest, err
			}
			tmp := dst + ".tmp"
			if err := os.WriteFile(tmp, data, 0o644); err != nil {
				return st, manifest, err
			}
			if err := os.Rename(tmp, dst); err != nil {
				return st, manifest, err
			}
		}
	}

	if st.Rejected > 0 {
		return st, manifest, fmt.Errorf(
			"%d blob(s) did not match the hash they were filed under; refusing the bundle", st.Rejected)
	}

	for _, line := range strings.Split(string(logLines), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			st.Skipped++
			continue
		}
		st.Records++
	}
	if verifyOnly {
		return st, manifest, nil
	}

	// Append rather than replace: a local cache may already hold work this
	// bundle knows nothing about, and the log is the source of truth that the
	// in-memory indexes are rebuilt from.
	f, err := os.OpenFile(StorePaths(dir).LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return st, manifest, err
	}
	defer f.Close()
	if len(logLines) > 0 && !strings.HasSuffix(string(logLines), "\n") {
		logLines = append(logLines, '\n')
	}
	_, err = f.Write(logLines)
	return st, manifest, err
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0),
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
