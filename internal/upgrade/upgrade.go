// Package upgrade implements self-upgrade from the source repository:
// fetch + fast-forward pull, rebuild, smoke-check the new binary, then
// atomically swap it into place. Running instances keep their old binary
// until restarted (the dashboard flags them as outdated).
package upgrade

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"lark-coding-agent-bridge-go/internal/buildinfo"
)

// Options parameterizes a Run.
type Options struct {
	CheckOnly bool
	SourceDir string
	Stdout    io.Writer
}

// Run executes the upgrade flow.
func Run(opts Options) error {
	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}

	src, err := findSourceDir(opts.SourceDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "源码目录: %s\n", src)

	// Fetch and compare.
	if err := git(src, "fetch", "--quiet"); err != nil {
		return fmt.Errorf("git fetch 失败: %w", err)
	}
	local, _ := gitOutput(src, "rev-parse", "HEAD")
	// No upstream (local-only repo): fall back to comparing the binary
	// against local HEAD and skip the pull step.
	remote, remoteErr := gitOutput(src, "rev-parse", "@{u}")
	hasUpstream := remoteErr == nil
	if !hasUpstream {
		remote = local
	}
	current := buildinfo.Current()
	fmt.Fprintf(out, "当前二进制: %s\n", current.Short())
	fmt.Fprintf(out, "本地 HEAD:  %s\n", short(local))
	if hasUpstream {
		fmt.Fprintf(out, "远端 HEAD:  %s\n", short(remote))
	} else {
		fmt.Fprintln(out, "（本地仓库无远端跟踪，按本地 HEAD 比对并跳过 pull）")
	}

	if local == remote && current.Commit == local && !current.Dirty {
		fmt.Fprintln(out, "已是最新，无需升级。")
		return nil
	}
	if opts.CheckOnly {
		if local != remote {
			behind, _ := gitOutput(src, "rev-list", "--count", "HEAD..@{u}")
			fmt.Fprintf(out, "可升级: 本地落后远端 %s 个提交。\n", behind)
		} else if current.Commit != local || current.Dirty {
			fmt.Fprintln(out, "可升级: 二进制与源码不一致（二进制较旧或含未提交改动）。")
		}
		return nil
	}

	// Pull (fast-forward only: never clobber local work).
	if hasUpstream && local != remote {
		if err := git(src, "pull", "--ff-only"); err != nil {
			return fmt.Errorf("git pull --ff-only 失败（本地可能有未提交改动）: %w", err)
		}
		fmt.Fprintln(out, "已拉取最新代码。")
	}

	// Build to a temp file first.
	tmp, err := os.CreateTemp("", "lark-coding-agent-bridge-go-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	fmt.Fprintln(out, "正在构建…")
	build := exec.Command("go", "build", "-o", tmpPath, "./cmd/lark-coding-agent-bridge-go")
	build.Dir = src
	if out2, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("构建失败: %w\n%s", err, out2)
	}
	if err := maybeCodesign(tmpPath, out); err != nil {
		return err
	}

	// Smoke-check the fresh binary.
	if out2, err := exec.Command(tmpPath, "version").CombinedOutput(); err != nil {
		return fmt.Errorf("新二进制自检失败: %w\n%s", err, out2)
	}

	// Atomic swap: rename over the running executable's path.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, self); err != nil {
		return fmt.Errorf("替换二进制失败（%s）: %w", self, err)
	}

	newHead, _ := gitOutput(src, "rev-parse", "HEAD")
	fmt.Fprintf(out, "✓ 升级完成: %s → %s\n", current.Short(), short(newHead))
	fmt.Fprintln(out, "正在运行的实例仍使用旧二进制，重启后生效（dashboard 里带 ⚠︎ 标记）。")
	return nil
}

// findSourceDir resolves the repo to build from: explicit flag, else the
// conventional checkout location, else the current directory's repo root.
func findSourceDir(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, "lark-coding-agent-bridge-go"))
		}
		if root, err := gitOutput(".", "rev-parse", "--show-toplevel"); err == nil {
			candidates = append(candidates, root)
		}
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("找不到源码仓库，请用 --source <dir> 指定")
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
