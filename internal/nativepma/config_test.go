package nativepma

import (
	"strings"
	"testing"
)

func TestDatabaseSelectionRetainsServerIdentifiers(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	s := Settings{Host: "panel.example.test", Port: 8082, PHP: "8.4"}
	native := Database{Key: "mysql-native", Name: "Native MySQL", Port: 3308}
	maria := Database{Key: "mariadb", Name: "MariaDB", Port: 3307}
	for _, dbs := range [][]Database{{native, maria}, {maria}} {
		contents := phpConfig(s, dbs, make([]byte, 32))
		for _, want := range []string{
			"$cfg['Servers'][3]['port'] = 3307;",
			"$cfg['Servers'][3]['auth_type'] = 'cookie';",
			"$cfg['AllowArbitraryServer'] = false;",
		} {
			if !strings.Contains(contents, want) {
				t.Fatalf("generated config missing %q", want)
			}
		}
		if len(dbs) == 1 && !strings.Contains(contents, "$cfg['ServerDefault'] = 3;") {
			t.Fatal("MariaDB was not selected as the default after removing MySQL")
		}
	}
}
