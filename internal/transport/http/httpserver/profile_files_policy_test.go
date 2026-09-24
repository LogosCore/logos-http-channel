package httpserver

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestPolicy_OneProfilePerYAML(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "examples", "profiles")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob yaml files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no example profile yaml files found")
	}

	reTopProfiles := regexp.MustCompile(`(?m)^profiles\s*:`)
	reListProfileID := regexp.MustCompile(`(?m)^\s*-\s*profile_id\s*:`)

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(b)
		if reTopProfiles.MatchString(s) {
			t.Fatalf("%s violates policy: contains top-level 'profiles:'", f)
		}
		if reListProfileID.MatchString(s) {
			t.Fatalf("%s violates policy: contains list '- profile_id'", f)
		}
	}
}
