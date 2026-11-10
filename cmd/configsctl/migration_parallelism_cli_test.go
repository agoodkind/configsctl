//go:build unix

package main_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type migrationPlanRequests struct {
	mutex   sync.Mutex
	active  map[string]int
	maximum map[string]int
	count   map[string]int
}

func newMigrationPlanService(t *testing.T) (*httptest.Server, *migrationPlanRequests) {
	t.Helper()
	root := t.TempDir()
	for _, stage := range []string{"original", "cutover"} {
		for _, name := range []string{"first", "second", "third"} {
			writeFixtureFile(t, filepath.Join(root, stage, name), "synthetic native data source content")
		}
	}
	requests := &migrationPlanRequests{mutex: sync.Mutex{}, active: make(map[string]int), maximum: make(map[string]int), count: make(map[string]int)}
	files := http.FileServer(http.Dir(root))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stage := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")[0]
		requests.mutex.Lock()
		requests.active[stage]++
		requests.count[stage]++
		requests.maximum[stage] = max(requests.maximum[stage], requests.active[stage])
		requests.mutex.Unlock()
		defer func() {
			requests.mutex.Lock()
			requests.active[stage]--
			requests.mutex.Unlock()
		}()
		time.Sleep(150 * time.Millisecond)
		files.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func (requests *migrationPlanRequests) reset(t *testing.T) {
	t.Helper()
	requests.mutex.Lock()
	defer requests.mutex.Unlock()
	for _, active := range requests.active {
		if active != 0 {
			t.Fatal("a native plan request remained active after command completion")
		}
	}
	clear(requests.maximum)
	clear(requests.count)
}

func (requests *migrationPlanRequests) require(t *testing.T, stage string, parallelism int) {
	t.Helper()
	requests.mutex.Lock()
	defer requests.mutex.Unlock()
	if requests.count[stage] != 3 {
		t.Fatalf("the %s native plan made %d reads, want 3", stage, requests.count[stage])
	}
	if parallelism == 0 {
		if requests.maximum[stage] < 2 {
			t.Fatalf("the %s native default plan did not perform concurrent independent reads", stage)
		}
	} else if requests.maximum[stage] != parallelism {
		t.Fatalf("the %s native plan made %d simultaneous reads, want %d", stage, requests.maximum[stage], parallelism)
	}
	t.Logf("the %s native plan made 3 reads with a peak of %d simultaneous requests", stage, requests.maximum[stage])
}

func configureMigrationPlanReads(t *testing.T, migration *cliMigration) {
	t.Helper()
	original := originalMigrationTree(*migration)
	configuration := migrationFixtureFile(t, "parallelism.tf")
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "parallelism.tf"), strings.ReplaceAll(configuration, "MIGRATION_PLAN_STAGE", "original"))
	originalInit := tofuArgs("legacy", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, original, originalInit...), originalInit...)
	migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/parallelism.tf", "workspaces/legacy/.terraform.lock.hcl")
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", "parallelism.tf"), strings.ReplaceAll(configuration, "MIGRATION_PLAN_STAGE", "cutover"))
	cutoverInit := tofuArgs("application", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, migration.tree, cutoverInit...), cutoverInit...)
	migration.manifest.Commit = commitMigrationFixture(t, migration.tree, "workspaces/application/parallelism.tf", "workspaces/application/.terraform.lock.hcl")
	migration.writeManifest(t)
}

func runMigrationParallelism(t *testing.T, migration cliMigration, endpoint, action string, parallelism int) commandResult {
	t.Helper()
	temporary, err := os.MkdirTemp("/tmp", "cmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(temporary); err != nil {
			t.Error(err)
		}
	})
	args := []string{"migrate-state", action, migration.file, migration.directory}
	if parallelism != 0 {
		args = append(args, "--parallelism="+strconv.Itoa(parallelism))
	}
	result := runTofuEnvironment(t, migration.tree, "", []string{
		"TF_VAR_probe_endpoint=" + endpoint,
		"TF_CLI_ARGS_plan=-parallelism=1 -invalid-migration-environment-option",
		"TMPDIR=" + temporary,
	}, args...)
	if result.exitCode != 0 {
		logMigrationParallelismReports(t, migration)
	}
	requireSuccess(t, result, args...)
	requireMigrationPrivateOutput(t, result)
	return result
}

func logMigrationParallelismReports(t *testing.T, migration cliMigration) {
	t.Helper()
	for _, pattern := range []string{"baseline-original-*.json", "preflight-*.json", "verify-*.json"} {
		paths, err := filepath.Glob(filepath.Join(migration.directory, "reports", pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			filename := filepath.Base(path)
			if strings.HasSuffix(filename, "-comparison.json") {
				continue
			}
			report := readCLIPlanReport(t, migration, filename)
			t.Logf("native plan report %s has status %s", filename, report.Status)
			for _, diagnostic := range report.Diagnostics {
				t.Logf("native plan report %s has category %s at %s:%d", filename, diagnostic.Category, diagnostic.Filename, diagnostic.Line)
			}
		}
	}
}

func TestMigrationCLIParallelismControlsOriginalAndCutoverNativePlans(t *testing.T) {
	t.Parallel()
	server, requests := newMigrationPlanService(t)
	migration := newCLIMigration(t)
	configureMigrationPlanReads(t, &migration)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	for _, parallelism := range []int{0, 1, 2} {
		requests.reset(t)
		runMigrationParallelism(t, migration, server.URL, "preflight", parallelism)
		requests.require(t, "original", parallelism)
		requests.require(t, "cutover", parallelism)
		requirePreflightUnchanged(t, migration, before)
	}
	for _, action := range []string{"execute", "resume", "verify"} {
		requests.reset(t)
		runMigrationParallelism(t, migration, server.URL, action, 1)
		if action != "verify" {
			requests.require(t, "original", 1)
		}
		requests.require(t, "cutover", 1)
	}
	after := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "parallelism-after"))
	if after.Before.Records[applicationInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("the parallelism option changed the transferred instance identity or provider state")
	}
	requirePreflightCleanup(t, migration)
}

func TestMigrationCLIParallelismControlsRestoredNativePlans(t *testing.T) {
	t.Parallel()
	server, requests := newMigrationPlanService(t)
	migration := newCLIMigration(t)
	configureMigrationPlanReads(t, &migration)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	runMigrationParallelism(t, migration, server.URL, "preflight", 1)
	before := readPreflightSource(t, migration)
	caller := migration
	caller.tree = originalMigrationTree(migration)
	requireSuccess(t, caller.run(t, "restore"), "migrate-state", "restore")
	requests.reset(t)
	runMigrationParallelism(t, caller, server.URL, "verify-restored", 1)
	requests.require(t, "original", 1)
	after := inspectCLIWorkspace(t, caller.tree, "legacy", filepath.Join(migration.private, "parallelism-restored"))
	if after.Before.Records[legacyInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("the restored plan parallelism option changed the original instance identity or provider state")
	}
	requirePreflightUnchanged(t, migration, before)
}

func TestMigrationCLIParallelismRejectsInvalidOptionsBeforeCommands(t *testing.T) {
	t.Parallel()
	for _, options := range [][]string{
		{"--parallelism"},
		{"--parallelism="},
		{"--parallelism=private-invalid-value"},
		{"--parallelism=1.5"},
		{"--parallelism=0"},
		{"--parallelism=-1"},
		{"--parallelism=999999999999999999999999999999999"},
		{"--parallelism=1", "--parallelism=2"},
		{"--unknown=1"},
	} {
		t.Run(strings.Join(options, ","), func(t *testing.T) {
			tree := newConfigsTree(t)
			args := append([]string{"migrate-state", "preflight", "absent-manifest", filepath.Join(t.TempDir(), "private")}, options...)
			result := runConfigsctl(t, tree, args...)
			if result.exitCode == 0 || !strings.Contains(result.stderr, "parallelism") {
				t.Fatal("the public migration command accepted an invalid parallelism option")
			}
			if strings.Contains(result.stderr+result.stdout, "private-invalid-value") || strings.Contains(result.stderr, "migration.command.started") {
				t.Fatal("the invalid option exposed its value or started a migration command")
			}
			requireNoFile(t, filepath.Join(tree.workspaces, "alpha", ".terraform"))
		})
	}
	for _, action := range []string{"inspect", "inspect-plan", "prepare", "restore"} {
		t.Run(action, func(t *testing.T) {
			tree := newConfigsTree(t)
			result := runConfigsctl(t, tree, "migrate-state", action, "absent-manifest", filepath.Join(t.TempDir(), "private"), "--parallelism=1")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "--parallelism requires") || strings.Contains(result.stderr, "migration.command.started") {
				t.Fatal("the irrelevant parallelism option started a migration command")
			}
		})
	}
}
