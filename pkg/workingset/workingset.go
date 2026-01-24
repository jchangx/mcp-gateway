package workingset

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	v0 "github.com/modelcontextprotocol/registry/pkg/api/v0"
	"github.com/modelcontextprotocol/registry/pkg/model"
	"gopkg.in/yaml.v3"

	"github.com/docker/mcp-gateway/pkg/catalog"
	"github.com/docker/mcp-gateway/pkg/db"
	"github.com/docker/mcp-gateway/pkg/log"
	"github.com/docker/mcp-gateway/pkg/oci"
	"github.com/docker/mcp-gateway/pkg/registryapi"
	"github.com/docker/mcp-gateway/pkg/sliceutil"
	"github.com/docker/mcp-gateway/pkg/validate"
)

const CurrentWorkingSetVersion = 1

// WorkingSet represents a collection of MCP servers and their configurations
type WorkingSet struct {
	Version int               `yaml:"version" json:"version" validate:"required,min=1,max=1"`
	ID      string            `yaml:"id" json:"id" validate:"required"`
	Name    string            `yaml:"name" json:"name" validate:"required,min=1"`
	Servers []Server          `yaml:"servers" json:"servers" validate:"dive"`
	Secrets map[string]Secret `yaml:"secrets,omitempty" json:"secrets,omitempty" validate:"dive"`
}

type ServerType string

const (
	ServerTypeRegistry ServerType = "registry"
	ServerTypeImage    ServerType = "image"
	ServerTypeRemote   ServerType = "remote"
)

// Server represents a server configuration in a working set
type Server struct {
	Type    ServerType     `yaml:"type" json:"type" validate:"required,oneof=registry image remote"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	Secrets string         `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Tools   ToolList       `yaml:"tools,omitempty" json:"tools"` // See IsZero() below

	// ServerTypeRegistry only
	Source string `yaml:"source,omitempty" json:"source,omitempty" validate:"required_if=Type registry"`

	// ServerTypeImage only
	Image string `yaml:"image,omitempty" json:"image,omitempty" validate:"required_if=Type image"`

	// ServerTypeRemote only
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty" validate:"required_if=Type remote"`

	// Optional snapshot of the server schema
	Snapshot *ServerSnapshot `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
}

type SecretProvider string

const (
	SecretProviderDockerDesktop SecretProvider = "docker-desktop-store"
)

// Secret represents a secret configuration in a working set
type Secret struct {
	Provider SecretProvider `yaml:"provider" json:"provider" validate:"required,oneof=docker-desktop-store"`
}

type ServerSnapshot struct {
	Server catalog.Server `yaml:"server" json:"server"`
}

type ToolList []string

// Needed for proper YAML encoding with omitempty. YAML defaults IsZero to true when a slice is empty, but we only want it on nil.
// This IsZero() + omitempty matches json behavior without omitempty.
func (tools ToolList) IsZero() bool {
	return tools == nil
}

func NewFromDb(dbSet *db.WorkingSet) WorkingSet {
	servers := make([]Server, len(dbSet.Servers))
	for i, server := range dbSet.Servers {
		servers[i] = Server{
			Type:    ServerType(server.Type),
			Config:  server.Config,
			Secrets: server.Secrets,
			Tools:   server.Tools,
		}
		if server.Type == "registry" {
			servers[i].Source = server.Source
		}
		if server.Type == "image" {
			servers[i].Image = server.Image
		}
		if server.Type == "remote" {
			servers[i].Endpoint = server.Endpoint
		}

		if server.Snapshot != nil {
			servers[i].Snapshot = &ServerSnapshot{
				Server: server.Snapshot.Server,
			}
		}
	}

	secrets := make(map[string]Secret)
	for name, secret := range dbSet.Secrets {
		secrets[name] = Secret{
			Provider: SecretProvider(secret.Provider),
		}
	}

	workingSet := WorkingSet{
		Version: CurrentWorkingSetVersion,
		ID:      dbSet.ID,
		Name:    dbSet.Name,
		Servers: servers,
		Secrets: secrets,
	}

	return workingSet
}

func (workingSet WorkingSet) ToDb() db.WorkingSet {
	dbServers := make(db.ServerList, len(workingSet.Servers))
	for i, server := range workingSet.Servers {
		dbServers[i] = db.Server{
			Type:    string(server.Type),
			Config:  server.Config,
			Secrets: server.Secrets,
			Tools:   server.Tools,
		}
		if server.Type == ServerTypeRegistry {
			dbServers[i].Source = server.Source
		}
		if server.Type == ServerTypeImage {
			dbServers[i].Image = server.Image
		}
		if server.Type == ServerTypeRemote {
			dbServers[i].Endpoint = server.Endpoint
		}
		if server.Snapshot != nil {
			dbServers[i].Snapshot = &db.ServerSnapshot{
				Server: server.Snapshot.Server,
			}
		}
	}

	dbSecrets := make(db.SecretMap, len(workingSet.Secrets))
	for name, secret := range workingSet.Secrets {
		dbSecrets[name] = db.Secret{
			Provider: string(secret.Provider),
		}
	}

	dbSet := db.WorkingSet{
		ID:      workingSet.ID,
		Name:    workingSet.Name,
		Servers: dbServers,
		Secrets: dbSecrets,
	}

	return dbSet
}

func (workingSet *WorkingSet) Validate() error {
	if err := validate.Get().Struct(workingSet); err != nil {
		return err
	}
	if err := workingSet.validateUniqueServerNames(); err != nil {
		return err
	}
	return workingSet.validateServerSnapshots()
}

func (workingSet *WorkingSet) validateUniqueServerNames() error {
	seen := make(map[string]bool)
	for _, server := range workingSet.Servers {
		// TODO: Update when Snapshot is required
		if server.Snapshot == nil {
			continue
		}
		name := server.Snapshot.Server.Name
		if seen[name] {
			return fmt.Errorf("duplicate server name %s", name)
		}
		seen[name] = true
	}
	return nil
}

func (workingSet *WorkingSet) validateServerSnapshots() error {
	for _, server := range workingSet.Servers {
		if err := server.Snapshot.ValidateInnerConfig(); err != nil {
			return err
		}
	}
	return nil
}

func (serverSnapshot *ServerSnapshot) ValidateInnerConfig() error {
	if serverSnapshot == nil {
		return nil
	}

	config := serverSnapshot.Server.Config
	if config == nil {
		return nil
	}

	for i, configItem := range config {
		configMap, ok := configItem.(map[string]any)
		if !ok {
			return fmt.Errorf("config[%d] is not a map", i)
		}

		_, ok = configMap["name"].(string)
		if !ok {
			return fmt.Errorf("config[%d] has no name field", i)
		}

		_, ok = configMap["description"].(string)
		if !ok {
			return fmt.Errorf("config[%d] has no description field", i)
		}

		t, ok := configMap["type"].(string)
		if !ok {
			return fmt.Errorf("config[%d] has no type field", i)
		}
		if t != "object" {
			return fmt.Errorf("config[%d].type must be 'object', got '%s'", i, t)
		}

		properties, ok := configMap["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("config[%d].properties is not a map", i)
		}

		if err := recursivePropertiesValidate(properties, fmt.Sprintf("config[%d].properties", i)); err != nil {
			return err
		}
	}

	return nil
}

func recursivePropertiesValidate(properties map[string]any, path string) error {
	for key, property := range properties {
		propertyPath := fmt.Sprintf("%s.%s", path, key)

		propertyMap, ok := property.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not a map", propertyPath)
		}

		t, ok := propertyMap["type"].(string)
		if !ok {
			return fmt.Errorf("%s has no type field", propertyPath)
		}

		switch t {
		case "string", "integer", "number", "boolean":
			continue
		case "object":
			innerProperties, ok := propertyMap["properties"].(map[string]any)
			if !ok {
				return fmt.Errorf("%s is type 'object' but has no properties field", propertyPath)
			}
			if err := recursivePropertiesValidate(innerProperties, propertyPath); err != nil {
				return err
			}
		case "array":
			items, ok := propertyMap["items"].(map[string]any)
			if !ok {
				return fmt.Errorf("%s is type 'array' but has no items field", propertyPath)
			}
			itemType, ok := items["type"].(string)
			if !ok {
				return fmt.Errorf("%s.items has no type field", propertyPath)
			}
			if itemType != "string" {
				return fmt.Errorf("%s.items type must be string", propertyPath)
			}
		default:
			return fmt.Errorf("%s.type %s is not supported", propertyPath, t)
		}
	}
	return nil
}

func (workingSet *WorkingSet) FindServer(serverName string) *Server {
	for i := range len(workingSet.Servers) {
		if workingSet.Servers[i].Snapshot == nil {
			// TODO(cody): Can happen with registry (for now)
			continue
		}
		if workingSet.Servers[i].Snapshot.Server.Name == serverName {
			return &workingSet.Servers[i]
		}
	}
	return nil
}

func (workingSet *WorkingSet) EnsureSnapshotsResolved(ctx context.Context, ociService oci.Service) error {
	// Ensure all snapshots are resolved
	for i := range len(workingSet.Servers) {
		if workingSet.Servers[i].Snapshot != nil {
			continue
		}
		log.Log(fmt.Sprintf("Server %s has no snapshot, lazy loading the snapshot...\n", workingSet.Servers[i].BasicName()))
		snapshot, err := ResolveSnapshot(ctx, ociService, workingSet.Servers[i])
		if err != nil {
			return fmt.Errorf("failed to resolve snapshot for server[%d]: %w", i, err)
		}
		// TODO(cody): Can be nil with registry (for now)
		if snapshot != nil {
			workingSet.Servers[i].Snapshot = snapshot
		}
	}

	return nil
}

func (s *Server) BasicName() string {
	switch s.Type {
	case ServerTypeImage:
		return s.Image
	case ServerTypeRegistry:
		return s.Source
	}
	return "unknown"
}

func createWorkingSetID(ctx context.Context, name string, dao db.DAO) (string, error) {
	// Replace all non-alphanumeric characters with a hyphen and make all uppercase lowercase
	re := regexp.MustCompile("[^a-zA-Z0-9]+")
	cleaned := re.ReplaceAllString(name, "-")
	baseName := strings.ToLower(cleaned)

	existingSets, err := dao.FindWorkingSetsByIDPrefix(ctx, baseName)
	if err != nil {
		return "", fmt.Errorf("failed to find profiles by name prefix: %w", err)
	}

	if len(existingSets) == 0 {
		return baseName, nil
	}

	takenIDs := make(map[string]bool)
	for _, set := range existingSets {
		takenIDs[set.ID] = true
	}

	// TODO(cody): there are better ways to do this, but this is a simple brute force for now
	// Append a number to the base name
	for i := 2; i <= 100; i++ {
		newName := fmt.Sprintf("%s-%d", baseName, i)
		if !takenIDs[newName] {
			return newName, nil
		}
	}

	return "", fmt.Errorf("failed to create profile id")
}

func ResolveServersFromString(ctx context.Context, registryClient registryapi.Client, ociService oci.Service, dao db.DAO, value string) ([]Server, error) {
	if v, ok := strings.CutPrefix(value, "docker://"); ok {
		fullRef, err := ResolveImageRef(ctx, ociService, v)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve image ref: %w", err)
		}
		serverSnapshot, err := ResolveImageSnapshot(ctx, ociService, fullRef)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve image snapshot: %w", err)
		}
		return []Server{{
			Type:     ServerTypeImage,
			Image:    fullRef,
			Secrets:  "default",
			Snapshot: serverSnapshot,
		}}, nil
	} else if v, ok := strings.CutPrefix(value, "catalog://"); ok {
		return ResolveCatalogServers(ctx, dao, v)
	} else if v, ok := strings.CutPrefix(value, "community://"); ok {
		// Convert community://namespace/server[@version] to registry URL
		registryURL, err := communityIdentifierToRegistryURL(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse community identifier: %w", err)
		}
		server, err := ResolveRegistry(ctx, registryClient, registryURL)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve community server: %w", err)
		}
		return []Server{server}, nil
	} else if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") { // Assume registry entry if it's a URL
		server, err := ResolveRegistry(ctx, registryClient, value)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve registry: %w", err)
		}
		return []Server{server}, nil
	} else if v, ok := strings.CutPrefix(value, "file://"); ok {
		return ResolveFile(v)
	}
	return nil, fmt.Errorf("invalid server value: %s", value)
}

func ResolveFile(value string) ([]Server, error) {
	buf, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// First, see if it's a full legacy catalog file.
	// Fallback to a single server if it's not.
	var probe struct {
		Registry map[string]catalog.Server `yaml:"registry,omitempty" json:"registry,omitempty"`
	}

	var servers []catalog.Server
	switch filepath.Ext(strings.ToLower(value)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(buf, &probe); err != nil {
			return nil, fmt.Errorf("failed to unmarshal server: %w", err)
		}
		if probe.Registry == nil {
			// Fallback to parsing single server
			var server catalog.Server
			if err := yaml.Unmarshal(buf, &server); err != nil {
				return nil, fmt.Errorf("failed to unmarshal server: %w", err)
			}
			servers = []catalog.Server{server}
		}
	case ".json":
		if err := json.Unmarshal(buf, &probe); err != nil {
			return nil, fmt.Errorf("failed to unmarshal server: %w", err)
		}
		if probe.Registry == nil {
			// Fallback to parsing single server
			var server catalog.Server
			if err := json.Unmarshal(buf, &server); err != nil {
				return nil, fmt.Errorf("failed to unmarshal server: %w", err)
			}
			servers = []catalog.Server{server}
		}
	default:
		return nil, fmt.Errorf("unsupported file extension: %s, must be .yaml or .json", value)
	}

	if probe.Registry != nil {
		for name, server := range probe.Registry {
			server.Name = name
			servers = append(servers, server)
		}
	}

	serversResolved := make([]Server, len(servers))
	for i, server := range servers {
		if (server.Type == "server" || server.Type == "poci") && server.Image != "" {
			serversResolved[i] = Server{
				Type:     ServerTypeImage,
				Image:    server.Image,
				Secrets:  "default",
				Snapshot: &ServerSnapshot{Server: server},
			}
		} else if server.Type == "remote" {
			serversResolved[i] = Server{
				Type:     ServerTypeRemote,
				Endpoint: server.Remote.URL,
				Secrets:  "default",
				Snapshot: &ServerSnapshot{Server: server},
			}
		} else {
			return nil, fmt.Errorf("unsupported server type: %s", server.Type)
		}
	}

	return serversResolved, nil
}

func ResolveCatalogServers(ctx context.Context, dao db.DAO, value string) ([]Server, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid catalog URL: catalog://%s", value)
	}
	catalogRef := strings.Join(parts[:len(parts)-1], "/")
	serverList := parts[len(parts)-1]

	serverNames := strings.Split(serverList, "+")

	if len(serverNames) == 0 {
		return nil, fmt.Errorf("no servers specified in catalog URL: catalog://%s", value)
	}

	ref, err := name.ParseReference(catalogRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse catalog reference %s: %w", catalogRef, err)
	}
	catalogRef = oci.FullNameWithoutDigest(ref)

	dbCatalog, err := dao.GetCatalog(ctx, catalogRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("catalog %s not found", catalogRef)
		}
		return nil, fmt.Errorf("failed to get catalog: %w", err)
	}

	filteredServers := make([]db.CatalogServer, 0, len(dbCatalog.Servers))
	for _, server := range dbCatalog.Servers {
		if slices.Contains(serverNames, server.Snapshot.Server.Name) {
			filteredServers = append(filteredServers, server)
		}
	}
	if len(filteredServers) != len(serverNames) {
		missingServers := sliceutil.Difference(serverNames, sliceutil.Map(filteredServers, func(server db.CatalogServer) string { return server.Snapshot.Server.Name }))
		return nil, fmt.Errorf("servers were not found in catalog: %v", missingServers)
	}

	return mapCatalogServersToWorkingSetServers(filteredServers, "default"), nil
}

func ResolveImageRef(ctx context.Context, ociService oci.Service, value string) (string, error) {
	ref, err := name.ParseReference(value)
	if err != nil {
		return "", fmt.Errorf("failed to parse reference: %w", err)
	}
	isRemote := false
	img, err := ociService.GetLocalImage(ctx, ref)
	if oci.IsNoSuchImageError(err) {
		img, err = ociService.GetRemoteImage(ctx, ref)
		isRemote = true
	}
	if err != nil {
		return "", fmt.Errorf("failed to get image: %w", err)
	}
	var fullRef string
	if !isRemote || oci.HasDigest(ref) {
		// Local images shouldn't be referenced by a digest
		fullRef = ref.String()
	} else {
		// Remotes should be pinned to a digest
		digest, err := ociService.GetImageDigest(img)
		if err != nil {
			return "", fmt.Errorf("failed to get image digest: %w", err)
		}
		fullRef = fmt.Sprintf("%s@%s", ref.String(), digest)
	}

	return fullRef, nil
}

// convertRegistryServerToCatalog converts a community MCP registry server to Docker catalog format
// Only processes OCI packages (non-OCI packages are ignored)
func convertRegistryServerToCatalog(serverResp *v0.ServerResponse) (catalog.Server, error) {
	server := serverResp.Server

	// Find OCI packages
	var ociPackages []catalog.Server
	for _, pkg := range server.Packages {
		if pkg.RegistryType != "oci" {
			continue
		}

		catalogSrv := catalog.Server{
			Type:        "server",
			Image:       pkg.Identifier,
			Description: server.Description,
			Title:       server.Title,
		}

		// Extract icon (use first icon if available)
		if len(server.Icons) > 0 {
			catalogSrv.Icon = server.Icons[0].Src
		}

		// Parse runtime arguments for volumes and user settings
		for _, arg := range pkg.RuntimeArguments {
			if arg.Type == model.ArgumentTypeNamed {
				switch arg.Name {
				case "-v", "--volume":
					if arg.Value != "" {
						catalogSrv.Volumes = append(catalogSrv.Volumes, arg.Value)
					}
				case "-u", "--user":
					if arg.Value != "" {
						catalogSrv.User = arg.Value
					}
				}
			}
		}

		// Convert package arguments to command array
		for _, arg := range pkg.PackageArguments {
			if arg.Value != "" {
				catalogSrv.Command = append(catalogSrv.Command, arg.Value)
			}
		}

		// Process environment variables - separate secrets from config
		var secrets []catalog.Secret
		var envVars []catalog.Env
		var configItems []any

		for _, envVar := range pkg.EnvironmentVariables {
			if envVar.IsSecret {
				// Create secret
				secretName := strings.ToLower(envVar.Name)
				secrets = append(secrets, catalog.Secret{
					Name: secretName,
					Env:  envVar.Name,
				})
			} else if envVar.IsRequired || envVar.Default != "" || envVar.Value != "" {
				// Check if this has variables (configuration item)
				if len(envVar.Variables) > 0 {
					// This is a complex config with nested variables
					properties := make(map[string]any)
					required := []string{}

					for varName, varInput := range envVar.Variables {
						prop := map[string]any{
							"type":        inferJSONType(string(varInput.Format)),
							"description": varInput.Description,
						}
						if varInput.Default != "" {
							prop["default"] = varInput.Default
						}
						if varInput.Placeholder != "" {
							prop["placeholder"] = varInput.Placeholder
						}
						if len(varInput.Choices) > 0 {
							prop["enum"] = varInput.Choices
						}
						properties[varName] = prop

						if varInput.IsRequired {
							required = append(required, varName)
						}
					}

					configItem := map[string]any{
						"name":        envVar.Name,
						"description": envVar.Description,
						"type":        "object",
						"properties":  properties,
					}
					if len(required) > 0 {
						configItem["required"] = required
					}
					configItems = append(configItems, configItem)
				} else {
					// Simple environment variable
					envVars = append(envVars, catalog.Env{
						Name:  envVar.Name,
						Value: envVar.Value,
					})

					// Also add to config if it doesn't have a value (needs user input)
					if envVar.Value == "" || strings.Contains(envVar.Value, "{") {
						configItem := map[string]any{
							"name":        envVar.Name,
							"description": envVar.Description,
							"type":        "object",
							"properties": map[string]any{
								envVar.Name: map[string]any{
									"type":        inferJSONType(string(envVar.Format)),
									"description": envVar.Description,
								},
							},
						}
						if envVar.IsRequired {
							configItem["required"] = []string{envVar.Name}
						}
						if envVar.Default != "" {
							(configItem["properties"].(map[string]any)[envVar.Name].(map[string]any))["default"] = envVar.Default
						}
						configItems = append(configItems, configItem)
					}
				}
			}
		}

		catalogSrv.Secrets = secrets
		catalogSrv.Env = envVars
		catalogSrv.Config = configItems

		// Extract OAuth if present
		// Note: The registry API doesn't have OAuth in the current schema
		// This would need to be added if OAuth support is required

		ociPackages = append(ociPackages, catalogSrv)
	}

	if len(ociPackages) == 0 {
		return catalog.Server{}, fmt.Errorf("no OCI packages found for server")
	}

	// For now, return the first OCI package
	// In the future, we might want to handle multiple packages differently
	result := ociPackages[0]
	result.Name = normalizeServerName(server.Name)

	return result, nil
}

// normalizeServerName converts a registry server name to a valid catalog name
// Example: io.github.user/server -> io-github-user-server
func normalizeServerName(name string) string {
	// Replace dots and slashes with hyphens
	normalized := strings.ReplaceAll(name, ".", "-")
	normalized = strings.ReplaceAll(normalized, "/", "-")
	return normalized
}

// inferJSONType converts registry format to JSON schema type
func inferJSONType(format string) string {
	switch format {
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "filepath":
		return "string"
	default:
		return "string"
	}
}

// communityIdentifierToRegistryURL converts a community:// identifier to a full registry URL
// Format: community://namespace/server-name[@version]
// Examples:
//   - community://io.github.user/myserver -> https://registry.modelcontextprotocol.io/v0/servers/io.github.user%2Fmyserver
//   - community://io.github.user/myserver@1.0.0 -> https://registry.modelcontextprotocol.io/v0/servers/io.github.user%2Fmyserver/versions/1.0.0
func communityIdentifierToRegistryURL(identifier string) (string, error) {
	// Check for version suffix
	var version string
	if idx := strings.LastIndex(identifier, "@"); idx != -1 {
		version = identifier[idx+1:]
		identifier = identifier[:idx]
	}

	// Validate identifier has namespace/name format
	if !strings.Contains(identifier, "/") {
		return "", fmt.Errorf("invalid community identifier %q: expected format namespace/server-name", identifier)
	}

	return registryapi.NewServerURL(identifier, version).String(), nil
}

func ResolveRegistry(ctx context.Context, registryClient registryapi.Client, value string) (Server, error) {
	url, err := registryapi.ParseServerURL(value)
	if err != nil {
		return Server{}, fmt.Errorf("failed to parse server URL %s: %w", value, err)
	}

	versions, err := registryClient.GetServerVersions(ctx, url)
	if err != nil {
		return Server{}, fmt.Errorf("failed to get server versions from URL %s: %w", url.VersionsListURL(), err)
	}

	if len(versions.Servers) == 0 {
		return Server{}, fmt.Errorf("no server versions found for URL %s", url.VersionsListURL())
	}

	if url.IsLatestVersion() {
		latestVersion, err := resolveLatestVersion(versions)
		if err != nil {
			return Server{}, fmt.Errorf("failed to resolve latest version for server %s: %w", url.VersionsListURL(), err)
		}
		url = url.WithVersion(latestVersion)
	}

	var serverResp *v0.ServerResponse
	for _, version := range versions.Servers {
		if version.Server.Version == url.Version {
			serverResp = &version
			break
		}
	}
	if serverResp == nil {
		return Server{}, fmt.Errorf("server version not found")
	}

	// Check for OCI packages and convert to catalog format
	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	if err != nil {
		return Server{}, fmt.Errorf("failed to convert registry server: %w", err)
	}

	return Server{
		Type:     ServerTypeRegistry,
		Source:   url.String(),
		Secrets:  "default",
		Snapshot: &ServerSnapshot{Server: catalogServer},
	}, nil
}

func ResolveSnapshot(ctx context.Context, ociService oci.Service, server Server) (*ServerSnapshot, error) {
	switch server.Type {
	case ServerTypeImage:
		return ResolveImageSnapshot(ctx, ociService, server.Image)
	case ServerTypeRegistry:
		// Snapshots for registry servers are resolved during ResolveRegistry
		return nil, nil //nolint:nilnil
	case ServerTypeRemote:
		// TODO(bobby): add snapshot when you can add remotes directly from URL
		return nil, nil //nolint:nilnil
	}
	return nil, fmt.Errorf("unsupported server type: %s", server.Type)
}

func ResolveImageSnapshot(ctx context.Context, ociService oci.Service, image string) (*ServerSnapshot, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, fmt.Errorf("failed to parse reference: %w", err)
	}

	var img v1.Image
	// Anything with a digest should be a remote image
	if oci.HasDigest(ref) {
		img, err = ociService.GetRemoteImage(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("failed to get remote image: %w", err)
		}
	} else {
		img, err = ociService.GetLocalImage(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("failed to get local image: %w", err)
		}
	}

	serverSnapshot, err := getCatalogServerFromImage(ociService, img, image)
	if err != nil {
		return nil, fmt.Errorf("failed to get catalog server from image: %w", err)
	}
	return &ServerSnapshot{
		Server: serverSnapshot,
	}, nil
}

// Pins the "latest" to a specific version
func resolveLatestVersion(versions v0.ServerListResponse) (string, error) {
	for _, version := range versions.Servers {
		if version.Meta.Official.IsLatest {
			return version.Server.Version, nil
		}
	}
	return "", fmt.Errorf("no latest version found")
}

func getCatalogServerFromImage(ociService oci.Service, img v1.Image, name string) (catalog.Server, error) {
	labels, err := ociService.GetImageLabels(img)
	if err != nil {
		return catalog.Server{}, fmt.Errorf("failed to get image labels: %w", err)
	}
	metadataLabel := labels["io.docker.server.metadata"]
	if metadataLabel == "" {
		return catalog.Server{}, fmt.Errorf("image %s is not a self-describing image", name)
	}

	// Basic parsing validation
	var server catalog.Server
	if err := yaml.Unmarshal([]byte(metadataLabel), &server); err != nil {
		return catalog.Server{}, fmt.Errorf("failed to parse metadata label for %s: %w", name, err)
	}

	server.Type = "server"
	server.Image = name

	return server, nil
}

func mapCatalogServersToWorkingSetServers(dbServers []db.CatalogServer, secrets string) []Server {
	servers := make([]Server, len(dbServers))
	for i, server := range dbServers {
		servers[i] = Server{
			Type:     ServerType(server.ServerType),
			Tools:    ToolList(server.Tools),
			Config:   map[string]any{},
			Source:   server.Source,
			Image:    server.Image,
			Endpoint: server.Endpoint,
			Snapshot: &ServerSnapshot{
				Server: server.Snapshot.Server,
			},
			Secrets: secrets,
		}
	}
	return servers
}
