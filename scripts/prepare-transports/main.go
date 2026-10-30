// Prepare the pinned transport dependencies used by the sing-box user-update patch.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type dependency struct{ name, commit, patch string }

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func prepare(dep dependency) error {
	patch, err := filepath.Abs(filepath.Join("patches", dep.patch))
	if err != nil {
		return err
	}
	if _, err := os.Stat(patch); err != nil {
		return err
	}
	dir := filepath.Join("third_party", dep.name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if _, err := git(dir, "init", "--quiet"); err != nil {
			return err
		}
		if _, err := git(dir, "remote", "add", "origin", "https://github.com/SagerNet/"+dep.name+".git"); err != nil {
			return err
		}
		if _, err := git(dir, "fetch", "--depth", "1", "origin", dep.commit); err != nil {
			return err
		}
		if _, err := git(dir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != dep.commit {
		return fmt.Errorf("unexpected %s revision %s; expected %s (checkout left untouched)", dep.name, head, dep.commit)
	}
	if _, err := git(dir, "apply", "--reverse", "--check", patch); err == nil {
		return nil
	}
	status, err := git(dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%s contains changes; refusing to overwrite them", dep.name)
	}
	if _, err := git(dir, "apply", "--check", patch); err != nil {
		return err
	}
	_, err = git(dir, "apply", patch)
	return err
}

func main() {
	for _, dep := range []dependency{
		{"sing-vmess", "a0c58542bd9d99b00ea0c3cfacce6f03cf8a4017", "sing-vmess-user-snapshot.patch"},
		{"sing-quic", "7e0a5869a5f49ed0a685143345dfda0448bbc708", "sing-quic-tuic-users.patch"},
	} {
		if err := prepare(dep); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(dep.name + " pinned source + user-update concurrency patch ready")
	}
}
