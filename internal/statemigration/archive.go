package statemigration

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

func archiveMigrationCommit(ctx context.Context, configurationDirectory, commit, destination string) error {
	slog.InfoContext(ctx, "migration.preflight.archive_started", "commit", commit)
	if err := os.Mkdir(destination, 0o700); err != nil {
		return migrationPreflightFailure(ctx, "create the private reviewed checkout", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return migrationPreflightFailure(ctx, "open the private reviewed checkout", err)
	}
	defer root.Close()
	command := exec.CommandContext(ctx, "git", "archive", "--format=tar", commit)
	command.Dir = configurationDirectory
	var stderr bytes.Buffer
	command.Stderr = &stderr
	archive, err := command.StdoutPipe()
	if err != nil {
		return migrationPreflightFailure(ctx, "open the reviewed Git archive", err)
	}
	if err := command.Start(); err != nil {
		return migrationPreflightFailure(ctx, "start the reviewed Git archive", err)
	}
	extractErr := extractMigrationArchive(ctx, root, tar.NewReader(archive))
	if extractErr != nil {
		_ = archive.Close()
	}
	waitErr := command.Wait()
	if err := errors.Join(extractErr, waitErr); err != nil {
		return migrationPreflightFailure(ctx, "extract the reviewed Git archive", err)
	}
	return nil
}

func extractMigrationArchive(ctx context.Context, root *os.Root, archive *tar.Reader) error {
	var links []string
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return migrationPreflightFailure(ctx, "read a reviewed archive entry", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") || path.Clean(name) != name {
			return errors.New("the reviewed archive contains an absolute or traversing path")
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return migrationPreflightFailure(ctx, "create a confined archive directory", err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return migrationPreflightFailure(ctx, "create a reviewed archive directory", err)
			}
		case tar.TypeReg:
			if err := extractMigrationFile(ctx, root, name, header.Mode, archive); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := extractMigrationLink(ctx, root, name, header.Linkname); err != nil {
				return err
			}
			links = append(links, name)
		default:
			return errors.New("the reviewed archive contains an unsupported file type")
		}
	}
	for _, link := range links {
		if _, err := root.Stat(link); err != nil {
			return migrationPreflightFailure(ctx, "resolve the reviewed symbolic link inside the copied repository", err)
		}
	}
	return nil
}

func extractMigrationLink(ctx context.Context, root *os.Root, name, link string) error {
	target := path.Clean(path.Join(path.Dir(name), link))
	if path.IsAbs(link) || !filepath.IsLocal(target) || strings.Contains(link, "\\") {
		return errors.New("the reviewed archive contains a symbolic link outside the copied repository")
	}
	if err := root.Symlink(link, name); err != nil {
		return migrationPreflightFailure(ctx, "create a confined reviewed symbolic link", err)
	}
	return nil
}

func extractMigrationFile(ctx context.Context, root *os.Root, name string, mode int64, archive io.Reader) error {
	permissions := os.FileMode(0o600)
	if mode&0o100 != 0 {
		permissions = 0o700
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permissions)
	if err != nil {
		return migrationPreflightFailure(ctx, "create a confined reviewed file", err)
	}
	_, copyErr := io.Copy(file, archive)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return migrationPreflightFailure(ctx, "write a confined reviewed file", err)
	}
	return nil
}
