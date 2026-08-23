package hp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tamperBlobPayload rewrites a bundle so one blob's contents no longer match
// the hash it is filed under, simulating corruption in transit or a bundle
// someone edited on purpose.
func tamperBlobPayload(t *testing.T, bundle []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(hdr.Name, "blobs/") {
			data = []byte("this is not what was recorded\n")
		}
		if err := writeTarFile(tw, hdr.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeEvilBundle(t *testing.T, w io.Writer) {
	t.Helper()
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	if err := writeTarFile(tw, "../../escaped.txt", []byte("owned")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeBundleWithManifest(t *testing.T, w io.Writer, m BundleManifest) {
	t.Helper()
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, "manifest.json", mb); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedStore(t *testing.T) (dir string, servableKey string) {
	t.Helper()
	dir = t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := store.PutBlob([]byte("128 passed in 4.28s\n"))
	if err != nil {
		t.Fatal(err)
	}
	errb, err := store.PutBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	servableKey = "hs-v1:servable"
	must := func(r *Record) {
		if err := store.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	must(&Record{
		V: 1, Agent: "a1", Key: servableKey, Cmd: "pytest -q", CmdNorm: "pytest -q",
		CwdRel: ".", TreeBefore: "t1", EnvFPBefore: "e1", TreeAfter: "t1", EnvFPAfter: "e1",
		Decision: DecisionMiss, Servable: true, DurationMS: 4280,
		StdoutBlob: out, StderrBlob: errb,
	})
	must(&Record{
		V: 1, Agent: "a1", Key: "hs-v1:dirty", Cmd: "uv sync", CmdNorm: "uv sync",
		CwdRel: ".", TreeBefore: "t1", EnvFPBefore: "e1", TreeAfter: "t1", EnvFPAfter: "e2",
		Decision: DecisionMiss, Servable: false, DurationMS: 900,
		StdoutBlob: out, StderrBlob: errb,
	})
	return dir, servableKey
}

// TestBundleRoundTripServesOnTheOtherSide is the property that makes a cache
// worth moving: a record packed on one machine must actually be servable
// after import on another.
func TestBundleRoundTripServesOnTheOtherSide(t *testing.T) {
	src, key := seedStore(t)

	var buf bytes.Buffer
	packed, err := PackCache(src, &buf, true, "test bundle")
	if err != nil {
		t.Fatal(err)
	}
	if packed.Records != 1 {
		t.Fatalf("servable-only should carry 1 record, got %d", packed.Records)
	}
	if packed.Skipped != 1 {
		t.Fatalf("the unservable record should have been skipped, got %d", packed.Skipped)
	}

	dst := t.TempDir()
	got, manifest, err := UnpackCache(dst, &buf, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Records != 1 {
		t.Fatalf("imported %d records, want 1", got.Records)
	}
	if manifest.KeyVersion != KeyVersion {
		t.Fatalf("manifest key version %q, want %q", manifest.KeyVersion, KeyVersion)
	}
	if manifest.Note != "test bundle" {
		t.Fatalf("provenance note lost: %q", manifest.Note)
	}

	store, err := OpenStore(dst)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Lookup(key)
	if !ok {
		t.Fatal("the imported record is not servable; moving a cache achieved nothing")
	}
	body, err := store.GetBlob(rec.StdoutBlob)
	if err != nil {
		t.Fatalf("imported record references a blob that did not come with it: %v", err)
	}
	if !strings.Contains(string(body), "128 passed") {
		t.Fatalf("blob contents did not survive the round trip: %q", body)
	}
}

func TestBundleAllIncludesUnservableRecords(t *testing.T) {
	src, _ := seedStore(t)
	var buf bytes.Buffer
	st, err := PackCache(src, &buf, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 2 {
		t.Fatalf("--all should carry both records, got %d", st.Records)
	}
}

// TestBundleRejectsTamperedBlob. A cache is a set of claims about what
// commands produce, so a bundle whose contents do not match the hash they are
// filed under is a mechanism for serving a wrong answer.
func TestBundleRejectsTamperedBlob(t *testing.T) {
	src, _ := seedStore(t)
	var buf bytes.Buffer
	if _, err := PackCache(src, &buf, true, ""); err != nil {
		t.Fatal(err)
	}

	tampered := tamperBlobPayload(t, buf.Bytes())

	dst := t.TempDir()
	_, _, err := UnpackCache(dst, bytes.NewReader(tampered), false)
	if err == nil {
		t.Fatal("a tampered blob was accepted; hash checking is the only thing " +
			"standing between an imported cache and a wrong answer")
	}
	if !strings.Contains(err.Error(), "hash") {
		t.Fatalf("error should name the hash mismatch, got: %v", err)
	}
	// Nothing may be installed from a rejected bundle.
	entries, _ := os.ReadDir(filepath.Join(dst, "blobs"))
	for _, e := range entries {
		sub, _ := os.ReadDir(filepath.Join(dst, "blobs", e.Name()))
		if len(sub) > 0 {
			t.Fatal("a rejected bundle still installed blobs")
		}
	}
}

func TestBundleVerifyInstallsNothing(t *testing.T) {
	src, _ := seedStore(t)
	var buf bytes.Buffer
	if _, err := PackCache(src, &buf, true, ""); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	st, _, err := UnpackCache(dst, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 1 || st.Blobs == 0 {
		t.Fatalf("verify should still report contents, got %+v", st)
	}
	if _, err := os.Stat(StorePaths(dst).LogPath()); err == nil {
		t.Fatal("--verify wrote a log; it must install nothing")
	}
}

func TestBundleRefusesUnsafePaths(t *testing.T) {
	var buf bytes.Buffer
	writeEvilBundle(t, &buf)
	_, _, err := UnpackCache(t.TempDir(), &buf, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("a bundle escaping the cache directory must be refused, got: %v", err)
	}
}

// TestBundleRefusesForeignKeyVersion: keys built under a different key version
// cannot match, so importing them would only add noise.
func TestBundleRefusesForeignKeyVersion(t *testing.T) {
	var buf bytes.Buffer
	writeBundleWithManifest(t, &buf, BundleManifest{
		Version: BundleVersion, KeyVersion: "hs-v99", Records: 0,
	})
	_, _, err := UnpackCache(t.TempDir(), &buf, false)
	if err == nil || !strings.Contains(err.Error(), "key version") {
		t.Fatalf("expected a key-version refusal, got: %v", err)
	}
}

func TestBundleRefusesFutureFormat(t *testing.T) {
	var buf bytes.Buffer
	writeBundleWithManifest(t, &buf, BundleManifest{
		Version: BundleVersion + 1, KeyVersion: KeyVersion,
	})
	_, _, err := UnpackCache(t.TempDir(), &buf, false)
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("expected a format-version refusal, got: %v", err)
	}
}

// TestBundleImportIsAdditive: a local cache may hold work the bundle knows
// nothing about, and importing must not discard it.
func TestBundleImportIsAdditive(t *testing.T) {
	src, key := seedStore(t)
	var buf bytes.Buffer
	if _, err := PackCache(src, &buf, true, ""); err != nil {
		t.Fatal(err)
	}

	dst, localKey := seedStore(t)
	// Give the local cache a record the bundle does not have.
	localStore, _ := OpenStore(dst)
	blob, _ := localStore.PutBlob([]byte("local\n"))
	if err := localStore.Append(&Record{
		V: 1, Agent: "local", Key: "hs-v1:local-only", Cmd: "make", CmdNorm: "make",
		CwdRel: ".", TreeBefore: "t9", EnvFPBefore: "e9", TreeAfter: "t9", EnvFPAfter: "e9",
		Decision: DecisionMiss, Servable: true, DurationMS: 100,
		StdoutBlob: blob, StderrBlob: blob,
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := UnpackCache(dst, &buf, false); err != nil {
		t.Fatal(err)
	}
	merged, err := OpenStore(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, localKey, "hs-v1:local-only"} {
		if _, ok := merged.Lookup(k); !ok {
			t.Fatalf("%s is missing after import; the merge dropped existing work", k)
		}
	}
}
