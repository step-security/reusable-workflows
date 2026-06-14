package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

type Severity string

const (
	SevMust   Severity = "must"
	SevShould Severity = "should"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

type Finding struct {
	RuleID   string   `json:"rule_id"`
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	Status   Status   `json:"status"`
	File     string   `json:"file,omitempty"`
	Message  string   `json:"message,omitempty"`
}

type Summary struct {
	Total        int `json:"total"`
	Passed       int `json:"passed"`
	FailedMust   int `json:"failed_must"`
	FailedShould int `json:"failed_should"`
	Skipped      int `json:"skipped"`
}

type Report struct {
	ActionType string    `json:"action_type"`
	Upstream   string    `json:"upstream"`
	Summary    Summary   `json:"summary"`
	Findings   []Finding `json:"findings"`
}

func main() {
	var repoPath, output, format string
	var failOnError bool
	flag.StringVar(&repoPath, "repo-path", ".", "Path to the action repository to review")
	flag.StringVar(&output, "output", "", "Write report to this file (default: stdout)")
	flag.StringVar(&format, "format", "json", "Output format: 'json' or 'table'")
	flag.StringVar(&format, "f", "json", "Output format: 'json' or 'table' (shorthand)")
	flag.BoolVar(&failOnError, "fail-on-error", false, "Exit 1 if any 'must' rule fails")
	flag.Parse()

	switch format {
	case "json", "table":
	default:
		fmt.Fprintf(os.Stderr, "invalid --format %q (must be 'json' or 'table')\n", format)
		os.Exit(2)
	}

	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve repo path: %v\n", err)
		os.Exit(2)
	}

	upstream := readUpstream(absRepo)
	actionType := detectActionType(absRepo)

	findings := runCommonChecks(absRepo, upstream)
	findings = append(findings, runTypeChecks(absRepo, actionType)...)
	sortFindings(findings)

	report := Report{
		ActionType: actionType,
		Upstream:   upstream,
		Findings:   findings,
		Summary:    summarize(findings),
	}

	var dst io.Writer = os.Stdout
	if output != "" {
		f, err := os.Create(output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create output file: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		dst = f
	}

	switch format {
	case "json":
		if err := writeJSON(dst, report); err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode JSON: %v\n", err)
			os.Exit(2)
		}
	case "table":
		if err := writeTable(dst, report, output == ""); err != nil {
			fmt.Fprintf(os.Stderr, "failed to render table: %v\n", err)
			os.Exit(2)
		}
	}

	// Summary always goes to stderr so it doesn't pollute stdout when piping.
	fmt.Fprintf(os.Stderr, "Action type: %s\n", actionType)
	fmt.Fprintf(os.Stderr, "Upstream:    %s\n", upstream)
	fmt.Fprintf(os.Stderr, "Total: %d  Passed: %d  Failed (must): %d  Failed (should): %d  Skipped: %d\n",
		report.Summary.Total, report.Summary.Passed, report.Summary.FailedMust,
		report.Summary.FailedShould, report.Summary.Skipped)

	if failOnError && report.Summary.FailedMust > 0 {
		os.Exit(1)
	}
}

func writeJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func writeTable(w io.Writer, report Report, colorize bool) error {
	if !colorize {
		text.DisableColors()
	} else {
		text.EnableColors()
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Action type: %s\n", report.ActionType)
	fmt.Fprintf(w, "  Upstream:    %s\n", report.Upstream)
	s := report.Summary
	fmt.Fprintf(w, "  Total: %d  Passed: %d  Failed (must): %d  Failed (should): %d  Skipped: %d\n\n",
		s.Total, s.Passed, s.FailedMust, s.FailedShould, s.Skipped)

	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	tw.SetStyle(table.StyleLight)
	tw.Style().Format.Header = text.FormatDefault
	tw.AppendHeader(table.Row{"Status", "Severity", "Rule", "File", "Message"})

	for _, f := range report.Findings {
		tw.AppendRow(table.Row{
			colorStatus(f.Status),
			colorSeverity(f.Severity),
			f.RuleID,
			f.File,
			oneLine(f.Message),
		})
	}

	tw.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft, WidthMax: 42, WidthMaxEnforcer: text.WrapText},
		{Number: 4, Align: text.AlignLeft, WidthMax: 30, WidthMaxEnforcer: text.WrapText},
		{Number: 5, Align: text.AlignLeft, WidthMax: 60, WidthMaxEnforcer: text.WrapText},
	})

	tw.Render()
	return nil
}

func colorStatus(s Status) string {
	switch s {
	case StatusPass:
		return text.FgGreen.Sprint("PASS")
	case StatusFail:
		return text.FgHiRed.Sprint("FAIL")
	case StatusSkip:
		return text.FgYellow.Sprint("SKIP")
	}
	return strings.ToUpper(string(s))
}

func colorSeverity(s Severity) string {
	switch s {
	case SevMust:
		return text.FgRed.Sprint("must")
	case SevShould:
		return text.FgYellow.Sprint("should")
	}
	return string(s)
}

// oneLine collapses newlines so a message stays on a single row before wrap.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func summarize(findings []Finding) Summary {
	s := Summary{Total: len(findings)}
	for _, f := range findings {
		switch f.Status {
		case StatusPass:
			s.Passed++
		case StatusSkip:
			s.Skipped++
		case StatusFail:
			if f.Severity == SevMust {
				s.FailedMust++
			} else {
				s.FailedShould++
			}
		}
	}
	return s
}

// ---------- upstream + action type detection ----------

func readUpstream(repoPath string) string {
	for _, name := range []string{"auto_cherry_pick.yml", "auto_cherry_pick.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, ".github", "workflows", name))
		if err != nil {
			continue
		}
		owner := extractYAMLValue(string(data), "original-owner")
		repo := extractYAMLValue(string(data), "repo-name")
		if owner != "" && repo != "" {
			return owner + "/" + repo
		}
	}
	return ""
}

func detectActionType(repoPath string) string {
	var content string
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		content = string(data)
		break
	}
	if content == "" {
		return "unknown"
	}
	using := strings.ToLower(extractYAMLValue(content, "using"))
	switch {
	case strings.HasPrefix(using, "node"):
		return "node"
	case using == "docker":
		return "docker"
	case using == "composite":
		return "composite"
	}
	if regexp.MustCompile(`(?m)^\s*image:\s*`).MatchString(content) {
		return "docker"
	}
	return "unknown"
}

func extractYAMLValue(content, key string) string {
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^\s*%s:\s*["']?([^"'\n#]+?)["']?\s*(#.*)?$`, regexp.QuoteMeta(key)))
	m := re.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ---------- common rule runner ----------

func runCommonChecks(repoPath, upstream string) []Finding {
	var findings []Finding

	findings = append(findings, checkLicense(repoPath))
	findings = append(findings, checkActionYMLAuthor(repoPath))
	findings = append(findings, checkSecurityMD(repoPath))
	findings = append(findings, checkRequiredWorkflows(repoPath)...)
	findings = append(findings, checkAbsent(repoPath, forbiddenPaths())...)
	findings = append(findings, checkPreCommitTooling(repoPath)...)
	findings = append(findings, checkBanner(repoPath))
	findings = append(findings, checkSubscriptionURL(repoPath))
	findings = append(findings, checkSubscriptionUpstream(repoPath, upstream))
	findings = append(findings, checkDocsUpstreamRefs(repoPath, upstream))
	findings = append(findings, checkNoUnauthorizedSchedules(repoPath)...)

	return findings
}

// ---------- type-specific rule runner ----------

func runTypeChecks(repoPath, actionType string) []Finding {
	switch actionType {
	case "node":
		return runNodeChecks(repoPath)
	case "docker":
		return runDockerChecks(repoPath)
	case "composite":
		return runCompositeChecks(repoPath)
	}
	return nil
}

func sortFindings(findings []Finding) {
	sevOrder := map[Status]int{StatusFail: 0, StatusSkip: 1, StatusPass: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		if sevOrder[findings[i].Status] != sevOrder[findings[j].Status] {
			return sevOrder[findings[i].Status] < sevOrder[findings[j].Status]
		}
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity == SevMust
		}
		return findings[i].RuleID < findings[j].RuleID
	})
}

// ---------- individual checks ----------

func checkLicense(repoPath string) Finding {
	rule := "common.license_present"
	for _, c := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt"} {
		if _, err := os.Stat(filepath.Join(repoPath, c)); err == nil {
			return Finding{
				RuleID:   rule,
				Title:    "LICENSE file present",
				Severity: SevMust,
				Status:   StatusPass,
				File:     c,
				Message:  "License copyright semantics (original author + StepSecurity copyright) must be verified by reviewer.",
			}
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "LICENSE file present",
		Severity: SevMust,
		Status:   StatusFail,
		Message:  "No LICENSE file found at repo root.",
	}
}

func checkActionYMLAuthor(repoPath string) Finding {
	rule := "common.action_yml_author"
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		author := extractYAMLValue(string(data), "author")
		if author == "" {
			return Finding{
				RuleID:   rule,
				Title:    "action.yml author field is 'step-security' when present",
				Severity: SevMust,
				Status:   StatusSkip,
				File:     name,
				Message:  "author field not present in action.yml; rule does not apply.",
			}
		}
		if author == "step-security" {
			return Finding{
				RuleID:   rule,
				Title:    "action.yml author field is 'step-security'",
				Severity: SevMust,
				Status:   StatusPass,
				File:     name,
			}
		}
		return Finding{
			RuleID:   rule,
			Title:    "action.yml author field is 'step-security'",
			Severity: SevMust,
			Status:   StatusFail,
			File:     name,
			Message:  fmt.Sprintf("author=%q (expected: step-security)", author),
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "action.yml author field is 'step-security'",
		Severity: SevMust,
		Status:   StatusFail,
		Message:  "No action.yml or action.yaml found at repo root.",
	}
}

func checkSecurityMD(repoPath string) Finding {
	rule := "common.security_md"
	for _, c := range []string{"SECURITY.md", ".github/SECURITY.md"} {
		data, err := os.ReadFile(filepath.Join(repoPath, c))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "security@stepsecurity.io") {
			return Finding{
				RuleID:   rule,
				Title:    "SECURITY.md present with StepSecurity contact",
				Severity: SevMust,
				Status:   StatusPass,
				File:     c,
			}
		}
		return Finding{
			RuleID:   rule,
			Title:    "SECURITY.md present with StepSecurity contact",
			Severity: SevMust,
			Status:   StatusFail,
			File:     c,
			Message:  "SECURITY.md found but missing 'security@stepsecurity.io' contact line.",
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "SECURITY.md present with StepSecurity contact",
		Severity: SevMust,
		Status:   StatusFail,
		Message:  "No SECURITY.md found at repo root or .github/.",
	}
}

func checkRequiredWorkflows(repoPath string) []Finding {
	required := []struct {
		Names []string
		Rule  string
		Title string
	}{
		{Names: []string{"auto_cherry_pick.yml", "auto_cherry_pick.yaml"}, Rule: "common.workflow_auto_cherry_pick", Title: "Workflow .github/workflows/auto_cherry_pick.yml present"},
		{Names: []string{"actions_release.yml", "actions_release.yaml"}, Rule: "common.workflow_actions_release", Title: "Workflow .github/workflows/actions_release.yml present"},
	}
	var findings []Finding
	for _, req := range required {
		found := ""
		for _, n := range req.Names {
			if _, err := os.Stat(filepath.Join(repoPath, ".github", "workflows", n)); err == nil {
				found = n
				break
			}
		}
		if found != "" {
			findings = append(findings, Finding{
				RuleID:   req.Rule,
				Title:    req.Title,
				Severity: SevMust,
				Status:   StatusPass,
				File:     filepath.Join(".github/workflows", found),
			})
		} else {
			findings = append(findings, Finding{
				RuleID:   req.Rule,
				Title:    req.Title,
				Severity: SevMust,
				Status:   StatusFail,
				Message:  fmt.Sprintf("Missing required workflow %s in .github/workflows/", req.Names[0]),
			})
		}
	}
	return findings
}

// checkNoUnauthorizedSchedules enforces that no workflow under
// .github/workflows/ declares `on.schedule`, with audit_package.yml/.yaml as
// the sole allowed exception.
func checkNoUnauthorizedSchedules(repoPath string) []Finding {
	rule := "common.no_unauthorized_schedule_triggers"
	title := "No workflow (except audit_package.yml) declares on.schedule"
	wfDir := filepath.Join(repoPath, ".github", "workflows")

	entries, err := os.ReadDir(wfDir)
	if err != nil {
		return []Finding{{
			RuleID: rule, Title: title,
			Severity: SevMust, Status: StatusSkip,
			Message: ".github/workflows/ directory not found.",
		}}
	}

	allowed := map[string]bool{
		"audit_package.yml":  true,
		"audit_package.yaml": true,
	}

	var fails []Finding
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		if allowed[name] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(wfDir, name))
		if err != nil {
			continue
		}
		if hasOnSchedule(string(data)) {
			fails = append(fails, Finding{
				RuleID:   rule,
				Title:    title,
				Severity: SevMust,
				Status:   StatusFail,
				File:     filepath.Join(".github/workflows", name),
				Message:  "Workflow declares on.schedule; only audit_package.yml may be scheduled.",
			})
		}
	}
	if len(fails) > 0 {
		return fails
	}
	return []Finding{{
		RuleID: rule, Title: title,
		Severity: SevMust, Status: StatusPass,
	}}
}

// hasOnSchedule reports whether the workflow YAML declares `schedule` as a
// trigger under the top-level `on:` key. Handles three forms:
//
//	on: schedule                 -> scalar
//	on: [push, schedule]         -> flow list
//	on:
//	  schedule:                  -> block-style child
//	    - cron: ...
func hasOnSchedule(content string) bool {
	lines := strings.Split(content, "\n")
	inOnBlock := false
	childIndent := -1

	for _, raw := range lines {
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(trimmed)

		if indent == 0 {
			inOnBlock = false
			childIndent = -1

			if rest, ok := strings.CutPrefix(raw, "on:"); ok {
				rest = strings.TrimSpace(rest)
				if i := strings.Index(rest, "#"); i >= 0 {
					rest = strings.TrimSpace(rest[:i])
				}
				if rest == "" {
					inOnBlock = true
					continue
				}
				// scalar (`on: schedule`) or flow list (`on: [push, schedule]`)
				rest = strings.Trim(rest, "[]")
				for part := range strings.SplitSeq(rest, ",") {
					if strings.TrimSpace(part) == "schedule" {
						return true
					}
				}
			}
			continue
		}

		if !inOnBlock {
			continue
		}
		if childIndent == -1 {
			childIndent = indent
		}
		if indent == childIndent {
			key := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[0])
			if key == "schedule" {
				return true
			}
		}
	}
	return false
}

type ForbiddenItem struct {
	Path  string
	Rule  string
	Title string
}

func forbiddenPaths() []ForbiddenItem {
	mk := func(path, rule string) ForbiddenItem {
		return ForbiddenItem{Path: path, Rule: rule, Title: "Must not be present: " + path}
	}
	return []ForbiddenItem{
		mk("FUNDING.yml", "common.no_funding_root"),
		mk("funding.yml", "common.no_funding_root_lower"),
		mk(".github/FUNDING.yml", "common.no_funding_github"),
		mk(".github/funding.yml", "common.no_funding_github_lower"),
		mk("renovate.json", "common.no_renovate_root"),
		mk(".github/renovate.json", "common.no_renovate_github"),
		mk("PULL_REQUEST.md", "common.no_pull_request_md_root"),
		mk(".github/PULL_REQUEST.md", "common.no_pull_request_md_github"),
		mk(".github/PULL_REQUEST_TEMPLATE.md", "common.no_pull_request_template"),
		mk(".github/ISSUE_TEMPLATE", "common.no_issue_template_github"),
		mk("ISSUE_TEMPLATE", "common.no_issue_template_root"),
		mk("CHANGELOG.md", "common.no_changelog"),
		mk(".vscode", "common.no_vscode"),
		mk(".editorconfig", "common.no_editorconfig"),
		mk(".gitattributes", "common.no_gitattributes"),
		mk(".github/dependabot.yml", "common.no_dependabot_yml"),
		mk(".github/dependabot.yaml", "common.no_dependabot_yaml"),
		mk(".devcontainer", "common.no_devcontainer"),
		mk("CODE_OF_CONDUCT.md", "common.no_code_of_conduct_root"),
		mk(".github/CODE_OF_CONDUCT.md", "common.no_code_of_conduct_github"),
		mk("NOTICE", "common.no_notice"),
		mk("CONTRIBUTING.md", "common.no_contributing_root"),
		mk(".github/CONTRIBUTING.md", "common.no_contributing_github"),
		mk("CODEOWNERS", "common.no_codeowners_root"),
		mk(".github/CODEOWNERS", "common.no_codeowners_github"),
		mk("docs/CODEOWNERS", "common.no_codeowners_docs"),
	}
}

func checkAbsent(repoPath string, items []ForbiddenItem) []Finding {
	findings := make([]Finding, 0, len(items))
	for _, item := range items {
		p := filepath.Join(repoPath, item.Path)
		_, err := os.Stat(p)
		if err == nil {
			findings = append(findings, Finding{
				RuleID:   item.Rule,
				Title:    item.Title,
				Severity: SevMust,
				Status:   StatusFail,
				File:     item.Path,
				Message:  "File or directory exists and must be removed per StepSecurity onboarding requirements.",
			})
		} else {
			findings = append(findings, Finding{
				RuleID:   item.Rule,
				Title:    item.Title,
				Severity: SevMust,
				Status:   StatusPass,
				File:     item.Path,
			})
		}
	}
	return findings
}

func checkPreCommitTooling(repoPath string) []Finding {
	var findings []Finding

	forbidden := []ForbiddenItem{
		{Path: ".husky", Rule: "precommit.no_husky_dir", Title: "Pre-commit: .husky directory absent"},
		{Path: ".pre-commit-config.yaml", Rule: "precommit.no_pre_commit_config", Title: "Pre-commit: .pre-commit-config.yaml absent"},
		{Path: ".pre-commit-config.yml", Rule: "precommit.no_pre_commit_config_yml", Title: "Pre-commit: .pre-commit-config.yml absent"},
		{Path: "lefthook.yml", Rule: "precommit.no_lefthook", Title: "Pre-commit: lefthook.yml absent"},
		{Path: ".lefthook.yml", Rule: "precommit.no_lefthook_dot", Title: "Pre-commit: .lefthook.yml absent"},
		{Path: "lefthook-local.yml", Rule: "precommit.no_lefthook_local", Title: "Pre-commit: lefthook-local.yml absent"},
	}
	findings = append(findings, checkAbsent(repoPath, forbidden)...)

	// .lintstagedrc* family
	matches, _ := filepath.Glob(filepath.Join(repoPath, ".lintstagedrc*"))
	if len(matches) > 0 {
		for _, m := range matches {
			rel, _ := filepath.Rel(repoPath, m)
			findings = append(findings, Finding{
				RuleID:   "precommit.no_lintstagedrc",
				Title:    "Pre-commit: no lint-staged config file",
				Severity: SevMust,
				Status:   StatusFail,
				File:     rel,
				Message:  "lint-staged config file must be removed.",
			})
		}
	} else {
		findings = append(findings, Finding{
			RuleID:   "precommit.no_lintstagedrc",
			Title:    "Pre-commit: no lint-staged config file",
			Severity: SevMust,
			Status:   StatusPass,
		})
	}

	// package.json: hook deps + husky install scripts
	if data, err := os.ReadFile(filepath.Join(repoPath, "package.json")); err == nil {
		s := string(data)

		depRe := regexp.MustCompile(`"(husky|@evilmartians/lefthook|lefthook|lint-staged)"\s*:`)
		if m := depRe.FindStringSubmatch(s); m != nil {
			findings = append(findings, Finding{
				RuleID:   "precommit.package_json_no_hook_deps",
				Title:    "Pre-commit: package.json has no hook-tooling dependencies",
				Severity: SevMust,
				Status:   StatusFail,
				File:     "package.json",
				Message:  fmt.Sprintf("Found pre-commit hook tooling dependency: %q", m[1]),
			})
		} else {
			findings = append(findings, Finding{
				RuleID:   "precommit.package_json_no_hook_deps",
				Title:    "Pre-commit: package.json has no hook-tooling dependencies",
				Severity: SevMust,
				Status:   StatusPass,
				File:     "package.json",
			})
		}

		scriptRe := regexp.MustCompile(`"(prepare|postinstall)"\s*:\s*"[^"]*husky[^"]*"`)
		if m := scriptRe.FindStringSubmatch(s); m != nil {
			findings = append(findings, Finding{
				RuleID:   "precommit.package_json_no_husky_install_script",
				Title:    "Pre-commit: package.json has no husky install script",
				Severity: SevMust,
				Status:   StatusFail,
				File:     "package.json",
				Message:  fmt.Sprintf("Found husky install script in %q.", m[1]),
			})
		} else {
			findings = append(findings, Finding{
				RuleID:   "precommit.package_json_no_husky_install_script",
				Title:    "Pre-commit: package.json has no husky install script",
				Severity: SevMust,
				Status:   StatusPass,
				File:     "package.json",
			})
		}
	}

	return findings
}

func checkBanner(repoPath string) Finding {
	rule := "common.readme_banner"
	data, err := os.ReadFile(filepath.Join(repoPath, "README.md"))
	if err != nil {
		return Finding{
			RuleID:   rule,
			Title:    "README.md banner image present",
			Severity: SevMust,
			Status:   StatusFail,
			Message:  "README.md not found.",
		}
	}
	if strings.Contains(string(data), "maintained-action-banner.png") {
		return Finding{
			RuleID:   rule,
			Title:    "README.md banner image present",
			Severity: SevMust,
			Status:   StatusPass,
			File:     "README.md",
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "README.md banner image present",
		Severity: SevMust,
		Status:   StatusFail,
		File:     "README.md",
		Message:  "Banner image (maintained-action-banner.png) not referenced in README.md.",
	}
}

func checkSubscriptionURL(repoPath string) Finding {
	rule := "common.subscription_url"
	hits := grepRepo(repoPath, "agent.api.stepsecurity.io/v1/github", 5)
	if len(hits) == 0 {
		return Finding{
			RuleID:   rule,
			Title:    "Subscription check URL present in codebase",
			Severity: SevMust,
			Status:   StatusFail,
			Message:  "No reference to 'agent.api.stepsecurity.io/v1/github' found in repo. Subscription check is missing.",
		}
	}
	endpointHits := grepRepo(repoPath, "maintained-actions-subscription", 5)
	if len(endpointHits) == 0 {
		return Finding{
			RuleID:   rule,
			Title:    "Subscription check uses /maintained-actions-subscription endpoint",
			Severity: SevMust,
			Status:   StatusFail,
			File:     strings.Join(hits, ", "),
			Message:  "Subscription URL found but does not use '/maintained-actions-subscription' endpoint. The older '/subscription' endpoint is deprecated.",
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "Subscription check URL present",
		Severity: SevMust,
		Status:   StatusPass,
		File:     strings.Join(endpointHits, ", "),
		Message:  "Subscription check semantics (invoked first, errors handled correctly) must be verified by reviewer.",
	}
}

func checkSubscriptionUpstream(repoPath, upstream string) Finding {
	rule := "common.subscription_upstream_value"
	if upstream == "" {
		return Finding{
			RuleID:   rule,
			Title:    "Subscription check upstream matches auto_cherry_pick.yml",
			Severity: SevMust,
			Status:   StatusSkip,
			Message:  "Could not extract original-owner/repo-name from auto_cherry_pick.yml; cannot verify subscription check upstream value.",
		}
	}
	hits := grepRepo(repoPath, upstream, 5)
	if len(hits) == 0 {
		return Finding{
			RuleID:   rule,
			Title:    "Subscription check upstream matches auto_cherry_pick.yml",
			Severity: SevMust,
			Status:   StatusFail,
			Message:  fmt.Sprintf("Expected upstream %q (from auto_cherry_pick.yml) not found anywhere in code. Subscription check 'upstream' variable likely has the wrong value.", upstream),
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "Subscription check upstream matches auto_cherry_pick.yml",
		Severity: SevMust,
		Status:   StatusPass,
		File:     strings.Join(hits, ", "),
		Message:  fmt.Sprintf("Upstream %q referenced in repo.", upstream),
	}
}

func checkDocsUpstreamRefs(repoPath, upstream string) Finding {
	rule := "common.docs_upstream_refs"
	if upstream == "" {
		return Finding{
			RuleID:   rule,
			Title:    "Docs do not reference upstream owner/repo as the action",
			Severity: SevShould,
			Status:   StatusSkip,
			Message:  "Could not extract upstream; skipping docs upstream-reference check.",
		}
	}
	re := regexp.MustCompile(`uses:\s*` + regexp.QuoteMeta(upstream) + `(@|[\s/])`)
	var hits []string
	for _, f := range []string{"README.md", "docs/README.md", "USAGE.md"} {
		data, err := os.ReadFile(filepath.Join(repoPath, f))
		if err != nil {
			continue
		}
		if re.MatchString(string(data)) {
			hits = append(hits, f)
		}
	}
	if len(hits) > 0 {
		return Finding{
			RuleID:   rule,
			Title:    "Docs use step-security/<action> in usage examples (not upstream)",
			Severity: SevMust,
			Status:   StatusFail,
			File:     strings.Join(hits, ", "),
			Message:  fmt.Sprintf("Found `uses: %s@...` in docs. Should be `uses: step-security/<action-name>@...`.", upstream),
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "Docs use step-security/<action> in usage examples (not upstream)",
		Severity: SevMust,
		Status:   StatusPass,
	}
}

// ---------- helpers ----------

// grepRepo searches the repo for `pattern`, skipping common heavy/irrelevant
// directories. Returns up to `maxHits` relative file paths that contain it.
func grepRepo(repoPath, pattern string, maxHits int) []string {
	skipDirs := map[string]bool{
		".git":         true,
		"node_modules": true,
		"dist":         true,
		".yarn":        true,
		"vendor":       true,
		"build":        true,
		"coverage":     true,
	}
	skipExt := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".pdf": true,
		".zip": true, ".tar": true, ".gz": true, ".ico": true, ".woff": true,
		".woff2": true, ".ttf": true, ".eot": true, ".svg": true,
	}
	var hits []string
	_ = filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipExt[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), pattern) {
			rel, _ := filepath.Rel(repoPath, path)
			hits = append(hits, rel)
			if maxHits > 0 && len(hits) >= maxHits {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return hits
}

// ---------- node-specific checks ----------

func runNodeChecks(repoPath string) []Finding {
	var findings []Finding
	findings = append(findings, checkDistPresent(repoPath))
	findings = append(findings, checkNpmrcMinReleaseAge(repoPath))
	findings = append(findings, checkActionYMLNodeVersion(repoPath))
	findings = append(findings, checkPackageJSONFields(repoPath)...)
	return findings
}

func checkDistPresent(repoPath string) Finding {
	rule := "node.dist_present"
	info, err := os.Stat(filepath.Join(repoPath, "dist"))
	if err != nil || !info.IsDir() {
		return Finding{
			RuleID:   rule,
			Title:    "dist/ directory present",
			Severity: SevMust,
			Status:   StatusFail,
			Message:  "dist/ directory is missing. Node-based actions must ship a built bundle.",
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "dist/ directory present",
		Severity: SevMust,
		Status:   StatusPass,
		File:     "dist",
	}
}

func checkNpmrcMinReleaseAge(repoPath string) Finding {
	rule := "node.npmrc_min_release_age"
	data, err := os.ReadFile(filepath.Join(repoPath, ".npmrc"))
	if err != nil {
		return Finding{
			RuleID:   rule,
			Title:    ".npmrc present with min-release-age=3",
			Severity: SevMust,
			Status:   StatusFail,
			Message:  ".npmrc file missing.",
		}
	}
	re := regexp.MustCompile(`(?m)^\s*min-release-age\s*=\s*3\s*$`)
	if !re.Match(data) {
		return Finding{
			RuleID:   rule,
			Title:    ".npmrc present with min-release-age=3",
			Severity: SevMust,
			Status:   StatusFail,
			File:     ".npmrc",
			Message:  ".npmrc exists but does not contain 'min-release-age=3'.",
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    ".npmrc present with min-release-age=3",
		Severity: SevMust,
		Status:   StatusPass,
		File:     ".npmrc",
	}
}

func checkActionYMLNodeVersion(repoPath string) Finding {
	rule := "node.action_yml_node24"
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		using := strings.ToLower(extractYAMLValue(string(data), "using"))
		if using == "node24" {
			return Finding{
				RuleID:   rule,
				Title:    "action.yml uses node24 runtime",
				Severity: SevMust,
				Status:   StatusPass,
				File:     name,
			}
		}
		return Finding{
			RuleID:   rule,
			Title:    "action.yml uses node24 runtime",
			Severity: SevMust,
			Status:   StatusFail,
			File:     name,
			Message:  fmt.Sprintf("runs.using=%q (expected: node24)", using),
		}
	}
	return Finding{
		RuleID:   rule,
		Title:    "action.yml uses node24 runtime",
		Severity: SevMust,
		Status:   StatusFail,
		Message:  "action.yml not found.",
	}
}

func checkPackageJSONFields(repoPath string) []Finding {
	authorRule := "node.package_json_author"
	repoRule := "node.package_json_repository"

	data, err := os.ReadFile(filepath.Join(repoPath, "package.json"))
	if err != nil {
		return []Finding{
			{
				RuleID: authorRule, Title: "package.json author is step-security (if present)",
				Severity: SevMust, Status: StatusSkip, Message: "package.json not found.",
			},
			{
				RuleID: repoRule, Title: "package.json repository contains step-security (if present)",
				Severity: SevMust, Status: StatusSkip, Message: "package.json not found.",
			},
		}
	}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return []Finding{{
			RuleID:   "node.package_json_parse",
			Title:    "package.json parses as JSON",
			Severity: SevMust,
			Status:   StatusFail,
			File:     "package.json",
			Message:  fmt.Sprintf("Failed to parse package.json: %v", err),
		}}
	}

	var findings []Finding

	// author: can be string or object {name, email, url}
	if raw, ok := pkg["author"]; ok {
		authorName := ""
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			authorName = asString
		} else {
			var asObj struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &asObj); err == nil {
				authorName = asObj.Name
			}
		}
		if strings.Contains(strings.ToLower(authorName), "step-security") {
			findings = append(findings, Finding{
				RuleID: authorRule, Title: "package.json author is step-security",
				Severity: SevMust, Status: StatusPass, File: "package.json",
			})
		} else {
			findings = append(findings, Finding{
				RuleID: authorRule, Title: "package.json author is step-security",
				Severity: SevMust, Status: StatusFail, File: "package.json",
				Message: fmt.Sprintf("package.json author=%q (expected to contain 'step-security')", authorName),
			})
		}
	} else {
		findings = append(findings, Finding{
			RuleID: authorRule, Title: "package.json author is step-security (if present)",
			Severity: SevMust, Status: StatusSkip, File: "package.json",
			Message: "author field not present in package.json; rule does not apply.",
		})
	}

	// repository: string or {url}
	if raw, ok := pkg["repository"]; ok {
		repoStr := ""
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			repoStr = asString
		} else {
			var asObj struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(raw, &asObj); err == nil {
				repoStr = asObj.URL
			}
		}
		if strings.Contains(strings.ToLower(repoStr), "step-security") {
			findings = append(findings, Finding{
				RuleID: repoRule, Title: "package.json repository contains step-security",
				Severity: SevMust, Status: StatusPass, File: "package.json",
			})
		} else {
			findings = append(findings, Finding{
				RuleID: repoRule, Title: "package.json repository contains step-security",
				Severity: SevMust, Status: StatusFail, File: "package.json",
				Message: fmt.Sprintf("package.json repository=%q (expected to contain 'step-security')", repoStr),
			})
		}
	} else {
		findings = append(findings, Finding{
			RuleID: repoRule, Title: "package.json repository contains step-security (if present)",
			Severity: SevMust, Status: StatusSkip, File: "package.json",
			Message: "repository field not present in package.json; rule does not apply.",
		})
	}

	return findings
}


func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------- docker-specific checks ----------

func runDockerChecks(repoPath string) []Finding {
	return []Finding{
		checkDockerWorkflowPresent(repoPath),
		checkDockerfileJq(repoPath),
	}
}

func checkDockerWorkflowPresent(repoPath string) Finding {
	rule := "docker.workflow_present"
	for _, name := range []string{"docker.yml", "docker.yaml"} {
		p := filepath.Join(repoPath, ".github", "workflows", name)
		if fileExists(p) {
			return Finding{
				RuleID: rule, Title: ".github/workflows/docker.yml present",
				Severity: SevMust, Status: StatusPass,
				File: filepath.Join(".github/workflows", name),
			}
		}
	}
	return Finding{
		RuleID: rule, Title: ".github/workflows/docker.yml present",
		Severity: SevMust, Status: StatusFail,
		Message: "docker.yml workflow is required for docker-based actions but was not found in .github/workflows/.",
	}
}

// checkDockerfileJq verifies `jq` is installed in the Dockerfile, but only when
// the Dockerfile's ENTRYPOINT/CMD invokes a shell script (`.sh`). When the
// entry-point is a JS/TS/Python/Go binary, the subscription check lives in that
// language and jq isn't required.
func checkDockerfileJq(repoPath string) Finding {
	rule := "docker.dockerfile_jq"
	var dockerfilePath string
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		p := filepath.Join(repoPath, name)
		if fileExists(p) {
			dockerfilePath = p
			break
		}
	}
	if dockerfilePath == "" {
		return Finding{
			RuleID: rule, Title: "Dockerfile installs jq (when entrypoint is a shell script)",
			Severity: SevMust, Status: StatusSkip,
			Message: "No Dockerfile found at repo root.",
		}
	}
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return Finding{
			RuleID: rule, Title: "Dockerfile installs jq",
			Severity: SevMust, Status: StatusSkip,
			Message: fmt.Sprintf("Failed to read Dockerfile: %v", err),
		}
	}
	content := string(data)

	// detect whether ENTRYPOINT/CMD invokes a .sh script
	entryRe := regexp.MustCompile(`(?im)^\s*(ENTRYPOINT|CMD)\b.*\.sh\b`)
	if !entryRe.MatchString(content) {
		return Finding{
			RuleID: rule, Title: "Dockerfile installs jq (when entrypoint is a shell script)",
			Severity: SevMust, Status: StatusSkip,
			File:    "Dockerfile",
			Message: "Dockerfile entrypoint/CMD is not a shell script; jq is not required (subscription check lives in the invoked binary).",
		}
	}

	jqRe := regexp.MustCompile(`(?i)\bjq\b`)
	if !jqRe.MatchString(content) {
		return Finding{
			RuleID: rule, Title: "Dockerfile installs jq (entrypoint is a shell script)",
			Severity: SevMust, Status: StatusFail,
			File:    "Dockerfile",
			Message: "Dockerfile invokes a shell script via ENTRYPOINT/CMD but does not install jq. The subscription check requires jq.",
		}
	}
	return Finding{
		RuleID: rule, Title: "Dockerfile installs jq (entrypoint is a shell script)",
		Severity: SevMust, Status: StatusPass,
		File: "Dockerfile",
	}
}

// ---------- composite-specific checks ----------

func runCompositeChecks(repoPath string) []Finding {
	return []Finding{checkCompositeFirstStepSubscription(repoPath)}
}

// checkCompositeFirstStepSubscription checks that the first step under
// `runs.steps` in action.yml contains the subscription check (i.e. references
// the agent.api.stepsecurity.io URL).
func checkCompositeFirstStepSubscription(repoPath string) Finding {
	rule := "composite.first_step_subscription"
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		firstStep, ok := extractFirstCompositeStep(string(data))
		if !ok {
			return Finding{
				RuleID: rule, Title: "First composite step is the subscription check",
				Severity: SevMust, Status: StatusFail, File: name,
				Message: "Could not locate any steps under runs.steps in action.yml.",
			}
		}
		if strings.Contains(firstStep, "agent.api.stepsecurity.io/v1/github") {
			return Finding{
				RuleID: rule, Title: "First composite step is the subscription check",
				Severity: SevMust, Status: StatusPass, File: name,
			}
		}
		return Finding{
			RuleID: rule, Title: "First composite step is the subscription check",
			Severity: SevMust, Status: StatusFail, File: name,
			Message: "First step under runs.steps does not contain the subscription check URL. The subscription bash snippet must be the first step.",
		}
	}
	return Finding{
		RuleID: rule, Title: "First composite step is the subscription check",
		Severity: SevMust, Status: StatusFail,
		Message: "action.yml not found.",
	}
}

// extractFirstCompositeStep returns the YAML block of the first step under
// `runs.steps`. It is a best-effort regex-based extractor — sufficient for the
// shape produced by the onboarding spec.
func extractFirstCompositeStep(content string) (string, bool) {
	stepsRe := regexp.MustCompile(`(?m)^\s*steps:\s*$`)
	loc := stepsRe.FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	after := content[loc[1]:]

	// find first list item (`-` at any indent) after steps:
	itemRe := regexp.MustCompile(`(?m)^(\s*)-\s`)
	itemLoc := itemRe.FindStringSubmatchIndex(after)
	if itemLoc == nil {
		return "", false
	}
	indent := after[itemLoc[2]:itemLoc[3]]
	start := itemLoc[0]

	// find the next list item at the same indent (or a sibling top-level key) to bound the step
	endRe := regexp.MustCompile(fmt.Sprintf(`(?m)^%s-\s|^[A-Za-z]`, regexp.QuoteMeta(indent)))
	endLoc := endRe.FindStringIndex(after[itemLoc[1]:])
	if endLoc == nil {
		return after[start:], true
	}
	return after[start : itemLoc[1]+endLoc[0]], true
}
