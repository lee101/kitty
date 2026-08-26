package netwrck_agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func resolveCodexBin(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("CODEX_BIN"); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates,
		"../codex/target/debug/codex",
		"../codex/codex-rs/target/debug/codex",
		"/home/lee/code/codex/target/debug/codex",
		"/home/lee/code/codex/codex-rs/target/debug/codex",
	)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path := candidate
		if !filepath.IsAbs(path) {
			wd, err := os.Getwd()
			if err == nil {
				path = filepath.Join(wd, candidate)
			}
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	if path, err := exec.LookPath("codex"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("could not find codex binary; set --codex-bin or CODEX_BIN, or build ../codex")
}
