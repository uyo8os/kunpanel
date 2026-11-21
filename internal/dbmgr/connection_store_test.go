package dbmgr

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kunpanel/internal/appstore"
)

func TestConnectionStoreProtectsPasswords(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	if err := appstore.RegisterNativeMySQL(3308); err != nil {
		t.Fatal(err)
	}
	inst, ok := appstore.InstanceRecord("mysql")
	if !ok {
		t.Fatal("missing instance record")
	}
	record := storedConnection{
		Instance: instanceIdentity(inst), Database: "sample", User: "sample",
		Host: "127.0.0.1", Note: "website", Password: "SecretFixturePassword123!",
	}
	aead, err := connectionCipher(true)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]storedConnection{recordKey(inst, record.Database): record}
	if err := saveConnections(aead, records); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, connectionData)
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte(record.Password)) {
		t.Fatalf("invalid encrypted connection file: %v", err)
	}
	list, err := ListConnections(inst.Key)
	if err != nil || len(list) != 1 || list[0].User != record.User {
		t.Fatalf("unexpected list: %v", err)
	}
	public, err := json.Marshal(list)
	if err != nil || bytes.Contains(public, []byte("password")) {
		t.Fatalf("connection list exposes password data: %v", err)
	}
	one, err := GetConnection(inst.Key, record.Database)
	if err != nil || one.Password != record.Password || one.Port != 3308 {
		t.Fatalf("cannot retrieve connection: %v", err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConnections(); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
