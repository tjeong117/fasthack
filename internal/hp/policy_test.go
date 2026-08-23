package hp

import (
	"strings"
	"testing"
)

// reason is a substring the returned reason must contain; "" skips the check.
type classifyCase struct {
	cmd    string
	want   Policy
	reason string
}

var classifyCases = []classifyCase{
	// --- empty and unparseable -------------------------------------------
	{"", PASSTHROUGH, "empty command"},
	{"   ", PASSTHROUGH, "empty command"},
	{";;", PASSTHROUGH, "empty command"},
	{"cat 'unterminated", PASSTHROUGH, "unparseable"},
	{`grep "unterminated x.go`, PASSTHROUGH, "unparseable"},
	{"src/", PASSTHROUGH, "unparseable: no head command"},

	// --- non-hermetic: clocks, network, machine identity -----------------
	{"date", PASSTHROUGH, "non-hermetic: date"},
	{"date +%s", PASSTHROUGH, "non-hermetic: date"},
	{"time make", PASSTHROUGH, "non-hermetic: time"},
	{"sleep 5", PASSTHROUGH, "non-hermetic: sleep"},
	{"curl https://x", PASSTHROUGH, "non-hermetic: curl"},
	{"/usr/bin/curl https://x", PASSTHROUGH, "non-hermetic: curl"},
	{"wget https://x", PASSTHROUGH, "non-hermetic: wget"},
	{"nc -l 8080", PASSTHROUGH, "non-hermetic: nc"},
	{"ssh host ls", PASSTHROUGH, "non-hermetic: ssh"},
	{"scp a host:b", PASSTHROUGH, "non-hermetic: scp"},
	{"ping -c 1 example.com", PASSTHROUGH, "non-hermetic: ping"},
	{"uuidgen", PASSTHROUGH, "non-hermetic: uuidgen"},
	{"hostname", PASSTHROUGH, "non-hermetic: hostname"},
	{"whoami", PASSTHROUGH, "non-hermetic: whoami"},
	{"ps aux", PASSTHROUGH, "non-hermetic: ps"},
	{"top -l 1", PASSTHROUGH, "non-hermetic: top"},
	{"df -h", PASSTHROUGH, "non-hermetic: df"},
	{"free -m", PASSTHROUGH, "non-hermetic: free"},
	{"uptime", PASSTHROUGH, "non-hermetic: uptime"},
	{"env", PASSTHROUGH, "non-hermetic: env"},
	{"printenv PATH", PASSTHROUGH, "non-hermetic: printenv"},
	{"mktemp -d", PASSTHROUGH, "non-hermetic: mktemp"},
	{"tempfile", PASSTHROUGH, "non-hermetic: tempfile"},
	{"echo $RANDOM", PASSTHROUGH, "non-hermetic: $RANDOM"},
	{`echo "$RANDOM"`, PASSTHROUGH, "non-hermetic: $RANDOM"},
	{"cat /dev/urandom", PASSTHROUGH, "non-hermetic: /dev/urandom"},
	{"head -c 10 /dev/random", PASSTHROUGH, "non-hermetic: /dev/random"},
	// The deny-list looks past the head: segmenting never sees this curl.
	{`find . -exec curl http://x \;`, PASSTHROUGH, "non-hermetic: curl"},

	// --- non-hermetic: substitution and pipes into a shell ----------------
	{"cat $(ls)", PASSTHROUGH, "command substitution"},
	{"echo `date`", PASSTHROUGH, "command substitution"},
	{"grep -rn foo $(git ls-files)", PASSTHROUGH, "command substitution"},
	{"foo | sh", PASSTHROUGH, ""},
	{"cat install.sh | bash", PASSTHROUGH, "pipe to shell"},
	{"cat setup | zsh", PASSTHROUGH, "pipe to shell"},

	// --- non-hermetic git -------------------------------------------------
	{"git push", PASSTHROUGH, "non-hermetic: git push"},
	{"git push --force origin main", PASSTHROUGH, "non-hermetic: git push"},
	{"git pull --rebase", PASSTHROUGH, "non-hermetic: git pull"},
	{"git fetch origin", PASSTHROUGH, "non-hermetic: git fetch"},
	{"git clone https://x r", PASSTHROUGH, "non-hermetic: git clone"},
	{"git remote -v", PASSTHROUGH, "non-hermetic: git remote"},

	// --- unrecognized heads and subcommands -------------------------------
	{"docker build .", PASSTHROUGH, "unrecognized head: docker"},
	{"./configure", PASSTHROUGH, "unrecognized head: configure"},
	{"git", PASSTHROUGH, "git with no subcommand"},
	{"git bisect start", PASSTHROUGH, "unrecognized git subcommand: bisect"},
	{"go", PASSTHROUGH, "go with no subcommand"},
	{"go mod tidy", PASSTHROUGH, "unrecognized subcommand: go mod"},
	{"cargo run", PASSTHROUGH, "unrecognized subcommand: cargo run"},
	{"npm install", PASSTHROUGH, "unrecognized subcommand: npm install"},
	{"FOO=1", PASSTHROUGH, "no command after inline env assignments"},
	{"x-y=1 ls", PASSTHROUGH, "unrecognized head: x-y=1"},

	// --- mutations --------------------------------------------------------
	{"rm -rf build", RECORD_ONLY, "mutation: rm"},
	{"mv a b", RECORD_ONLY, "mutation: mv"},
	{"cp a b", RECORD_ONLY, "mutation: cp"},
	{"touch f", RECORD_ONLY, "mutation: touch"},
	{"mkdir -p a/b", RECORD_ONLY, "mutation: mkdir"},
	{"rmdir d", RECORD_ONLY, "mutation: rmdir"},
	{"chmod +x run.sh", RECORD_ONLY, "mutation: chmod"},
	{"chown me f", RECORD_ONLY, "mutation: chown"},
	{"ln -s a b", RECORD_ONLY, "mutation: ln"},
	{"dd if=a of=b", RECORD_ONLY, "mutation: dd"},
	{"truncate -s 0 log", RECORD_ONLY, "mutation: truncate"},
	{"install -m 755 a b", RECORD_ONLY, "mutation: install"},
	{"tee out.txt", RECORD_ONLY, "mutation: tee"},
	{"patch -p1 < fix.diff", RECORD_ONLY, "mutation: patch"},
	{"git apply fix.patch", RECORD_ONLY, "mutation: git apply"},
	{"git checkout main", RECORD_ONLY, "mutation: git checkout"},
	{"git commit -m 'wip'", RECORD_ONLY, "mutation: git commit"},
	{"git add -A", RECORD_ONLY, "mutation: git add"},
	{"sed -i 's/a/b/' a.py", RECORD_ONLY, "mutation: sed -i"},
	{"sed -i.bak 's/a/b/' a.py", RECORD_ONLY, "mutation: sed -i"},
	{"sed --in-place 's/a/b/' a.py", RECORD_ONLY, "mutation: sed -i"},
	{"sed --in-place=.bak 's/a/b/' a.py", RECORD_ONLY, "mutation: sed -i"},
	{"sed -ne '1,5p' a.py", SERVE, "read: sed -n"},
	{"sed --quiet '1,5p' a.py", SERVE, "read: sed -n"},
	{"sed -E 's/a/b/' a.py", RECORD_ONLY, "mutation: sed without -n"},
	{"sed 's/a/b/' a.py", RECORD_ONLY, "mutation: sed without -n"},
	{"black .", RECORD_ONLY, "black rewrites files"},

	// --- redirection is a mutation ----------------------------------------
	{"echo hi > out.txt", RECORD_ONLY, "mutation: output redirection"},
	{"echo hi >> log.txt", RECORD_ONLY, "mutation: output redirection"},
	{"pytest -q > report.txt", RECORD_ONLY, "mutation: output redirection"},
	// ">&N" dups a descriptor rather than writing a file, so it stays
	// serveable. This idiom is common on exactly the expensive commands worth
	// caching, and treating it as a mutation would cost hits for no safety.
	{"go test ./... 2>&1", SERVE, "build: go test"},
	{"pytest -q 2>&1", SERVE, "build: pytest"},
	{"echo hi >&2", SERVE, "read: echo"},
	// "&>file" is a genuine write; the '&' precedes the '>'.
	{"make &> all.log", RECORD_ONLY, "mutation: output redirection"},
	// Strictness still wins over the redirect: this one may never be recorded.
	{"curl https://x > out.txt", PASSTHROUGH, "non-hermetic: curl"},

	// --- reads ------------------------------------------------------------
	{"grep -rn 'foo' src/", SERVE, "read: grep"},
	{"rg TODO", SERVE, "read: rg"},
	{"cat README.md", SERVE, "read: cat"},
	{"ls", SERVE, "read: ls"},
	// A long listing prints mtimes; identical trees diverge. See
	// TestFileMetadataIsNeverServed.
	{"ls -la", PASSTHROUGH, "long format"},
	{"find . -name '*.go'", SERVE, "read: find"},
	{"head -n 20 main.go", SERVE, "read: head"},
	{"tail -n 5 log.txt", SERVE, "read: tail"},
	{"wc -l main.go", SERVE, "read: wc"},
	{"sort names.txt", SERVE, "read: sort"},
	{"uniq -c names.txt", SERVE, "read: uniq"},
	{"cut -d, -f1 data.csv", SERVE, "read: cut"},
	{"awk '{print $1}' data.txt", SERVE, "read: awk"},
	{"diff a.txt b.txt", SERVE, "read: diff"},
	// stat prints mtimes, sizes and inodes, none of which git's tree hash
	// covers, so identical trees can produce different output. See
	// TestFileMetadataIsNeverServed.
	{"stat main.go", PASSTHROUGH, "prints file metadata"},
	{"file main.go", SERVE, "read: file"},
	{"tree -L 2", SERVE, "read: tree"},
	{"basename /a/b.txt", SERVE, "read: basename"},
	{"dirname /a/b.txt", SERVE, "read: dirname"},
	{"realpath .", SERVE, "read: realpath"},
	{"nl main.go", SERVE, "read: nl"},
	{"md5sum main.go", SERVE, "read: md5sum"},
	{"shasum -a 256 main.go", SERVE, "read: shasum"},
	{"echo hi", SERVE, "read: echo"},
	{"pwd", SERVE, "read: pwd"},
	{"which go", SERVE, "read: which"},
	{"sed -n '1,5p' a.py", SERVE, "read: sed -n"},
	// A deny-listed word inside quotes is data, not a command.
	{"grep 'date' notes.txt", SERVE, "read: grep"},
	{`echo '$RANDOM'`, SERVE, "read: echo"},

	// --- reads via git ----------------------------------------------------
	{"git status --porcelain", SERVE, "read: git status"},
	{"git log --oneline -n 5", SERVE, "read: git log"},
	{"git diff HEAD~1", SERVE, "read: git diff"},
	{"git show HEAD", SERVE, "read: git show"},
	{"git blame main.go", SERVE, "read: git blame"},
	{"git ls-files", SERVE, "read: git ls-files"},
	{"git rev-parse --git-dir", SERVE, "read: git rev-parse"},
	{"git describe --tags", SERVE, "read: git describe"},
	{"git branch", SERVE, "read: git branch"},
	{"git tag", SERVE, "read: git tag"},
	{"git cat-file -p HEAD", SERVE, "read: git cat-file"},
	{"git -C /repo status", SERVE, "read: git status"},

	// --- builds, tests, linters -------------------------------------------
	// tsc emits .js and cargo test writes target/. SERVE is correct here; the
	// runtime gate compares tree and env fingerprints and refuses them.
	{"pytest tests/ -q", SERVE, "build: pytest"},
	{"tsc", SERVE, "build: tsc"},
	{"go test ./...", SERVE, "build: go test"},
	{"go build ./cmd/hindsight", SERVE, "build: go build"},
	{"go vet ./...", SERVE, "build: go vet"},
	{"cargo test", SERVE, "build: cargo test"},
	{"cargo build --release", SERVE, "build: cargo build"},
	{"npm test", SERVE, "build: npm test"},
	{"npm run build", SERVE, "build: npm run"},
	{"yarn", SERVE, "build: yarn"},
	{"pnpm install", SERVE, "build: pnpm"},
	{"make -j4", SERVE, "build: make"},
	{"mypy .", SERVE, "build: mypy"},
	{"ruff check .", SERVE, "build: ruff"},
	{"flake8 src", SERVE, "build: flake8"},
	{"black --check .", SERVE, "build: black --check"},
	{"eslint .", SERVE, "build: eslint"},
	{"jest --ci", SERVE, "build: jest"},
	{"vitest run", SERVE, "build: vitest"},
	{"python -m pytest", SERVE, "build: python"},
	{"python3 -c 'import sys;print(sys.version)'", SERVE, "build: python3"},
	{"node index.js", SERVE, "build: node"},
	// Installs are never servable (the env fingerprint always moves), and
	// SERVE would make peers block on a lease and then install anyway.
	{"pip install -r requirements.txt", RECORD_ONLY, "install:"},
	{"uv sync", RECORD_ONLY, "install:"},
	// "uv run X" executes inside the venv rather than changing it.
	{"uv run pytest", SERVE, "pytest"},
	{"npm --silent run build", SERVE, "build: npm run"},
	{"FOO=1 pytest", SERVE, "build: pytest"},
	{"FOO2=1 pytest", SERVE, "build: pytest"},
	{"FOO=1 BAR=2 go test ./...", SERVE, "build: go test"},

	// --- chains: the strictest segment wins -------------------------------
	{"ls && curl x", PASSTHROUGH, "chain: strictest segment is PASSTHROUGH"},
	{"ls && rm -rf build", RECORD_ONLY, "chain: strictest segment is RECORD_ONLY"},
	{"ls; date", PASSTHROUGH, "chain: strictest segment is PASSTHROUGH"},
	{"ls\ndate", PASSTHROUGH, "chain: strictest segment is PASSTHROUGH"},
	{"ls || echo hi", SERVE, "chain: strictest segment is SERVE"},
	{"cat a | grep b", SERVE, "chain: strictest segment is SERVE"},
	{"cat a | grep b > out.txt", RECORD_ONLY, "chain: strictest segment is RECORD_ONLY"},
	{"rm -rf build && go build ./...", RECORD_ONLY, "chain: strictest segment is RECORD_ONLY"},
	{"go build ./... && git push", PASSTHROUGH, "chain: strictest segment is PASSTHROUGH"},
	{"ls;", SERVE, "read: ls"},
	// Separators inside quotes are literal, so these stay single segments.
	{`echo "a && b"`, SERVE, "read: echo"},
	{"echo 'a; date'", SERVE, "read: echo"},
	{`grep -e 'a|b' f.txt`, SERVE, "read: grep"},
	{`echo "a \" b" && ls`, SERVE, "chain: strictest segment is SERVE"},
	{`echo hi\`, SERVE, "read: echo"},
}

func TestClassify(t *testing.T) {
	for _, tc := range classifyCases {
		t.Run(tc.cmd, func(t *testing.T) {
			got, reason := Classify(tc.cmd)
			if got != tc.want {
				t.Errorf("Classify(%q) = %v (%s), want %v", tc.cmd, got, reason, tc.want)
			}
			if reason == "" {
				t.Errorf("Classify(%q) returned an empty reason", tc.cmd)
			}
			if tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Errorf("Classify(%q) reason = %q, want it to contain %q", tc.cmd, reason, tc.reason)
			}
		})
	}
}

// The default has to be PASSTHROUGH: a classification bug must cost a hit,
// never a wrong answer.
func TestClassifyDefaultsToPassthrough(t *testing.T) {
	unknown := []string{
		"kubectl get pods", "terraform apply", "brew install jq",
		"!!", "for f in *; do echo $f; done", "\x00\x01",
	}
	for _, cmd := range unknown {
		if got, reason := Classify(cmd); got != PASSTHROUGH {
			t.Errorf("Classify(%q) = %v (%s), want PASSTHROUGH", cmd, got, reason)
		}
	}
}

func TestClassifyChainStrictestWins(t *testing.T) {
	// Every ordering of the same three segments must agree.
	orderings := []string{
		"ls && rm -rf build && curl x",
		"curl x && ls && rm -rf build",
		"rm -rf build; curl x | ls",
	}
	for _, cmd := range orderings {
		got, reason := Classify(cmd)
		if got != PASSTHROUGH {
			t.Errorf("Classify(%q) = %v, want PASSTHROUGH", cmd, got)
		}
		if !strings.Contains(reason, "chain: strictest segment is PASSTHROUGH") {
			t.Errorf("Classify(%q) reason = %q, want the chain prefix", cmd, reason)
		}
	}
	if got, _ := Classify("ls && rm -rf build"); got != RECORD_ONLY {
		t.Errorf("dropping the non-hermetic segment should leave RECORD_ONLY, got %v", got)
	}
	if got, _ := Classify("ls && cat f"); got != SERVE {
		t.Errorf("two reads should chain to SERVE, got %v", got)
	}
}

// Classify is keyed on nothing but its argument. Same string in, same policy
// out, every time.
func TestClassifyIsDeterministic(t *testing.T) {
	for _, tc := range classifyCases {
		first, firstReason := Classify(tc.cmd)
		for i := 0; i < 3; i++ {
			got, reason := Classify(tc.cmd)
			if got != first || reason != firstReason {
				t.Fatalf("Classify(%q) drifted: %v/%q then %v/%q",
					tc.cmd, first, firstReason, got, reason)
			}
		}
	}
}

func TestClassifyPolicyString(t *testing.T) {
	cases := []struct {
		p    Policy
		want string
	}{
		{PASSTHROUGH, "PASSTHROUGH"},
		{RECORD_ONLY, "RECORD_ONLY"},
		{SERVE, "SERVE"},
		{Policy(99), "PASSTHROUGH"},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("Policy(%d).String() = %q, want %q", int(tc.p), got, tc.want)
		}
	}
	// The strictness ordering the chain rule relies on.
	if !(PASSTHROUGH < RECORD_ONLY && RECORD_ONLY < SERVE) {
		t.Fatal("policy constants must stay ordered PASSTHROUGH < RECORD_ONLY < SERVE")
	}
}
