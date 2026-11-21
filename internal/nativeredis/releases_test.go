package nativeredis

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseCatalogUsesLatestOfficialStableReleasePerBranch(t *testing.T) {
	hash := strings.Repeat("f", 64)
	var lines []string
	for _, v := range []string{"6.2.22", "7.2.9", "7.2.10", "7.4.2", "8.2.11", "8.10.3", "8.12-rc1"} {
		name := archiveName(v)
		lines = append(lines, fmt.Sprintf("hash %s sha256 %s http://download.redis.io/releases/%s", name, hash, name))
	}
	lines = append(lines,
		"hash redis-8.10.4.tar.gz sha256 short https://download.redis.io/releases/redis-8.10.4.tar.gz",
		"hash redis-8.10.5.tar.gz sha256 "+hash+" https://untrusted.example/redis-8.10.5.tar.gz")
	result, err := parseCatalog(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Release{
		{Version: "8.10.3", Branch: "8.10", digest: hash},
		{Version: "8.2.11", Branch: "8.2", digest: hash},
		{Version: "7.4.2", Branch: "7.4", digest: hash},
		{Version: "7.2.10", Branch: "7.2", digest: hash},
	}
	if !slices.Equal(result, want) {
		t.Fatalf("got %v, want %v", result, want)
	}
}

func TestCanUpgradeValidatesDirectionAndMajor(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		want            bool
	}{
		{"7.2.9", "7.2.10", true},
		{"8.8.4", "8.10.3", true},
		{"8.10.3", "8.8.4", false},
		{"7.4.1", "8.0.1", false},
		{"8.10.3", "8.10.3", false},
		{"8.10.3", "8.12.0-rc1", false},
	} {
		if got := CanUpgrade(tc.current, tc.target); got != tc.want {
			t.Errorf("CanUpgrade(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}
