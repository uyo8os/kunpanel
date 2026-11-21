package server

import (
	"errors"
	"net/http"

	"kunpanel/internal/dbmgr"
)

func sendDBConnection(w http.ResponseWriter, conn dbmgr.Connection, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, dbmgr.ErrNoConnection):
		Fail(w, http.StatusNotFound, err.Error())
	case err != nil:
		Fail(w, http.StatusBadRequest, err.Error())
	default:
		OK(w, conn)
	}
}

func (s *Server) handleDbConnections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := dbmgr.ListConnections(r.PathValue("key"))
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, result)
}

func (s *Server) handleDbConnection(w http.ResponseWriter, r *http.Request) {
	key, database := r.PathValue("key"), r.PathValue("database")
	if r.Method == http.MethodGet {
		conn, err := dbmgr.GetConnection(key, database)
		sendDBConnection(w, conn, err)
		return
	}
	var req dbmgr.AccountOptions
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "账号参数错误")
		return
	}
	conn, err := dbmgr.ProvisionConnection(key, database, req, false)
	sendDBConnection(w, conn, err)
}

func (s *Server) handleDbConnectionPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "密码参数错误")
		return
	}
	conn, err := dbmgr.RotateConnection(r.PathValue("key"), r.PathValue("database"), req.Password)
	sendDBConnection(w, conn, err)
}
