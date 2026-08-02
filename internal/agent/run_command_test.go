package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestAgentCommandPrefixIsLocalAndPreservesArgv(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "prefix-ran")
	runtime := config.AgentRuntime{
		CommandPrefix: `export BRIDGE_PROXY_MARKER=enabled; printf ran >"$PREFIX_MARKER_FILE"`,
	}
	cmd := agentCommand(runtime, "/bin/sh", "-c", `printf '%s\n%s\n' "$BRIDGE_PROXY_MARKER" "$1"`, "agent", `literal;$(touch nope)`)
	cmd.Env = append(os.Environ(), "PREFIX_MARKER_FILE="+marker)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "enabled\nliteral;$(touch nope)\n" {
		t.Fatalf("output = %q", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prefix marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "nope")); !os.IsNotExist(err) {
		t.Fatalf("agent argv was interpreted by the shell")
	}
	if os.Getenv("BRIDGE_PROXY_MARKER") != "" {
		t.Fatal("prefix environment leaked into the bridge process")
	}
}

func TestAgentCommandPrefixFailureStopsLaunch(t *testing.T) {
	cmd := agentCommand(config.AgentRuntime{CommandPrefix: "exit 23"}, "/bin/sh", "-c", "echo should-not-run")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected prefix failure, output = %q", out)
	}
	if strings.Contains(string(out), "should-not-run") {
		t.Fatalf("agent launched after prefix failure: %q", out)
	}
}

func TestAgentCommandContextDirectMode(t *testing.T) {
	cmd := agentCommandContext(context.Background(), config.AgentRuntime{}, "/bin/sh", "-c", "printf direct")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "direct" {
		t.Fatalf("output = %q", out)
	}
}

func TestAgentCommandFindsUserInstalledCLIWithMinimalPath(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".volta", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "pi")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf user-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	cmd := agentCommand(config.AgentRuntime{}, "pi")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "user-cli" {
		t.Fatalf("output = %q", out)
	}
	if cmd.Path != binary {
		t.Fatalf("resolved path = %q, want %q", cmd.Path, binary)
	}
}

func TestResolveAgentBinaryIgnoresNonExecutableCandidate(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".volta", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	if got := resolveAgentBinary("pi"); got != "pi" {
		t.Fatalf("resolved non-executable candidate to %q", got)
	}
}

func TestAgentCommandSupportsInteractiveShellAlias(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	zdotdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(zdotdir, ".zshrc"), []byte("alias proxy_on='export PREFIX_ALIAS_OK=yes'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := agentCommand(config.AgentRuntime{
		CommandPrefix: "proxy_on",
		Shell:         zsh,
		ShellArgs:     []string{"-ic"},
	}, "/bin/sh", "-c", `printf %s "$PREFIX_ALIAS_OK"`)
	cmd.Env = append(os.Environ(), "ZDOTDIR="+zdotdir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if string(out) != "yes" {
		t.Fatalf("output = %q", out)
	}
}
