package main

import (
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Commands are command-line programs an app ships beside its desktop
// executable, such as a CLI working on the app's data: nsgo.yaml's
// commands, each built from a Go main package for the target, as the app
// is, or copied from a prebuilt file per target. A macOS app holds them in
// Contents/MacOS, signed before the bundle as its libraries are; Windows
// and Linux packages hold them beside the executable, where the installers
// take every file. The Linux packages link each into /usr/bin, and the
// Windows installer puts its folder on the user's Path with windows.path. Store packages leave them
// out: the Mac App Store's sandbox, the MSIX and the Flatpak each run only
// the app's own executable.

// command is one of nsgo.yaml's commands: Name is its file's name, with
// .exe on Windows, and it comes from exactly one of Package, a Go main
// package built with the app's GOOS and GOARCH and CGO_ENABLED=0, and
// Paths, a prebuilt executable for each desktop target, relative to
// nsgo.yaml. LDFlags are the package's own linker flags, each as go
// build's -ldflags takes it, after the app's build.ldflags (commandLdflags).
type command struct {
	Name    string            `yaml:"name"`
	Package string            `yaml:"package"`
	Paths   map[string]string `yaml:"paths"`
	LDFlags []string          `yaml:"ldflags"`
}

// commandTargets are the targets whose packages hold commands, for which
// commands' paths name files.
var commandTargets = []string{"macos_arm64", "macos_x64", "windows_arm64", "windows_x64", "linux_arm64", "linux_x64"}

// validateCommands checks commands' names, which become file names in
// every desktop package and links in /usr/bin, and their sources.
func (p *project) validateCommands() error {
	for i, c := range p.Commands {
		if !validCommandName(c.Name) {
			return fmt.Errorf("commands entry %d: name %q must be letters, digits, '-' and '_', starting with a letter or a digit", i+1, c.Name)
		}
		base := strings.ToUpper(c.Name)
		if slices.Contains([]string{"CON", "PRN", "AUX", "NUL"}, base) || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return fmt.Errorf("commands: %s is a reserved Windows file name", c.Name)
		}
		// The app's executable, its Linux command, the Windows uninstaller and the AppImage's entry point are files of the same names.
		for _, taken := range []string{p.Name, p.packageSlug(), "Uninstall", "AppRun"} {
			if strings.EqualFold(c.Name, taken) {
				return fmt.Errorf("commands: %s is the name of the app's own %s; name the command another way", c.Name, takenFile(p, taken))
			}
		}
		for _, other := range p.Commands[:i] {
			// macOS and Windows file systems ignore case.
			if strings.EqualFold(c.Name, other.Name) {
				return fmt.Errorf("commands: %s and %s name the same file", other.Name, c.Name)
			}
		}
		if (c.Package == "") == (len(c.Paths) == 0) {
			return fmt.Errorf("commands: %s needs exactly one of package and paths", c.Name)
		}
		if len(c.LDFlags) > 0 && c.Package == "" {
			return fmt.Errorf("commands: %s's ldflags need a package to link; a prebuilt file is linked already", c.Name)
		}
		if c.Package != "" && (strings.HasPrefix(c.Package, "-") || strings.Contains(c.Package, "...") || strings.ContainsAny(c.Package, " \t\r\n")) {
			return fmt.Errorf("commands: %s's package %q is not one Go package's path", c.Name, c.Package)
		}
		for _, target := range slices.Sorted(maps.Keys(c.Paths)) {
			if !slices.Contains(commandTargets, target) {
				return fmt.Errorf("commands: %s's paths name %q; use %s", c.Name, target, strings.Join(commandTargets, ", "))
			}
			if c.Paths[target] == "" {
				return fmt.Errorf("commands: %s's %s path is empty", c.Name, target)
			}
		}
	}
	return nil
}

// validCommandName reports whether name is letters, digits, '-' and '_' and starts with a letter or a digit, so it is a file name on every system, without an extension any package treats as its own.
func validCommandName(name string) bool {
	for i, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '-' || r == '_')) {
			return false
		}
	}
	return name != ""
}

// takenFile says what of the app's is named taken.
func takenFile(p *project, taken string) string {
	switch taken {
	case p.Name:
		return "executable"
	case p.packageSlug():
		return "command in /usr/bin"
	case "Uninstall":
		return "Windows uninstaller"
	}
	return "AppImage entry point"
}

// commandsIncluded reports whether the target's packages hold the app's commands: every build but a store's.
func (p *project) commandsIncluded() bool {
	return len(p.Commands) > 0 && p.channel() != channelStore
}

// commandFile is the file name of the command name on target's system, which target may name alone, as windows.
func commandFile(name, target string) string {
	if system, _, _ := strings.Cut(target, "_"); system == "windows" {
		return name + ".exe"
	}
	return name
}

// commandFiles are the paths in dir of the commands a package for target holds.
func (p *project) commandFiles(dir, target string) []string {
	if !p.commandsIncluded() {
		return nil
	}
	var paths []string
	for _, c := range p.Commands {
		path := filepath.Join(dir, commandFile(c.Name, target))
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			paths = append(paths, path)
		}
	}
	return paths
}

// addCommands builds or copies the app's commands for target, a desktop architecture's, into dir, where nothing of the package may have their names.
func (p *project) addCommands(ctx context.Context, target, dir string) error {
	if len(p.Commands) == 0 {
		return nil
	}
	if !p.commandsIncluded() {
		detailIn(ctx, "the store's package leaves out the app's commands, which only the app's own executable runs in")
		return nil
	}
	stage(ctx, "adding the commands")
	for _, c := range p.Commands {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(dir, commandFile(c.Name, target))
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("commands: %s collides with %s, which the package holds already", c.Name, rel(dst))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if c.Package != "" {
			if err := p.buildCommand(ctx, c, target, dst); err != nil {
				return err
			}
		} else if err := p.copyCommand(ctx, c, target, dst); err != nil {
			return err
		}
		if err := validateExecutable(dst, target); err != nil {
			return fmt.Errorf("commands: %s: %w", c.Name, err)
		}
	}
	return nil
}

// buildCommand builds c's Go package for target into out, as the app is built: with CGO_ENABLED=0 and the app's build tags, stripped and without file paths for a release.
func (p *project) buildCommand(ctx context.Context, c command, target, out string) error {
	system, arch, _ := strings.Cut(target, "_")
	goos := system
	if goos == "macos" {
		goos = "darwin"
	}
	env := []string{"GOOS=" + goos, "GOARCH=" + goArch(arch), "CGO_ENABLED=0"}
	tags, err := goTags(ctx, p, env)
	if err != nil {
		return err
	}
	list := exec.CommandContext(ctx, "go", append(append([]string{"list"}, tagsArgs(tags)...), "-f", "{{.Name}}", "--", c.Package)...)
	list.Dir, list.Env = p.Dir, append(os.Environ(), env...)
	name, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("commands: %s's package %s: %s", c.Name, c.Package, strings.TrimSpace(string(exit.Stderr)))
		}
		return fmt.Errorf("commands: %s's package %s: %w", c.Name, c.Package, err)
	}
	if strings.TrimSpace(string(name)) != "main" {
		return fmt.Errorf("commands: %s's package %s is not a main package", c.Name, c.Package)
	}
	args := append([]string{"build"}, tagsArgs(tags)...)
	var ldflags []string
	if p.Release {
		args = append(args, "-trimpath", "-buildvcs=false")
		ldflags = append(ldflags, "-s", "-w")
	}
	args = append(args, "-ldflags", p.commandLdflags(c, ldflags...))
	build := exec.CommandContext(ctx, "go", append(args, "-o", out, "--", c.Package)...)
	build.Dir, build.Env = p.Dir, append(os.Environ(), env...)
	if data, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("commands: go build %s failed\n%s", c.Package, strings.TrimSpace(string(data)))
	}
	return nil
}

// commandLdflags is the -ldflags value c's package links with: the app's own linker flags, build.ldflags and -ldflags, then c's, so its -X values win over the app's, then the identity the app takes in core.App, which a command reads as the app does and neither replaces, and extra, the tool's.
func (p *project) commandLdflags(c command, extra ...string) string {
	flags := append(append([]string{}, p.Build.LDFlags...), c.LDFlags...)
	flags = append(flags, p.identityLdflags())
	if p.Release {
		flags = append(flags, "-X "+corePath+".appRelease=1")
	}
	return strings.Join(append(flags, extra...), " ")
}

// copyCommand copies c's prebuilt file for target to dst as an executable, thinning a universal macOS one to target's architecture.
func (p *project) copyCommand(ctx context.Context, c command, target, dst string) error {
	path, ok := c.Paths[target]
	if !ok {
		return fmt.Errorf("commands: %s has no path for %s", c.Name, target)
	}
	src := p.resolve(path)
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("commands: %s's %s file %s does not exist", c.Name, target, path)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("commands: %s's %s file %s is not a regular file", c.Name, target, path)
	}
	if err := validateExecutable(src, target); err != nil {
		return fmt.Errorf("commands: %s's %s file %s: %w", c.Name, target, path, err)
	}
	if err := copyExecutable(src, dst); err != nil {
		return err
	}
	if strings.HasPrefix(target, "macos_") {
		if fat, err := macho.OpenFat(dst); err == nil {
			multiple := len(fat.Arches) > 1
			fat.Close()
			if multiple {
				arch := "arm64"
				if target == "macos_x64" {
					arch = "x86_64"
				}
				return run(ctx, "lipo", "-thin", arch, dst, "-output", dst)
			}
		}
	}
	return nil
}

// copyExecutable copies src to dst with the mode of an executable.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// validateExecutable rejects a program for another OS or architecture, or a file that is not a program, before signing or making an installer. A universal Mach-O may supply either slice.
func validateExecutable(path, target string) error {
	system, arch, _ := strings.Cut(target, "_")
	switch system {
	case "macos":
		cpu := macho.CpuAmd64
		if arch == "arm64" {
			cpu = macho.CpuArm64
		}
		if fat, err := macho.OpenFat(path); err == nil {
			defer fat.Close()
			for _, a := range fat.Arches {
				if a.Cpu == cpu && a.Magic == macho.Magic64 && a.Type == macho.TypeExec {
					return nil
				}
			}
		} else if f, err := macho.Open(path); err == nil {
			defer f.Close()
			if f.Cpu == cpu && f.Magic == macho.Magic64 && f.Type == macho.TypeExec {
				return nil
			}
		}
	case "windows":
		machine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
		if arch == "arm64" {
			machine = pe.IMAGE_FILE_MACHINE_ARM64
		}
		if f, err := pe.Open(path); err == nil {
			defer f.Close()
			_, bits64 := f.OptionalHeader.(*pe.OptionalHeader64)
			if f.Machine == machine && bits64 && f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE != 0 && f.Characteristics&pe.IMAGE_FILE_DLL == 0 {
				return nil
			}
		}
	case "linux":
		machine := elf.EM_X86_64
		if arch == "arm64" {
			machine = elf.EM_AARCH64
		}
		if f, err := elf.Open(path); err == nil {
			defer f.Close()
			if f.Machine == machine && f.Class == elf.ELFCLASS64 && f.Data == elf.ELFDATA2LSB && (f.OSABI == elf.ELFOSABI_NONE || f.OSABI == elf.ELFOSABI_LINUX) && linuxExecutable(f) {
				return nil
			}
		}
	}
	return fmt.Errorf("not an executable for %s", target)
}

// linuxExecutable reports whether f is a program rather than a shared library: a fixed executable, or a position-independent one, which has an interpreter or, static, says it is one.
func linuxExecutable(f *elf.File) bool {
	switch f.Type {
	case elf.ET_EXEC:
		return true
	case elf.ET_DYN:
		if slices.ContainsFunc(f.Progs, func(p *elf.Prog) bool { return p.Type == elf.PT_INTERP }) {
			return true
		}
		flags, err := f.DynValue(elf.DT_FLAGS_1)
		return err == nil && len(flags) > 0 && elf.DynFlag1(flags[0])&elf.DF_1_PIE != 0
	}
	return false
}

// signMacCommands signs the commands in a macOS app's Contents/MacOS, which the app's signature seals, with codesign's arguments for each from args, and <id>.<name> as each one's identifier in place of the one the linker gave it, a hash of its contents. They take no entitlements: those are the app's, which a command run from a terminal is not.
func (p *project) signMacCommands(ctx context.Context, app string, args func(path string) []string) error {
	for _, path := range p.commandFiles(filepath.Join(app, "Contents", "MacOS"), "macos") {
		a := args(path)
		a = append(a[:len(a)-1:len(a)-1], "--identifier", p.ID+"."+filepath.Base(path), path)
		if err := run(ctx, "codesign", a...); err != nil {
			return err
		}
	}
	return nil
}
