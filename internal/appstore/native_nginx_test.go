package appstore

import "testing"

func TestNativeNginxRecordsPreserveLegacyAndUpdates(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	if err := saveInstances([]Instance{{ID: "7", Key: "nginx", Name: "Nginx"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := NativeNginxInstance(); ok || !HasNginxInstance() {
		t.Fatal("legacy Docker record must not be treated as native")
	}
	if err := RegisterNativeNginx("1.30.1", "stable"); err == nil {
		t.Fatal("native install must not replace an existing Docker record")
	}
	if err := saveInstances([]Instance{{ID: "8", Key: "redis", Name: "Redis"}}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNativeNginx("1.30.1", "stable"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNativeNginxVersion("1.31.1", "mainline"); err != nil {
		t.Fatal(err)
	}
	inst, ok := NativeNginxInstance()
	if !ok || inst.Params["Version"] != "1.31.1" || inst.Params["Channel"] != "mainline" {
		t.Fatalf("native record not updated: %+v", inst)
	}
	if err := ForgetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	list := loadInstances()
	if len(list) != 1 || list[0].Key != "redis" {
		t.Fatalf("unrelated records changed: %+v", list)
	}
}
