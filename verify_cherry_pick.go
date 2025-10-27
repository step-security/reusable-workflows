package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

var ignoredPaths []string

type FileComparison struct {
	Path         string
	Status       string // "matched", "missing", "conflicting"
	UpstreamHash string
	PRHash       string
	DiffSummary  string
}

func main() {
	var upstreamOwner, upstreamRepo, baseBranch, prBranch, token, ignored string
	flag.StringVar(&upstreamOwner, "upstream-owner", "", "Upstream GitHub owner")
	flag.StringVar(&upstreamRepo, "upstream-repo", "", "Upstream GitHub repo name")
	flag.StringVar(&baseBranch, "base-branch", "main", "Base branch name")
	flag.StringVar(&prBranch, "pr-branch", "auto-cherry-pick", "PR branch to verify")
	flag.StringVar(&ignored, "ignored-paths", "", "Comma-separated list of ignored paths")
	flag.StringVar(&token, "token", os.Getenv("GITHUB_TOKEN"), "GitHub token")
	flag.Parse()

	if token == "" {
		fmt.Println("❌ GITHUB_TOKEN not provided")
		return
	}

	if ignored != "" {
		ignoredPaths = strings.Split(ignored, ",")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	fullRepo := os.Getenv("GITHUB_REPOSITORY")

	// Always parse from GITHUB_REPOSITORY since it's the full repo name
	parts := strings.Split(fullRepo, "/")
	if len(parts) != 2 {
		fmt.Printf("❌ Invalid GITHUB_REPOSITORY format: %s\n", fullRepo)
		return
	}

	repoOwner, repoName := parts[0], parts[1]
	fmt.Printf("🔍 Looking for PR with branch: %s in %s/%s\n", prBranch, repoOwner, repoName)
	pr, err := findCherryPickPR(ctx, client, repoOwner, repoName, prBranch)
	if err != nil {
		fmt.Printf("❌ Unable to locate cherry-pick PR: %v\n", err)
		return
	}

	prHeadSHA := pr.GetHead().GetSHA()
	fmt.Printf("🔍 Using PR head SHA: %s\n", prHeadSHA)

	// Get PR comments to find Target Release Version
	comments, _, err := client.Issues.ListComments(ctx, repoOwner, repoName, pr.GetNumber(), nil)
	if err != nil {
		fmt.Printf("❌ Failed to get PR comments: %v\n", err)
		return
	}

	targetVersion, err := extractTargetReleaseVersionFromComments(comments)
	if err != nil {
		fmt.Printf("❌ Failed to extract target release version: %v\n", err)
		return
	}

	targetTag := targetVersion
	prevTag, err := extractPreviousReleaseVersionFromComments(comments)
	if err != nil {
		fmt.Printf("❌ Failed to find previous tag: %v\n", err)
		return
	}

	fmt.Printf("🔍 Comparing %s...%s from upstream\n\n", prevTag, targetTag)

	compare, _, err := client.Repositories.CompareCommits(ctx, upstreamOwner, upstreamRepo, prevTag, targetTag, nil)
	if err != nil {
		fmt.Printf("❌ Failed to get compare: %v\n", err)
		return
	}

	var comparisons []FileComparison

	for _, f := range compare.Files {
		path := f.GetFilename()
		if isIgnored(path) {
			continue
		}

		fmt.Printf("🔍 Analyzing changes for file: %s\n", path)

		// Get the specific patch/diff for this file from the upstream commit
		upstreamPatch := f.GetPatch()
		if upstreamPatch == "" {
			log.Printf("⚠️  No patch data for file: %s\n", path)
			continue
		}

		// Check if file exists in PR branch
		fmt.Printf("🔍 Checking if %s exists in PR branch %s\n", path, prHeadSHA)
		_, err := getFileContent(ctx, client, repoOwner, repoName, path, prHeadSHA)
		if err != nil {
			fmt.Printf("❌ Failed to get PR content: %v\n", err)
			// Check if the file should exist by checking if it exists upstream
			_, upstreamErr := getFileContent(ctx, client, upstreamOwner, upstreamRepo, path, targetTag)
			if upstreamErr == nil {
				// File exists upstream but not in PR - it's missing
				comparisons = append(comparisons, FileComparison{
					Path:   path,
					Status: "missing",
					DiffSummary: fmt.Sprintf("File missing in PR (upstream has %d additions, %d deletions)",
						f.GetAdditions(), f.GetDeletions()),
				})
			} else {
				// File doesn't exist in either - skip
				fmt.Printf("⚠️ File doesn't exist in upstream either, skipping: %s\n", path)
			}
			continue
		}
		// Debug output can be added with a debug flag if needed

		// Get PR patch for this file
		fmt.Printf("🔍 Getting PR patch for %s from %s/%s %s...%s\n", path, repoOwner, repoName, baseBranch, prHeadSHA)
		prPatch, err := getPRPatchForFile(ctx, client, repoOwner, repoName, baseBranch, prHeadSHA, path)
		if err != nil {
			fmt.Printf("⚠️ Failed to get PR patch: %v\n", err)
			prPatch = "" // Continue with empty patch
		}

		var status string
		if prPatch == "" {
			status = "missing"
		}

		// Compare upstream patch with PR patch to verify cherry-pick
		isApplied, diffSummary := comparePatches(upstreamPatch, prPatch, f.GetAdditions(), f.GetDeletions())

		if isApplied {
			status = "matched"
		}

		comparisons = append(comparisons, FileComparison{
			Path:        path,
			Status:      status,
			DiffSummary: diffSummary,
		})
	}

	// Generate the markdown report
	report := generateDetailedMarkdownReport(targetTag, prevTag, comparisons)

	// Post the comment to the PR
	comment := &github.IssueComment{
		Body: &report,
	}
	_, _, err = client.Issues.CreateComment(ctx, repoOwner, repoName, pr.GetNumber(), comment)
	if err != nil {
		fmt.Printf("❌ Failed to post comment to PR: %v\n", err)
		return
	}

	fmt.Println("✅ Verification comment posted to PR successfully")

	// Also print to stdout for debugging
	fmt.Println(report)
}

func findCherryPickPR(ctx context.Context, client *github.Client, owner, repo, branch string) (*github.PullRequest, error) {
	opts := &github.PullRequestListOptions{Head: fmt.Sprintf("%s:%s", owner, branch), State: "open", ListOptions: github.ListOptions{PerPage: 10}}
	prs, _, err := client.PullRequests.List(ctx, owner, repo, opts)
	if err != nil || len(prs) == 0 {
		return nil, fmt.Errorf("no open PR found for branch %s", branch)
	}
	return prs[0], nil
}

func extractTargetReleaseVersionFromComments(comments []*github.IssueComment) (string, error) {
	for _, comment := range comments {
		body := comment.GetBody()
		if version := extractVersionFromText(body); version != "" {
			return version, nil
		}
	}
	return "", fmt.Errorf("target Release Version not found in PR comments")
}

func extractVersionFromText(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "📦 Target Release Version:") {
			parts := strings.Split(line, "`")
			if len(parts) >= 2 {
				return parts[1]
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[len(fields)-1]
			}
		}
	}
	return ""
}

func extractPreviousReleaseVersionFromComments(comments []*github.IssueComment) (string, error) {
	for _, comment := range comments {
		body := comment.GetBody()
		if version := extractPreviousVersionFromText(body); version != "" {
			return version, nil
		}
	}
	return "", fmt.Errorf("previous Release Version not found in PR comments")
}

func extractPreviousVersionFromText(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "📋 Previous Release Version:") {
			parts := strings.Split(line, "`")
			if len(parts) >= 2 {
				return parts[1]
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[len(fields)-1]
			}
		}
	}
	return ""
}

func isIgnored(path string) bool {
	for _, ign := range ignoredPaths {
		if strings.HasSuffix(ign, "/") {
			if strings.HasPrefix(path, ign) {
				return true
			}
		} else if path == ign {
			return true
		}
	}
	return false
}

func getFileContent(ctx context.Context, client *github.Client, owner, repo, path, ref string) (string, error) {
	file, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	//fmt.Println(file.GetContent())
	if err != nil || file == nil {
		return "", err
	}
	content, err := file.GetContent()
	if err != nil {
		return "", err
	}
	return content, nil
}

func getPRPatchForFile(ctx context.Context, client *github.Client, owner, repo, base, head, filePath string) (string, error) {
	// Compare base branch against PR head to get the diff
	compare, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return "", fmt.Errorf("failed to compare commits: %v", err)
	}

	// Find the specific file in the comparison
	for _, file := range compare.Files {
		if file.GetFilename() == filePath {
			patch := file.GetPatch()
			if patch == "" {
				return "", fmt.Errorf("no patch data for file: %s", filePath)
			}
			return patch, nil
		}
	}

	return "", fmt.Errorf("file not found in PR diff: %s", filePath)
}

func generateDetailedMarkdownReport(target, prev string, comparisons []FileComparison) string {
	var report strings.Builder

	report.WriteString("## 🔍 Cherry-Pick Verification Report\n")
	report.WriteString(fmt.Sprintf("📦 **Upstream Changes:** `%s...%s`\n\n", prev, target))

	// Count totals for summary
	totalFiles := len(comparisons)
	filesInPR := 0
	changesMatched := 0

	report.WriteString("### 📋 **File-by-File Analysis:**\n\n")

	for _, comp := range comparisons {
		report.WriteString(fmt.Sprintf("#### `%s`\n", comp.Path))

		// Step 1: Does upstream have changes in this file?
		report.WriteString("- **Upstream has changes:** ✅ Yes\n")

		// Step 2: Do we have this file in PR?
		if comp.Status == "missing" {
			report.WriteString("- **File exists in PR:** ❌ No\n")
			report.WriteString(fmt.Sprintf("- **Status:** 🔴 Missing - %s\n\n", comp.DiffSummary))
		} else {
			filesInPR++
			report.WriteString("- **File exists in PR:** ✅ Yes\n")

			// Step 3: Do the changes match?
			if comp.Status == "matched" {
				changesMatched++
				report.WriteString("- **Changes match:** ✅ Yes\n")
				report.WriteString(fmt.Sprintf("- **Status:** 🟢 Perfect - %s\n\n", comp.DiffSummary))
			} else {
				report.WriteString("- **Changes match:** ❌ No\n")
				report.WriteString(fmt.Sprintf("- **Status:** 🟡 Partial - %s\n\n", comp.DiffSummary))
			}
		}
	}

	// Summary section
	report.WriteString("---\n")
	report.WriteString("### 📊 **Summary:**\n")
	report.WriteString(fmt.Sprintf("- **Total files changed upstream:** %d\n", totalFiles))
	report.WriteString(fmt.Sprintf("- **Files present in PR:** %d/%d\n", filesInPR, totalFiles))
	report.WriteString(fmt.Sprintf("- **Files with matching changes:** %d/%d\n", changesMatched, totalFiles))

	// Overall status
	if changesMatched == totalFiles {
		report.WriteString("\n🎉 **Overall Status:** ✅ **PERFECT** - All upstream changes successfully applied!")
	} else if filesInPR == totalFiles && changesMatched < totalFiles {
		report.WriteString("\n⚠️ **Overall Status:** 🟡 **PARTIAL** - All files present but some changes missing")
	} else {
		report.WriteString("\n❌ **Overall Status:** 🔴 **INCOMPLETE** - Missing files or changes")
	}

	return report.String()
}

func comparePatches(upstreamPatch, prPatch string, additions, deletions int) (bool, string) {
	if prPatch == "" {
		return false, fmt.Sprintf("❌ No PR patch available (+%d -%d)", additions, deletions)
	}

	// Extract changes from both patches
	upstreamChanges := extractPatchChanges(upstreamPatch)
	prChanges := extractPatchChanges(prPatch)

	// Compare the changes
	missingAdditions := []string{}
	missingDeletions := []string{}
	extraAdditions := []string{}

	// Check if all upstream additions are in PR
	for _, upstreamAdd := range upstreamChanges.Additions {
		found := false
		for _, prAdd := range prChanges.Additions {
			if strings.TrimSpace(upstreamAdd) == strings.TrimSpace(prAdd) {
				found = true
				break
			}
		}
		if !found {
			missingAdditions = append(missingAdditions, upstreamAdd)
		}
	}

	// Check if all upstream deletions are in PR
	for _, upstreamDel := range upstreamChanges.Deletions {
		found := false
		for _, prDel := range prChanges.Deletions {
			if strings.TrimSpace(upstreamDel) == strings.TrimSpace(prDel) {
				found = true
				break
			}
		}
		if !found {
			missingDeletions = append(missingDeletions, upstreamDel)
		}
	}

	// Check for extra additions in PR that aren't in upstream (could be legitimate)
	for _, prAdd := range prChanges.Additions {
		found := false
		for _, upstreamAdd := range upstreamChanges.Additions {
			if strings.TrimSpace(prAdd) == strings.TrimSpace(upstreamAdd) {
				found = true
				break
			}
		}
		if !found {
			extraAdditions = append(extraAdditions, prAdd)
		}
	}

	// Generate summary
	if len(missingAdditions) == 0 && len(missingDeletions) == 0 {
		if len(extraAdditions) > 0 {
			return true, fmt.Sprintf("✅ All upstream changes applied (+%d -%d) with %d additional changes",
				additions, deletions, len(extraAdditions))
		}
		return true, fmt.Sprintf("✅ All changes applied correctly (+%d -%d)", additions, deletions)
	}

	summary := fmt.Sprintf("❌ Cherry-pick incomplete (+%d -%d)", additions, deletions)
	if len(missingAdditions) > 0 {
		summary += fmt.Sprintf(" | Missing %d additions", len(missingAdditions))
	}
	if len(missingDeletions) > 0 {
		summary += fmt.Sprintf(" | Missing %d deletions", len(missingDeletions))
	}

	return false, summary
}

type PatchChanges struct {
	Additions []string
	Deletions []string
}

func extractPatchChanges(patch string) PatchChanges {
	changes := PatchChanges{}
	lines := strings.Split(patch, "\n")

	for _, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			changes.Additions = append(changes.Additions, strings.TrimPrefix(line, "+"))
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			changes.Deletions = append(changes.Deletions, strings.TrimPrefix(line, "-"))
		}
	}

	return changes
}
