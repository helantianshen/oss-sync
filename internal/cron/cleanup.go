package cron

import (
	"errors"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/devices"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/synclock"
)

type Cleanup struct {
	DB  *gorm.DB
	Cfg *config.Config
	now func() time.Time
}

func NewCleanup(db *gorm.DB, cfg *config.Config) *Cleanup {
	return &Cleanup{DB: db, Cfg: cfg, now: time.Now}
}

// CompactTombstones 清理已删除文件的残留正文，并在所有活跃设备确认后删除墓碑。
func (c *Cleanup) CompactTombstones() error {
	type vaultOwner struct {
		UserID  uint
		VaultID string
	}
	var owners []vaultOwner
	if err := c.DB.Model(&models.File{}).
		Where("is_deleted = ?", true).
		Distinct("user_id", "vault_id").
		Find(&owners).Error; err != nil {
		return err
	}
	now := c.now()
	for _, owner := range owners {
		lockKey := owner.VaultID
		if lockKey == "" {
			lockKey = "legacy-user-" + strconv.FormatUint(uint64(owner.UserID), 10)
		}
		vaultLock := synclock.Vault(lockKey)
		vaultLock.Lock()
		err := c.compactTombstonesForVault(owner.UserID, owner.VaultID, now)
		vaultLock.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Cleanup) compactTombstonesForVault(userID uint, vaultID string, now time.Time) error {
	safeCursor := int64(0)
	var state models.VaultSyncState
	if vaultID != "" {
		err := c.DB.Where("vault_id = ?", vaultID).First(&state).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			state = models.VaultSyncState{VaultID: vaultID}
			if err := c.DB.Create(&state).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var maxRevision int64
		if err := c.DB.Model(&models.File{}).Where("vault_id = ?", vaultID).
			Select("COALESCE(MAX(revision), 0)").Scan(&maxRevision).Error; err != nil {
			return err
		}
		if maxRevision > state.HeadRevision {
			state.HeadRevision = maxRevision
			if err := c.DB.Model(&models.VaultSyncState{}).Where("vault_id = ?", vaultID).
				Update("head_revision", maxRevision).Error; err != nil {
				return err
			}
		}
		staleBefore := now.AddDate(0, 0, -c.Cfg.Sync.EffectiveDeviceStaleDays())
		minCursor, activeDevices, err := devices.MinActiveCursor(c.DB, vaultID, staleBefore)
		if err != nil {
			return err
		}
		if activeDevices == 0 {
			safeCursor = state.HeadRevision
		} else {
			safeCursor = minCursor
		}
	}

	maxCompacted := state.CompactedRevision
	return c.DB.Transaction(func(tx *gorm.DB) error {
		var files []models.File
		if err := tx.Where(
			"user_id = ? AND vault_id = ? AND is_deleted = ?",
			userID,
			vaultID,
			true,
		).Find(&files).Error; err != nil {
			return err
		}
		for _, f := range files {
			abs := filestore.DiskPath(c.Cfg.Storage.DataDir, f)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			if f.Revision > 0 && f.Revision > safeCursor {
				continue
			}
			if err := tx.Unscoped().Delete(&f).Error; err != nil {
				return err
			}
			if f.Revision > maxCompacted {
				maxCompacted = f.Revision
			}
		}
		if vaultID != "" && maxCompacted > state.CompactedRevision {
			return tx.Model(&models.VaultSyncState{}).Where("vault_id = ?", vaultID).
				Updates(map[string]any{
					"compacted_revision": maxCompacted,
					"updated_at":         now,
				}).Error
		}
		return nil
	})
}

// PurgeOrphanAttachments 清理超过宽限期且未被引用的旧版附件。
func (c *Cleanup) PurgeOrphanAttachments() error {
	var users []models.User
	if err := c.DB.Find(&users).Error; err != nil {
		return err
	}
	now := c.now()
	for _, u := range users {
		var vaultIDs []string
		if err := c.DB.Model(&models.File{}).
			Where("user_id = ? AND is_deleted = ?", u.ID, false).
			Distinct("vault_id").
			Pluck("vault_id", &vaultIDs).Error; err != nil {
			return err
		}
		for _, vaultID := range vaultIDs {
			if err := c.purgeOrphansForVault(u.ID, vaultID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Cleanup) purgeOrphansForVault(userID uint, vaultID string, now time.Time) error {
	var mdFiles []models.File
	if err := c.DB.Where(
		"user_id = ? AND vault_id = ? AND is_deleted = ? AND type = ?",
		userID, vaultID, false, "markdown",
	).Find(&mdFiles).Error; err != nil {
		return err
	}

	referenced := make(map[string]struct{})
	for _, f := range mdFiles {
		abs := filestore.DiskPath(c.Cfg.Storage.DataDir, f)
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		for _, ref := range extractAttachmentRefs(string(raw), f.Path) {
			referenced[ref] = struct{}{}
		}
	}

	var attachments []models.File
	if err := c.DB.Where(
		"user_id = ? AND vault_id = ? AND is_deleted = ? AND type = ?",
		userID, vaultID, false, "attachment",
	).Find(&attachments).Error; err != nil {
		return err
	}

	grace := 24 * time.Hour
	for _, a := range attachments {
		// 已进入 revision 协议的附件必须走同步删除，确保其他设备收到墓碑。
		if a.Revision > 0 {
			continue
		}
		norm := normalizeRel(a.Path)
		if _, ok := referenced[norm]; ok {
			continue
		}
		abs := filestore.DiskPath(c.Cfg.Storage.DataDir, a)
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

// extractAttachmentRefs 提取 Markdown、Obsidian、frontmatter 和 HTML 中的附件引用。
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
	mdImageRe       = regexp.MustCompile(`!\[.*?\]\((.+?)\)`)
	obsidianImageRe = regexp.MustCompile(`!\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
	htmlImgRe       = regexp.MustCompile(`<img[^>]+src=["'](.+?)["']`)
	yamlImageRe     = regexp.MustCompile(`(?m)^\s*(?:cover|image|banner|thumbnail)\s*:\s*(\S+)`)
)

// splitFrontmatter 分离文档开头的 frontmatter。
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

// resolveRel 以 Markdown 文件所在目录为基准解析附件路径。
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
