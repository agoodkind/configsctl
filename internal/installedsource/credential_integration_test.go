//go:build unix

package installedsource_test

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"goodkind.io/configsctl/internal/dellcredential"
	"goodkind.io/configsctl/internal/installedsource"
)

const rotatorValue = "example-rotator-value"

type sourceCredentialController struct {
	mu         sync.Mutex
	requests   int
	deployment string
}

func (s *sourceCredentialController) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	user, value, _ := request.BasicAuth()
	rotator := user == "rotator" && value == rotatorValue
	deployment := user == "configsctl" && s.deployment != "" && value == s.deployment
	if !rotator && !deployment {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}

	if request.Method == http.MethodPatch {
		var update struct {
			Password string `json:"Password"`
		}
		if err := json.NewDecoder(request.Body).Decode(&update); err != nil || !rotator {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		s.deployment = update.Password
		writer.WriteHeader(http.StatusNoContent)

		return
	}

	if strings.HasPrefix(request.URL.Path, "/redfish/v1/AccountService/Accounts/") {
		account := struct {
			UserName string `json:"UserName"`
			Enabled  bool   `json:"Enabled"`
		}{
			UserName: "configsctl",
			Enabled:  true,
		}
		if err := json.NewEncoder(writer).Encode(account); err != nil {
			http.Error(writer, "encode account response failed", http.StatusInternalServerError)
		}

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

func (s *sourceCredentialController) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.requests
}

func redirectCachedModule(t *testing.T, path, directory string) []byte {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var manifest cachedModules
	if err := json.Unmarshal(original, &manifest); err != nil {
		t.Fatal(err)
	}

	found := false

	for index := range manifest.Modules {
		if manifest.Modules[index].Key == "item" {
			manifest.Modules[index].Dir = directory
			found = true
		}
	}

	if !found {
		t.Fatal("the initialized manifest did not contain the selected module")
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	return original
}

func TestRedirectedModulesStopCredentialRequestsForPlanAndApply(t *testing.T) {
	for _, command := range []string{"plan", "apply"} {
		t.Run(
			command,
			func(t *testing.T) {
				f := newFixture(t)
				state := filepath.Join(t.TempDir(), "state.tfstate")
				f.replaceFile(
					t,
					"workspace/main.tf",
					fmt.Sprintf(reviewSource(t, "local-modules.tf"), state),
				)
				f.replaceFile(t, "workspace/item/main.tf", reviewSource(t, "local-module.tf"))
				declarations, err := os.ReadFile(
					filepath.Join(
						"..",
						"..",
						"cmd",
						"configsctl",
						"testdata",
						"nativeconfiguration",
						"credentials.tf",
					),
				)
				if err != nil {
					t.Fatal(err)
				}

				f.addFile(t, "workspace/credentials.tf", string(declarations))
				controller := &sourceCredentialController{
					mu:         sync.Mutex{},
					requests:   0,
					deployment: "",
				}
				server := httptest.NewTLSServer(controller)
				defer server.Close()
				certificate := pem.EncodeToMemory(&pem.Block{
					Type:    "CERTIFICATE",
					Headers: nil,
					Bytes:   server.Certificate().Raw,
				})
				f.addFile(t, "controller.pem", string(certificate))
				source, err := f.verify(t)
				if err != nil {
					t.Fatal(err)
				}

				if err := source.CheckModuleSources("workspace", "", ""); err != nil {
					t.Fatal(err)
				}

				copyRoot := t.TempDir()
				if err := source.Copy(copyRoot, nil); err != nil {
					t.Fatal(err)
				}

				root := filepath.Join(copyRoot, "workspace")
				data := t.TempDir()
				tofuAt(t, root, data, nil, "init", "-input=false")
				if command == "apply" {
					environment := []string{
						"TF_VAR_deployment_user=example-operator",
						"TF_VAR_deployment_password=example-ephemeral-value",
					}
					tofuAt(
						t,
						root,
						data,
						environment,
						"plan",
						"-input=false",
						"-out="+filepath.Join(t.TempDir(), "saved.tfplan"),
					)
				}

				outside := t.TempDir()
				if err := os.WriteFile(
					filepath.Join(outside, "main.tf"),
					[]byte(reviewSource(t, "unlisted-module.tf")),
					0o600,
				); err != nil {
					t.Fatal(err)
				}

				manifestPath := filepath.Join(data, "modules", "modules.json")
				original := redirectCachedModule(t, manifestPath, outside)
				request := installedsource.ModuleCacheRequest{
					Binary:            tofuExecutable(t),
					Workspace:         "workspace",
					CopyRoot:          copyRoot,
					DataDirectory:     data,
					WorkspaceSelector: "",
					AllowMissing:      false,
				}
				adapter := dellcredential.Config{
					Endpoint:           server.URL,
					CAFile:             filepath.Join(f.settings.Root, "controller.pem"),
					ServerName:         "",
					RotatorUser:        "rotator",
					RotatorPasswordKey: "fixture",
					AccountID:          "3",
					Username:           "configsctl",
					ReadPaths:          []string{"/redfish/v1/Systems/System.Embedded.1"},
				}
				_, err = source.PrepareCredential(t.Context(), request, adapter, rotatorValue)
				if err == nil ||
					!strings.Contains(
						err.Error(),
						"cached module resolutions do not match verified source",
					) {
					t.Fatalf(
						"%s did not refuse the redirected manifest before credential preparation: %v",
						command,
						err,
					)
				}

				if controller.requestCount() != 0 {
					t.Fatalf(
						"%s contacted the credential controller before module validation",
						command,
					)
				}

				if err := os.WriteFile(manifestPath, original, 0o600); err != nil {
					t.Fatal(err)
				}

				if _, err := source.PrepareCredential(
					t.Context(),
					request,
					adapter,
					rotatorValue,
				); err != nil {
					t.Fatalf(
						"the real TLS credential control did not succeed with verified modules: %v",
						err,
					)
				}

				if controller.requestCount() != 4 {
					t.Fatal("the TLS control did not execute the real credential protocol")
				}
			},
		)
	}
}
