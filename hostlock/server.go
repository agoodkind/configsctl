package hostlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

const (
	protocolVersion       = 1
	maximumRequestBytes   = 4096
	maximumAcquireSeconds = 4200
)

var lockIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$`)

type wireRequest struct {
	Version      int       `json:"protocol_version"`
	Operation    Operation `json:"operation"`
	Run          string    `json:"run_id"`
	LeaseSeconds int64     `json:"lease_seconds"`
}

type responseStatus string

const (
	statusOK      responseStatus = "ok"
	statusHeld    responseStatus = "held"
	statusNotHeld responseStatus = "not_held"
	statusFailed  responseStatus = "failed"
)

type wireResponse struct {
	Status    responseStatus `json:"status"`
	HostTime  int64          `json:"host_time"`
	ExpiresAt int64          `json:"expires_at"`
}

func (request wireRequest) validate() error {
	if request.Version != protocolVersion || !lockIdentity.MatchString(request.Run) {
		return errors.New("host lock request requires protocol version 1 and a valid run identity")
	}
	maximum := int64(maximumAcquireSeconds)
	switch request.Operation {
	case Acquire, Renew, Release:
	case extend:
		maximum = int64(MaxExtend / time.Second)
	case Unlock:
		return errors.New("host lock request operation is invalid")
	default:
		return errors.New("host lock request operation is invalid")
	}
	if request.LeaseSeconds < 1 || request.LeaseSeconds > maximum {
		return errors.New("host lock request lease is outside the allowed range")
	}
	return nil
}

// Serve reads one bounded request for the fixed server lock directory.
// The SSH administrator supplies principal independently of client input.
func Serve(ctx context.Context, principal string, input io.Reader, output io.Writer) error {
	return (Server{Principal: principal, Directory: DefaultDir}).Serve(ctx, input, output)
}

// Server serves the existing lock protocol for an administrator-selected directory.
// A request cannot change Principal or Directory.
type Server struct {
	Principal string
	Directory string
}

// Serve reads one bounded request and writes one bounded status response.
func (server Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	principal, directory := server.Principal, server.Directory
	if !filepath.IsAbs(directory) {
		return errors.New("host lock server requires an absolute lock directory")
	}
	if !lockIdentity.MatchString(principal) {
		return errors.New("host lock server requires a valid principal")
	}
	body, err := io.ReadAll(io.LimitReader(input, maximumRequestBytes+1))
	if err != nil || len(body) > maximumRequestBytes {
		return errors.New("host lock request exceeds the input limit or cannot be read")
	}
	var request wireRequest
	if err := unmarshalStrict(body, &request, "protocol_version", "operation", "run_id", "lease_seconds"); err != nil {
		return errors.New("host lock request must contain exactly the supported JSON fields")
	}
	if err := request.validate(); err != nil {
		return err
	}
	if filepath.Clean(directory) != directory {
		return errors.New("host lock server requires a clean directory path")
	}
	result, operationErr := runScript(ctx, Host{Name: principal, Dir: directory}, command{
		op: request.Operation, run: principal + ":" + request.Run, controller: principal,
		lease: time.Duration(request.LeaseSeconds) * time.Second, server: true,
	})
	response := wireResponse{Status: statusOK, HostTime: clock.NowUTC().Unix(), ExpiresAt: result.expiry.Unix()}
	var held *HeldError
	switch {
	case operationErr == nil:
		if !result.expiry.IsZero() {
			response.HostTime = result.expiry.Add(-result.remaining).Unix()
		} else {
			response.ExpiresAt = 0
		}
	case errors.As(operationErr, &held):
		response.Status, response.ExpiresAt = statusHeld, held.Expiry.Unix()
	case errors.Is(operationErr, ErrNotHeld):
		response.Status, response.ExpiresAt = statusNotHeld, 0
	default:
		response.Status, response.ExpiresAt = statusFailed, 0
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return errors.New("write host lock response")
	}
	return nil
}

// unmarshalStrict rejects repeated fields before decoding the typed object.
func unmarshalStrict[T wireRequest | wireResponse](body []byte, target *T, fields ...string) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("expected a JSON object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid JSON field")
		}
		key, ok := token.(string)
		if !ok || !slices.Contains(fields, key) || seen[key] {
			return errors.New("unknown or repeated JSON field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("invalid JSON field value")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("null JSON field")
		}
	}
	if len(seen) != len(fields) {
		return errors.New("missing JSON field")
	}
	if _, err := decoder.Token(); err != nil {
		return errors.New("invalid JSON object end")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("additional JSON document")
	}
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON object")
	}
	return nil
}
