package destination

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Target is a destination written as one line, the way an operator would
// say it out loud: cpbackup@backup.example.com:/backups, or
// https://backup.example.com, or s3://my-bucket/cp01, or /mnt/nas.
//
// Filling in a four-field form is a poor way to say something that short.
// Everything the form asks for that cannot be read off the line — a
// password, a bucket's credentials — is asked for separately.
type Target struct {
	Type Type
	// Config is the destination's stored configuration, as far as the
	// line determines it.
	Config map[string]string
	// Repository is the folder inside the destination, when the line
	// named one beyond the destination's own root.
	Repository string
}

// ParseTarget reads a destination out of one line.
//
// It refuses what it cannot recognise rather than guessing: a backup
// written to the wrong place is not a backup, and the operator is right
// there to be asked.
func ParseTarget(raw string) (Target, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return Target{}, fmt.Errorf("destination: nothing to read")
	}

	switch {
	case strings.HasPrefix(line, "/"):
		return Target{Type: TypeLocal, Config: map[string]string{"root": path.Clean(line)}}, nil
	case strings.HasPrefix(line, "s3://"):
		return parseS3(line)
	case strings.HasPrefix(line, "https://"):
		return Target{Type: TypeREST, Config: map[string]string{
			"base_url": strings.TrimRight(line, "/"),
		}}, nil
	case strings.HasPrefix(line, "http://"):
		return Target{}, fmt.Errorf(
			"destination: %s is not https, and a backup server reached over plain http "+
				"hands every credential and every byte to anyone on the path", line)
	case strings.HasPrefix(line, "sftp://"), strings.HasPrefix(line, "ssh://"):
		return parseSFTPURL(line)
	case strings.Contains(line, "@") && strings.Contains(line, ":"):
		return parseSCP(line)
	}
	return Target{}, fmt.Errorf(
		"destination: %q is not a destination this understands. Write it as "+
			"user@host:/path, sftp://user@host:port/path, https://backup.example.com, "+
			"s3://bucket/folder, or an absolute path on this server", line)
}

func parseS3(line string) (Target, error) {
	rest := strings.TrimPrefix(line, "s3://")
	bucket, folder, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Target{}, fmt.Errorf("destination: %s names no bucket", line)
	}
	return Target{
		Type:       TypeS3,
		Config:     map[string]string{"bucket": bucket},
		Repository: strings.Trim(folder, "/"),
	}, nil
}

func parseSFTPURL(line string) (Target, error) {
	parsed, err := url.Parse(line)
	if err != nil {
		return Target{}, fmt.Errorf("destination: %s is not a usable address: %w", line, err)
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		return Target{}, fmt.Errorf(
			"destination: %s does not say which user to log in as, as in sftp://cpbackup@%s%s",
			line, parsed.Host, parsed.Path)
	}
	config := map[string]string{
		"host": parsed.Hostname(),
		"user": parsed.User.Username(),
		"root": path.Clean("/" + strings.TrimPrefix(parsed.Path, "/")),
	}
	if port := parsed.Port(); port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return Target{}, fmt.Errorf("destination: %q is not a port", port)
		}
		config["port"] = port
	}
	if config["root"] == "/" {
		return Target{}, fmt.Errorf(
			"destination: %s does not say where on that server to write, as in %s/backups",
			line, strings.TrimRight(line, "/"))
	}
	return Target{Type: TypeSFTP, Config: config}, nil
}

// parseSCP reads the shape ssh itself takes: user@host:/path, with an
// optional :port before the path.
func parseSCP(line string) (Target, error) {
	user, rest, _ := strings.Cut(line, "@")
	if user == "" {
		return Target{}, fmt.Errorf("destination: %s does not say which user to log in as", line)
	}
	host, location, found := strings.Cut(rest, ":")
	if !found || host == "" {
		return Target{}, fmt.Errorf(
			"destination: %s does not say where on that server to write, as in %s:/backups",
			line, line)
	}
	config := map[string]string{"host": host, "user": user}

	// user@host:2222:/backups — a port only when what follows it is a path.
	if port, remainder, ok := strings.Cut(location, ":"); ok {
		if _, err := strconv.Atoi(port); err != nil {
			return Target{}, fmt.Errorf("destination: %q is not a port", port)
		}
		config["port"], location = port, remainder
	}
	if location == "" {
		return Target{}, fmt.Errorf(
			"destination: %s does not say where on that server to write, as in %s/backups", line, line)
	}
	if !strings.HasPrefix(location, "/") {
		// ssh reads a bare path as relative to the home directory, and so
		// does restic. Keeping it relative is what the operator meant.
		config["root"] = location
	} else {
		config["root"] = path.Clean(location)
	}
	if config["root"] == "/" {
		return Target{}, fmt.Errorf(
			"destination: %s writes to the root of that server, which is never what was "+
				"meant. Name a directory, as in %s/backups", line, strings.TrimRight(line, "/"))
	}
	return Target{Type: TypeSFTP, Config: config}, nil
}
