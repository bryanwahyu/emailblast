package checkpoint

import (
	"path/filepath"
	"testing"
)

func TestDoneAndHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.log")
	cp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Has("u1") {
		t.Fatal("fresh checkpoint should not have u1")
	}
	if err := cp.Done("u1"); err != nil {
		t.Fatal(err)
	}
	if !cp.Has("u1") {
		t.Fatal("u1 should be marked done")
	}
	if cp.Count() != 1 {
		t.Fatalf("count=%d want 1", cp.Count())
	}
}

// TestResumeReload proves progress survives process restarts: write, close,
// reopen the same file, and confirm prior IDs are still known.
func TestResumeReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.log")

	cp1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := cp1.Done(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := cp1.Close(); err != nil { // flush to disk
		t.Fatal(err)
	}

	cp2, err := Open(path) // simulate a rerun
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	for _, id := range []string{"a", "b", "c"} {
		if !cp2.Has(id) {
			t.Errorf("resumed checkpoint missing %q", id)
		}
	}
	if cp2.Has("d") {
		t.Error("unexpected id d")
	}
}

// TestFlushPersistsWithoutClose proves Flush durably writes so a mid-run crash
// (no clean Close) still resumes what was flushed.
func TestFlushPersistsWithoutClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.log")

	cp1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp1.Done("x"); err != nil {
		t.Fatal(err)
	}
	if err := cp1.Flush(); err != nil { // NO Close — simulate hard kill after flush
		t.Fatal(err)
	}

	cp2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	if !cp2.Has("x") {
		t.Error("flushed id x not recovered")
	}
}

// TestEmptyPathDisabled confirms an empty path disables persistence safely.
func TestEmptyPathDisabled(t *testing.T) {
	cp, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Done("x"); err != nil {
		t.Fatal(err)
	}
	if cp.Has("x") != true {
		t.Error("in-memory set should still track within the run")
	}
	if err := cp.Flush(); err != nil {
		t.Error(err)
	}
	if err := cp.Close(); err != nil {
		t.Error(err)
	}
}
