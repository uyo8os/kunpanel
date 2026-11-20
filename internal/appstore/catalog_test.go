package appstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogKeepsOnlyRequestedBuiltinApps(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	want := map[string]bool{"nginx": true, "php": true, "mysql": true, "mariadb": true, "postgresql": true, "phpmyadmin": true, "redis": true}
	for _, tpl := range Templates() {
		if !want[tpl.Key] {
			t.Errorf("unexpected application in catalog: %s", tpl.Key)
		}
		delete(want, tpl.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing catalog applications: %v", want)
	}
	if _, ok := GetTemplate("wordpress"); ok {
		t.Fatal("retired app remains available for new installations")
	}
	if _, err := Install("wordpress", nil); err == nil {
		t.Fatal("retired app should not be installable")
	}
}

func TestRetiredInstanceRemainsManageable(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	if err := saveInstances([]Instance{{ID: "1", Key: "wordpress", Name: "WordPress", Params: map[string]string{"Port": "8080", "DbPassword": "test"}}}); err != nil {
		t.Fatal(err)
	}
	if err := ReRenderCompose("wordpress"); err != nil {
		t.Fatalf("cannot rebuild retired instance config: %v", err)
	}
	if _, err := GetCompose("wordpress"); err != nil {
		t.Fatalf("cannot inspect retired instance config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(InstanceDir("wordpress"), "docker-compose.yml")); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPreservesCustomApps(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	if err := saveCustomTemplates([]Template{{Key: "custom_abc", Name: "My App", ComposeRaw: "services: {}"}}); err != nil {
		t.Fatal(err)
	}
	if tpl, ok := GetTemplate("custom_abc"); !ok || tpl.Name != "My App" {
		t.Fatal("existing custom app disappeared")
	}
}
