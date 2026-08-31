package destination

import "testing"

func TestParseTargetReadsTheShapesAnOperatorWrites(t *testing.T) {
	for _, tc := range []struct {
		line   string
		kind   Type
		config map[string]string
		repo   string
	}{
		{"cpbackup@backup.example.com:/backups", TypeSFTP,
			map[string]string{"host": "backup.example.com", "user": "cpbackup", "root": "/backups"}, ""},
		{"cpbackup@backup.example.com:2222:/srv/backups", TypeSFTP,
			map[string]string{"host": "backup.example.com", "user": "cpbackup", "port": "2222", "root": "/srv/backups"}, ""},
		{"sftp://cpbackup@backup.example.com:2222/srv/backups", TypeSFTP,
			map[string]string{"host": "backup.example.com", "user": "cpbackup", "port": "2222", "root": "/srv/backups"}, ""},
		{"cpbackup@backup.example.com:backups", TypeSFTP,
			map[string]string{"host": "backup.example.com", "user": "cpbackup", "root": "backups"}, ""},
		{"https://backup.example.com", TypeREST,
			map[string]string{"base_url": "https://backup.example.com"}, ""},
		{"https://backup.example.com:8000/", TypeREST,
			map[string]string{"base_url": "https://backup.example.com:8000"}, ""},
		{"s3://my-bucket/cp01", TypeS3, map[string]string{"bucket": "my-bucket"}, "cp01"},
		{"s3://my-bucket", TypeS3, map[string]string{"bucket": "my-bucket"}, ""},
		{"/mnt/nas/backups", TypeLocal, map[string]string{"root": "/mnt/nas/backups"}, ""},
	} {
		got, err := ParseTarget(tc.line)
		if err != nil {
			t.Errorf("%s: %v", tc.line, err)
			continue
		}
		if got.Type != tc.kind {
			t.Errorf("%s: type = %q, want %q", tc.line, got.Type, tc.kind)
		}
		if len(got.Config) != len(tc.config) {
			t.Errorf("%s: config = %v, want %v", tc.line, got.Config, tc.config)
			continue
		}
		for key, want := range tc.config {
			if got.Config[key] != want {
				t.Errorf("%s: config[%s] = %q, want %q", tc.line, key, got.Config[key], want)
			}
		}
		if got.Repository != tc.repo {
			t.Errorf("%s: repository = %q, want %q", tc.line, got.Repository, tc.repo)
		}
	}
}

// A line this cannot read is refused rather than guessed at: a backup
// written somewhere other than where it was meant to go is not a backup,
// and the operator is right there to be asked.
func TestParseTargetRefusesWhatItCannotRead(t *testing.T) {
	for _, line := range []string{
		"",
		"   ",
		"backup.example.com",
		"cpbackup@backup.example.com",
		"@backup.example.com:/backups",
		"cpbackup@backup.example.com:/",
		"sftp://backup.example.com/backups",
		"sftp://cpbackup@backup.example.com/",
		"cpbackup@backup.example.com:notaport:/backups",
		"http://backup.example.com",
		"s3://",
		"ftp://backup.example.com/backups",
	} {
		if got, err := ParseTarget(line); err == nil {
			t.Errorf("%q was read as %+v", line, got)
		}
	}
}
