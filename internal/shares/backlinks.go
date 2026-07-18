package shares

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
)

var wikilinkRe = regexp.MustCompile(`\[\[([^\[\]\|]+)(?:\|[^\[\]]+)?\]\]`)

func (h *Handler) collectBacklinks(userID uint, vaultID, sourcePath string) ([]string, error) {
	var f models.File
	if err := h.DB.Where(
		"user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ? AND type = ?",
		userID, vaultID, sourcePath, false, "markdown",
	).First(&f).Error; err != nil {
		return nil, fmt.Errorf("source file not found: %s", sourcePath)
	}

	abs := filestore.DiskPath(h.Cfg.Storage.DataDir, f)
	raw, err := readAbsFile(abs)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []string
	for _, m := range wikilinkRe.FindAllStringSubmatch(string(raw), -1) {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		candidate := name
		if !strings.HasSuffix(strings.ToLower(candidate), ".md") {
			candidate = candidate + ".md"
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		var matches []models.File
		h.DB.Where(
			"user_id = ? AND vault_id = ? AND is_deleted = ? AND type = ? AND path LIKE ?",
			userID, vaultID, false, "markdown", "%/"+candidate,
		).
			Or(
				"user_id = ? AND vault_id = ? AND is_deleted = ? AND type = ? AND path = ?",
				userID, vaultID, false, "markdown", candidate,
			).
			Find(&matches)
		if len(matches) == 0 {
			continue
		}
		chosen := matches[0]
		for _, mf := range matches {
			if mf.MTime > chosen.MTime {
				chosen = mf
			}
		}
		norm := normalizeRelPath(chosen.Path)
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}

func isSafeSharePath(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	if strings.Contains(p, ":") && !strings.HasSuffix(p, ":") {
		return false
	}
	return true
}

func normalizeRelPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return path.Clean(p)
}

var readAbsFile = func(p string) ([]byte, error) {
	return readFileBytes(p)
}

func readFileBytes(p string) ([]byte, error) {
	return os.ReadFile(p)
}
