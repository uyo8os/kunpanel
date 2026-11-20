package appstore

import "testing"

func TestNativePHPRecordsPreserveLegacyInstances(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	if err := saveInstances([]Instance{{ID: "1", Key: "php", Name: "Legacy PHP", Params: map[string]string{"Version": "8.4"}}}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNativePHP("8.4", "8.4.1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNativePHP("8.5", "8.5.1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNativePHP("8.4", "8.4.2"); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if err := UpdateNativePHPVersion("php84", "8.4.2"); err != nil {
		t.Fatal(err)
	}
	inst, ok := NativePHPInstance("php84")
	if !ok || inst.Params["Minor"] != "8.4" || inst.Params["Version"] != "8.4.2" {
		t.Fatalf("incorrect native record: %+v", inst)
	}
	if err := ForgetNativePHP("php84"); err != nil {
		t.Fatal(err)
	}
	list := loadInstances()
	if len(list) != 2 || list[0].Key != "php" || list[1].Key != "php85" {
		t.Fatalf("other instances changed: %+v", list)
	}
}
