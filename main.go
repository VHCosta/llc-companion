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
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	listenAddr = "127.0.0.1:27183"
	maxBytes   = 64 * 1024       // 64 KB output cap
	runTimeout = 5 * time.Second // per compile+run
)

var version = "1.0.0"

// RunRequest is sent by the browser over the WebSocket.
type RunRequest struct {
	Source   string    `json:"source"`   // Legacy single-file source text
	Filename string    `json:"filename"` // Legacy single-file name, e.g. "hello.c"
	Files    []RunFile `json:"files"`    // Structured lesson files for multi-file runs
	Cmd      string    `json:"cmd"`      // e.g. "gcc -std=c11 -Wall hello.c -o hello && ./hello"
}

type RunFile struct {
	Source   string `json:"source"`
	Filename string `json:"filename"`
}

// RunMessage is streamed back to the browser.
// Types: "out" (stdout), "err" (stderr), "success" (exit 0), "exit" (always last)
type RunMessage struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Code int    `json:"code,omitempty"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return originAllowed(r.Header.Get("Origin"))
	},
}

var staticAllowedOrigins = map[string]struct{}{
	"https://c-systems-lab.pages.dev": {},
}

type runState struct {
	total     atomic.Int64
	truncated atomic.Bool
	truncOnce sync.Once
}

func originAllowed(origin string) bool {
	if origin == "" {
		return false
	}

	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}

	if _, ok := staticAllowedOrigins[origin]; ok {
		return true
	}

	for _, extra := range strings.Split(os.Getenv("LLC_ALLOWED_ORIGINS"), ",") {
		if strings.TrimSpace(extra) == origin {
			return true
		}
	}

	return false
}

func setCORSHeaders(w http.ResponseWriter, origin string) {
	w.Header().Set("Vary", "Origin")
	if originAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	}
}

// cors wraps a handler with permissive CORS headers so the course app
// (hosted on an approved origin) can reach /ping.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		setCORSHeaders(w, origin)
		if r.Method == http.MethodOptions {
			if originAllowed(origin) {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
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

func fail(conn *websocket.Conn, mu *sync.Mutex, text string, code int) {
	send(conn, mu, RunMessage{Type: "err", Text: text})
	send(conn, mu, RunMessage{Type: "exit", Code: code})
}

func sanitizeFilename(name string) (string, error) {
	if name == "" {
		return "main.c", nil
	}

	if strings.ContainsAny(name, `/\`) {
		return "", errInvalid("filename must not contain path separators")
	}

	clean := filepath.Base(name)
	if clean == "." || clean == ".." || clean != name {
		return "", errInvalid("invalid filename")
	}

	switch filepath.Ext(clean) {
	case ".c", ".h", ".s":
	default:
		return "", errInvalid("filename must end with .c, .h, or .s")
	}

	return clean, nil
}

type invalidInput string

func (e invalidInput) Error() string { return string(e) }

func errInvalid(msg string) error { return invalidInput(msg) }

func splitSegments(raw string) ([]string, error) {
	parts := strings.Split(raw, "&&")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errInvalid("empty command segment")
		}
		segments = append(segments, part)
	}
	return segments, nil
}

func parseArgs(segment string) ([]string, error) {
	if strings.ContainsAny(segment, `|;<>(){}[]$"'`) {
		return nil, errInvalid("unsupported shell syntax")
	}

	args := strings.Fields(segment)
	if len(args) == 0 {
		return nil, errInvalid("empty command")
	}

	return args, nil
}

func validateCompileArgs(args []string) error {
	if len(args) == 0 {
		return errInvalid("missing compiler command")
	}

	switch args[0] {
	case "gcc", "clang", "cc":
	default:
		return errInvalid("only gcc/clang/cc compile commands are allowed")
	}

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if strings.ContainsAny(arg, `/\`) && !strings.HasPrefix(arg, "-I") {
			return errInvalid("paths are not allowed in compile commands")
		}

		if arg == "-o" {
			if i+1 >= len(args) {
				return errInvalid("missing output file after -o")
			}
			out := args[i+1]
			if out == "" || strings.HasPrefix(out, "-") || strings.ContainsAny(out, `/\`) {
				return errInvalid("invalid output filename")
			}
			i++
		}
	}

	return nil
}

func validateRunArgs(args []string) error {
	if len(args) == 0 {
		return errInvalid("missing run command")
	}

	if !strings.HasPrefix(args[0], "./") {
		return errInvalid("run command must execute a local binary")
	}

	target := strings.TrimPrefix(args[0], "./")
	if target == "" || strings.ContainsAny(target, `/\`) {
		return errInvalid("invalid run target")
	}

	for _, arg := range args[1:] {
		if strings.ContainsAny(arg, `/\`) {
			return errInvalid("run arguments must not contain paths")
		}
	}

	return nil
}

func materializeFiles(req RunRequest) ([]RunFile, error) {
	if len(req.Files) > 0 {
		files := make([]RunFile, 0, len(req.Files))
		seen := make(map[string]struct{}, len(req.Files))
		for _, file := range req.Files {
			filename, err := sanitizeFilename(file.Filename)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[filename]; ok {
				return nil, errInvalid("duplicate filename: " + filename)
			}
			if file.Source == "" {
				return nil, errInvalid("empty source for " + filename)
			}
			seen[filename] = struct{}{}
			files = append(files, RunFile{
				Filename: filename,
				Source:   file.Source,
			})
		}
		return files, nil
	}

	if req.Source == "" {
		return nil, errInvalid("no source provided")
	}

	filename, err := sanitizeFilename(req.Filename)
	if err != nil {
		return nil, err
	}

	return []RunFile{{
		Filename: filename,
		Source:   req.Source,
	}}, nil
}

func validateCommandChain(raw string) ([][]string, error) {
	segments, err := splitSegments(raw)
	if err != nil {
		return nil, err
	}

	argvs := make([][]string, 0, len(segments))
	for _, segment := range segments {
		args, err := parseArgs(segment)
		if err != nil {
			return nil, err
		}

		switch {
		case args[0] == "gcc" || args[0] == "clang" || args[0] == "cc":
			if err := validateCompileArgs(args); err != nil {
				return nil, err
			}
		case strings.HasPrefix(args[0], "./"):
			if err := validateRunArgs(args); err != nil {
				return nil, err
			}
		default:
			return nil, errInvalid("unsupported command")
		}

		argvs = append(argvs, args)
	}

	return argvs, nil
}

func runExec(
	ctx context.Context,
	tmpDir string,
	args []string,
	conn *websocket.Conn,
	mu *sync.Mutex,
	state *runState,
) int {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = tmpDir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		send(conn, mu, RunMessage{Type: "err", Text: "pipe error: " + err.Error()})
		return 1
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		send(conn, mu, RunMessage{Type: "err", Text: "pipe error: " + err.Error()})
		return 1
	}

	if err := cmd.Start(); err != nil {
		send(conn, mu, RunMessage{Type: "err", Text: "cannot start: " + err.Error()})
		return 1
	}

	var wg sync.WaitGroup
	stream := func(pipe interface{ Read([]byte) (int, error) }, msgType string) {
		defer wg.Done()
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			line := scanner.Text()
			n := state.total.Add(int64(len(line) + 1))
			if n > maxBytes {
				state.truncOnce.Do(func() {
					state.truncated.Store(true)
					send(conn, mu, RunMessage{Type: "err", Text: "[output truncated — 64 KB limit reached]"})
					cmd.Process.Kill() //nolint:errcheck
				})
				return
			}
			send(conn, mu, RunMessage{Type: msgType, Text: line})
		}
	}

	wg.Add(2)
	go stream(stdoutPipe, "out")
	go stream(stderrPipe, "err")
	wg.Wait()

	err = cmd.Wait()
	if err == nil {
		return 0
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}

	if ctx.Err() == context.DeadlineExceeded {
		send(conn, mu, RunMessage{Type: "err", Text: "killed: exceeded 5s timeout"})
	}

	return -1
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

	var mu sync.Mutex

	// Temp directory — cleaned up when the handler returns.
	tmpDir, err := os.MkdirTemp("", "llc-run-*")
	if err != nil {
		fail(conn, &mu, "cannot create temp dir: "+err.Error(), 1)
		return
	}
	defer os.RemoveAll(tmpDir)

	argvs, err := validateCommandChain(req.Cmd)
	if err != nil {
		fail(conn, &mu, err.Error(), 1)
		return
	}

	files, err := materializeFiles(req)
	if err != nil {
		fail(conn, &mu, err.Error(), 1)
		return
	}

	for _, file := range files {
		srcPath := filepath.Join(tmpDir, file.Filename)
		if err := os.WriteFile(srcPath, []byte(file.Source), 0600); err != nil {
			fail(conn, &mu, "cannot write source: "+err.Error(), 1)
			return
		}
	}

	if len(files) == 0 {
		fail(conn, &mu, "no source provided", 1)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	state := &runState{}
	code := 0
	for _, args := range argvs {
		code = runExec(ctx, tmpDir, args, conn, &mu, state)
		if code != 0 {
			break
		}
	}

	if code == 0 && !state.truncated.Load() {
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
