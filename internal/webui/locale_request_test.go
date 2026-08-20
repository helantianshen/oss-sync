package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func requestedWebLanguageWithHeader(t *testing.T, accept string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		req.Header.Set("Accept-Language", accept)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return requestedWebLanguage(c)
}

func TestRequestedWebLanguage_whenHeaderVaries_selectsExpectedLanguage(t *testing.T) {
	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{name: "no header falls back", accept: "", want: ""},
		{name: "simple chinese", accept: "zh", want: "zh"},
		{name: "simple english", accept: "en", want: "en"},
		{name: "region subtag zh-CN", accept: "zh-CN", want: "zh"},
		{name: "region subtag en-US", accept: "en-US", want: "en"},
		{name: "uppercase tags", accept: "ZH-CN", want: "zh"},
		{name: "comma list picks first supported", accept: "fr, en", want: "en"},
		{name: "q values pick highest", accept: "en;q=0.2, zh;q=0.9", want: "zh"},
		{name: "q=0 excluded even when first", accept: "en;q=0, zh;q=0.9", want: "zh"},
		{name: "all q=0 falls back", accept: "en;q=0, zh;q=0", want: ""},
		{name: "unsupported only falls back", accept: "fr-FR;q=1.0", want: ""},
		{name: "whitespace tolerated", accept: "  en ; q=0.8 , zh-CN ;q=0.5 ", want: "en"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requestedWebLanguageWithHeader(t, tt.accept)
			if got != tt.want {
				t.Fatalf("requestedWebLanguage(%q) = %q, want %q", tt.accept, got, tt.want)
			}
		})
	}
}
