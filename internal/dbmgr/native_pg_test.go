package dbmgr

import (
	"reflect"
	"testing"
)

func TestParsePGTableKeepsMultilineDataAndSQLNull(t *testing.T) {
	text := "id,value,absent\n1,\"a\tb\nc\",NULL_SENTINEL\n2,\"NULL\",\"\"\n"
	result, err := parsePGTable(text, "NULL_SENTINEL")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]interface{}{{"1", "a\tb\nc", nil}, {"2", "NULL", ""}}
	if !reflect.DeepEqual(result.Columns, []string{"id", "value", "absent"}) || !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("result = %#v, want %#v", result, want)
	}
}
