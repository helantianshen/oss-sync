package syncapi

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func TestV2UploadCompareAndSwapPreservesContent(t *testing.T) {
	f := newSyncFixture(t)
	const path = "notes/正文.md"
	const original = "# 原始正文\n"
	const updated = "# 更新正文\n第二行\n"

	first := f.upload(path, original, 0, "create")
	if first.Revision <= 0 || first.Path != path {
		t.Fatalf("initial upload metadata = %+v", first)
	}
	f.assertDownload(first, original)
	second := f.upload(path, updated, first.Revision, "update")
	if second.Revision <= first.Revision {
		t.Fatalf("updated revision = %d, must advance beyond %d", second.Revision, first.Revision)
	}
	f.assertDownload(second, updated)

	// 历史快照保存覆盖前正文，避免只验证当前文件而遗漏数据恢复能力
	before := f.history(path)
	if len(before) != 2 || !before[0].HasSnapshot || before[0].Revision != second.Revision {
		t.Fatalf("history after replacement = %+v", before)
	}
	detail := decodeResponse[historyDetailOut](t, f.request(http.MethodGet,
		f.endpoint("history/")+strconv.FormatUint(uint64(before[0].ID), 10)+"?mode=current",
		f.token, "", nil), http.StatusOK)
	if !detail.IsText || detail.Content != original {
		t.Fatalf("replacement snapshot = %+v, want original content %q", detail, original)
	}

	conflict := f.request(http.MethodPost, uploadTarget(f.vault.ID, path, first.Revision, "stale-update"),
		f.token, "application/octet-stream", []byte("must not overwrite"))
	current := decodeResponse[struct {
		Current V2FileMeta `json:"current"`
	}](t, conflict, http.StatusConflict).Current
	if current.Revision != second.Revision || current.Hash != second.Hash || current.Deleted {
		t.Fatalf("CAS conflict current = %+v, want revision %d and hash %s", current, second.Revision, second.Hash)
	}
	f.assertDownload(second, updated)

	// 同正文即使携带旧修订也应合并为空操作，不新增历史或变更游标
	same := f.upload(path, updated, first.Revision, "same-content")
	if same.Revision != second.Revision || same.MTime != second.MTime || same.Hash != second.Hash {
		t.Fatalf("identical-content upload changed metadata: got %+v, previous %+v", same, second)
	}
	after := f.history(path)
	if len(after) != len(before) {
		t.Fatalf("rejected/identical uploads changed history count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Revision != before[i].Revision {
			t.Fatalf("rejected/identical uploads changed history: before %+v, after %+v", before, after)
		}
	}
	changes := f.changes(second.Revision, 10)
	if len(changes.Files) != 0 || changes.NextCursor != second.Revision || changes.SnapshotRevision != second.Revision || changes.HasMore {
		t.Fatalf("rejected/identical uploads advanced changes: %+v", changes)
	}
	f.assertDownload(second, updated)

	// 下载携带旧修订时不得混入新正文，客户端应重新获取元数据
	staleDownload := f.request(http.MethodGet,
		f.endpoint("download")+"?path="+url.QueryEscape(path)+"&revision="+strconv.FormatInt(first.Revision, 10),
		f.token, "", nil)
	downloadConflict := decodeResponse[struct {
		Current V2FileMeta `json:"current"`
	}](t, staleDownload, http.StatusConflict)
	if downloadConflict.Current.Revision != second.Revision {
		t.Fatalf("stale download conflict = %+v, want revision %d", downloadConflict, second.Revision)
	}
}

func TestV2RenamePublishesTombstoneAndResumableChanges(t *testing.T) {
	f := newSyncFixture(t)
	const content = "# Rename me\n"
	first := f.upload("old.md", content, 0, "create")
	response := f.postJSON(f.endpoint("rename"), v2RenameRequest{
		OldPath: first.Path, NewPath: "folder/new.md", BaseRevision: first.Revision,
		TargetRevision: 0, OperationID: "rename", ClientMTime: 1700000000456,
	})
	renamed := decodeResponse[v2RenameResponse](t, response, http.StatusOK)
	if renamed.Old.Path != first.Path || !renamed.Old.Deleted || renamed.Old.Revision <= first.Revision {
		t.Fatalf("old-path tombstone = %+v, original %+v", renamed.Old, first)
	}
	if renamed.New.Path != "folder/new.md" || renamed.New.Deleted || renamed.New.Revision <= renamed.Old.Revision {
		t.Fatalf("new-path metadata = %+v, old %+v", renamed.New, renamed.Old)
	}
	f.assertDownload(renamed.New, content)
	gone := f.request(http.MethodGet, f.endpoint("download")+"?path="+url.QueryEscape(first.Path), f.token, "", nil)
	old := decodeResponse[struct {
		Current V2FileMeta `json:"current"`
	}](t, gone, http.StatusGone).Current
	if !old.Deleted || old.Revision != renamed.Old.Revision {
		t.Fatalf("old path remains downloadable or lost tombstone: %+v", old)
	}

	// 页边界必须允许客户端分别收到墓碑和新路径，断点续传不能丢失其中任一条
	page1 := f.changes(first.Revision, 1)
	if len(page1.Files) != 1 || page1.Files[0].Path != first.Path || !page1.Files[0].Deleted ||
		page1.NextCursor != renamed.Old.Revision || !page1.HasMore {
		t.Fatalf("first rename change page = %+v", page1)
	}
	page2 := f.changes(page1.NextCursor, 1)
	if len(page2.Files) != 1 || page2.Files[0].Path != renamed.New.Path || page2.Files[0].Deleted ||
		page2.Files[0].Hash != first.Hash || page2.NextCursor != renamed.New.Revision || page2.HasMore {
		t.Fatalf("second rename change page = %+v", page2)
	}
	if page1.SnapshotRevision != renamed.New.Revision || page2.SnapshotRevision != renamed.New.Revision {
		t.Fatalf("rename pages disagree with committed head: %+v, %+v", page1, page2)
	}
	caughtUp := f.changes(page2.NextCursor, 1)
	if len(caughtUp.Files) != 0 || caughtUp.NextCursor != page2.NextCursor || caughtUp.HasMore {
		t.Fatalf("caught-up cursor replays changes: %+v", caughtUp)
	}
}

func TestV2DeleteAndRecycleRestorePreserveBody(t *testing.T) {
	f := newSyncFixture(t)
	const content = "# Restore this exact body\n正文\n"
	first := f.upload("recover.md", content, 0, "create")
	deleted := decodeResponse[V2FileMeta](t, f.postJSON(f.endpoint("delete"), v2DeleteRequest{
		Path: first.Path, BaseRevision: first.Revision, OperationID: "delete", ClientMTime: 1700000000789,
	}), http.StatusOK)
	if !deleted.Deleted || deleted.Revision <= first.Revision {
		t.Fatalf("delete metadata = %+v, original %+v", deleted, first)
	}
	gone := decodeResponse[struct {
		Current V2FileMeta `json:"current"`
	}](t, f.request(http.MethodGet, f.endpoint("download")+"?path="+first.Path, f.token, "", nil), http.StatusGone)
	if !gone.Current.Deleted || gone.Current.Revision != deleted.Revision {
		t.Fatalf("deleted file download = %+v", gone)
	}
	recycleURL := "/api/vaults/" + f.vault.ID + "/recycle-bin"
	listed := decodeResponse[struct {
		Files []struct {
			ID         uint   `json:"id"`
			Path       string `json:"path"`
			CanRestore bool   `json:"can_restore"`
		} `json:"files"`
	}](t, f.request(http.MethodGet, recycleURL, f.token, "", nil), http.StatusOK)
	if len(listed.Files) != 1 || listed.Files[0].Path != first.Path || !listed.Files[0].CanRestore {
		t.Fatalf("recycle listing = %+v", listed)
	}
	restored := f.postJSON(recycleURL+"/"+strconv.FormatUint(uint64(listed.Files[0].ID), 10)+"/restore", struct{}{})
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body %s", restored.Code, restored.Body.String())
	}
	changes := f.changes(deleted.Revision, 10)
	if len(changes.Files) != 1 || changes.Files[0].Path != first.Path || changes.Files[0].Deleted ||
		changes.Files[0].Revision <= deleted.Revision || changes.NextCursor != changes.Files[0].Revision {
		t.Fatalf("restore changes = %+v", changes)
	}
	f.assertDownload(changes.Files[0], content)
	remaining := decodeResponse[struct {
		Files []any `json:"files"`
	}](t, f.request(http.MethodGet, recycleURL, f.token, "", nil), http.StatusOK)
	if len(remaining.Files) != 0 {
		t.Fatalf("restored file still listed in recycle bin: %+v", remaining)
	}
}
