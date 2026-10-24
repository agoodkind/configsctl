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

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

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

func TestCheckPlanRetainsNativeProviderInstanceAddressWithoutDetail(t *testing.T) {
	workspace := cloudflareDiagnosticWorkspace(t)
	path := filepath.Join(workspace.Directory, "main.tf")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configuration, diagnostics := hclwrite.ParseConfig(source, path, hcl.InitialPos)
	if diagnostics.HasErrors() {
		t.Fatal("parse the native HTTP fixture configuration")
	}
	block := configuration.Body().FirstMatchingBlock("data", []string{"cloudflare_dns_record", "diagnostic"})
	if block == nil {
		t.Fatal("the native HTTP fixture lacks its data source")
	}
	block.Body().SetAttributeValue("for_each", cty.ObjectVal(map[string]cty.Value{"fixture-instance": cty.StringVal("public-fixture")}))
	if err := os.WriteFile(path, configuration.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet {
			t.Error("the native instance fixture attempted a non-read request")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		if _, err := fmt.Fprintf(writer, `{"success":false,"errors":[{"code":10000,"message":%q}]}`, httpDiagnosticMarker); err != nil {
			t.Error("write the local instance diagnostic response")
		}
	}))
	t.Cleanup(server.Close)
	codes := assertNativeHTTPDiagnosticAddress(t, workspace, server.URL+"/"+httpDiagnosticMarker+"/", http.StatusForbidden, "", `data.cloudflare_dns_record.diagnostic["fixture-instance"]`)
	if requests.Load() == 0 || len(codes) != 1 || codes[0] != 10000 {
		t.Fatal("the real native indexed read did not retain its safe API error code")
	}
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
	return assertNativeHTTPDiagnosticAddress(t, workspace, endpoint, status, transport, "data.cloudflare_dns_record.diagnostic")
}

func assertNativeHTTPDiagnosticAddress(t *testing.T, workspace statemigration.Workspace, endpoint string, status int, transport string, address string) []int64 {
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
			Address       string  `json:"address"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(data, &report) != nil || len(report.Diagnostics) != 1 {
		t.Fatal("CheckPlan did not classify exactly one native HTTP diagnostic")
	}
	diagnostic := report.Diagnostics[0]
	if diagnostic.Address != address {
		t.Fatal("the native diagnostic omitted or changed its exact resource instance address")
	}
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
