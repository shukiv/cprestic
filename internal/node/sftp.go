package node

import (
	"fmt"
	"net"
	"os"
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
	// AdminUser and AdminPassword, when given, are an account on the
	// remote server that can create the backup account: usually root.
	// cprest makes the user, its home, the backup directory and the
	// authorized_keys entry, then locks the account's password so the key
	// is the only way in. The password is used for that one connection
	// and is never stored.
	AdminUser     string
	AdminPassword string
	// ConfirmedFingerprint is the remote server's host key fingerprint as
	// the operator has agreed to it. Empty means they have not been shown
	// one yet, and nothing is sent to that server until they have: the
	// whole point of a fingerprint is that somebody looked at it.
	ConfirmedFingerprint string
}

// UnconfirmedHostError is what AddSFTPDestination returns when it has
// learnt a server's identity and nobody has agreed to it yet.
//
// Trust on first use is what an operator does when they type "yes" at
// ssh's prompt, and it is a real decision: the fingerprint is the only
// thing standing between a backup destination and whoever answered on
// that address. Storing it silently and then sending a password made
// that decision for them, invisibly, every time.
type UnconfirmedHostError struct {
	Host        string
	Fingerprint string
	KeyType     string
}

func (e *UnconfirmedHostError) Error() string {
	return fmt.Sprintf("%s identifies itself with a %s key, fingerprint %s. "+
		"Check it against that server before going on: on it, run "+
		"ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub",
		e.Host, e.KeyType, e.Fingerprint, strings.TrimPrefix(e.KeyType, "ssh-"))
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
	// server, Verified when logging in with it then worked, and Created
	// when cprest made the account it logs in to.
	Installed bool
	Verified  bool
	Created   bool
	// Warning explains what still needs doing by hand.
	Warning string
}

const sshTimeout = 20 * time.Second

// detailOf appends a remote command's output to an error, when it said
// anything worth reading.
func detailOf(output string) string {
	if output == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(output, "\n", " | ")
}

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

	// Anything written before the destination is stored is litter if this
	// fails, and an operator retrying a typo should not accumulate keys.
	var litter []string
	defer func() {
		for _, path := range litter {
			_ = os.Remove(path)
		}
	}()

	// 1. The server's identity, read before we send it anything. This
	//    needs no credentials: the host proves itself first.
	hostKey, err := sshkeys.FetchHostKey(address, sshTimeout)
	if err != nil {
		return SFTPResult{}, fmt.Errorf(
			"node: could not reach %s over SSH: %w", address, err)
	}
	// Nothing has been sent to that server yet, and nothing is until the
	// operator has seen who answered.
	switch {
	case req.ConfirmedFingerprint == "":
		return SFTPResult{}, &UnconfirmedHostError{
			Host: req.Host, Fingerprint: hostKey.Fingerprint, KeyType: hostKey.Type,
		}
	case req.ConfirmedFingerprint != hostKey.Fingerprint:
		return SFTPResult{}, fmt.Errorf(
			"node: %s answered with a different host key than the one you agreed to. "+
				"You agreed to %s and it presented %s. Either that server was rebuilt, or "+
				"something else is answering on its address -- do not go on until you know "+
				"which", req.Host, req.ConfirmedFingerprint, hostKey.Fingerprint)
	}

	knownHostsPath := filepath.Join(settings.ConfigDir, "known_hosts", destinationID)
	if err := sshkeys.WriteKnownHosts(knownHostsPath, hostKey); err != nil {
		return SFTPResult{}, err
	}
	litter = append(litter, knownHostsPath)

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
		litter = append(litter, identityPath)
	}

	// 3. Make the account, if we were given something that can.
	if req.AdminPassword != "" && privatePEM != nil {
		output, err := sshkeys.RunAsAdmin(address, req.AdminUser, req.AdminPassword, hostKey,
			sshkeys.ProvisionScript(req.User, req.RemoteDir, result.AuthorizedKey), sshTimeout)
		if err != nil {
			return SFTPResult{}, fmt.Errorf(
				"node: could not set up %s on %s as %s: %w%s",
				req.User, req.Host, req.AdminUser, err, detailOf(output))
		}
		result.Created = true
		result.Installed = true

		if err := sshkeys.VerifyKeyLogin(address, req.User, privatePEM, hostKey, sshTimeout); err != nil {
			return SFTPResult{}, fmt.Errorf(
				"node: %s was created on %s but logging in as it with the new key failed: %w. "+
					"Check that sshd on that server allows this account to log in with a key",
				req.User, req.Host, err)
		}
		result.Verified = true
	}

	// 4. Install the key, if we were trusted with the account's password
	//    and did not just create the account ourselves.
	switch {
	case result.Verified:
		// Already done, above.
	case req.Password != "" && privatePEM != nil:
		if err := sshkeys.InstallAuthorizedKey(address, req.User, req.Password,
			hostKey, result.AuthorizedKey, sshTimeout); err != nil {
			return SFTPResult{}, err
		}
		result.Installed = true

		if err := sshkeys.VerifyKeyLogin(address, req.User, privatePEM, hostKey, sshTimeout); err != nil {
			// sshd refuses a key silently when ~/.ssh or authorized_keys
			// are writable by anyone but their owner, so say what is
			// actually there rather than leaving the operator guessing.
			detail := sshkeys.Diagnose(address, req.User, req.Password, hostKey, sshTimeout)
			if detail != "" {
				return SFTPResult{}, fmt.Errorf(
					"node: the key was added to %s@%s but logging in with it failed: %w. "+
						"sshd ignores authorized_keys when the home directory or ~/.ssh is "+
						"writable by others. On that server: %s",
					req.User, req.Host, err, strings.ReplaceAll(detail, "\n", " | "))
			}
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

	// 5. Store it. The private key stays a file on disk with the
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
	// The destination owns these files now.
	litter = nil
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
	req.AdminUser = strings.TrimSpace(req.AdminUser)
	if req.AdminPassword != "" && req.AdminUser == "" {
		req.AdminUser = "root"
	}

	switch {
	case req.Name == "":
		return fmt.Errorf("node: give the destination a name")
	case req.Host == "":
		return fmt.Errorf("node: the remote host is required")
	case req.User == "":
		return fmt.Errorf("node: the SSH user is required")
	case !strings.HasPrefix(req.RemoteDir, "/"):
		return fmt.Errorf("node: the remote directory must be an absolute path")
	case req.AdminPassword != "" && req.AdminUser == req.User:
		// Creating an account requires an account that already exists.
		return fmt.Errorf(
			"node: %s is the account to create, so it cannot also be the one that creates it. "+
				"Give an account that can already log in, usually root", req.User)
	case req.AdminPassword != "" && req.Password != "":
		return fmt.Errorf(
			"node: give either the backup account's own password or an administrator's, " +
				"not both: the first installs a key on an account that exists, the second " +
				"creates the account")
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return fmt.Errorf("node: port %d is out of range", req.Port)
	}
	return nil
}
