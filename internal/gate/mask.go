package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/redact"
)

func maskedRequest(req Request, secrets []redact.Pattern) (Request, error) {
	if len(req.ExtraVars) == 0 {
		return req, nil
	}
	patterns, err := jsonSecretPatterns(secrets)
	if err != nil {
		return Request{}, err
	}
	var masked bytes.Buffer
	writer := redact.New(&masked, patterns)
	if _, err := writer.Write(req.ExtraVars); err != nil {
		slog.Error("gate.extra_vars.mask_failed", "err", err)
		return Request{}, fmt.Errorf("mask extra_vars: %w", err)
	}
	if err := writer.Close(); err != nil {
		slog.Error("gate.extra_vars.mask_close_failed", "err", err)
		return Request{}, fmt.Errorf("finish masking extra_vars: %w", err)
	}
	stored := req
	stored.ExtraVars = json.RawMessage(masked.Bytes())
	return stored, nil
}

// jsonSecretPatterns includes raw secret bytes and their JSON-escaped form.
// Masking does not match JSON \uXXXX escapes for ordinary characters.
func jsonSecretPatterns(secrets []redact.Pattern) ([]redact.Pattern, error) {
	patterns := make([]redact.Pattern, 0, 2*len(secrets))
	for _, secret := range secrets {
		patterns = append(patterns, secret)
		escaped, err := jsonEscaped(secret.Value)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(escaped, secret.Value) {
			patterns = append(patterns, redact.Pattern{Value: escaped, Label: secret.Label})
		}
	}
	return patterns, nil
}

func jsonEscaped(value []byte) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(string(value)); err != nil {
		slog.Error("gate.extra_vars.escape_failed", "err", err)
		return nil, fmt.Errorf("escape a secret value as a JSON string: %w", err)
	}
	quoted := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	return quoted[1 : len(quoted)-1], nil
}
