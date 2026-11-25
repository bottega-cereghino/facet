package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// reapQueueWorkspace runs facet's guarded path reap for an issue workspace.
// A missing workspace needs no cleanup; every other error is reported so the
// operator can handle a workspace reap refused to remove.
func reapQueueWorkspace(repo string, number int, mergedHead string) error {
	ws, err := findIssueWorkspace(roots.Workspaces, repo, number)
	if err != nil {
		return err
	}
	if ws == "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find facet executable to reap %s#%d: %w", repo, number, err)
	}
	args := []string{"reap", "--path", ws, "--yes"}
	if mergedHead != "" {
		args = append(args, "--merged-head", mergedHead)
	}
	out, err := exec.Command(exe, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reap workspace for %s#%d: %w: %s", repo, number, err, reapReason(string(out)))
	}
	return nil
}

// reapReason is what reap said, on one line: its lines joined with "; ", the
// last 600 bytes at most, since the reason is at the end.
func reapReason(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	s := strings.Join(lines, "; ")
	if len(s) > 600 {
		s = "..." + s[len(s)-600:]
	}
	if s == "" {
		return "reap printed nothing"
	}
	return s
}

// prepareWorkspace returns the workspace directory for repo#number, spawning
// one only when there is not one already. It never deletes a workspace.
func prepareWorkspace(repo string, number int) (string, error) {
	return prepareWorkspaceSpawn(roots.Workspaces, repo, number, func() error {
		// Yes skips the confirmation: a queue has no one to answer the prompt.
		// The list already required the issue to be open.
		return runSpawn(queueSpawnOpts(repo, number))
	})
}

// queueSpawnOpts is how the queue spawns a workspace. Every repository is a
// clone that shares no file with its source: a worktree would make
// each commit, ref update and hook of the agent write into another checkout's
// .git, and a sandbox that confines the agent to its workspace would have to
// open that directory.
func queueSpawnOpts(repo string, number int) spawnOpts {
	return spawnOpts{Number: number, Repo: repo, Yes: true, NoWorktree: true, NoHardlinks: true}
}

// prepareWorkspaceSpawn uses spawn only when findIssueWorkspace reports none.
// A spawn that fails after a workspace has appeared (two queues, a moment
// apart) returns that workspace rather than the error.
func prepareWorkspaceSpawn(root, repo string, number int, spawn func() error) (string, error) {
	ws, err := findIssueWorkspace(root, repo, number)
	if err != nil {
		return "", err
	}
	if ws != "" {
		return ws, nil
	}
	spawnErr := spawn()
	ws, err = findIssueWorkspace(root, repo, number)
	if err != nil {
		return "", err
	}
	if ws != "" {
		return ws, nil
	}
	if spawnErr != nil {
		return "", spawnErr
	}
	return "", fmt.Errorf("spawn %s#%d did not create a workspace under %s\nfix: run `facet spawn %d --repo %s -y` and read why it stopped",
		repo, number, root, number, repo)
}

// findIssueWorkspace looks one level down from the workspaces root, at real
// directories whose names start with the issue prefix. A symlink is not
// followed, so a link cannot point the search outside the root. Two
// workspaces for one issue is a refusal: guessing would start the agent in
// the wrong tree.
func findIssueWorkspace(root, repo string, number int) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read workspaces %s: %w\nfix: FACET_WORKSPACES must be a directory", root, err)
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), config.IssuePrefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		m, err := readIssueManifest(dir)
		if err != nil {
			return "", err
		}
		if m == nil || m.Issue == nil || m.Issue.Repo != repo || m.Issue.Number != number {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("two workspaces for %s#%d (%s and %s)\nfix: leave only one of them, so the queue does not guess which to start in",
				repo, number, found, dir)
		}
		found = dir
	}
	return found, nil
}

// readIssueManifest returns nil when the directory has no manifest. A
// manifest that cannot be read or parsed is an error: skipping it could
// hide the workspace this issue already has, and the queue would spawn
// another.
func readIssueManifest(dir string) (*manifest.Manifest, error) {
	b, err := os.ReadFile(manifest.Path(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w\nfix: repair or move that workspace before the queue starts, so an existing one is not missed", manifest.Path(dir), err)
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w\nfix: repair or move that workspace before the queue starts, so an existing one is not missed", manifest.Path(dir), err)
	}
	return m, nil
}
