package main_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildCGI compiles the plugin once for the tests that exercise it as WHM
// would: as a CGI process, driven entirely by environment variables.
func buildCGI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "cprest.cgi")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cgi: %v\n%s", err, output)
	}
	return binary
}

func runCGI(t *testing.T, binary string, env map[string]string) string {
	t.Helper()

	script := "/cgi/cprest.cgi"
	if value, given := env["SCRIPT_NAME"]; given {
		script = value
	}
	// Go's CGI server prefers REQUEST_URI when it is set, so it has to
	// carry the query string or the parameters never arrive — which is
	// also how a real web server sends it.
	requestURI := script
	if query := env["QUERY_STRING"]; query != "" {
		requestURI += "?" + query
	}
	if value, given := env["REQUEST_URI"]; given {
		requestURI = value
	}

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"GATEWAY_INTERFACE=CGI/1.1",
		"REQUEST_METHOD=GET",
		"SERVER_PROTOCOL=HTTP/1.1",
		"SCRIPT_NAME="+script,
		"REQUEST_URI="+requestURI,
	)
	for key, value := range env {
		if key == "REQUEST_URI" || key == "SCRIPT_NAME" {
			continue
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, _ := cmd.CombinedOutput()
	return string(output)
}

func TestNonRootWHMUserIsRefused(t *testing.T) {
	binary := buildCGI(t)

	// cprest can read and delete every backup on the server. A reseller
	// with a WHM login must not reach it, whatever the AppConfig
	// registration says.
	for _, user := range []string{"reseller", "someuser", ""} {
		output := runCGI(t, binary, map[string]string{"REMOTE_USER": user})
		if !strings.Contains(output, "403") {
			t.Errorf("WHM user %q was not refused:\n%s", user, output)
		}
		if !strings.Contains(output, "root WHM account only") {
			t.Errorf("WHM user %q got no explanation:\n%s", user, output)
		}
	}
}

func TestRootWithNoServiceGetsAnExplanation(t *testing.T) {
	binary := buildCGI(t)

	// The socket does not exist in a test environment, which is the same
	// thing an operator sees before the service is started.
	output := runCGI(t, binary, map[string]string{"REMOTE_USER": "root"})
	if !strings.Contains(output, "not running") {
		t.Errorf("output does not explain that the service is down:\n%s", output)
	}
	if !strings.Contains(output, "systemctl start cprest") {
		t.Errorf("output does not say how to fix it:\n%s", output)
	}
}

// TestProxiesToTheSocket checks the one thing the plugin actually does.
//
// The socket path is a compile-time constant so that a writable config
// cannot redirect the plugin, which means this test builds a variant
// pointing at a temporary socket.
func TestProxiesToTheSocket(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "ui.sock")

	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(source),
		`const socketPath = "/var/run/cprest/ui.sock"`,
		`const socketPath = "`+socket+`"`, 1)
	if patched == string(source) {
		t.Fatal("could not point the plugin at a test socket")
	}

	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	writeModule(t, buildDir)

	binary := filepath.Join(buildDir, "cprest.cgi")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = buildDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the patched plugin here: %v\n%s", err, output)
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := &http.Server{Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("interface saw " + r.URL.Path + " " + r.URL.RawQuery))
		})}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	// cpsrvd cannot route a path after the script name, so the route
	// arrives as a query parameter and is turned back into a path here.
	output := runCGI(t, binary, map[string]string{
		"REMOTE_USER":  "root",
		"QUERY_STRING": "p=destinations&account=customer1",
	})
	if !strings.Contains(output, "interface saw /destinations") {
		t.Errorf("the route did not reach the interface as a path:\n%s", output)
	}
	if !strings.Contains(output, "account=customer1") {
		t.Errorf("the other query parameters were dropped:\n%s", output)
	}

	// No route at all is the dashboard.
	root := runCGI(t, binary, map[string]string{"REMOTE_USER": "root"})
	if !strings.Contains(root, "interface saw /") {
		t.Errorf("the bare entry URL did not reach the dashboard:\n%s", root)
	}
}

func writeModule(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module cprestcgitest\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsARouteThatIsNotOurs(t *testing.T) {
	binary := buildCGI(t)

	// "p" arrives from the browser, and the proxy turns it into a path.
	// A separator Go's query parser discards (a bare ";" or space) never
	// reaches safeRoute at all: the parameter is dropped and the request
	// lands on the dashboard, which is harmless.
	for _, route := range []string{"../../etc/passwd", "a//b", "a%20b", "%2e%2e%2fetc"} {
		output := runCGI(t, binary, map[string]string{
			"REMOTE_USER":  "root",
			"QUERY_STRING": "p=" + route,
		})
		if !strings.Contains(output, "400") {
			t.Errorf("route %q was accepted:\n%s", route, output)
		}
	}
}
