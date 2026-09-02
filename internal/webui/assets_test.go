package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConsoleCSS_whenLoaded_usesSharedButtonTokens(t *testing.T) {
	t.Parallel()

	// Given
	raw, err := webFS.ReadFile("assets/console.css")
	if err != nil {
		t.Fatalf("read console CSS: %v", err)
	}
	css := string(raw)

	// Then
	wantFragments := []string{
		"--button-surface: #35519c;",
		"--button-ink: #ffffff;",
		"--button-shadow: #10182b;",
		"--button-shadow: #000000;",
		"color: var(--button-ink);",
		"background: var(--button-surface);",
		"box-shadow: 2px 2px 0 var(--button-shadow);",
		"box-shadow: 3px 3px 0 var(--button-shadow);",
	}
	for _, want := range wantFragments {
		if !strings.Contains(css, want) {
			t.Errorf("console CSS missing shared button contract %q", want)
		}
	}
	for _, token := range []string{"--cobalt: #35519c;", "--button-surface: #35519c;", "--button-ink: #ffffff;"} {
		if strings.Count(css, token) != 2 {
			t.Errorf("console CSS token %q is not identical in light and dark themes", token)
		}
	}
}

func TestServerUpdateAssets_whenLoaded_respectCSPAndHideConfirmUntilAvailable(t *testing.T) {
	t.Parallel()

	jsRaw, err := webFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app JS: %v", err)
	}
	cssRaw, err := webFS.ReadFile("assets/console.css")
	if err != nil {
		t.Fatalf("read console CSS: %v", err)
	}

	js := string(jsRaw)
	for _, want := range []string{
		"initServerUpdate();",
		"!capabilityReady || !updateAvailable",
		"triggerForm.hidden = hidden;",
		`setNote(msg("checking"))`,
		`var checkBody = new FormData();`,
		`fetch(checkForm.getAttribute("data-update-action"), { method: "POST", body: checkBody`,
		`window.confirm(msg("confirm")`,
		`body.append("download_source", sourceSelect.value)`,
		`body.append("download_proxy", customProxyInput.value.trim())`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app JS missing server update contract %q", want)
		}
	}
	if !strings.Contains(string(cssRaw), ".server-update-trigger[hidden] { display: none; }") {
		t.Error("console CSS must keep the update confirmation hidden")
	}
}

func TestConsoleCSS_whenLoaded_setsMarkdownPreviewWrappingForTextNotCode(t *testing.T) {
	t.Parallel()

	// Given
	raw, err := webFS.ReadFile("assets/console.css")
	if err != nil {
		t.Fatalf("read console CSS: %v", err)
	}
	css := string(raw)

	// Then
	wantFragments := []string{
		".markdown-preview {",
		"min-width: 0;",
		"overflow-wrap: anywhere;",
		"word-break: break-word;",
		".markdown-preview pre {",
		"white-space: pre;",
		"overflow-x: auto;",
		".markdown-preview table {",
		"display: block;",
		"overflow-x: auto;",
		".markdown-preview pre,",
		"word-break: normal;",
	}
	for _, want := range wantFragments {
		if !strings.Contains(css, want) {
			t.Errorf("console CSS missing markdown preview wrapping contract %q", want)
		}
	}
}

func TestConsoleCSS_whenLoaded_usesCompactSwitchControlGrid(t *testing.T) {
	t.Parallel()

	// Given
	raw, err := webFS.ReadFile("assets/console.css")
	if err != nil {
		t.Fatalf("read console CSS: %v", err)
	}
	css := string(raw)

	// Then
	wantFragments := []string{
		".switch-control {",
		"grid-template-columns: auto minmax(0, 1fr);",
		".switch-control > span,",
		".switch-control > small { grid-column: 2; }",
	}
	for _, want := range wantFragments {
		if !strings.Contains(css, want) {
			t.Errorf("console CSS missing switch control contract %q", want)
		}
	}
}

func TestSetPageHeaders_whenRendered_setsCSPImagePolicyWithoutRelaxingOtherDirectives(t *testing.T) {
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodGet, "/dashboard", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx.Request = req

	setPageHeaders(ctx)
	policy := w.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("missing CSP header")
	}

	if got := policy; !strings.Contains(got, "img-src 'self' data: https:") {
		t.Fatalf("unexpected img-src directive in CSP: %q", got)
	}
	if strings.Contains(policy, "img-src 'self' data: http:") {
		t.Fatalf("img-src allows insecure http: scheme: %q", policy)
	}

	for _, want := range []string{
		"default-src 'none'",
		"connect-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(policy, want) {
			t.Fatalf("CSP missing %q directive in %q", want, policy)
		}
	}
}
