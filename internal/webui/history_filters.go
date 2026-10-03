package webui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/history"
	"github.com/helantianshen/oss-sync/internal/models"
)

const historyFilterTimeLayout = "2006-01-02T15:04"

type historyRow struct {
	ID           uint
	FilePath     string
	PreviousPath string
	Action       string
	ActionLabel  string
	Version      int
	Username     string
	DeviceName   string
	HasSnapshot  bool
	CreatedAt    time.Time
}

type historyFilters struct {
	Action   string
	Username string
	Device   string
	From     string
	To       string
	fromTime time.Time
	toTime   time.Time
}

type historyData struct {
	VaultID  string
	FilePath string
	Filters  historyFilters
	History  []historyRow
	Error    string
	Page     Page
	PageSize int
}

func (d historyData) paginationPage() Page { return d.Page }
func (d historyData) paginationBasePath() string {
	return "/dashboard/vaults/" + d.VaultID + "/history"
}

func parseHistoryFilters(values url.Values, location *time.Location) (historyFilters, error) {
	filters := historyFilters{
		Action:   strings.TrimSpace(values.Get("action")),
		Username: strings.TrimSpace(values.Get("username")),
		Device:   strings.TrimSpace(values.Get("device")),
		From:     strings.TrimSpace(values.Get("from")),
		To:       strings.TrimSpace(values.Get("to")),
	}
	if !isHistoryAction(filters.Action) {
		return filters, errors.New("操作类型无效")
	}
	if filters.From != "" {
		parsed, err := time.ParseInLocation(historyFilterTimeLayout, filters.From, location)
		if err != nil {
			return filters, errors.New("开始时间格式无效")
		}
		filters.fromTime = parsed
	}
	if filters.To != "" {
		parsed, err := time.ParseInLocation(historyFilterTimeLayout, filters.To, location)
		if err != nil {
			return filters, errors.New("结束时间格式无效")
		}
		filters.toTime = parsed
	}
	if !filters.fromTime.IsZero() && !filters.toTime.IsZero() && filters.fromTime.After(filters.toTime) {
		return filters, errors.New("开始时间不能晚于结束时间")
	}
	return filters, nil
}

func (f historyFilters) apply(query *gorm.DB) *gorm.DB {
	if f.Action != "" {
		query = query.Where("action = ?", f.Action)
	}
	if f.Username != "" {
		query = query.Where("username = ?", f.Username)
	}
	if f.Device != "" {
		query = query.Where("device_name = ? OR client_id = ?", f.Device, f.Device)
	}
	if !f.fromTime.IsZero() {
		query = query.Where("created_at >= ?", f.fromTime)
	}
	if !f.toTime.IsZero() {
		query = query.Where("created_at <= ?", f.toTime)
	}
	return query
}

func isHistoryAction(action string) bool {
	switch action {
	case "", history.ActionCreate, history.ActionModify, history.ActionDelete, history.ActionRestore, history.ActionRename:
		return true
	default:
		return false
	}
}

func (h *Handler) historyPage(c *gin.Context) {
	vault, _, ok := h.resolveVaultPage(c)
	if !ok {
		return
	}
	filters, err := parseHistoryFilters(c.Request.URL.Query(), time.Local)
	d := historyData{VaultID: vault.ID, Filters: filters, Error: c.Query("error")}
	ld := layoutData{}
	h.setVaultLayout(&ld, vault)
	if err != nil {
		d.Error = err.Error()
		h.renderVaultStatus(c, http.StatusBadRequest, ld, "vault-history", h.t(c, "page.vault_history", vault.Name), d)
		return
	}
	if rawPath := c.Query("path"); rawPath != "" {
		filePath, valid := normalizeWebPath(rawPath)
		if !valid {
			c.String(http.StatusBadRequest, "invalid path")
			return
		}
		d.FilePath = filePath
	}
	user := h.webUser(c)
	var setting models.UserSetting
	if err := h.DB.Where("user_id = ?", user.ID).First(&setting).Error; err != nil && !errorsIsNotFound(err) {
		h.renderVaultStatus(c, http.StatusInternalServerError, ld, "vault-history", h.t(c, "page.vault_history", vault.Name), d)
		return
	}
	d.PageSize = h.userPageSize(c, &setting)
	query := h.DB.Where("vault_id = ?", vault.ID)
	if d.FilePath != "" {
		query = query.Where("file_path = ?", d.FilePath)
	}
	var total int64
	if err := filters.apply(query.Model(&models.FileHistory{})).Count(&total).Error; err != nil {
		h.renderVaultStatus(c, http.StatusInternalServerError, ld, "vault-history", h.t(c, "page.vault_history", vault.Name), d)
		return
	}
	d.Page = h.currentPage(c, total, d.PageSize)
	var rows []models.FileHistory
	if err := filters.apply(query).Order("created_at desc, id desc").
		Limit(d.PageSize).Offset(d.Page.Offset()).Find(&rows).Error; err != nil {
		h.renderVaultStatus(c, http.StatusInternalServerError, ld, "vault-history", h.t(c, "page.vault_history", vault.Name), d)
		return
	}
	for _, row := range rows {
		d.History = append(d.History, historyRow{
			ID: row.ID, FilePath: row.FilePath, PreviousPath: row.PreviousPath,
			Action: row.Action, ActionLabel: historyActionLabel(row.Action), Version: row.Version, Username: row.Username,
			DeviceName: row.DeviceName, HasSnapshot: row.ContentKey != "", CreatedAt: row.CreatedAt,
		})
	}
	h.renderVault(c, ld, "vault-history", h.t(c, "page.vault_history", vault.Name), d)
}

func historyActionLabel(action string) string {
	switch action {
	case history.ActionCreate:
		return "创建"
	case history.ActionModify:
		return "修改"
	case history.ActionDelete:
		return "删除"
	case history.ActionRestore:
		return "恢复"
	case history.ActionRename:
		return "重命名"
	default:
		return action
	}
}
