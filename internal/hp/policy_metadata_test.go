package hp

import "testing"

// TestFileMetadataIsNeverServed is a regression test for a hole that shadow
// verification found on the first real fleet run.
//
// `ls -la` was classified as a read and served. It diverged, because long
// listings print mtimes, sizes and ownership, and git's tree hash deliberately
// covers none of those. Two worktrees with byte-identical trees legitimately
// produce different output, which means the key cannot dominate the output and
// the command must not be served.
//
// This is the class of bug the classifier cannot reason its way out of and the
// purity gate cannot catch either: the command mutates nothing, so before and
// after state match perfectly. Only the deny-list stops it.
func TestFileMetadataIsNeverServed(t *testing.T) {
	mustNotServe := []string{
		"ls -l",
		"ls -la",
		"ls -lah",
		"ls -al src/",
		"ls -n",
		"ls -g",
		"stat parser.py",
		"du -sh .",
		"df -h",
	}
	for _, cmd := range mustNotServe {
		if p, reason := Classify(cmd); p == SERVE {
			t.Errorf("%q classified SERVE (%s); it prints metadata outside the tree hash", cmd, reason)
		}
	}

	// Plain listings print names only and stay useful to cache.
	mustServe := []string{"ls", "ls src/", "ls --color=auto", "cat parser.py"}
	for _, cmd := range mustServe {
		if p, reason := Classify(cmd); p != SERVE {
			t.Errorf("%q classified %s (%s); it should still be serveable", cmd, p, reason)
		}
	}
}
