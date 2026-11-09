package syncapi

import (
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/collaboration"
	"github.com/helantianshen/oss-sync/internal/models"
)

const cursorLifetime = 7 * time.Second

type collaborationCursor struct {
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
	Offset   int    `json:"offset"`
	Hash     string `json:"hash"`
	expires  time.Time
}

// Presence lives in memory and expires independently of document revisions.
type collaborationPresence struct {
	mu        sync.Mutex
	files     map[uint]map[uint]collaborationCursor
	nextSweep time.Time
}

func (p *collaborationPresence) exchange(fileID, viewerID uint, cursor *collaborationCursor, permitted map[uint]bool, now time.Time) []collaborationCursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !now.Before(p.nextSweep) {
		for id, users := range p.files {
			for user, entry := range users {
				if !now.Before(entry.expires) {
					delete(users, user)
				}
			}
			if len(users) == 0 {
				delete(p.files, id)
			}
		}
		p.nextSweep = now.Add(cursorLifetime)
	}
	users := p.files[fileID]
	if cursor != nil {
		if p.files == nil {
			p.files = make(map[uint]map[uint]collaborationCursor)
		}
		if users == nil {
			users = make(map[uint]collaborationCursor)
			p.files[fileID] = users
		}
		cursor.expires = now.Add(cursorLifetime)
		users[viewerID] = *cursor
	}
	active := make([]collaborationCursor, 0, len(users))
	for id, entry := range users {
		if !permitted[id] || !now.Before(entry.expires) {
			delete(users, id)
		} else if id != viewerID {
			active = append(active, entry)
		}
	}
	slices.SortFunc(active, func(a, b collaborationCursor) int {
		if a.UserID < b.UserID {
			return -1
		}
		if a.UserID > b.UserID {
			return 1
		}
		return 0
	})
	return active
}

func (h *Handler) CollabPresence(c *gin.Context) {
	user, _, ok := h.requireCollaborationDevice(c)
	if !ok {
		return
	}
	fileID, err := strconv.ParseUint(c.Param("file_id"), 10, 64)
	if err != nil || fileID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ? AND type = ? AND is_deleted = ?", fileID, c.Param("vault_id"), "markdown", false).First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "collaboration file not found"})
		return
	}
	var collaborators []uint
	if err := h.DB.Model(&models.Collaboration{}).Distinct("collaborator_id").
		Where("vault_id = ? AND file_id = ? AND status = ?", file.VaultID, file.ID, collaboration.StatusAccepted).
		Pluck("collaborator_id", &collaborators).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read collaboration access"})
		return
	}
	permitted := map[uint]bool{file.UserID: true}
	for _, id := range collaborators {
		permitted[id] = true
	}
	if len(collaborators) == 0 || !permitted[user.ID] {
		c.JSON(http.StatusForbidden, gin.H{"error": "collaboration is not accepted"})
		return
	}
	var cursor *collaborationCursor
	if c.Request.Method == http.MethodPost {
		var input struct {
			Offset *int   `json:"offset"`
			Hash   string `json:"hash"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512)
		if err := c.ShouldBindJSON(&input); err != nil || input.Offset == nil || *input.Offset < 0 ||
			*input.Offset > deltaMaxBytes || len(input.Hash) != 64 || !isHex(input.Hash) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid collaboration cursor"})
			return
		}
		cursor = &collaborationCursor{UserID: user.ID, Username: user.Username, Offset: *input.Offset, Hash: input.Hash}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"cursors": h.presence.exchange(file.ID, user.ID, cursor, permitted, time.Now())})
}
