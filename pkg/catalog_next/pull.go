package catalognext

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/docker/mcp-gateway/pkg/db"
	"github.com/docker/mcp-gateway/pkg/oci"
	"github.com/docker/mcp-gateway/pkg/telemetry"
	"github.com/docker/mcp-gateway/pkg/workingset"
)

// Pull pulls a catalog from its source (OCI registry or community API)
// It auto-detects well-known community registries like "mcp-community"
func Pull(ctx context.Context, dao db.DAO, ociService oci.Service, refStr string) error {
	telemetry.Init()
	start := time.Now()
	var success bool
	defer func() {
		duration := time.Since(start)
		telemetry.RecordCatalogOperation(ctx, "pull", refStr, float64(duration.Milliseconds()), success)
	}()

	// Check if this is a well-known community registry
	if IsWellKnownRegistry(refStr) {
		result, err := PullCommunity(ctx, dao, refStr, DefaultPullCommunityOptions())
		if err != nil {
			return err
		}
		fmt.Printf("Pulled %d servers from community registry\n", result.ServersAdded)
		fmt.Printf("  Total in registry: %d\n", result.TotalServers)
		fmt.Printf("  With OCI packages: %d\n", result.ServersAdded)
		fmt.Printf("  Skipped (no OCI):  %d\n", result.ServersSkipped)
		success = true
		return nil
	}

	// Check if catalog exists in DB and has a community registry source
	dbCatalog, err := dao.GetCatalog(ctx, refStr)
	if err == nil && strings.HasPrefix(dbCatalog.Source, SourcePrefixCommunityRegistry) {
		// Refresh from community registry
		result, err := PullCommunity(ctx, dao, refStr, DefaultPullCommunityOptions())
		if err != nil {
			return err
		}
		fmt.Printf("Pulled %d servers from community registry\n", result.ServersAdded)
		success = true
		return nil
	}

	// Default to OCI pull
	catalog, err := pullOCI(ctx, dao, ociService, refStr)
	if err != nil {
		return err
	}

	fmt.Printf("Catalog %s pulled\n", catalog.Ref)

	success = true
	return nil
}

// PullAll pulls/refreshes all catalogs in the database
func PullAll(ctx context.Context, dao db.DAO, ociService oci.Service) error {
	catalogs, err := dao.ListCatalogs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list catalogs: %w", err)
	}

	if len(catalogs) == 0 {
		fmt.Println("No catalogs found. Use 'docker mcp catalog-next pull <ref>' to add a catalog.")
		return nil
	}

	var pullErrors []string
	for _, cat := range catalogs {
		fmt.Printf("Pulling %s...\n", cat.Ref)
		if err := Pull(ctx, dao, ociService, cat.Ref); err != nil {
			pullErrors = append(pullErrors, fmt.Sprintf("%s: %v", cat.Ref, err))
			continue
		}
	}

	if len(pullErrors) > 0 {
		return fmt.Errorf("failed to pull some catalogs:\n  %s", strings.Join(pullErrors, "\n  "))
	}

	return nil
}

// pullOCI pulls a catalog from an OCI registry
func pullOCI(ctx context.Context, dao db.DAO, ociService oci.Service, refStr string) (*db.Catalog, error) {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI reference %s: %w", refStr, err)
	}
	source := oci.FullName(ref)

	catalogArtifact, err := oci.ReadArtifact[CatalogArtifact](refStr, MCPCatalogArtifactType)
	if err != nil {
		return nil, fmt.Errorf("failed to read OCI catalog: %w", err)
	}

	catalog := Catalog{
		CatalogArtifact: catalogArtifact,
		Ref:             oci.FullNameWithoutDigest(ref),
		Source:          SourcePrefixOCI + source,
	}

	// Resolve any unresolved snapshots first
	for i := range len(catalog.Servers) {
		if catalog.Servers[i].Snapshot != nil {
			continue
		}
		switch catalog.Servers[i].Type {
		case workingset.ServerTypeImage:
			serverSnapshot, err := workingset.ResolveImageSnapshot(ctx, ociService, catalog.Servers[i].Image)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve image snapshot: %w", err)
			}
			catalog.Servers[i].Snapshot = serverSnapshot
		case workingset.ServerTypeRegistry:
			// TODO(cody): Ignore until supported
		}
	}

	if err := catalog.Validate(); err != nil {
		return nil, fmt.Errorf("invalid catalog: %w", err)
	}

	dbCatalog, err := catalog.ToDb()
	if err != nil {
		return nil, fmt.Errorf("failed to convert catalog to db: %w", err)
	}

	err = dao.UpsertCatalog(ctx, dbCatalog)
	if err != nil {
		return nil, fmt.Errorf("failed to create catalog: %w", err)
	}

	err = dao.RecordPull(ctx, refStr)
	if err != nil {
		return nil, fmt.Errorf("failed to record pull record: %w", err)
	}

	return &dbCatalog, nil
}

// pullCatalog is kept for compatibility with show.go
func pullCatalog(ctx context.Context, dao db.DAO, ociService oci.Service, refStr string) (*db.Catalog, error) {
	// Check if this is a well-known community registry
	if IsWellKnownRegistry(refStr) {
		_, err := PullCommunity(ctx, dao, refStr, DefaultPullCommunityOptions())
		if err != nil {
			return nil, err
		}
		// Return the catalog from the database
		return dao.GetCatalog(ctx, refStr)
	}

	// Check if catalog exists in DB and has a community registry source
	dbCatalog, err := dao.GetCatalog(ctx, refStr)
	if err == nil && strings.HasPrefix(dbCatalog.Source, SourcePrefixCommunityRegistry) {
		_, err := PullCommunity(ctx, dao, refStr, DefaultPullCommunityOptions())
		if err != nil {
			return nil, err
		}
		return dao.GetCatalog(ctx, refStr)
	}

	return pullOCI(ctx, dao, ociService, refStr)
}
