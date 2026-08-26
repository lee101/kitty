package netwrck_agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lee101/gobed"
)

const (
	laneShell     = "shell"
	laneAgent     = "agent"
	laneAmbiguous = "ambiguous"
)

type routeDecision struct {
	Lane       string
	Reason     string
	Confidence float64
}

type routerModel struct {
	model        *gobed.EmbeddingModel
	shellPhrases []string
	agentPhrases []string
	shellVecs    [][]float32
	agentVecs    [][]float32
}

var (
	modelOnce   sync.Once
	modelValue  *gobed.EmbeddingModel
	modelErr    error
	shellStarts = map[string]struct{}{
		"ls": {}, "pwd": {}, "cd": {}, "git": {}, "grep": {}, "rg": {}, "find": {},
		"cat": {}, "sed": {}, "awk": {}, "make": {}, "npm": {}, "pnpm": {}, "yarn": {},
		"go": {}, "cargo": {}, "python": {}, "pytest": {}, "uv": {}, "just": {},
		"docker": {}, "kubectl": {}, "echo": {}, "mkdir": {}, "rm": {}, "cp": {}, "mv": {},
		"touch": {}, "chmod": {}, "head": {}, "tail": {}, "wc": {}, "ps": {}, "top": {},
	}
)

func loadGobedModel() (*gobed.EmbeddingModel, error) {
	modelOnce.Do(func() {
		modelValue, modelErr = gobed.LoadModel()
	})
	return modelValue, modelErr
}

func newRouter() (*routerModel, error) {
	model, err := loadGobedModel()
	if err != nil {
		return nil, fmt.Errorf("load gobed model: %w", err)
	}
	r := &routerModel{
		model: model,
		shellPhrases: []string{
			"ls", "git status", "git diff", "grep foo -R .", "npm run serve",
			"make test", "build app", "open config", "search auth in files",
			"run pytest", "show current directory", "list repository files",
		},
		agentPhrases: []string{
			"explain this repo", "fix this failing test", "why is this build failing",
			"summarize the last 3 commits", "find where login state is stored",
			"what should I change", "debug the failing build", "review this codebase",
			"search auth logic and explain it", "help me find the bug",
		},
	}
	for _, phrase := range r.shellPhrases {
		vec, err := r.model.Encode(phrase)
		if err != nil {
			return nil, fmt.Errorf("encode shell phrase %q: %w", phrase, err)
		}
		r.shellVecs = append(r.shellVecs, vec)
	}
	for _, phrase := range r.agentPhrases {
		vec, err := r.model.Encode(phrase)
		if err != nil {
			return nil, fmt.Errorf("encode agent phrase %q: %w", phrase, err)
		}
		r.agentVecs = append(r.agentVecs, vec)
	}
	return r, nil
}

func (r *routerModel) Route(input string) routeDecision {
	input = strings.TrimSpace(input)
	if input == "" {
		return routeDecision{Lane: laneAmbiguous, Reason: "empty input", Confidence: 0}
	}
	if looksLikeNaturalLanguage(input) {
		return routeDecision{Lane: laneAgent, Reason: "natural language request", Confidence: 0.98}
	}
	if looksLikeShell(input) {
		if !firstTokenExists(input) {
			return routeDecision{Lane: laneAgent, Reason: "command token not found on PATH; falling back to agent", Confidence: 0.93}
		}
		return routeDecision{Lane: laneShell, Reason: "obvious shell command", Confidence: 0.99}
	}
	return r.routeWithEmbeddings(input)
}

func (r *routerModel) routeWithEmbeddings(input string) routeDecision {
	if !firstTokenExists(input) && !strings.Contains(input, " ") {
		return routeDecision{Lane: laneAgent, Reason: "token is not executable; treating as agent intent", Confidence: 0.8}
	}
	vec, err := r.model.Encode(input)
	if err != nil {
		return routeDecision{Lane: laneAmbiguous, Reason: "embedding classification failed", Confidence: 0}
	}
	shellScore := bestSimilarity(vec, r.shellVecs)
	agentScore := bestSimilarity(vec, r.agentVecs)
	diff := shellScore - agentScore
	if diff > 0.08 {
		if !firstTokenExists(input) && maybeCommandLike(input) {
			return routeDecision{Lane: laneAgent, Reason: "shell-like text but command token is missing", Confidence: float64(diff)}
		}
		return routeDecision{Lane: laneShell, Reason: fmt.Sprintf("gobed routing favored shell (%.3f vs %.3f)", shellScore, agentScore), Confidence: float64(diff)}
	}
	if diff < -0.08 {
		return routeDecision{Lane: laneAgent, Reason: fmt.Sprintf("gobed routing favored agent (%.3f vs %.3f)", agentScore, shellScore), Confidence: float64(-diff)}
	}
	return routeDecision{Lane: laneAmbiguous, Reason: fmt.Sprintf("gobed scores were close (shell %.3f vs agent %.3f)", shellScore, agentScore), Confidence: float64(abs32(diff))}
}

func looksLikeNaturalLanguage(input string) bool {
	lower := strings.ToLower(input)
	prefixes := []string{
		"why ", "how ", "what ", "where ", "explain ", "fix ", "summarize ",
		"find ", "review ", "debug ", "help ", "can you ", "could you ",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return strings.ContainsAny(input, "?!") || strings.Contains(lower, " failing ") || strings.Contains(lower, " repo")
}

func looksLikeShell(input string) bool {
	if strings.ContainsAny(input, "|><;&") {
		return true
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	if _, ok := shellStarts[fields[0]]; ok {
		return true
	}
	return strings.HasPrefix(input, "./") || strings.HasPrefix(input, "../") || strings.HasPrefix(input, "/")
}

func maybeCommandLike(input string) bool {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	if _, ok := shellStarts[fields[0]]; ok {
		return true
	}
	return len(fields) <= 3
}

func firstTokenExists(input string) bool {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	token := fields[0]
	switch token {
	case "cd", "export", "unset":
		return true
	}
	if strings.Contains(token, string(os.PathSeparator)) {
		if !filepath.IsAbs(token) {
			if wd, err := os.Getwd(); err == nil {
				token = filepath.Join(wd, token)
			}
		}
		info, err := os.Stat(token)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(token)
	return err == nil
}

func bestSimilarity(query []float32, candidates [][]float32) float32 {
	best := float32(-1)
	for _, candidate := range candidates {
		score := gobed.CosineSimilarity(query, candidate)
		if score > best {
			best = score
		}
	}
	return best
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
