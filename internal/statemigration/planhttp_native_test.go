package statemigration_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

const httpDiagnosticMarker = "synthetic-private-http-detail-never-print"

func TestCheckPlanClassifiesNativeProviderHTTPFailuresWithoutDetail(t *testing.T) {
	workspace := cloudflareDiagnosticWorkspace(t)
	for _, status := range []int{401, 403, 429, 503, 599} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method != http.MethodGet {
					t.Error("the native diagnostic fixture attempted a non-read request")
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("X-Private-Fixture", httpDiagnosticMarker)
				writer.WriteHeader(status)
				if _, err := fmt.Fprintf(writer, `{"success":false,"errors":[{"code":10000,"message":%q}]}`, httpDiagnosticMarker+` GET "http://private.invalid": 418 I'm a teapot`); err != nil {
					t.Error("write the local diagnostic response")
				}
			}))
			t.Cleanup(server.Close)
			expected := status
			if status == 599 {
				expected = 0
			}
			assertNativeHTTPDiagnostic(t, workspace, server.URL+"/"+httpDiagnosticMarker+"/", expected, "")
			if requests.Load() == 0 {
				t.Fatal("the real native provider did not read the local HTTP server")
			}
		})
	}
}

func TestCheckPlanClassifiesNativeProviderTransportWithoutDetail(t *testing.T) {
	workspace := cloudflareDiagnosticWorkspace(t)
	t.Run("connection_refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		endpoint := server.URL
		server.Close()
		assertNativeHTTPDiagnostic(t, workspace, endpoint+"/"+httpDiagnosticMarker+"/", 0, "connection_refused")
	})
	t.Run("tls_certificate_invalid", func(t *testing.T) {
		server := httptest.NewUnstartedServer(http.NotFoundHandler())
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.StartTLS()
		t.Cleanup(server.Close)
		assertNativeHTTPDiagnostic(t, workspace, server.URL+"/"+httpDiagnosticMarker+"/", 0, "tls_certificate_invalid")
	})
}

func cloudflareDiagnosticWorkspace(t *testing.T) statemigration.Workspace {
	t.Helper()
	directory := privateFixtureDir(t)
	configuration, err := os.ReadFile(filepath.Join("testdata", "plan_cloudflare_http.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.tf"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := statemigration.Workspace{
		Directory: directory,
		Environment: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + privateFixtureDir(t),
			"TF_IN_AUTOMATION=true",
			"CHECKPOINT_DISABLE=1",
			fmt.Sprintf("CLOUDFLARE_API_TOKEN=%s", rand.Text()+rand.Text()),
		},
	}
	arguments := []string{"init", "-backend=false", "-input=false", "-no-color"}
	if mirror, declared := os.LookupEnv("CONFIGSCTL_TEST_CLOUDFLARE_PLUGIN_DIR"); declared {
		arguments = append(arguments, "-plugin-dir="+mirror)
	}
	command := exec.CommandContext(t.Context(), "tofu", arguments...)
	command.Dir, command.Env = workspace.Directory, workspace.Environment
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		t.Fatalf("initialize the real Cloudflare 5.28.1 diagnostic fixture: %v", err)
	}
	return workspace
}

func assertNativeHTTPDiagnostic(t *testing.T, workspace statemigration.Workspace, endpoint string, status int, transport string) []int64 {
	t.Helper()
	workspace.Environment = append(workspace.Environment, "TF_VAR_fixture_base_url="+endpoint)
	private := privateFixtureDir(t)
	if err := statemigration.CheckPlan(t.Context(), workspace, private); err == nil {
		t.Fatal("CheckPlan accepted a real native provider read failure")
	}
	files, err := filepath.Glob(filepath.Join(private, "reports", "verification-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("CheckPlan did not retain its safe native diagnostic report")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Diagnostics []struct {
			Category      string  `json:"category"`
			HTTPStatus    int     `json:"http_status"`
			Transport     string  `json:"transport_category"`
			APIErrorCodes []int64 `json:"api_error_codes"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(data, &report) != nil || len(report.Diagnostics) != 1 {
		t.Fatal("CheckPlan did not classify exactly one native HTTP diagnostic")
	}
	diagnostic := report.Diagnostics[0]
	if diagnostic.Category != "provider_http_request_failed" || diagnostic.HTTPStatus != status || diagnostic.Transport != transport {
		t.Fatalf("unexpected safe diagnostic category=%s status=%d transport=%s", diagnostic.Category, diagnostic.HTTPStatus, diagnostic.Transport)
	}
	for _, forbidden := range []string{httpDiagnosticMarker, endpoint, "11111111111111111111111111111111", "X-Private-Fixture", "detail", "errors", "headers"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("the public diagnostic metadata retained private HTTP detail")
		}
	}
	assertPrivatePlanMetadataOnly(t, private)
	return diagnostic.APIErrorCodes
}
