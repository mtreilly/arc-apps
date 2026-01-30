// Copyright (c) 2025 Arc Engineering
// SPDX-License-Identifier: MIT

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	arcer "github.com/yourorg/arc-sdk/errors"
	"github.com/yourorg/arc-sdk/output"
	"github.com/yourorg/arc-sdk/utils"
)

// NewRootCmd creates the root command for arc-apps.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arc-apps",
		Short: "Export installed macOS apps and Homebrew inventory",
		Long:  "Generate a text report plus Homebrew JSON metadata for installed applications, CLI tools, and their metadata.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(exportCmd())
	return cmd
}

type exportStats struct {
	AppBundleCount        int `json:"app_bundle_count" yaml:"app_bundle_count"`
	ApplicationsDirCount  int `json:"applications_dir_count" yaml:"applications_dir_count"`
	UserApplicationsCount int `json:"user_applications_count" yaml:"user_applications_count"`
	BrewCaskCount         int `json:"brew_cask_count" yaml:"brew_cask_count"`
	BrewFormulaCount      int `json:"brew_formula_count" yaml:"brew_formula_count"`
}

type exportResult struct {
	ReportPath        string      `json:"report_path" yaml:"report_path"`
	ReportSizeBytes   int64       `json:"report_size_bytes" yaml:"report_size_bytes"`
	BrewJSONPath      string      `json:"brew_json_path" yaml:"brew_json_path"`
	BrewJSONSizeBytes int64       `json:"brew_json_size_bytes" yaml:"brew_json_size_bytes"`
	Compact           bool        `json:"compact" yaml:"compact"`
	Stats             exportStats `json:"stats" yaml:"stats"`
	DurationSeconds   float64     `json:"duration_seconds" yaml:"duration_seconds"`
	StartedAt         time.Time   `json:"started_at" yaml:"started_at"`
	CompletedAt       time.Time   `json:"completed_at" yaml:"completed_at"`
	Warnings          []string    `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

type exportOptions struct {
	reportPath string
	jsonPath   string
	compact    bool
}

func exportCmd() *cobra.Command {
	defaultReport := fmt.Sprintf("mac_installed_software_%s.txt", time.Now().Format("2006-01-02_15-04-05"))
	defaultJSON := "brew_installed.json"

	var (
		opts       output.OutputOptions
		reportPath = defaultReport
		jsonPath   = defaultJSON
		compact    bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export installed apps, Homebrew casks, formulae, and metadata",
		Long: strings.TrimSpace(`
Export a full inventory of installed macOS apps, Homebrew casks (GUI), formulae (CLI),
and Homebrew metadata. Outputs a text report plus a JSON file from 'brew info --installed --json=v2'.
`),
		Example: strings.TrimSpace(`
Example:
  # Default export with timestamped text report
  arc-apps export

Example:
  # Write reports to a custom directory
  arc-apps export --output-file ~/Desktop/mac_apps.txt --brew-json-file ~/Desktop/brew.json

Example:
  # Emit a JSON summary for scripting while still writing files
  arc-apps export --output json

Example:
  # Keep quiet output for cronjobs
  arc-apps export --output quiet

Example:
  # Compact run (skip brew doctor/config and brew JSON)
  arc-apps export --compact --output-file ~/Desktop/apps_compact.txt
`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return &arcer.CLIError{
					Msg:  "arc-apps export currently supports macOS only",
					Hint: "This command wraps Spotlight (mdfind) and Homebrew. Run from macOS where these tools exist.",
				}
			}

			if err := opts.Resolve(); err != nil {
				return err
			}

			expOpts := exportOptions{
				reportPath: utils.ExpandPath(reportPath),
				jsonPath:   utils.ExpandPath(jsonPath),
				compact:    compact,
			}

			result, err := runExport(cmd.Context(), expOpts)
			if err != nil {
				return err
			}

			switch {
			case opts.Is(output.OutputJSON):
				enc := jsonEncoder(cmd.OutOrStdout())
				return enc.Encode(result)
			case opts.Is(output.OutputYAML):
				enc := yamlEncoder(cmd.OutOrStdout())
				return enc.Encode(result)
			case opts.Is(output.OutputQuiet):
				fmt.Fprintln(cmd.OutOrStdout(), result.ReportPath)
				fmt.Fprintln(cmd.OutOrStdout(), result.BrewJSONPath)
				return nil
			default:
				printSummary(cmd.OutOrStdout(), result)
				return nil
			}
		},
	}

	cmd.Flags().StringVarP(&reportPath, "output-file", "f", reportPath, "Path for the text report (default includes timestamp)")
	cmd.Flags().StringVar(&jsonPath, "brew-json-file", jsonPath, "Path for the Homebrew JSON metadata output")
	cmd.Flags().BoolVar(&compact, "compact", false, "Skip brew doctor/config output and brew JSON (faster, smaller)")
	opts.AddOutputFlags(cmd, output.OutputTable)
	return cmd
}

func runExport(ctx context.Context, opts exportOptions) (exportResult, error) {
	var result exportResult

	if err := ensureCommand("mdfind", "Spotlight CLI missing. Ensure you're on macOS with Spotlight enabled."); err != nil {
		return result, err
	}
	if err := ensureCommand("brew", "Install Homebrew from https://brew.sh/ to capture casks and formulae."); err != nil {
		return result, err
	}

	absReport, err := filepath.Abs(opts.reportPath)
	if err != nil {
		return result, err
	}
	absJSON, err := filepath.Abs(opts.jsonPath)
	if err != nil {
		return result, err
	}

	if err := os.MkdirAll(filepath.Dir(absReport), 0o755); err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(absJSON), 0o755); err != nil {
		return result, err
	}

	reportFile, err := os.Create(absReport)
	if err != nil {
		return result, err
	}
	defer reportFile.Close()

	writer := bufio.NewWriter(reportFile)
	defer writer.Flush()

	result.ReportPath = absReport
	if !opts.compact {
		result.BrewJSONPath = absJSON
	}
	result.StartedAt = time.Now()

	stats := exportStats{}

	if err := writeSectionHeader(writer, "MAC SYSTEM + USER INSTALLED APPLICATIONS (.app bundles)"); err != nil {
		return result, err
	}

	appBundles, err := commandLines(ctx, "mdfind", "kMDItemContentType == 'com.apple.application-bundle'")
	if err != nil {
		return result, wrapCommandErr("mdfind", err, "")
	}
	sort.Strings(appBundles)
	stats.AppBundleCount = len(appBundles)
	if err := writeLines(writer, appBundles); err != nil {
		return result, err
	}

	if _, err := fmt.Fprintln(writer); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintln(writer, "-- /Applications ---"); err != nil {
		return result, err
	}
	systemApps, err := listDirSorted("/Applications")
	if err != nil {
		return result, wrapCommandErr("ls /Applications", err, "")
	}
	stats.ApplicationsDirCount = len(systemApps)
	if err := writeLines(writer, systemApps); err != nil {
		return result, err
	}

	if _, err := fmt.Fprintln(writer); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintln(writer, "-- ~/Applications ---"); err != nil {
		return result, err
	}
	homeDir, _ := os.UserHomeDir()
	userApps, err := listDirSorted(filepath.Join(homeDir, "Applications"))
	if err == nil {
		stats.UserApplicationsCount = len(userApps)
		if err := writeLines(writer, userApps); err != nil {
			return result, err
		}
	}

	if err := writeSectionHeader(writer, "HOMEBREW CASK APPLICATIONS (GUI)"); err != nil {
		return result, err
	}
	casks, err := commandLines(ctx, "brew", "list", "--cask", "--versions")
	if err != nil {
		return result, wrapCommandErr("brew list --cask --versions", err, "Confirm Homebrew is installed and casks are set up.")
	}
	sort.Strings(casks)
	stats.BrewCaskCount = len(casks)
	if err := writeLines(writer, casks); err != nil {
		return result, err
	}

	if !opts.compact {
		if _, err := fmt.Fprintln(writer); err != nil {
			return result, err
		}
		if _, err := fmt.Fprintln(writer, "-- Installed paths --"); err != nil {
			return result, err
		}
		caskroomDirs, err := caskroomDirectories(ctx)
		if err != nil {
			return result, err
		}
		if err := writeLines(writer, caskroomDirs); err != nil {
			return result, err
		}
	}

	if err := writeSectionHeader(writer, "HOMEBREW FORMULAE (CLI tools)"); err != nil {
		return result, err
	}
	formulae, err := commandLines(ctx, "brew", "list", "--formula", "--versions")
	if err != nil {
		return result, wrapCommandErr("brew list --formula --versions", err, "Confirm Homebrew is installed and formulae are set up.")
	}
	sort.Strings(formulae)
	stats.BrewFormulaCount = len(formulae)
	if err := writeLines(writer, formulae); err != nil {
		return result, err
	}

	if !opts.compact {
		if err := writeSectionHeader(writer, "BREW ENV & METADATA"); err != nil {
			return result, err
		}
		if warn, err := appendCommandOutput(ctx, writer, false, "brew", "config"); err != nil {
			return result, err
		} else if warn != "" {
			result.Warnings = append(result.Warnings, warn)
		}
		if warn, err := appendCommandOutput(ctx, writer, true, "brew", "doctor"); err != nil {
			return result, err
		} else if warn != "" {
			result.Warnings = append(result.Warnings, warn)
		}

		if err := writeSectionHeader(writer, "FULL BREW PACKAGE METADATA (JSON)"); err != nil {
			return result, err
		}
		if err := writeBrewJSON(ctx, absJSON); err != nil {
			return result, err
		}
		if _, err := fmt.Fprintf(writer, "Saved JSON -> %s\n", absJSON); err != nil {
			return result, err
		}
	}

	if _, err := fmt.Fprintln(writer); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintln(writer, "==============================="); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintln(writer, "Report complete!"); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintf(writer, "Text report: %s\n", absReport); err != nil {
		return result, err
	}
	if !opts.compact {
		if _, err := fmt.Fprintf(writer, "JSON metadata: %s\n", absJSON); err != nil {
			return result, err
		}
	} else {
		if _, err := fmt.Fprintln(writer, "JSON metadata: skipped (compact mode)"); err != nil {
			return result, err
		}
	}
	if _, err := fmt.Fprintln(writer, "==============================="); err != nil {
		return result, err
	}

	if err := writer.Flush(); err != nil {
		return result, err
	}

	result.ReportSizeBytes = fileSize(absReport)
	if !opts.compact {
		result.BrewJSONSizeBytes = fileSize(absJSON)
	}
	result.Stats = stats
	result.CompletedAt = time.Now()
	result.DurationSeconds = result.CompletedAt.Sub(result.StartedAt).Seconds()
	result.Compact = opts.compact

	return result, nil
}

func writeSectionHeader(w io.Writer, title string) error {
	parts := []string{"", "===============================", title, "==============================="}
	for _, line := range parts {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func writeLines(w io.Writer, lines []string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func commandLines(ctx context.Context, name string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmdOutput, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(cmdOutput)))
	}
	raw := strings.Split(strings.TrimSpace(string(cmdOutput)), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines, nil
}

func listDirSorted(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func appendCommandOutput(ctx context.Context, w io.Writer, allowWarn bool, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	multi := io.MultiWriter(w, &buf)
	cmd.Stdout = multi
	cmd.Stderr = multi
	if err := cmd.Run(); err != nil {
		cmdStr := strings.Join(append([]string{name}, args...), " ")
		msg := strings.TrimSpace(buf.String())
		warn := fmt.Sprintf("%s failed: %v", cmdStr, err)
		if msg != "" {
			warn = fmt.Sprintf("%s: %s", warn, msg)
		}
		if allowWarn {
			return warn, nil
		}
		return "", wrapCommandErr(cmdStr, err, msg)
	}
	return "", nil
}

func writeBrewJSON(ctx context.Context, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "brew", "info", "--installed", "--json=v2")
	cmd.Stdout = file
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return wrapCommandErr("brew info --installed --json=v2", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func caskroomDirectories(ctx context.Context) ([]string, error) {
	prefixLines, err := commandLines(ctx, "brew", "--prefix")
	if err != nil || len(prefixLines) == 0 {
		return nil, wrapCommandErr("brew --prefix", err, "")
	}
	caskroom := filepath.Join(prefixLines[0], "Caskroom")
	if _, err := os.Stat(caskroom); err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var dirs []string
	err = filepath.WalkDir(caskroom, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		depth := strings.Count(strings.TrimPrefix(path, caskroom), string(os.PathSeparator))
		if depth > 2 {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

func ensureCommand(name, hint string) error {
	if _, err := exec.LookPath(name); err != nil {
		return &arcer.CLIError{
			Msg:  fmt.Sprintf("%s is required but not found in PATH", name),
			Hint: hint,
			Suggestions: []string{
				fmt.Sprintf("which %s", name),
				"echo $PATH",
			},
		}
	}
	return nil
}

func wrapCommandErr(cmdName string, err error, hint string) error {
	if err == nil {
		return nil
	}
	h := hint
	if h == "" {
		h = fmt.Sprintf("Re-run with verbose logging or manually execute `%s` for details.", cmdName)
	}
	return &arcer.CLIError{
		Msg:  fmt.Sprintf("%s failed: %v", cmdName, err),
		Hint: h,
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func printSummary(w io.Writer, result exportResult) {
	fmt.Fprintf(w, "Apps export completed in %s\n", time.Duration(result.DurationSeconds*float64(time.Second)))
	fmt.Fprintf(w, "Text report: %s (%s)\n", result.ReportPath, humanize.Bytes(uint64(result.ReportSizeBytes)))
	if result.BrewJSONPath != "" {
		fmt.Fprintf(w, "Brew JSON:  %s (%s)\n", result.BrewJSONPath, humanize.Bytes(uint64(result.BrewJSONSizeBytes)))
	} else {
		fmt.Fprintln(w, "Brew JSON:  skipped (compact mode)")
	}

	fmt.Fprintln(w, "\nCounts")
	fmt.Fprintln(w, strings.Repeat("-", 40))
	fmt.Fprintf(w, "  App bundles (mdfind): %d\n", result.Stats.AppBundleCount)
	fmt.Fprintf(w, "  /Applications:        %d\n", result.Stats.ApplicationsDirCount)
	fmt.Fprintf(w, "  ~/Applications:       %d\n", result.Stats.UserApplicationsCount)
	fmt.Fprintf(w, "  Brew casks:           %d\n", result.Stats.BrewCaskCount)
	fmt.Fprintf(w, "  Brew formulae:        %d\n", result.Stats.BrewFormulaCount)

	if len(result.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings")
		fmt.Fprintln(w, strings.Repeat("-", 40))
		for _, warn := range result.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}
}

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

func yamlEncoder(w io.Writer) *yaml.Encoder {
	return yaml.NewEncoder(w)
}
