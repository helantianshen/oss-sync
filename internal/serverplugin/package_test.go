package serverplugin

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/helantianshen/oss-sync/internal/blog"
)

func TestValidateManifestAcceptsV1NamespacedRoute(t *testing.T) {
	manifest := Manifest{
		ID:         "hello-world",
		Name:       "Hello world",
		Version:    "1.0.0",
		APIVersion: 1,
		Routes: []RouteSpec{{
			Method: "GET",
			Path:   "/hello",
			Public: true,
		}},
	}

	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestValidateManifestRejectsUnsafeRoutePath(t *testing.T) {
	manifest := Manifest{
		ID:         "hello-world",
		Name:       "Hello world",
		Version:    "1.0.0",
		APIVersion: 1,
		Routes: []RouteSpec{{
			Method: "GET",
			Path:   "/../admin",
		}},
	}

	err := ValidateManifest(manifest)
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest() error = %v, want ErrInvalidManifest", err)
	}
}

func TestValidateManifestRejectsUnsupportedAPIAndDuplicateRoute(t *testing.T) {
	manifest := Manifest{
		ID:         "hello-world",
		Name:       "Hello world",
		Version:    "1.0.0",
		APIVersion: 99,
		Routes: []RouteSpec{
			{Method: "GET", Path: "/hello"},
			{Method: "GET", Path: "/hello"},
		},
	}
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest() error = %v, want ErrInvalidManifest", err)
	}
}

func TestValidateManifestAcceptsHostRenderedSettings(t *testing.T) {
	manifest := Manifest{
		ID: "settings-world", Name: "Settings world", Version: "1.0.0", APIVersion: CurrentAPIVersion,
		Routes:   []RouteSpec{{Method: "GET", Path: "/hello", Public: true}},
		Settings: []blog.ThemeSettingField{{Key: "endpoint", Label: "Endpoint", Type: "url", MaxLength: 500}},
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestValidateManifestAcceptsPluginThemeResources(t *testing.T) {
	manifest := Manifest{
		ID: "theme-plugin", Name: "Theme plugin", Version: "1.0.0", APIVersion: CurrentAPIVersion,
		Routes:        []RouteSpec{{Method: "GET", Path: "/hello", Public: true}},
		BlogThemes:    []ThemeResource{{ID: "clean", Name: "Clean", Path: "blog/clean"}},
		ConsoleThemes: []ThemeResource{{ID: "clean", Name: "Clean console", Path: "console/clean"}},
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
	if got := manifest.BlogThemes[0].Key(manifest.ID); got != "theme-plugin--clean" {
		t.Fatalf("resource key = %q", got)
	}
}

func TestValidateManifestRejectsUnsafePluginThemeResource(t *testing.T) {
	manifest := Manifest{
		ID: "theme-plugin", Name: "Theme plugin", Version: "1.0.0", APIVersion: CurrentAPIVersion,
		Routes:     []RouteSpec{{Method: "GET", Path: "/hello", Public: true}},
		BlogThemes: []ThemeResource{{ID: "clean", Name: "Clean", Path: "../blog"}},
	}
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest() error = %v, want ErrInvalidManifest", err)
	}
}

func TestValidateManifestRejectsUnsafeUpdateMetadata(t *testing.T) {
	t.Parallel()
	base := Manifest{
		ID: "update-metadata", Name: "Update metadata", Version: "1.0.0", APIVersion: CurrentAPIVersion,
		Routes: []RouteSpec{{Method: "GET", Path: "/hello", Public: true}},
	}
	unsafeURL := base
	unsafeURL.UpdateURL = "http://example.com/plugin.json"
	if err := ValidateManifest(unsafeURL); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest() unsafe update URL error = %v, want ErrInvalidManifest", err)
	}
	unknownVisibility := base
	unknownVisibility.SettingsVisibility = "sometimes"
	if err := ValidateManifest(unknownVisibility); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest() unknown settings visibility error = %v, want ErrInvalidManifest", err)
	}
}

func TestPluginRequestIncludesVaultSettings(t *testing.T) {
	raw, err := json.Marshal(PluginRequest{Method: "GET", Path: "/hello", Settings: map[string]any{"endpoint": "https://example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"method":"GET","path":"/hello","settings":{"endpoint":"https://example.com"}}` {
		t.Fatalf("PluginRequest JSON = %s", raw)
	}
}
