package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// clientBinary builds the CLI once and returns its path.
func clientBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sipclient-bin")
		if err != nil {
			binaryErr = err
			return
		}
		binaryPath = filepath.Join(dir, "sipclient")
		args := []string{"build"}
		// SIPCLIENT_TEST_RACE instruments the client itself, not just the test
		// binary, which is what makes the race check meaningful (NFR-3).
		if os.Getenv("SIPCLIENT_TEST_RACE") != "" {
			args = append(args, "-race")
		}
		args = append(args, "-o", binaryPath, ".")
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatalf("cannot build client: %v", binaryErr)
	}
	return binaryPath
}

// startPBX brings up the fake PBX with user 1001.
func startPBX(t *testing.T, tune func(*testpbx.PBX)) *testpbx.PBX {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	pbx, err := testpbx.New(logger)
	if err != nil {
		t.Fatalf("testpbx.New: %v", err)
	}
	pbx.Users["1001"] = "s3cret"
	if tune != nil {
		tune(pbx)
	}
	if err := pbx.Listen(); err != nil {
		t.Fatalf("testpbx.Listen: %v", err)
	}
	t.Cleanup(pbx.Close)
	return pbx
}

// testDir returns the working directory for a test run. Setting
// SIPCLIENT_TEST_DIR keeps the logs and SIP trace after the test, which is how
// you debug a failing scenario.
func testDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("SIPCLIENT_TEST_DIR"); d != "" {
		dir := filepath.Join(d, t.Name())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		return dir
	}
	return t.TempDir()
}

// writeConfig produces a config.json pointing at the PBX. The SIP domain is
// deliberately not the server host, which proves requests are routed to the
// configured server rather than resolved from the Request-URI.
func writeConfig(t *testing.T, dir string, pbx *testpbx.PBX, mutate func(*config.Config)) string {
	t.Helper()

	host, port := splitAddr(t, pbx.Addr)
	cfg := config.Default()
	cfg.SIP.Username = "1001"
	cfg.SIP.AuthUsername = "1001"
	cfg.SIP.Password = "s3cret"
	cfg.SIP.Domain = "testpbx"
	cfg.SIP.Server.Host = host
	cfg.SIP.Server.Port = port
	if pbx.Network != "" {
		cfg.SIP.Server.Transport = pbx.Network
	}
	cfg.SIP.RegisterExpirySeconds = 60
	cfg.Network.LocalAddress = "127.0.0.1"
	cfg.Network.LocalSIPPort = 0 // ephemeral, so tests can run in parallel
	// RTP ports are left at the default (OS-assigned), so parallel scenarios
	// cannot collide on a range. Tests that need a pinned range set one.
	// Keep the call log with the rest of the run's output; the default is a
	// relative path, which would otherwise litter the repository root.
	cfg.History.File = filepath.Join(dir, "call-history.jsonl")
	cfg.Logging.File = filepath.Join(dir, "app.log")
	cfg.Logging.SIPTraceFile = filepath.Join(dir, "sip.log")
	cfg.Logging.Level = "debug"
	cfg.Logging.ConsoleLevel = "error"
	if mutate != nil {
		mutate(&cfg)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, found := strings.Cut(addr, ":")
	if !found {
		t.Fatalf("bad addr %q", addr)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("bad port in %q", addr)
	}
	return host, port
}

// startClientWithWeb runs the client with the browser UI enabled on a free
// port, holding it open until the returned stop function is called. It returns
// the UI's base URL.
func startClientWithWeb(t *testing.T, configPath string) (string, func()) {
	t.Helper()

	port := freeTCPPort(t)
	cmd := exec.Command(clientBinary(t), "-config", configPath, "-no-audio",
		"-wait-register", "8s", "-web", "-web-port", fmt.Sprint(port))
	// Keep it alive; the test drives it over HTTP, not stdin.
	cmd.Stdin = strings.NewReader("sleep 600000\nquit\n")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}

	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(stop)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return base, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("web UI never came up on %s\noutput:\n%s", base, out.String())
	return "", stop
}

// freeTCPPort reserves a port number by binding and releasing it.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runClient feeds the script to the client on stdin and returns its output and
// exit code. Scripted mode is FR-9.8; the exit codes are FR-9.9.
func runClient(t *testing.T, configPath, script string, timeout time.Duration) (string, int) {
	return runClientWithFlags(t, configPath, script, timeout, "-no-audio")
}

// runClientWithFlags is runClient with control over the command line, so the
// real audio path can be exercised too.
func runClientWithFlags(t *testing.T, configPath, script string,
	timeout time.Duration, extra ...string) (string, int) {
	t.Helper()

	args := append([]string{"-config", configPath, "-wait-register", "8s"}, extra...)
	cmd := exec.Command(clientBinary(t), args...)
	cmd.Stdin = strings.NewReader(script)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("client wait: %v\noutput:\n%s", err, out.String())
		}
		return out.String(), code

	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("client did not exit within %s\noutput:\n%s", timeout, out.String())
		return "", -1
	}
}

// requireContains fails with the full output when a line is missing, which is
// what makes these tests debuggable.
func requireContains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q", w)
		}
	}
	if t.Failed() {
		t.Logf("full client output:\n%s", out)
	}
}

func requireNotContains(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(out, w) {
			t.Errorf("output unexpectedly contains %q", w)
		}
	}
}

// testWriter routes the fake PBX's log into the test output, so a failing
// scenario shows both sides of the exchange.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("pbx: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
