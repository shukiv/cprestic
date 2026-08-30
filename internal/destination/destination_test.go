package destination

import (
	"strings"
	"testing"
)

func TestCleanRepoPath(t *testing.T) {
	valid := map[string]string{
		"cp01":        "cp01",
		"/cp01":       "cp01",
		"/cp01/":      "cp01",
		"  cp01  ":    "cp01",
		"eu/cp01":     "eu/cp01",
		"eu//cp01":    "eu/cp01",
		"eu/./cp01":   "eu/cp01",
		"eu/x/../cp1": "eu/cp1",
	}
	for input, want := range valid {
		got, err := CleanRepoPath(input)
		if err != nil {
			t.Errorf("CleanRepoPath(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("CleanRepoPath(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"", "   ", "/", "..", "../etc", "cp01/../../etc",
		"a\\b", "cp\x0001",
	}
	for _, input := range invalid {
		if got, err := CleanRepoPath(input); err == nil {
			t.Errorf("CleanRepoPath(%q) = %q, want an error", input, got)
		}
	}
}

func TestLocalURI(t *testing.T) {
	dest := &Local{Root: "/srv/backups"}
	got, err := dest.URI("cp01")
	if err != nil {
		t.Fatalf("URI: %v", err)
	}
	if want := "/srv/backups/cp01"; got != want {
		t.Errorf("URI = %q, want %q", got, want)
	}
	if _, err := (&Local{Root: "relative"}).URI("cp01"); err == nil {
		t.Error("relative root should be rejected")
	}
	if _, err := dest.URI("../../etc"); err == nil {
		t.Error("traversal should be rejected")
	}
}

func TestSFTPURI(t *testing.T) {
	cases := []struct {
		name string
		dest *SFTP
		want string
	}{
		{
			name: "default port uses short form",
			dest: &SFTP{Host: "backup.example.com", User: "cpbackup", Root: "/backup", IdentityFile: "/k"},
			want: "sftp:cpbackup@backup.example.com:/backup/cp01",
		},
		{
			name: "explicit 22 uses short form",
			dest: &SFTP{Host: "h", Port: 22, User: "u", Root: "/b", IdentityFile: "/k"},
			want: "sftp:u@h:/b/cp01",
		},
		{
			// The URL form needs a double slash before an absolute path.
			name: "custom port uses url form",
			dest: &SFTP{Host: "h", Port: 2222, User: "u", Root: "/srv/repo", IdentityFile: "/k"},
			want: "sftp://u@h:2222//srv/repo/cp01",
		},
		{
			name: "ipv6 literal is bracketed",
			dest: &SFTP{Host: "::1", Port: 2222, User: "u", Root: "/srv/repo", IdentityFile: "/k"},
			want: "sftp://u@[::1]:2222//srv/repo/cp01",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.dest.URI("cp01")
			if err != nil {
				t.Fatalf("URI: %v", err)
			}
			if got != tc.want {
				t.Errorf("URI = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := (&SFTP{Host: "h", User: "u", Root: "relative", IdentityFile: "/k"}).URI("cp01"); err == nil {
		t.Error("relative root should be rejected")
	}
}

func TestRESTURIExcludesCredentials(t *testing.T) {
	dest := &REST{
		BaseURL:  "https://backup.example.com",
		Username: "cp01",
		Password: "hunter2",
	}
	got, err := dest.URI("cp01")
	if err != nil {
		t.Fatalf("URI: %v", err)
	}
	if want := "rest:https://backup.example.com/cp01/"; got != want {
		t.Errorf("URI = %q, want %q", got, want)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "cp01:") {
		t.Errorf("URI %q leaks credentials", got)
	}

	env, err := dest.Env()
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	if env["RESTIC_REST_USERNAME"] != "cp01" || env["RESTIC_REST_PASSWORD"] != "hunter2" {
		t.Errorf("Env = %v, want credentials present", env)
	}
}

func TestS3URI(t *testing.T) {
	cases := []struct {
		name string
		dest *S3
		want string
	}{
		{
			name: "custom endpoint",
			dest: &S3{Endpoint: "s3.us-east-1.wasabisys.com", Bucket: "cp-backups"},
			want: "s3:https://s3.us-east-1.wasabisys.com/cp-backups/cp01",
		},
		{
			name: "endpoint with scheme and port",
			dest: &S3{Endpoint: "https://minio.internal:9000", Bucket: "b"},
			want: "s3:https://minio.internal:9000/b/cp01",
		},
		{
			name: "amazon by region",
			dest: &S3{Region: "eu-west-1", Bucket: "b"},
			want: "s3:s3.amazonaws.com/b/cp01",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.dest.URI("cp01")
			if err != nil {
				t.Fatalf("URI: %v", err)
			}
			if got != tc.want {
				t.Errorf("URI = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := (&S3{Endpoint: "h", Bucket: ""}).URI("cp01"); err == nil {
		t.Error("missing bucket should be rejected")
	}
	if _, err := (&S3{Bucket: "b"}).URI("cp01"); err == nil {
		t.Error("missing endpoint and region should be rejected")
	}
	if _, err := (&S3{Endpoint: "https://h/some/path", Bucket: "b"}).URI("cp01"); err == nil {
		t.Error("endpoint with a path should be rejected")
	}
}

func TestS3EnvRequiresCredentials(t *testing.T) {
	if _, err := (&S3{Bucket: "b", Region: "r"}).Env(); err == nil {
		t.Error("missing credentials should be rejected")
	}
	env, err := (&S3{
		Bucket: "b", Region: "r",
		AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "ST",
	}).Env()
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	for key, want := range map[string]string{
		"AWS_ACCESS_KEY_ID":     "AK",
		"AWS_SECRET_ACCESS_KEY": "SK",
		"AWS_DEFAULT_REGION":    "r",
		"AWS_SESSION_TOKEN":     "ST",
	} {
		if env[key] != want {
			t.Errorf("Env[%s] = %q, want %q", key, env[key], want)
		}
	}
}
