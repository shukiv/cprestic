package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveCertificationProducesAuditReport(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runLiveCertification(context.Background(), config{
		certifyArchive: archive, certifyUser: "cprv1234",
		certifyIsolatedHost: true, fakeRoot: filepath.Join(root, "cpanel"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.FinishedAt.IsZero() || len(report.Checks) != 4 {
		t.Fatalf("incomplete certification report: %+v", report)
	}
}

func TestLiveCertificationReportRecordsRefusedUnsafeRun(t *testing.T) {
	report, err := runLiveCertification(context.Background(), config{
		certifyArchive: "/tmp/archive.tar", certifyUser: "cprv1234",
	})
	if err == nil || report.Passed || report.Error == "" || report.FinishedAt.IsZero() {
		t.Fatalf("unsafe certification was not recorded as failed: %+v, %v", report, err)
	}
}
