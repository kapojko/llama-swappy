// Package backup mirrors the current configuration (the config file and
// every referenced launch script) into a per-machine directory, so a
// user can keep, for example, a git repository of configs from many
// machines and inspect another machine's launch setup from anywhere.
//
// The feature is fully opt-in (see config.BackupConfig). When enabled,
// Sync at startup reconciles <dir>/<machineName>/ against the current
// files: new files are copied in, changed files are overwritten, files
// that are no longer referenced are deleted, and a dated entry is
// appended to CHANGELOG.txt describing the change.
package backup

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/inspect"
)

// changelogName is the file that records every sync update. It is never
// treated as a config file and is never deleted as "outdated".
const changelogName = "CHANGELOG.txt"

// Source is one file to keep in the machine backup directory.
type Source struct {
	// Src is the absolute path of the current file on this machine.
	Src string
	// Name is the file name it is stored under in the machine directory
	// (typically the file's base name).
	Name string
}

// Options configure a single Sync call.
type Options struct {
	// Dir is the absolute backup root.
	Dir string
	// MachineName is the per-machine sub-directory (already sanitized
	// and defaulted by the caller).
	MachineName string
	// Sources are the current files to reconcile into the directory.
	Sources []Source
}

// DefaultMachineName derives a machine directory name from the host
// name and the OS, e.g. "workpc_linux". It works on both Windows and
// Linux (os.Hostname and runtime.GOOS are portable).
func DefaultMachineName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return sanitizeName(host + "_" + runtime.GOOS)
}

// SourcesForConfig derives the files that make up the current
// configuration: the config file itself plus every launch script
// referenced by a model's cmd (the wrapper-script tokens, resolved
// relative to the config file's directory). It returns the sources in a
// stable order (config file first, then scripts in model order) and a
// list of human-readable warnings for anything skipped (e.g. two scripts
// sharing a base name).
func SourcesForConfig(cfg *config.Config, configPath string) ([]Source, []string) {
	base, err := filepath.Abs(configPath)
	if err != nil {
		return nil, []string{fmt.Sprintf("resolve config path: %v", err)}
	}
	baseDir := filepath.Dir(base)

	var out []Source
	var warns []string
	seenPath := map[string]bool{}
	seenName := map[string]string{}

	add := func(src, name string) {
		if seenPath[src] {
			return
		}
		if prev, ok := seenName[name]; ok && prev != src {
			// Keep the first file for a given name; a second file
			// with the same base name would clobber it.
			warns = append(warns, fmt.Sprintf("skipping %q: name %q already used by %s", src, name, prev))
			return
		}
		seenPath[src] = true
		seenName[name] = src
		out = append(out, Source{Src: src, Name: name})
	}

	add(base, filepath.Base(base))
	for _, m := range cfg.Models {
		for _, tok := range inspect.ScriptTokens(m.Cmd, cfg.StartPort) {
			p := tok
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			p = filepath.Clean(p)
			add(p, filepath.Base(p))
		}
	}
	return out, warns
}

// Sync reconciles the machine backup directory against Sources. It
// returns an error only for I/O problems (the caller is expected to log
// and continue — a backup failure never prevents the app from running).
// When nothing changed it is a no-op and returns nil.
func Sync(opts Options, log *slog.Logger) error {
	machineDir := filepath.Join(opts.Dir, sanitizeName(opts.MachineName))
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	existing, err := os.ReadDir(machineDir)
	if err != nil {
		return fmt.Errorf("read backup dir: %w", err)
	}
	existingFiles := map[string]bool{}
	for _, e := range existing {
		if e.IsDir() || e.Name() == changelogName {
			continue
		}
		existingFiles[e.Name()] = true
	}

	var changes []string
	expected := map[string]bool{}
	for _, s := range opts.Sources {
		dest := filepath.Join(machineDir, s.Name)
		expected[s.Name] = true
		if !existingFiles[s.Name] {
			if err := copyFile(s.Src, dest); err != nil {
				return err
			}
			changes = append(changes, "added "+s.Name)
		} else if !sameFile(s.Src, dest) {
			if err := copyFile(s.Src, dest); err != nil {
				return err
			}
			changes = append(changes, "updated "+s.Name)
		}
	}
	for name := range existingFiles {
		if !expected[name] {
			if err := os.Remove(filepath.Join(machineDir, name)); err != nil {
				return fmt.Errorf("remove outdated %q: %w", name, err)
			}
			changes = append(changes, "removed "+name)
		}
	}

	if len(changes) == 0 {
		return nil
	}
	if err := appendChangelog(machineDir, changes); err != nil {
		return err
	}
	log.Info("config backup updated", "dir", machineDir, "changes", len(changes))
	return nil
}

// copyFile writes src to dest, creating or truncating dest.
func copyFile(src, dest string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// sameFile reports whether two files have identical content.
func sameFile(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(da, db)
}

// appendChangelog appends a dated, multi-line entry describing the
// changes made by this sync to CHANGELOG.txt in the machine directory.
func appendChangelog(machineDir string, changes []string) error {
	path := filepath.Join(machineDir, changelogName)
	_, statErr := os.Stat(path)
	exists := statErr == nil

	var b strings.Builder
	b.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	for _, c := range changes {
		b.WriteString("\n  " + c)
	}
	b.WriteString("\n")
	if exists {
		b.WriteString("\n")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open changelog: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return fmt.Errorf("write changelog: %w", err)
	}
	return nil
}

// sanitizeName strips characters that are unsafe in a directory name on
// either Windows or Linux so both a derived host name and a user-supplied
// machine name can be used as a directory.
func sanitizeName(s string) string {
	replacer := strings.NewReplacer(
		"\\", "_", "/", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	s = replacer.Replace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		s = "unnamed"
	}
	return s
}
