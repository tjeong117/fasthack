package hp

import (
	"bytes"
	"regexp"
)

// Ported from seed/twomachine/norm.py. Deliberately the same patterns: the
// python version is what the corpus was scrubbed with.
var (
	normTmpRe  = regexp.MustCompile(`/var/folders/[^\s"':,)]+|/tmp/[A-Za-z0-9._\-]*[0-9A-Za-z]{6,}[^\s"':,)]*`)
	normAddrRe = regexp.MustCompile(`0x[0-9a-f]{6,16}`)
	normDurRe  = regexp.MustCompile(`\b\d+\.\d+\s?(s|ms|sec|seconds)\b`)
	normPidRe  = regexp.MustCompile(`(?i)\bpid[= ]\d+`)
)

// Normalize scrubs the parts of command output that legitimately differ
// between two correct runs in different worktrees: absolute paths, temp dirs,
// durations, pids, heap addresses.
//
// Shadow verification diffs normalized output. A raw byte-diff false-positives
// on essentially every pytest run.
//
// The input is never modified.
func Normalize(b []byte, root, home string) []byte {
	// Longest first. A worktree under $HOME means home is a prefix of root, and
	// substituting home first would leave {{HOME}}/repo, which root can never
	// match again.
	first, firstTag := root, []byte("{{ROOT}}")
	second, secondTag := home, []byte("{{HOME}}")
	if len(second) > len(first) {
		first, firstTag, second, secondTag = second, secondTag, first, firstTag
	}
	// An empty root or home would otherwise match at every byte boundary.
	if first != "" {
		b = bytes.ReplaceAll(b, []byte(first), firstTag)
	}
	if second != "" {
		b = bytes.ReplaceAll(b, []byte(second), secondTag)
	}
	b = normTmpRe.ReplaceAll(b, []byte("{{TMP}}"))
	b = normAddrRe.ReplaceAll(b, []byte("{{ADDR}}"))
	b = normDurRe.ReplaceAll(b, []byte("{{DUR}}"))
	b = normPidRe.ReplaceAll(b, []byte("{{PID}}"))
	return b
}
