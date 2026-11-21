package dbmgr

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativemysql"
)

func isNativeMySQL(key string) bool {
	_, ok := appstore.NativeMySQLInstance(key)
	return ok
}

const mysqlResultLimit = 200 << 10

type cappedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := min(len(p), b.limit-b.buffer.Len())
	_, _ = b.buffer.Write(p[:n])
	b.truncated = b.truncated || n != len(p)
	return len(p), nil
}

func (b *cappedOutput) String() string { return b.buffer.String() }

func nativeSQL(database string, input io.Reader, timeout time.Duration) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"--batch", "--binary-mode", "--local-infile=0", "--default-character-set=utf8mb4"}
	if database != "" {
		args = append(args, "--database="+database)
	}
	cmd, err := nativemysql.Command(ctx, "mysql", args...)
	if err != nil {
		return "", false, err
	}
	stdout, stderr := &cappedOutput{limit: mysqlResultLimit}, &cappedOutput{limit: 8192}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return stdout.String(), stdout.truncated, fmt.Errorf("MySQL 执行失败: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), stdout.truncated, nil
}

var mysqlBatchEscapes = strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\t`, "\t", `\0`, "\x00")

func nativeMySQLBackup(key, database string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd, err := nativemysql.Command(ctx, "mysqldump", "--default-character-set=utf8mb4", "--single-transaction", "--quick", "--no-tablespaces", "--set-gtid-purged=OFF", "--routines", "--events", "--triggers", "--", database)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupsDir(), 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(backupsDir(), ".mysql-backup-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	zip := gzip.NewWriter(file)
	stderr := &cappedOutput{limit: 8192}
	cmd.Stdout, cmd.Stderr = zip, stderr
	dumpErr := cmd.Run()
	if err := errors.Join(dumpErr, zip.Close(), file.Close()); err != nil {
		return "", fmt.Errorf("MySQL 导出失败: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	name := BackupPrefix(key, database) + time.Now().Format("20060102_150405.000000000") + ".sql.gz"
	path := filepath.Join(backupsDir(), name)
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func nativeMySQLImport(database, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var input io.Reader = file
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		zip, err := gzip.NewReader(file)
		if err != nil {
			return "", fmt.Errorf("打开 gzip SQL 文件失败: %w", err)
		}
		defer zip.Close()
		input = zip
	}
	if _, _, err := nativeSQL(database, input, 30*time.Minute); err != nil {
		return "", err
	}
	return "导入完成", nil
}
