package statemigration_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCheckPlanRetainsOnlyNativeProviderAPIErrorCodes(t *testing.T) {
	workspace := cloudflareDiagnosticWorkspace(t)
	fixtures := []struct {
		name  string
		body  string
		codes []int64
	}{
		{
			name:  "nested_codes",
			body:  fmt.Sprintf(`{"success":false,"code":900001,"errors":[{"code":7000,"message":%q,"documentation_url":"https://private.invalid/900002","source":{"pointer":"/900003","code":900004},"error_chain":[{"code":7003,"message":%q,"error_chain":[{"code":10000,"message":%q}]}]},{"code":1001,"message":%q}],"messages":[{"code":900005}],"result":{"code":900006}}`, httpDiagnosticMarker+` {"errors":[{"code":900007}]}`, httpDiagnosticMarker, httpDiagnosticMarker, httpDiagnosticMarker),
			codes: []int64{7000, 7003, 10000, 1001},
		},
		{name: "private_text_only", body: fmt.Sprintf(`{"message":%q,"result":{"errors":[{"code":900008}]}}`, httpDiagnosticMarker+` {"errors":[{"code":900009}]}`), codes: nil},
		{name: "nonpositive_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":0}]}]}`, codes: nil},
		{name: "negative_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":-1}]}]}`, codes: nil},
		{name: "fractional_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":7003.5}]}]}`, codes: nil},
		{name: "string_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":"7003"}]}]}`, codes: nil},
		{name: "null_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":null}]}]}`, codes: nil},
		{name: "exponent_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":7e3}]}]}`, codes: nil},
		{name: "overflow_code", body: `{"errors":[{"code":7000,"error_chain":[{"code":9223372036854775808}]}]}`, codes: nil},
		{name: "wrong_case_code", body: `{"errors":[{"code":7000,"error_chain":[{"Code":7003}]}]}`, codes: nil},
		{name: "missing_code", body: `{"errors":[{"code":7000,"error_chain":[{"message":"private"}]}]}`, codes: nil},
		{name: "nonarray_chain", body: `{"errors":[{"code":7000,"error_chain":{"code":7003}}]}`, codes: nil},
		{name: "depth_limit", body: nestedAPIErrorBody(8), codes: slices.Repeat([]int64{7000}, 8)},
		{name: "depth_overflow", body: nestedAPIErrorBody(9), codes: nil},
		{name: "count_limit", body: `{"errors":[` + strings.TrimSuffix(strings.Repeat(`{"code":7000},`, 32), ",") + `]}`, codes: slices.Repeat([]int64{7000}, 32)},
		{name: "count_overflow", body: `{"errors":[` + strings.TrimSuffix(strings.Repeat(`{"code":7000},`, 33), ",") + `]}`, codes: nil},
		{name: "body_overflow", body: fmt.Sprintf(`{"errors":[{"code":7000,"message":%q}]}`, strings.Repeat(httpDiagnosticMarker, 2048)), codes: nil},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method != http.MethodGet {
					t.Error("the native API code fixture attempted a non-read request")
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusBadRequest)
				if _, err := io.WriteString(writer, fixture.body); err != nil {
					t.Error("write the local API error response")
				}
			}))
			t.Cleanup(server.Close)
			codes := assertNativeHTTPDiagnostic(t, workspace, server.URL+"/"+httpDiagnosticMarker+"/", http.StatusBadRequest, "")
			if !slices.Equal(codes, fixture.codes) {
				t.Fatal("the native diagnostic did not retain exactly the allowed API error codes")
			}
			if requests.Load() == 0 {
				t.Fatal("the real native provider did not read the local API error server")
			}
		})
	}
}

func nestedAPIErrorBody(depth int) string {
	entry := `{"code":7000}`
	for level := 1; level < depth; level++ {
		entry = `{"code":7000,"error_chain":[` + entry + `]}`
	}
	return `{"errors":[` + entry + `]}`
}
