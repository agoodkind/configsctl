package statemigration

import (
	"encoding/json"
	"regexp"
	"strconv"
)

const (
	providerAPIErrorBodyLimit  = 64 * 1024
	providerAPIErrorCodeLimit  = 32
	providerAPIErrorDepthLimit = 8
)

var providerAPIErrorInteger = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// Decode only errors and error_chain codes from the SDK's verified response body.
// Any malformed code or exceeded bound discards the complete code list.
func providerAPIErrorCodes(body string) []int64 {
	if len(body) > providerAPIErrorBodyLimit {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return nil
	}
	codes := []int64{}
	if !collectProviderAPIErrorCodes(envelope["errors"], 1, &codes) {
		return nil
	}
	return codes
}

func collectProviderAPIErrorCodes(raw json.RawMessage, depth int, codes *[]int64) bool {
	if depth > providerAPIErrorDepthLimit {
		return false
	}
	var errors []map[string]json.RawMessage
	if json.Unmarshal(raw, &errors) != nil || len(errors) > providerAPIErrorCodeLimit-len(*codes) {
		return false
	}
	for _, entry := range errors {
		rawCode := entry["code"]
		if !providerAPIErrorInteger.Match(rawCode) {
			return false
		}
		code, err := strconv.ParseInt(string(rawCode), 10, 64)
		if err != nil || code <= 0 || len(*codes) == providerAPIErrorCodeLimit {
			return false
		}
		*codes = append(*codes, code)
		chain := entry["error_chain"]
		if len(chain) != 0 && !collectProviderAPIErrorCodes(chain, depth+1, codes) {
			return false
		}
	}
	return true
}
