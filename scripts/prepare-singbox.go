// Prepare the pinned upstream sing-box source and apply the reviewed node patch.
// Run from the repository root: go run ./scripts/prepare-singbox.go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const upstream = "c005f35300ce76427dc285481ec5695f5b179cd2"

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func prepare() error {
	patch, err := filepath.Abs("patches/sing-box-1.14.4.patch")
	if err != nil {
		return err
	}
	if _, err := os.Stat(patch); err != nil {
		return err
	}
	dir := filepath.Join("third_party", "sing-box")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll("third_party", 0755); err != nil {
			return err
		}
		if _, err := git(".", "clone", "--depth", "1", "--branch", "v1.14.4", "https://github.com/SagerNet/sing-box.git", dir); err != nil {
			return err
		}
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != upstream {
		return fmt.Errorf("unexpected sing-box revision %s; expected %s (existing checkout left untouched)", head, upstream)
	}
	if _, err := git(dir, "apply", "--reverse", "--check", patch); err == nil {
		return nil
	}
	status, err := git(dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("sing-box checkout contains changes; refusing to overwrite them")
	}
	if _, err := git(dir, "apply", "--check", patch); err != nil {
		return err
	}
	_, err = git(dir, "apply", patch)
	return err
}

func main() {
	if err := prepare(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("sing-box v1.14.4 + Xboard compatibility patch ready")
}
