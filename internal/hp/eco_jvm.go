package hp

import (
	"hash"
	"path/filepath"
)

func init() { RegisterEcosystem(jvmEcosystem{}) }

// jvmEcosystem covers Maven and Gradle.
//
// This is the weakest of the five, and it is worth being explicit about why.
// Maven and Gradle resolve into caches that are global to the machine
// (~/.m2/repository, ~/.gradle/caches) and neither writes a per-workspace
// record of what it resolved, so there is no installed set to list the way
// node_modules or vendor/bundle can be listed. The manifests are the honest
// signal: they are what determines resolution, and they are all we can read
// for a millisecond.
//
// Residual gap, stated rather than papered over: a -SNAPSHOT or a Gradle
// dynamic version resolves to different bytes at different times under a
// byte-identical manifest, and we cannot see that. Detecting it would mean
// parsing pom.xml and Groovy, and a substring check does not work because
// nearly every Maven project in development carries -SNAPSHOT as its own
// version, which says nothing about its dependencies. Abstaining on that
// substring would disable the cache for every Java project in exchange for
// almost no real safety, so it is documented here instead.
type jvmEcosystem struct{}

func (jvmEcosystem) Name() string { return "jvm" }

func (jvmEcosystem) Detect(root string) bool {
	return exists(
		filepath.Join(root, "pom.xml"),
		filepath.Join(root, "build.gradle"),
		filepath.Join(root, "build.gradle.kts"),
	)
}

// jvmManifests determine what gets resolved. settings.gradle decides which
// modules exist at all, so it belongs here with the build scripts.
var jvmManifests = []string{
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"settings.gradle",
	"settings.gradle.kts",
}

// jvmPinned are the files that actually pin versions when a project bothers to
// pin them. gradle-wrapper.properties is in the list because it selects which
// Gradle build runs; the wrapper then fetches that version into a global cache,
// so this file is the only per-workspace trace of the toolchain in use.
var jvmPinned = []string{
	"gradle.lockfile",
	"gradle/libs.versions.toml",
	"gradle/wrapper/gradle-wrapper.properties",
}

func (jvmEcosystem) Fingerprint(root string, h hash.Hash) error {
	hashFiles(root, h, jvmManifests...)
	pinned := hashFiles(root, h, jvmPinned...)

	// The only per-workspace evidence that resolution has happened: Maven's
	// target/, Gradle's build/, and Gradle's per-project .gradle/ state. Top
	// level only — these are build output directories and can be enormous.
	// Listing them also means a build shows up as a change in the fingerprint,
	// which is exactly what the purity gate needs to mark that build
	// unservable.
	var local bool
	for _, d := range []string{"target", "build", ".gradle"} {
		present, err := hashDirIfPresent(d, filepath.Join(root, d), h)
		if err != nil {
			return err
		}
		local = local || present
	}

	if !pinned && !local {
		return ErrNotInstalled
	}
	return nil
}
