package cpanel

import (
	"strings"
	"testing"
)

// TestTheSystemManifestCarriesWhatCannotBeRegenerated covers the gap an
// audit found: the interface offered this as what a replacement server
// needs, and it left out the things a replacement server cannot work out
// for itself.
//
// DNSSEC keys are the sharp one. A signed zone whose keys are gone has to
// be unsigned at the registrar before it resolves again -- a support
// ticket per domain, and an outage until each is done. Nothing else in a
// backup can produce them.
func TestTheSystemManifestCarriesWhatCannotBeRegenerated(t *testing.T) {
	carried := map[string]bool{}
	for _, path := range systemPaths {
		carried[path] = true
	}
	for _, want := range []string{
		"/var/cpanel/dnssec_keys", // cannot be regenerated at all
		"/etc/passwd",             // who exists on the machine
		"/etc/shadow",             //
		"/etc/group",              //
		"/var/spool/cron",         // work belonging to no account
		"/etc/localdomains",       // where mail for a domain goes
	} {
		if !carried[want] {
			t.Errorf("%s is not in the system backup, and nothing else can produce it", want)
		}
	}

	// And what it leaves out is stated rather than implied. An operator
	// who thinks this is a copy of the machine finds out during the
	// rebuild that it is not.
	if len(SystemNotCarried) == 0 {
		t.Fatal("nothing says what a system backup does not hold")
	}
	var mentionsTheKey bool
	for _, said := range SystemNotCarried {
		if strings.Contains(said, "/etc/cprest") {
			mentionsTheKey = true
		}
	}
	if !mentionsTheKey {
		t.Error("the one exclusion an operator must know about -- the key to the backups -- " +
			"is not among the things this says it leaves out")
	}
}
