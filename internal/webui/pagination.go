package webui

import (
	"math"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/models"
)

// 控制台列表每页条数的可选值与默认值
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// AllowedPageSizes 是用户可选的每页条数，按从小到大排列
var AllowedPageSizes = []int{10, 20, 50, 100}

// Page 描述一次列表分页结果
type Page struct {
	Page       int   // 当前页码，从 1 开始
	PageSize   int   // 每页条数
	Total      int64 // 总记录数
	TotalPages int   // 总页数
}

// HasPrev 是否存在上一页
func (p Page) HasPrev() bool { return p.Page > 1 }

// HasNext 是否存在下一页
func (p Page) HasNext() bool { return p.Page < p.TotalPages }

// Offset 返回当前页在数据库查询中的偏移量
func (p Page) Offset() int { return (p.Page - 1) * p.PageSize }

// From 返回当前页起始序号，用于「第 x-y 条」展示
func (p Page) From() int {
	if p.Total == 0 {
		return 0
	}
	return p.Offset() + 1
}

// To 返回当前页结束序号
func (p Page) To() int {
	if p.Total == 0 {
		return 0
	}
	end := int64(p.Offset() + p.PageSize)
	if end > p.Total {
		end = p.Total
	}
	return int(end)
}

// PageURL 构造指向指定页码的查询地址，保留除 page/size 外的原有参数
func PageURL(base string, query url.Values, page int) string {
	next := url.Values{}
	for key, values := range query {
		if key == "page" {
			continue
		}
		next[key] = values
	}
	if page > 1 {
		next.Set("page", strconv.Itoa(page))
	}
	encoded := next.Encode()
	if encoded == "" {
		return base
	}
	return base + "?" + encoded
}

// userPageSize 返回当前请求生效的每页条数
// 查询参数 size 优先，便于临时调整；其次是用户保存的偏好，最后是默认值
func (h *Handler) userPageSize(c *gin.Context, setting *models.UserSetting) int {
	return resolvePageSize(c.Query("size"), strconv.Itoa(setting.PageSize))
}

// resolvePage 解析当前页码，越界时收敛到最后一页
func (h *Handler) currentPage(c *gin.Context, total int64, pageSize int) Page {
	totalPages := computeTotalPages(total, pageSize)
	return Page{
		Page:       resolvePage(c.Query("page"), totalPages),
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
	}
}

// normalizePageSize 将用户选择规范到允许范围，非允许值退回默认值
func normalizePageSize(size int) int {
	for _, allowed := range AllowedPageSizes {
		if size == allowed {
			return size
		}
	}
	return DefaultPageSize
}

// resolvePageSize 解析请求中的每页条数：请求非法或缺失时使用用户偏好，再退回默认值
func resolvePageSize(requested, preferred string) int {
	preferredSize := DefaultPageSize
	if parsed, err := strconv.Atoi(preferred); err == nil {
		preferredSize = normalizePageSize(parsed)
	}
	if requested == "" {
		return preferredSize
	}
	parsed, err := strconv.Atoi(requested)
	if err != nil {
		return preferredSize
	}
	for _, allowed := range AllowedPageSizes {
		if parsed == allowed {
			return parsed
		}
	}
	return preferredSize
}

// resolvePage 解析请求页码，越界或非法时收敛到第一页或最后一页
func resolvePage(requested string, totalPages int) int {
	page := 1
	if parsed, err := strconv.Atoi(requested); err == nil && parsed > 0 {
		page = parsed
	}
	if totalPages > 0 && page > totalPages {
		page = totalPages
	}
	if page < 1 {
		page = 1
	}
	return page
}

// computeTotalPages 计算总页数，至少为 1
func computeTotalPages(total int64, pageSize int) int {
	if total <= 0 || pageSize <= 0 {
		return 1
	}
	return int(math.Ceil(float64(total) / float64(pageSize)))
}
