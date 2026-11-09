//go:build unix

package main_test

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type nativeCredentialServer struct {
	mu          sync.Mutex
	lockPath    string
	deployment  credentialUpdate
	reads       int
	lockMissing bool
}

type credentialUpdate struct {
	Password string `json:"Password"`
}

func (s *nativeCredentialServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.lockPath); err != nil {
		s.lockMissing = true
		writer.WriteHeader(http.StatusConflict)

		return
	}

	user, password, _ := request.BasicAuth()
	rotator := user == "rotator" && password == vaultBetaValue
	deployment := s.deployment.Password != "" && user == "configsctl" &&
		password == s.deployment.Password
	if !rotator && !deployment {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}

	if request.Method == http.MethodPatch {
		if err := json.NewDecoder(request.Body).Decode(&s.deployment); err != nil || !rotator {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		writer.WriteHeader(http.StatusNoContent)

		return
	}

	if strings.HasPrefix(request.URL.Path, "/redfish/v1/AccountService/Accounts/") {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"UserName":"configsctl","Enabled":true}`))

		return
	}

	s.reads++
	writer.WriteHeader(http.StatusNoContent)
}

func installNativeCredential(t *testing.T, fixture explicitFixture, server *httptest.Server) {
	t.Helper()
	installNativeConfiguration(t, fixture.tree)

	for source, destination := range map[string]string{
		"credential-targets.tf": "native/suburban/tack/main.tf",
		"credentials.tf":        "workspaces/suburban/tack/credentials.tf",
	} {
		body, err := os.ReadFile(filepath.Join("testdata", "nativeconfiguration", source))
		if err != nil {
			t.Fatal(err)
		}

		writeFixtureFile(t, filepath.Join(fixture.tree.root, destination), string(body))
	}

	certificate := pem.EncodeToMemory(&pem.Block{
		Type:    "CERTIFICATE",
		Headers: nil,
		Bytes:   server.Certificate().Raw,
	})
	certificatePath := filepath.Join(fixture.tree.root, "controller.pem")
	writeFixtureFile(t, certificatePath, string(certificate))
	settingsPath := filepath.Join(fixture.tree.root, settingsName)
	configuration, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	adapterSettings := strings.ReplaceAll(
		explicitCredentialSettings,
		"https://127.0.0.1:1",
		server.URL,
	)
	adapterSettings = strings.ReplaceAll(
		adapterSettings,
		"rotator_password_key: vault_rotator",
		"rotator_password_key: vault_beta",
	)
	adapterSettings = strings.ReplaceAll(
		adapterSettings,
		"        rotator_user:",
		"        ca_file: "+certificatePath+"\n        rotator_user:",
	)
	writeFixtureFile(t, settingsPath, string(configuration)+adapterSettings)
}

func TestTofuPreparesNativeCredentialUnderItsLock(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	controller := &nativeCredentialServer{
		mu:          sync.Mutex{},
		lockPath:    filepath.Join(fixture.lockDir, "deploy.lock"),
		deployment:  credentialUpdate{Password: ""},
		reads:       0,
		lockMissing: false,
	}
	server := httptest.NewTLSServer(controller)
	defer server.Close()
	installNativeCredential(t, fixture, server)
	result := fixture.apply(t, "-auto-approve")
	requireSuccess(t, result)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.lockMissing || controller.deployment.Password == "" || controller.reads != 2 {
		t.Fatal(
			"the CLI did not retain its native host lock across credential preparation and verification",
		)
	}

	state, err := os.ReadFile(fixture.stateFile())
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(result.stdout+result.stderr+string(state), controller.deployment.Password) {
		t.Fatal("the CLI persisted or printed its generated credential")
	}

	requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
}

func TestTofuAddsThePlannedGuestLockAfterNativeCredentialPreparation(t *testing.T) {
	base, err := os.MkdirTemp(shortTempRoot, "credential-guest-*")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(base) })
	fixture := newFixtureWithTree(t, explicitSettings, false, newConfigsTreeAt(t, base))
	controller := &nativeCredentialServer{
		mu:          sync.Mutex{},
		lockPath:    filepath.Join(fixture.lockDir, "deploy.lock"),
		deployment:  credentialUpdate{Password: ""},
		reads:       0,
		lockMissing: false,
	}
	server := httptest.NewTLSServer(controller)
	defer server.Close()
	installNativeCredential(t, fixture, server)
	installGuestProviderMirror(t, fixture.tree)
	targets, err := os.ReadFile(
		filepath.Join("testdata", "nativeconfiguration", "credential-guest-targets.tf"),
	)
	if err != nil {
		t.Fatal(err)
	}

	writeFixtureFile(
		t,
		filepath.Join(fixture.tree.root, "native/suburban/tack/main.tf"),
		string(targets),
	)
	root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "lock-guest.tf"))
	writeFixtureFile(
		t,
		filepath.Join(root, "item/main.tf"),
		workspaceFixture(t, "lock-guest-item.tf"),
	)
	requireSuccess(t, fixture.mutate(t, "init", "-input=false"))
	result := fixture.apply(t, "-auto-approve")
	if result.exitCode == 0 {
		t.Fatal("the real provider unexpectedly connected to its closed loopback endpoint")
	}

	for _, expected := range []string{
		"hostlock.additional_acquired",
		"subcommand=apply mutation=true",
	} {
		if !strings.Contains(result.stderr, expected) {
			t.Fatalf("the CLI did not add its planned guest lock before apply: %s", result.stderr)
		}
	}

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.lockMissing || controller.reads != 2 {
		t.Fatal("credential preparation did not retain its original host lock")
	}

	requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
}
