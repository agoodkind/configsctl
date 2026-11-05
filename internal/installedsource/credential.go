package installedsource

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/dellcredential"
)

// PrepareCredential checks selected module resolutions before a controller request.
// The caller retains the existing credential host lock during this operation.
func (s *Source) PrepareCredential(ctx context.Context, request ModuleCacheRequest, adapter dellcredential.Config, rotatorValue string) (dellcredential.Credential, error) {
	if err := s.CheckModuleCache(ctx, request); err != nil {
		slog.Error("tofu.source.credential_modules_refused", "err", err)
		return dellcredential.Credential{}, fmt.Errorf("verify installed modules before preparing credentials: %w", err)
	}
	credential, err := adapter.Prepare(ctx, rotatorValue)
	if err != nil {
		return dellcredential.Credential{}, fmt.Errorf("prepare the verified workspace credential: %w", err)
	}
	return credential, nil
}
