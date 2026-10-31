package tofurun_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/tofurun"
)

const (
	passphraseVariable = "TF_VAR_state_passphrase"
	passphraseValue    = "synthetic-state-passphrase-0001"
	tokenVariable      = "TF_VAR_token_file"
	tokenValue         = "synthetic-token-file-contents-0002"
	pidVariable        = "TF_VAR_pid_file"
	testWorkspace      = "qa"
	lockedHost         = "gateway"
	otherHost          = "hypervisor"
	firstLabel         = "first"
	secondLabel        = "second-changed"
	waitLimit          = 60 * time.Second
	pollInterval       = 5 * time.Millisecond
	largePlanSize      = 256 << 20
	lockRun            = "20261030T000000Z-tofurun1"
)

const (
	encryptedRoot    = "testdata/encrypted-root/main.tf"
	waitingRoot      = "testdata/waiting-root/main.tf"
	waitScript       = "testdata/wait-provisioner.sh"
	waitVariable     = "TF_VAR_wait_script"
	brokenRoot       = "testdata/broken-root/main.tf"
	remoteModuleRoot = "testdata/remote-module-root.tf.tmpl"
	remoteModule     = "testdata/remote-module/main.tf"
)

const (
	initialLock = "# initial provider lock file\n"
	changedLock = "# changed provider lock file\n"
)

// fixture is one OpenTofu root with private HOME, TMPDIR, and credential
// directories.
type fixture struct {
	binary     string
	root       string
	tmpdir     string
	home       string
	token      string
	pidFile    string
	waitScript string
}

// newFixture copies the module file to main.tf in a new root. An empty module
// path leaves the root without main.tf.
func newFixture(t *testing.T, module string) fixture {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	binary, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("find tofu: %v", err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatalf("resolve tofu: %v", err)
	}
	built := fixture{
		binary: binary, root: t.TempDir(), tmpdir: privateDirectory(t), home: privateDirectory(t),
		token: filepath.Join(privateDirectory(t), "token"), pidFile: "", waitScript: "",
	}
	built.pidFile = filepath.Join(built.tmpdir, "sleep.pid")
	built.waitScript = filepath.Join(privateDirectory(t), filepath.Base(waitScript))
	if module != "" {
		writeFile(t, filepath.Join(built.root, "main.tf"), readTestdata(t, module))
	}
	writeFile(t, built.token, tokenValue)
	if err := os.WriteFile(built.waitScript, []byte(readTestdata(t, waitScript)), 0o700); err != nil {
		t.Fatalf("the test cannot write the local-exec script: %v", err)
	}
	return built
}

func readTestdata(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the test cannot read %s: %v", path, err)
	}
	return string(body)
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restrict %s: %v", directory, err)
	}
	return directory
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f fixture) environment() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"TMPDIR=" + f.tmpdir,
		"TF_IN_AUTOMATION=1",
		passphraseVariable + "=" + passphraseValue,
		tokenVariable + "=" + f.token,
		pidVariable + "=" + f.pidFile,
		"GITHUB_TOKEN=synthetic-github-token-0003",
		waitVariable + "=" + f.waitScript,
	}
}

func (f fixture) request(args ...string) tofurun.Request {
	return tofurun.Request{
		Binary: f.binary, Root: f.root, Workspace: testWorkspace,
		Args: args, Environment: f.environment(),
		RequiredEnvironment: []string{pidVariable}, SecretEnvironment: []string{passphraseVariable},
		SecretFiles: map[string]string{tokenVariable: f.token},
		OutputMode:  tofurun.RedactedProgress, JSONPath: "", RequiredHosts: []string{lockedHost},
	}
}

func run(t *testing.T, request tofurun.Request, scope *hostlock.Scope) tofurun.Result {
	t.Helper()
	result, err := tofurun.Run(t.Context(), request, scope)
	if err != nil {
		t.Fatalf("Run %v: %v", request.Args, err)
	}
	return result
}

func requireExit(t *testing.T, result tofurun.Result, want int, args []string) {
	t.Helper()
	if result.ExitCode != want {
		t.Fatalf("Run %v exited with %d, want %d; log %s", args, result.ExitCode, want, result.LogPath)
	}
}

// tofu runs a setup command that Run does not accept, such as workspace new.
func (f fixture) tofu(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), f.binary, args...)
	command.Dir = f.root
	command.Env = f.environment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("tofu %v: %v\n%s", args, err, stderr.String())
	}
	return stdout.String()
}

// initialize initializes the root, creates the test workspace, and uses
// Run to select it.
func (f fixture) initialize(t *testing.T) {
	t.Helper()
	initArgs := []string{"init", "-input=false", "-lockfile=readonly", "-no-color"}
	requireExit(t, run(t, f.request(initArgs...), nil), 0, initArgs)
	f.tofu(t, "workspace", "new", "-no-color", testWorkspace)
	f.tofu(t, "workspace", "select", "-no-color", "default")
	selectArgs := []string{"workspace", "select", "-no-color", testWorkspace}
	requireExit(t, run(t, f.request(selectArgs...), nil), 0, selectArgs)
}

func (f fixture) plan(t *testing.T, label string) string {
	t.Helper()
	path := filepath.Join(f.tmpdir, label+".tfplan")
	variables := filepath.Join(f.tmpdir, label+".tfvars")
	writeFile(t, variables, "label = \""+label+"\"\n")
	args := planArgs(path, "-var-file="+variables)
	requireExit(t, run(t, f.request(args...), nil), 2, args)
	return path
}

// planArgs returns the plan arguments that Run requires, followed by extra.
func planArgs(out string, extra ...string) []string {
	args := []string{"plan", "-input=false", "-no-color", "-detailed-exitcode", "-parallelism=1", "-out=" + out}
	return append(args, extra...)
}

// applyArgs returns the apply arguments that Run requires for planPath.
func applyArgs(planPath string) []string {
	return []string{"apply", "-input=false", "-no-color", "-parallelism=1", planPath}
}

func acquire(t *testing.T, lease hostlock.Lease, hosts ...string) *hostlock.Set {
	t.Helper()
	targets := make([]hostlock.Host, 0, len(hosts))
	for _, name := range hosts {
		targets = append(targets, hostlock.Host{Name: name, User: "", Address: "", Dir: t.TempDir()})
	}
	set, err := hostlock.AcquireAll(t.Context(), targets, lockRun, "controller-a", lease)
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	t.Cleanup(func() { set.ReleaseAll(context.WithoutCancel(t.Context())) })
	return set
}

func scopeOf(t *testing.T, set *hostlock.Set) *hostlock.Scope {
	t.Helper()
	scope, err := set.Scope()
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	return scope
}

func readLog(t *testing.T, result tofurun.Result) string {
	t.Helper()
	body, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	return string(body)
}

// requireNoState checks that the test workspace does not have a state file.
func requireNoState(t *testing.T, f fixture) {
	t.Helper()
	path := filepath.Join(f.root, "terraform.tfstate.d", testWorkspace, "terraform.tfstate")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused apply wrote the state file %s: %v", path, err)
	}
}

func TestRunRedactsSecretValuesAndSecretFileContents(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	args := planArgs(filepath.Join(f.tmpdir, "plan"))
	result := run(t, f.request(args...), nil)
	requireExit(t, result, 2, args)
	logText := readLog(t, result)
	for _, secret := range []string{passphraseValue, tokenValue} {
		if strings.Contains(logText, secret) {
			t.Fatalf("the run log contains a secret value:\n%s", logText)
		}
	}
	for _, label := range []string{passphraseVariable, tokenVariable} {
		if !strings.Contains(logText, "<redacted:"+label+">") {
			t.Fatalf("the run log lacks the redaction marker for %s:\n%s", label, logText)
		}
	}
}

func TestRunWritesPrivateJSONWithTheUnchangedShowOutput(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	planPath := f.plan(t, firstLabel)
	jsonDirectory := filepath.Join(f.tmpdir, "report")
	if err := os.Mkdir(jsonDirectory, 0o700); err != nil {
		t.Fatalf("create the JSON directory: %v", err)
	}
	request := f.request("show", "-json", planPath)
	request.OutputMode = tofurun.PrivateJSON
	request.JSONPath = filepath.Join(jsonDirectory, "plan.json")
	requireExit(t, run(t, request, nil), 0, request.Args)

	info, err := os.Lstat(request.JSONPath)
	if err != nil {
		t.Fatalf("stat the JSON file: %v", err)
	}
	if mode := info.Mode(); !mode.IsRegular() || mode.Perm() != 0o600 {
		t.Fatalf("the JSON file has mode %v, want a regular file with mode 0600", mode)
	}
	written, err := os.ReadFile(request.JSONPath)
	if err != nil {
		t.Fatalf("read the JSON file: %v", err)
	}
	direct := f.tofu(t, "show", "-json", planPath)
	if !bytes.Equal(written, []byte(direct)) {
		t.Fatalf("the JSON file differs from the output of tofu show -json")
	}
	if _, err := tofurun.Run(t.Context(), request, nil); !errors.Is(err, tofurun.ErrRejected) {
		t.Fatalf("Run with an existing JSONPath: err = %v, want ErrRejected", err)
	}
}

func TestRunAppliesTheSavedPlanCopiedBeforeTheOriginalChanged(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	firstPlan := f.plan(t, firstLabel)
	secondPlan := f.plan(t, secondLabel)
	firstBytes, err := os.ReadFile(firstPlan)
	if err != nil {
		t.Fatalf("read the first plan: %v", err)
	}
	secondBytes, err := os.ReadFile(secondPlan)
	if err != nil {
		t.Fatalf("read the second plan: %v", err)
	}
	set := acquire(t, hostlock.DefaultLease(), lockedHost)
	replaced := make(chan error, 1)
	go replaceAfterCopy(f.tmpdir, firstPlan, firstBytes, secondBytes, replaced)

	args := applyArgs(firstPlan)
	requireExit(t, run(t, f.request(args...), scopeOf(t, set)), 0, args)
	if err := <-replaced; err != nil {
		t.Fatalf("replace the original plan after the copy: %v", err)
	}
	if label := strings.TrimSpace(f.tofu(t, "output", "-raw", "label")); label != firstLabel {
		t.Fatalf("the applied label is %q, want %q from the copied plan", label, firstLabel)
	}
	copies, err := filepath.Glob(filepath.Join(f.tmpdir, "tofurun-plan-*"))
	if err != nil || len(copies) != 0 {
		t.Fatalf("Run left private plan copies %v (err %v)", copies, err)
	}
}

// replaceAfterCopy waits until Run has copied the original plan into a
// tofurun-plan directory and then replaces the original with other bytes.
func replaceAfterCopy(tmpdir, original string, contents, replacement []byte, done chan<- error) {
	deadline := time.Now().Add(waitLimit)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		copies, err := filepath.Glob(filepath.Join(tmpdir, "tofurun-plan-*", filepath.Base(original)))
		if err != nil {
			done <- err
			return
		}
		if len(copies) == 1 {
			if copied, readErr := os.ReadFile(copies[0]); readErr == nil && bytes.Equal(copied, contents) {
				staged := original + ".new"
				if err := os.WriteFile(staged, replacement, 0o600); err != nil {
					done <- err
					return
				}
				done <- os.Rename(staged, original)
				return
			}
		}
		<-ticker.C
	}
	done <- errors.New("the private plan copy did not appear")
}

func TestRunReportsOpenTofuExitStatuses(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	validateArgs := []string{"validate", "-no-color"}
	requireExit(t, run(t, f.request(validateArgs...), nil), 0, validateArgs)
	f.plan(t, firstLabel)

	missing := f.request("workspace", "select", "-no-color", "missing")
	missing.Workspace = "missing"
	requireExit(t, run(t, missing, nil), 1, missing.Args)

	broken := newFixture(t, brokenRoot)
	brokenArgs := []string{"validate", "-no-color"}
	requireExit(t, run(t, broken.request(brokenArgs...), nil), 1, brokenArgs)
}

func TestRunStopsTheProcessGroupWhenTheContextEnds(t *testing.T) {
	f := newFixture(t, waitingRoot)
	request := f.request()
	request.SecretEnvironment, request.SecretFiles = nil, nil
	request.RequiredEnvironment = append(request.RequiredEnvironment, waitVariable)
	initArgs := []string{"init", "-input=false", "-lockfile=readonly", "-no-color"}
	request.Args = initArgs
	requireExit(t, run(t, request, nil), 0, initArgs)
	f.tofu(t, "workspace", "new", "-no-color", testWorkspace)
	planPath := filepath.Join(f.tmpdir, "wait.tfplan")
	request.Args = planArgs(planPath)
	requireExit(t, run(t, request, nil), 2, request.Args)

	set := acquire(t, hostlock.DefaultLease(), lockedHost)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request.Args = applyArgs(planPath)
	scope := scopeOf(t, set)
	finished := make(chan error, 1)
	go func() {
		_, err := tofurun.Run(ctx, request, scope)
		finished <- err
	}()
	pid := waitForPID(t, f.pidFile)
	cancel()
	if err := <-finished; !errors.Is(err, tofurun.ErrIncomplete) {
		t.Fatalf("Run after cancellation: err = %v, want ErrIncomplete", err)
	}
	waitForExit(t, pid)
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(body), "\n") {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr != nil {
				t.Fatalf("read the process id in %s: %v", path, parseErr)
			}
			return pid
		}
		<-ticker.C
	}
	t.Fatalf("the provisioner did not write %s", path)
	return 0
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		<-ticker.C
	}
	t.Fatalf("process %d of the OpenTofu process group still runs after Run returned", pid)
}

func TestRunRefusesMutationWithoutAValidScope(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	planPath := f.plan(t, firstLabel)
	args := applyArgs(planPath)

	var unacquired hostlock.Set
	if _, err := unacquired.Scope(); !errors.Is(err, hostlock.ErrScope) {
		t.Fatalf("Scope of a Set without AcquireAll: err = %v, want ErrScope", err)
	}
	if err := scopeOf(t, acquire(t, hostlock.DefaultLease())).Check(nil); err != nil {
		t.Fatalf("Check of a scope from an acquired Set without hosts: %v", err)
	}

	released := acquire(t, hostlock.DefaultLease(), lockedHost)
	releasedScope := scopeOf(t, released)
	released.ReleaseAll(t.Context())

	expiring := acquire(t, hostlock.Lease{TTL: 3 * time.Second, RenewInterval: time.Second}, lockedHost)
	expiredScope := scopeOf(t, expiring)

	renewed := acquire(t, hostlock.DefaultLease(), lockedHost)
	renewCtx, endRenewal := context.WithCancel(t.Context())
	stop := renewed.KeepRenewed(renewCtx, func(error) {})
	canceledScope := scopeOf(t, renewed)
	endRenewal()
	stop()

	partial := acquire(t, hostlock.DefaultLease(), lockedHost)
	partialScope := scopeOf(t, partial)
	partialRequest := f.request(args...)
	partialRequest.RequiredHosts = []string{lockedHost, otherHost}

	stateRequest := f.request(args...)
	stateRequest.Environment = append(stateRequest.Environment, hostlock.StateEnvVar+"="+partial.StateDir())

	waitUntilExpired(t, expiring)
	cases := []struct {
		name    string
		request tofurun.Request
		scope   *hostlock.Scope
		want    error
	}{
		{name: "missing", request: f.request(args...), scope: nil, want: hostlock.ErrScope},
		{name: "zero value", request: f.request(args...), scope: &hostlock.Scope{}, want: hostlock.ErrScope},
		{name: "released", request: f.request(args...), scope: releasedScope, want: hostlock.ErrScope},
		{name: "expired", request: f.request(args...), scope: expiredScope, want: hostlock.ErrScope},
		{name: "canceled", request: f.request(args...), scope: canceledScope, want: hostlock.ErrScope},
		{name: "incomplete target", request: partialRequest, scope: partialScope, want: hostlock.ErrScope},
		{name: "lock state variable", request: stateRequest, scope: nil, want: hostlock.ErrScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tofurun.Run(t.Context(), tc.request, tc.scope)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run apply: err = %v, want %v", err, tc.want)
			}
			if result.LogPath != "" {
				t.Fatalf("Run apply opened the run log %s before refusing", result.LogPath)
			}
		})
	}
	requireNoState(t, f)
}

// TestRunChecksTheScopeAgainBeforeStartingOpenTofu releases the Set when Run
// creates its private plan directory for a 256 MiB saved plan and expects ErrScope.
func TestRunChecksTheScopeAgainBeforeStartingOpenTofu(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	largePlan := filepath.Join(f.tmpdir, "large.tfplan")
	writeFile(t, largePlan, "large")
	if err := os.Truncate(largePlan, largePlanSize); err != nil {
		t.Fatalf("extend the large plan file: %v", err)
	}
	set := acquire(t, hostlock.DefaultLease(), lockedHost)
	scope := scopeOf(t, set)
	released := make(chan error, 1)
	go releaseDuringCopy(f.tmpdir, set, released)

	_, err := tofurun.Run(t.Context(), f.request(applyArgs(largePlan)...), scope)
	if releaseErr := <-released; releaseErr != nil {
		t.Fatalf("release the Set during the copy: %v", releaseErr)
	}
	if !errors.Is(err, hostlock.ErrScope) {
		t.Fatalf("Run apply after release during preparation: err = %v, want ErrScope", err)
	}
	requireNoState(t, f)
}

// releaseDuringCopy releases set when Run creates its private plan directory.
func releaseDuringCopy(tmpdir string, set *hostlock.Set, done chan<- error) {
	deadline := time.Now().Add(waitLimit)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		copies, err := filepath.Glob(filepath.Join(tmpdir, "tofurun-plan-*"))
		if err != nil {
			done <- err
			return
		}
		if len(copies) == 1 {
			set.ReleaseAll(context.Background())
			done <- nil
			return
		}
		<-ticker.C
	}
	done <- errors.New("the private plan directory did not appear")
}

func waitUntilExpired(t *testing.T, set *hostlock.Set) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		if _, err := set.Scope(); errors.Is(err, hostlock.ErrScope) {
			return
		}
		<-ticker.C
	}
	t.Fatal("the short lease did not pass its loss deadline")
}

func TestRunRefusesUnsafeRequestsBeforeStartingOpenTofu(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	f.initialize(t)
	planPath := filepath.Join(f.tmpdir, "plan")
	validPlan := planArgs(planPath)
	set := acquire(t, hostlock.DefaultLease(), lockedHost)
	scope := scopeOf(t, set)
	publicDirectory := t.TempDir()
	if err := os.Chmod(publicDirectory, 0o755); err != nil {
		t.Fatalf("open the directory: %v", err)
	}
	publicToken := filepath.Join(publicDirectory, "token")
	writeFile(t, publicToken, tokenValue)

	modify := func(change func(*tofurun.Request), args ...string) tofurun.Request {
		request := f.request(args...)
		change(&request)
		return request
	}
	keep := func(*tofurun.Request) {}
	privateJSON := func(path string) func(*tofurun.Request) {
		return func(r *tofurun.Request) {
			r.OutputMode = tofurun.PrivateJSON
			r.JSONPath = path
		}
	}
	outside := privateDirectory(t)
	if err := os.Mkdir(filepath.Join(outside, "report"), 0o700); err != nil {
		t.Fatalf("create the outside report directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(f.tmpdir, "link")); err != nil {
		t.Fatalf("create the link to the outside directory: %v", err)
	}
	linkedJSON := filepath.Join(f.tmpdir, "link", "report", "plan.json")
	existingPlan := filepath.Join(f.tmpdir, "existing.tfplan")
	writeFile(t, existingPlan, "existing")
	savedPlan := f.plan(t, firstLabel)
	cases := []struct {
		name    string
		request tofurun.Request
		scope   *hostlock.Scope
	}{
		{name: "excluded variable", request: modify(func(r *tofurun.Request) {
			r.RequiredEnvironment = append(r.RequiredEnvironment, "GITHUB_TOKEN")
		}, validPlan...)},
		{name: "duplicate assignment", request: modify(func(r *tofurun.Request) {
			r.Environment = append(r.Environment, passphraseVariable+"="+passphraseValue)
		}, validPlan...)},
		{name: "missing secret", request: modify(func(r *tofurun.Request) {
			r.SecretEnvironment = append(r.SecretEnvironment, "TF_VAR_absent")
		}, validPlan...)},
		{name: "unmaskable secret", request: modify(func(r *tofurun.Request) {
			r.Environment = append(r.Environment, "TF_VAR_short=short")
			r.SecretEnvironment = append(r.SecretEnvironment, "TF_VAR_short")
		}, validPlan...)},
		{name: "secret argument", request: modify(keep, planArgs(planPath, "-var-file="+filepath.Join(f.tmpdir, passphraseValue))...)},
		{name: "public secret file", request: modify(func(r *tofurun.Request) {
			r.SecretFiles = map[string]string{tokenVariable: publicToken}
			r.Environment = append(r.Environment[:5:5], tokenVariable+"="+publicToken, pidVariable+"="+f.pidFile)
		}, validPlan...)},
		{name: "unbound secret file", request: modify(func(r *tofurun.Request) {
			r.SecretFiles = map[string]string{tokenVariable: f.token + ".other"}
		}, validPlan...)},
		{name: "missing automation setting", request: modify(func(r *tofurun.Request) {
			r.Environment = slicesWithout(r.Environment, "TF_IN_AUTOMATION=1")
		}, validPlan...)},
		{name: "workspace creation", request: modify(keep, "workspace", "select", "-or-create", testWorkspace)},
		{name: "workspace new", request: modify(keep, "workspace", "new", testWorkspace)},
		{name: "writable provider lock", request: modify(keep, "init", "-input=false")},
		{name: "unlocked deployment plan", request: modify(keep, planArgs(planPath, "-lock=false")...), scope: scope},
		{name: "plan without no-color", request: modify(keep, slicesWithout(validPlan, "-no-color")...)},
		{name: "plan without detailed exit code", request: modify(keep, slicesWithout(validPlan, "-detailed-exitcode")...)},
		{name: "plan without parallelism", request: modify(keep, slicesWithout(validPlan, "-parallelism=1")...)},
		{name: "plan with zero parallelism", request: modify(keep, planArgs(planPath, "-parallelism=0")...)},
		{name: "plan without output", request: modify(keep, slicesWithout(validPlan, "-out="+planPath)...)},
		{name: "plan output outside TMPDIR", request: modify(keep, planArgs(filepath.Join(f.root, "plan"))...)},
		{name: "plan output through a link", request: modify(keep, planArgs(filepath.Join(f.tmpdir, "link", "plan"))...)},
		{name: "existing plan output", request: modify(keep, planArgs(existingPlan)...)},
		{name: "plan with a variable argument", request: modify(keep, planArgs(planPath, "-var=label=other")...)},
		{name: "unlocked apply", request: modify(keep, "apply", "-input=false", "-parallelism=1", "-lock=false", savedPlan), scope: scope},
		{name: "apply without a saved plan", request: modify(keep, "apply", "-input=false", "-parallelism=1"), scope: scope},
		{name: "apply without parallelism", request: modify(keep, "apply", "-input=false", savedPlan), scope: scope},
		{name: "apply with negative parallelism", request: modify(keep, "apply", "-input=false", "-parallelism=-1", savedPlan), scope: scope},
		{name: "apply with auto-approve", request: modify(keep, "apply", "-input=false", "-parallelism=1", "-auto-approve", savedPlan), scope: scope},
		{name: "destroy", request: modify(keep, "destroy", "-auto-approve"), scope: scope},
		{name: "state removal", request: modify(keep, "state", "rm", "terraform_data.item"), scope: scope},
		{name: "private JSON plan", request: modify(privateJSON(filepath.Join(f.tmpdir, "plan.json")), validPlan...)},
		{name: "private JSON with json false", request: modify(privateJSON(filepath.Join(f.tmpdir, "plan.json")), "show", "-json=false", savedPlan)},
		{name: "private JSON through a link", request: modify(privateJSON(linkedJSON), "show", "-json", savedPlan)},
		{name: "show JSON to progress", request: modify(keep, "show", "-json", savedPlan)},
	}
	refusedPlans := []string{planPath}
	for position, spelling := range []string{"0", "f", "F", "FALSE", "False", "false"} {
		spellingPlan := filepath.Join(f.tmpdir, "plan-lock-"+strconv.Itoa(position))
		refusedPlans = append(refusedPlans, spellingPlan)
		cases = append(cases, struct {
			name    string
			request tofurun.Request
			scope   *hostlock.Scope
		}{name: "scoped plan with lock " + spelling, request: modify(keep, planArgs(spellingPlan, "-lock="+spelling)...), scope: scope})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tofurun.Run(t.Context(), tc.request, tc.scope)
			if !errors.Is(err, tofurun.ErrRejected) {
				t.Fatalf("Run %v: err = %v, want ErrRejected", tc.request.Args, err)
			}
			if result.LogPath != "" {
				t.Fatalf("Run %v opened the run log %s before refusing", tc.request.Args, result.LogPath)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(outside, "report", "plan.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check that the JSON file outside TMPDIR does not exist: %v", err)
	}
	for _, path := range refusedPlans {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("check that the refused plan %s does not exist: %v", path, err)
		}
	}
	requireNoState(t, f)
}

// TestRunRejectionDoesNotDiscloseDeclaredSecrets places declared secret values
// in the subcommand and flag-name positions that Run checks before it
// registers the secrets.
func TestRunRejectionDoesNotDiscloseDeclaredSecrets(t *testing.T) {
	f := newFixture(t, encryptedRoot)
	var logs bytes.Buffer
	previous, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	requests := [][]string{
		{passphraseValue},
		{tokenValue, "-input=false"},
		planArgs(filepath.Join(f.tmpdir, "plan"), "-"+passphraseValue),
		planArgs(filepath.Join(f.tmpdir, "plan"), "--"+tokenValue+"=1"),
		{"validate", "-" + passphraseValue + "=true"},
	}
	for _, args := range requests {
		_, err := tofurun.Run(t.Context(), f.request(args...), nil)
		if !errors.Is(err, tofurun.ErrRejected) {
			t.Fatalf("Run with a secret argument at a checked position: err = %v, want ErrRejected", err)
		}
		for _, secret := range []string{passphraseValue, tokenValue} {
			if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
				t.Fatal("the rejection error or log contains a declared secret value")
			}
		}
	}
}

// TestRunReportsAProviderLockChangedDuringInit serves a module archive from a
// local HTTP server. In the changed case, the server rewrites the provider lock
// file before returning the archive.
func TestRunReportsAProviderLockChangedDuringInit(t *testing.T) {
	initArgs := []string{"init", "-input=false", "-lockfile=readonly", "-no-color"}
	for _, change := range []bool{false, true} {
		archive := moduleArchive(t)
		f := newFixture(t, "")
		lockFile := filepath.Join(f.root, ".terraform.lock.hcl")
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if change {
				if err := os.WriteFile(lockFile, []byte(changedLock), 0o600); err != nil {
					http.Error(writer, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			_, _ = writer.Write(archive)
		}))
		t.Cleanup(server.Close)
		writeFile(t, filepath.Join(f.root, "main.tf"), renderRemoteModuleRoot(t, server.URL))
		writeFile(t, lockFile, initialLock)
		result, err := tofurun.Run(t.Context(), f.request(initArgs...), nil)
		if result.ExitCode != 0 {
			t.Fatalf("OpenTofu init exited with %d; log %s", result.ExitCode, result.LogPath)
		}
		if change && !errors.Is(err, tofurun.ErrProviderLockChanged) {
			t.Fatalf("Run init with a changed lock file: err = %v, want ErrProviderLockChanged", err)
		}
		if !change && err != nil {
			t.Fatalf("Run init with an unchanged lock file: %v", err)
		}
	}
}

// renderRemoteModuleRoot fills the root module template with the module
// server URL.
func renderRemoteModuleRoot(t *testing.T, serverURL string) string {
	t.Helper()
	parsed, err := template.ParseFiles(remoteModuleRoot)
	if err != nil {
		t.Fatalf("the test cannot parse %s: %v", remoteModuleRoot, err)
	}
	var rendered strings.Builder
	if err := parsed.Execute(&rendered, struct{ ServerURL string }{ServerURL: serverURL}); err != nil {
		t.Fatalf("the test cannot render %s: %v", remoteModuleRoot, err)
	}
	return rendered.String()
}

// moduleArchive returns a zip archive with one OpenTofu module file.
func moduleArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	module, err := os.ReadFile(remoteModule)
	if err != nil {
		t.Fatalf("the test cannot read %s: %v", remoteModule, err)
	}
	file, err := writer.Create("main.tf")
	if err != nil {
		t.Fatalf("create the module archive entry: %v", err)
	}
	if _, err := file.Write(module); err != nil {
		t.Fatalf("write the module archive entry: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close the module archive: %v", err)
	}
	return archive.Bytes()
}

func slicesWithout(values []string, removed string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			kept = append(kept, value)
		}
	}
	return kept
}
