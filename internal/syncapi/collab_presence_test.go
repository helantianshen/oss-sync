package syncapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/oss-sync/internal/collaboration"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/models"
)

func TestCollaborationCursorRequiresAcceptedAccess(t *testing.T) {
	f := newSyncFixture(t)
	const path = "shared.md"
	f.upload(path, "# Shared", 0, "create")
	var file models.File
	if err := f.db.Where("vault_id = ? AND path = ?", f.vault.ID, path).First(&file).Error; err != nil {
		t.Fatal(err)
	}
	peer := models.User{Username: "reader", PasswordHash: "unused", Role: "user"}
	f.create(&peer)
	f.create(&models.ClientDevice{UserID: peer.ID, ClientID: "peer-device", Status: deviceauth.DeviceStatusApproved})
	token := f.sign(peer, "peer-device")
	relation := models.Collaboration{VaultID: f.vault.ID, FileID: file.ID, OwnerID: f.user.ID, CollaboratorID: peer.ID, Status: collaboration.StatusPending}
	f.create(&relation)
	endpoint := fmt.Sprintf("/api/vaults/%s/collaborations/files/%d/presence", f.vault.ID, file.ID)
	payload := []byte(fmt.Sprintf(`{"offset":4,"hash":"%s"}`, strings.Repeat("a", 64)))
	if response := f.request(http.MethodPost, endpoint, token, "application/json", payload); response.Code != http.StatusForbidden {
		t.Fatalf("pending collaborator can submit presence: %d", response.Code)
	}
	if response := f.request(http.MethodGet, endpoint, token, "", nil); response.Code != http.StatusForbidden {
		t.Fatalf("pending collaborator can read presence: %d", response.Code)
	}
	if err := f.db.Model(&relation).Update("status", collaboration.StatusAccepted).Error; err != nil {
		t.Fatal(err)
	}
	first := f.request(http.MethodPost, endpoint, token, "application/json", payload)
	if first.Code != http.StatusOK {
		t.Fatalf("accepted peer failed to publish cursor: %d %s", first.Code, first.Body)
	}
	type response struct {
		Cursors []collaborationCursor `json:"cursors"`
	}
	owner := decodeResponse[response](t, f.request(http.MethodGet, endpoint, f.token, "", nil), http.StatusOK)
	if len(owner.Cursors) != 1 || owner.Cursors[0].Username != peer.Username || owner.Cursors[0].Offset != 4 {
		t.Fatalf("owner received wrong cursors: %+v", owner.Cursors)
	}
	passiveOwner := decodeResponse[response](t, f.request(http.MethodGet, endpoint, token, "", nil), http.StatusOK)
	if len(passiveOwner.Cursors) != 0 {
		t.Fatalf("passive reads published an owner cursor: %+v", passiveOwner.Cursors)
	}
	if err := f.db.Model(&relation).Update("status", collaboration.StatusRevoked).Error; err != nil {
		t.Fatal(err)
	}
	if response := f.request(http.MethodPost, endpoint, token, "application/json", payload); response.Code != http.StatusForbidden {
		t.Fatalf("revoked collaborator can submit presence: %d", response.Code)
	}
	if response := f.request(http.MethodGet, endpoint, token, "", nil); response.Code != http.StatusForbidden {
		t.Fatalf("revoked collaborator can read presence: %d", response.Code)
	}
}

func TestCollaborationCursorExpires(t *testing.T) {
	var presence collaborationPresence
	allowed := map[uint]bool{1: true, 2: true}
	now := time.Unix(100, 0)
	presence.exchange(123, 1, &collaborationCursor{UserID: 1}, allowed, now)
	if got := presence.exchange(123, 2, nil, allowed, now.Add(time.Second)); len(got) != 1 {
		t.Fatalf("missing active cursor: %v", got)
	}
	if got := presence.exchange(123, 2, nil, allowed, now.Add(cursorLifetime)); len(got) != 0 {
		t.Fatalf("expired cursor remains: %v", got)
	}
}
