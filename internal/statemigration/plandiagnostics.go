package statemigration

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type planDiagnostic struct {
	Severity      string  `json:"severity"`
	Category      string  `json:"category"`
	Digest        string  `json:"summary_digest"`
	Filename      string  `json:"filename,omitempty"`
	Line          int     `json:"line,omitempty"`
	HTTPStatus    int     `json:"http_status,omitempty"`
	Transport     string  `json:"transport_category,omitempty"`
	APIErrorCodes []int64 `json:"api_error_codes,omitempty"`
}

// Only exact static summaries select a public category. Provider text remains private.
var planDiagnosticCategories = map[string]string{
	"No value for required variable":         "required_variable",
	"Invalid value for input variable":       "invalid_input_variable",
	"Reference to undeclared resource":       "undeclared_resource",
	"Reference to undeclared input variable": "undeclared_variable",
	"Reference to undeclared local value":    "undeclared_local",
	"Missing required argument":              "required_argument",
	"Unsupported argument":                   "unsupported_argument",
	"Invalid function argument":              "invalid_function_argument",
	"Invalid expression":                     "invalid_expression",
	"Invalid reference":                      "invalid_reference",
	"Invalid index":                          "invalid_index",
	"Unsupported attribute":                  "unsupported_attribute",
	"Invalid count argument":                 "invalid_count",
	"Invalid for_each argument":              "invalid_for_each",
	"Error acquiring the state lock":         "state_lock",
	"Inconsistent dependency lock file":      "dependency_lock",
	"Provider configuration not present":     "missing_provider_configuration",
	"Invalid provider configuration":         "invalid_provider_configuration",
	"failed to make http request":            "provider_http_request_failed",
}

func readPlanDiagnostics(path, workspace string) ([]planDiagnostic, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open private native JSON diagnostics")
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	diagnostics := []planDiagnostic{}
	for {
		var event struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Detail   string `json:"detail"`
				Range    struct {
					Filename string `json:"filename"`
					Start    struct {
						Line int `json:"line"`
					} `json:"start"`
				} `json:"range"`
			} `json:"diagnostic"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			return diagnostics, nil
		} else if err != nil {
			return diagnostics, errors.New("decode private native JSON diagnostics; event text remains private")
		}
		if event.Type != "diagnostic" {
			continue
		}
		native := event.Diagnostic
		category, known := planDiagnosticCategories[native.Summary]
		if !known {
			category = "unclassified_native_diagnostic"
		}
		severity := "unknown"
		if native.Severity == "error" || native.Severity == "warning" {
			severity = native.Severity
		}
		diagnostic := planDiagnostic{Severity: severity, Category: category, Digest: digest([]byte(native.Summary)), Filename: "", Line: 0, HTTPStatus: 0, Transport: "", APIErrorCodes: nil}
		if category == "provider_http_request_failed" {
			diagnostic.HTTPStatus, diagnostic.Transport, diagnostic.APIErrorCodes = providerHTTPDiagnostic(native.Detail)
		}
		diagnostic.Filename = diagnosticFilename(native.Range.Filename, workspace)
		if diagnostic.Filename != "" && native.Range.Start.Line > 0 {
			diagnostic.Line = native.Range.Start.Line
		}
		diagnostics = append(diagnostics, diagnostic)
	}
}

func diagnosticFilename(filename, workspace string) string {
	if filepath.IsAbs(filename) {
		relative, err := filepath.Rel(workspace, filename)
		if err != nil {
			return ""
		}
		filename = relative
	}
	if !filepath.IsLocal(filename) || (!strings.HasSuffix(filename, ".tf") && !strings.HasSuffix(filename, ".tf.json")) {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(filename))
}
