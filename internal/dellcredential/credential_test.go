package dellcredential_test

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/dellcredential"
)

func generationConfig(t *testing.T) dellcredential.Config {
	t.Helper()
	return dellcredential.Config{
		Endpoint: "https://127.0.0.1:1", CAFile: filepath.Join(t.TempDir(), "absent.pem"), ServerName: "synthetic-controller.invalid",
		RotatorUser: "synthetic-rotator", RotatorPasswordKey: "synthetic_rotator_key", AccountID: "3",
		Username: "synthetic-deployment", ReadPaths: []string{"/redfish/v1/Systems/System.Embedded.1"},
	}
}

func TestGenerateCredentialPreservesPasswordPropertiesWithoutActivation(t *testing.T) {
	configuration := generationConfig(t)
	credential, err := configuration.GenerateCredential()
	if err != nil {
		t.Fatalf("credential generation required controller access or a certificate file: %v", err)
	}
	if credential.Username != configuration.Username || len(credential.Password) != 36 {
		t.Fatal("generation changed the configured username or password length")
	}
	for index, alphabet := range []string{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz", "0123456789", "!@#$%"} {
		if !strings.ContainsRune(alphabet, rune(credential.Password[index])) {
			t.Fatal("generation omitted a required password character class")
		}
	}
	random, err := base64.RawURLEncoding.DecodeString(credential.Password[4:])
	if err != nil || len(random) != 24 {
		t.Fatal("generation changed the 24-byte random password suffix")
	}
	next, err := configuration.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if next.Password == credential.Password {
		t.Fatal("independent generation reused the deployment password")
	}
}

func TestPrepareCredentialRejectsInvalidInputsBeforeActivation(t *testing.T) {
	const marker = "synthetic-password-validation-value-never-print"
	for _, test := range []struct {
		name     string
		rotator  string
		change   func(*dellcredential.Credential)
		expected string
	}{
		{"username", marker, func(credential *dellcredential.Credential) { credential.Username = "synthetic-other-account" }, "iDRAC deployment credential requires the configured username and a nonempty password"},
		{"password", marker, func(credential *dellcredential.Credential) { credential.Password = "" }, "iDRAC deployment credential requires the configured username and a nonempty password"},
		{"rotator", "", nil, "iDRAC rotator password is empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := generationConfig(t)
			credential, err := configuration.GenerateCredential()
			if err != nil {
				t.Fatal(err)
			}
			generatedPassword := credential.Password
			if test.change != nil {
				test.change(&credential)
			}
			err = configuration.PrepareCredential(t.Context(), test.rotator, credential)
			if err == nil || err.Error() != test.expected {
				t.Fatal("activation did not reject invalid credentials before certificate and controller access")
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), generatedPassword) {
				t.Fatal("the credential validation error exposed a password")
			}
		})
	}
}

func TestCredentialPreparationValidatesConfigBeforeGenerationAndActivation(t *testing.T) {
	configuration := generationConfig(t)
	candidate, err := configuration.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	configuration.Username = configuration.RotatorUser
	credential, err := configuration.GenerateCredential()
	if err == nil || credential != (dellcredential.Credential{}) {
		t.Fatal("generation accepted a deployment account equal to the rotator account")
	}
	if err := configuration.PrepareCredential(t.Context(), "synthetic-rotator-password", candidate); err == nil || !strings.Contains(err.Error(), "differ from root and the rotator") {
		t.Fatal("activation did not validate account separation before certificate and controller access")
	}
	credential, err = configuration.Prepare(t.Context(), "synthetic-rotator-password")
	if err == nil || credential != (dellcredential.Credential{}) {
		t.Fatal("the compatibility API accepted invalid account separation")
	}
}
