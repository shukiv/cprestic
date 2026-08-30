package node

import "testing"

func TestValidateSFTP(t *testing.T) {
	valid := SFTPRequest{
		Name: "Backup server", Host: "backup.example.com",
		User: "cpbackup", RemoteDir: "/home/cpbackup/backups",
	}
	request := valid
	if err := validateSFTP(&request); err != nil {
		t.Fatalf("validateSFTP: %v", err)
	}
	if request.Port != 22 {
		t.Errorf("port defaulted to %d, want 22", request.Port)
	}

	missing := map[string]func(*SFTPRequest){
		"name":            func(r *SFTPRequest) { r.Name = "  " },
		"host":            func(r *SFTPRequest) { r.Host = "" },
		"user":            func(r *SFTPRequest) { r.User = "" },
		"relative remote": func(r *SFTPRequest) { r.RemoteDir = "backups" },
		"bad port":        func(r *SFTPRequest) { r.Port = 70000 },
	}
	for name, break_ := range missing {
		request := valid
		break_(&request)
		if err := validateSFTP(&request); err == nil {
			t.Errorf("a request with a bad %s was accepted", name)
		}
	}
}

func TestValidateSFTPTrimsWhitespace(t *testing.T) {
	// Operators paste these out of terminals and tickets.
	request := SFTPRequest{
		Name: " Backup server ", Host: " backup.example.com ",
		User: " cpbackup ", RemoteDir: " /home/cpbackup/backups ",
	}
	if err := validateSFTP(&request); err != nil {
		t.Fatalf("validateSFTP: %v", err)
	}
	if request.Host != "backup.example.com" || request.User != "cpbackup" {
		t.Errorf("request = %+v", request)
	}
}
