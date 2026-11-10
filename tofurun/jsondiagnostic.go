package tofurun

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/internal/redact"
)

const jsonIntoFlag = "json-into"

type jsonDiagnostic struct {
	directory   *os.Root
	file        *os.File
	temporary   string
	destination string
	staging     string
	patterns    []redact.Pattern
	published   bool
}

func (p *prepared) prepareJSONDiagnostic() error {
	path, found := p.command.value(jsonIntoFlag)
	if !found {
		return nil
	}
	if path == "" || path == trueValue {
		return rejection("tofurun.json_diagnostic_refused", "-json-into requires a file path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.request.Root, path)
	}
	directory, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return diagnosticFailure("open the JSON diagnostic directory", err)
	}
	temporary := ".tofurun-json-" + rand.Text()
	p.diagnostic = &jsonDiagnostic{
		directory: directory, file: nil, temporary: "",
		destination: filepath.Base(path), staging: "", patterns: p.environment.patterns, published: false,
	}
	if err := p.diagnostic.checkDestination(); err != nil {
		return err
	}
	p.diagnostic.file, err = directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return diagnosticFailure("create the private redacted JSON diagnostic file", err)
	}
	p.diagnostic.temporary = temporary
	if err := p.diagnostic.file.Chmod(privateFileMode); err != nil {
		return diagnosticFailure("restrict the redacted JSON diagnostic file", err)
	}
	stageDirectory, err := os.MkdirTemp(p.environment.tmpdir, "tofurun-json-*")
	if err != nil {
		return diagnosticFailure("create the private JSON diagnostic directory", err)
	}
	p.removals = append(p.removals, stageDirectory)
	p.diagnostic.staging = filepath.Join(stageDirectory, "events.json")
	staging, err := createPrivateFile(p.environment.tmpdir, p.diagnostic.staging)
	if err != nil {
		return err
	}
	closeQuietly(staging)
	for _, flag := range p.command.flags {
		if flag.name == jsonIntoFlag {
			p.args[flag.index] = "-json-into=" + p.diagnostic.staging
		}
	}
	return nil
}

func (p *prepared) publishJSONDiagnostic() error {
	if p.diagnostic == nil {
		return nil
	}
	return p.diagnostic.publish()
}

func (d *jsonDiagnostic) checkDestination() error {
	info, err := d.directory.Lstat(d.destination)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return diagnosticFailure("inspect the JSON diagnostic destination", err)
	}
	if !info.Mode().IsRegular() {
		return rejection("tofurun.json_diagnostic_refused", "the JSON diagnostic destination must be a regular file without symbolic links")
	}
	return checkOwner("JSON diagnostic", d.destination, info)
}

func (d *jsonDiagnostic) publish() error {
	if err := checkPrivateFile("JSON diagnostic staging", d.staging); err != nil {
		return err
	}
	input, err := os.Open(d.staging)
	if err != nil {
		return diagnosticFailure("read the private JSON diagnostic file", err)
	}
	defer closeQuietly(input)
	redacted := redact.New(d.file, d.patterns)
	_, copyErr := io.Copy(redacted, input)
	flushErr := redacted.Close()
	closeErr := d.file.Close()
	d.file = nil
	if err := errors.Join(copyErr, flushErr, closeErr); err != nil {
		return diagnosticFailure("write the redacted JSON diagnostic file", err)
	}
	if err := d.checkDestination(); err != nil {
		return err
	}
	if err := d.directory.Rename(d.temporary, d.destination); err != nil {
		return diagnosticFailure("publish the redacted JSON diagnostic file", err)
	}
	d.published = true
	return nil
}

func (d *jsonDiagnostic) cleanup() {
	if d.file != nil {
		closeQuietly(d.file)
	}
	if !d.published && d.temporary != "" {
		if err := d.directory.Remove(d.temporary); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("tofurun.json_diagnostic_cleanup_failed", "err", err)
		}
	}
	closeRoot(d.directory)
}

func diagnosticFailure(action string, err error) error {
	slog.Error("tofurun.json_diagnostic_failed", "action", action, "err", err)
	return fmt.Errorf("%s: %w", action, err)
}
