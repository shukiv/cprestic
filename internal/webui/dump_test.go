package webui_test

import (
	"os"
	"strings"
	"testing"
)

// TestDumpPages is a development aid: it writes every page as WHM would
// receive it, so the rendered result can be checked against WHM's own
// stylesheets outside this process. Off unless CPREST_DUMP names a directory.
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("CPREST_DUMP")
	if dir == "" {
		t.Skip("set CPREST_DUMP to a directory to dump rendered pages")
	}
	client, _, _ := newUI(t)
	for _, path := range []string{"/", "/destinations", "/schedule", "/accounts", "/restore", "/jobs", "/settings"} {
		status, body := get(t, client, path)
		if status != 200 {
			t.Errorf("GET %s = %d", path, status)
			continue
		}
		name := strings.Trim(strings.ReplaceAll(path, "/", "_"), "_")
		if name == "" {
			name = "dashboard"
		}
		if err := os.WriteFile(dir+"/frag-"+name+".html", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
