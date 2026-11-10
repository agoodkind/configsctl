// Package dellcredential authenticates iDRAC reads and rotates a deployment account.
package dellcredential

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config selects existing accounts. It never creates accounts or changes roles.
type Config struct {
	Endpoint           string   `yaml:"endpoint"`
	CAFile             string   `yaml:"ca_file"`
	ServerName         string   `yaml:"server_name"`
	RotatorUser        string   `yaml:"rotator_user"`
	RotatorPasswordKey string   `yaml:"rotator_password_key"`
	AccountID          string   `yaml:"account_id"`
	Username           string   `yaml:"username"`
	ReadPaths          []string `yaml:"read_paths"`
}

// Validate rejects incomplete configuration before an HTTP operation.
func (c Config) Validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("iDRAC credential endpoint must be an HTTPS origin without credentials")
	}
	if c.RotatorUser == "" || c.RotatorPasswordKey == "" || c.Username == "" || c.AccountID == "" {
		return errors.New("iDRAC credential requires rotator_user, rotator_password_key, username, and account_id")
	}
	if c.RotatorUser == c.Username || c.Username == "root" || strings.ContainsAny(c.AccountID, "/?#") {
		return errors.New("iDRAC deployment account must differ from root and the rotator account")
	}
	if len(c.ReadPaths) == 0 {
		return errors.New("iDRAC credential read_paths is empty")
	}
	for _, path := range c.ReadPaths {
		if !strings.HasPrefix(path, "/redfish/v1/") || strings.ContainsAny(path, "?#") {
			return errors.New("iDRAC credential read_paths must contain Redfish resource paths")
		}
	}
	return nil
}

// Credential exists only for the current process and its OpenTofu child.
type Credential struct {
	Username string
	Password string
}

// GenerateCredential creates a deployment credential in memory without network operations.
func (c Config) GenerateCredential() (Credential, error) {
	if err := c.Validate(); err != nil {
		return Credential{}, err
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		slog.Error("dell.credential.random_failed", "err", err)
		return Credential{}, fmt.Errorf("generate Dell deployment password: %w", err)
	}
	var candidate strings.Builder
	for _, alphabet := range []string{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz", "0123456789", "!@#$%"} {
		choice, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			slog.Error("dell.credential.random_failed", "err", err)
			return Credential{}, fmt.Errorf("generate iDRAC password character: %w", err)
		}
		candidate.WriteByte(alphabet[choice.Int64()])
	}
	candidate.WriteString(base64.RawURLEncoding.EncodeToString(random))
	return Credential{c.Username, candidate.String()}, nil
}

// Prepare generates and activates a credential under the caller's host lock.
func (c Config) Prepare(ctx context.Context, password string) (Credential, error) {
	if err := c.Validate(); err != nil {
		return Credential{}, err
	}
	if password == "" {
		return Credential{}, errors.New("iDRAC rotator password is empty")
	}
	credential, err := c.GenerateCredential()
	if err != nil {
		return Credential{}, err
	}
	if err := c.PrepareCredential(ctx, password, credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// PrepareCredential activates the supplied credential under the caller's host lock.
func (c Config) PrepareCredential(ctx context.Context, rotatorPassword string, credential Credential) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if rotatorPassword == "" {
		return errors.New("iDRAC rotator password is empty")
	}
	if credential.Username != c.Username || credential.Password == "" {
		return errors.New("iDRAC deployment credential requires the configured username and a nonempty password")
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}}
	if c.CAFile != "" {
		certificate, err := os.ReadFile(c.CAFile)
		if err != nil {
			slog.Error("dell.credential.ca_read_failed", "err", fmt.Sprintf("%T", err))
			return fmt.Errorf("read iDRAC CA certificate: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certificate) {
			return errors.New("iDRAC CA file does not contain a PEM certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	rotator := Credential{c.RotatorUser, rotatorPassword}
	for _, path := range c.ReadPaths {
		if err := c.request(ctx, client, rotator, http.MethodGet, path, nil); err != nil {
			return err
		}
	}
	accountPath := "/redfish/v1/AccountService/Accounts/" + c.AccountID
	if err := c.verifyAccount(ctx, client, rotator, accountPath); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"Password": credential.Password})
	if err != nil {
		return errors.New("encode Dell password request")
	}
	if err := c.request(ctx, client, rotator, http.MethodPatch, accountPath, body); err != nil {
		return err
	}
	for _, path := range c.ReadPaths {
		if err := c.request(ctx, client, credential, http.MethodGet, path, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) verifyAccount(ctx context.Context, client *http.Client, credential Credential, path string) error {
	response, err := c.send(ctx, client, credential, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var account struct {
		UserName string `json:"UserName"`
		Enabled  bool   `json:"Enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&account); err != nil {
		return errors.New("decode Dell deployment account")
	}
	if account.UserName != c.Username || !account.Enabled {
		return errors.New("iDRAC deployment account does not match configured enabled username")
	}
	return nil
}

func (c Config) request(ctx context.Context, client *http.Client, credential Credential, method, path string, body []byte) error {
	response, err := c.send(ctx, client, credential, method, path, body)
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		slog.Error("dell.credential.response_close_failed", "err", err)
		return fmt.Errorf("close iDRAC response: %w", err)
	}
	return nil
}

func (c Config) send(ctx context.Context, client *http.Client, credential Credential, method, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		slog.Error("dell.credential.request_failed", "method", method, "path", path, "err", err)
		return nil, errors.New("create Dell credential request")
	}
	request.SetBasicAuth(credential.Username, credential.Password)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		slog.Error("dell.credential.request_failed", "method", method, "path", path, "err", err)
		return nil, fmt.Errorf("iDRAC credential %s %s failed: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("iDRAC credential %s %s returned HTTP %d", method, path, response.StatusCode)
	}
	return response, nil
}
