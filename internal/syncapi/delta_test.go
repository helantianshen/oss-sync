package syncapi

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestMarkdownDeltaRoundTripAndConflict(t *testing.T) {
	f := newSyncFixture(t)
	const path = "notes/long.md"
	var note strings.Builder
	for line := 0; line < 8000; line++ {
		fmt.Fprintf(&note, "## Section %05d: 中文 Markdown with an emoji 😀\r\n", line)
	}
	base := []byte(note.String())
	target := bytes.Replace(base, []byte("Section 02342:"), []byte("Edited section:"), 1)
	first := f.upload(path, string(base), 0, "initial")
	baseQuery := "?path=" + url.QueryEscape(path) + "&revision=" + strconv.FormatInt(first.Revision, 10)
	signature := f.request(http.MethodGet, f.endpoint("delta-base")+baseQuery, f.token, "", nil)
	if signature.Code != http.StatusOK || signature.Header().Get("Content-Type") != deltaSignatureType ||
		signature.Header().Get("X-OSS-Hash") != first.Hash {
		t.Fatalf("base signature: %d %v", signature.Code, signature.Header())
	}
	patch := buildMarkdownDelta(signature.Body.Bytes(), target)
	if len(patch) == 0 || len(patch)+signature.Body.Len() >= len(target)/20 {
		t.Fatalf("expected a small upload: patch=%d signature=%d full=%d", len(patch), signature.Body.Len(), len(target))
	}
	newHash := fmt.Sprintf("%x", sha256.Sum256(target))
	uploadURL := uploadTarget(f.vault.ID, path, first.Revision, "delta") + "&base_hash=" + first.Hash + "&hash=" + newHash
	badPatch := append([]byte(nil), patch...)
	badPatch[len(badPatch)-1] ^= 1
	bad := f.request(http.MethodPost, uploadURL, f.token, deltaContentType, badPatch)
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("corrupt delta: %d %s", bad.Code, bad.Body)
	}
	f.assertDownload(first, string(base))

	second := decodeResponse[V2FileMeta](t, f.request(http.MethodPost, uploadURL, f.token, deltaContentType, patch), http.StatusOK)
	retried := decodeResponse[V2FileMeta](t, f.request(http.MethodPost, uploadURL, f.token, deltaContentType, patch), http.StatusOK)
	if retried.Revision != second.Revision || second.Revision <= first.Revision {
		t.Fatalf("unexpected revision after retry: first=%d next=%d retry=%d", first.Revision, second.Revision, retried.Revision)
	}
	f.assertDownload(second, string(target))
	staleURL := uploadTarget(f.vault.ID, path, first.Revision, "stale") + "&base_hash=" + first.Hash + "&hash=" + first.Hash
	stale := f.request(http.MethodPost, staleURL, f.token, deltaContentType, patch)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale delta: %d %s", stale.Code, stale.Body)
	}

	downloadQuery := "?path=" + url.QueryEscape(path) + "&revision=" + strconv.FormatInt(second.Revision, 10)
	download := f.request(http.MethodPost, f.endpoint("download-delta")+downloadQuery, f.token, deltaSignatureType, signature.Body.Bytes())
	if download.Code != http.StatusOK || download.Header().Get("Content-Type") != deltaContentType || download.Body.Len() >= len(target)/20 {
		t.Fatalf("expected a small download: status=%d size=%d full=%d", download.Code, download.Body.Len(), len(target))
	}
	restored, err := applyMarkdownDelta(base, download.Body.Bytes())
	if err != nil || !bytes.Equal(restored, target) {
		t.Fatalf("download delta did not restore exact target: %v", err)
	}
	badSignature := f.request(http.MethodPost, f.endpoint("download-delta")+downloadQuery, f.token, deltaSignatureType, []byte{1, 2, 3})
	if badSignature.Code != http.StatusNoContent {
		t.Fatalf("missing matching base should allow full download, status=%d", badSignature.Code)
	}
}
