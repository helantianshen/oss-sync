package cron

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

type Cleanup struct {
	DB  *gorm.DB
	Cfg *config.Config
	now func() time.Time
}

func NewCleanup(db *gorm.DB, cfg *config.Config) *Cleanup {
	return &Cleanup{DB: db, Cfg: cfg, now: time.Now}
}

// PurgeRecycleBin implements PRD 二、3.1.
// For each soft-deleted File whose deleted_at is older than the owning user's
// RecycleBinDays (0 = immediate), physically remove the disk file and hard
// delete the DB row.
func (c *Cleanup) PurgeRecycleBin() error {
	var users []models.User
	if err := c.DB.Find(&users).Error; err != nil {
		return err
	}
	now := c.now()
	for _, u := range users {
		days, err := c.recycleBinDays(u.ID)
		if err != nil {
			return err
		}
		var files []models.File
		q := c.DB.Where("user_id = ? AND is_deleted = ?", u.ID, true)
		if days > 0 {
			threshold := now.AddDate(0, 0, -days)
			q = q.Where("deleted_at IS NOT NULL AND deleted_at < ?", threshold)
		}
		if err := q.Find(&files).Error; err != nil {
			return err
		}
		for _, f := range files {
			abs := filepath.Join(c.Cfg.Storage.DataDir, fmt.Sprintf("%d", u.ID), f.Path)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := c.DB.Unscoped().Delete(&f).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Cleanup) recycleBinDays(userID uint) (int, error) {
	var us models.UserSetting
	err := c.DB.Where("user_id = ?", userID).First(&us).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 30, nil
	}
	if err != nil {
		return 0, err
	}
	if us.RecycleBinDays == nil {
		return 30, nil
	}
	d := *us.RecycleBinDays
	if d < 0 {
		return 30, nil
	}
	return d, nil
}

// PurgeOrphanAttachments implements PRD 二、3.2 + decision 6.3.
// Scans every non-deleted markdown file, extracts referenced attachment paths
// covering all 4 reference types, then deletes on-disk attachment files that
// are not referenced and older than 24h (decision 6.3 new-file grace period).
func (c *Cleanup) PurgeOrphanAttachments() error {
	var users []models.User
	if err := c.DB.Find(&users).Error; err != nil {
		return err
	}
	now := c.now()
	for _, u := range users {
		if err := c.purgeOrphansForUser(u.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cleanup) purgeOrphansForUser(userID uint, now time.Time) error {
	var mdFiles []models.File
	if err := c.DB.Where("user_id = ? AND is_deleted = ? AND type = ?",
		userID, false, "markdown").Find(&mdFiles).Error; err != nil {
		return err
	}

	referenced := make(map[string]struct{})
	userDir := filepath.Join(c.Cfg.Storage.DataDir, fmt.Sprintf("%d", userID))
	for _, f := range mdFiles {
		abs := filepath.Join(userDir, f.Path)
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		for _, ref := range extractAttachmentRefs(string(raw), f.Path) {
			referenced[ref] = struct{}{}
		}
	}

	var attachments []models.File
	if err := c.DB.Where("user_id = ? AND is_deleted = ? AND type = ?",
		userID, false, "attachment").Find(&attachments).Error; err != nil {
		return err
	}

	grace := 24 * time.Hour
	for _, a := range attachments {
		norm := normalizeRel(a.Path)
		if _, ok := referenced[norm]; ok {
			continue
		}
		abs := filepath.Join(userDir, a.Path)
		st, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if now.Sub(st.ModTime()) < grace {
			continue
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := c.DB.Unscoped().Delete(&a).Error; err != nil {
			return err
		}
	}
	return nil
}

// extractAttachmentRefs implements decision 6.3 — must cover all 4 reference
// types: standard MD image, Obsidian wikilink image, YAML frontmatter image,
// and HTML <img>. Returned paths are normalized relative to user root.
func extractAttachmentRefs(content, mdPath string) []string {
	refs := map[string]struct{}{}
	dir := path.Dir(mdPath)

	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			return
		}
		refs[normalizeRel(resolveRel(dir, raw))] = struct{}{}
	}

	fm, body := splitFrontmatter(content)
	for _, m := range mdImageRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range obsidianImageRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range htmlImgRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range yamlImageRe.FindAllStringSubmatch(fm, -1) {
		add(m[1])
	}

	out := make([]string, 0, len(refs))
	for r := range refs {
		out = append(out, r)
	}
	return out
}

var (
	mdImageRe      = regexp.MustCompile(`!\[.*?\]\((.+?)\)`)
	obsidianImageRe = regexp.MustCompile(`!\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
	htmlImgRe      = regexp.MustCompile(`<img[^>]+src=["'](.+?)["']`)
	yamlImageRe    = regexp.MustCompile(`(?m)^\s*(?:cover|image|banner|thumbnail)\s*:\s*(\S+)`)
)

// splitFrontmatter splits the leading `---\n...\n---` block from the body.
func splitFrontmatter(content string) (frontmatter, body string) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", content
	}
	rest := content[3:]
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", content
	}
	fm := rest[:idx]
	body = rest[idx+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r\n")
	return fm, body
}

// resolveRel resolves a relative path against the markdown file's directory,
// collapsing `./` and `../` segments, returning a slash-separated path.
func resolveRel(dir, ref string) string {
	if strings.HasPrefix(ref, "/") {
		return strings.TrimPrefix(ref, "/")
	}
	combined := path.Join(dir, ref)
	return combined
}

func normalizeRel(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return path.Clean(p)
}
