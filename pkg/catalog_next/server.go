package catalognext

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/docker/mcp-gateway/pkg/db"
	"github.com/docker/mcp-gateway/pkg/fetch"
	"github.com/docker/mcp-gateway/pkg/oci"
	"github.com/docker/mcp-gateway/pkg/policy"
	policycli "github.com/docker/mcp-gateway/pkg/policy/cli"
	"github.com/docker/mcp-gateway/pkg/workingset"
)

type InspectResult struct {
	Server        `yaml:",inline"`
	ReadmeContent string `json:"readmeContent,omitempty" yaml:"readmeContent,omitempty"`
}

type serverFilter struct {
	key   string
	value string
}

// ServerWithSource represents a server with its source catalog information
type ServerWithSource struct {
	Server
	CatalogRef string `json:"catalogRef" yaml:"catalogRef"`
	Source     string `json:"source" yaml:"source"` // "docker" or "community"
}

func InspectServer(ctx context.Context, dao db.DAO, catalogRef string, serverName string, format workingset.OutputFormat) error {
	ref, err := name.ParseReference(catalogRef)
	if err != nil {
		return fmt.Errorf("failed to parse oci-reference %s: %w", catalogRef, err)
	}
	if !oci.IsValidInputReference(ref) {
		return fmt.Errorf("reference %s must be a valid OCI reference without a digest", catalogRef)
	}

	catalogRef = oci.FullNameWithoutDigest(ref)

	// Get the catalog
	dbCatalog, err := dao.GetCatalog(ctx, catalogRef)
	if err != nil {
		return fmt.Errorf("failed to get catalog %s: %w", catalogRef, err)
	}

	catalog := NewFromDb(dbCatalog)

	server := catalog.FindServer(serverName)
	if server == nil {
		return fmt.Errorf("server %s not found in catalog %s", serverName, catalogRef)
	}

	inspectResult := InspectResult{
		Server: *server,
	}

	if server.Snapshot != nil && server.Snapshot.Server.ReadmeURL != "" {
		readmeContent, err := fetch.Untrusted(ctx, server.Snapshot.Server.ReadmeURL)
		if err != nil {
			return fmt.Errorf("failed to fetch readme: %w", err)
		}
		inspectResult.ReadmeContent = string(readmeContent)
	}

	var data []byte

	switch format {
	case workingset.OutputFormatJSON:
		data, err = json.MarshalIndent(inspectResult, "", "  ")
	case workingset.OutputFormatYAML, workingset.OutputFormatHumanReadable:
		data, err = yaml.Marshal(inspectResult)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return fmt.Errorf("failed to marshal server: %w", err)
	}

	fmt.Println(string(data))

	return nil
}

// ListAllServers lists servers from all catalogs with optional filtering
func ListAllServers(ctx context.Context, dao db.DAO, filters []string, format workingset.OutputFormat) error {
	parsedFilters, err := parseFilters(filters)
	if err != nil {
		return err
	}

	// Get all catalogs
	dbCatalogs, err := dao.ListCatalogs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list catalogs: %w", err)
	}

	if len(dbCatalogs) == 0 {
		fmt.Println("No catalogs found. Use 'docker mcp catalog-next pull <ref>' to add a catalog.")
		return nil
	}

	// Apply name filter
	var nameFilter string
	for _, filter := range parsedFilters {
		switch filter.key {
		case "name":
			nameFilter = filter.value
		default:
			return fmt.Errorf("unsupported filter key: %s", filter.key)
		}
	}

	// Collect servers from all catalogs
	var allServers []ServerWithSource
	for i := range dbCatalogs {
		dbCatalog := &dbCatalogs[i]
		catalog := NewFromDb(dbCatalog)

		// Determine source type
		source := "docker"
		if strings.HasPrefix(dbCatalog.Source, SourcePrefixCommunityRegistry) {
			source = "community"
		}

		// Filter and add servers
		filtered := filterServers(catalog.Servers, nameFilter)
		for _, server := range filtered {
			allServers = append(allServers, ServerWithSource{
				Server:     server,
				CatalogRef: catalog.Ref,
				Source:     source,
			})
		}
	}

	return outputAllServers(allServers, format)
}

// ListServers lists servers in a catalog with optional filtering
func ListServers(ctx context.Context, dao db.DAO, catalogRef string, filters []string, format workingset.OutputFormat) error {
	parsedFilters, err := parseFilters(filters)
	if err != nil {
		return err
	}

	ref, err := name.ParseReference(catalogRef)
	if err != nil {
		return fmt.Errorf("failed to parse oci-reference %s: %w", catalogRef, err)
	}
	if !oci.IsValidInputReference(ref) {
		return fmt.Errorf("reference %s must be a valid OCI reference without a digest", catalogRef)
	}

	catalogRef = oci.FullNameWithoutDigest(ref)

	// Get the catalog
	dbCatalog, err := dao.GetCatalog(ctx, catalogRef)
	if err != nil {
		return fmt.Errorf("failed to get catalog %s: %w", catalogRef, err)
	}

	catalog := NewFromDb(dbCatalog)

	policyClient := policycli.ClientForCLI(ctx)
	showPolicy := policyClient != nil
	attachCatalogPolicy(ctx, policyClient, catalog.Ref, &catalog, true)

	// Apply name filter
	var nameFilter string
	for _, filter := range parsedFilters {
		switch filter.key {
		case "name":
			nameFilter = filter.value
		default:
			return fmt.Errorf("unsupported filter key: %s", filter.key)
		}
	}

	// Filter servers
	servers := filterServers(catalog.Servers, nameFilter)

	// Output results
	return outputServers(catalog.Ref, catalog.Title, catalog.Policy, servers, format, showPolicy)
}

func parseFilters(filters []string) ([]serverFilter, error) {
	parsed := make([]serverFilter, 0, len(filters))
	for _, filter := range filters {
		parts := strings.SplitN(filter, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid filter format: %s (expected key=value)", filter)
		}
		parsed = append(parsed, serverFilter{
			key:   parts[0],
			value: parts[1],
		})
	}
	return parsed, nil
}

func filterServers(servers []Server, nameFilter string) []Server {
	if nameFilter == "" {
		return servers
	}

	nameLower := strings.ToLower(nameFilter)
	filtered := make([]Server, 0)

	for _, server := range servers {
		if matchesNameFilter(server, nameLower) {
			filtered = append(filtered, server)
		}
	}

	return filtered
}

func matchesNameFilter(server Server, nameLower string) bool {
	if server.Snapshot == nil {
		return false
	}
	serverName := strings.ToLower(server.Snapshot.Server.Name)
	return strings.Contains(serverName, nameLower)
}

func outputServers(catalogRef, catalogTitle string, catalogPolicy *policy.Decision, servers []Server, format workingset.OutputFormat, showPolicy bool) error {
	// Sort servers by name
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Snapshot == nil || servers[j].Snapshot == nil {
			return false
		}
		return servers[i].Snapshot.Server.Name < servers[j].Snapshot.Server.Name
	})

	var data []byte
	var err error

	switch format {
	case workingset.OutputFormatHumanReadable:
		printServersHuman(catalogRef, catalogTitle, catalogPolicy, servers, showPolicy)
		return nil
	case workingset.OutputFormatJSON:
		output := map[string]any{
			"catalog": catalogRef,
			"title":   catalogTitle,
			"servers": servers,
		}
		if showPolicy && catalogPolicy != nil {
			output["policy"] = catalogPolicy
		}
		data, err = json.MarshalIndent(output, "", "  ")
	case workingset.OutputFormatYAML:
		output := map[string]any{
			"catalog": catalogRef,
			"title":   catalogTitle,
			"servers": servers,
		}
		if showPolicy && catalogPolicy != nil {
			output["policy"] = catalogPolicy
		}
		data, err = yaml.Marshal(output)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return fmt.Errorf("failed to format servers: %w", err)
	}

	fmt.Println(string(data))
	return nil
}

func printServersHuman(catalogRef, catalogTitle string, catalogPolicy *policy.Decision, servers []Server, showPolicy bool) {
	if len(servers) == 0 {
		fmt.Println("No servers found")
		return
	}

	fmt.Printf("Catalog: %s\n", catalogRef)
	fmt.Printf("Title: %s\n", catalogTitle)
	if showPolicy {
		fmt.Printf("Policy: %s\n", policycli.StatusMessage(catalogPolicy))
	}
	fmt.Printf("Servers (%d):\n\n", len(servers))

	for _, server := range servers {
		if server.Snapshot == nil {
			continue
		}
		srv := server.Snapshot.Server
		fmt.Printf("  %s\n", srv.Name)
		if srv.Title != "" {
			fmt.Printf("    Title: %s\n", srv.Title)
		}
		if srv.Description != "" {
			fmt.Printf("    Description: %s\n", srv.Description)
		}
		fmt.Printf("    Type: %s\n", server.Type)
		switch server.Type {
		case workingset.ServerTypeImage:
			fmt.Printf("    Image: %s\n", server.Image)
		case workingset.ServerTypeRegistry:
			fmt.Printf("    Source: %s\n", server.Source)
		case workingset.ServerTypeRemote:
			fmt.Printf("    Endpoint: %s\n", server.Endpoint)
		}
		if showPolicy {
			fmt.Printf("    Policy: %s\n", policycli.StatusMessage(server.Policy))
		}
		if len(srv.Tools) > 0 {
			fmt.Printf("    Tools: %d\n", allowedToolCount(srv.Tools))
		}
		fmt.Println()
	}
}

func outputAllServers(servers []ServerWithSource, format workingset.OutputFormat) error {
	// Sort servers by name
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Snapshot == nil || servers[j].Snapshot == nil {
			return false
		}
		return servers[i].Snapshot.Server.Name < servers[j].Snapshot.Server.Name
	})

	var data []byte
	var err error

	switch format {
	case workingset.OutputFormatHumanReadable:
		printAllServersHuman(servers)
		return nil
	case workingset.OutputFormatJSON:
		output := map[string]any{
			"servers": servers,
			"total":   len(servers),
		}
		data, err = json.MarshalIndent(output, "", "  ")
	case workingset.OutputFormatYAML:
		output := map[string]any{
			"servers": servers,
			"total":   len(servers),
		}
		data, err = yaml.Marshal(output)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return fmt.Errorf("failed to format servers: %w", err)
	}

	fmt.Println(string(data))
	return nil
}

func printAllServersHuman(servers []ServerWithSource) {
	if len(servers) == 0 {
		fmt.Println("No servers found")
		return
	}

	fmt.Printf("All Servers (%d):\n\n", len(servers))

	for _, server := range servers {
		if server.Snapshot == nil {
			continue
		}
		srv := server.Snapshot.Server

		// Show source badge
		sourceBadge := "[docker]"
		if server.Source == "community" {
			sourceBadge = "[community]"
		}

		fmt.Printf("  %s %s\n", srv.Name, sourceBadge)
		if srv.Description != "" {
			fmt.Printf("    %s\n", srv.Description)
		}
		fmt.Println()
	}
}
