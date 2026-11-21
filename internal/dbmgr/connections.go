package dbmgr

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kunpanel/internal/appstore"
	"kunpanel/internal/util"
)

type AccountOptions struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Note     string `json:"note"`
}

type Connection struct {
	Database    string `json:"database"`
	User        string `json:"user"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	AccountHost string `json:"account_host,omitempty"`
	Note        string `json:"note,omitempty"`
	Password    string `json:"password,omitempty"`
}

var ErrNoConnection = errors.New("该库尚未绑定面板管理的应用账号")

func connectionInstance(key string) (appstore.Instance, int, error) {
	if !isSupported(key) {
		return appstore.Instance{}, 0, fmt.Errorf("不支持的数据库实例")
	}
	inst, found := appstore.InstanceRecord(key)
	if !found {
		return inst, 0, fmt.Errorf("未找到数据库实例的安装记录")
	}
	port, err := strconv.Atoi(inst.Params["Port"])
	if err != nil || port < 1 || port > 65535 {
		return inst, 0, fmt.Errorf("数据库实例的端口记录无效")
	}
	return inst, port, nil
}

func instanceIdentity(inst appstore.Instance) string {
	return inst.Key + "/" + inst.InstallMode + "/" + inst.ID + "/" + inst.CreatedAt
}

func recordKey(inst appstore.Instance, database string) string {
	return instanceIdentity(inst) + "/" + database
}

func (r storedConnection) connection(port int, reveal bool) Connection {
	c := Connection{Database: r.Database, User: r.User, Host: "127.0.0.1", Port: port, AccountHost: r.Host, Note: r.Note}
	if reveal {
		c.Password = r.Password
	}
	return c
}

func ListConnections(key string) ([]Connection, error) {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	inst, port, err := connectionInstance(key)
	if err != nil {
		return nil, err
	}
	records, err := readConnections()
	if err != nil {
		return nil, err
	}
	result := make([]Connection, 0)
	for _, r := range records {
		if r.Instance == instanceIdentity(inst) {
			result = append(result, r.connection(port, false))
		}
	}
	return result, nil
}

func GetConnection(key, database string) (Connection, error) {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	inst, port, err := connectionInstance(key)
	if err != nil {
		return Connection{}, err
	}
	records, err := readConnections()
	if err != nil {
		return Connection{}, err
	}
	record, ok := records[recordKey(inst, database)]
	if !ok {
		return Connection{}, ErrNoConnection
	}
	return record.connection(port, true), nil
}

func applicationPassword(password string) (string, error) {
	if password == "" {
		password = rand.Text() + "a1!"
	}
	if len(password) < 8 {
		return "", fmt.Errorf("数据库账号密码至少需要 8 位")
	}
	return password, validateUserPassword(password)
}

// Legacy containers see a host connection through the bridge. Keep that mode
// available only when the published database port is bound to loopback.
func accountHost(inst appstore.Instance, port int) (string, error) {
	if appstore.IsPostgreSQLKey(inst.Key) {
		return "", nil
	}
	if inst.InstallMode == "native" {
		return "127.0.0.1", nil
	}
	out, err := util.RunWithTimeout(5*time.Second, "docker", "port", "kun-"+inst.Key, "3306/tcp")
	if err != nil || strings.TrimSpace(out) != "127.0.0.1:"+strconv.Itoa(port) {
		return "", fmt.Errorf("容器数据库须将端口发布到 127.0.0.1 才能创建面板管理的账号")
	}
	return "%", nil
}

// Run password SQL on stdin so application and administrator passwords are
// absent from the Docker and database client argument lists.
func connectionSQL(inst appstore.Instance, database, sql, secret string) error {
	var err error
	input := strings.NewReader(sql + ";\n")
	switch {
	case inst.Key == appstore.NativePGKey:
		_, _, err = nativePGSQL(database, input, time.Minute, "--file=-")
	case inst.InstallMode == "native":
		_, _, err = nativeSQL(database, input, time.Minute)
	default:
		pwd := strings.TrimSpace(inst.Params["RootPassword"])
		if pwd == "" {
			pwd = strings.TrimSpace(inst.Params["Password"])
		}
		if pwd == "" {
			return fmt.Errorf("该实例未记录数据库管理员密码")
		}
		env, args, argErr := clientArgs(inst.Key, pwd, database, "")
		if argErr != nil {
			return argErr
		}
		args = args[:len(args)-2]
		if appstore.IsPostgreSQLKey(inst.Key) {
			args = append(args, "--set=ON_ERROR_STOP=1", "--file=-")
		} else {
			args = append(args, "--batch", "--binary-mode", "--local-infile=0")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		argv := append([]string{"exec", "-i", "--env", envKey(inst.Key), "kun-" + inst.Key}, args...)
		cmd := exec.CommandContext(ctx, "docker", argv...)
		cmd.Env = append(util.ChildEnv(), env...)
		cmd.Stdin = input
		out := &cappedOutput{limit: 8192}
		cmd.Stdout, cmd.Stderr = out, out
		if runErr := cmd.Run(); runErr != nil {
			err = fmt.Errorf("数据库操作失败: %s (%w)", strings.TrimSpace(out.String()), runErr)
		}
	}
	if err != nil && secret != "" {
		return errors.New(strings.ReplaceAll(err.Error(), secret, "[密码]"))
	}
	return err
}

// exactMySQLScope accounts for MySQL interpreting underscore as a wildcard
// in database-level grants when partial_revokes is off.
func exactMySQLScope(key, database string) (string, error) {
	if strings.Contains(database, "_") {
		out, err := Query(key, "", "SHOW VARIABLES LIKE 'partial_revokes'")
		if err != nil {
			return "", err
		}
		if !slices.Contains(strings.Fields(out), "ON") {
			database = strings.ReplaceAll(database, "_", "\\_")
		}
	}
	return "`" + database + "`.*", nil
}

func ProvisionConnection(key, database string, options AccountOptions, createDB bool) (Connection, error) {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	inst, port, err := connectionInstance(key)
	if err != nil {
		return Connection{}, err
	}
	pg := appstore.IsPostgreSQLKey(key)
	database = strings.ToLower(strings.TrimSpace(database))
	if !ValidIdentifier(database) || systemDBs[database] || pg && len(database) > 63 {
		return Connection{}, fmt.Errorf("请输入有效的业务数据库名")
	}
	username := strings.TrimSpace(options.User)
	if username == "" {
		username = database[:min(len(database), 32)]
		if username == "root" || username == "postgres" || strings.HasPrefix(username, "pg_") {
			username = "app_" + username[:min(len(username), 28)]
		}
	}
	if !dbUserNameRe.MatchString(username) || systemDBUsers[strings.ToLower(username)] ||
		strings.EqualFold(username, "root") || strings.EqualFold(username, "postgres") ||
		pg && strings.HasPrefix(strings.ToLower(username), "pg_") {
		return Connection{}, fmt.Errorf("请输入有效的应用账号，用户名为 1 到 32 位字母、数字、下划线或连字符")
	}
	password, err := applicationPassword(options.Password)
	if err != nil {
		return Connection{}, err
	}
	note := strings.TrimSpace(options.Note)
	if len([]rune(note)) > 120 || strings.IndexFunc(note, unicode.IsControl) >= 0 {
		return Connection{}, fmt.Errorf("备注最多 120 个字符")
	}
	host, err := accountHost(inst, port)
	if err != nil {
		return Connection{}, err
	}
	records, err := readConnections()
	if err != nil {
		return Connection{}, err
	}
	id := recordKey(inst, database)
	if _, exists := records[id]; exists {
		return Connection{}, fmt.Errorf("该库已有应用账号，请在连接信息中查看或修改密码")
	}
	aead, err := connectionCipher(true)
	if err != nil {
		return Connection{}, err
	}
	dbs, err := Databases(key)
	if err != nil {
		return Connection{}, err
	}
	if createDB == slices.Contains(dbs, database) {
		if createDB {
			return Connection{}, fmt.Errorf("数据库已存在，可在该库的操作菜单创建账号")
		}
		return Connection{}, fmt.Errorf("找不到要授权的数据库")
	}
	target := "\"" + username + "\""
	createSQL, dropSQL := "CREATE ROLE "+target+" LOGIN PASSWORD '"+password+"'", "DROP ROLE "+target
	var scope string
	if !pg {
		target = quoteUser(username, host)
		createSQL, dropSQL = "CREATE USER "+target+" IDENTIFIED BY '"+password+"'", "DROP USER "+target
		scope, err = exactMySQLScope(key, database)
		if err != nil {
			return Connection{}, err
		}
	}
	if err := connectionSQL(inst, "", createSQL, password); err != nil {
		return Connection{}, fmt.Errorf("创建应用账号失败，已有同名账号时请换一个名称: %w", err)
	}
	created := false
	rollback := func(cause error) (Connection, error) {
		if created {
			cause = errors.Join(cause, DropDatabase(key, database))
		} else if pg && !createDB {
			cause = errors.Join(cause, connectionSQL(inst, database, "DROP OWNED BY "+target, ""))
		}
		return Connection{}, errors.Join(cause, connectionSQL(inst, "", dropSQL, ""))
	}
	if createDB {
		if err := CreateDatabase(key, database); err != nil {
			return rollback(err)
		}
		created = true
	}
	if pg {
		if err := provisionPGGrants(inst, database, target, created); err != nil {
			return rollback(err)
		}
	} else if err := connectionSQL(inst, "", "GRANT ALL PRIVILEGES ON "+scope+" TO "+target, ""); err != nil {
		return rollback(err)
	}
	record := storedConnection{Instance: instanceIdentity(inst), Database: database, User: username, Host: host, Note: note, Password: password}
	records[id] = record
	if err := saveConnections(aead, records); err != nil {
		return rollback(err)
	}
	return record.connection(port, true), nil
}

func provisionPGGrants(inst appstore.Instance, database, role string, created bool) error {
	db := "\"" + database + "\""
	sql := "BEGIN; GRANT CONNECT, TEMPORARY ON DATABASE " + db + " TO " + role + "; "
	if created {
		sql += "REVOKE CONNECT ON DATABASE " + db + " FROM PUBLIC; "
	}
	sql += "GRANT USAGE, CREATE ON SCHEMA public TO " + role + "; " +
		"GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO " + role + "; " +
		"GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO " + role + "; COMMIT"
	return connectionSQL(inst, database, sql, "")
}

func changeConnectionPassword(inst appstore.Instance, r storedConnection, password string) error {
	var sql string
	if appstore.IsPostgreSQLKey(inst.Key) {
		sql = "ALTER ROLE \"" + r.User + "\" PASSWORD '" + password + "'"
	} else {
		sql = "ALTER USER " + quoteUser(r.User, r.Host) + " IDENTIFIED BY '" + password + "'"
	}
	return connectionSQL(inst, "", sql, password)
}

func RotateConnection(key, database, password string) (Connection, error) {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	inst, port, err := connectionInstance(key)
	if err != nil {
		return Connection{}, err
	}
	records, err := readConnections()
	if err != nil {
		return Connection{}, err
	}
	return rotateConnection(inst, port, records, recordKey(inst, database), password)
}

func rotateConnection(inst appstore.Instance, port int, records map[string]storedConnection, id, password string) (Connection, error) {
	record, ok := records[id]
	if !ok {
		return Connection{}, ErrNoConnection
	}
	password, err := applicationPassword(password)
	if err != nil {
		return Connection{}, err
	}
	aead, err := connectionCipher(false)
	if err != nil {
		return Connection{}, err
	}
	if err := changeConnectionPassword(inst, record, password); err != nil {
		return Connection{}, err
	}
	old := record.Password
	record.Password = password
	records[id] = record
	if err := saveConnections(aead, records); err != nil {
		return Connection{}, errors.Join(err, changeConnectionPassword(inst, record, old))
	}
	return record.connection(port, true), nil
}

// Keep existing account management in sync with stored connection details.
func UpdateApplicationUser(key, username, host, password string) error {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	inst, ok := appstore.InstanceRecord(key)
	if !ok {
		return SetUserPassword(key, username, host, password)
	}
	records, err := readConnections()
	if err != nil {
		return err
	}
	if host == "" {
		host = "%"
	}
	for id, r := range records {
		if r.Instance == instanceIdentity(inst) && r.User == username && r.Host == host {
			_, err := rotateConnection(inst, 0, records, id, password)
			return err
		}
	}
	return SetUserPassword(key, username, host, password)
}

func RemoveApplicationEntity(key, database, username, host string) error {
	connectionMu.Lock()
	defer connectionMu.Unlock()
	op := func() error { return DropDatabase(key, database) }
	if username != "" {
		if host == "" {
			host = "%"
		}
		op = func() error { return DropUser(key, username, host) }
	}
	inst, exists := appstore.InstanceRecord(key)
	if !exists {
		return op()
	}
	records, err := readConnections()
	if err != nil {
		return err
	}
	for id, r := range records {
		if r.Instance != instanceIdentity(inst) || (database != r.Database && (username != r.User || host != r.Host)) {
			continue
		}
		aead, err := connectionCipher(false)
		if err != nil {
			return err
		}
		delete(records, id)
		if err := saveConnections(aead, records); err != nil {
			return err
		}
		if err := op(); err != nil {
			records[id] = r
			return errors.Join(err, saveConnections(aead, records))
		}
		return nil
	}
	return op()
}
