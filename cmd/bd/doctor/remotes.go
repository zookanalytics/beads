package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltremote"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/doltutil"
)

var (
	querySQLRemotesForDoctor = querySQLRemotes
	listCLIRemotesForDoctor  = doltutil.ListCLIRemotes
)

// CheckRemoteConsistency compares remotes registered in the SQL server
// vs the filesystem CLI config and reports discrepancies.
// Returns a check with Fix set for cases where --fix can resolve it.
func CheckRemoteConsistency(repoPath string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(repoPath)

	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil || cfg.GetBackend() != configfile.BackendDolt {
		return DoctorCheck{
			Name:     "Remote Consistency",
			Status:   StatusOK,
			Message:  "N/A (not using Dolt backend)",
			Category: CategoryData,
		}
	}

	// Get SQL remotes via direct connection
	sqlRemotes, sqlErr := querySQLRemotesForDoctor(beadsDir)
	if sqlErr != nil {
		return DoctorCheck{
			Name:     "Remote Consistency",
			Status:   StatusWarning,
			Message:  "Could not query SQL remotes (server may not be running)",
			Category: CategoryData,
		}
	}

	// Get CLI remotes
	doltDir := doltserver.ResolveDoltDir(beadsDir)
	dbName := cfg.GetDoltDatabase()
	dbDir := filepath.Join(doltDir, dbName)
	cliRemotes, cliErr := listCLIRemotesForDoctor(dbDir)
	if cliErr != nil {
		return DoctorCheck{
			Name:     "Remote Consistency",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Could not query CLI remotes: %v", cliErr),
			Category: CategoryData,
		}
	}

	// Compare (convert to maps for O(1) lookup)
	sqlMap := doltutil.ToRemoteNameMap(sqlRemotes)
	cliMap := doltutil.ToRemoteNameMap(cliRemotes)

	// Detect remotes stranded at the dolt server root (.beads/dolt/) instead of
	// the database subdir (.beads/dolt/<db>/). They are invisible to both the
	// SQL query and the CLI db-dir query above, so the wrong-directory case
	// would otherwise report "No remotes configured". We surface them as a
	// warning rather than auto-fixing: when the server root hosts multiple
	// databases there is no reliable way to know which one a root-level remote
	// was meant for, so copying it into the currently-configured database could
	// wire this project to the wrong remote. The user resolves it by adding the
	// remote to the intended project explicitly with `bd dolt remote add`.
	stranded := strandedRootRemotes(doltDir, dbDir, cliMap)

	// No remotes anywhere (including the server root)
	if len(sqlRemotes) == 0 && len(cliRemotes) == 0 && len(stranded) == 0 {
		return DoctorCheck{
			Name:     "Remote Consistency",
			Status:   StatusWarning,
			Message:  "No remotes configured",
			Detail:   remoteAdoptionDetail(repoPath),
			Category: CategoryData,
		}
	}

	var issues []string
	hasConflict := false

	// Check all SQL remotes
	for name, sqlURL := range sqlMap {
		cliURL, inCLI := cliMap[name]
		if !inCLI {
			issues = append(issues, fmt.Sprintf("%s: SQL only (%s)", name, sqlURL))
		} else if sqlURL != cliURL {
			issues = append(issues, fmt.Sprintf("%s: CONFLICT — SQL=%s, CLI=%s", name, sqlURL, cliURL))
			hasConflict = true
		}
	}

	// Check CLI-only remotes
	for name, cliURL := range cliMap {
		if _, inSQL := sqlMap[name]; !inSQL {
			issues = append(issues, fmt.Sprintf("%s: CLI only (%s)", name, cliURL))
		}
	}

	// Remotes stranded at the server root are advisory-only: --fix never touches
	// them (the intended database is ambiguous), so the user must act manually.
	var strandedNotes []string
	for _, r := range stranded {
		strandedNotes = append(strandedNotes, fmt.Sprintf(
			"%s: stranded at server root — not auto-fixed; add it to this project with 'bd dolt remote add %s %s'",
			r.Name, r.Name, r.URL))
	}

	if len(issues) == 0 && len(strandedNotes) == 0 {
		msg := fmt.Sprintf("%d remote(s) in sync", len(sqlRemotes))
		// Add refs/dolt/data note for git+ssh remotes
		for _, r := range sqlRemotes {
			if doltutil.IsSSHURL(r.URL) {
				msg += " — git+ssh remotes also support refs/dolt/data (see https://docs.dolthub.com/concepts/dolt/git/remotes)"
				break
			}
		}
		return DoctorCheck{
			Name:     "Remote Consistency",
			Status:   StatusOK,
			Message:  msg,
			Category: CategoryData,
		}
	}

	// Only SQL/CLI discrepancies are auto-fixable; stranded remotes never set Fix.
	fix := ""
	if len(issues) > 0 && !hasConflict {
		fix = "Run 'bd doctor --fix' to sync remotes"
	}

	allNotes := append(issues, strandedNotes...)
	return DoctorCheck{
		Name:     "Remote Consistency",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d discrepancies found", len(allNotes)),
		Detail:   strings.Join(allNotes, "\n"),
		Fix:      fix,
		Category: CategoryData,
	}
}

// strandedRootRemotes returns remotes that live at the dolt server root
// (doltDir) but are missing from the database subdir (dbDir) where the CLI
// push/pull actually looks — the GH#2118 wrong-directory case. The guards
// ensure both the root and the database dir are real dolt repos so we only
// flag a remote the current project genuinely can't see; the caller surfaces
// these as a warning (not an auto-fix), since the intended database is
// ambiguous when the root hosts more than one.
func strandedRootRemotes(doltDir, dbDir string, dbMap map[string]string) []storage.RemoteInfo {
	if doltDir == "" || doltDir == dbDir {
		return nil
	}
	if _, err := os.Stat(filepath.Join(doltDir, ".dolt")); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dbDir, ".dolt")); err != nil {
		return nil
	}
	rootRemotes, err := listCLIRemotesForDoctor(doltDir)
	if err != nil {
		return nil
	}
	var stranded []storage.RemoteInfo
	for _, r := range rootRemotes {
		if _, inDB := dbMap[r.Name]; !inDB {
			stranded = append(stranded, r)
		}
	}
	return stranded
}

func remoteAdoptionDetail(repoPath string) string {
	if originURL := gitOriginRemoteURL(repoPath); originURL != "" {
		remoteURL := doltremote.Normalize(originURL)
		return fmt.Sprintf("git origin is configured. Adopt it with: bd dolt remote add origin %s", remoteURL)
	}
	return "Add a remote with: bd dolt remote add origin <url>"
}

func gitOriginRemoteURL(repoPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// querySQLRemotes gets remotes from the SQL server.
func querySQLRemotes(beadsDir string) ([]storage.RemoteInfo, error) {
	db, _, err := openDoltDB(beadsDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT name, url FROM dolt_remotes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var remotes []storage.RemoteInfo
	for rows.Next() {
		var r storage.RemoteInfo
		if err := rows.Scan(&r.Name, &r.URL); err != nil {
			return nil, err
		}
		remotes = append(remotes, r)
	}
	return remotes, rows.Err()
}
