package node

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/sshkeys"
)

// SFTPRequest is what an operator fills in to back up to another Linux
// server.
//
// There is no field for a key: cprest generates its own, one per
// destination, so revoking access to one destination does not lock it out
// of the others.
type SFTPRequest struct {
	Name           string
	Host           string
	Port           int
	User           string
	RemoteDir      string
	RepositoryPath string
	// Password, when given, is used once to install the generated public
	// key on the far side and is then discarded. It is never stored.
	Password string
	// ExistingKeyPath uses a key the operator already has instead of
	// generating one.
	ExistingKeyPath string
}

// SFTPResult reports what was set up, including the public key an operator
// must install if cprest could not do it for them.
type SFTPResult struct {
	Destination     nodestore.Destination
	Repository      nodestore.Repository
	AuthorizedKey   string
	KeyFingerprint  string
	HostFingerprint string
	HostKeyType     string
	// Installed is true when the public key was placed on the remote
	// server, Verified when logging in with it then worked.
	Installed bool
	Verified  bool
	// Warning explains what still needs doing by hand.
	Warning string
}

const sshTimeout = 20 * time.Second

// AddSFTPDestination sets up a backup destination on another Linux server.
//
// It generates a key, learns the server's host key, and — given the remote
// password once — installs the key and proves it works. What an operator
// used to do with ssh-keygen, ssh-copy-id and a hand-written known_hosts.
func (e *Engine) AddSFTPDestination(req SFTPRequest) (SFTPResult, error) {
	if err := validateSFTP(&req); err != nil {
		return SFTPResult{}, err
	}

	address := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	settings := e.settings
	destinationID := nodestore.NewID()

	// 1. The server's identity, read before we send it anything. This
	//    needs no credentials: the host proves itself first.
	hostKey, err := sshkeys.FetchHostKey(address, sshTimeout)
	if err != nil {
		return SFTPResult{}, fmt.Errorf(
			"node: could not reach %s over SSH: %w", address, err)
	}
	knownHostsPath := filepath.Join(settings.ConfigDir, "known_hosts", destinationID)
	if err := sshkeys.WriteKnownHosts(knownHostsPath, hostKey); err != nil {
		return SFTPResult{}, err
	}

	result := SFTPResult{
		HostFingerprint: hostKey.Fingerprint,
		HostKeyType:     hostKey.Type,
	}

	// 2. Our key for this destination.
	identityPath := req.ExistingKeyPath
	var privatePEM []byte
	if identityPath == "" {
		pair, err := sshkeys.Generate("cprest@" + settings.Hostname)
		if err != nil {
			return SFTPResult{}, err
		}
		identityPath, err = sshkeys.WritePrivateKey(
			filepath.Join(settings.ConfigDir, "keys"), destinationID, pair)
		if err != nil {
			return SFTPResult{}, err
		}
		privatePEM = pair.PrivatePEM
		result.AuthorizedKey = pair.AuthorizedKey
		result.KeyFingerprint = pair.Fingerprint
	}

	// 3. Install it, if we were trusted with the password.
	switch {
	case req.Password != "" && privatePEM != nil:
		if err := sshkeys.InstallAuthorizedKey(address, req.User, req.Password,
			hostKey, result.AuthorizedKey, sshTimeout); err != nil {
			return SFTPResult{}, err
		}
		result.Installed = true

		if err := sshkeys.VerifyKeyLogin(address, req.User, privatePEM, hostKey, sshTimeout); err != nil {
			return SFTPResult{}, fmt.Errorf(
				"node: the key was installed but logging in with it failed: %w", err)
		}
		result.Verified = true

		if err := sshkeys.EnsureRemoteDir(address, req.User, privatePEM,
			hostKey, req.RemoteDir, sshTimeout); err != nil {
			result.Warning = fmt.Sprintf(
				"Logged in, but could not create %s: %v. Create it yourself and test again.",
				req.RemoteDir, err)
		}
	case privatePEM != nil:
		result.Warning = "Add the public key below to " + req.User +
			"@" + req.Host + ":~/.ssh/authorized_keys, then use Test."
	}

	// 4. Store it. The private key stays a file on disk with the
	//    permissions ssh insists on; only its path is recorded.
	dest := nodestore.Destination{
		ID:   destinationID,
		Name: req.Name,
		Type: string(destination.TypeSFTP),
		Config: map[string]string{
			"host":             req.Host,
			"user":             req.User,
			"root":             req.RemoteDir,
			"identity_file":    identityPath,
			"known_hosts_file": knownHostsPath,
		},
	}
	if req.Port != 22 {
		dest.Config["port"] = strconv.Itoa(req.Port)
	}

	spec := destination.Spec{
		Type: destination.TypeSFTP, Config: dest.Config, Secrets: map[string]string{},
	}
	if _, err := destination.Build(spec); err != nil {
		return SFTPResult{}, err
	}

	stored, err := e.store.PutDestination(dest)
	if err != nil {
		return SFTPResult{}, err
	}
	result.Destination = stored

	repository, err := e.newRepository(stored.ID, req.RepositoryPath)
	if err != nil {
		return SFTPResult{}, err
	}
	result.Repository = repository
	return result, nil
}

// PublicKeyFor returns the public key cprest generated for a destination, so
// the interface can show it again later.
func (e *Engine) PublicKeyFor(dest nodestore.Destination) string {
	identity := dest.Config["identity_file"]
	if identity == "" {
		return ""
	}
	pair, err := sshkeys.PublicKeyFromFile(identity)
	if err != nil {
		e.log.Error("read public key", "destination", dest.Name, "error", err)
		return ""
	}
	return pair
}

func validateSFTP(req *SFTPRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.RemoteDir = strings.TrimSpace(req.RemoteDir)

	switch {
	case req.Name == "":
		return fmt.Errorf("node: give the destination a name")
	case req.Host == "":
		return fmt.Errorf("node: the remote host is required")
	case req.User == "":
		return fmt.Errorf("node: the SSH user is required")
	case !strings.HasPrefix(req.RemoteDir, "/"):
		return fmt.Errorf("node: the remote directory must be an absolute path")
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return fmt.Errorf("node: port %d is out of range", req.Port)
	}
	return nil
}
