package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeProcessLogBySlug(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("logs", "process"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("logs", "process", "Abc_123.log"), []byte("job output"), 0644); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	serveProcessLogBySlug().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/log/Abc_123.log", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "job output" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q", contentType)
	}
}

func TestServeProcessLogBySlugRejectsTraversal(t *testing.T) {
	for _, path := range []string{"/log/../secret.log", "/log/a/b.log", "/log/missing-extension"} {
		recorder := httptest.NewRecorder()
		serveProcessLogBySlug().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusOK {
			t.Errorf("%s unexpectedly returned 200", path)
		}
	}
}

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
