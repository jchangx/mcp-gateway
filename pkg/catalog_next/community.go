package catalognext

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/mcp-gateway/pkg/catalog"
	"github.com/docker/mcp-gateway/pkg/db"
	"github.com/docker/mcp-gateway/pkg/registryapi"
	"github.com/docker/mcp-gateway/pkg/workingset"
)

// Well-known community registries
const (
	// CommunityCatalogRef is the reference for the community catalog
	CommunityCatalogRef = "mcp-community:latest"
	// CommunityCatalogName is the name prefix for community catalogs
	CommunityCatalogName = "mcp-community"
	// CommunityCatalogDisplayName is the human-readable name
	CommunityCatalogDisplayName = "MCP Community Registry"
	// CommunityTag is added to servers from community registries
	CommunityTag = "community"
)

// WellKnownRegistries maps catalog names to their API URLs
var WellKnownRegistries = map[string]string{
	CommunityCatalogName: registryapi.CommunityRegistryBaseURL,
}

// IsWellKnownRegistry checks if a reference is a well-known community registry
func IsWellKnownRegistry(refStr string) bool {
	// Extract the name part (before :tag if present)
	name := refStr
	if idx := strings.Index(refStr, ":"); idx != -1 {
		name = refStr[:idx]
	}
	_, ok := WellKnownRegistries[name]
	return ok
}

// GetRegistryURL returns the API URL for a well-known registry
func GetRegistryURL(refStr string) string {
	name := refStr
	if idx := strings.Index(refStr, ":"); idx != -1 {
		name = refStr[:idx]
	}
	return WellKnownRegistries[name]
}

// PullCommunityOptions contains options for pulling from community registries
type PullCommunityOptions struct {
	// Reserved for future options
}

// DefaultPullCommunityOptions returns the default options
func DefaultPullCommunityOptions() PullCommunityOptions {
	return PullCommunityOptions{}
}

// PullCommunityResult contains the results of a community registry pull
type PullCommunityResult struct {
	ServersAdded   int
	ServersSkipped int
	TotalServers   int
}

// PullCommunity fetches servers from a community registry API and writes to the database
func PullCommunity(ctx context.Context, dao db.DAO, refStr string, opts PullCommunityOptions) (*PullCommunityResult, error) {
	registryURL := GetRegistryURL(refStr)
	if registryURL == "" {
		return nil, fmt.Errorf("unknown community registry: %s", refStr)
	}

	client := registryapi.NewClient()

	// Fetch all servers from community registry (uses cache if available)
	servers, err := client.ListServers(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch servers from community registry: %w", err)
	}

	// Convert to catalog format (API already returns only latest versions)
	catalogServers := make(map[string]catalog.Server)
	skipped := 0

	for _, serverResp := range servers {
		// Get normalized name
		normalizedName := workingset.NormalizeServerName(serverResp.Server.Name)

		// Try to convert to catalog format (filters to OCI packages)
		catalogServer, err := workingset.ConvertRegistryServerToCatalog(&serverResp)
		if err != nil {
			// Skip non-OCI servers
			skipped++
			continue
		}

		// Add "community" tag to identify source
		if catalogServer.Metadata == nil {
			catalogServer.Metadata = &catalog.Metadata{}
		}
		catalogServer.Metadata.Tags = appendIfMissing(catalogServer.Metadata.Tags, CommunityTag)

		catalogServers[normalizedName] = catalogServer
	}

	// Build the ref string
	catalogRef := refStr
	if !strings.Contains(refStr, ":") {
		catalogRef = refStr + ":latest"
	}

	// Write to database
	if err := writeCommunityToDatabase(ctx, dao, catalogRef, catalogServers); err != nil {
		return nil, fmt.Errorf("failed to write catalog to database: %w", err)
	}

	return &PullCommunityResult{
		ServersAdded:   len(catalogServers),
		ServersSkipped: skipped,
		TotalServers:   len(servers),
	}, nil
}

// writeCommunityToDatabase writes the community catalog to the database
func writeCommunityToDatabase(ctx context.Context, dao db.DAO, catalogRef string, catalogServers map[string]catalog.Server) error {
	// Convert catalog.Server entries to Server entries with snapshots
	nextServers := make([]Server, 0, len(catalogServers))
	for name, server := range catalogServers {
		// Ensure server has its name set
		server.Name = name

		nextServer := Server{
			Type:  workingset.ServerTypeImage,
			Image: server.Image,
			Snapshot: &workingset.ServerSnapshot{
				Server: server,
			},
		}
		nextServers = append(nextServers, nextServer)
	}

	// Sort servers by name for consistent ordering
	sort.Slice(nextServers, func(i, j int) bool {
		return nextServers[i].Snapshot.Server.Name < nextServers[j].Snapshot.Server.Name
	})

	// Extract name from ref for source
	name := catalogRef
	if idx := strings.Index(catalogRef, ":"); idx != -1 {
		name = catalogRef[:idx]
	}

	// Create the catalog structure
	nextCatalog := Catalog{
		Ref:    catalogRef,
		Source: SourcePrefixCommunityRegistry + name,
		CatalogArtifact: CatalogArtifact{
			Title:   CommunityCatalogDisplayName,
			Servers: nextServers,
		},
	}

	// Convert to database format and upsert
	dbCatalog, err := nextCatalog.ToDb()
	if err != nil {
		return fmt.Errorf("failed to convert catalog to database format: %w", err)
	}

	if err := dao.UpsertCatalog(ctx, dbCatalog); err != nil {
		return fmt.Errorf("failed to upsert catalog: %w", err)
	}

	return nil
}

// appendIfMissing appends a value to a slice if it's not already present
func appendIfMissing(slice []string, val string) []string {
	for _, item := range slice {
		if item == val {
			return slice
		}
	}
	return append(slice, val)
}
