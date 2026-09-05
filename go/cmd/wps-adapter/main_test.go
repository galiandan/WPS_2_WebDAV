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
	want := `{"status":"ok","service":"wps-enterprise-adapter","version":"0.9.8","network_calls":"on-demand"}`
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
