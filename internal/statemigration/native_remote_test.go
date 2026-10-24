package statemigration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/statemigration"
)

func nativeHTTPStateStore(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(privateFixtureDir(t), "remote.tfstate")
	var mutex sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if request.URL.Path != "/state" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		switch request.Method {
		case http.MethodGet:
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if err != nil {
				writer.WriteHeader(http.StatusInternalServerError)
				t.Errorf("read the local native HTTP backend: %v", err)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			if _, err := writer.Write(data); err != nil {
				t.Errorf("serve the local native HTTP backend: %v", err)
			}
		case http.MethodPost:
			data, err := io.ReadAll(request.Body)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				t.Errorf("read the native HTTP state upload: %v", err)
				return
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				writer.WriteHeader(http.StatusInternalServerError)
				t.Errorf("persist the local native HTTP backend: %v", err)
				return
			}
			writer.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				writer.WriteHeader(http.StatusInternalServerError)
				t.Errorf("delete the local native HTTP backend: %v", err)
				return
			}
			writer.WriteHeader(http.StatusOK)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + "/state", path
}

func nativeHTTPWorkspace(t *testing.T, endpoint string) statemigration.Workspace {
	t.Helper()
	workspace := nativeHTTPConfiguredWorkspace(t, endpoint, "original")
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	return workspace
}

func nativeHTTPConfiguredWorkspace(t *testing.T, endpoint, resource string) statemigration.Workspace {
	t.Helper()
	directory := privateFixtureDir(t)
	configuration, err := os.ReadFile(filepath.Join("testdata", "http.tf"))
	if err != nil {
		t.Fatal(err)
	}
	configuration = bytes.ReplaceAll(configuration, []byte(`"original"`), []byte(`"`+resource+`"`))
	if err := os.WriteFile(filepath.Join(directory, "main.tf"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := statemigration.Workspace{
		Directory: directory,
		Environment: []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + directory,
			"TF_HTTP_ADDRESS=" + endpoint, "CHECKPOINT_DISABLE=1", "TF_INPUT=0", "TF_IN_AUTOMATION=true",
			fmt.Sprintf("TF_VAR_vault_tofu_state_passphrase=%s", nativeEncryptionKey),
			"TF_VAR_fixture_value=" + syntheticMarker,
		},
	}
	nativeCommand(t, workspace, "init", "-input=false", "-no-color")
	return workspace
}

func TestMigrationHandlesInitiallyAbsentRemoteState(t *testing.T) {
	for _, recovery := range []string{"confirmed", "unconfirmed", "changed-confirmed"} {
		t.Run(recovery, func(t *testing.T) {
			engine, before := migrationFixture(t)
			endpoint, remote := nativeHTTPStateStore(t)
			destination := nativeHTTPConfiguredWorkspace(t, endpoint, "renamed")
			engine.Workspaces["application"] = destination
			engine.Manifest.Workspaces[1].Backend = "http:" + endpoint
			if err := engine.Prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(remote); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("preparation wrote the initially absent remote state")
			}
			journalPath := filepath.Join(engine.Directory, "journal.json")
			preparedJournal := readNativeTestBytes(t, journalPath)
			if err := engine.Execute(t.Context()); err != nil {
				t.Fatal(err)
			}
			actual := restoredState(t, engine, "application")
			if actual.Lineage == "" || actual.Serial != 1 || actual.Records["terraform_data.renamed"] != before.Records["terraform_data.original"] {
				t.Fatal("the initial native remote write did not preserve the complete provider record at serial 1")
			}
			if recovery == "unconfirmed" {
				if err := os.WriteFile(journalPath, preparedJournal, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if recovery == "changed-confirmed" {
				nativeCommand(t, destination, "taint", "terraform_data.renamed")
				nativeCommand(t, destination, "untaint", "terraform_data.renamed")
				advanced := restoredState(t, engine, "application")
				if advanced.Serial <= actual.Serial || advanced.Content != actual.Content {
					t.Fatal("the native commands did not restore identical state contents at a later serial")
				}
				remoteBefore := readNativeTestBytes(t, remote)
				if err := engine.Execute(t.Context()); err == nil {
					t.Fatal("migration accepted an external serial change after confirming the first remote write")
				}
				if !bytes.Equal(remoteBefore, readNativeTestBytes(t, remote)) {
					t.Fatal("the refused confirmed-state retry changed the remote backend")
				}
				return
			}
			remoteBefore := readNativeTestBytes(t, remote)
			if err := engine.Execute(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(remoteBefore, readNativeTestBytes(t, remote)) {
				t.Fatal("migration repeated the confirmed or unconfirmed initial remote write")
			}
			if err := engine.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			requireOriginalState(t, engine, before)
			engine.CurrentCommit = engine.Manifest.RestoreCommit
			if err := engine.VerifyRestored(t.Context()); err != nil {
				t.Fatal(err)
			}
			requirePrivateRecoveryFiles(t, engine)
		})
	}
}

func readNativeTestBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeMoveRefusesRemoteBackendWithoutChangingEitherState(t *testing.T) {
	endpoint, remote := nativeHTTPStateStore(t)
	workspace := nativeHTTPWorkspace(t, endpoint)
	private := privateFixtureDir(t)
	source := filepath.Join(private, "source.tfstate")
	before, err := statemigration.Pull(t.Context(), workspace, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Records) != 1 || before.Records["terraform_data.original"] == "" {
		t.Fatal("the real native HTTP backend did not contain the synthetic instance")
	}
	remoteBefore := readNativeTestBytes(t, remote)
	privateBefore := readNativeTestBytes(t, source)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(remoteBefore, &envelope); err != nil || len(envelope["encrypted_data"]) == 0 || bytes.Contains(remoteBefore, []byte(syntheticMarker)) {
		t.Fatal("the native HTTP backend did not persist encrypted state")
	}
	destination := filepath.Join(private, "destination.tfstate")
	err = statemigration.Move(t.Context(), workspace, source, destination, "terraform_data.original", "terraform_data.renamed")
	if err == nil || !strings.Contains(err.Error(), "require an initialized local backend") {
		t.Fatalf("Move did not refuse the initialized remote backend: %v", err)
	}
	if !bytes.Equal(remoteBefore, readNativeTestBytes(t, remote)) || !bytes.Equal(privateBefore, readNativeTestBytes(t, source)) {
		t.Fatal("the refused remote move changed native state bytes")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused remote move created a destination state: %v", err)
	}
	after, err := statemigration.Pull(t.Context(), workspace, filepath.Join(private, "after.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the refused remote move changed native state metadata")
	}
}

func TestNativePreflightOverrideWritesOnlyPrivateLocalState(t *testing.T) {
	endpoint, remote := nativeHTTPStateStore(t)
	workspace := nativeHTTPWorkspace(t, endpoint)
	remoteBefore := readNativeTestBytes(t, remote)
	private := privateFixtureDir(t)
	plaintext := filepath.Join(private, "prepared.tfstate")
	before, err := statemigration.Pull(t.Context(), workspace, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	shadowDirectory := privateFixtureDir(t)
	configuration := readNativeTestBytes(t, filepath.Join(workspace.Directory, "main.tf"))
	if err := os.WriteFile(filepath.Join(shadowDirectory, "main.tf"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	template := readNativeTestBytes(t, "configsctl_migration_override.tf")
	state := filepath.Join(private, "literal-${not_a_variable}-state.tfstate")
	quoted := hclwrite.TokensForValue(cty.StringVal(state)).Bytes()
	override := bytes.ReplaceAll(template, []byte("CONFIGSCTL_MIGRATION_STATE_PATH"), quoted)
	if err := os.WriteFile(filepath.Join(shadowDirectory, "configsctl_migration_override.tf"), override, 0o600); err != nil {
		t.Fatal(err)
	}
	shadow := statemigration.Workspace{Directory: shadowDirectory, Environment: workspace.Environment}
	if err := statemigration.Initialize(t.Context(), shadow); err != nil {
		t.Fatal(err)
	}
	var initialized struct {
		Backend struct {
			Type string `json:"type"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(readNativeTestBytes(t, filepath.Join(shadowDirectory, ".terraform", "terraform.tfstate")), &initialized); err != nil || initialized.Backend.Type != "local" {
		t.Fatal("the production backend override did not select a local backend")
	}
	if err := statemigration.Push(t.Context(), shadow, plaintext); err != nil {
		t.Fatal(err)
	}
	if err := statemigration.CheckPlan(t.Context(), shadow, private); err != nil {
		t.Fatal(err)
	}
	after, err := statemigration.Pull(t.Context(), shadow, filepath.Join(private, "verified.tfstate"))
	if err != nil || before.Digest != after.Digest || before.Content != after.Content {
		t.Fatal("the private local shadow changed the prepared instance contents")
	}
	if !bytes.Equal(remoteBefore, readNativeTestBytes(t, remote)) {
		t.Fatal("the production override changed the original remote backend")
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatal("the native local backend did not use the literal HCL-quoted private path")
	}
}
