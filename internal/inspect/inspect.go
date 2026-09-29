// Package inspect derives static metadata about a model (context size,
// max output tokens, reasoning support) from its run script or command
// line. The metadata is logged at startup and reported by the
// /llama-swappy/info endpoint so clients (e.g. the pi agent) can
// configure the model automatically.
package inspect

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// defaultMaxTokens caps the derived per-response output token limit;
// reasoning models need a generous budget, but it must stay far below
// the context size.
const defaultMaxTokens = 32768

// Info holds metadata derived from a model's run script or command
// line. The zero value means "unknown" for every field.
type Info struct {
	// ContextSize is the llama-server context size (-c / --ctx-size).
	// Zero means the script does not set it (llama-server then
	// defaults to 4096).
	ContextSize int
	// MaxTokens is an explicit --max-tokens value from the script.
	// Zero means the script does not set it.
	MaxTokens int
	// Reasoning is set when the script configures --reasoning; nil
	// means the flag is absent.
	Reasoning *bool
}

// MaxTokens resolves the per-response output token limit for a model:
// an explicit (config) value wins, then the script's --max-tokens, then
// min(defaultMaxTokens, contextSize/2). It returns 0 when no value can
// be derived (unknown context size and no explicit setting).
func MaxTokens(explicit int, info Info) int {
	if explicit > 0 {
		return explicit
	}
	if info.MaxTokens > 0 {
		return info.MaxTokens
	}
	if info.ContextSize > 0 {
		return min(defaultMaxTokens, info.ContextSize/2)
	}
	return 0
}

// scriptExtensions are file extensions recognized as wrapper scripts.
var scriptExtensions = []string{".sh", ".bash", ".ps1", ".bat", ".cmd"}

// isScript reports whether a command token looks like a wrapper script
// path (Unix or Windows).
func isScript(arg string) bool {
	lower := strings.ToLower(arg)
	for _, ext := range scriptExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Inspect derives model metadata from the model's configured command
// line. If a token in cmd is a wrapper script (.sh, .bash, .ps1, .bat
// or .cmd) the script file is read and parsed; otherwise the command
// line itself is parsed as llama-server arguments. ${PORT} is
// substituted with port before the script path is resolved.
//
// A missing or binary script file is an error; flags that are simply
// absent leave the corresponding Info field at its zero value.
func Inspect(cmd string, port int) (Info, error) {
	line := strings.ReplaceAll(cmd, "${PORT}", strconv.Itoa(port))
	args, err := splitLine(line)
	if err != nil {
		return Info{}, fmt.Errorf("split cmd: %w", err)
	}
	for _, a := range args {
		if isScript(a) {
			return parseFile(a)
		}
	}
	return parseArgs(args), nil
}

// parseFile reads a wrapper script and extracts metadata from its
// llama-server flags.
func parseFile(path string) (Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, fmt.Errorf("read script %s: %w", path, err)
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		return Info{}, fmt.Errorf("script %s is not a text file", path)
	}
	return parseLines(string(data)), nil
}

// parseArgs extracts metadata from a raw command line (no wrapper
// script).
func parseArgs(args []string) Info {
	var info Info
	applyFlags(args, &info)
	return info
}

// parseLines extracts metadata from a script's text. Lines starting
// with '#' (after leading whitespace) are comments and ignored, as are
// trailing comments after the code of a line; both Unix and Windows
// line endings are handled. When a flag appears several times (e.g.
// commented-out older values above the active one) the last
// uncommented occurrence wins.
func parseLines(text string) Info {
	var info Info
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimLeft(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		applyFlags(tokensBeforeComment(line), &info)
	}
	return info
}

// tokensBeforeComment splits a script line into tokens, stopping at an
// unquoted '#'. Double and single quotes are honored and stripped. Unquoted
// ',' and ';' are treated as separators so PowerShell array syntax
// (e.g. @('-c', '131072')) parses the same as bash.
func tokensBeforeComment(line string) []string {
	var tokens []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inArg = true
		case r == '#' && !inArg:
			return tokens
		case r == ' ' || r == '\t' || r == ',' || r == ';':
			if inArg {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// splitLine splits a command line on whitespace, treating double-quoted
// segments as a single argument (quotes stripped).
func splitLine(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inQuote := false
	inArg := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			inArg = true
		case (r == ' ' || r == '\t') && !inQuote:
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unbalanced quote in cmd")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// applyFlags scans tokens for the llama-server flags of interest and
// records the last occurrence of each in info.
func applyFlags(tokens []string, info *Info) {
	for i := 0; i < len(tokens); i++ {
		name, val, hasVal := splitFlag(tokens[i])
		if !hasVal && i+1 < len(tokens) {
			if v, ok := flagValue(tokens[i+1]); ok {
				val, hasVal = v, true
				i++
			}
		}
		switch name {
		case "-c", "--ctx-size":
			if hasVal {
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					info.ContextSize = n
				}
			}
		case "--max-tokens":
			if hasVal {
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					info.MaxTokens = n
				}
			}
		case "--reasoning":
			switch {
			case hasVal:
				if b, ok := parseBool(val); ok {
					info.Reasoning = &b
				}
			default:
				// Bare flag: llama-server treats --reasoning
				// without a value as on.
				b := true
				info.Reasoning = &b
			}
		}
	}
}

// splitFlag separates a --flag=value token into name and value.
func splitFlag(tok string) (name, val string, hasVal bool) {
	if i := strings.IndexByte(tok, '='); i > 0 {
		return tok[:i], tok[i+1:], true
	}
	return tok, "", false
}

// flagValue reports whether tok is a value for the preceding flag
// rather than the next flag.
func flagValue(tok string) (string, bool) {
	if strings.HasPrefix(tok, "-") {
		return "", false
	}
	return tok, true
}

// parseBool understands the on/off spellings llama-server accepts.
func parseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	default:
		return false, false
	}
}
