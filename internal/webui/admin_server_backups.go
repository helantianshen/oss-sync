package webui

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/serverbackup"
)

const serverBackupPagePath = "/dashboard/admin/server-backups"

type serverBackupPageData struct {
	Archives []serverbackup.Archive
	Mirrors  []serverbackup.Mirror
	Saved    bool
	Error    string
}

func (h *Handler) serverBackupPage(c *gin.Context) {
	h.renderServerBackups(c, nil)
}

func (h *Handler) renderServerBackups(c *gin.Context, actionErr error) {
	c.Header("Cache-Control", "no-store")
	archives, archiveErr := h.backups.List()
	mirrors, mirrorErr := h.backups.Mirrors()
	data := serverBackupPageData{Archives: archives, Mirrors: mirrors, Saved: c.Query("saved") == "1"}
	status := http.StatusOK
	if err := errors.Join(actionErr, archiveErr, mirrorErr); err != nil {
		data.Error = err.Error()
		data.Saved = false
		status = http.StatusBadRequest
	}
	h.render(c, status, "admin-server-backups", h.t(c, "backup.title"), "admin", "admin-server-backups", data)
}

func (h *Handler) backupResult(c *gin.Context, err error) {
	if err != nil {
		h.renderServerBackups(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, serverBackupPagePath+"?saved=1")
}

func (h *Handler) createServerBackup(c *gin.Context) {
	_, err := h.backups.Create(c.Request.Context(), serverbackup.Kind(c.PostForm("kind")))
	h.backupResult(c, err)
}

func (h *Handler) importServerBackup(c *gin.Context) {
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	header, err := c.FormFile("archive")
	if err != nil {
		h.backupResult(c, errors.New(h.t(c, "backup.invalid_upload")))
		return
	}
	file, err := header.Open()
	if err != nil {
		h.backupResult(c, err)
		return
	}
	defer file.Close()
	_, err = h.backups.Import(file)
	h.backupResult(c, err)
}

func (h *Handler) exportServerBackup(c *gin.Context) {
	file, archive, err := h.backups.Open(c.Param("id"))
	if errors.Is(err, fs.ErrNotExist) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		h.renderServerBackups(c, err)
		return
	}
	defer file.Close()
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", `attachment; filename="`+archive.Name()+`"`)
	http.ServeContent(c.Writer, c.Request, archive.Name(), archive.CreatedAt, file)
}

func (h *Handler) restoreServerBackup(c *gin.Context) {
	h.backupResult(c, h.backups.Restore(c.Request.Context(), c.Param("id")))
}

func (h *Handler) deleteServerBackup(c *gin.Context) {
	h.backupResult(c, h.backups.Delete(c.Param("id")))
}

func (h *Handler) addServerMirror(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	h.backupResult(c, h.backups.AddMirror(ctx, serverbackup.Mirror{
		Name: c.PostForm("name"), Protocol: c.PostForm("protocol"), Address: c.PostForm("address"),
		Username: c.PostForm("username"), Password: c.PostForm("password"),
	}))
}

func (h *Handler) syncServerMirror(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	h.backupResult(c, h.backups.SyncMirror(ctx, c.Param("id")))
}

func (h *Handler) deleteServerMirror(c *gin.Context) {
	h.backupResult(c, h.backups.DeleteMirror(c.Param("id")))
}
