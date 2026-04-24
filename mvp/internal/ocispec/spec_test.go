package ocispec

import "testing"

// TestLoadSampleConfig checks the sample config.json parses cleanly and
// exposes the fields the runtime consumes. We keep this test minimal —
// full field-by-field coverage would duplicate the struct definition.
func TestLoadSampleConfig(t *testing.T) {
	s, err := Load("testdata/config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.OCIVersion == "" {
		t.Error("ociVersion missing")
	}
	if s.Process == nil || len(s.Process.Args) == 0 {
		t.Fatal("process.args missing")
	}
	if s.Process.Args[0] != "/bin/sh" {
		t.Errorf("unexpected process.args[0]: %q", s.Process.Args[0])
	}
	if s.Root == nil || s.Root.Path != "rootfs" {
		t.Errorf("unexpected root.path: %+v", s.Root)
	}
	if s.Linux == nil || s.Linux.Resources == nil || s.Linux.Resources.Memory == nil {
		t.Fatal("linux.resources.memory missing")
	}
	if got := s.Linux.Resources.Memory.Limit; got != 67108864 {
		t.Errorf("memory.limit = %d, want 67108864", got)
	}
}

func TestLoadRejectsBadSpec(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.json"); err == nil {
		t.Error("expected error for missing file")
	}
}
