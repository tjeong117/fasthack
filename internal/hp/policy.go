package hp

import (
	"path"
	"strings"
	"unicode"
)

// Policy is the frozen three-value classification from AGENTS.md.
//
// The word "deny" is deliberately absent: in Codex, permissionDecision "deny"
// blocks the tool call outright, and an agent that cannot run curl is broken.
type Policy int

const (
	// PASSTHROUGH runs the command normally and records nothing.
	PASSTHROUGH Policy = iota
	// RECORD_ONLY runs the command normally but records it for the corpus.
	RECORD_ONLY
	// SERVE marks the command eligible to be served, subject to the purity gate.
	SERVE
)

func (p Policy) String() string {
	switch p {
	case SERVE:
		return "SERVE"
	case RECORD_ONLY:
		return "RECORD_ONLY"
	default:
		return "PASSTHROUGH"
	}
}

// nonHermeticHeads can print something different on two runs at an identical
// workspace state. State hashing is blind to them, so no amount of measurement
// makes them servable. This list is correctness, not polish.
var nonHermeticHeads = map[string]bool{
	"date": true, "time": true, "sleep": true,
	"curl": true, "wget": true, "nc": true, "ssh": true, "scp": true, "ping": true,
	"uuidgen": true, "hostname": true, "whoami": true,
	"ps": true, "top": true, "df": true, "free": true, "uptime": true,
	"env": true, "printenv": true,
	"mktemp": true, "tempfile": true,
}

// mutationHeads change the workspace. They run normally and are recorded for
// the phase-2 transition corpus, but are never served.
var mutationHeads = map[string]bool{
	"rm": true, "mv": true, "cp": true, "touch": true, "mkdir": true, "rmdir": true,
	"chmod": true, "chown": true, "ln": true, "dd": true, "truncate": true,
	"install": true, "tee": true, "patch": true,
}

// readHeads are plausible reads.
var readHeads = map[string]bool{
	"grep": true, "rg": true, "cat": true, "ls": true, "find": true,
	"head": true, "tail": true, "wc": true, "sort": true, "uniq": true, "cut": true,
	"awk": true, "diff": true, "file": true, "tree": true,
	"basename": true, "dirname": true, "realpath": true, "nl": true,
	"md5sum": true, "shasum": true, "echo": true, "pwd": true, "which": true,
}

// metadataHeads print file metadata — mtimes, sizes, inodes, ownership — that
// git's tree hash deliberately does not cover. Two worktrees with byte-identical
// trees legitimately produce different output here, so the key cannot dominate
// the output and these must never be served.
//
// Found by shadow verification on the first real fleet run: `ls -la` was being
// served and diverged. Plain `ls` prints names only and stays serveable.
var metadataHeads = map[string]bool{
	"stat": true, "du": true, "df": true,
}

// longListing reports whether an ls invocation will print timestamps.
func longListing(toks []shellToken) bool {
	for _, t := range toks {
		a := t.text
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		if strings.ContainsAny(a[1:], "lgn") {
			return true
		}
	}
	return false
}

// buildHeads are test, build and lint commands. Several of them write to the
// tree — tsc emits .js, cargo test writes target/, pip install moves the
// interpreter's site-packages out from under us — and SERVE is still the right
// answer here. The runtime gate compares the tree hash and env fingerprint
// around execution and refuses to serve anything that moved. This table is a
// cheap pre-filter, not a purity proof.
var buildHeads = map[string]bool{
	"pytest": true, "make": true, "tsc": true, "mypy": true, "ruff": true,
	"flake8": true, "eslint": true, "jest": true, "vitest": true,
	"python": true, "python3": true, "node": true,
	"yarn": true, "pnpm": true,
}

// installHeads move the interpreter's packages out from under us. The purity
// gate always refuses to serve them, because the env fingerprint changes, so
// SERVE would be a lie that costs real time rather than merely a missed hit:
// the first agent takes a lease, its peers block for the whole install, the
// record comes back unservable, and they then install anyway. Four agents wait
// and then pay, which is strictly worse than letting them run in parallel.
//
// RECORD_ONLY keeps them out of the lease path entirely. They flip back to
// SERVE when the artifact guard exists and can replay the produced directory
// instead of just the transcript.
// innerHead returns the command a wrapper like "uv run" is about to execute.
func innerHead(toks []shellToken, sub string) string {
	for i, t := range toks {
		if t.text == sub && i+1 < len(toks) {
			for _, next := range toks[i+1:] {
				if strings.HasPrefix(next.text, "-") {
					continue
				}
				return cmdBase(next.text)
			}
		}
	}
	return ""
}

var installHeads = map[string]bool{
	"pip": true, "pip3": true, "uv": true, "poetry": true, "conda": true,
}

var gitReadSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true,
	"ls-files": true, "rev-parse": true, "describe": true, "branch": true,
	"tag": true, "cat-file": true,
}

var gitMutationSubs = map[string]bool{
	"apply": true, "checkout": true, "commit": true, "add": true,
}

var gitNonHermeticSubs = map[string]bool{
	"push": true, "pull": true, "fetch": true, "clone": true, "remote": true,
}

// subcommandHeads gate a head on its first subcommand: go test is a build,
// go mod tidy is something we have no opinion about.
var subcommandHeads = map[string]map[string]bool{
	"go":    {"test": true, "build": true, "vet": true},
	"cargo": {"test": true, "build": true},
	"npm":   {"test": true, "run": true},
}

var pipeShells = map[string]bool{"sh": true, "bash": true, "zsh": true}

// Classify is a pure function of the command string. It never consults the
// filesystem, the daemon, the environment, or the clock.
//
// The chain rule: split on &&, ||, ;, | and newline, classify each segment, and
// let the strictest one win. PASSTHROUGH is stricter than RECORD_ONLY, which is
// stricter than SERVE, which is the ordering the Policy constants already have,
// so "strictest" is just the minimum.
func Classify(cmd string) (Policy, string) {
	if strings.TrimSpace(cmd) == "" {
		return PASSTHROUGH, "empty command"
	}
	// Checked before splitting: a substitution can span the separators.
	if hasCommandSubstitution(cmd) {
		return PASSTHROUGH, "non-hermetic: command substitution"
	}

	worst := SERVE
	reason := ""
	segments := 0
	for _, seg := range splitSegments(cmd) {
		if strings.TrimSpace(seg.text) == "" {
			continue
		}
		segments++
		p, r := classifySegment(seg)
		if reason == "" || p < worst {
			worst, reason = p, r
		}
	}
	if segments == 0 {
		return PASSTHROUGH, "empty command"
	}
	if segments > 1 {
		return worst, "chain: strictest segment is " + worst.String() + " (" + reason + ")"
	}
	return worst, reason
}

func classifySegment(seg shellSegment) (Policy, string) {
	toks, redirect, ok := tokenizeSegment(seg.text)
	if !ok {
		return PASSTHROUGH, "unparseable: unterminated quote"
	}
	for len(toks) > 0 && isEnvAssignment(toks[0].text) {
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return PASSTHROUGH, "no command after inline env assignments"
	}
	head := cmdBase(toks[0].text)
	if head == "" {
		return PASSTHROUGH, "unparseable: no head command"
	}

	if seg.sep == "|" && pipeShells[head] {
		return PASSTHROUGH, "non-hermetic: pipe to shell"
	}
	if w := nonHermeticWord(toks); w != "" {
		return PASSTHROUGH, "non-hermetic: " + w
	}

	p, reason := classifyHead(head, toks)
	if redirect && p == SERVE {
		return RECORD_ONLY, "mutation: output redirection"
	}
	return p, reason
}

func classifyHead(head string, toks []shellToken) (Policy, string) {
	switch head {
	case "git":
		return classifyGit(toks)
	case "sed":
		return classifySed(toks)
	case "black":
		if segHasFlag(toks, "--check") {
			return SERVE, "build: black --check"
		}
		return RECORD_ONLY, "mutation: black rewrites files without --check"
	}
	if subs, ok := subcommandHeads[head]; ok {
		sub := firstSubcommand(toks)
		switch {
		case sub == "":
			return PASSTHROUGH, "unrecognized: " + head + " with no subcommand"
		case subs[sub]:
			return SERVE, "build: " + head + " " + sub
		}
		return PASSTHROUGH, "unrecognized subcommand: " + head + " " + sub
	}
	if mutationHeads[head] {
		return RECORD_ONLY, "mutation: " + head
	}
	if metadataHeads[head] {
		return PASSTHROUGH, "prints file metadata the tree hash does not cover: " + head
	}
	if head == "ls" && longListing(toks[1:]) {
		return PASSTHROUGH, "ls long format prints mtimes the tree hash does not cover"
	}
	if installHeads[head] {
		// "uv run pytest" and "poetry run pytest" execute inside the venv
		// rather than changing it, so they are whatever the inner command is.
		if sub := firstSubcommand(toks); sub == "run" || sub == "tool" {
			if inner := innerHead(toks, sub); inner != "" {
				p, reason := classifyHead(inner, toks)
				return p, head + " " + sub + " -> " + reason
			}
		}
		return RECORD_ONLY, "install: never servable without the artifact guard"
	}
	if readHeads[head] {
		return SERVE, "read: " + head
	}
	if buildHeads[head] {
		return SERVE, "build: " + head
	}
	return PASSTHROUGH, "unrecognized head: " + head
}

func classifyGit(toks []shellToken) (Policy, string) {
	sub := ""
	for i := 1; i < len(toks); i++ {
		t := toks[i].text
		if strings.HasPrefix(t, "-") {
			// -C <dir> and -c <k=v> take a value that is not the subcommand.
			if t == "-C" || t == "-c" {
				i++
			}
			continue
		}
		sub = t
		break
	}
	switch {
	case sub == "":
		return PASSTHROUGH, "unrecognized: git with no subcommand"
	case gitNonHermeticSubs[sub]:
		return PASSTHROUGH, "non-hermetic: git " + sub
	case gitMutationSubs[sub]:
		return RECORD_ONLY, "mutation: git " + sub
	case gitReadSubs[sub]:
		return SERVE, "read: git " + sub
	}
	return PASSTHROUGH, "unrecognized git subcommand: " + sub
}

// classifySed splits the one head whose policy is decided entirely by a flag:
// sed -n prints, sed -i rewrites the file in place.
func classifySed(toks []shellToken) (Policy, string) {
	sawN := false
	for _, t := range toks[1:] {
		if t.quoted || !strings.HasPrefix(t.text, "-") {
			continue
		}
		body := strings.TrimPrefix(t.text, "-")
		if body == "-in-place" || strings.HasPrefix(body, "-in-place=") {
			return RECORD_ONLY, "mutation: sed -i"
		}
		if strings.HasPrefix(body, "-") {
			if body == "-quiet" || body == "-silent" {
				sawN = true
			}
			continue
		}
		// Short flags bundle, and -i takes an optional suffix: -i.bak, -ni.
		if j := strings.IndexByte(body, '.'); j >= 0 {
			body = body[:j]
		}
		if strings.ContainsRune(body, 'i') {
			return RECORD_ONLY, "mutation: sed -i"
		}
		if strings.ContainsRune(body, 'n') {
			sawN = true
		}
	}
	if sawN {
		return SERVE, "read: sed -n"
	}
	return RECORD_ONLY, "mutation: sed without -n"
}

// nonHermeticWord looks past the head as well, because find -exec curl and
// cat /dev/urandom are exactly as unservable as curl and are not caught by
// segmenting. Quoted arguments are data, not command names, so grep 'date' f
// stays a read.
func nonHermeticWord(toks []shellToken) string {
	for i, t := range toks {
		if strings.Contains(t.bare, "$RANDOM") {
			return "$RANDOM"
		}
		if strings.Contains(t.text, "/dev/urandom") {
			return "/dev/urandom"
		}
		if strings.Contains(t.text, "/dev/random") {
			return "/dev/random"
		}
		if i > 0 && (t.quoted || strings.HasPrefix(t.text, "-")) {
			continue
		}
		if b := cmdBase(t.text); nonHermeticHeads[b] {
			return b
		}
	}
	return ""
}

func firstSubcommand(toks []shellToken) string {
	for i := 1; i < len(toks); i++ {
		if strings.HasPrefix(toks[i].text, "-") {
			continue
		}
		return toks[i].text
	}
	return ""
}

func segHasFlag(toks []shellToken, flag string) bool {
	for _, t := range toks[1:] {
		if t.text == flag {
			return true
		}
	}
	return false
}

// cmdBase reduces /usr/bin/curl to curl. A token ending in / is a directory
// argument, never a command, so ls env/ is not mistaken for env.
func cmdBase(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return ""
	}
	if strings.Contains(s, "/") {
		return path.Base(s)
	}
	return s
}

// isEnvAssignment matches a leading FOO=1 so that FOO=1 pytest classifies on
// pytest. The assignment stays in the command elsewhere; it is only skipped
// while looking for the head.
func isEnvAssignment(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case j > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// hasCommandSubstitution reports $(...) or backticks outside single quotes.
// $((1+2)) trips it too; over-triggering costs a cache hit, and PASSTHROUGH is
// the safe default.
func hasCommandSubstitution(cmd string) bool {
	inSingle, inDouble := false, false
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && !inSingle && i+1 < len(rs) {
			i++
			continue
		}
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle:
		case c == '`':
			return true
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			return true
		}
	}
	return false
}

// shellSegment is one link of a command chain, tagged with the separator that
// introduced it. The separator matters: a pipe into bash is non-hermetic, and
// the same bash after && is merely unrecognized.
type shellSegment struct {
	sep  string // "", "&&", "||", ";" or "|"
	text string
}

func splitSegments(cmd string) []shellSegment {
	var segs []shellSegment
	var b strings.Builder
	var quote rune
	sep := ""
	flush := func(next string) {
		segs = append(segs, shellSegment{sep: sep, text: b.String()})
		b.Reset()
		sep = next
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			b.WriteRune(c)
			if c == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				b.WriteRune(rs[i])
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '\\' && i+1 < len(rs):
			b.WriteRune(c)
			i++
			b.WriteRune(rs[i])
		case c == '\'' || c == '"':
			quote = c
			b.WriteRune(c)
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
			i++
			flush("&&")
		case c == '|' && i+1 < len(rs) && rs[i+1] == '|':
			i++
			flush("||")
		case c == '|':
			flush("|")
		case c == ';', c == '\n':
			flush(";")
		default:
			b.WriteRune(c)
		}
	}
	flush("")
	return segs
}

// shellToken carries enough quoting history to answer two different questions.
// text is the value with quotes removed, for path and flag matching. bare is
// only the part that was outside single quotes, which is the part the shell
// would expand: echo "$RANDOM" is non-hermetic and echo '$RANDOM' is not.
type shellToken struct {
	text   string
	bare   string
	quoted bool
}

func tokenizeSegment(seg string) (toks []shellToken, redirect bool, ok bool) {
	var text, bare strings.Builder
	quoted, have := false, false
	flush := func() {
		if !have {
			return
		}
		toks = append(toks, shellToken{text: text.String(), bare: bare.String(), quoted: quoted})
		text.Reset()
		bare.Reset()
		quoted, have = false, false
	}
	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			have, quoted = true, true
			i++
			for ; i < len(rs) && rs[i] != '\''; i++ {
				text.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, false, false
			}
		case '"':
			have, quoted = true, true
			i++
			closed := false
			for ; i < len(rs); i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
					text.WriteRune(rs[i])
					bare.WriteRune(rs[i])
					continue
				}
				if rs[i] == '"' {
					closed = true
					break
				}
				text.WriteRune(rs[i])
				bare.WriteRune(rs[i])
			}
			if !closed {
				return nil, false, false
			}
		case '\\':
			if i+1 < len(rs) {
				i++
				have, quoted = true, true
				text.WriteRune(rs[i])
				bare.WriteRune(rs[i])
			}
		case '>':
			flush()
			if i+1 < len(rs) && rs[i+1] == '>' {
				i++
			}
			// ">&N" and ">&-" duplicate or close a file descriptor. They write
			// no file and mutate nothing, so they are not a redirection in the
			// sense that matters here. This is worth special-casing because
			// "2>&1" is a very common idiom on exactly the expensive commands
			// most worth caching, and treating it as a mutation would cost
			// real hits for no safety.
			//
			// Note "&>file" is unaffected: it is a genuine write, and the '&'
			// there precedes the '>' rather than following it.
			if i+2 < len(rs) && rs[i+1] == '&' && (unicode.IsDigit(rs[i+2]) || rs[i+2] == '-') {
				i += 2
				continue
			}
			redirect = true
		case '<':
			flush()
		default:
			have = true
			text.WriteRune(c)
			bare.WriteRune(c)
		}
	}
	flush()
	return toks, redirect, true
}
