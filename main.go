// llc-companion — local compile-and-run helper for the C Systems Lab course.
//
// Listens on 127.0.0.1:27183.
// GET  /ping  — health check (returns JSON)
// WS   /run   — receive RunRequest, compile, stream output as RunMessages
//
// Build:  go build -o llc-companion .
// Cross:  make all        (mac/linux/windows via Makefile)
// Run:    ./llc-companion

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	listenAddr = "127.0.0.1:27183"
	maxBytes   = 64 * 1024       // 64 KB output cap
	runTimeout = 5 * time.Second // per compile+run
	version    = "1.0.0"
)

// RunRequest is sent by the browser over the WebSocket.
type RunRequest struct {
	Source   string `json:"source"`   // C source text
	Filename string `json:"filename"` // e.g. "hello.c"
	Cmd      string `json:"cmd"`      // e.g. "gcc -std=c11 -Wall hello.c -o hello && ./hello"
}

// RunMessage is streamed back to the browser.
// Types: "out" (stdout), "err" (stderr), "success" (exit 0), "exit" (always last)
type RunMessage struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Code int    `json:"code,omitempty"`
}

var upgrader = websocket.Upgrader{
	// Allow any origin — the companion is localhost-only so origin checks
	// add no security. Browsers permit localhost cross-origin regardless.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// cors wraps a handler with permissive CORS headers so the course app
// (hosted on https://*.pages.dev or a custom domain) can reach /ping.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
		"os":      runtime.GOOS,
	})
}

// send writes a RunMessage to the WebSocket under the connection mutex.
func send(conn *websocket.Conn, mu *sync.Mutex, msg RunMessage) {
	mu.Lock()
	defer mu.Unlock()
	conn.WriteJSON(msg) //nolint:errcheck — connection errors are terminal
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Read the single run request message.
	var req RunRequest
	if err := conn.ReadJSON(&req); err != nil {
		return
	}

	if req.Source == "" {
		conn.WriteJSON(RunMessage{Type: "err", Text: "no source provided"})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}

	// Temp directory — cleaned up when the handler returns.
	tmpDir, err := os.MkdirTemp("", "llc-run-*")
	if err != nil {
		conn.WriteJSON(RunMessage{Type: "err", Text: "cannot create temp dir: " + err.Error()})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}
	defer os.RemoveAll(tmpDir)

	filename := req.Filename
	if filename == "" {
		filename = "main.c"
	}
	srcPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(srcPath, []byte(req.Source), 0600); err != nil {
		conn.WriteJSON(RunMessage{Type: "err", Text: "cannot write source: " + err.Error()})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}

	// Run the command (supports `&&` chains via sh -c).
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)
	cmd.Dir = tmpDir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		conn.WriteJSON(RunMessage{Type: "err", Text: "pipe error: " + err.Error()})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		conn.WriteJSON(RunMessage{Type: "err", Text: "pipe error: " + err.Error()})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}

	if err := cmd.Start(); err != nil {
		conn.WriteJSON(RunMessage{Type: "err", Text: "cannot start: " + err.Error()})
		conn.WriteJSON(RunMessage{Type: "exit", Code: 1})
		return
	}

	var mu sync.Mutex
	var total int64
	var wg sync.WaitGroup
	truncated := false

	// stream reads lines from pipe and sends them as RunMessages of the given type.
	stream := func(pipe interface{ Read([]byte) (int, error) }, msgType string) {
		defer wg.Done()
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			line := scanner.Text()
			n := atomic.AddInt64(&total, int64(len(line)+1))
			if n > maxBytes {
				if !truncated {
					truncated = true
					send(conn, &mu, RunMessage{Type: "err", Text: "[output truncated — 64 KB limit reached]"})
					cmd.Process.Kill() //nolint:errcheck
				}
				return
			}
			send(conn, &mu, RunMessage{Type: msgType, Text: line})
		}
	}

	wg.Add(2)
	go stream(stdoutPipe, "out")
	go stream(stderrPipe, "err")
	wg.Wait()

	err = cmd.Wait()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			send(conn, &mu, RunMessage{Type: "err", Text: "killed: exceeded 5s timeout"})
			code = -1
		} else {
			code = -1
		}
	}

	if code == 0 && !truncated {
		send(conn, &mu, RunMessage{Type: "success", Text: "exit 0"})
	}
	send(conn, &mu, RunMessage{Type: "exit", Code: code})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", cors(handlePing))
	mux.HandleFunc("/run", handleRun)

	log.Printf("llc-companion v%s listening on %s", version, listenAddr)
	log.Printf("Run  — POST JSON to ws://%s/run", listenAddr)
	log.Printf("Ping — GET http://%s/ping", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
