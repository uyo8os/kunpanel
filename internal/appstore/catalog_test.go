package appstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogKeepsOnlyRequestedBuiltinApps(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	want := map[string]bool{"nginx": true, "php": true, "mysql": true, "postgresql": true, "phpmyadmin": true, "redis": true}
	for _, tpl := range Templates() {
		if !want[tpl.Key] {
			t.Errorf("unexpected application in catalog: %s", tpl.Key)
		}
		delete(want, tpl.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing catalog applications: %v", want)
	}
	for _, key := range []string{"mariadb", "wordpress"} {
		if _, ok := GetTemplate(key); ok {
			t.Errorf("retired app remains available for new installations: %s", key)
		}
		if _, err := Install(key, nil); err == nil {
			t.Errorf("retired app should not be installable: %s", key)
		}
	}
}

func TestRetiredInstanceRemainsManageable(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	instances := []Instance{
		{ID: "1", Key: "wordpress", Name: "WordPress", Params: map[string]string{"Port": "8080", "DbPassword": "test"}},
		{ID: "2", Key: "mariadb", Name: "MariaDB", Params: map[string]string{"Version": "11.8", "Port": "3307", "RootPassword": "test"}},
	}
	if err := saveInstances(instances); err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		t.Run(inst.Key, func(t *testing.T) {
			if err := ReRenderCompose(inst.Key); err != nil {
				t.Fatalf("cannot rebuild retired instance config: %v", err)
			}
			if _, err := GetCompose(inst.Key); err != nil {
				t.Fatalf("cannot inspect retired instance config: %v", err)
			}
			if _, err := os.Stat(filepath.Join(InstanceDir(inst.Key), "docker-compose.yml")); err != nil {
				t.Fatal(err)
			}
		})
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
