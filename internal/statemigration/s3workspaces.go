package statemigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	localBackendType   = "local"
	maxS3WorkspaceKeys = 1000
)

type s3Settings struct {
	Bucket         string            `json:"bucket"`
	Key            string            `json:"key"`
	Region         string            `json:"region"`
	Endpoint       string            `json:"endpoint"`
	Endpoints      map[string]string `json:"endpoints"`
	Prefix         *string           `json:"workspace_key_prefix"`
	PathStyle      bool              `json:"use_path_style"`
	ForcePathStyle bool              `json:"force_path_style"`
}

var (
	errBackendMetadata     = errors.New("cannot verify the initialized backend configuration")
	errListS3              = errors.New("cannot list every S3 workspace state object")
	errS3WorkspaceMismatch = errors.New(
		"the native workspace list differs from the complete S3 object listing",
	)
)

// The native S3 backend treats a denied listing page as the last page.
// Read the same prefix with signed SDK requests and compare both lists.
func verifyWorkspaceList(ctx context.Context, workspace Workspace, names []string) error {
	data, err := os.ReadFile(filepath.Join(workspace.Directory, ".terraform", "terraform.tfstate"))
	if err != nil {
		return errBackendMetadata
	}

	var metadata struct {
		Backend struct {
			Type   string     `json:"type"`
			Config s3Settings `json:"config"`
		} `json:"backend"`
	}

	if err := json.Unmarshal(data, &metadata); err != nil {
		return errBackendMetadata
	}

	if metadata.Backend.Type == localBackendType {
		return nil
	}

	if metadata.Backend.Type != "s3" {
		return errBackendMetadata
	}

	settings := metadata.Backend.Config
	if settings.Bucket == "" || settings.Key == "" || settings.Region == "" {
		return errBackendMetadata
	}

	client := settings.client(workspace.Environment)
	expected, err := settings.workspaces(ctx, client)
	if err != nil {
		return err
	}

	if !slices.Equal(slices.Sorted(slices.Values(names)), expected) {
		return errS3WorkspaceMismatch
	}

	return nil
}

func (settings s3Settings) client(environment []string) *s3.Client {
	vars := map[string]string{}

	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		vars[key] = value
	}

	auth := credentials.NewStaticCredentialsProvider(
		vars["AWS_ACCESS_KEY_ID"],
		vars["AWS_SECRET_ACCESS_KEY"],
		vars["AWS_SESSION_TOKEN"],
	)
	opts := s3.Options{
		Region:       settings.Region,
		Credentials:  auth,
		UsePathStyle: settings.PathStyle || settings.ForcePathStyle,
	}

	endpoint := settings.Endpoints["s3"]
	if endpoint == "" {
		endpoint = settings.Endpoint
	}

	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}

	return s3.New(opts)
}

func (settings s3Settings) workspaces(ctx context.Context, client *s3.Client) ([]string, error) {
	prefix := "env:/"
	if settings.Prefix != nil {
		prefix = *settings.Prefix
		if prefix != "" {
			prefix += "/"
		}
	}

	query := &s3.ListObjectsV2Input{
		Bucket:  aws.String(settings.Bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(maxS3WorkspaceKeys),
	}

	seen := map[string]bool{}
	names := map[string]bool{"default": true}

	for {
		page, err := client.ListObjectsV2(ctx, query)
		if err != nil {
			slog.ErrorContext(ctx, "retirement.s3_list_failed", "err", fmt.Sprintf("%T", err))

			return nil, errListS3
		}

		for _, object := range page.Contents {
			if object.Key == nil {
				return nil, errListS3
			}

			relative, prefixed := strings.CutPrefix(*object.Key, prefix)
			if !prefixed {
				return nil, errListS3
			}

			name, key, separated := strings.Cut(relative, "/")
			if separated && key == settings.Key && name != "" {
				names[name] = true
			}
		}

		if page.IsTruncated == nil {
			return nil, errListS3
		}

		if !*page.IsTruncated {
			return slices.Sorted(maps.Keys(names)), nil
		}

		token := aws.ToString(page.NextContinuationToken)
		if token == "" || seen[token] {
			return nil, errListS3
		}

		seen[token] = true
		query.ContinuationToken = page.NextContinuationToken
	}
}
