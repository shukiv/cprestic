package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/protocol"
)

// A restore that writes into a live account has no transaction, and cannot
// have one: cPanel gives no way to undo a loaded database, and a home
// directory written over cannot be put back. So one database can be
// overwritten and the next fail.
//
// Before this the failure discarded everything the restore had already
// done and reported nothing but the error. The account had changed, the
// operator was told only that the restore failed, and the customer found
// out on their own.
func TestAFailedPutBackSaysWhatItAlreadyOverwrote(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{
		Databases:      map[string][]string{"c1": {"c1_shop", "c1_wp"}},
		RefuseLoadInto: "c1_wp",
	}
	agent := quietAgent(fake)

	written, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop", "c1_wp"},
		}, out)
	if err == nil {
		t.Fatal("a restore that failed halfway reported success")
	}
	if len(fake.LoadedDatabases) != 1 || fake.LoadedDatabases[0].Database != "c1_shop" {
		t.Fatalf("the fixture did not overwrite the first database: %+v", fake.LoadedDatabases)
	}

	if !strings.Contains(written, "c1_shop") {
		t.Errorf("what the restore already wrote is not reported: %q -- the "+
			"account has been changed and nothing says so", written)
	}
	if !strings.Contains(err.Error(), "c1_wp") {
		t.Errorf("the error does not name the database that failed: %v", err)
	}
	if !strings.Contains(err.Error(), "already been changed") {
		t.Errorf("the error does not say the account was changed: %v", err)
	}

	// And the customer, who is the person whose database was overwritten
	// and who is shown the hint in place of the error, is told the same.
	if !strings.Contains(hint, "c1_shop") {
		t.Errorf("the customer is not told which database was already "+
			"overwritten: %q", hint)
	}
	if strings.Contains(hint, "agent:") {
		t.Errorf("the operator's wording reached the customer: %q", hint)
	}
}

// And the report carries it, which is where an operator actually reads it.
// Applied means the live account was written into; a failure that got part
// of the way through is exactly the case it has to be true for.
func TestAFailedPutBackIsReportedAsHavingChangedTheAccount(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{
		Databases:      map[string][]string{"c1": {"c1_shop", "c1_wp"}},
		RefuseLoadInto: "c1_wp",
	}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop", "c1_wp"},
		}, out)
	if err == nil {
		t.Fatal("a restore that failed halfway reported success")
	}

	// What internal/agent/items.go does with those three values.
	report := protocol.RestoreReport{Status: "failed", Error: err.Error()}
	report.Detail = written
	report.Applied = written != ""
	if !report.Applied {
		t.Error("a restore that overwrote a live database is not reported as applied")
	}
	if report.Detail == "" {
		t.Error("the report says nothing about what was written")
	}
}

// A restore that fails before writing anything reports nothing written,
// and must not claim the account was changed.
func TestAPutBackThatFailedBeforeWritingSaysNothingWasChanged(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{
		Databases:      map[string][]string{"c1": {"c1_shop"}},
		RefuseLoadInto: "c1_shop",
	}
	agent := quietAgent(fake)

	written, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err == nil {
		t.Fatal("a refused load reported success")
	}
	if written != "" {
		t.Errorf("a restore that wrote nothing reported %q", written)
	}
	if strings.Contains(err.Error(), "already been changed") {
		t.Errorf("a restore that changed nothing says it did: %v", err)
	}
	if strings.Contains(hint, "put back before it failed") {
		t.Errorf("the customer is told part of it was written when none was: %q", hint)
	}
}
