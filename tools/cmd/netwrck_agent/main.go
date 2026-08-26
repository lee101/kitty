// License: GPLv3 Copyright: 2026

package netwrck_agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kovidgoyal/kitty/tools/cli"
)

type Options struct {
	ListenAddr string `json:"listen_addr"`
	Cwd        string `json:"cwd"`
	CodexBin   string `json:"codex_bin"`
}

type doctorReport struct {
	Cwd          string `json:"cwd"`
	CodexBin     string `json:"codex_bin,omitempty"`
	CodexFound   bool   `json:"codex_found"`
	GobedReady   bool   `json:"gobed_ready"`
	GobedError   string `json:"gobed_error,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
}

func defaultCwd(raw string) string {
	if raw != "" {
		return raw
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func runDoctor(opts *Options) (int, error) {
	cwd := defaultCwd(opts.Cwd)
	report := doctorReport{Cwd: cwd}
	if bin, err := resolveCodexBin(opts.CodexBin); err == nil {
		report.CodexFound = true
		report.CodexBin = bin
	}
	model, err := loadGobedModel()
	if err == nil {
		report.GobedReady = true
		report.DefaultModel = fmt.Sprintf("embed_dim=%d", model.EmbedDim)
	} else {
		report.GobedError = err.Error()
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return 1, err
	}
	if !report.CodexFound || !report.GobedReady {
		return 1, nil
	}
	return 0, nil
}

func runServe(opts *Options) (int, error) {
	cwd := defaultCwd(opts.Cwd)
	router, err := newRouter()
	if err != nil {
		return 1, err
	}
	agent, err := newCodexClient(opts.CodexBin, cwd)
	if err != nil {
		return 1, err
	}
	defer agent.Close()

	svc := &service{
		defaultCwd: cwd,
		router:     router,
		agent:      agent,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", svc.handleHealthz)
	mux.HandleFunc("/route", svc.handleRoute)
	mux.HandleFunc("/shell", svc.handleShell)
	mux.HandleFunc("/agent", svc.handleAgent)

	server := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "netwrck-agent listening on http://%s\n", opts.ListenAddr)
	return 0, server.ListenAndServe()
}

func EntryPoint(root *cli.Command) *cli.Command {
	cmd := root.AddSubCommand(&cli.Command{
		Name:                 "netwrck-agent",
		ShortDescription:     "Run the Netwrck routing bridge for shell plus codex-agent execution",
		Usage:                "subcommand [options]",
		SubCommandIsOptional: false,
	})

	addCommonOpts := func(sc *cli.Command) {
		sc.Add(cli.OptionSpec{
			Name:    "--listen-addr",
			Default: "127.0.0.1:8420",
			Help:    "Address for the HTTP bridge when using the serve subcommand.",
		})
		sc.Add(cli.OptionSpec{
			Name: "--cwd",
			Help: "Default working directory for shell and agent requests.",
		})
		sc.Add(cli.OptionSpec{
			Name: "--codex-bin",
			Help: "Explicit path to the codex binary. Defaults to CODEX_BIN, ../codex target builds, or PATH.",
		})
	}

	serve := cmd.AddSubCommand(&cli.Command{
		Name: "serve",
		Run: func(cmd *cli.Command, args []string) (int, error) {
			opts := &Options{}
			if err := cmd.GetOptionValues(opts); err != nil {
				return 1, err
			}
			return runServe(opts)
		},
	})
	addCommonOpts(serve)

	doctor := cmd.AddSubCommand(&cli.Command{
		Name: "doctor",
		Run: func(cmd *cli.Command, args []string) (int, error) {
			opts := &Options{}
			if err := cmd.GetOptionValues(opts); err != nil {
				return 1, err
			}
			return runDoctor(opts)
		},
	})
	addCommonOpts(doctor)

	return cmd
}

type service struct {
	defaultCwd string
	router     *routerModel
	agent      *codexClient
}

type routeRequest struct {
	Input string `json:"input"`
	Cwd   string `json:"cwd,omitempty"`
}

type routeResponse struct {
	Lane          string   `json:"lane"`
	Reason        string   `json:"reason"`
	Confidence    float64  `json:"confidence"`
	RunAsChoices  []string `json:"run_as_choices,omitempty"`
	ResolvedInput string   `json:"resolved_input,omitempty"`
}

type shellRequest struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
}

type shellResponse struct {
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type agentRequest struct {
	Prompt   string `json:"prompt"`
	Cwd      string `json:"cwd,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

type agentResponse struct {
	ThreadID string `json:"thread_id"`
	Response string `json:"response"`
}

func (s *service) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"cwd": s.defaultCwd,
	})
}

func (s *service) handleRoute(w http.ResponseWriter, r *http.Request) {
	req := routeRequest{}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	route := s.router.Route(strings.TrimSpace(req.Input))
	resp := routeResponse{
		Lane:       route.Lane,
		Reason:     route.Reason,
		Confidence: route.Confidence,
	}
	if route.Lane == laneAmbiguous {
		resp.RunAsChoices = []string{laneShell, laneAgent}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *service) handleShell(w http.ResponseWriter, r *http.Request) {
	req := shellRequest{}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cwd := s.resolveCwd(req.Cwd)
	resp, err := runShellCommand(cwd, strings.TrimSpace(req.Command))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *service) handleAgent(w http.ResponseWriter, r *http.Request) {
	req := agentRequest{}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cwd := s.resolveCwd(req.Cwd)
	threadID, text, err := s.agent.RunPrompt(cwd, req.ThreadID, strings.TrimSpace(req.Prompt))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, agentResponse{
		ThreadID: threadID,
		Response: text,
	})
}

func (s *service) resolveCwd(in string) string {
	if in == "" {
		return s.defaultCwd
	}
	if filepath.IsAbs(in) {
		return in
	}
	return filepath.Join(s.defaultCwd, in)
}

const (
	maxJSONBodyBytes = 1 << 20
	shellTimeout     = 2 * time.Minute
	maxShellOutput   = 2 << 20
)

func decodeJSON(r *http.Request, dest any) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBodyBytes)
	return json.NewDecoder(r.Body).Decode(dest)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func runShellCommand(cwd, command string) (*shellResponse, error) {
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	if strings.HasPrefix(command, "cd ") || command == "cd" {
		next := strings.TrimSpace(strings.TrimPrefix(command, "cd"))
		if next == "" {
			next = os.Getenv("HOME")
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(cwd, next)
		}
		if st, err := os.Stat(next); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("directory not found: %s", next)
		}
		return &shellResponse{Command: command, Cwd: next, ExitCode: 0}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = cwd
	var stdout, stderr limitedBuffer
	stdout.limit = maxShellOutput
	stderr.limit = maxShellOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	resp := &shellResponse{Command: command, Cwd: cwd, Stdout: stdout.String()}
	if err == nil {
		return resp, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		resp.ExitCode = ee.ExitCode()
		resp.Stderr = stderr.String()
		return resp, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		resp.ExitCode = -1
		resp.Stderr = fmt.Sprintf("timed out after %s\n%s", shellTimeout, stderr.String())
		return resp, nil
	}
	return nil, err
}

// limitedBuffer caps captured output so a chatty command cannot exhaust
// memory; the head is kept and truncation is marked.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.limit {
		b.truncated = true
		return len(p), nil
	}
	n := min(b.limit-b.buf.Len(), len(p))
	b.buf.Write(p[:n])
	if n < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s += "\n...[output truncated]"
	}
	return s
}

type codexClient struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     *json.Encoder
	stdout    *json.Decoder
	threadIDs map[string]string
}

func newCodexClient(codexBin, cwd string) (*codexClient, error) {
	bin, err := resolveCodexBin(codexBin)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "app-server", "--listen", "stdio://")
	cmd.Dir = cwd
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &codexClient{
		cmd:       cmd,
		stdin:     json.NewEncoder(in),
		stdout:    json.NewDecoder(out),
		threadIDs: make(map[string]string),
	}
	if err := c.initialize(); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return c, nil
}

func (c *codexClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
}

func (c *codexClient) initialize() error {
	req := map[string]any{
		"id":     "initialize",
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{
				"name":    "kitty_netwrck_agent",
				"title":   "Kitty Netwrck Agent",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
		},
	}
	if err := c.stdin.Encode(req); err != nil {
		return err
	}
	for {
		msg, err := c.nextMessage()
		if err != nil {
			return err
		}
		if msg["id"] == "initialize" {
			break
		}
	}
	return c.stdin.Encode(map[string]any{"method": "initialized", "params": map[string]any{}})
}

func (c *codexClient) RunPrompt(cwd, threadID, prompt string) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prompt == "" {
		return "", "", fmt.Errorf("prompt is required")
	}
	if threadID == "" {
		var err error
		threadID, err = c.startThread(cwd)
		if err != nil {
			return "", "", err
		}
	}
	turnID, err := c.startTurn(threadID, cwd, prompt)
	if err != nil {
		return "", "", err
	}
	var delta strings.Builder
	var final string
	for {
		msg, err := c.nextMessage()
		if err != nil {
			return threadID, "", err
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		switch method {
		case "item/agentMessage/delta":
			if asString(params["turnId"]) == turnID {
				delta.WriteString(asString(params["delta"]))
			}
		case "item/completed":
			if asString(params["turnId"]) != turnID {
				continue
			}
			item, _ := params["item"].(map[string]any)
			if asString(item["type"]) == "agentMessage" {
				final = asString(item["text"])
			}
		case "turn/completed":
			turn, _ := params["turn"].(map[string]any)
			if asString(turn["id"]) != turnID {
				continue
			}
			if final == "" {
				final = delta.String()
			}
			if final == "" {
				final = "(no assistant output)"
			}
			return threadID, final, nil
		}
	}
}

func (c *codexClient) startThread(cwd string) (string, error) {
	id := fmt.Sprintf("thread-start-%d", time.Now().UnixNano())
	req := map[string]any{
		"id":     id,
		"method": "thread/start",
		"params": map[string]any{
			"cwd":            cwd,
			"approvalPolicy": "never",
			"sandbox":        "danger-full-access",
		},
	}
	if err := c.stdin.Encode(req); err != nil {
		return "", err
	}
	for {
		msg, err := c.nextMessage()
		if err != nil {
			return "", err
		}
		if msg["id"] != id {
			continue
		}
		result, _ := msg["result"].(map[string]any)
		thread, _ := result["thread"].(map[string]any)
		threadID := asString(thread["id"])
		if threadID == "" {
			return "", fmt.Errorf("codex thread/start returned no thread id")
		}
		return threadID, nil
	}
}

func (c *codexClient) startTurn(threadID, cwd, prompt string) (string, error) {
	id := fmt.Sprintf("turn-start-%d", time.Now().UnixNano())
	req := map[string]any{
		"id":     id,
		"method": "turn/start",
		"params": map[string]any{
			"threadId": threadID,
			"cwd":      cwd,
			"input": []map[string]any{
				{"type": "text", "text": prompt},
			},
		},
	}
	if err := c.stdin.Encode(req); err != nil {
		return "", err
	}
	for {
		msg, err := c.nextMessage()
		if err != nil {
			return "", err
		}
		if msg["id"] != id {
			continue
		}
		result, _ := msg["result"].(map[string]any)
		turn, _ := result["turn"].(map[string]any)
		turnID := asString(turn["id"])
		if turnID == "" {
			return "", fmt.Errorf("codex turn/start returned no turn id")
		}
		return turnID, nil
	}
}

func (c *codexClient) nextMessage() (map[string]any, error) {
	var msg map[string]any
	if err := c.stdout.Decode(&msg); err != nil {
		return nil, err
	}
	if _, ok := msg["error"]; ok {
		return nil, fmt.Errorf("codex app-server error: %v", msg["error"])
	}
	return msg, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
