package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage"
)

func TestCheckRemoteConsistency_WorktreeFallbackUsesSharedConfig(t *testing.T) {
	clearResolveBeadsDirCache()
	t.Cleanup(clearResolveBeadsDirCache)

	mainRepoDir, worktreeDir := setupWorktreeRepo(t)
	beadsDir := filepath.Join(mainRepoDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("failed to create shared .beads: %v", err)
	}
	if err := (&configfile.Config{}).Save(beadsDir); err != nil {
		t.Fatalf("failed to write shared metadata: %v", err)
	}

	t.Setenv("BEADS_DOLT_SERVER_PORT", "1")

	check := CheckRemoteConsistency(worktreeDir)
	if check.Status != StatusWarning {
		t.Fatalf("expected warning when shared config resolves but server is unavailable, got %q: %s", check.Status, check.Message)
	}
	if check.Message == "N/A (not using Dolt backend)" {
		t.Fatalf("expected shared worktree config to be used, got %q", check.Message)
	}
}

func TestRemoteAdoptionDetailUsesGitOrigin(t *testing.T) {
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", "git@github.com:org/repo.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
	}

	detail := remoteAdoptionDetail(repoDir)
	for _, want := range []string{
		"git origin is configured",
		"bd dolt remote add origin git+ssh://git@github.com/org/repo.git",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("remote adoption detail missing %q:\n%s", want, detail)
		}
	}
}

func TestCheckRemoteConsistencyNoRemotesIncludesGitOriginAdoptionDetail(t *testing.T) {
	clearResolveBeadsDirCache()
	t.Cleanup(clearResolveBeadsDirCache)

	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", "git@github.com:org/repo.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
	}

	beadsDir := filepath.Join(repoDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}
	if err := (&configfile.Config{}).Save(beadsDir); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	oldQuerySQLRemotes := querySQLRemotesForDoctor
	oldListCLIRemotes := listCLIRemotesForDoctor
	t.Cleanup(func() {
		querySQLRemotesForDoctor = oldQuerySQLRemotes
		listCLIRemotesForDoctor = oldListCLIRemotes
	})
	querySQLRemotesForDoctor = func(string) ([]storage.RemoteInfo, error) {
		return nil, nil
	}
	listCLIRemotesForDoctor = func(string) ([]storage.RemoteInfo, error) {
		return nil, nil
	}

	check := CheckRemoteConsistency(repoDir)
	if check.Status != StatusWarning {
		t.Fatalf("expected warning when no remotes are configured, got %q: %s", check.Status, check.Message)
	}
	if !strings.Contains(check.Detail, "bd dolt remote add origin git+ssh://git@github.com/org/repo.git") {
		t.Fatalf("remote consistency detail missing git origin adoption command:\n%s", check.Detail)
	}
}

func TestCheckRemoteConsistencyFlagsStrandedRootRemote(t *testing.T) {
	clearResolveBeadsDirCache()
	t.Cleanup(clearResolveBeadsDirCache)

	repoDir := t.TempDir()
	beadsDir := filepath.Join(repoDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}
	if err := (&configfile.Config{}).Save(beadsDir); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Simulate the wrong-directory case: a remote added at the dolt server root
	// (.beads/dolt/) instead of the database subdir (.beads/dolt/beads/). Both
	// dirs must look like dolt dirs for the migration guards to pass.
	doltDir := filepath.Join(beadsDir, "dolt")
	dbDir := filepath.Join(doltDir, "beads")
	for _, d := range []string{filepath.Join(doltDir, ".dolt"), filepath.Join(dbDir, ".dolt")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("failed to create %s: %v", d, err)
		}
	}

	oldQuerySQLRemotes := querySQLRemotesForDoctor
	oldListCLIRemotes := listCLIRemotesForDoctor
	t.Cleanup(func() {
		querySQLRemotesForDoctor = oldQuerySQLRemotes
		listCLIRemotesForDoctor = oldListCLIRemotes
	})
	querySQLRemotesForDoctor = func(string) ([]storage.RemoteInfo, error) {
		return nil, nil
	}
	listCLIRemotesForDoctor = func(dir string) ([]storage.RemoteInfo, error) {
		if dir == doltDir {
			return []storage.RemoteInfo{{Name: "origin", URL: "git+ssh://git@github.com/org/repo.git"}}, nil
		}
		return nil, nil
	}

	check := CheckRemoteConsistency(repoDir)
	if check.Status != StatusWarning {
		t.Fatalf("expected warning for stranded root remote, got %q: %s", check.Status, check.Message)
	}
	if check.Fix == "" {
		t.Fatalf("expected non-empty Fix so --fix reaches the migration, got empty Fix\nDetail: %s", check.Detail)
	}
	if !strings.Contains(check.Detail, "stranded at server root") {
		t.Fatalf("expected detail to mention the stranded remote, got:\n%s", check.Detail)
	}
}

func TestRemoteAdoptionDetailWithoutGitOrigin(t *testing.T) {
	detail := remoteAdoptionDetail(t.TempDir())
	if !strings.Contains(detail, "bd dolt remote add origin <url>") {
		t.Fatalf("remote adoption detail without git origin should keep generic command, got:\n%s", detail)
	}
}
