package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var routineReadCommands = map[string]bool{
	"cat": true, "command": true, "date": true, "diff": true, "echo": true, "env": true,
	"file": true, "find": true, "grep": true, "head": true, "hostname": true, "id": true,
	"ls": true, "printenv": true, "printf": true, "pwd": true, "rg": true, "sort": true,
	"stat": true, "tail": true, "tree": true, "true": true, "uname": true, "uniq": true,
	"wc": true, "which": true, "whoami": true,
}

var routineDevCommands = map[string]bool{
	"bun": true, "cargo": true, "cmake": true, "deno": true, "eslint": true, "gcc": true,
	"g++": true, "go": true, "gofmt": true, "gofumpt": true, "gradle": true, "javac": true,
	"make": true, "mvn": true, "node": true, "npm": true, "pnpm": true, "prettier": true,
	"pytest": true, "python": true, "python3": true, "tsc": true, "yarn": true,
}

var workspaceMutationCommands = map[string]bool{
	"chmod": true, "chown": true, "chgrp": true, "cp": true, "install": true, "ln": true,
	"mkdir": true, "mv": true, "rm": true, "rmdir": true, "touch": true, "truncate": true,
}

var highRiskCommands = map[string]bool{
	"apt": true, "apt-get": true, "at": true, "bash": true, "brew": true, "crontab": true,
	"curl": true, "dd": true, "dnf": true, "doas": true, "docker": true, "eval": true,
	"exec": true, "fdisk": true, "flatpak": true, "halt": true, "helm": true, "kill": true,
	"killall": true, "kubectl": true, "mkfs": true, "mount": true, "nc": true, "pacman": true,
	"parted": true, "passwd": true, "ping": true, "pkill": true, "podman": true, "poweroff": true,
	"reboot": true, "rsync": true, "scp": true, "service": true, "sftp": true, "sh": true,
	"shutdown": true, "snap": true, "ssh": true, "su": true, "sudo": true, "systemctl": true,
	"umount": true, "useradd": true, "userdel": true, "wget": true, "xargs": true, "yum": true,
	"zsh": true,
}

var gitReadSubcommands = map[string]bool{
	"blame": true, "describe": true, "diff": true, "log": true, "ls-files": true,
	"rev-parse": true, "show": true, "status": true,
}

var writeSubcommands = map[string]map[string]bool{
	"cargo":    {"install": true, "publish": true},
	"composer": {"global": true, "install": true, "require": true, "update": true},
	"gem":      {"install": true, "push": true},
	"go":       {"get": true, "install": true},
	"npm":      {"add": true, "ci": true, "install": true, "i": true, "login": true, "logout": true, "publish": true, "remove": true, "uninstall": true},
	"pip":      {"install": true, "uninstall": true},
	"pip3":     {"install": true, "uninstall": true},
	"pnpm":     {"add": true, "install": true, "publish": true, "remove": true, "uninstall": true},
	"yarn":     {"add": true, "install": true, "publish": true, "remove": true, "uninstall": true},
}

func autoDecision(req Request, workingDir string) Decision {
	if isEditTool(req.ToolName) {
		return Allowed()
	}
	if !strings.EqualFold(req.ToolName, "bash") {
		return AskUser(fmt.Sprintf("non-read-only tool %q requires review in auto mode", req.ToolName))
	}
	command, ok := bashCommand(req)
	if !ok {
		return AskUser("bash command requires review in auto mode")
	}
	allowed, reason := routineBashCommand(command, workingDir)
	if allowed {
		return Allowed()
	}
	return AskUser(reason)
}

func routineBashCommand(command, workingDir string) (bool, string) {
	words, ok := shellWords(command)
	if !ok || len(words) == 0 {
		return false, "shell syntax requires review"
	}
	name, args, ok := commandInvocation(words)
	if !ok {
		return false, "command requires review"
	}
	if highRiskCommands[name] {
		return false, fmt.Sprintf("command %q is high-risk and requires review", name)
	}
	if name == "git" {
		return classifyGitCommand(args)
	}
	if name == "find" {
		return classifyFindCommand(args, workingDir)
	}
	if name == "sed" {
		return classifySedCommand(args, workingDir)
	}
	if isWriteSubcommand(name, args) {
		return false, fmt.Sprintf("command %q modifies dependencies or requires network access", name)
	}
	if workspaceMutationCommands[name] {
		return classifyWorkspaceMutation(name, args, workingDir)
	}
	if routineReadCommands[name] || routineDevCommands[name] {
		return true, ""
	}
	return false, fmt.Sprintf("unknown command %q requires review", name)
}

func classifyGitCommand(args []string) (bool, string) {
	if len(args) == 0 {
		return false, "git command requires review"
	}
	subcommand := strings.ToLower(args[0])
	if gitReadSubcommands[subcommand] {
		return true, ""
	}
	return false, fmt.Sprintf("git subcommand %q requires review", subcommand)
}

func classifyFindCommand(args []string, workingDir string) (bool, string) {
	for _, arg := range args {
		switch arg {
		case "-exec", "-execdir", "-ok", "-okdir":
			return false, fmt.Sprintf("find %s executes commands and requires review", arg)
		case "-fls", "-fprint", "-fprint0", "-fprintf":
			return false, fmt.Sprintf("find %s writes files and requires review", arg)
		}
	}
	deletes := false
	for _, arg := range args {
		if arg == "-delete" {
			deletes = true
			break
		}
	}
	if !deletes {
		return true, ""
	}
	roots := findRoots(args)
	if len(roots) == 0 {
		roots = []string{"."}
	}
	return classifyWorkspacePaths("find -delete", roots, workingDir)
}

// findRoots 提取 find 的根路径操作数：先跳过 -H/-L/-P/-D/-O 全局选项，
// 再收集表达式（首个以 - 开头的 token）之前的非选项参数。
func findRoots(args []string) []string {
	i := 0
globals:
	for i < len(args) {
		switch arg := args[i]; {
		case arg == "-H" || arg == "-L" || arg == "-P":
			i++
		case arg == "-D":
			// -D 的 debugopts 是独立参数。
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i += 2
			} else {
				i++
			}
		case strings.HasPrefix(arg, "-O"):
			// -Olevel 优化级别内嵌在选项中。
			i++
		default:
			break globals
		}
	}
	var roots []string
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		roots = append(roots, args[i])
		i++
	}
	return roots
}

func classifySedCommand(args []string, workingDir string) (bool, string) {
	inline := false
	for _, arg := range args {
		if arg == "-i" || strings.HasPrefix(arg, "-i") || arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			inline = true
			break
		}
	}
	if !inline {
		return true, ""
	}
	var paths []string
	scriptSeen := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if arg == "-e" || arg == "-f" {
				return false, "sed option arguments require review"
			}
			continue
		}
		if !scriptSeen {
			scriptSeen = true
			continue
		}
		paths = append(paths, arg)
	}
	return classifyWorkspacePaths("sed -i", paths, workingDir)
}

func classifyWorkspaceMutation(name string, args []string, workingDir string) (bool, string) {
	for _, arg := range args {
		// --opt=value 形式会把路径内嵌在选项中，绕过路径提取。
		if strings.HasPrefix(arg, "--") && strings.Contains(arg, "=") {
			return false, fmt.Sprintf("command %q option %q embeds a value and requires review", name, arg)
		}
		if embedsTargetDirectory(name, arg) {
			return false, fmt.Sprintf("command %q option %q embeds a target directory and requires review", name, arg)
		}
	}
	paths := mutationPaths(name, args)
	return classifyWorkspacePaths(name, paths, workingDir)
}

// targetDirectoryOptions 列出接受 -t DIR 短选项的命令；DIR 可内嵌在选项中（如 cp -t/tmp）。
var targetDirectoryOptions = map[string]byte{
	"cp": 't', "install": 't', "ln": 't', "mv": 't',
}

// embedsTargetDirectory 报告短选项组中 -t 之后仍带字符的情况（值内嵌，
// 如 -t/tmp 或 -ft/tmp）；单独的 -t/-ft 的 DIR 是独立参数，会作为路径校验。
func embedsTargetDirectory(name, arg string) bool {
	letter, ok := targetDirectoryOptions[name]
	if !ok || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	idx := strings.IndexByte(arg[1:], letter)
	return idx >= 0 && idx+1 < len(arg)-1
}

func mutationPaths(name string, args []string) []string {
	var paths []string
	operandSkipped := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && arg != "-" {
			continue
		}
		if (name == "chmod" || name == "chown" || name == "chgrp") && !operandSkipped {
			operandSkipped = true
			continue
		}
		paths = append(paths, arg)
	}
	return paths
}

func classifyWorkspacePaths(name string, paths []string, workingDir string) (bool, string) {
	if len(paths) == 0 {
		return false, fmt.Sprintf("command %q has no file target and requires review", name)
	}
	inside, err := pathsWithinWorkspace(workingDir, paths)
	if err != nil {
		return false, fmt.Sprintf("command %q path resolution failed: %v", name, err)
	}
	if !inside {
		return false, fmt.Sprintf("command %q modifies files outside the workspace", name)
	}
	return true, ""
}

func pathsWithinWorkspace(workingDir string, paths []string) (bool, error) {
	root, err := commandWorkspaceRoot(workingDir)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		resolved, err := resolveCommandPath(root, path)
		if err != nil {
			return false, err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || !filepath.IsLocal(rel) {
			return false, nil
		}
	}
	return true, nil
}

func commandWorkspaceRoot(workingDir string) (string, error) {
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(workingDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func resolveCommandPath(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveCommandPath(root, parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func shellWords(command string) ([]string, bool) {
	if hasUnsafeCommandSyntax(command) {
		return nil, false
	}
	var words []string
	var word strings.Builder
	var singleQuoted, doubleQuoted, escaped bool
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range command {
		if escaped {
			word.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if singleQuoted {
			if r == '\'' {
				singleQuoted = false
				continue
			}
			word.WriteRune(r)
			continue
		}
		if r == '\'' {
			singleQuoted = true
			continue
		}
		if doubleQuoted {
			if r == '"' {
				doubleQuoted = false
				continue
			}
			word.WriteRune(r)
			continue
		}
		if r == '"' {
			doubleQuoted = true
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		word.WriteRune(r)
	}
	flush()
	if singleQuoted || doubleQuoted || escaped {
		return nil, false
	}
	return words, true
}

func commandInvocation(words []string) (string, []string, bool) {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	if i < len(words) && words[i] == "env" {
		i++
		for i < len(words) && strings.HasPrefix(words[i], "-") {
			i++
		}
		for i < len(words) && isEnvAssignment(words[i]) {
			i++
		}
	}
	if i < len(words) && words[i] == "command" {
		i++
	}
	if i >= len(words) {
		return "", nil, false
	}
	name := strings.ToLower(filepath.Base(words[i]))
	return name, words[i+1:], true
}

func isEnvAssignment(word string) bool {
	if word == "" || !(unicode.IsLetter(rune(word[0])) || word[0] == '_') {
		return false
	}
	for i := 1; i < len(word); i++ {
		r := rune(word[i])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			continue
		}
		return r == '=' && i > 0
	}
	return false
}

func isWriteSubcommand(name string, args []string) bool {
	subcommands := writeSubcommands[name]
	if len(subcommands) == 0 || len(args) == 0 {
		return false
	}
	return subcommands[strings.ToLower(args[0])]
}
