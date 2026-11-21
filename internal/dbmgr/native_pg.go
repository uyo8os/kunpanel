package dbmgr

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativepg"
)

func nativePGPort() (int, error) {
	inst, ok := appstore.NativePGInstance()
	if !ok {
		return 0, fmt.Errorf("原生 PostgreSQL 未安装")
	}
	port, err := strconv.Atoi(inst.Params["Port"])
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("原生 PostgreSQL 端口记录无效")
	}
	return port, nil
}

func nativePGSQL(database string, input io.Reader, timeout time.Duration, options ...string) (string, bool, error) {
	port, err := nativePGPort()
	if err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, err := nativepg.Command(ctx, "psql", database, port, options...)
	if err != nil {
		return "", false, err
	}
	stdout, stderr := &cappedOutput{limit: 200 << 10}, &cappedOutput{limit: 8192}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return stdout.String(), stdout.truncated, fmt.Errorf("PostgreSQL 执行失败: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), stdout.truncated, nil
}

func nativePGTable(database, sql string) (*TableResult, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	null := "NULL_" + hex.EncodeToString(token[:])
	out, truncated, err := nativePGSQL(database, strings.NewReader(sql), time.Minute, "--quiet", "--csv", "--pset=footer=off", "--pset=null="+null)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("查询结果过长，请缩小范围")
	}
	return parsePGTable(out, null)
}

func parsePGTable(text, null string) (*TableResult, error) {
	result := &TableResult{Columns: []string{}, Rows: [][]interface{}{}}
	records, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("无法解析 PostgreSQL 查询结果，请分别执行返回数据的查询: %w", err)
	}
	if len(records) == 0 {
		return result, nil
	}
	result.Columns = records[0]
	for _, record := range records[1:] {
		row := make([]interface{}, len(record))
		for i, value := range record {
			if value != null {
				row[i] = value
			}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func nativePGBackup(key, database string) (string, error) {
	port, err := nativePGPort()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd, err := nativepg.Command(ctx, "pg_dump", database, port, "--format=plain")
	if err != nil {
		return "", err
	}
	return streamSQLBackup(key, database, cmd)
}

func nativePGImport(database, path string) (string, error) {
	if err := withSQLInput(path, func(input io.Reader) error {
		// Send COMMIT only after the input is read successfully. A gzip checksum
		// error closes stdin without committing the data already sent to psql.
		transaction := io.MultiReader(strings.NewReader("BEGIN;\n"), input, strings.NewReader("\nCOMMIT;\n"))
		_, _, err := nativePGSQL(database, transaction, 30*time.Minute, "--quiet", "--file=-")
		return err
	}); err != nil {
		return "", err
	}
	return "导入完成", nil
}
