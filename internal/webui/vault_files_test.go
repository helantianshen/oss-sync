package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/helantianshen/oss-sync/internal/models"
)

func TestVaultFilesTemplate_whenRendered_exposesManagementAndPerFileHistoryLinks(t *testing.T) {
	t.Parallel()

	tpl, err := template.New("web").Funcs(templateFuncs()).
		ParseFS(webFS, "templates/vault_files.html")
	if err != nil {
		t.Fatalf("parse vault files template: %v", err)
	}
	data := struct {
		Layout layoutData
		Data   vaultFilesData
	}{
		Layout: layoutData{CSRF: "csrf-token"},
		Data: vaultFilesData{
			VaultID:   "vault-1",
			VaultName: "Notes",
			FileCount: 1,
			Files: []fileRow{{
				Name: "Shared.md",
				Path: "Notes/Shared.md",
				Type: "markdown",
				Size: 10,
			}},
		},
	}

	var rendered bytes.Buffer
	if err := tpl.ExecuteTemplate(&rendered, "vault-files", data); err != nil {
		t.Fatalf("render vault files template: %v", err)
	}

	wantLinks := []string{
		`href="/dashboard/vaults/vault-1/shares"`,
		`href="/dashboard/vaults/vault-1/recycle"`,
		`href="/dashboard/vaults/vault-1/history"`,
		`href="/dashboard/vaults/vault-1/settings"`,
		`href="/dashboard/vaults/vault-1/history?path=Notes%2FShared.md"`,
	}
	for _, want := range wantLinks {
		if !strings.Contains(rendered.String(), want) {
			t.Errorf("rendered vault files page missing link %s", want)
		}
	}
}

func TestBuildVaultFileBrowser_whenViewingRoot_listsDirectFilesAndFolders(t *testing.T) {
	files := []models.File{
		{Path: "# OpenCode Cli.md", Type: "markdown", Size: 12},
		{Path: "Rust/Rust.md", Type: "markdown", Size: 24},
		{Path: "Rust/Notes/guide.md", Type: "markdown", Size: 36},
	}

	browser := buildVaultFileBrowser(files, "")

	if len(browser.Files) != 1 || browser.Files[0].Name != "# OpenCode Cli.md" {
		t.Fatalf("root files = %#v, want only the direct root file", browser.Files)
	}
	if len(browser.Folders) != 1 || browser.Folders[0] != (folderRow{Name: "Rust", Path: "Rust"}) {
		t.Fatalf("root folders = %#v, want Rust", browser.Folders)
	}
	if len(browser.Breadcrumbs) != 0 {
		t.Fatalf("root breadcrumbs = %#v, want none", browser.Breadcrumbs)
	}
}

func TestBuildVaultFileBrowser_whenViewingNestedFolder_listsChildrenAndBreadcrumbs(t *testing.T) {
	files := []models.File{
		{Path: "Rust/Rust.md", Type: "markdown", Size: 24},
		{Path: "Rust/Notes/guide.md", Type: "markdown", Size: 36},
		{Path: "Rustacean.md", Type: "markdown", Size: 48},
	}

	browser := buildVaultFileBrowser(files, "Rust")

	if len(browser.Files) != 1 || browser.Files[0].Name != "Rust.md" {
		t.Fatalf("folder files = %#v, want Rust.md", browser.Files)
	}
	if len(browser.Folders) != 1 || browser.Folders[0] != (folderRow{Name: "Notes", Path: "Rust/Notes"}) {
		t.Fatalf("folder folders = %#v, want Notes", browser.Folders)
	}
	wantBreadcrumbs := []breadcrumbRow{{Name: "Rust", Path: "Rust", Current: true}}
	if len(browser.Breadcrumbs) != len(wantBreadcrumbs) || browser.Breadcrumbs[0] != wantBreadcrumbs[0] {
		t.Fatalf("breadcrumbs = %#v, want %#v", browser.Breadcrumbs, wantBreadcrumbs)
	}
}
