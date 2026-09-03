package whm_test

import (
	"encoding/json"
	"image/png"
	"os"
	"strings"
	"testing"
)

// cPanel's install_plugin inherits the installer's restrictive umask when it
// writes the Jupiter DynamicUI record. The record is later read while cPanel
// builds an account's application list, so leaving it at 0600 makes the tile
// disappear even when Feature Manager enables it.
func TestInstallerMakesDynamicUIRegistrationAccountReadable(t *testing.T) {
	scriptBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	installAt := strings.Index(script, `/usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter`)
	chmodAt := strings.Index(script, `chmod 0644 "$DYNAMICUI"`)
	if installAt < 0 {
		t.Fatal("installer no longer registers the Jupiter plugin")
	}
	if chmodAt < 0 {
		t.Fatal("installer does not make the generated DynamicUI record account-readable")
	}
	if chmodAt < installAt {
		t.Fatal("DynamicUI permissions are set before cPanel generates the record")
	}
	if !strings.Contains(script, `( umask 022; /usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter )`) {
		t.Fatal("cPanel's plugin installer does not run with a public-metadata umask")
	}
	if touchAt := strings.Index(script, `touch "$DYNAMICUI"`); touchAt < chmodAt {
		t.Fatal("installer does not invalidate cPanel's per-account application cache after fixing permissions")
	}
}

func TestCPanelTileUsesTheRasterIcon(t *testing.T) {
	metadataBytes, err := os.ReadFile("../cpanel/install.json")
	if err != nil {
		t.Fatal(err)
	}
	var links []struct {
		Icon string `json:"icon"`
	}
	if err := json.Unmarshal(metadataBytes, &links); err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("Jupiter metadata contains %d links, want 1", len(links))
	}
	if links[0].Icon != "cprest.png" {
		t.Fatalf("Jupiter metadata icon = %q, want cprest.png", links[0].Icon)
	}

	icon, err := os.Open("../branding/cprestic-icon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer icon.Close()
	config, err := png.DecodeConfig(icon)
	if err != nil {
		t.Fatalf("decode cPanel icon: %v", err)
	}
	if config.Width != 48 || config.Height != 48 {
		t.Fatalf("cPanel icon is %dx%d, want 48x48", config.Width, config.Height)
	}

	scriptBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`install -m 0644 "$SOURCE_DIR/branding/cprestic-icon.png" "$PLUGIN_META/cprest.png"`,
		`rm -f "$FRONTEND/assets/application_icons/cprest.svg"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("installer is missing %q", required)
		}
	}
}

func TestInstallerDeploysAndRemovesTheSessionBridge(t *testing.T) {
	installerBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	uninstallerBytes, err := os.ReadFile("uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)
	uninstaller := string(uninstallerBytes)

	for _, required := range []string{
		`$CPANEL_UAPI_DIR/Cprest.pm`,
		`$CPANEL_ADMIN_DIR/Session.pm`,
		`-m 0700 "$SOURCE_DIR/cpanel/admin/Cprest/Session.pm"`,
	} {
		if !strings.Contains(installer, required) {
			t.Errorf("installer is missing %q", required)
		}
	}
	for _, required := range []string{
		`/usr/local/cpanel/Cpanel/API/Cprest.pm`,
		`/var/cpanel/perl/Cpanel/Admin/Modules/Cprest/Session.pm`,
	} {
		if !strings.Contains(uninstaller, required) {
			t.Errorf("uninstaller is missing %q", required)
		}
	}
}
