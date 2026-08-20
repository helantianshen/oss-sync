package webui

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/oss/oss-server/internal/blog"
	"github.com/oss/oss-server/internal/deviceauth"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/history"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/recycle"
	"github.com/oss/oss-server/internal/settingspolicy"
	"github.com/oss/oss-server/internal/shares"
	"github.com/oss/oss-server/internal/synclock"
	"github.com/oss/oss-server/internal/vaultaccess"
	"github.com/oss/oss-server/internal/vaultbackup"
)

const maxCustomFragmentRunes = 2000

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func (h *Handler) newSharesService() *shares.Handler {
	return shares.New(h.DB, h.Cfg)
}

// 仓库列表

type vaultRow struct {
	ID           string
	Name         string
	Description  string
	IsDefault    bool
	AccessRole   string
	StorageUsed  int64
	StorageQuota int64
	MemberCount  int64
}

func (h *Handler) vaultsPage(c *gin.Context) {
	u := h.webUser(c)
	d := struct {
		Vaults []vaultRow
		Error  string
	}{Error: c.Query("error")}

	var owned []models.Vault
	if err := h.DB.Where("owner_id = ?", u.ID).Order("is_default desc, created_at asc").Find(&owned).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vaults", h.t(c, "page.vaults"), "vaults", "vaults", d)
		return
	}
	var members []models.VaultMember
	if err := h.DB.Where("user_id = ?", u.ID).Find(&members).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vaults", h.t(c, "page.vaults"), "vaults", "vaults", d)
		return
	}
	ids := make([]string, 0, len(owned)+len(members))
	roleByID := map[string]string{}
	for _, v := range owned {
		ids = append(ids, v.ID)
		roleByID[v.ID] = vaultaccess.RoleOwner
	}
	for _, m := range members {
		ids = append(ids, m.VaultID)
		if roleByID[m.VaultID] == "" {
			roleByID[m.VaultID] = m.Role
		}
	}
	if len(ids) == 0 {
		h.render(c, http.StatusOK, "vaults", h.t(c, "page.vaults"), "vaults", "vaults", d)
		return
	}
	var all []models.Vault
	if err := h.DB.Where("id IN ?", ids).Find(&all).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vaults", h.t(c, "page.vaults"), "vaults", "vaults", d)
		return
	}
	memberCounts := map[string]int64{}
	for _, id := range ids {
		var cnt int64
		if err := h.DB.Model(&models.VaultMember{}).Where("vault_id = ?", id).Count(&cnt).Error; err == nil {
			memberCounts[id] = cnt
		}
	}
	for _, v := range all {
		d.Vaults = append(d.Vaults, vaultRow{
			ID: v.ID, Name: v.Name, Description: v.Description,
			IsDefault: v.IsDefault, AccessRole: roleByID[v.ID],
			StorageUsed: v.StorageUsed, StorageQuota: v.StorageQuota, MemberCount: memberCounts[v.ID],
		})
	}
	h.render(c, http.StatusOK, "vaults", h.t(c, "page.vaults"), "vaults", "vaults", d)
}

type newVaultData struct {
	Name        string
	Description string
	Error       string
}

func (h *Handler) newVaultPage(c *gin.Context) {
	h.render(c, http.StatusOK, "vaults-new", h.t(c, "page.new_vault"), "vaults", "vaults-new", newVaultData{Error: c.Query("error")})
}

func (h *Handler) createVault(c *gin.Context) {
	u := h.webUser(c)
	name := strings.TrimSpace(c.PostForm("name"))
	desc := strings.TrimSpace(c.PostForm("description"))
	if name == "" || len(name) > 128 {
		h.render(c, http.StatusBadRequest, "vaults-new", h.t(c, "page.new_vault"), "vaults", "vaults-new",
			newVaultData{Name: name, Description: desc, Error: h.t(c, "err.vault_name_invalid")})
		return
	}
	vault := models.Vault{
		ID:          uuid.NewString(),
		OwnerID:     u.ID,
		Name:        name,
		Description: desc,
	}
	if err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&vault).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.VaultSetting{VaultID: vault.ID, ThemeName: "default", KeepDirectoryTree: true}).Error; err != nil {
			return err
		}
		return tx.Create(&models.VaultSyncState{VaultID: vault.ID}).Error
	}); err != nil {
		h.render(c, http.StatusInternalServerError, "vaults-new", h.t(c, "page.new_vault"), "vaults", "vaults-new",
			newVaultData{Name: name, Description: desc, Error: h.t(c, "err.create_vault_failed")})
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID)
}

// 仓库上下文

// resolveVaultPage 解析当前用户对仓库的权限，返回 (vault, role, ok)。
func (h *Handler) resolveVaultPage(c *gin.Context) (models.Vault, string, bool) {
	u := h.webUser(c)
	vault, role, err := vaultaccess.Resolve(h.DB, u.ID, c.Param("vault_id"))
	if err != nil {
		if u.Role == "admin" {
			if err := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err != nil {
				c.Redirect(http.StatusSeeOther, "/dashboard/vaults?error="+url.QueryEscape(h.t(c, "err.vault_not_found")))
				return models.Vault{}, "", false
			}
			role = vaultaccess.RoleAdmin
		} else {
			c.Redirect(http.StatusSeeOther, "/dashboard/vaults?error="+url.QueryEscape(h.t(c, "err.vault_not_found")))
			return models.Vault{}, "", false
		}
	}
	return vault, role, true
}

// setVaultLayout 填充侧边栏"当前仓库"上下文。
func (h *Handler) setVaultLayout(ld *layoutData, vault models.Vault) {
	themeName := "default"
	var setting models.VaultSetting
	if err := h.DB.Where("vault_id = ?", vault.ID).First(&setting).Error; err == nil {
		if setting.ThemeName != "" {
			themeName = setting.ThemeName
		}
	}
	fields, err := blog.ThemeSettings(h.Cfg.Storage.DataDir, themeName)
	ld.CurrentVault = &vaultNav{
		ID:                 vault.ID,
		Name:               vault.Name,
		HasThemeSettings:   err == nil && len(fields) > 0,
		ThemeSettingsLabel: themeSettingsLabel(themeName),
	}
}

// 仓库文件

type fileRow struct {
	Name string
	Path string
	Type string
	Size int64
}

type folderRow struct {
	Name string
	Path string
}

type breadcrumbRow struct {
	Name    string
	Path    string
	Current bool
}

type vaultFileBrowser struct {
	Folders     []folderRow
	Files       []fileRow
	Breadcrumbs []breadcrumbRow
}

type vaultFilesData struct {
	VaultID      string
	VaultName    string
	StorageUsed  int64
	StorageQuota int64
	FileCount    int
	Folders      []folderRow
	Files        []fileRow
	Breadcrumbs  []breadcrumbRow
	Error        string
}

func (h *Handler) vaultFilesPage(c *gin.Context) {
	vault, _, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	directory, valid := normalizeWebDirectory(c.Query("dir"))
	if !valid {
		c.String(http.StatusBadRequest, "invalid directory")
		return
	}
	d := vaultFilesData{VaultID: vault.ID, VaultName: vault.Name, StorageUsed: vault.StorageUsed, StorageQuota: vault.StorageQuota}
	var files []models.File
	if err := h.DB.Where("vault_id = ? AND is_deleted = ?", vault.ID, false).
		Order("path asc").Find(&files).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vault-files", h.t(c, "page.vault_files", vault.Name), "vault", "vault-files", d)
		return
	}
	browser := buildVaultFileBrowser(files, directory)
	d.Folders = browser.Folders
	d.Files = browser.Files
	d.Breadcrumbs = browser.Breadcrumbs
	d.FileCount = len(files)
	ld := layoutData{}
	h.setVaultLayout(&ld, vault)
	h.renderVault(c, ld, "vault-files", h.t(c, "page.vault_files", vault.Name), d)
}

func buildVaultFileBrowser(files []models.File, directory string) vaultFileBrowser {
	browser := vaultFileBrowser{}
	prefix := directory
	if prefix != "" {
		prefix += "/"
	}

	folders := make(map[string]folderRow)
	for _, file := range files {
		relativePath, found := strings.CutPrefix(file.Path, prefix)
		if !found || relativePath == "" {
			continue
		}
		name, _, nested := strings.Cut(relativePath, "/")
		if nested {
			folders[name] = folderRow{Name: name, Path: prefix + name}
			continue
		}
		browser.Files = append(browser.Files, fileRow{
			Name: name,
			Path: file.Path,
			Type: file.Type,
			Size: file.Size,
		})
	}

	for _, folder := range folders {
		browser.Folders = append(browser.Folders, folder)
	}
	sort.Slice(browser.Folders, func(i, j int) bool {
		return browser.Folders[i].Name < browser.Folders[j].Name
	})
	sort.Slice(browser.Files, func(i, j int) bool {
		return browser.Files[i].Name < browser.Files[j].Name
	})

	browser.Breadcrumbs = buildVaultBreadcrumbs(directory)
	return browser
}

func buildVaultBreadcrumbs(directory string) []breadcrumbRow {
	if directory == "" {
		return nil
	}
	parts := strings.Split(directory, "/")
	breadcrumbs := make([]breadcrumbRow, 0, len(parts))
	path := ""
	for index, part := range parts {
		if path == "" {
			path = part
		} else {
			path += "/" + part
		}
		breadcrumbs = append(breadcrumbs, breadcrumbRow{
			Name: part, Path: path, Current: index == len(parts)-1,
		})
	}
	return breadcrumbs
}

// renderVault 使用带"当前仓库"上下文的布局渲染。
func (h *Handler) renderVault(c *gin.Context, ld layoutData, page, title string, data any) {
	h.renderVaultStatus(c, http.StatusOK, ld, page, title, data)
}

func (h *Handler) renderVaultStatus(c *gin.Context, status int, ld layoutData, page, title string, data any) {
	u := h.webUser(c)
	ld.Page = page
	ld.Title = title
	ld.ActiveGroup = "vault"
	ld.ActivePage = page
	ld.ShowSidebar = true
	ld.Username = u.Username
	ld.IsAdmin = u.Role == "admin"
	ld.ConsoleThemeName = h.selectedConsoleTheme(u.ID)
	ld.Language = h.userLang(c)
	if token, err := c.Cookie(csrfCookie); err == nil {
		ld.CSRF = token
	}
	h.renderWithLayout(c, status, ld, data)
}

// downloadFile 预览或下载仓库文件（角色校验）。
func (h *Handler) downloadFile(c *gin.Context) {
	vault, _, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	path, valid := normalizeWebPath(c.Query("path"))
	if !valid {
		c.String(http.StatusBadRequest, "invalid path")
		return
	}
	var file models.File
	if err := h.DB.Where("user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ?",
		vault.OwnerID, vault.ID, path, false).First(&file).Error; err != nil {
		c.String(http.StatusNotFound, "file not found")
		return
	}
	diskPath := filestore.DiskPath(h.Cfg.Storage.DataDir, file)
	fh, err := os.Open(diskPath)
	if err != nil {
		c.String(http.StatusNotFound, "file content missing")
		return
	}
	defer fh.Close()
	if isTextFile(path) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
	} else {
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
	}
	c.Header("Cache-Control", "no-store")
	info, _ := fh.Stat()
	if info != nil {
		c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, fh)
}

// deleteFile 网页端删除文件：移入回收站并记录历史。
func (h *Handler) deleteFile(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"?error="+url.QueryEscape(h.t(c, "err.no_delete_file_permission")))
		return
	}
	u := h.webUser(c)
	path, valid := normalizeWebPath(c.PostForm("path"))
	if !valid {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"?error="+url.QueryEscape(h.t(c, "err.invalid_path")))
		return
	}
	if err := h.webDeleteFile(vault, u, path); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID)
}

// webDeleteFile 供网页端使用的删除服务，写入历史并把正文移入回收站。
func (h *Handler) webDeleteFile(vault models.Vault, u *models.User, path string) error {
	lock := synclock.Vault(vault.ID)
	lock.Lock()
	defer lock.Unlock()
	var file models.File
	if err := h.DB.Where("user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ?",
		vault.OwnerID, vault.ID, path, false).First(&file).Error; err != nil {
		return errors.New("文件不存在")
	}
	return h.DB.Transaction(func(tx *gorm.DB) error {
		// 快照旧正文。
		contentPath := filestore.DiskPath(h.Cfg.Storage.DataDir, file)
		if _, err := os.Stat(contentPath); err == nil {
			revision, err := nextWebRevision(tx, vault.ID)
			if err != nil {
				return err
			}
			now := time.Now()
			recycleKey, err := recycle.MoveIn(h.Cfg.Storage.DataDir, vault.ID, file.ID, contentPath)
			if err != nil {
				return err
			}
			updates := map[string]any{
				"revision":    revision,
				"is_deleted":  true,
				"deleted_at":  now,
				"storage_key": recycleKey,
				"updated_at":  now,
			}
			if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(updates).Error; err != nil {
				return err
			}
			actor := history.Actor{Username: u.Username, DeviceName: "网页控制台"}
			recyclePath := recycle.DiskPath(h.Cfg.Storage.DataDir, models.File{StorageKey: recycleKey})
			if err := history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor,
				history.ActionDelete, file.Path, "", recyclePath, revision); err != nil {
				return err
			}
		}
		return nil
	})
}

func nextWebRevision(tx *gorm.DB, vaultID string) (int64, error) {
	var state models.VaultSyncState
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("vault_id = ?", vaultID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		state = models.VaultSyncState{VaultID: vaultID}
		if err := tx.Create(&state).Error; err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	state.HeadRevision++
	if err := tx.Model(&models.VaultSyncState{}).Where("vault_id = ?", vaultID).
		Updates(map[string]any{"head_revision": state.HeadRevision, "updated_at": time.Now()}).Error; err != nil {
		return 0, err
	}
	return state.HeadRevision, nil
}

func normalizeWebPath(p string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") ||
		strings.Contains(p, "/../") || strings.Contains(p, "\x00") {
		return "", false
	}
	return p, true
}

func normalizeWebDirectory(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", true
	}
	directory, valid := normalizeWebPath(p)
	if !valid {
		return "", false
	}
	return strings.TrimSuffix(directory, "/"), true
}

func isTextFile(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".md", ".markdown", ".txt", ".css", ".js", ".json", ".yaml", ".yml", ".html", ".csv", ".xml"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// 分享管理

type shareRow struct {
	ShareID    string
	TargetPath string
	URL        string
	Views      int
	AllowCopy  bool
	CreatedAt  time.Time
}

type sharesData struct {
	VaultID string
	Shares  []shareRow
	Error   string
	Saved   bool
}

func (h *Handler) sharesPage(c *gin.Context) {
	vault, _, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	d := sharesData{VaultID: vault.ID, Error: c.Query("error"), Saved: c.Query("saved") == "1"}
	var rows []models.Share
	if err := h.DB.Where("vault_id = ?", vault.ID).Order("created_at desc").Find(&rows).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vault-shares", h.t(c, "page.shares"), "vault", "vault-shares", d)
		return
	}
	for _, r := range rows {
		d.Shares = append(d.Shares, shareRow{
			ShareID: r.ShareID, TargetPath: r.TargetPath, URL: "/p/" + r.ShareID,
			Views: r.Views, AllowCopy: r.AllowCopy, CreatedAt: r.CreatedAt,
		})
	}
	ld := layoutData{}
	h.setVaultLayout(&ld, vault)
	h.renderVault(c, ld, "vault-shares", h.t(c, "page.vault_shares", vault.Name), d)
}

func (h *Handler) createShare(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.no_share_permission")))
		return
	}
	target := strings.TrimSpace(c.PostForm("target_path"))
	if target == "" {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.path_required")))
		return
	}
	isFolder := c.PostForm("is_folder") == "on"
	allowCopy := c.PostForm("allow_copy") == "on"

	// 复用 shares 服务生成 ID 与校验。
	if _, err := h.newSharesService().CreateWeb(vault.OwnerID, vault.ID, target, isFolder, allowCopy); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?saved=1")
}

func (h *Handler) toggleShareCopy(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	next := c.PostForm("allow_copy") == "true"
	if err := h.DB.Model(&models.Share{}).
		Where("share_id = ? AND vault_id = ?", c.Param("share_id"), vault.ID).
		Update("allow_copy", next).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.update_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?saved=1")
}

func (h *Handler) deleteShare(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	if err := h.DB.Where("share_id = ? AND vault_id = ?", c.Param("share_id"), vault.ID).
		Delete(&models.Share{}).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?error="+url.QueryEscape(h.t(c, "err.unshare_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/shares?saved=1")
}

// 回收站

type recycleRow struct {
	ID        uint
	Path      string
	DeletedAt time.Time
	ExpiresAt time.Time
}

type recycleData struct {
	VaultID string
	Files   []recycleRow
	Error   string
}

func (h *Handler) recyclePage(c *gin.Context) {
	vault, _, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	d := recycleData{VaultID: vault.ID, Error: c.Query("error")}
	days, err := recycle.RetentionDays(h.DB, vault.ID)
	if err != nil {
		days = 30
	}
	var files []models.File
	if err := h.DB.Where("vault_id = ? AND is_deleted = ?", vault.ID, true).
		Order("deleted_at desc").Find(&files).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "vault-recycle", h.t(c, "page.recycle"), "vault", "vault-recycle", d)
		return
	}
	for _, f := range files {
		expires := f.DeletedAt.Time.Add(time.Duration(days) * 24 * time.Hour)
		d.Files = append(d.Files, recycleRow{
			ID: f.ID, Path: f.Path,
			DeletedAt: f.DeletedAt.Time, ExpiresAt: expires,
		})
	}
	ld := layoutData{}
	h.setVaultLayout(&ld, vault)
	h.renderVault(c, ld, "vault-recycle", h.t(c, "page.vault_recycle", vault.Name), d)
}

func (h *Handler) restoreRecycle(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	u := h.webUser(c)
	fileID, err := strconv.ParseUint(c.Param("file_id"), 10, 64)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.invalid_file")))
		return
	}
	if err := h.webRestoreRecycle(vault, u, uint(fileID)); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?saved=1")
}

func (h *Handler) webRestoreRecycle(vault models.Vault, u *models.User, fileID uint) error {
	lock := synclock.Vault(vault.ID)
	lock.Lock()
	defer lock.Unlock()
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ? AND is_deleted = ?", fileID, vault.ID, true).
		First(&file).Error; err != nil {
		return errors.New("回收站项目不存在")
	}
	return h.DB.Transaction(func(tx *gorm.DB) error {
		target := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(filestore.VaultStorageKey(vault.ID, file.Path)))
		if err := recycle.MoveOut(h.Cfg.Storage.DataDir, file, target); err != nil {
			return fmt.Errorf("恢复正文失败: %w", err)
		}
		revision, err := nextWebRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(map[string]any{
			"revision":    revision,
			"is_deleted":  false,
			"deleted_at":  nil,
			"storage_key": filestore.VaultStorageKey(vault.ID, file.Path),
			"updated_at":  now,
		}).Error; err != nil {
			return err
		}
		actor := history.Actor{Username: u.Username, DeviceName: "网页控制台"}
		return history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor,
			history.ActionRestore, file.Path, "", target, revision)
	})
}

func (h *Handler) purgeRecycle(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	fileID, err := strconv.ParseUint(c.Param("file_id"), 10, 64)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.invalid_file")))
		return
	}
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ? AND is_deleted = ?", uint(fileID), vault.ID, true).
		First(&file).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.item_not_found")))
		return
	}
	_ = recycle.Remove(h.Cfg.Storage.DataDir, file)
	if err := h.DB.Unscoped().Delete(&file).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?error="+url.QueryEscape(h.t(c, "err.permanent_delete_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/recycle?saved=1")
}

// 修改记录

func (h *Handler) restoreHistory(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/history?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	u := h.webUser(c)
	histID, err := strconv.ParseUint(c.Param("history_id"), 10, 64)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/history?error="+url.QueryEscape(h.t(c, "err.invalid_record")))
		return
	}
	if err := h.webRestoreHistory(vault, u, uint(histID)); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/history?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/history?saved=1")
}

func (h *Handler) webRestoreHistory(vault models.Vault, u *models.User, histID uint) error {
	lock := synclock.Vault(vault.ID)
	lock.Lock()
	defer lock.Unlock()
	var hist models.FileHistory
	if err := h.DB.Where("id = ? AND vault_id = ?", histID, vault.ID).First(&hist).Error; err != nil {
		return errors.New("记录不存在")
	}
	if hist.ContentKey == "" {
		return errors.New("该记录没有可恢复的快照")
	}
	content, err := history.ReadSnapshot(h.Cfg.Storage.DataDir, hist.ContentKey)
	if err != nil {
		return fmt.Errorf("读取快照失败: %w", err)
	}
	return h.DB.Transaction(func(tx *gorm.DB) error {
		var file models.File
		exists := true
		if err := tx.Where("user_id = ? AND vault_id = ? AND path = ?",
			vault.OwnerID, vault.ID, hist.FilePath).First(&file).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				exists = false
			} else {
				return err
			}
		}
		revision, err := nextWebRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		target := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(filestore.VaultStorageKey(vault.ID, hist.FilePath)))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
		now := time.Now()
		if exists {
			updates := map[string]any{
				"revision":   revision,
				"is_deleted": false,
				"deleted_at": nil,
				"hash":       fmt.Sprintf("%x", sha256sum(content)),
				"size":       len(content),
				"updated_at": now,
			}
			if file.StorageKey == "" {
				updates["storage_key"] = filestore.VaultStorageKey(vault.ID, hist.FilePath)
			}
			if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(updates).Error; err != nil {
				return err
			}
		} else {
			newFile := models.File{
				UserID: vault.OwnerID, VaultID: vault.ID, Path: hist.FilePath,
				Type: classifyWebFile(hist.FilePath), Hash: fmt.Sprintf("%x", sha256sum(content)),
				Size: int64(len(content)), Revision: revision,
				StorageKey: filestore.VaultStorageKey(vault.ID, hist.FilePath),
				MTime:      now.UnixMilli(),
			}
			if err := tx.Create(&newFile).Error; err != nil {
				return err
			}
		}
		actor := history.Actor{Username: u.Username, DeviceName: "网页控制台"}
		return history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor,
			history.ActionRestore, hist.FilePath, "", target, revision)
	})
}

func classifyWebFile(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
		return "markdown"
	}
	if strings.Contains(lower, ".obsidian") {
		return "config"
	}
	return "attachment"
}

// 仓库设置

type vaultSettingsData struct {
	VaultID                string
	VaultName              string
	ThemeName              string
	Themes                 []blog.ThemeInfo
	CustomHeader           string
	CustomFooter           string
	RecycleBinDays         int
	DefaultRecycleDays     int
	IsPublicBlog           bool
	CustomFragmentsEnabled bool
	CanManage              bool
	Error                  string
	Saved                  bool
}

func (h *Handler) vaultSettingsPage(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	d := vaultSettingsData{
		VaultID: vault.ID, VaultName: vault.Name,
		ThemeName: "default", CanManage: vaultaccess.CanManage(role),
		CustomFragmentsEnabled: settingspolicy.CustomFragmentsEnabled(h.DB),
		Error:                  c.Query("error"), Saved: c.Query("saved") == "1",
	}
	themes, err := blog.ListThemes(h.DB, h.Cfg.Storage.DataDir)
	if err != nil {
		h.render(c, http.StatusInternalServerError, "vault-settings", h.t(c, "page.vault_settings", vault.Name), "vault", "vault-settings", d)
		return
	}
	d.Themes = themes
	var setting models.VaultSetting
	if err := h.DB.Where("vault_id = ?", vault.ID).First(&setting).Error; err == nil {
		if setting.ThemeName != "" {
			d.ThemeName = setting.ThemeName
		}
		d.RecycleBinDays = setting.RecycleBinDays
		d.IsPublicBlog = setting.IsPublicBlog
		d.CustomHeader = setting.CustomHeader
		d.CustomFooter = setting.CustomFooter
	}
	defDays, err := systemDefaultRecycleDays(h.DB)
	if err == nil {
		d.DefaultRecycleDays = defDays
	} else {
		d.DefaultRecycleDays = 30
	}
	ld := layoutData{}
	h.setVaultLayout(&ld, vault)
	h.renderVault(c, ld, "vault-settings", h.t(c, "page.vault_settings", vault.Name), d)
}

func (h *Handler) saveVaultSettings(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.no_permission")))
		return
	}
	themeName := strings.TrimSpace(c.PostForm("theme_name"))
	themeExists := false
	themes, err := blog.ListThemes(h.DB, h.Cfg.Storage.DataDir)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.load_themes_failed")))
		return
	}
	for _, theme := range themes {
		if theme.Name == themeName {
			themeExists = true
			break
		}
	}
	if !themeExists {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.theme_not_found")))
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(c.PostForm("recycle_bin_days")))
	if err != nil || days < 0 || days > 3650 {
		days = 0
	}
	isPublic := c.PostForm("is_public_blog") == "on"
	customFragmentsEnabled := settingspolicy.CustomFragmentsEnabled(h.DB)
	customHeader := ""
	customFooter := ""
	if customFragmentsEnabled {
		customHeader = trimCustomHTMLFragment(c.PostForm("custom_header"))
		customFooter = trimCustomHTMLFragment(c.PostForm("custom_footer"))
	}

	var setting models.VaultSetting
	settingErr := h.DB.Where("vault_id = ?", vault.ID).First(&setting).Error
	if settingErr != nil && !errors.Is(settingErr, gorm.ErrRecordNotFound) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.save_failed")))
		return
	}
	if errors.Is(settingErr, gorm.ErrRecordNotFound) {
		setting = models.VaultSetting{
			VaultID: vault.ID, ThemeName: themeName, KeepDirectoryTree: true,
			RecycleBinDays: days, IsPublicBlog: isPublic,
			CustomHeader: customHeader, CustomFooter: customFooter,
		}
		if err := h.DB.Create(&setting).Error; err != nil {
			c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.save_failed")))
			return
		}
	} else {
		updates := map[string]any{
			"theme_name": themeName, "recycle_bin_days": days, "is_public_blog": isPublic,
		}
		if customFragmentsEnabled {
			updates["custom_header"] = customHeader
			updates["custom_footer"] = customFooter
		}
		if err := h.DB.Model(&setting).Updates(updates).Error; err != nil {
			c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.save_failed")))
			return
		}
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?saved=1")
}

func trimCustomHTMLFragment(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxCustomFragmentRunes {
		return string(runes[:maxCustomFragmentRunes])
	}
	return string(runes)
}

func systemDefaultRecycleDays(db *gorm.DB) (int, error) {
	var setting models.SystemSetting
	if err := db.Where("id = 1").First(&setting).Error; err != nil {
		return 30, err
	}
	if setting.DefaultRecycleBinDays <= 0 {
		return 30, nil
	}
	return setting.DefaultRecycleBinDays, nil
}

// 设备管理

type deviceRow struct {
	ClientID        string
	Name            string
	Status          string
	UserID          uint
	LastSeenAt      time.Time
	LastCursor      int64
	AuthorizedCount int
	AuthorizedNames []string
}

type vaultOption struct {
	ID                  string
	Name                string
	AuthorizedForClient map[string]bool
}

type devicesData struct {
	Devices   []deviceRow
	AllVaults []vaultOption
	Error     string
	Saved     bool
}

func (h *Handler) devicesPage(c *gin.Context) {
	u := h.webUser(c)
	d := devicesData{Error: c.Query("error"), Saved: c.Query("saved") == "1"}
	var devices []models.ClientDevice
	if err := h.DB.Where("user_id = ? AND status <> ?", u.ID, deviceauth.DeviceStatusRevoked).
		Order("created_at desc").Find(&devices).Error; err != nil {
		h.render(c, http.StatusInternalServerError, "devices", h.t(c, "page.devices"), "", "devices", d)
		return
	}
	// 用户可访问的全部仓库（供授权选择）。
	var owned []models.Vault
	h.DB.Where("owner_id = ?", u.ID).Find(&owned)
	var memberships []models.VaultMember
	h.DB.Where("user_id = ?", u.ID).Find(&memberships)
	for _, v := range owned {
		d.AllVaults = append(d.AllVaults, vaultOption{ID: v.ID, Name: v.Name, AuthorizedForClient: map[string]bool{}})
	}
	for _, m := range memberships {
		var v models.Vault
		if err := h.DB.Where("id = ?", m.VaultID).First(&v).Error; err == nil {
			d.AllVaults = append(d.AllVaults, vaultOption{ID: v.ID, Name: v.Name, AuthorizedForClient: map[string]bool{}})
		}
	}
	accessByClient := map[string]map[string]bool{}
	for _, dev := range devices {
		accessByClient[dev.ClientID] = map[string]bool{}
	}
	var accesses []models.DeviceVaultAccess
	if len(devices) > 0 {
		clientIDs := make([]string, 0, len(devices))
		for _, dev := range devices {
			clientIDs = append(clientIDs, dev.ClientID)
		}
		h.DB.Where("user_id = ? AND client_id IN ?", u.ID, clientIDs).Find(&accesses)
		for _, a := range accesses {
			if accessByClient[a.ClientID] != nil {
				accessByClient[a.ClientID][a.VaultID] = true
			}
		}
	}
	for _, opt := range d.AllVaults {
		opt.AuthorizedForClient = map[string]bool{}
		for clientID, m := range accessByClient {
			if m[opt.ID] {
				opt.AuthorizedForClient[clientID] = true
			}
		}
	}
	// 回填设备授权。
	byID := map[string]*vaultOption{}
	for i := range d.AllVaults {
		byID[d.AllVaults[i].ID] = &d.AllVaults[i]
	}
	for clientID, m := range accessByClient {
		for vaultID := range m {
			if opt := byID[vaultID]; opt != nil {
				if opt.AuthorizedForClient == nil {
					opt.AuthorizedForClient = map[string]bool{}
				}
				opt.AuthorizedForClient[clientID] = true
			}
		}
	}
	for _, dev := range devices {
		authCnt, authNms := deviceAuthSummary(d.AllVaults, dev.ClientID)
		row := deviceRow{
			ClientID: dev.ClientID, Name: dev.Name, Status: dev.Status,
			UserID: dev.UserID, LastSeenAt: dev.LastSeenAt,
			AuthorizedCount: authCnt, AuthorizedNames: authNms,
		}
		var dv models.DeviceVault
		if err := h.DB.Where("user_id = ? AND client_id = ?", u.ID, dev.ClientID).
			Order("last_sync_at desc").First(&dv).Error; err == nil {
			row.LastCursor = dv.LastCursor
		}
		d.Devices = append(d.Devices, row)
	}
	h.render(c, http.StatusOK, "devices", h.t(c, "page.devices"), "", "devices", d)
}

// saveDeviceAuthorization 批准设备并更新仓库授权。
func (h *Handler) saveDeviceAuthorization(
	targetUserID uint,
	clientID, name, status string,
	vaultIDs []string,
	actorID uint,
	now time.Time,
) error {
	return h.DB.Transaction(func(tx *gorm.DB) error {
		var dev models.ClientDevice
		if err := tx.Where("user_id = ? AND client_id = ?", targetUserID, clientID).
			First(&dev).Error; err != nil {
			return err
		}
		updates := map[string]any{"last_seen_at": now}
		if dev.Status == deviceauth.DeviceStatusPending && status == deviceauth.DeviceStatusApproved {
			updates["name"] = name
			updates["status"] = deviceauth.DeviceStatusApproved
			updates["approved_at"] = now
			updates["approved_by_user_id"] = actorID
		}
		if err := tx.Model(&models.ClientDevice{}).
			Where("user_id = ? AND client_id = ?", targetUserID, clientID).
			Updates(updates).Error; err != nil {
			return err
		}
		// 整体替换仓库授权。
		return deviceauth.ReplaceVaultAccesses(tx, targetUserID, clientID, vaultIDs, actorID)
	})
}

// approveDevice 仅批准设备。
func (h *Handler) approveDevice(c *gin.Context) {
	u := h.webUser(c)
	clientID := deviceauth.NormalizeClientID(c.Param("client_id"))
	if clientID == "" {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.invalid_device")))
		return
	}
	var dev models.ClientDevice
	if err := h.DB.Where("user_id = ? AND client_id = ?", u.ID, clientID).First(&dev).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.device_not_found")))
		return
	}
	now := time.Now()
	if err := h.DB.Model(&dev).Updates(map[string]any{
		"status": "approved", "approved_at": now, "approved_by_user_id": u.ID,
	}).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.approve_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/devices?saved=1")
}

// renameDevice 仅允许修改待批准设备。
func (h *Handler) renameDevice(c *gin.Context) {
	u := h.webUser(c)
	clientID := deviceauth.NormalizeClientID(c.Param("client_id"))
	name := strings.TrimSpace(c.PostForm("name"))
	if clientID == "" || name == "" || len([]rune(name)) > 128 {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.invalid_device_or_name")))
		return
	}
	result := h.DB.Model(&models.ClientDevice{}).
		Where("user_id = ? AND client_id = ? AND status = ?", u.ID, clientID, deviceauth.DeviceStatusPending).
		Update("name", name)
	if result.Error != nil || result.RowsAffected == 0 {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.rename_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/devices?saved=1")
}

// authorizeDevice 保存设备授权。
func (h *Handler) authorizeDevice(c *gin.Context) {
	u := h.webUser(c)
	clientID := deviceauth.NormalizeClientID(c.Param("client_id"))
	if clientID == "" {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.invalid_device")))
		return
	}
	var dev models.ClientDevice
	if err := h.DB.Where("user_id = ? AND client_id = ?", u.ID, clientID).First(&dev).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.device_not_found")))
		return
	}

	name := dev.Name
	if dev.Status == deviceauth.DeviceStatusPending {
		name = strings.TrimSpace(c.PostForm("name"))
		if name == "" {
			name = dev.Name
		} else if len([]rune(name)) > 128 {
			c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.device_name_length")))
			return
		}
	}

	// 待批准设备必须提交 approved 状态。
	status := strings.TrimSpace(c.PostForm("status"))
	if status == "" {
		status = dev.Status
	}
	if status != deviceauth.DeviceStatusApproved {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.invalid_device_status")))
		return
	}

	// 授权仓库必须属于当前用户。
	var wanted []string
	for _, id := range c.PostFormArray("vault_ids") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, _, err := vaultaccess.Resolve(h.DB, u.ID, id); err != nil {
			c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.inaccessible_vault")))
			return
		}
		wanted = append(wanted, id)
	}

	if err := h.saveDeviceAuthorization(u.ID, clientID, name, status, wanted, u.ID, time.Now()); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.save_authorization_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/devices?saved=1")
}

func (h *Handler) revokeDevice(c *gin.Context) {
	u := h.webUser(c)
	clientID := deviceauth.NormalizeClientID(c.Param("client_id"))
	if clientID == "" {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.invalid_device")))
		return
	}
	if err := deviceauth.RevokeAllDeviceAccesses(h.DB, u.ID, clientID); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.revoke_failed")))
		return
	}
	now := time.Now()
	if err := h.DB.Model(&models.ClientDevice{}).
		Where("user_id = ? AND client_id = ?", u.ID, clientID).
		Updates(map[string]any{"status": "revoked", "revoked_at": now}).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/devices?error="+url.QueryEscape(h.t(c, "err.revoke_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/devices?saved=1")
}

// 删除仓库

func (h *Handler) deleteVault(c *gin.Context) {
	vault, role, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	if !vaultaccess.CanDelete(role) {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape(h.t(c, "err.no_delete_permission")))
		return
	}
	if _, err := vaultbackup.Purge(h.DB, h.Cfg.Storage.DataDir, vault); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/vaults/"+vault.ID+"/settings?error="+url.QueryEscape("删除失败："+err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/vaults")
}

var _ = fmt.Sprintf
