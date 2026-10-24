// Command install-opentofu installs the pinned native dependency for CI tests.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	archiveURL    = "https://github.com/opentofu/opentofu/releases/download/v1.12.6/tofu_1.12.6_linux_amd64.zip"
	archiveSHA256 = "5dc43da4f750f33873dc25e94587128709e819e544b7be9016b255316153c3a8"
	maximumBytes  = 128 << 20
	downloadLimit = 2 * time.Minute
)

func main() {
	if err := installOpenTofu(); err != nil {
		slog.Error("OpenTofu CI setup failed", "err", err)
		os.Exit(1)
	}
}

func installOpenTofu() error {
	for _, key := range []string{"RUNNER_TEMP", "GITHUB_PATH", "RUNNER_OS", "RUNNER_ARCH"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("set required runner input %s", key)
		}
	}
	if os.Getenv("RUNNER_OS") != "Linux" || os.Getenv("RUNNER_ARCH") != "X64" {
		return errors.New("OpenTofu CI setup requires a Linux X64 runner")
	}
	if !filepath.IsAbs(os.Getenv("RUNNER_TEMP")) || !filepath.IsAbs(os.Getenv("GITHUB_PATH")) {
		return errors.New("RUNNER_TEMP and GITHUB_PATH must be absolute paths")
	}
	archive, err := readArchive()
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "configsctl-opentofu-")
	if err != nil {
		slog.Error("OpenTofu installation directory failed", "err", err)
		return fmt.Errorf("create the private CI dependency directory: %w", err)
	}
	if err := writeExecutable(archive, directory); err != nil {
		return err
	}
	return writeRunnerPath(directory)
}

func readArchive() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), downloadLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		slog.Error("OpenTofu request creation failed", "err", err)
		return nil, fmt.Errorf("create the pinned OpenTofu download request: %w", err)
	}
	client := http.Client{Timeout: downloadLimit}
	response, err := client.Do(request)
	if err != nil {
		slog.Error("OpenTofu download failed", "err", err)
		return nil, fmt.Errorf("download the pinned OpenTofu archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the pinned OpenTofu archive: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil {
		slog.Error("OpenTofu archive read failed", "err", err)
		return nil, fmt.Errorf("read the pinned OpenTofu archive: %w", err)
	}
	if len(data) > maximumBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != archiveSHA256 {
		return nil, errors.New("the pinned OpenTofu archive failed its size or SHA-256 check")
	}
	return data, nil
}

func writeExecutable(data []byte, directory string) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		slog.Error("OpenTofu archive decode failed", "err", err)
		return fmt.Errorf("decode the verified OpenTofu archive: %w", err)
	}
	for _, entry := range archive.File {
		if entry.Name != "tofu" {
			continue
		}
		if !entry.Mode().IsRegular() {
			return errors.New("the verified OpenTofu executable has an unsupported type")
		}
		return writeArchiveEntry(entry, filepath.Join(directory, "tofu"))
	}
	return errors.New("the verified OpenTofu archive does not contain its executable")
}

func writeArchiveEntry(entry *zip.File, path string) error {
	if entry.UncompressedSize64 > maximumBytes {
		return errors.New("the verified OpenTofu executable exceeds its size limit")
	}
	input, err := entry.Open()
	if err != nil {
		slog.Error("OpenTofu executable read failed", "err", err)
		return fmt.Errorf("read the verified OpenTofu executable: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		slog.Error("OpenTofu executable creation failed", "err", err)
		return fmt.Errorf("create the private OpenTofu executable: %w", err)
	}
	count, copyErr := io.Copy(output, io.LimitReader(input, maximumBytes+1))
	if err := errors.Join(copyErr, output.Close()); err != nil {
		slog.Error("OpenTofu executable extraction failed", "err", err)
		return fmt.Errorf("extract the verified OpenTofu executable: %w", err)
	}
	if count != int64(entry.UncompressedSize64) {
		return errors.New("the verified OpenTofu executable has an unexpected length")
	}
	return nil
}

func writeRunnerPath(directory string) error {
	file, err := os.OpenFile(os.Getenv("GITHUB_PATH"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		slog.Error("runner path file open failed", "err", err)
		return fmt.Errorf("open the required runner path file: %w", err)
	}
	_, writeErr := fmt.Fprintln(file, directory)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		slog.Error("runner path update failed", "err", err)
		return fmt.Errorf("append the OpenTofu directory to the runner path: %w", err)
	}
	return nil
}
