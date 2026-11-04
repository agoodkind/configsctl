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
	stdout         io.Writer
	stderr         io.Writer
	logPath        string
	log            *runlog.Log
	logStreams     runlog.StreamPair
	consoleStreams runlog.StreamPair
	json           *os.File
}

// openOutput opens the private run log and creates JSONPath with mode 0600 in PrivateJSON mode.
// It redacts each stream across writes with separate run log and console redactors.
// The order between standard output and standard error is approximate.
// In PrivateJSON mode, it writes standard output unchanged to the JSON file.
func openOutput(request Request, parsed command, built environment) (*output, error) {
	patterns := built.patterns
	log, err := runlog.Open("tofu-"+runlog.Name(parsed.name), patterns)
	if err != nil {
		slog.Error("tofurun.log_open_failed", "root", request.Root, "err", err)
		return nil, fmt.Errorf("open the OpenTofu run log: %w", err)
	}
	console := os.Stdout
	if request.ProgressToStderr || request.OutputMode == PrivateJSON {
		console = os.Stderr
	}
	announcement := os.Stdout
	if request.ProgressToStderr {
		announcement = os.Stderr
	}
	log.AnnounceTo(announcement, "Tofu")
	sink := &output{
		stdout: nil, stderr: nil, logPath: log.Path(), log: log,
		logStreams: log.Streams(), consoleStreams: runlog.NewStreamPair(console, patterns), json: nil,
	}
	sink.stdout = &teeWriter{log: sink.logStreams.Stdout, console: sink.consoleStreams.Stdout}
	sink.stderr = &teeWriter{log: sink.logStreams.Stderr, console: sink.consoleStreams.Stderr}
	if request.OutputMode != PrivateJSON {
		return sink, nil
	}
	file, err := createPrivateFile(built.tmpdir, request.JSONPath)
	if err != nil {
		closeErr := sink.close(false)
		return nil, errors.Join(err, closeErr)
	}
	sink.json, sink.stdout = file, file
	return sink, nil
}

// createPrivateFile checks path again with privateOutputPath, opens an
// [os.Root] at tmpdir, and creates the file through that root.
func createPrivateFile(tmpdir, path string) (*os.File, error) {
	relative, err := privateOutputPath("JSONPath", tmpdir, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(tmpdir)
	if err != nil {
		slog.Error("tofurun.json_create_failed", "path", path, "err", err)
		return nil, fmt.Errorf("open TMPDIR for the private JSON file: %w", err)
	}
	defer closeRoot(root)
	file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
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
	consoleErr := o.consoleStreams.Close()
	if consoleErr != nil {
		slog.Error("tofurun.console_flush_failed", "err", consoleErr)
		consoleErr = fmt.Errorf("flush the redacted OpenTofu output: %w", consoleErr)
	}
	logErr := errors.Join(o.logStreams.Close(), o.log.Close())
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
	log     *redact.Writer
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
