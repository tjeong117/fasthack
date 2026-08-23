package hp

import "testing"

// TestDirectoryChangePrefixIsTransparent.
//
// Measured on the sealed corpus, 37% of commands open with `cd`. Treating that
// as an unrecognized head made the chain rule pass the entire command through,
// which cost more than half of all theoretically available hits — the single
// largest gap between what the cache could serve and what it did.
//
// Skipping it is safe inside a chain because the whole command string is part
// of the key, so `cd a && pytest` and `cd b && pytest` never collide.
func TestDirectoryChangePrefixIsTransparent(t *testing.T) {
	serve := []string{
		"cd tests && pytest -q",
		"cd src && cat main.go",
		"cd tests; pytest",
		"cd deeply/nested/dir && grep -rn TODO .",
		"cd tests && pytest -q && cat report.txt",
	}
	for _, cmd := range serve {
		if p, reason := Classify(cmd); p != SERVE {
			t.Errorf("%q classified %s (%s); a cd prefix should be transparent", cmd, p, reason)
		}
	}
}

// TestBareDirectoryChangeIsNeverServed.
//
// On its own the move *is* the effect. Serving a recorded no-op would swallow
// it, and harnesses that persist the working directory between tool calls
// would then run every later command in the wrong place.
func TestBareDirectoryChangeIsNeverServed(t *testing.T) {
	for _, cmd := range []string{"cd", "cd /tmp", "cd ..", "cd tests", "cd tests;"} {
		if p, reason := Classify(cmd); p == SERVE {
			t.Errorf("%q classified SERVE (%s); serving it would discard the directory change",
				cmd, reason)
		}
	}
}

// TestDirectoryChangeDoesNotWeakenTheChainRule: skipping the cd must not let
// anything else through with it.
func TestDirectoryChangeDoesNotWeakenTheChainRule(t *testing.T) {
	cases := []struct {
		cmd  string
		want Policy
	}{
		{"cd .. && curl https://example.com", PASSTHROUGH},
		{"cd tests && rm -rf build", RECORD_ONLY},
		{"cd src && date", PASSTHROUGH},
		{"cd tests && echo hi > out.txt", RECORD_ONLY},
		{"cd tests && git push", PASSTHROUGH},
		{"cd tests && ls -la", PASSTHROUGH},
	}
	for _, tc := range cases {
		if p, reason := Classify(tc.cmd); p != tc.want {
			t.Errorf("%q classified %s (%s), want %s", tc.cmd, p, reason, tc.want)
		}
	}
}

// TestDirectoryChangeRefusesAnythingItCannotRead: the justification for
// skipping a cd is that we know exactly what it does, so anything unusual
// falls back to the normal path rather than being waved through.
func TestDirectoryChangeRefusesAnythingItCannotRead(t *testing.T) {
	for _, cmd := range []string{
		"cd $HOME && ls",
		"cd \"$WORKDIR\" && pytest",
		"cd -P /tmp && ls",
		"cd a b && ls",
		"cd * && ls",
	} {
		if p, reason := Classify(cmd); p == SERVE {
			t.Errorf("%q classified SERVE (%s); a cd we cannot read must not be skipped",
				cmd, reason)
		}
	}
}
