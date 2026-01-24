package workingset

import (
	"testing"

	v0 "github.com/modelcontextprotocol/registry/pkg/api/v0"
	"github.com/modelcontextprotocol/registry/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertRegistryServerToCatalog_BasicOCI(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test-server",
			Description: "A test MCP server",
			Title:       "Test Server",
			Version:     "1.0.0",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test-server:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Equal(t, "io-github-user-test-server", catalogServer.Name)
	assert.Equal(t, "server", catalogServer.Type)
	assert.Equal(t, "ghcr.io/user/test-server:1.0.0", catalogServer.Image)
	assert.Equal(t, "A test MCP server", catalogServer.Description)
	assert.Equal(t, "Test Server", catalogServer.Title)
}

func TestConvertRegistryServerToCatalog_WithIcon(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Icons: []model.Icon{
				{
					Src: "https://example.com/icon.png",
				},
			},
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Equal(t, "https://example.com/icon.png", catalogServer.Icon)
}

func TestConvertRegistryServerToCatalog_WithVolumesAndUser(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
					RuntimeArguments: []model.Argument{
						{
							Type: model.ArgumentTypeNamed,
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Value: "/host/path:/container/path",
								},
							},
							Name: "-v",
						},
						{
							Type: model.ArgumentTypeNamed,
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Value: "1000:1000",
								},
							},
							Name: "--user",
						},
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Len(t, catalogServer.Volumes, 1)
	assert.Equal(t, "/host/path:/container/path", catalogServer.Volumes[0])
	assert.Equal(t, "1000:1000", catalogServer.User)
}

func TestConvertRegistryServerToCatalog_WithCommand(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
					PackageArguments: []model.Argument{
						{
							Type: model.ArgumentTypePositional,
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Value: "--verbose",
								},
							},
						},
						{
							Type: model.ArgumentTypePositional,
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Value: "--config=/etc/config",
								},
							},
						},
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Len(t, catalogServer.Command, 2)
	assert.Equal(t, "--verbose", catalogServer.Command[0])
	assert.Equal(t, "--config=/etc/config", catalogServer.Command[1])
}

func TestConvertRegistryServerToCatalog_WithSecrets(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
					EnvironmentVariables: []model.KeyValueInput{
						{
							Name: "API_KEY",
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Description: "API key for authentication",
									IsSecret:    true,
									IsRequired:  true,
								},
							},
						},
						{
							Name: "DATABASE_PASSWORD",
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Description: "Database password",
									IsSecret:    true,
									IsRequired:  true,
								},
							},
						},
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Len(t, catalogServer.Secrets, 2)
	assert.Equal(t, "api_key", catalogServer.Secrets[0].Name)
	assert.Equal(t, "API_KEY", catalogServer.Secrets[0].Env)
	assert.Equal(t, "database_password", catalogServer.Secrets[1].Name)
	assert.Equal(t, "DATABASE_PASSWORD", catalogServer.Secrets[1].Env)
}

func TestConvertRegistryServerToCatalog_WithEnvironmentVariables(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
					EnvironmentVariables: []model.KeyValueInput{
						{
							Name: "LOG_LEVEL",
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Description: "Logging level",
									Value:       "info",
									IsRequired:  true,
								},
							},
						},
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Len(t, catalogServer.Env, 1)
	assert.Equal(t, "LOG_LEVEL", catalogServer.Env[0].Name)
	assert.Equal(t, "info", catalogServer.Env[0].Value)
}

func TestConvertRegistryServerToCatalog_WithConfigVariables(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
					EnvironmentVariables: []model.KeyValueInput{
						{
							Name: "DATABASE_URL",
							InputWithVariables: model.InputWithVariables{
								Input: model.Input{
									Description: "Database connection string",
									IsRequired:  true,
									Format:      model.FormatString,
									Value:       "{host}:{port}/{db}",
								},
								Variables: map[string]model.Input{
									"host": {
										Description: "Database host",
										Default:     "localhost",
										Format:      model.FormatString,
									},
									"port": {
										Description: "Database port",
										Default:     "5432",
										Format:      model.FormatNumber,
									},
									"db": {
										Description: "Database name",
										IsRequired:  true,
										Format:      model.FormatString,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	assert.Len(t, catalogServer.Config, 1)
	configItem := catalogServer.Config[0].(map[string]any)

	assert.Equal(t, "DATABASE_URL", configItem["name"])
	assert.Equal(t, "Database connection string", configItem["description"])
	assert.Equal(t, "object", configItem["type"])

	properties := configItem["properties"].(map[string]any)
	assert.Len(t, properties, 3)

	// Check host property
	hostProp := properties["host"].(map[string]any)
	assert.Equal(t, "string", hostProp["type"])
	assert.Equal(t, "localhost", hostProp["default"])

	// Check port property
	portProp := properties["port"].(map[string]any)
	assert.Equal(t, "number", portProp["type"])
	assert.Equal(t, "5432", portProp["default"])

	// Check db property (required)
	dbProp := properties["db"].(map[string]any)
	assert.Equal(t, "string", dbProp["type"])

	required := configItem["required"].([]string)
	assert.Contains(t, required, "db")
}

func TestConvertRegistryServerToCatalog_NoOCIPackages(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "npm",
					Identifier:   "@user/test-server",
					Version:      "1.0.0",
				},
			},
		},
	}

	_, err := convertRegistryServerToCatalog(serverResp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no OCI packages found")
}

func TestConvertRegistryServerToCatalog_MultipleOCIPackages(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
				},
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test-alt:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	// Should return the first OCI package
	assert.Equal(t, "ghcr.io/user/test:1.0.0", catalogServer.Image)
}

func TestConvertRegistryServerToCatalog_MixedPackageTypes(t *testing.T) {
	serverResp := &v0.ServerResponse{
		Server: v0.ServerJSON{
			Name:        "io.github.user/test",
			Description: "Test server",
			Packages: []model.Package{
				{
					RegistryType: "npm",
					Identifier:   "@user/test-server",
					Version:      "1.0.0",
				},
				{
					RegistryType: "oci",
					Identifier:   "ghcr.io/user/test:1.0.0",
					Transport: model.Transport{
						Type: "stdio",
					},
				},
			},
		},
	}

	catalogServer, err := convertRegistryServerToCatalog(serverResp)
	require.NoError(t, err)

	// Should return the OCI package, ignoring npm
	assert.Equal(t, "ghcr.io/user/test:1.0.0", catalogServer.Image)
}

func TestNormalizeServerName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "io.github.user/server",
			expected: "io-github-user-server",
		},
		{
			input:    "com.example.test/my-server",
			expected: "com-example-test-my-server",
		},
		{
			input:    "simple",
			expected: "simple",
		},
		{
			input:    "io.github.idjohnson/vikunjamcp",
			expected: "io-github-idjohnson-vikunjamcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeServerName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestInferJSONType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{input: "string", expected: "string"},
		{input: "number", expected: "number"},
		{input: "boolean", expected: "boolean"},
		{input: "filepath", expected: "string"},
		{input: "unknown", expected: "string"},
		{input: "", expected: "string"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := inferJSONType(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCommunityIdentifierToRegistryURL(t *testing.T) {
	tests := []struct {
		name        string
		identifier  string
		expected    string
		expectError bool
	}{
		{
			name:       "basic identifier",
			identifier: "io.github.user/myserver",
			expected:   "https://registry.modelcontextprotocol.io/v0/servers/io.github.user%2Fmyserver",
		},
		{
			name:       "identifier with version",
			identifier: "io.github.user/myserver@1.0.0",
			expected:   "https://registry.modelcontextprotocol.io/v0/servers/io.github.user%2Fmyserver/versions/1.0.0",
		},
		{
			name:       "complex namespace",
			identifier: "io.github.idjohnson/vikunjamcp@1.0.26",
			expected:   "https://registry.modelcontextprotocol.io/v0/servers/io.github.idjohnson%2Fvikunjamcp/versions/1.0.26",
		},
		{
			name:        "invalid - no slash",
			identifier:  "invalid-no-slash",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := communityIdentifierToRegistryURL(tt.identifier)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}
