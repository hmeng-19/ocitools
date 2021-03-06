package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Sirupsen/logrus"
	"github.com/blang/semver"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/urfave/cli"
)

var bundleValidateFlags = []cli.Flag{
	cli.StringFlag{Name: "path", Value: ".", Usage: "path to a bundle"},
	cli.BoolFlag{Name: "host-specific", Usage: "Check host specific configs."},
}

var (
	defaultRlimits = []string{
		"RLIMIT_CPU",
		"RLIMIT_FSIZE",
		"RLIMIT_DATA",
		"RLIMIT_STACK",
		"RLIMIT_CORE",
		"RLIMIT_RSS",
		"RLIMIT_NPROC",
		"RLIMIT_NOFILE",
		"RLIMIT_MEMLOCK",
		"RLIMIT_AS",
		"RLIMIT_LOCKS",
		"RLIMIT_SIGPENDING",
		"RLIMIT_MSGQUEUE",
		"RLIMIT_NICE",
		"RLIMIT_RTPRIO",
		"RLIMIT_RTTIME",
	}
)

var bundleValidateCommand = cli.Command{
	Name:   "validate",
	Usage:  "validate a OCI bundle",
	Flags:  bundleValidateFlags,
	Before: before,
	Action: func(context *cli.Context) error {
		inputPath := context.String("path")
		if inputPath == "" {
			return fmt.Errorf("Bundle path shouldn't be empty")
		}

		if _, err := os.Stat(inputPath); err != nil {
			return err
		}

		configPath := path.Join(inputPath, "config.json")
		content, err := ioutil.ReadFile(configPath)
		if err != nil {
			return err
		}
		if !utf8.Valid(content) {
			return fmt.Errorf("%q is not encoded in UTF-8", configPath)
		}
		var spec rspec.Spec
		if err = json.Unmarshal(content, &spec); err != nil {
			return err
		}

		rootfsPath := path.Join(inputPath, spec.Root.Path)
		if fi, err := os.Stat(rootfsPath); err != nil {
			return fmt.Errorf("Cannot find the root path %q", rootfsPath)
		} else if !fi.IsDir() {
			return fmt.Errorf("The root path %q is not a directory.", rootfsPath)
		}

		hostCheck := context.Bool("host-specific")
		bundleValidate(spec, rootfsPath, hostCheck)
		logrus.Infof("Bundle validation succeeded.")
		return nil
	},
}

func bundleValidate(spec rspec.Spec, rootfs string, hostCheck bool) {
	checkMandatoryField(spec)
	checkSemVer(spec.Version)
	checkPlatform(spec.Platform)
	checkProcess(spec.Process, rootfs)
	checkMounts(spec, hostCheck)
	checkLinux(spec)
	checkHooks(spec.Hooks, hostCheck)
}

func checkSemVer(version string) {
	_, err := semver.Parse(version)
	if err != nil {
		logrus.Fatalf("%q is not valid SemVer: %s", version, err.Error())
	}
	if version != rspec.Version {
		logrus.Fatalf("internal error: validate currently only handles version %s, but the supplied configuration targets %s", rspec.Version, version)
	}
}

func checkPlatform(platform rspec.Platform) {
	validCombins := map[string][]string{
		"darwin":    {"386", "amd64", "arm", "arm64"},
		"dragonfly": {"amd64"},
		"freebsd":   {"386", "amd64", "arm"},
		"linux":     {"386", "amd64", "arm", "arm64", "ppc64", "ppc64le", "mips64", "mips64le"},
		"netbsd":    {"386", "amd64", "arm"},
		"openbsd":   {"386", "amd64", "arm"},
		"plan9":     {"386", "amd64"},
		"solaris":   {"amd64"},
		"windows":   {"386", "amd64"}}
	for os, archs := range validCombins {
		if os == platform.OS {
			for _, arch := range archs {
				if arch == platform.Arch {
					return
				}
			}
			logrus.Fatalf("Combination of %q and %q is invalid.", platform.OS, platform.Arch)
		}
	}
	logrus.Fatalf("Operation system %q of the bundle is not supported yet.", platform.OS)
}

func checkHooks(hooks rspec.Hooks, hostCheck bool) {
	checkEventHooks("pre-start", hooks.Prestart, hostCheck)
	checkEventHooks("post-start", hooks.Poststart, hostCheck)
	checkEventHooks("post-stop", hooks.Poststop, hostCheck)
}

func checkEventHooks(hookType string, hooks []rspec.Hook, hostCheck bool) {
	for _, hook := range hooks {
		if !filepath.IsAbs(hook.Path) {
			logrus.Fatalf("The %s hook %v: is not absolute path", hookType, hook.Path)
		}

		if hostCheck {
			fi, err := os.Stat(hook.Path)
			if err != nil {
				logrus.Fatalf("Cannot find %s hook: %v", hookType, hook.Path)
			}
			if fi.Mode()&0111 == 0 {
				logrus.Fatalf("The %s hook %v: is not executable", hookType, hook.Path)
			}
		}

		for _, env := range hook.Env {
			if !envValid(env) {
				logrus.Fatalf("Env %q for hook %v is in the invalid form.", env, hook.Path)
			}
		}
	}
}

func checkProcess(process rspec.Process, rootfs string) {
	if !path.IsAbs(process.Cwd) {
		logrus.Fatalf("cwd %q is not an absolute path", process.Cwd)
	}

	for _, env := range process.Env {
		if !envValid(env) {
			logrus.Fatalf("env %q should be in the form of 'key=value'. The left hand side must consist solely of letters, digits, and underscores '_'.", env)
		}
	}

	for index := 0; index < len(process.Capabilities); index++ {
		capability := process.Capabilities[index]
		if !capValid(capability) {
			logrus.Fatalf("capability %q is not valid, man capabilities(7)", process.Capabilities[index])
		}
	}

	for index := 0; index < len(process.Rlimits); index++ {
		if !rlimitValid(process.Rlimits[index].Type) {
			logrus.Fatalf("rlimit type %q is invalid.", process.Rlimits[index].Type)
		}
	}

	if len(process.ApparmorProfile) > 0 {
		profilePath := path.Join(rootfs, "/etc/apparmor.d", process.ApparmorProfile)
		_, err := os.Stat(profilePath)
		if err != nil {
			logrus.Fatal(err)
		}
	}
}

func supportedMountTypes(OS string, hostCheck bool) (map[string]bool, error) {
	supportedTypes := make(map[string]bool)

	if OS != "linux" && OS != "windows" {
		logrus.Warnf("%v is not supported to check mount type", OS)
		return nil, nil
	} else if OS == "windows" {
		supportedTypes["ntfs"] = true
		return supportedTypes, nil
	}

	if hostCheck {
		f, err := os.Open("/proc/filesystems")
		if err != nil {
			return nil, err
		}
		defer f.Close()

		s := bufio.NewScanner(f)
		for s.Scan() {
			if err := s.Err(); err != nil {
				return supportedTypes, err
			}

			text := s.Text()
			parts := strings.Split(text, "\t")
			if len(parts) > 1 {
				supportedTypes[parts[1]] = true
			} else {
				supportedTypes[parts[0]] = true
			}
		}

		supportedTypes["bind"] = true

		return supportedTypes, nil
	} else {
		logrus.Warn("Checking linux mount types without --host-specific is not supported yet")
		return nil, nil
	}
}

func checkMounts(spec rspec.Spec, hostCheck bool) {
	supportedTypes, err := supportedMountTypes(spec.Platform.OS, hostCheck)
	if err != nil {
		logrus.Fatal(err)
	}

	if supportedTypes != nil {
		for _, mount := range spec.Mounts {
			if !supportedTypes[mount.Type] {
				logrus.Fatalf("Unsupported mount type %q", mount.Type)
			}
		}
	}
}

//Linux only
func checkLinux(spec rspec.Spec) {
	utsExists := false
	ipcExists := false
	mountExists := false
	netExists := false

	if len(spec.Linux.UIDMappings) > 5 {
		logrus.Fatalf("Only 5 UID mappings are allowed (linux kernel restriction).")
	}
	if len(spec.Linux.GIDMappings) > 5 {
		logrus.Fatalf("Only 5 GID mappings are allowed (linux kernel restriction).")
	}

	for index := 0; index < len(spec.Linux.Namespaces); index++ {
		if !namespaceValid(spec.Linux.Namespaces[index]) {
			logrus.Fatalf("namespace %v is invalid.", spec.Linux.Namespaces[index])
		} else if len(spec.Linux.Namespaces[index].Path) == 0 {
			if spec.Linux.Namespaces[index].Type == rspec.UTSNamespace {
				utsExists = true
			} else if spec.Linux.Namespaces[index].Type == rspec.IPCNamespace {
				ipcExists = true
			} else if spec.Linux.Namespaces[index].Type == rspec.NetworkNamespace {
				netExists = true
			} else if spec.Linux.Namespaces[index].Type == rspec.MountNamespace {
				mountExists = true
			}
		}
	}

	for k := range spec.Linux.Sysctl {
		if strings.HasPrefix(k, "net.") && !netExists {
			logrus.Fatalf("Sysctl %v requires a new Network namespace to be specified as well", k)
		}
		if strings.HasPrefix(k, "fs.mqueue.") {
			if !mountExists || !ipcExists {
				logrus.Fatalf("Sysctl %v requires a new IPC namespace and Mount namespace to be specified as well", k)
			}
		}
	}

	if spec.Platform.OS == "linux" && !utsExists && spec.Hostname != "" {
		logrus.Fatalf("On Linux, hostname requires a new UTS namespace to be specified as well")
	}

	for index := 0; index < len(spec.Linux.Devices); index++ {
		if !deviceValid(spec.Linux.Devices[index]) {
			logrus.Fatalf("device %v is invalid.", spec.Linux.Devices[index])
		}
	}

	if spec.Linux.Seccomp != nil {
		checkSeccomp(*spec.Linux.Seccomp)
	}

	switch spec.Linux.RootfsPropagation {
	case "":
	case "private":
	case "rprivate":
	case "slave":
	case "rslave":
	case "shared":
	case "rshared":
	default:
		logrus.Fatalf("rootfsPropagation must be empty or one of \"private|rprivate|slave|rslave|shared|rshared\"")
	}
}

func checkSeccomp(s rspec.Seccomp) {
	if !seccompActionValid(s.DefaultAction) {
		logrus.Fatalf("seccomp defaultAction %q is invalid.", s.DefaultAction)
	}
	for index := 0; index < len(s.Syscalls); index++ {
		if !syscallValid(s.Syscalls[index]) {
			logrus.Fatalf("syscall %v is invalid.", s.Syscalls[index])
		}
	}
	for index := 0; index < len(s.Architectures); index++ {
		switch s.Architectures[index] {
		case rspec.ArchX86:
		case rspec.ArchX86_64:
		case rspec.ArchX32:
		case rspec.ArchARM:
		case rspec.ArchAARCH64:
		case rspec.ArchMIPS:
		case rspec.ArchMIPS64:
		case rspec.ArchMIPS64N32:
		case rspec.ArchMIPSEL:
		case rspec.ArchMIPSEL64:
		case rspec.ArchMIPSEL64N32:
		case rspec.ArchPPC:
		case rspec.ArchPPC64:
		case rspec.ArchPPC64LE:
		case rspec.ArchS390:
		case rspec.ArchS390X:
		default:
			logrus.Fatalf("seccomp architecture %q is invalid", s.Architectures[index])
		}
	}
}

func envValid(env string) bool {
	items := strings.Split(env, "=")
	if len(items) < 2 {
		return false
	}
	for i, ch := range strings.TrimSpace(items[0]) {
		if !unicode.IsDigit(ch) && !unicode.IsLetter(ch) && ch != '_' {
			return false
		}
		if i == 0 && unicode.IsDigit(ch) {
			logrus.Warnf("Env %v: variable name beginning with digit is not recommended.", env)
		}
	}
	return true
}

func capValid(capability string) bool {
	for _, val := range defaultCaps {
		if val == capability {
			return true
		}
	}
	return false
}

func rlimitValid(rlimit string) bool {
	for _, val := range defaultRlimits {
		if val == rlimit {
			return true
		}
	}
	return false
}

func namespaceValid(ns rspec.Namespace) bool {
	switch ns.Type {
	case rspec.PIDNamespace:
	case rspec.NetworkNamespace:
	case rspec.MountNamespace:
	case rspec.IPCNamespace:
	case rspec.UTSNamespace:
	case rspec.UserNamespace:
	case rspec.CgroupNamespace:
	default:
		return false
	}
	return true
}

func deviceValid(d rspec.Device) bool {
	switch d.Type {
	case "b":
	case "c":
	case "u":
		if d.Major <= 0 {
			return false
		}
		if d.Minor <= 0 {
			return false
		}
	case "p":
		if d.Major > 0 || d.Minor > 0 {
			return false
		}
	default:
		return false
	}
	return true
}

func seccompActionValid(secc rspec.Action) bool {
	switch secc {
	case "":
	case rspec.ActKill:
	case rspec.ActTrap:
	case rspec.ActErrno:
	case rspec.ActTrace:
	case rspec.ActAllow:
	default:
		return false
	}
	return true
}

func syscallValid(s rspec.Syscall) bool {
	if !seccompActionValid(s.Action) {
		return false
	}
	for index := 0; index < len(s.Args); index++ {
		arg := s.Args[index]
		switch arg.Op {
		case rspec.OpNotEqual:
		case rspec.OpLessThan:
		case rspec.OpLessEqual:
		case rspec.OpEqualTo:
		case rspec.OpGreaterEqual:
		case rspec.OpGreaterThan:
		case rspec.OpMaskedEqual:
		default:
			return false
		}
	}
	return true
}

func isStruct(t reflect.Type) bool {
	return t.Kind() == reflect.Struct
}

func isStructPtr(t reflect.Type) bool {
	return t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Struct
}

func checkMandatoryUnit(field reflect.Value, tagField reflect.StructField, parent string) ([]string, bool) {
	var msgs []string
	mandatory := !strings.Contains(tagField.Tag.Get("json"), "omitempty")
	switch field.Kind() {
	case reflect.Ptr:
		if mandatory && field.IsNil() {
			msgs = append(msgs, fmt.Sprintf("'%s.%s' should not be empty.", parent, tagField.Name))
			return msgs, false
		}
	case reflect.String:
		if mandatory && (field.Len() == 0) {
			msgs = append(msgs, fmt.Sprintf("'%s.%s' should not be empty.", parent, tagField.Name))
			return msgs, false
		}
	case reflect.Slice:
		if mandatory && (field.IsNil() || field.Len() == 0) {
			msgs = append(msgs, fmt.Sprintf("'%s.%s' should not be empty.", parent, tagField.Name))
			return msgs, false
		}
		valid := true
		for index := 0; index < field.Len(); index++ {
			mValue := field.Index(index)
			if mValue.CanInterface() {
				if ms, ok := checkMandatory(mValue.Interface()); !ok {
					msgs = append(msgs, ms...)
					valid = false
				}
			}
		}
		return msgs, valid
	case reflect.Map:
		if mandatory && (field.IsNil() || field.Len() == 0) {
			msgs = append(msgs, fmt.Sprintf("'%s.%s' should not be empty.", parent, tagField.Name))
			return msgs, false
		}
		valid := true
		keys := field.MapKeys()
		for index := 0; index < len(keys); index++ {
			mValue := field.MapIndex(keys[index])
			if mValue.CanInterface() {
				if ms, ok := checkMandatory(mValue.Interface()); !ok {
					msgs = append(msgs, ms...)
					valid = false
				}
			}
		}
		return msgs, valid
	default:
	}

	return nil, true
}

func checkMandatory(obj interface{}) (msgs []string, valid bool) {
	objT := reflect.TypeOf(obj)
	objV := reflect.ValueOf(obj)
	if isStructPtr(objT) {
		objT = objT.Elem()
		objV = objV.Elem()
	} else if !isStruct(objT) {
		return nil, true
	}

	valid = true
	for i := 0; i < objT.NumField(); i++ {
		t := objT.Field(i).Type
		if isStructPtr(t) && objV.Field(i).IsNil() {
			if !strings.Contains(objT.Field(i).Tag.Get("json"), "omitempty") {
				msgs = append(msgs, fmt.Sprintf("'%s.%s' should not be empty", objT.Name(), objT.Field(i).Name))
				valid = false
			}
		} else if (isStruct(t) || isStructPtr(t)) && objV.Field(i).CanInterface() {
			if ms, ok := checkMandatory(objV.Field(i).Interface()); !ok {
				msgs = append(msgs, ms...)
				valid = false
			}
		} else {
			if ms, ok := checkMandatoryUnit(objV.Field(i), objT.Field(i), objT.Name()); !ok {
				msgs = append(msgs, ms...)
				valid = false
			}
		}

	}
	return msgs, valid
}

func checkMandatoryField(obj interface{}) {
	if msgs, valid := checkMandatory(obj); !valid {
		logrus.Fatalf("Mandatory information missing: %s.", msgs)
	}
}
