package main

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	encCtx    *cryptashCtx
	encPhrase string
)

type Record struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Message   *MessagePayload `json:"message,omitempty"`
}

type MessagePayload struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolName   string          `json:"toolName,omitempty"`
	ToolCallId string          `json:"toolCallId,omitempty"`
	Details    *ToolDetails    `json:"details,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

type ToolDetails struct {
	ExitCode *int `json:"exitCode,omitempty"`
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseId string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type ToolInput struct {
	Command  string `json:"command,omitempty"`
	Path     string `json:"path,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Action   string `json:"action,omitempty"`
	Query    string `json:"query,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Entry struct {
	Time     time.Time `json:"-"`
	Ts       string    `json:"ts"`
	Unix     int64     `json:"unix"`
	SID      string    `json:"sid"`
	Role     string    `json:"role"`
	Text     string    `json:"text"`
	ToolName string    `json:"toolName,omitempty"`
	ToolMeta string    `json:"toolMeta,omitempty"`
	ToolDesc string    `json:"toolDesc,omitempty"`
}

//go:embed index.html
var staticFS embed.FS

var (
	sessionsDir string
	noTools     bool
	noColor     bool
	noWatch     bool
	port        int
	timezone    *time.Location

	mu        sync.Mutex
	offsets   = make(map[string]int64)
	toolCalls = make(map[string]string)

	histMu     sync.RWMutex
	history    = make([]*Entry, 0)
	generation int
	notifyMu   sync.Mutex
	notifyCh   = make(chan struct{})

	rescanMu    sync.Mutex
	rescanTimer *time.Timer
)

const (
	cReset = "\033[0m"
	cBlue  = "\033[38;5;75m"  // bright blue (User)
	cGreen = "\033[1;32m"     // bold green (Agent)
	cGray  = "\033[38;5;245m" // medium gray (Tool)
	cDim   = "\033[0;90m"     // dim gray (time, sid)
)

const (
	serviceName = "cmon"
	serveEnv    = "CMON_SERVE"
)

type config struct {
	Dir   string `json:"dir,omitempty"`
	Token string `json:"token"`
	Port  int    `json:"port,omitempty"`
}

func cwdToProjectDirName(cwd string) string {
	var b strings.Builder
	for _, c := range cwd {
		if c == '/' || c == '.' {
			b.WriteRune('-')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func defaultDir() string {
	home, _ := os.UserHomeDir()
	// Claude Code stores sessions under ~/.claude/projects/<cwd-based-name>/
	workspace := filepath.Join(home, ".openclaw", "workspace")
	claudeDir := filepath.Join(home, ".claude", "projects", cwdToProjectDirName(workspace))
	if _, err := os.Stat(claudeDir); err == nil {
		return claudeDir
	}
	return filepath.Join(home, ".openclaw", "agents", "main", "sessions")
}

func configDir() string {
	d := os.Getenv("XDG_CONFIG_HOME")
	if d == "" {
		if home, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(home, ".config")
		}
	}
	return d
}

func cmonConfigDir() string {
	d := configDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "cmon")
}

func configPath() string {
	d := cmonConfigDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "config.json")
}

func loadConfig() *config {
	p := configPath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var cfg config
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	return &cfg
}

func usage() {
	fmt.Fprintf(os.Stderr, `cmon -- OpenClaw session monitor

Usage:
  cmon           Start web server (foreground, Ctrl+C to stop)
  cmon run       Install and start as user service
  cmon stop      Stop and remove user service
  cmon cli       CLI mode (foreground, stdout)

Config: %s

  {
    "token": "your-secret-token",
    "port": 18787,
    "dir": "/path/to/sessions"
  }

  token is required. port defaults to 18787.
  dir defaults to ~/.openclaw/agents/main/sessions

Options (cli):
  -notools       Hide tool calls/results
  -nowatch       Dump history and exit
  -nocolor       Disable ANSI colors
`, configPath())
}

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "":
		if os.Getenv(serveEnv) == "1" {
			doServe()
		} else {
			doForeground(args)
		}
	case "run":
		if os.Getenv(serveEnv) == "1" {
			doServe()
		} else {
			doRun(args)
		}
	case "stop":
		doStop()
	case "cli":
		doCLI(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "cmon: unknown command %q\n\n", cmd)
		usage()
	}
}

func parseCLIFlags(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-dir" && i+1 < len(args):
			i++
			sessionsDir = args[i]
		case a == "-notools":
			noTools = true
		case a == "-nowatch":
			noWatch = true
		case a == "-nocolor":
			noColor = true
		default:
			fmt.Fprintf(os.Stderr, "cmon: unknown flag %q\n", a)
			os.Exit(1)
		}
	}
}

func parseRunFlags(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-port" && i+1 < len(args):
			i++
			port, _ = strconv.Atoi(args[i])
		case a == "-dir" && i+1 < len(args):
			i++
			sessionsDir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "cmon: unknown flag %q\n", a)
			os.Exit(1)
		}
	}
}

// Web init (shared by foreground + service)

func initWeb(args []string) {
	parseRunFlags(args)
	cfg := requireConfig()

	if cfg.Dir != "" && sessionsDir == "" {
		sessionsDir = cfg.Dir
	}
	if cfg.Port != 0 && port == 0 {
		port = cfg.Port
	}
	if sessionsDir == "" {
		sessionsDir = defaultDir()
	}
	if port == 0 {
		port = 18787
	}

	noTools = false
	timezone = time.Now().Location()

	if _, err := os.Stat(sessionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "cmon: sessions dir: %v\n", err)
		os.Exit(1)
	}

	initEncryption(cfg.Token)
}

func startWebServer(ln net.Listener) {
	entries := collectAll()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Time.Before(entries[j].Time)
	})
	histMu.Lock()
	for i := range entries {
		history = append(history, &entries[i])
	}
	histMu.Unlock()

	go watchLoop()

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/api", serveAPI)
	if err := http.Serve(ln, nil); err != nil {
		fmt.Fprintf(os.Stderr, "cmon: http: %v\n", err)
		os.Exit(1)
	}
}

// Foreground

func doForeground(args []string) {
	initWeb(args)

	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmon: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("http://0.0.0.0%s\n", addr)
	fmt.Printf("%s\n", encPhrase)

	startWebServer(ln)
}

// Run (service)

func requireConfig() *config {
	cfg := loadConfig()
	if cfg == nil {
		p := configPath()
		fmt.Fprintf(os.Stderr, "cmon: config not found: %s\n\n", p)
		fmt.Fprintf(os.Stderr, "Create it with at least a token:\n\n")
		fmt.Fprintf(os.Stderr, "  mkdir -p %s\n", cmonConfigDir())
		fmt.Fprintf(os.Stderr, "  cat > %s << 'EOF'\n", p)
		fmt.Fprintf(os.Stderr, "  {\n    \"token\": \"your-secret-token\"\n  }\n  EOF\n\n")
		os.Exit(1)
	}
	if cfg.Token == "" {
		fmt.Fprintf(os.Stderr, "cmon: token is required in %s\n", configPath())
		os.Exit(1)
	}
	return cfg
}

func doRun(args []string) {
	initWeb(args)

	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmon: %v\n", err)
		os.Exit(1)
	}
	ln.Close()

	fmt.Printf("http://0.0.0.0%s\n", addr)

	if hasSystemd() {
		if err := installService(); err != nil {
			fmt.Fprintf(os.Stderr, "cmon: service install failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "cmon: falling back to daemon fork\n")
			daemonFork()
			return
		}
		exec.Command("loginctl", "enable-linger").Run()
	} else {
		daemonFork()
	}
}

func doServe() {
	initWeb(nil)

	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmon: %v\n", err)
		os.Exit(1)
	}

	startWebServer(ln)
}

// Service management

func serviceDir() string {
	d := configDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "systemd", "user")
}

func servicePath() string {
	d := serviceDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, serviceName+".service")
}

func hasSystemd() bool {
	_, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	out, err := exec.Command("systemctl", "--user", "is-system-running").CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		return s == "degraded" || s == "running"
	}
	return true
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	// ExecStart: just "cmon run" -- all config from file, no secrets in cmdline
	var parts []string
	parts = append(parts, exe, "run")
	if port != 18787 {
		parts = append(parts, "-port", strconv.Itoa(port))
	}
	if sessionsDir != defaultDir() {
		parts = append(parts, "-dir", sessionsDir)
	}
	execStart := strings.Join(parts, " ")

	unit := fmt.Sprintf(`[Unit]
Description=cmon -- OpenClaw session monitor

[Service]
Type=simple
Environment=%s=1
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, serveEnv, execStart)

	dir := serviceDir()
	if dir == "" {
		return fmt.Errorf("cannot determine systemd user dir")
	}
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(servicePath(), []byte(unit), 0644); err != nil {
		return err
	}

	cmds := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", serviceName},
		{"systemctl", "--user", "start", serviceName},
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s", strings.Join(c, " "), strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func daemonFork() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmon: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), serveEnv+"=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "cmon: daemon: %v\n", err)
		os.Exit(1)
	}
	cmd.Process.Release()
}

// Stop

func doStop() {
	stopped := false
	if hasSystemd() {
		cmds := [][]string{
			{"systemctl", "--user", "stop", serviceName},
			{"systemctl", "--user", "disable", serviceName},
		}
		for _, c := range cmds {
			if err := exec.Command(c[0], c[1:]...).Run(); err == nil {
				stopped = true
			}
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		if p := servicePath(); p != "" {
			os.Remove(p)
		}
	}

	// Also kill any stray processes
	out, _ := exec.Command("pgrep", "-x", "cmon").Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid == os.Getpid() {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(os.Interrupt)
			stopped = true
		}
	}

	if stopped {
		fmt.Println("cmon: stopped")
	} else {
		fmt.Fprintf(os.Stderr, "cmon: not running\n")
		os.Exit(1)
	}
}

// CLI

func doCLI(args []string) {
	parseCLIFlags(args)

	cfg := loadConfig()
	if cfg != nil {
		if cfg.Dir != "" && sessionsDir == "" {
			sessionsDir = cfg.Dir
		}
	}
	if sessionsDir == "" {
		sessionsDir = defaultDir()
	}

	timezone = time.Now().Location()

	if _, err := os.Stat(sessionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "cmon: sessions dir: %v\n", err)
		os.Exit(1)
	}

	entries := collectAll()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Time.Before(entries[j].Time)
	})
	for _, e := range entries {
		printEntryCLI(&e)
	}
	if noWatch {
		return
	}
	cliWatchLoop()
}

// Cryptash init

func initEncryption(token string) {
	encPhrase = token
	key := sha256.Sum256([]byte(token))
	encCtx = cryptashNew(key[:], 16, 16)
}

// Notify helpers

func notifyClients() {
	notifyMu.Lock()
	close(notifyCh)
	notifyCh = make(chan struct{})
	notifyMu.Unlock()
}

func addEntry(e *Entry) {
	histMu.Lock()
	history = append(history, e)
	histMu.Unlock()
	notifyClients()
}

func fullRescan() {
	mu.Lock()
	for k := range offsets {
		delete(offsets, k)
	}
	for k := range toolCalls {
		delete(toolCalls, k)
	}
	mu.Unlock()

	entries := collectAll()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Time.Before(entries[j].Time)
	})

	histMu.Lock()
	history = make([]*Entry, len(entries))
	for i := range entries {
		history[i] = &entries[i]
	}
	generation++
	histMu.Unlock()

	notifyClients()
}

func scheduleRescan() {
	rescanMu.Lock()
	defer rescanMu.Unlock()
	if rescanTimer != nil {
		rescanTimer.Stop()
	}
	rescanTimer = time.AfterFunc(500*time.Millisecond, fullRescan)
}

// Web handlers

func serveIndex(w http.ResponseWriter, r *http.Request) {
	data, _ := staticFS.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

type apiRequest struct {
	Cmd   string `json:"cmd"`
	Nonce string `json:"nonce,omitempty"`
	After int    `json:"after,omitempty"`
	Gen   int    `json:"gen,omitempty"`
}

func serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	plain := encCtx.decrypt(body)
	if plain == nil {
		apiError(w, "decrypt failed")
		return
	}

	var req apiRequest
	if json.Unmarshal(plain, &req) != nil {
		apiError(w, "bad json")
		return
	}

	switch req.Cmd {
	case "auth":
		apiRespond(w, map[string]interface{}{"ok": true, "nonce": req.Nonce})

	case "history":
		histMu.RLock()
		entries := history
		gen := generation
		histMu.RUnlock()
		data, _ := json.Marshal(map[string]interface{}{
			"gen": gen, "total": len(entries), "entries": entries,
		})
		apiRespondRaw(w, data)

	case "poll":
		histMu.RLock()
		count := len(history)
		gen := generation
		histMu.RUnlock()

		if count <= req.After && gen == req.Gen {
			notifyMu.Lock()
			ch := notifyCh
			notifyMu.Unlock()
			select {
			case <-ch:
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Second):
			}
		}

		histMu.RLock()
		gen = generation
		if gen != req.Gen {
			data, _ := json.Marshal(map[string]interface{}{
				"gen": gen, "total": len(history), "entries": history, "reset": true,
			})
			histMu.RUnlock()
			apiRespondRaw(w, data)
		} else {
			var entries []*Entry
			if req.After < len(history) {
				entries = history[req.After:]
			}
			if entries == nil {
				entries = make([]*Entry, 0)
			}
			data, _ := json.Marshal(map[string]interface{}{
				"gen": gen, "total": len(history), "entries": entries,
			})
			histMu.RUnlock()
			apiRespondRaw(w, data)
		}

	default:
		apiError(w, "unknown cmd")
	}
}

func apiRespond(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	apiRespondRaw(w, data)
}

func apiRespondRaw(w http.ResponseWriter, plaintext []byte) {
	ct := encCtx.encrypt(plaintext)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(ct)
}

func apiError(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

// Watch loops

func watchLoop() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsnotify: %v\n", err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(sessionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "watch %s: %v\n", sessionsDir, err)
		return
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !isSessionFile(event.Name) {
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				mu.Lock()
				delete(offsets, event.Name)
				mu.Unlock()
				scheduleRescan()
			} else if event.Has(fsnotify.Write) {
				for _, e := range readNewEntries(event.Name) {
					addEntry(&e)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
		}
	}
}

func cliWatchLoop() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsnotify: %v\n", err)
		os.Exit(1)
	}
	defer watcher.Close()
	if err := watcher.Add(sessionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "watch %s: %v\n", sessionsDir, err)
		os.Exit(1)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !isSessionFile(event.Name) {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if event.Has(fsnotify.Create) && sessionStatus(event.Name) != "" {
					readNewEntries(event.Name)
					continue
				}
				for _, e := range readNewEntries(event.Name) {
					printEntryCLI(&e)
				}
			}
			if event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				mu.Lock()
				delete(offsets, event.Name)
				mu.Unlock()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
		}
	}
}

// File parsing

func isSessionFile(path string) bool {
	base := filepath.Base(path)
	if !strings.Contains(base, ".jsonl") {
		return false
	}
	if strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".tmp") {
		return false
	}
	if strings.HasPrefix(base, "sessions.json") {
		return false
	}
	return true
}

func sessionID(path string) string {
	base := filepath.Base(path)
	if idx := strings.Index(base, ".jsonl"); idx > 0 {
		id := base[:idx]
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	return base
}

func sessionStatus(path string) string {
	base := filepath.Base(path)
	if strings.Contains(base, ".deleted.") {
		return "deleted"
	}
	if strings.Contains(base, ".reset.") {
		return "reset"
	}
	return ""
}

func collectAll() []Entry {
	pattern := filepath.Join(sessionsDir, "*.jsonl*")
	files, _ := filepath.Glob(pattern)
	var all []Entry
	for _, f := range files {
		if !isSessionFile(f) {
			continue
		}
		all = append(all, readAllEntries(f)...)
	}
	return all
}

func readAllEntries(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sid := sessionID(path)
	status := sessionStatus(path)
	entries := parseEntries(f, sid, status)

	offset, _ := f.Seek(0, io.SeekCurrent)
	mu.Lock()
	offsets[path] = offset
	mu.Unlock()
	return entries
}

func readNewEntries(path string) []Entry {
	mu.Lock()
	offset := offsets[path]
	mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	if offset > 0 {
		f.Seek(offset, io.SeekStart)
	}

	sid := sessionID(path)
	status := sessionStatus(path)
	entries := parseEntries(f, sid, status)

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	mu.Lock()
	offsets[path] = newOffset
	mu.Unlock()
	return entries
}

func parseEntries(r io.ReadSeeker, sid, status string) []Entry {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	var entries []Entry
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if e, ok := recordToEntry(sid, status, &rec); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

func recordToEntry(sid, status string, rec *Record) (Entry, bool) {
	// New Claude Code format: type = "user" or "assistant"
	if rec.Type == "user" || rec.Type == "assistant" {
		return recordToEntryCC(sid, status, rec)
	}

	// Old openclaw format: type = "message"
	if rec.Type != "message" || rec.Message == nil {
		return Entry{}, false
	}

	role := rec.Message.Role
	t := parseTime(rec.Timestamp)
	ts := formatTime(t)

	displaySID := sid
	if status != "" {
		displaySID = sid + "/" + status
	}

	switch role {
	case "user":
		text := extractText(rec.Message.Content)
		if text == "" {
			return Entry{}, false
		}
		text = cleanUserText(text)
		if text == "" {
			return Entry{}, false
		}
		return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "User", Text: text}, true

	case "assistant":
		registerToolCalls(rec.Message.Content)
		text := extractText(rec.Message.Content)
		if text == "" {
			return Entry{}, false
		}
		return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "Agent", Text: text}, true

	case "toolResult":
		if noTools {
			return Entry{}, false
		}
		text := extractText(rec.Message.Content)
		toolName := rec.Message.ToolName
		toolMeta := ""
		if d := rec.Message.Details; d != nil {
			var parts []string
			if d.ExitCode != nil && *d.ExitCode != 0 {
				parts = append(parts, fmt.Sprintf("exit:%d", *d.ExitCode))
			}
			if rec.Message.IsError {
				parts = append(parts, "ERROR")
			}
			toolMeta = strings.Join(parts, " ")
		}
		toolDesc := ""
		if rec.Message.ToolCallId != "" {
			mu.Lock()
			toolDesc = toolCalls[rec.Message.ToolCallId]
			delete(toolCalls, rec.Message.ToolCallId)
			mu.Unlock()
		}
		if text != "" || toolName != "" {
			return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "Tool", Text: text, ToolName: toolName, ToolMeta: toolMeta, ToolDesc: toolDesc}, true
		}
	}
	return Entry{}, false
}

// recordToEntryCC handles the Claude Code JSONL format where top-level type is "user" or "assistant".
func recordToEntryCC(sid, status string, rec *Record) (Entry, bool) {
	if rec.Message == nil {
		return Entry{}, false
	}
	t := parseTime(rec.Timestamp)
	ts := formatTime(t)
	displaySID := sid
	if status != "" {
		displaySID = sid + "/" + status
	}

	switch rec.Type {
	case "assistant":
		registerToolCalls(rec.Message.Content)
		text := extractText(rec.Message.Content)
		if text == "" {
			return Entry{}, false
		}
		return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "Agent", Text: text}, true

	case "user":
		var blocks []ContentBlock
		if json.Unmarshal(rec.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type != "tool_result" {
					continue
				}
				if noTools {
					return Entry{}, false
				}
				toolDesc := ""
				mu.Lock()
				if b.ToolUseId != "" {
					toolDesc = toolCalls[b.ToolUseId]
					delete(toolCalls, b.ToolUseId)
				}
				mu.Unlock()
				text := extractText(b.Content)
				if text != "" || toolDesc != "" {
					return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "Tool", Text: text, ToolDesc: toolDesc}, true
				}
				return Entry{}, false
			}
		}
		text := extractText(rec.Message.Content)
		if text == "" {
			return Entry{}, false
		}
		text = cleanUserText(text)
		if text == "" {
			return Entry{}, false
		}
		return Entry{Time: t, Ts: ts, Unix: t.UnixMilli(), SID: displaySID, Role: "User", Text: text}, true
	}
	return Entry{}, false
}

// Text extraction

func registerToolCalls(raw json.RawMessage) {
	var blocks []ContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if (b.Type != "tool_use" && b.Type != "toolCall") || b.ID == "" {
			continue
		}
		var inp ToolInput
		raw := b.Input
		if raw == nil {
			raw = b.Arguments
		}
		if raw != nil {
			json.Unmarshal(raw, &inp)
		}
		desc := ""
		switch b.Name {
		case "exec":
			desc = inp.Command
		case "read", "Read", "write", "Write", "edit", "Edit":
			desc = firstNonEmpty(inp.Path, inp.FilePath)
		case "process":
			desc = inp.Action
		case "web_search":
			desc = inp.Query
		case "web_fetch":
			desc = inp.URL
		default:
			desc = firstNonEmpty(inp.Action, inp.Path, inp.Command, inp.Query, inp.URL)
		}
		if len(desc) > 120 {
			desc = desc[:120] + "..."
		}
		mu.Lock()
		toolCalls[b.ID] = desc
		mu.Unlock()
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func extractText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func cleanUserText(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if isMetadataHeader(trimmed) {
			i++
			i = skipCodeBlock(lines, i)
			continue
		}
		if strings.HasPrefix(trimmed, "System: [") {
			i++
			continue
		}
		result = append(result, lines[i])
		i++
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

func isMetadataHeader(line string) bool {
	return strings.HasPrefix(line, "Conversation info (untrusted metadata):") ||
		strings.HasPrefix(line, "Sender (untrusted metadata):") ||
		strings.HasPrefix(line, "Replied message (untrusted, for context):")
}

func skipCodeBlock(lines []string, i int) int {
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
		i++
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "```" {
				i++
				return i
			}
			i++
		}
	}
	return i
}

// Time

func parseTime(ts string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, ts)
	}
	return t
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "????-??-?? ??:??:??"
	}
	return t.In(timezone).Format("2006.01.02 15:04:05")
}

// CLI output

func col(c string) string {
	if noColor {
		return ""
	}
	return c
}

var cliToolNames = map[string]string{
	"memory_search": "memory",
	"web_search":    "search",
	"web_fetch":     "fetch",
}

func cliToolName(name string) string {
	if short, ok := cliToolNames[name]; ok {
		return short
	}
	return strings.ToLower(name)
}

func printEntryCLI(e *Entry) {
	text := e.Text

	if e.Role == "Tool" {
		var desc []string
		if e.ToolDesc != "" {
			desc = append(desc, e.ToolDesc)
		}
		if e.ToolMeta != "" {
			desc = append(desc, e.ToolMeta)
		}
		if len(desc) > 0 && text != "" {
			text = strings.Join(desc, " ") + "\n" + text
		} else if len(desc) > 0 {
			text = strings.Join(desc, " ")
		}
	}

	text = strings.ReplaceAll(text, "\r", "")

	// Spoiler: collapse long tool output (same threshold as web)
	if e.Role == "Tool" {
		lines := strings.Split(text, "\n")
		const spoilerThreshold = 10
		const spoilerHard = 15
		if len(lines) > spoilerHard {
			hidden := len(lines) - spoilerThreshold
			text = strings.Join(lines[:spoilerThreshold], "\n") +
				fmt.Sprintf("\n<... %d more lines>", hidden)
		}
	}

	color := cGray
	switch e.Role {
	case "User":
		color = cBlue
	case "Agent":
		color = cGreen
	case "Tool":
		color = cGray
	}

	// Use toolName as role label for Tool entries
	roleLabel := e.Role
	if e.Role == "Tool" && e.ToolName != "" {
		roleLabel = cliToolName(e.ToolName)
	}

	header := fmt.Sprintf("[%s] [%s] %s: ", e.Ts, e.SID, roleLabel)
	indent := strings.Repeat(" ", len(header))

	lines := strings.Split(text, "\n")
	colorHeader := fmt.Sprintf("%s[%s]%s %s[%s]%s %s%s:%s ",
		col(cDim), e.Ts, col(cReset),
		col(cDim), e.SID, col(cReset),
		col(color), roleLabel, col(cReset),
	)
	fmt.Print(colorHeader)
	for i, line := range lines {
		if i > 0 {
			fmt.Print(indent)
		}
		fmt.Println(line)
	}
}
