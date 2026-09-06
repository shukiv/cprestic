package destination

import (
	"strings"
	"testing"
)

func TestBuildEachType(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "local",
			spec: Spec{Type: TypeLocal, Config: map[string]string{"root": "/srv/backups"}},
			want: "/srv/backups/cp01",
		},
		{
			name: "sftp",
			spec: Spec{Type: TypeSFTP, Config: map[string]string{
				"host": "backup.example.com", "port": "2222", "user": "cpbackup",
				"root": "/backup", "identity_file": "/etc/gniza/id",
				"known_hosts_file": "/etc/gniza/known_hosts",
			}},
			want: "sftp://cpbackup@backup.example.com:2222//backup/cp01",
		},
		{
			name: "rest",
			spec: Spec{
				Type:    TypeREST,
				Config:  map[string]string{"base_url": "https://backup.example.com", "append_only": "true"},
				Secrets: map[string]string{"username": "cp01", "password": "p"},
			},
			want: "rest:https://backup.example.com/cp01/",
		},
		{
			name: "s3",
			spec: Spec{
				Type:    TypeS3,
				Config:  map[string]string{"endpoint": "s3.us-east-1.wasabisys.com", "bucket": "cp-backups"},
				Secrets: map[string]string{"access_key_id": "AK", "secret_access_key": "SK"},
			},
			want: "s3:https://s3.us-east-1.wasabisys.com/cp-backups/cp01",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, err := Build(tc.spec)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			uri, err := dest.URI("cp01")
			if err != nil {
				t.Fatalf("URI: %v", err)
			}
			if uri != tc.want {
				t.Errorf("URI = %q, want %q", uri, tc.want)
			}
		})
	}
}

func TestBuildReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Build(Spec{Type: TypeSFTP, Config: map[string]string{"host": "h"}})
	if err == nil {
		t.Fatal("an sftp destination missing four required keys was accepted")
	}
	for _, want := range []string{"user", "root", "identity_file", "known_hosts_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention missing %q", err, want)
		}
	}
}

func TestBuildRejectsUnknownConfigKey(t *testing.T) {
	// A silently ignored "endpoint" typo would send backups to the wrong
	// provider, so unknown keys are an error rather than a shrug.
	_, err := Build(Spec{Type: TypeLocal, Config: map[string]string{
		"root": "/srv/b", "endpiont": "typo",
	}})
	if err == nil || !strings.Contains(err.Error(), "endpiont") {
		t.Fatalf("err = %v, want a complaint about the unknown key", err)
	}
}

func TestBuildRejectsUnsupportedType(t *testing.T) {
	if _, err := Build(Spec{Type: "azure"}); err == nil {
		t.Error("an unimplemented destination type should be rejected")
	}
}

func TestBuildRejectsMalformedNumbersAndBooleans(t *testing.T) {
	_, err := Build(Spec{Type: TypeSFTP, Config: map[string]string{
		"host": "h", "user": "u", "root": "/b", "port": "not-a-number",
		"identity_file": "/k", "known_hosts_file": "/kh",
	}})
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want a complaint about port", err)
	}

	_, err = Build(Spec{
		Type:    TypeREST,
		Config:  map[string]string{"base_url": "https://b", "append_only": "maybe"},
		Secrets: map[string]string{"username": "u"},
	})
	if err == nil || !strings.Contains(err.Error(), "append_only") {
		t.Errorf("err = %v, want a complaint about append_only", err)
	}
}

func TestParseSpecCoercesJSONTypes(t *testing.T) {
	spec, err := ParseSpec(TypeSFTP,
		[]byte(`{"host":"h","port":2222,"append_only":true}`),
		map[string]string{"username": "u"})
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.Config["port"] != "2222" {
		t.Errorf("port = %q, want 2222", spec.Config["port"])
	}
	if spec.Config["append_only"] != "true" {
		t.Errorf("append_only = %q, want true", spec.Config["append_only"])
	}
	if spec.Secrets["username"] != "u" {
		t.Errorf("secrets were not carried through: %v", spec.Secrets)
	}

	if _, err := ParseSpec(TypeLocal, []byte(`{"root":["a"]}`), nil); err == nil {
		t.Error("an unsupported JSON value type should be rejected")
	}
	if _, err := ParseSpec(TypeLocal, []byte(`not json`), nil); err == nil {
		t.Error("malformed JSON should be rejected")
	}
}

func TestForMaintenanceSwapsTheEndpoint(t *testing.T) {
	// rest-server's --append-only blocks deletes from every caller, so
	// retention has to reach the storage through a second endpoint.
	spec := Spec{
		Type: TypeREST,
		Config: map[string]string{
			"base_url":             "https://backup.example.com",
			"maintenance_base_url": "https://backup.internal:8000",
			"append_only":          "true",
		},
		Secrets: map[string]string{"username": "cp01", "password": "p"},
	}

	agentDest, err := Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	agentURI, err := agentDest.URI("cp01")
	if err != nil {
		t.Fatal(err)
	}
	if agentURI != "rest:https://backup.example.com/cp01/" {
		t.Errorf("agent URI = %q", agentURI)
	}

	maintenanceDest, err := Build(ForMaintenance(spec))
	if err != nil {
		t.Fatalf("Build for maintenance: %v", err)
	}
	maintenanceURI, err := maintenanceDest.URI("cp01")
	if err != nil {
		t.Fatal(err)
	}
	if maintenanceURI != "rest:https://backup.internal:8000/cp01/" {
		t.Errorf("maintenance URI = %q, want the delete-capable endpoint", maintenanceURI)
	}

	// The original must not be mutated: the same spec is used for both roles.
	if spec.Config["base_url"] != "https://backup.example.com" {
		t.Errorf("ForMaintenance mutated the input spec: %v", spec.Config)
	}
	if rest, ok := maintenanceDest.(*REST); ok && rest.AppendOnly {
		t.Error("the maintenance endpoint should not be marked append-only")
	}
}

func TestForMaintenanceLeavesOtherDestinationsAlone(t *testing.T) {
	// Local, SFTP and S3 are delete-capable already.
	spec := Spec{Type: TypeLocal, Config: map[string]string{"root": "/srv/b"}}
	if got := ForMaintenance(spec); got.Config["root"] != "/srv/b" || len(got.Config) != 1 {
		t.Errorf("ForMaintenance changed a delete-capable destination: %v", got.Config)
	}
}
