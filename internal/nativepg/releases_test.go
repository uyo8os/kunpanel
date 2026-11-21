package nativepg

import (
	"reflect"
	"testing"
	"time"
)

func TestParseVersionsUsesMaintainedPublishedBranches(t *testing.T) {
	data := []byte(`[
		{"major":"14","latestMinor":"25","relDate":"2026-11-12","supported":false},
		{"major":"18","latestMinor":"7","relDate":"2026-11-12","supported":true},
		{"major":"19","latestMinor":"0","relDate":"2026-11-19","supported":true},
		{"major":"20","latestMinor":"0","relDate":"2027-11-19","supported":true},
		{"major":"20","latestMinor":"rc1","relDate":"2026-11-18","supported":true}
	]`)
	got, err := parseVersions(data, time.Date(2026, time.November, 21, 0, 0, 0, 0, time.UTC))
	want := []Release{{Major: "19", Version: "19.0"}, {Major: "18", Version: "18.7"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parseVersions = %#v, %v, want %#v", got, err, want)
	}
}

func TestDatabaseOptionQuotesLibpqOptions(t *testing.T) {
	name := `name'\ host=example.invalid`
	want := `--dbname=dbname='name\'\\ host=example.invalid'`
	if got := databaseOption(name); got != want {
		t.Fatalf("databaseOption = %q, want %q", got, want)
	}
}
