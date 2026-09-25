package testgit

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRealGit_IgnoresWrapperOnPATH is a resolution-only assertion: it never
// runs the resolved binary, let alone through the fake CLI forwarding chain.
// Doing so live would recurse the test runner itself - the exact failure
// this package exists to prevent (issue #5).
func TestRealGit_IgnoresWrapperOnPATH(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("/usr/bin/git not present on this host")
	}

	// The operator override must not skew this PATH-isolation assertion.
	t.Setenv("NM_TESTGIT_PATH", "")

	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := RealGit()
	if err != nil {
		t.Fatalf("RealGit() error = %v", err)
	}
	if got == wrapper {
		t.Fatalf("RealGit() returned the PATH-shadowing wrapper %q, want an absolute real-git path", got)
	}
	if got != "/usr/bin/git" {
		t.Fatalf("RealGit() = %q, want /usr/bin/git", got)
	}
}

// TestRealGit_EnvOverride covers the NM_TESTGIT_PATH operator override:
// resolution-only, and an unusable override fails closed instead of falling
// back to a location that might hold a wrapper.
func TestRealGit_EnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(override, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TESTGIT_PATH", override)

	got, err := RealGit()
	if err != nil {
		t.Fatalf("RealGit() error = %v", err)
	}
	if got != override {
		t.Fatalf("RealGit() = %q, want override %q", got, override)
	}

	t.Setenv("NM_TESTGIT_PATH", filepath.Join(t.TempDir(), "missing-git"))
	if _, err := RealGit(); err == nil {
		t.Fatal("RealGit() with an unusable NM_TESTGIT_PATH succeeded, want fail-closed error")
	}
}
