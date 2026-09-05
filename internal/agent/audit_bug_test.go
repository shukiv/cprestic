package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/protocol"
)

type failSecondDatabaseLoad struct {
	*cpanel.Fake
	loads int
}

func (p *failSecondDatabaseLoad) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	p.loads++
	if p.loads == 2 {
		return errors.New("synthetic second database import failure")
	}
	return p.Fake.LoadDatabase(ctx, user, database, dumpPath)
}

// Positive audit reproduction: a passing test means the failed restore left
// one live database changed and did not report that partial mutation.
func TestAuditMultiDatabaseFailureLeavesFirstDatabaseChanged(t *testing.T) {
	out := restoredTree(t)
	provider := &failSecondDatabaseLoad{Fake: &cpanel.Fake{
		Databases: map[string][]string{"c1": {"c1_shop", "c1_wp"}},
	}}
	agent := quietAgent(provider)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{{
				Kind:  string(granular.KindDatabase),
				Names: []string{"c1_shop", "c1_wp"},
			}},
		}, out)
	if err == nil {
		t.Fatal("the synthetic second database failure was not reached")
	}
	if len(provider.LoadedDatabases) != 1 || provider.LoadedDatabases[0].Database != "c1_shop" {
		t.Fatalf("first database was not changed before failure: %+v", provider.LoadedDatabases)
	}
	if written != "" {
		t.Fatalf("partial mutation was unexpectedly disclosed as %q", written)
	}
}
