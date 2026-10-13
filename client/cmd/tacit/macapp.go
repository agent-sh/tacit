package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// On macOS `tacit install app` (from the desktop build) makes
// ~/Applications/tacit.app: Info.plist, a copy of the running binary as
// Contents/MacOS/tacit-app, and Contents/Resources/tacit.icns (committed in
// macos/, rebuilt from reviewui/icon.svg by scripts/make-icns.sh). A copy,
// not a link: the bundle's executable must sit inside it for macOS to read
// its Info.plist and icon. Finder starts the executable with no arguments,
// so main opens the window when it runs from a bundle (launchedFromBundle).
// The copy is signed ad hoc when codesign is there (Apple Silicon runs only
// signed code; the Go linker signs the binary, and the bundle signature
// covers Info.plist and the icon too). Re-run install app after rebuilding
// tacit-app.

const (
	macAppName       = "tacit.app"
	macAppExecutable = "tacit-app"
	macAppBundleID   = "ai.tacit.app"
)

// macAppDir is ~/Applications/tacit.app (a per-user Applications folder,
// so no admin rights are needed).
func macAppDir(home string) string { return filepath.Join(home, "Applications", macAppName) }

// macInfoPlist is Contents/Info.plist. LSEnvironment gives the app, which
// Finder starts with launchd's short PATH, the PATH it needs for git and
// scrump, and TACIT_STORE when one was chosen.
func macInfoPlist(storeEnv, path string) string {
	p := newPlist()
	p.str("CFBundleDevelopmentRegion", "en")
	p.str("CFBundleDisplayName", "tacit")
	p.str("CFBundleExecutable", macAppExecutable)
	p.str("CFBundleIconFile", "tacit")
	p.str("CFBundleIdentifier", macAppBundleID)
	p.str("CFBundleInfoDictionaryVersion", "6.0")
	p.str("CFBundleName", "tacit")
	p.str("CFBundlePackageType", "APPL")
	p.str("CFBundleShortVersionString", "1.0")
	p.str("CFBundleVersion", "1")
	p.str("LSApplicationCategoryType", "public.app-category.developer-tools")
	p.str("LSMinimumSystemVersion", "11.0")
	p.boolean("NSHighResolutionCapable", true)
	p.dict("LSEnvironment", jobEnv(path, storeEnv)...)
	return p.String()
}

// launchedFromBundle reports an executable inside a macOS app bundle.
func launchedFromBundle(exe string) bool {
	return strings.Contains(filepath.ToSlash(exe), ".app/Contents/MacOS/")
}

// installMacApp writes the bundle under home and returns what it wrote.
// sign is run on the bundle when it is not nil.
func installMacApp(home, exe, storeEnv, path string, icns []byte, sign func(app string) error) ([]string, error) {
	if len(icns) == 0 {
		return nil, errors.New("this build carries no app icon")
	}
	app := macAppDir(home)
	contents := filepath.Join(app, "Contents")
	bin := filepath.Join(contents, "MacOS", macAppExecutable)
	var out []string
	if err := writeFileMode(filepath.Join(contents, "Info.plist"), []byte(macInfoPlist(storeEnv, path)), 0o644); err != nil {
		return out, err
	}
	out = append(out, filepath.Join(contents, "Info.plist"))
	if err := writeFileMode(filepath.Join(contents, "Resources", "tacit.icns"), icns, 0o644); err != nil {
		return out, err
	}
	out = append(out, filepath.Join(contents, "Resources", "tacit.icns"))
	if same, _ := sameFile(exe, bin); !same {
		b, err := os.ReadFile(exe)
		if err != nil {
			return out, err
		}
		if err := writeFileMode(bin, b, 0o755); err != nil {
			return out, err
		}
	}
	out = append(out, bin)
	if sign != nil {
		if err := sign(app); err != nil {
			return out, fmt.Errorf("sign %s: %w", app, err)
		}
	}
	return out, nil
}

func sameFile(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

// adHocSign signs the bundle with the ad hoc identity (no certificate). It
// returns nil without signing when codesign is not installed (it comes with
// the Xcode command line tools, which the desktop build needs anyway).
func adHocSign(app string) error {
	cs, err := exec.LookPath("codesign")
	if err != nil {
		return nil
	}
	out, err := exec.Command(cs, "--force", "--sign", "-", app).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// openURL opens url in the default browser.
func openURL(goos, url string) {
	if goos == "windows" {
		exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
		return
	}
	cmd := "xdg-open"
	if goos == "darwin" {
		cmd = "open"
	}
	exec.Command(cmd, url).Start()
}
