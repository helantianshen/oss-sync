package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestOverviewTemplate_whenRendered_showsHomeMetricsAndDestinations(t *testing.T) {
	t.Parallel()

	tpl, err := template.New("web").Funcs(templateFuncs()).ParseFS(webFS, "templates/overview.html")
	if err != nil {
		t.Fatalf("parse overview template: %v", err)
	}
	tests := []struct {
		name          string
		language      string
		wantFragments []string
		destination   string
	}{
		{
			name:     "zh",
			language: "zh",
			wantFragments: []string{
				"<h1>首页</h1>",
				"Example CPU",
				"12.5%",
				"进程内存",
				"37.5%",
				"文件总量",
				`href="/dashboard/vaults"`,
				`href="/dashboard/devices"`,
			},
			destination: "进入",
		},
		{
			name:     "en",
			language: "en",
			wantFragments: []string{
				"<h1>Home</h1>",
				"12.5%",
			},
			destination: "Enter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := struct {
				Layout layoutData
				Data   map[string]any
			}{
				Layout: layoutData{Language: tt.language},
				Data: map[string]any{
					"VaultCount":         int64(3),
					"PendingDevices":     int64(1),
					"DeviceCount":        int64(4),
					"FileCount":          int64(28),
					"CPUCores":           8,
					"CPUModelName":       "Example CPU",
					"CPUUsagePercent":    12.5,
					"MemoryBytes":        int64(64 * 1024 * 1024),
					"MemoryTotalBytes":   int64(128 * 1024 * 1024),
					"MemoryUsagePercent": 37.5,
					"StorageUsed":        int64(64 * 1024 * 1024),
					"StorageQuota":       int64(1024 * 1024 * 1024),
					"RecentHistory":      []any{},
					"RecentDevices":      []any{},
				},
			}

			var rendered bytes.Buffer
			if err := tpl.ExecuteTemplate(&rendered, "overview", data); err != nil {
				t.Fatalf("render overview template: %v", err)
			}

			page := rendered.String()
			for _, want := range tt.wantFragments {
				if !strings.Contains(page, want) {
					t.Errorf("rendered home page missing %s", want)
				}
			}
			if strings.Count(page, ">"+tt.destination+"</a>") < 2 {
				t.Errorf("rendered home page has fewer than two destination links")
			}
		})
	}
}

func TestUsagePercent_whenInputsVary_clampsToValidRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		used  float64
		total float64
		want  float64
	}{
		{name: "returns percentage", used: 25, total: 200, want: 12.5},
		{name: "returns zero for empty total", used: 25, total: 0, want: 0},
		{name: "clamps negative usage", used: -1, total: 100, want: 0},
		{name: "clamps usage above total", used: 125, total: 100, want: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usagePercent(tt.used, tt.total)

			if got != tt.want {
				t.Fatalf("usagePercent(%v, %v) = %v, want %v", tt.used, tt.total, got, tt.want)
			}
		})
	}
}

func TestCPUUsage_whenSamplesAdvance_calculatesSystemBusyTime(t *testing.T) {
	t.Parallel()

	previous := cpuSample{total: 1_000, idle: 700, at: time.Unix(0, 0)}
	current := cpuSample{total: 1_400, idle: 900, at: time.Unix(1, 0)}

	got := cpuUsage(previous, current)

	if got <= 0 || got > 100 {
		t.Fatalf("cpuUsage() = %v, want a valid non-zero percentage", got)
	}
}
