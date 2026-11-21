package nativemysql

import (
	"errors"
	"testing"
	"time"
)

func TestVersionFromNotesUsesLatestPublishedPatch(t *testing.T) {
	notes := []byte(`
Changes in MySQL 9.7.10 (Not yet released)
Changes in MySQL 9.7.9 (2027-01-19)
Changes in MySQL 9.7.4 (2026-10-20)
Changes in MySQL 9.7.4.1 (2026-10-21)
Changes in MySQL 9.7.3 (2026-08-18)
Changes in MySQL 8.4.13 (2026-10-20)`)
	got, err := versionFromNotes("9.7", notes, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err != nil || got != "9.7.4.1" {
		t.Fatalf("versionFromNotes = %q, %v", got, err)
	}
}

func TestSignatureRequiresPinnedKeyAndNoVerificationFailures(t *testing.T) {
	valid := "[GNUPG:] VALIDSIG " + releaseKey
	if !signatureOK(valid, releaseKey, false, nil) || signatureOK(valid, archivedKey, true, nil) {
		t.Fatal("only the pinned fingerprint may be accepted")
	}
	if signatureOK(valid+"\n[GNUPG:] BADSIG", releaseKey, false, nil) {
		t.Fatal("bad signature must be rejected")
	}
	expired := "[GNUPG:] EXPKEYSIG\n[GNUPG:] VALIDSIG " + archivedKey
	if !signatureOK(expired, archivedKey, true, errors.New("expired")) || signatureOK(expired, archivedKey, false, nil) {
		t.Fatal("expired keys are acceptable only for a valid historical signature")
	}
}
