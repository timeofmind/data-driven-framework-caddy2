package app

import (
	"strings"
	"testing"
)

func TestStagingBannerText(t *testing.T) {
	if stagingBanner("") != "" || stagingLink("") != "" {
		t.Fatal("a live node shows no banner")
	}
	if b := stagingBanner("on"); !strings.HasPrefix(b, "Preview") || stagingLink("on") != "" {
		t.Fatalf("staging on: %q", b)
	}
	if stagingLink("https://example.org") != "https://example.org" {
		t.Fatal("the banner links the live site")
	}
}
