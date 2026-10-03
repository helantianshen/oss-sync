package webui

import (
	"net/url"
	"testing"
)

func TestResolvePageSize(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		preferred string
		want      int
	}{
		{"request wins when allowed", "50", "10", 50},
		{"request ignored when empty", "", "100", 100},
		{"preferred used when request missing", "", "10", 10},
		{"default when both empty", "", "", DefaultPageSize},
		{"disallowed request falls back to preferred", "7", "50", 50},
		{"disallowed preferred falls back to default", "", "999", DefaultPageSize},
		{"garbage falls back to default", "abc", "abc", DefaultPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePageSize(tc.requested, tc.preferred); got != tc.want {
				t.Fatalf("resolvePageSize(%q, %q) = %d, want %d", tc.requested, tc.preferred, got, tc.want)
			}
		})
	}
}

func TestResolvePageClampsOutOfRange(t *testing.T) {
	cases := []struct {
		requested  string
		totalPages int
		want       int
	}{
		{"", 5, 1},
		{"1", 5, 1},
		{"3", 5, 3},
		{"9", 5, 5},
		{"0", 5, 1},
		{"-2", 5, 1},
		{"abc", 5, 1},
		{"3", 0, 3},
	}
	for _, tc := range cases {
		if got := resolvePage(tc.requested, tc.totalPages); got != tc.want {
			t.Errorf("resolvePage(%q, %d) = %d, want %d", tc.requested, tc.totalPages, got, tc.want)
		}
	}
}

func TestComputeTotalPages(t *testing.T) {
	cases := []struct {
		total    int64
		pageSize int
		want     int
	}{
		{0, 20, 1},
		{1, 20, 1},
		{20, 20, 1},
		{21, 20, 2},
		{95, 10, 10},
		{100, 10, 10},
		{101, 10, 11},
	}
	for _, tc := range cases {
		if got := computeTotalPages(tc.total, tc.pageSize); got != tc.want {
			t.Errorf("computeTotalPages(%d, %d) = %d, want %d", tc.total, tc.pageSize, got, tc.want)
		}
	}
}

func TestPageRangeAndFlags(t *testing.T) {
	page := Page{Page: 2, PageSize: 20, Total: 95, TotalPages: 5}
	if !page.HasPrev() || !page.HasNext() {
		t.Fatalf("middle page flags = prev %v next %v, want both true", page.HasPrev(), page.HasNext())
	}
	if page.Offset() != 20 || page.From() != 21 || page.To() != 40 {
		t.Fatalf("page range = offset %d from %d to %d", page.Offset(), page.From(), page.To())
	}

	first := Page{Page: 1, PageSize: 20, Total: 95, TotalPages: 5}
	if first.HasPrev() {
		t.Error("first page must not have prev")
	}

	last := Page{Page: 5, PageSize: 20, Total: 95, TotalPages: 5}
	if last.HasNext() {
		t.Error("last page must not have next")
	}
	if last.To() != 95 {
		t.Fatalf("last page To() = %d, want 95", last.To())
	}

	empty := Page{Page: 1, PageSize: 20, Total: 0, TotalPages: 1}
	if empty.From() != 0 || empty.To() != 0 {
		t.Fatalf("empty page range = from %d to %d, want 0 to 0", empty.From(), empty.To())
	}
}

func TestPageURLPreservesFilters(t *testing.T) {
	query := url.Values{}
	query.Set("status", "pending")
	query.Set("q", "vault")
	query.Set("page", "4")
	query.Set("size", "100")

	got := PageURL("/dashboard/admin/devices", query, 7)
	want := "/dashboard/admin/devices?page=7&q=vault&size=100&status=pending"
	if got != want {
		t.Fatalf("PageURL() = %q, want %q", got, want)
	}

	if first := PageURL("/dashboard/admin/devices", query, 1); first != "/dashboard/admin/devices?q=vault&size=100&status=pending" {
		t.Fatalf("PageURL(page 1) = %q, want no page param", first)
	}

	if bare := PageURL("/dashboard/admin/devices", url.Values{}, 1); bare != "/dashboard/admin/devices" {
		t.Fatalf("PageURL(empty query) = %q, want bare base", bare)
	}
}
