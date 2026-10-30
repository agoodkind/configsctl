package tofurun

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/runlog"
)

// output is the destination of one OpenTofu process.
type output struct {
	stdout  io.Writer
	stderr  io.Writer
	logPath string
	log     *runlog.Log
	console *redact.Writer
	json    *os.File
}

// openOutput opens the private run log and, for PrivateJSON, creates
// JSONPath with mode 0600. RedactedProgress uses one writer for standard
// output and standard error. The writer records their writes in order.
func openOutput(request Request, parsed command, patterns []redact.Pattern) (*output, error) {
	log, err := runlog.Open("tofu-"+runlog.Name(parsed.name), patterns)
	if err != nil {
		slog.Error("tofurun.log_open_failed", "root", request.Root, "err", err)
		return nil, fmt.Errorf("open the OpenTofu run log: %w", err)
	}
	log.Announce("Tofu")
	console := os.Stdout
	if request.OutputMode == PrivateJSON {
		console = os.Stderr
	}
	sink := &output{
		stdout: nil, stderr: nil, logPath: log.Path(), log: log,
		console: redact.New(console, patterns), json: nil,
	}
	progress := &teeWriter{log: log, console: sink.console}
	sink.stdout, sink.stderr = progress, progress
	if request.OutputMode != PrivateJSON {
		return sink, nil
	}
	file, err := createPrivateFile(request.JSONPath)
	if err != nil {
		closeErr := sink.close(false)
		return nil, errors.Join(err, closeErr)
	}
	sink.json, sink.stdout = file, file
	return sink, nil
}

func createPrivateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		slog.Error("tofurun.json_create_failed", "path", path, "err", err)
		return nil, fmt.Errorf("create the private JSON file: %w", err)
	}
	if err := file.Chmod(privateFileMode); err != nil {
		closeQuietly(file)
		removeQuietly(path)
		slog.Error("tofurun.json_create_failed", "path", path, "err", err)
		return nil, fmt.Errorf("restrict the private JSON file: %w", err)
	}
	return file, nil
}

// close flushes the redacted console output and closes the run log and the
// JSON file. close removes the JSON file unless keep is true.
func (o *output) close(keep bool) error {
	consoleErr := o.console.Close()
	if consoleErr != nil {
		slog.Error("tofurun.console_flush_failed", "err", consoleErr)
		consoleErr = fmt.Errorf("flush the redacted OpenTofu output: %w", consoleErr)
	}
	logErr := o.log.Close()
	if logErr != nil {
		slog.Error("tofurun.log_close_failed", "log", o.logPath, "err", logErr)
		logErr = fmt.Errorf("close the OpenTofu run log: %w", logErr)
	}
	var jsonErr error
	if o.json != nil {
		jsonErr = o.json.Close()
		if jsonErr != nil {
			slog.Error("tofurun.json_close_failed", "path", o.json.Name(), "err", jsonErr)
			jsonErr = fmt.Errorf("close the private JSON file: %w", jsonErr)
		}
		if !keep || jsonErr != nil {
			removeQuietly(o.json.Name())
		}
	}
	return errors.Join(consoleErr, logErr, jsonErr)
}

func removeQuietly(path string) {
	if err := os.Remove(path); err != nil {
		slog.Warn("tofurun.remove_failed", "path", path, "err", err)
	}
}

// teeWriter writes each chunk to the run log and to the redacted console.
// Each destination applies its own redaction.
type teeWriter struct {
	log     *runlog.Log
	console *redact.Writer
}

func (t *teeWriter) Write(chunk []byte) (int, error) {
	if _, err := t.log.Write(chunk); err != nil {
		return 0, fmt.Errorf("write the OpenTofu run log: %w", err)
	}
	if _, err := t.console.Write(chunk); err != nil {
		return 0, fmt.Errorf("write the redacted OpenTofu output: %w", err)
	}
	return len(chunk), nil
}
