package plugins

import (
	"encoding/json"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicconfig "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/config"
)

func NormalizeConfigValues(manifest *pluginv1.PluginManifest, key string, value map[string]any) map[string]any {
	if manifest == nil || value == nil {
		return value
	}
	schema := publicconfig.FindSchema(manifest.GetGlobalConfigSchema(), key)
	if schema == nil {
		return value
	}
	schemaJSON := strings.TrimSpace(schema.GetJsonSchema())
	if schemaJSON == "" {
		return value
	}
	var doc struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schemaJSON), &doc); err != nil || doc.Properties == nil {
		return value
	}
	for propKey, propSpec := range doc.Properties {
		if propSpec.Type == "array" {
			if strVal, ok := value[propKey].(string); ok {
				var parsedArray []any
				if err := json.Unmarshal([]byte(strings.TrimSpace(strVal)), &parsedArray); err == nil {
					value[propKey] = parsedArray
				}
			}
		}
	}
	return value
}

func ValidateGlobalConfigValue(manifest *pluginv1.PluginManifest, key string, value map[string]any) error {
	NormalizeConfigValues(manifest, key, value)
	return publicconfig.ValidateManifestGlobalValue(manifest, key, value)
}
