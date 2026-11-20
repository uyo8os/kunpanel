package panelupdate

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubReleaseSource(t *testing.T) {
	oldClient := client
	defer func() { client = oldClient }()
	client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") != uaHeader {
			t.Errorf("missing updater User-Agent")
		}
		var body string
		switch r.URL.String() {
		case "https://api.github.com/repos/uyo8os/kunpanel/releases/latest":
			body = `{"tag_name":"v3.0.0","name":"Release 3"}`
		case "https://github.com/uyo8os/kunpanel/releases/download/v3.0.0/SHA256SUMS":
			body = strings.Repeat("a", 64) + "  ./kunpanel_v3.0.0_linux_amd64.tar.gz\n"
		default:
			t.Fatalf("unexpected URL: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	r, err := sources[0].latest()
	if err != nil || r.TagName != "v3.0.0" {
		t.Fatalf("release: %+v, %v", r, err)
	}
	sums, err := sources[0].sums(r.TagName)
	if err != nil || expectedHash(sums, "kunpanel_v3.0.0_linux_amd64.tar.gz") != strings.Repeat("a", 64) {
		t.Fatalf("checksum: %q, %v", sums, err)
	}
	if got := githubAssetURL(r.TagName, "kunpanel_v3.0.0_linux_amd64.tar.gz"); got != "https://github.com/uyo8os/kunpanel/releases/download/v3.0.0/kunpanel_v3.0.0_linux_amd64.tar.gz" {
		t.Fatalf("asset URL: %s", got)
	}
}
