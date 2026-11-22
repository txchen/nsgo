package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nsgo-dev/nsgo/cmd/nsgo/authenticode"
	"github.com/nsgo-dev/nsgo/internal/platform/updates"
	"github.com/nsgo-dev/nsgo/tsx/pkg"
)

func TestCommandsConfig(t *testing.T) {
	for _, c := range []struct {
		yaml, err string
	}{
		{"commands: [{name: gv, package: ./cmd/gv}]", ""},
		{"commands: [{name: gv, paths: {macos_arm64: bin/gv, linux_x64: bin/gv-linux}}]", ""},
		{"commands: [{name: gv-admin_2, package: example.com/gv/cmd/admin}, {name: gv, package: ./cmd/gv}]", ""},
		{"commands: [{name: '', package: ./cmd/gv}]", "commands entry 1: name"},
		{"commands: [{name: gv.exe, package: ./cmd/gv}]", "letters, digits"},
		{"commands: [{name: -gv, package: ./cmd/gv}]", "letters, digits"},
		{"commands: [{name: ../gv, package: ./cmd/gv}]", "letters, digits"},
		{"commands: [{name: nul, package: ./cmd/gv}]", "reserved Windows file name"},
		{"commands: [{name: com1, package: ./cmd/gv}]", "reserved Windows file name"},
		{"commands: [{name: govault, package: ./cmd/gv}]", "the app's own executable"},
		{"name: Go Vault\ncommands: [{name: go-vault, package: ./cmd/gv}]", "the app's own command in /usr/bin"},
		{"commands: [{name: uninstall, package: ./cmd/gv}]", "the app's own Windows uninstaller"},
		{"commands: [{name: apprun, package: ./cmd/gv}]", "the app's own AppImage entry point"},
		{"commands: [{name: gv, package: ./cmd/gv}, {name: GV, package: ./cmd/gv2}]", "gv and GV name the same file"},
		{"commands: [{name: gv}]", "exactly one of package and paths"},
		{"commands: [{name: gv, package: ./cmd/gv, paths: {linux_x64: bin/gv}}]", "exactly one of package and paths"},
		{"commands: [{name: gv, package: ./cmd/...}]", "is not one Go package's path"},
		{"commands: [{name: gv, package: -toolexec=x}]", "is not one Go package's path"},
		{"commands: [{name: gv, paths: {macos_universal: bin/gv}}]", `paths name "macos_universal"`},
		{"commands: [{name: gv, paths: {linux_x64: ''}}]", "linux_x64 path is empty"},
		{"commands: [{name: gv, package: ./cmd/gv, ldflags: [-X main.channel=beta]}]", ""},
		{"commands: [{name: gv, paths: {linux_x64: bin/gv}, ldflags: [-X main.channel=beta]}]", "ldflags need a package"},
	} {
		dir := t.TempDir()
		manifest := "name: GoVault\nid: dev.example.govault\n" + c.yaml + "\n"
		if strings.HasPrefix(c.yaml, "name:") {
			manifest = "id: dev.example.govault\n" + c.yaml + "\n"
		}
		writeFiles(t, dir, map[string]string{"nsgo.yaml": manifest})
		_, err := load(dir)
		if c.err == "" && err != nil {
			t.Errorf("%s: %v", c.yaml, err)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: error %v, want %q", c.yaml, err, c.err)
		}
	}
}

// commandFixture is an app's module with a command's main package, cmd/tool, and a package that is not one, lib.
func commandFixture(t *testing.T) *project {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":           "module example.com/fixture\n\ngo 1.26\n",
		"cmd/tool/main.go": "package main\n\nimport \"fmt\"\n\nvar version, channel = \"dev\", \"stable\"\n\nfunc main() { fmt.Println(version, channel) }\n",
		"lib/lib.go":       "package lib\n",
	})
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	return &project{Dir: dir, Name: "App", ID: "dev.example.app", Version: "1.0.0", Release: true}
}

// buildFixtureProgram builds the fixture's command for goos and goarch into the fixture's bin directory, as a prebuilt file the app names.
func buildFixtureProgram(t *testing.T, p *project, goos, goarch string) string {
	t.Helper()
	out := filepath.Join(p.Dir, "bin", "tool-"+goos+"-"+goarch)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/tool")
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, data)
	}
	rel, _ := filepath.Rel(p.Dir, out)
	return rel
}

// A command built from a Go package or copied from a prebuilt file lands beside the app as an executable for the target, named for its system.
func TestAddCommands(t *testing.T) {
	p := commandFixture(t)
	p.Commands = []command{
		{Name: "tool", Package: "./cmd/tool"},
		{Name: "prebuilt", Paths: map[string]string{
			"linux_x64":   buildFixtureProgram(t, p, "linux", "amd64"),
			"windows_x64": buildFixtureProgram(t, p, "windows", "amd64"),
		}},
	}
	linux := t.TempDir()
	if err := p.addCommands(t.Context(), "linux_x64", linux); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tool", "prebuilt"} {
		path := filepath.Join(linux, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s's mode is %v", name, info.Mode())
		}
		f, err := elf.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if f.Machine != elf.EM_X86_64 {
			t.Errorf("%s is for %v", name, f.Machine)
		}
		f.Close()
	}
	if got := p.commandFiles(linux, "linux_x64"); len(got) != 2 {
		t.Errorf("commandFiles = %q", got)
	}

	windows := t.TempDir()
	if err := p.addCommands(t.Context(), "windows_x64", windows); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tool.exe", "prebuilt.exe"} {
		f, err := pe.Open(filepath.Join(windows, name))
		if err != nil {
			t.Fatal(err)
		}
		if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
			t.Errorf("%s is for machine %#x", name, f.Machine)
		}
		f.Close()
	}

	p.Commands = p.Commands[:1]
	mac := t.TempDir()
	if err := p.addCommands(t.Context(), "macos_arm64", mac); err != nil {
		t.Fatal(err)
	}
	f, err := macho.Open(filepath.Join(mac, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Cpu != macho.CpuArm64 || f.Type != macho.TypeExec {
		t.Errorf("tool is a %v %v", f.Cpu, f.Type)
	}
	f.Close()
}

// A command links with the app's linker flags, then its own, whose -X values win, and the identity core.App reads, which neither replaces.
func TestCommandLdflags(t *testing.T) {
	p := commandFixture(t)
	p.Build.LDFlags = []string{"-X main.version=1.0.0", "-X main.channel=app"}
	c := command{Name: "tool", Package: "./cmd/tool", LDFlags: []string{"-X 'main.channel=beta builds'"}}
	flags := p.commandLdflags(c, "-s", "-w")
	for _, part := range []string{"-X main.version=1.0.0", "-X main.channel=app", "-X 'main.channel=beta builds'", "-X '" + corePath + ".appVersion=1.0.0'", "-X '" + corePath + ".appID=dev.example.app'", "-X " + corePath + ".appRelease=1", "-s -w"} {
		if !strings.Contains(flags, part) {
			t.Errorf("ldflags %q lack %q", flags, part)
		}
	}
	if strings.Index(flags, "main.channel=app") > strings.Index(flags, "main.channel=beta") || strings.Index(flags, "main.channel=beta") > strings.Index(flags, corePath) {
		t.Errorf("ldflags %q are out of order", flags)
	}

	// Built for this machine, the command runs with the values its flags gave.
	system, arch := runtime.GOOS, goArchName(runtime.GOARCH)
	if system == "darwin" {
		system = "macos"
	}
	target := system + "_" + arch
	if !slices.Contains(commandTargets, target) {
		t.Skipf("no command target for %s", target)
	}
	p.Commands = []command{c}
	dir := t.TempDir()
	if err := p.addCommands(t.Context(), target, dir); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(dir, commandFile("tool", target))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "1.0.0 beta builds" {
		t.Errorf("the command prints %q, want its version and channel from the flags", got)
	}
}

func TestAddCommandsRefuses(t *testing.T) {
	p := commandFixture(t)
	linux := buildFixtureProgram(t, p, "linux", "amd64")
	for _, c := range []struct {
		name   string
		cmd    command
		target string
		before string
		err    string
	}{
		{"a target without a file", command{Name: "tool", Paths: map[string]string{"linux_x64": linux}}, "linux_arm64", "", "tool has no path for linux_arm64"},
		{"a missing file", command{Name: "tool", Paths: map[string]string{"linux_x64": "bin/missing"}}, "linux_x64", "", "bin/missing does not exist"},
		{"another architecture", command{Name: "tool", Paths: map[string]string{"linux_arm64": linux}}, "linux_arm64", "", "not an executable for linux_arm64"},
		{"another system", command{Name: "tool", Paths: map[string]string{"windows_x64": linux}}, "windows_x64", "", "not an executable for windows_x64"},
		{"a file that is no program", command{Name: "tool", Paths: map[string]string{"linux_x64": "go.mod"}}, "linux_x64", "", "not an executable for linux_x64"},
		{"a package that is no main package", command{Name: "tool", Package: "./lib"}, "linux_x64", "", "./lib is not a main package"},
		{"a package that does not exist", command{Name: "tool", Package: "./missing"}, "linux_x64", "", "tool's package ./missing"},
		{"a file the package holds", command{Name: "tool", Package: "./cmd/tool"}, "linux_x64", "tool", "tool collides with"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.Commands = []command{c.cmd}
			dir := t.TempDir()
			if c.before != "" {
				writeFiles(t, dir, map[string]string{c.before: "resource"})
			}
			err := p.addCommands(t.Context(), c.target, dir)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("error %v, want %q", err, c.err)
			}
		})
	}
}

// The store's packages run only the app's own executable, so they leave the commands out.
func TestStorePackagesLeaveOutCommands(t *testing.T) {
	p := commandFixture(t)
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}}
	p.Updates.Channel = channelStore
	dir := t.TempDir()
	if err := p.addCommands(t.Context(), "linux_x64", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tool")); err == nil {
		t.Error("the store's package holds the command")
	}
	p.Windows.Path = true
	if p.windowsPath() {
		t.Error("the MSIX puts its folder on Path")
	}
}

// codesign signs the commands with the identity, the hardened runtime and a timestamp but none of the app's entitlements, after the libraries and before the bundle, whose signature seals them.
func TestSignMacAppSignsCommandsBeforeBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in codesign is a shell script")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "codesign.log")
	writeFiles(t, bin, map[string]string{"codesign": "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\n"})
	os.Chmod(filepath.Join(bin, "codesign"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	app := filepath.Join(t.TempDir(), "App.app")
	writeFiles(t, app, map[string]string{
		"Contents/MacOS/App":                "app",
		"Contents/MacOS/tool":               "tool",
		"Contents/Frameworks/fixture.dylib": "lib",
	})
	p := &project{Name: "App", ID: "dev.example.app", Release: true}
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}, {Name: "absent", Package: "./cmd/absent"}}
	p.Sign.Identity = "Developer ID Application: Example (ABCDE12345)"
	p.Sign.Notarize = "nsgo"
	p.Apple.Info = map[string]any{"NSMicrophoneUsageDescription": "Calls"}
	if err := signMacApp(t.Context(), p, app); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var order []string
	for _, line := range lines {
		order = append(order, filepath.Base(line[strings.LastIndex(line, " ")+1:]))
	}
	if want := []string{"fixture.dylib", "tool", "App.app"}; !slices.Equal(order, want) {
		t.Fatalf("signed %q, want %q", order, want)
	}
	if want := "--force --sign " + p.Sign.Identity + " --options runtime --timestamp --identifier dev.example.app.tool " + filepath.Join(app, "Contents", "MacOS", "tool"); lines[1] != want {
		t.Errorf("the command is signed with %q, want %q", lines[1], want)
	}
	if !strings.Contains(lines[2], "--entitlements") {
		t.Errorf("the app is signed without its entitlements: %q", lines[2])
	}
}

// The Debian and Arch packages install the commands beside the app and own a link to each in /usr/bin, which removing them removes; the AppImage holds them beside the app.
func TestLinuxPackagesLinkCommands(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"App": "binary", "tool": "command", "dev.example.app.png": "icon"})
	os.Chmod(filepath.Join(dir, "App"), 0o755)
	os.Chmod(filepath.Join(dir, "tool"), 0o755)
	p := &project{Dir: dir, Name: "App", ID: "dev.example.app", Version: "1.0.0"}
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}}
	files, err := linuxPackageFiles(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]linuxPackageEntry{}
	for _, f := range files {
		byName[f.name] = f
	}
	if f := byName["opt/app/tool"]; f.mode != 0o755 || string(f.data) != "command" {
		t.Errorf("opt/app/tool is %+v", f)
	}
	if f := byName["usr/bin/tool"]; f.link != "/opt/app/tool" {
		t.Errorf("usr/bin/tool links to %q", f.link)
	}
	if f := byName["usr/bin/app"]; f.link != "/opt/app/App" {
		t.Errorf("usr/bin/app links to %q", f.link)
	}
	deb, err := debPackage(p, files, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := archPackage(p, files, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for name, archive := range map[string][]byte{"deb": arMembers(t, deb)["data.tar.gz"], "arch": pkg} {
		prefix := ""
		if name == "deb" {
			prefix = "./"
		}
		headers := tarHeaders(t, archive)
		if h := headers[prefix+"usr/bin/tool"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "/opt/app/tool" {
			t.Errorf("the %s package's usr/bin/tool is %+v", name, h)
		}
		if h := headers[prefix+"opt/app/tool"]; h == nil || h.Typeflag != tar.TypeReg || h.Mode != 0o755 {
			t.Errorf("the %s package's opt/app/tool is %+v", name, h)
		}
	}
	entries, err := appDirEntries(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e linuxPackageEntry) bool { return e.name == "tool" && e.mode == 0o755 }) {
		t.Error("the AppImage lacks the command")
	}
	if slices.ContainsFunc(entries, func(e linuxPackageEntry) bool { return strings.HasPrefix(e.name, "usr/bin/") }) {
		t.Error("the AppImage links commands into usr/bin")
	}

	// Without commands, the package links the app alone.
	p.Commands = nil
	files, err = linuxPackageFiles(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(files, func(f linuxPackageEntry) bool { return f.name == "usr/bin/tool" }) {
		t.Error("a package without commands links tool")
	}
}

// tarHeaders reads the headers of a gzip-compressed tar archive by name.
func tarHeaders(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]*tar.Header{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return headers
		}
		if err != nil {
			t.Fatal(err)
		}
		headers[h.Name] = h
	}
}

// The installer installs and removes the commands as the app's files, and with windows.path puts its folder on the user's Path.
func TestNSISScriptCommands(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"App.exe": "exe", "tool.exe": "command"})
	p := &project{Dir: dir, Name: "App", ID: "dev.example.app", Version: "1.0.0"}
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}}
	script := func() string {
		d, err := nsisScript(t.Context(), p, dir, "x64")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := nsisTemplate.Execute(&out, d); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	plain := script()
	if !strings.Contains(plain, `Delete "$INSTDIR\tool.exe"`) {
		t.Error("the uninstaller leaves the command")
	}
	if strings.Contains(plain, "Call UserPath") {
		t.Error("the installer changes Path without windows.path")
	}
	p.Windows.Path = true
	withPath := script()
	for _, want := range []string{"WriteUninstaller \"$INSTDIR\\Uninstall.exe\"\n  Call UserPath\n", "Call un.CloseApp\n  Call un.UserPath\n"} {
		if !strings.Contains(withPath, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(withPath, "App.com") {
		t.Error("windows.path adds the command companion")
	}
}

// A signed Windows package signs the commands as the app's executable, replacing a signature they carried.
func TestSignsWindowsCommands(t *testing.T) {
	image := buildTestPE(t)
	other := testWindowsKey(t)
	signedElsewhere, err := authenticode.Sign(image, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"App.exe": string(image), "tool.exe": string(signedElsewhere)})
	key := testWindowsKey(t)
	cert := filepath.Join(t.TempDir(), "release.p12")
	writeTestCertificate(t, cert, key)
	p := &project{Dir: dir, Name: "App", ID: "dev.example.app", Version: "1.0.0", Release: true}
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}}
	p.Sign.Certificate = cert
	p.Sign.TimestampURL = "none"
	if err := windowsSignerOf(p).signWindowsPackage(t.Context(), p, dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tool.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(data, signedElsewhere) {
		t.Error("the command keeps another signature")
	}
	if !bytes.Contains(data, key.Cert.Raw) {
		t.Error("the command is not signed with the app's certificate")
	}
}

// The installers holding the commands are what the checksums and the appcast describe, as for the app's other files.
func TestCommandsInReleasedPackages(t *testing.T) {
	p, key := updatingProject(t, "")
	t.Setenv("NSGO_UPDATES_KEY", pkg.EncodeSecretKey(key))
	p.Commands = []command{{Name: "tool", Package: "./cmd/tool"}}
	if err := p.cleanReleaseDir(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Hello": "binary", "tool": "command"})
	os.Chmod(filepath.Join(dir, "tool"), 0o755)
	deb, _, err := buildLinuxPackages(t.Context(), p, dir, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.releaseInstaller("linux_x64", deb); err != nil {
		t.Fatal(err)
	}
	released := filepath.Join(p.releaseDir(), filepath.Base(deb))
	data, err := os.ReadFile(released)
	if err != nil {
		t.Fatal(err)
	}
	if got := tarFiles(t, arMembers(t, data)["data.tar.gz"])["./opt/hello/tool"]; got != "command" {
		t.Fatalf("the released package's command is %q", got)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	checksum, err := os.ReadFile(released + ".sha256")
	if err != nil || !strings.HasPrefix(string(checksum), sum) {
		t.Errorf("checksum %q, %v; want %s", checksum, err, sum)
	}
	feed, err := os.ReadFile(filepath.Join(p.releaseDir(), "appcast-linux_x64.xml"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := updates.ParseAppcast(feed)
	if err != nil {
		t.Fatal(err)
	}
	if item.Size != int64(len(data)) || !strings.Contains(string(feed), `sparkle:sha256="`+sum+`"`) {
		t.Errorf("appcast item %+v for a package of %d bytes:\n%s", item, len(data), feed)
	}
}
