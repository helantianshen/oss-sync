package syncapi

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/models"
)

func TestV2AccessDenialsDoNotReadOrMutateVault(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		setup  func(*syncFixture) string
	}{
		{
			name: "pending device with vault grant", status: http.StatusForbidden, code: "device_pending",
			setup: func(f *syncFixture) string {
				f.create(&models.ClientDevice{UserID: f.user.ID, ClientID: "pending-device", Status: deviceauth.DeviceStatusPending})
				f.grant(f.user.ID, "pending-device", f.vault.ID)
				return f.sign(f.user, "pending-device")
			},
		},
		{
			name: "approved device granted only another vault", status: http.StatusForbidden, code: "device_not_authorized",
			setup: func(f *syncFixture) string {
				otherVault := models.Vault{ID: "other-vault", OwnerID: f.user.ID, Name: "Other vault"}
				f.create(&otherVault)
				f.create(&models.VaultSyncState{VaultID: otherVault.ID})
				f.create(&models.ClientDevice{UserID: f.user.ID, ClientID: "other-device", Status: deviceauth.DeviceStatusApproved})
				f.grant(f.user.ID, "other-device", otherVault.ID)
				return f.sign(f.user, "other-device")
			},
		},
		{
			name: "foreign user even with device grant", status: http.StatusNotFound,
			setup: func(f *syncFixture) string {
				stranger := models.User{Username: "stranger", PasswordHash: "unused", Role: "user"}
				f.create(&stranger)
				f.create(&models.ClientDevice{UserID: stranger.ID, ClientID: "stranger-device", Status: deviceauth.DeviceStatusApproved})
				// 设备授权不能替代用户对 Vault 的成员权限
				f.grant(stranger.ID, "stranger-device", f.vault.ID)
				return f.sign(stranger, "stranger-device")
			},
		},
		{
			name: "revoked device retains vault grant", status: http.StatusForbidden, code: "device_revoked",
			setup: func(f *syncFixture) string {
				f.create(&models.ClientDevice{UserID: f.user.ID, ClientID: "revoked-device", Status: deviceauth.DeviceStatusRevoked})
				f.grant(f.user.ID, "revoked-device", f.vault.ID)
				return f.sign(f.user, "revoked-device")
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newSyncFixture(t)
			file := f.upload("private.md", "private vault content\n", 0, "create")
			token := test.setup(f)
			before := f.snapshot()
			requests := []struct {
				name        string
				method      string
				target      string
				contentType string
				body        []byte
			}{
				{"overwrite", http.MethodPost, uploadTarget(f.vault.ID, file.Path, file.Revision, "unauthorized-overwrite"), "application/octet-stream", []byte("attacker content")},
				{"create", http.MethodPost, uploadTarget(f.vault.ID, "injected.md", 0, "unauthorized-create"), "application/octet-stream", []byte("attacker content")},
				{"delete", http.MethodPost, f.endpoint("delete"), "application/json", mustJSON(t, v2DeleteRequest{Path: file.Path, BaseRevision: file.Revision, OperationID: "unauthorized-delete"})},
				{"rename", http.MethodPost, f.endpoint("rename"), "application/json", mustJSON(t, v2RenameRequest{OldPath: file.Path, NewPath: "stolen.md", BaseRevision: file.Revision, OperationID: "unauthorized-rename"})},
				{"ack", http.MethodPost, f.endpoint("ack"), "application/json", mustJSON(t, v2AckRequest{Cursor: file.Revision})},
				{"download", http.MethodGet, f.endpoint("download") + "?path=" + file.Path, "", nil},
				{"manifest", http.MethodGet, f.endpoint("manifest"), "", nil},
				{"changes", http.MethodGet, f.endpoint("changes") + "?after=0", "", nil},
				{"history", http.MethodGet, f.endpoint("history") + "?path=" + file.Path, "", nil},
				{"recycle bin", http.MethodGet, "/api/vaults/" + f.vault.ID + "/recycle-bin", "", nil},
			}
			for _, request := range requests {
				t.Run(request.name, func(t *testing.T) {
					response := f.request(request.method, request.target, token, request.contentType, request.body)
					denied := decodeResponse[struct {
						Code string `json:"code"`
					}](t, response, test.status)
					if test.code != "" && denied.Code != test.code {
						t.Fatalf("denial code = %q, want %q", denied.Code, test.code)
					}
					if after := f.snapshot(); !reflect.DeepEqual(before, after) {
						t.Fatalf("denied %s changed database or stored files", request.name)
					}
				})
			}
			f.assertDownload(file, "private vault content\n")
		})
	}
}

func TestV2DeviceIdentityCannotBeReplacedByRequestClientID(t *testing.T) {
	f := newSyncFixture(t)
	file := f.upload("private.md", "owner body\n", 0, "create")
	f.create(&models.ClientDevice{UserID: f.user.ID, ClientID: "pending-device", Status: deviceauth.DeviceStatusPending})
	token := f.sign(f.user, "pending-device")
	before := f.snapshot()
	target := uploadTarget(f.vault.ID, file.Path, file.Revision, "spoof-owner") + "&client_id=owner-device"
	denied := decodeResponse[struct {
		Code string `json:"code"`
	}](t, f.request(http.MethodPost, target, token, "application/octet-stream", []byte("forged body")), http.StatusUnauthorized)
	if denied.Code != "device_identity_mismatch" {
		t.Fatalf("spoofed device denial code = %q", denied.Code)
	}
	if after := f.snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("spoofed device changed database or stored files")
	}
	f.assertDownload(file, "owner body\n")
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return body
}

type vaultSnapshot struct {
	Files    []models.File
	History  []models.FileHistory
	States   []models.VaultSyncState
	Vaults   []models.Vault
	Devices  []models.ClientDevice
	Progress []models.DeviceVault
	Grants   []models.DeviceVaultAccess
	Disk     map[string]string
}

// 同时比较正文、快照、墓碑、配额和设备游标，防止拒绝响应掩盖已发生的副作用
func (f *syncFixture) snapshot() vaultSnapshot {
	f.t.Helper()
	var result vaultSnapshot
	for _, query := range []struct {
		value any
		order string
	}{
		{&result.Files, "id"}, {&result.History, "id"},
		{&result.States, "vault_id"}, {&result.Vaults, "id"},
		{&result.Devices, "id"}, {&result.Progress, "id"}, {&result.Grants, "id"},
	} {
		if err := f.db.Order(query.order).Find(query.value).Error; err != nil {
			f.t.Fatalf("snapshot database: %v", err)
		}
	}
	result.Disk = make(map[string]string)
	if err := filepath.WalkDir(f.cfg.Storage.DataDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(f.cfg.Storage.DataDir, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result.Disk[relative+"/"] = ""
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result.Disk[relative] = strconv.Itoa(len(body)) + ":" + string(body)
		return nil
	}); err != nil {
		f.t.Fatalf("snapshot storage: %v", err)
	}
	return result
}
