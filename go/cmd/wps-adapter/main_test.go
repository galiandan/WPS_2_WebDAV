package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain builds the real binary once; the lifecycle guarantees below are
// process-level properties and cannot be tested in-process.
var binaryPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wps-adapter-bin")
	if err != nil {
		panic(err)
	}
	binaryPath = dir + "/wps-adapter-test"
	build := exec.Command("go", "build", "-o", binaryPath,
		"github.com/galiandan/WPS_2_WebDAV/go/cmd/wps-adapter")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		panic("go build failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

type serverProcess struct {
	cmd        *exec.Cmd
	stdout     *bufio.Reader
	stderrBuf  *bytes.Buffer // written by the copier goroutine, read after stderrDone
	stderrDone chan struct{}
	port       int
}

func startServer(t *testing.T, env []string, args ...string) *serverProcess {
	t.Helper()
	cmd := exec.Command(binaryPath, args...)
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	stderrBuffer := &bytes.Buffer{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrBuffer, stderrPipe)
	}()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	process := &serverProcess{
		cmd:        cmd,
		stdout:     bufio.NewReader(stdout),
		stderrBuf:  stderrBuffer,
		stderrDone: stderrDone,
		port:       portOf(args),
	}
	t.Cleanup(func() {
		if process.cmd.Process != nil {
			process.cmd.Process.Signal(syscall.SIGKILL)
			process.cmd.Wait()
		}
	})
	return process
}

func portOf(args []string) int {
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port := 0
			for _, char := range args[i+1] {
				port = port*10 + int(char-'0')
			}
			return port
		}
	}
	return 0
}

// waitListening reads stdout until the listening line or the deadline.
func (p *serverProcess) waitListening(t *testing.T) string {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		text, err := p.stdout.ReadString('\n')
		if err != nil {
			line <- ""
			return
		}
		line <- text
	}()
	select {
	case text := <-line:
		if !strings.HasPrefix(text, "listening=") {
			t.Fatalf("expected listening line, got %q", text)
		}
		return text
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report listening within 5s")
	}
	return ""
}

func (p *serverProcess) waitExit(t *testing.T, wantCode int) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit within 15s")
	}
	if p.cmd.ProcessState.ExitCode() != wantCode {
		t.Fatalf("exit code = %d, want %d; stderr: %s",
			p.cmd.ProcessState.ExitCode(), wantCode, p.stderrSoFar())
	}
}

// stderrSoFar drains the copier goroutine before reading, so the buffer is
// never read while the pipe copy is still writing.
func (p *serverProcess) stderrSoFar() string {
	<-p.stderrDone
	return p.stderrBuf.String()
}

func (p *serverProcess) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v: %v", sig, err)
	}
}

// TestVersionReportsTheBuildSummary pins the 07 §8.2 build summary: the
// bare version leads the line so release checks can compare it directly,
// and the injected commit and build time ride along without secrets.
func TestVersionReportsTheBuildSummary(t *testing.T) {
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("--version failed: %v", err)
	}
	line := strings.TrimSpace(string(output))
	if !strings.HasPrefix(line, "1.0.9 commit=") || !strings.Contains(line, " build_time=") {
		t.Fatalf("--version = %q, want a version/commit/build-time summary", line)
	}
}

func TestServePrintsListeningLinesAndStopsOnSIGTERM(t *testing.T) {
	port := freePort(t)
	process := startServer(t, nil, "serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	listening := process.waitListening(t)
	want := strconv.Itoa(port)
	if !strings.HasSuffix(strings.TrimSpace(listening), ":"+want) {
		t.Errorf("listening line = %q", listening)
	}
	second, err := process.stdout.ReadString('\n')
	if err != nil || !strings.HasPrefix(second, "webdav=http://") || !strings.Contains(second, " rest=http://") {
		t.Errorf("second line = %q (err %v)", second, err)
	}
	process.signal(t, syscall.SIGTERM)
	process.waitExit(t, 0)
}

func TestServeStopsOnSIGINT(t *testing.T) {
	port := freePort(t)
	process := startServer(t, nil, "serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	process.waitListening(t)
	process.signal(t, syscall.SIGINT)
	process.waitExit(t, 0)
}

func TestServeAnswersHealthzBeforeStop(t *testing.T) {
	port := freePort(t)
	process := startServer(t, nil, "serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	process.waitListening(t)
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	body := make([]byte, 256)
	n, _ := response.Body.Read(body)
	response.Body.Close()
	want := `{"status":"ok","service":"wps-enterprise-adapter","version":"1.0.9","network_calls":"on-demand"}`
	if response.StatusCode != http.StatusOK || string(body[:n]) != want {
		t.Errorf("healthz = %d %q", response.StatusCode, body[:n])
	}
	process.signal(t, syscall.SIGTERM)
	process.waitExit(t, 0)
}

func TestServePortConflictExitsOne(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	process := startServer(t, nil, "serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	process.waitExit(t, 1)
	if !strings.Contains(process.stderrSoFar(), "adapter failed") {
		t.Error("expected adapter failed message on stderr")
	}
}

func TestServeRefusesPublicBindWithoutAuth(t *testing.T) {
	port := freePort(t)
	process := startServer(t, []string{"ADAPTER_USERNAME=", "ADAPTER_PASSWORD="},
		"serve", "--bind", "0.0.0.0", "--port", strconv.Itoa(port))
	process.waitExit(t, 1)
	if !strings.Contains(process.stderrSoFar(), "refusing a non-local bind") {
		t.Error("expected the public bind refusal")
	}
}

func TestServeAcceptsPublicBindWithAuth(t *testing.T) {
	port := freePort(t)
	process := startServer(t, []string{"ADAPTER_USERNAME=u", "ADAPTER_PASSWORD=p"},
		"serve", "--bind", "0.0.0.0", "--port", strconv.Itoa(port))
	process.waitListening(t)
	process.signal(t, syscall.SIGTERM)
	process.waitExit(t, 0)
}

func TestShutdownTimeoutForceCloses(t *testing.T) {
	// A client stuck mid-request must not outlive the deadline.
	port := freePort(t)
	process := startServer(t, nil, "serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	process.waitListening(t)

	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	// Half a request: the server blocks reading the rest.
	if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\n")); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	process.signal(t, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		process.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		if process.cmd.ProcessState.ExitCode() != 0 {
			t.Fatalf("exit code = %d, want 0", process.cmd.ProcessState.ExitCode())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("forced shutdown did not complete within 15s")
	}
}

func TestCheckConfigAssemblesOffline(t *testing.T) {
	dir := t.TempDir()
	private := dir + "/secrets"
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceFile := private + "/workspace.json"
	if err := os.WriteFile(workspaceFile, []byte(`{"group_id": "group-1", "root_id": "root-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binaryPath, "check-config")
	cmd.Env = append(os.Environ(), "WPS_WORKSPACE_FILE="+workspaceFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-config failed: %v (%s)", err, output)
	}
	line := strings.TrimSpace(string(output))
	want := "config=ok group_id=ready auth=disabled dav=/dav rest=/api/v1"
	if line != want {
		t.Errorf("check-config output = %q, want %q", line, want)
	}
}

func TestCheckConfigFailsOnBrokenWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	private := dir + "/secrets"
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceFile := private + "/workspace.json"
	if err := os.WriteFile(workspaceFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binaryPath, "check-config")
	cmd.Env = append(os.Environ(), "WPS_WORKSPACE_FILE="+workspaceFile)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-config unexpectedly succeeded: %s", output)
	}
	if !strings.Contains(string(output), "adapter failed") {
		t.Errorf("stderr = %q", output)
	}
}

func TestServeServesWebPageAndAssets(t *testing.T) {
	port := freePort(t)
	process := startServer(t, []string{"ADAPTER_USERNAME=u", "ADAPTER_PASSWORD=p"},
		"serve", "--bind", "127.0.0.1", "--port", strconv.Itoa(port))
	process.waitListening(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	// The browser shell is public so it can render the in-page login form.
	response, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("public / = %d", response.StatusCode)
	}
	response, err = http.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("anonymous auth/me = %d", response.StatusCode)
	}
	response.Body.Close()

	request, err := http.NewRequest(http.MethodGet, base+"/assets/app.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("u", "p")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("authenticated asset = %d", response.StatusCode)
	}
	if ct := response.Header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("asset Content-Type = %q", ct)
	}
	if len(body) == 0 {
		t.Error("asset body is empty")
	}

	process.signal(t, syscall.SIGTERM)
	process.waitExit(t, 0)
}
