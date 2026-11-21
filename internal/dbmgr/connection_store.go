package dbmgr

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"kunpanel/internal/config"
	"kunpanel/internal/util"
)

// Serialize read-modify-write operations, including the SQL that they track.
var connectionMu sync.Mutex

type storedConnection struct {
	Instance string `json:"instance"`
	Database string `json:"database"`
	User     string `json:"user"`
	Host     string `json:"account_host,omitempty"`
	Note     string `json:"note,omitempty"`
	Password string `json:"password"`
}

const connectionData = "database-connections.enc"
const connectionAAD = "KunPanel database connections v1"

func connectionCipher(create bool) (cipher.AEAD, error) {
	path := filepath.Join(config.DataDir(), "database-connections.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		key = make([]byte, 32)
		rand.Read(key)
		err = util.AtomicWrite(path, key, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("数据库连接密钥无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func readConnections() (map[string]storedConnection, error) {
	data, err := os.ReadFile(filepath.Join(config.DataDir(), connectionData))
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]storedConnection), nil
	}
	if err != nil {
		return nil, err
	}
	aead, err := connectionCipher(false)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nil, data, []byte(connectionAAD))
	if err != nil {
		return nil, fmt.Errorf("无法解密数据库连接记录: %w", err)
	}
	var records map[string]storedConnection
	if err := json.Unmarshal(plain, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func saveConnections(aead cipher.AEAD, records map[string]storedConnection) error {
	plain, err := json.Marshal(records)
	if err != nil {
		return err
	}
	data := aead.Seal(nil, nil, plain, []byte(connectionAAD))
	return util.AtomicWrite(filepath.Join(config.DataDir(), connectionData), data, 0o600)
}
