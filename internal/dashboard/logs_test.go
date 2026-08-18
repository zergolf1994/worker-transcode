package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessLogPathRejectsTraversal(t *testing.T) {
	want := filepath.Join("logs", "process", "Abc_123-test.log")
	if got, ok := processLogPath("Abc_123-test"); !ok || got != want {
		t.Fatalf("valid path = %q, %v", got, ok)
	}
	for _, slug := range []string{"../secret", "a/b", `a\\b`, "", "name.log"} {
		if path, ok := processLogPath(slug); ok {
			t.Errorf("unexpected path for %q: %q", slug, path)
		}
	}
}

func TestReadLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	content, truncated, err := readLogTail(path, 4)
	if err != nil || !truncated || string(content) != "6789" {
		t.Fatalf("tail=%q truncated=%v err=%v", content, truncated, err)
	}
}
