package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"kunpanel/internal/config"
	"kunpanel/internal/dbmgr"
	"kunpanel/internal/taskx"
)

type dbOpReq struct {
	Name string `json:"name"`
}

func (s *Server) handleDbCreate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		Name    string                `json:"name"`
		Account *dbmgr.AccountOptions `json:"account"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		Fail(w, http.StatusBadRequest, "请填写库名")
		return
	}
	if req.Account != nil {
		conn, err := dbmgr.ProvisionConnection(key, req.Name, *req.Account, true)
		sendDBConnection(w, conn, err)
		return
	}
	if err := dbmgr.CreateDatabase(key, req.Name); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}

func (s *Server) handleDbDrop(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req dbOpReq
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		Fail(w, http.StatusBadRequest, "请填写库名")
		return
	}
	if err := dbmgr.RemoveApplicationEntity(key, strings.ToLower(strings.TrimSpace(req.Name)), "", ""); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}

// handleDbImport 上传 SQL 文件(.sql/.sql.gz)导入数据库(后台任务)
func (s *Server) handleDbImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	key, database, tmp, err := receiveDBImport(r)
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	t, terr := taskx.M.Start("db-import-"+key+"-"+database, func(w io.Writer) error {
		defer os.Remove(tmp)
		_, err := dbmgr.Import(key, database, tmp)
		return err
	})
	if terr != nil {
		os.Remove(tmp)
		Fail(w, http.StatusConflict, terr.Error())
		return
	}
	OK(w, t)
}

func receiveDBImport(r *http.Request) (key, database, path string, err error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return "", "", "", fmt.Errorf("需要上传 SQL 文件: %w", err)
	}
	defer func() {
		if err != nil && path != "" {
			_ = os.Remove(path)
		}
	}()
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return key, database, path, nextErr
		}
		switch part.FormName() {
		case "key", "database":
			value, readErr := io.ReadAll(io.LimitReader(part, 257))
			if readErr != nil || len(value) > 256 {
				return key, database, path, fmt.Errorf("数据库参数无效")
			}
			if part.FormName() == "key" {
				key = string(value)
			} else {
				database = string(value)
			}
		case "file":
			if path != "" {
				return key, database, path, fmt.Errorf("每次只能导入一个 SQL 文件")
			}
			path, err = saveDBImport(part.FileName(), part)
			if err != nil {
				return key, database, path, err
			}
		}
	}
	if key == "" || database == "" || path == "" {
		return key, database, path, fmt.Errorf("缺少数据库实例、库名或 SQL 文件")
	}
	return key, database, path, nil
}

func saveDBImport(filename string, input io.Reader) (string, error) {
	name := strings.ToLower(filename)
	suffix := ".sql"
	if strings.HasSuffix(name, ".sql.gz") {
		suffix = ".sql.gz"
	} else if !strings.HasSuffix(name, suffix) {
		return "", fmt.Errorf("仅支持 .sql 或 .sql.gz 文件")
	}
	dir := filepath.Join(config.DataDir(), "backups", "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "db-import-*"+suffix)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(file, input)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("保存上传文件失败: %w", err)
	}
	return file.Name(), nil
}

// handleDbImportBackup 从面板已有备份文件一键恢复
func (s *Server) handleDbImportBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key      string `json:"key"`
		Database string `json:"database"`
		File     string `json:"file"`
	}
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	path, err := dbmgr.BackupPath(req.Key, req.File)
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	t, terr := taskx.M.Start("db-restore-"+req.Key+"-"+req.Database, func(w io.Writer) error {
		_, err := dbmgr.Import(req.Key, req.Database, path)
		return err
	})
	if terr != nil {
		Fail(w, http.StatusConflict, terr.Error())
		return
	}
	OK(w, t)
}
