package cpanel

import (
	"strings"
	"testing"
)

// pkgacctUsage is what pkgacct printed on a live cPanel 136.0.38 server,
// asked to package a name with a cPanel user file and no unix user behind
// it. It says what is wrong in its first line and then prints its whole
// usage block, so the last line is about something else entirely.
const pkgacctUsage = `Unable to get user id for user “cptktjyvhbvisx9x”
Usage:
    /usr/local/cpanel/bin/pkgacct [options] user [workdir]

      user = the user to package
      workdir = the directory to put the package

     Options:
         --help                        brief help message
         --serialized_output           encode output as JSON
         --stdout-archive              output the cpmove file to STDOUT and disable all other output
`

// TestAPkgacctRefusalSaysWhatWasRefused covers the sentence an operator
// reads in the log when a backup fails before it starts.
//
// The failure was reported as the last line of pkgacct's output. When
// pkgacct refuses, that line is the last option in its usage block, so a
// backup of an account cPanel could not resolve to a uid was reported as
// "--stdout-archive output the cpmove file to STDOUT and disable all
// other output" -- a true sentence about pkgacct and no help at all.
func TestAPkgacctRefusalSaysWhatWasRefused(t *testing.T) {
	reason := pkgacctFailure([]byte(pkgacctUsage))
	if !strings.Contains(reason, "Unable to get user id") {
		t.Errorf("the failure is reported as %q, which is not why it failed", reason)
	}
	if strings.Contains(reason, "stdout-archive") {
		t.Errorf("the last line of the usage block is reported as the failure: %q", reason)
	}
}

// A refusal that names no reason still has to report something, and the
// last line is the only thing left.
func TestAPkgacctFailureWithNothingToSayFallsBackToItsLastLine(t *testing.T) {
	reason := pkgacctFailure([]byte("something happened\nand then it stopped\n"))
	if reason != "and then it stopped" {
		t.Errorf("pkgacctFailure = %q", reason)
	}
}
