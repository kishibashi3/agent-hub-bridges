// bridge spawn — individually spawn a bridge worker.
//
// Usage: agenthubctl bridge spawn --user <handle> [flags]
//
// Issue: #150
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	bridges "github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/bridges"
)

// handleRe matches valid handle/template names (no path traversal).
var handleRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// runBridgeCmd dispatches bridge sub-commands.
func runBridgeCmd(args []string) error {
	if len(args) == 0 {
		printBridgeUsage()
		return fmt.Errorf("bridge requires a subcommand")
	}
	switch args[0] {
	case "spawn":
		return runSpawn(args[1:])
	case "-h", "--help", "help":
		printBridgeUsage()
		return nil
	default:
		return fmt.Errorf("unknown bridge subcommand %q (available: spawn)", args[0])
	}
}

func printBridgeUsage() {
	fmt.Fprintln(os.Stderr, "Usage: agenthubctl bridge <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  spawn   Spawn a bridge worker")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Run 'agenthubctl bridge spawn --help' for spawn flags.")
}

// runSpawn implements `agenthubctl bridge spawn`.
func runSpawn(args []string) error {
	fs := flag.NewFlagSet("bridge spawn", flag.ContinueOnError)
	user := fs.String("user", "", "handle to spawn (required)")
	workdirFlag := fs.String("workdir", "", "workdir path (auto-detected from $AGENT_HUB_ROLES/<handle>/ if omitted)")
	bin := fs.String("bin", "bridge-claude2", "bridge binary name (resolved via $AGENT_HUB_BIN/<bin> or PATH)")
	templateHandle := fs.String("template", "_template", "template handle for CLAUDE.md (default: _template; use '' to skip)")
	tenant := fs.String("tenant", "", "tenant name (defaults to $AGENT_HUB_TENANT)")
	bridgesFile := fs.String("bridges-file", "", "bridges.json path (default: ~/.agent-hub/bridges.json)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agenthubctl bridge spawn --user <handle> [flags]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Environment:")
		fmt.Fprintln(os.Stderr, "  AGENT_HUB_ROLES   root of role workdirs (used for --workdir auto-detect and --template)")
		fmt.Fprintln(os.Stderr, "  AGENT_HUB_BIN     directory containing bridge binaries")
		fmt.Fprintln(os.Stderr, "  AGENT_HUB_TENANT  default tenant (used when --tenant is omitted)")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *user == "" {
		fs.Usage()
		return fmt.Errorf("--user is required")
	}

	handle := strings.TrimPrefix(*user, "@")
	if !handleRe.MatchString(handle) {
		return fmt.Errorf("invalid handle %q: must match [A-Za-z0-9_][A-Za-z0-9_-]*", handle)
	}

	tenantStr := *tenant
	if tenantStr == "" {
		tenantStr = os.Getenv("AGENT_HUB_TENANT")
	}

	registryPath := *bridgesFile
	if registryPath == "" {
		var err error
		registryPath, err = bridges.DefaultPath()
		if err != nil {
			return fmt.Errorf("resolve bridges.json path: %w", err)
		}
	}

	workdir, err := resolveWorkdir(handle, *workdirFlag)
	if err != nil {
		return err
	}

	// Copy CLAUDE.md template if the destination does not already exist.
	if *templateHandle != "" {
		if err := maybeApplyTemplate(workdir, *templateHandle); err != nil {
			return err
		}
	}

	binPath, err := resolveBin(*bin)
	if err != nil {
		return err
	}

	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("bridge-%s.log", handle))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	defer logFile.Close()

	cmdArgs := []string{"--user", handle, "--workdir", workdir}
	if tenantStr != "" {
		cmdArgs = append(cmdArgs, "--tenant", tenantStr)
	}

	cmd := exec.Command(binPath, cmdArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", *bin, err)
	}

	reg, err := bridges.Load(registryPath)
	if err != nil {
		return fmt.Errorf("load bridges.json: %w", err)
	}
	reg[handle] = &bridges.Entry{
		Handle:    handle,
		Bin:       *bin,
		Workdir:   workdir,
		Tenant:    tenantStr,
		PID:       cmd.Process.Pid,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := bridges.Save(registryPath, reg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update bridges.json: %v\n", err)
	}

	fmt.Printf("spawned @%s (pid=%d)\n", handle, cmd.Process.Pid)
	fmt.Printf("  bin:     %s\n", *bin)
	fmt.Printf("  workdir: %s\n", workdir)
	fmt.Printf("  log:     %s\n", logPath)
	if tenantStr != "" {
		fmt.Printf("  tenant:  %s\n", tenantStr)
	}
	fmt.Printf("  bridges: %s\n", registryPath)
	return nil
}

// resolveWorkdir resolves the workdir for a handle.
//   - explicit non-empty: use it directly (create if missing, no prompt)
//   - explicit empty: check $AGENT_HUB_ROLES/<handle>/, prompt to create if absent
func resolveWorkdir(handle, explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve workdir %q: %w", explicit, err)
		}
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return "", fmt.Errorf("create workdir %q: %w", abs, err)
		}
		return abs, nil
	}

	rolesDir := os.Getenv("AGENT_HUB_ROLES")
	if rolesDir == "" {
		return "", fmt.Errorf("--workdir not specified and AGENT_HUB_ROLES is not set")
	}

	candidate := filepath.Join(rolesDir, handle)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}

	fmt.Fprintf(os.Stderr, "workdir not found: %s/\nCreate it? [y/N] ", candidate)
	scanner := bufio.NewScanner(os.Stdin)
	var answer string
	if scanner.Scan() {
		answer = strings.TrimSpace(scanner.Text())
	}
	if strings.ToLower(answer) != "y" {
		return "", fmt.Errorf("aborted")
	}
	if err := os.MkdirAll(candidate, 0o750); err != nil {
		return "", fmt.Errorf("create workdir %q: %w", candidate, err)
	}
	return candidate, nil
}

// maybeApplyTemplate copies $AGENT_HUB_ROLES/<templateHandle>/CLAUDE.md to
// workdir/CLAUDE.md only when the destination does not already exist.
func maybeApplyTemplate(workdir, templateHandle string) error {
	if !handleRe.MatchString(templateHandle) && templateHandle != "_template" {
		return fmt.Errorf("invalid template handle %q: must match [A-Za-z0-9_][A-Za-z0-9_-]*", templateHandle)
	}

	dst := filepath.Join(workdir, "CLAUDE.md")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}

	rolesDir := os.Getenv("AGENT_HUB_ROLES")
	if rolesDir == "" {
		fmt.Fprintln(os.Stderr, "warning: AGENT_HUB_ROLES not set — skipping CLAUDE.md template copy")
		return nil
	}

	// Security: prevent path traversal via templateHandle.
	absRoles, err := filepath.Abs(rolesDir)
	if err != nil {
		return fmt.Errorf("resolve AGENT_HUB_ROLES: %w", err)
	}
	src := filepath.Join(absRoles, templateHandle, "CLAUDE.md")
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolve template path: %w", err)
	}
	if !strings.HasPrefix(absSrc, absRoles+string(filepath.Separator)) {
		return fmt.Errorf("template path escapes AGENT_HUB_ROLES: %q", absSrc)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "warning: CLAUDE.md not found, starting with empty workdir")
			return nil
		}
		return fmt.Errorf("read template CLAUDE.md %q: %w", src, err)
	}

	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	fmt.Printf("  template: %s → %s\n", src, dst)
	return nil
}

// resolveBin returns the full path of a bridge binary.
// Search order: $AGENT_HUB_BIN/<bin>, then PATH.
func resolveBin(bin string) (string, error) {
	if dir := os.Getenv("AGENT_HUB_BIN"); dir != "" {
		candidate := filepath.Join(dir, bin)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(bin); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%q not found: set AGENT_HUB_BIN to the directory containing bridge binaries, or add %q to PATH", bin, bin)
}
