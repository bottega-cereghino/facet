// Package gitx wraps the git CLI.
//
// facet shells out rather than using a pure-Go git library, deliberately: it
// needs Git-LFS, credential helpers, SSH-agent auth, and the --local hardlink
// clone optimisation, none of which go-git provides.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs git. The interface exists so callers can be tested against a fake.
type Runner interface {
	// Run executes git in dir (empty for the working directory) with extra
	// environment entries, returning trimmed stdout.
	Run(dir string, env []string, args ...string) (string, error)
}

// Git is the real implementation.
type Git struct{}

// Error carries git's stderr, which is where the useful part always is.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// Run executes git, optionally in dir and with extra env entries. Passing env
// per-command means GIT_LFS_SKIP_SMUDGE is scoped to the one clone that wants
// it, rather than mutated globally and unset in a defer.
func (Git) Run(dir string, env []string, args ...string) (string, error) {
	return run(context.Background(), dir, env, args...)
}

// RunTimeout behaves like Run but kills git if it has not finished within
// timeout. Local git operations return in milliseconds; a network operation
// (fetch) against a blackholed connection or captive portal does not return
// at all. Callers that must not hang forever on the latter use this instead
// of Run.
func (Git) RunTimeout(dir string, env []string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := run(ctx, dir, env, args...)
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), timeout)
	}
	return out, err
}

// waitDelay bounds how long Wait() will wait for the stdout/stderr pipes to
// close once a context deadline kills the process. Killing `git fetch` does
// not kill an orphaned grandchild -- a remote helper (ssh, git-remote-ext,
// git-remote-https) that inherited the pipe and is still running -- so
// without this, Wait blocks until that grandchild exits on its own, which
// defeats the entire point of a bounded RunTimeout: a hung transport hangs
// the caller exactly as before, just with an extra unread context error.
const waitDelay = 2 * time.Second

func run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = environ(os.Environ(), env)
	if _, ok := ctx.Deadline(); ok {
		cmd.WaitDelay = waitDelay
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return trimOutput(stdout.String()), &Error{Args: args, ExitCode: code, Stderr: stderr.String()}
	}
	return trimOutput(stdout.String()), nil
}

// repositoryEnv names the variables that point git at a repository, index,
// object store, config or pathspec mode other than the one facet names: the
// list `git rev-parse --local-env-vars` prints, plus the namespace, the
// discovery ceiling and the pathspec modes. git sets GIT_DIR for a `!` alias
// (`alias.reap = !facet reap`), so a probe inheriting it reads that
// repository's refs and index with the clone as its work tree, and a clone
// with an unpushed branch reads clean. facet always says which repository it
// means, by running git in it, so none of these is ever wanted.
var repositoryEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GLOB_PATHSPECS":               true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_ICASE_PATHSPECS":              true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_LITERAL_PATHSPECS":            true,
	"GIT_NAMESPACE":                    true,
	"GIT_NOGLOB_PATHSPECS":             true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,
}

// fixedEnv is set on every call, so git reads what is on disk:
//   - GIT_NO_REPLACE_OBJECTS: replace refs are local and never fetched or
//     pushed, and `git replace --graft origin/main <local commit>` would make
//     that commit read as reachable from a remote.
//   - GIT_LITERAL_PATHSPECS: every path facet hands git is a file name, never
//     a pattern; a file named ":todo.md" or ":(exclude)*" is pathspec magic
//     otherwise, and hides itself (and its neighbours) from status and diff.
var fixedEnv = []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_LITERAL_PATHSPECS=1"}

// environ is base without repositoryEnv and GIT_CONFIG_KEY_n/VALUE_n, then
// fixedEnv, then extra. Credentials, SSH, LFS and the rest pass through.
func environ(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(fixedEnv)+len(extra))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if repositoryEnv[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, fixedEnv...)
	return append(out, extra...)
}

// trimOutput drops the trailing newline git ends its output with. Leading
// whitespace is kept: in `status --porcelain` it is the status column, and
// trimming it cut a character off the first path and hid a modified file.
func trimOutput(s string) string {
	return strings.TrimRight(s, " \t\r\n")
}

// IsRepo reports whether dir contains a git repository.
func IsRepo(dir string) bool {
	fi, err := os.Stat(dir + string(os.PathSeparator) + ".git")
	return err == nil && (fi.IsDir() || fi.Mode().IsRegular()) // .git is a file in a worktree
}

// Origin returns dir's `origin` remote URL, or "" when dir is not a repo or has
// no origin.
func Origin(r Runner, dir string) string {
	if !IsRepo(dir) {
		return ""
	}
	out, err := r.Run(dir, nil, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// RemoteURL returns the URL of the named remote, and whether it exists.
func RemoteURL(r Runner, dir, remote string) (string, bool) {
	out, err := r.Run(dir, nil, "remote", "get-url", "--", remote)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// CloneOptions controls Clone.
type CloneOptions struct {
	// SkipLFS fetches Git-LFS pointers instead of blobs.
	SkipLFS bool
	// SetOriginTo, when non-empty, repoints origin after cloning. Used when the
	// clone source is a local mirror but pushes must reach the forge.
	SetOriginTo string
}

// Clone clones src into dst, then checks out HEAD.
//
// It clones with --no-checkout and then runs `reset --hard`, because git
// refuses to run an active hook during clone (GIT_CLONE_PROTECTION_ACTIVE) and
// git-lfs installs a post-checkout hook. Populating the tree afterwards
// sidesteps that without weakening any git safety flag.
//
// When SetOriginTo is set, origin is repointed *before* the checkout, so that an
// LFS smudge pulls blobs from the forge rather than from a blob-less mirror.
//
// src and dst follow a `--`: they come from a user-authored manifest, and a URL
// such as `--upload-pack=<command>` would otherwise be read as an option. Every
// call here that passes a URL, path or name to git does the same.
func Clone(r Runner, src, dst string, opt CloneOptions) error {
	var env []string
	if opt.SkipLFS {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	if _, err := r.Run("", env, "clone", "--no-checkout", "--", src, dst); err != nil {
		return err
	}
	if opt.SetOriginTo != "" {
		if _, err := r.Run(dst, nil, "remote", "set-url", "--", "origin", opt.SetOriginTo); err != nil {
			return err
		}
	}
	if _, err := r.Run(dst, env, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		return err
	}
	return nil
}
