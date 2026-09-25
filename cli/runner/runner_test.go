package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const timeSecond = time.Second

func TestWaitPortAndSpawn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := portOf(t, srv)

	if err := WaitPort(1, 300*time.Millisecond); err == nil {
		t.Fatal("expected timeout against port 1")
	}
	if err := WaitPort(port, 2*timeSecond); err != nil {
		t.Fatalf("WaitPort against live server: %v", err)
	}
}

func TestSpawnDetached(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "svc.log")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	free := portOf(t, srv) + 1

	cmd := "python3 -m http.server " + itoa(free) + " --bind 127.0.0.1"
	pid, err := Spawn(SpawnOptions{Name: "t", Cwd: dir, Command: cmd, Port: free, LogFile: logFile})
	if err != nil {
		t.Skipf("python3 unavailable or spawn failed: %v", err)
	}
	KillGroup(pid)
	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`bash -c 'set -a && source .env && exec air'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-c", "set -a && source .env && exec air"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := splitCommand(`echo 'unterminated`); err == nil {
		t.Fatal("expected unterminated quote error")
	}
	got, err = splitCommand(`npm run dev -- --port 3000`)
	if err != nil || len(got) != 6 {
		t.Fatalf("plain split: %q %v", got, err)
	}
	// adjacent quoted and bare segments concatenate into one word
	got, err = splitCommand(`echo "a b"c`)
	if err != nil || fmt.Sprint(got) != `[echo a bc]` {
		t.Fatalf("concatenation: %q %v", got, err)
	}
}

func TestReclaimPort(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	port := 59876
	cmd := exec.Command(python, "-m", "http.server", fmt.Sprint(port), "--bind", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer cmd.Process.Kill()

	if err := WaitPort(port, 5*timeSecond); err != nil {
		t.Fatalf("listener did not come up: %v", err)
	}
	if listeners := PortListeners(port); len(listeners) == 0 {
		t.Fatal("expected PortListeners to find the listener")
	}

	reclaimed := ReclaimPort(port)
	if len(reclaimed) == 0 {
		t.Fatal("expected ReclaimPort to report reclaimed PIDs")
	}

	deadline := time.Now().Add(3 * timeSecond)
	for time.Now().Before(deadline) {
		if len(PortListeners(port)) == 0 {
			return // freed
		}
		time.Sleep(100 * time.Millisecond)
	}
	if listeners := PortListeners(port); len(listeners) > 0 {
		t.Fatalf("port %d still held by %v after reclaim", port, listeners)
	}
}
