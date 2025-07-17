package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
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
		log.Fatal("❌ GITHUB_TOKEN not provided")
	}

	ignoredPaths = strings.Split(ignored, ",")

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	fullRepo := os.Getenv("GITHUB_REPOSITORY")

	// Always parse from GITHUB_REPOSITORY since it's the full repo name
	parts := strings.Split(fullRepo, "/")
	if len(parts) != 2 {
		log.Fatalf("❌ Invalid GITHUB_REPOSITORY format: %s", fullRepo)
	}

	repoOwner, repoName := parts[0], parts[1]
	fmt.Printf("🔍 Looking for PR with branch: %s in %s/%s\n", prBranch, repoOwner, repoName)
	pr, err := findCherryPickPR(ctx, client, repoOwner, repoName, prBranch)
	if err != nil {
		log.Fatalf("❌ Unable to locate cherry-pick PR: %v", err)
	}

	prHeadSHA := pr.GetHead().GetSHA()
	fmt.Printf("🔍 Using PR head SHA: %s\n", prHeadSHA)

	// Get PR comments to find Target Release Version
	comments, _, err := client.Issues.ListComments(ctx, repoOwner, repoName, pr.GetNumber(), nil)
	if err != nil {
		log.Fatalf("❌ Failed to get PR comments: %v", err)
	}

	targetVersion, err := extractTargetReleaseVersionFromComments(comments)
	if err != nil {
		log.Fatalf("❌ Failed to extract target release version: %v", err)
	}

	targetTag := targetVersion
	prevTag, err := getPreviousTag(ctx, client, upstreamOwner, upstreamRepo, targetTag)
	if err != nil {
		log.Fatalf("❌ Failed to find previous tag: %v", err)
	}

	fmt.Printf("🔍 Comparing %s...%s from upstream\n\n", prevTag, targetTag)

	compare, _, err := client.Repositories.CompareCommits(ctx, upstreamOwner, upstreamRepo, prevTag, targetTag, nil)
	if err != nil {
		log.Fatalf("❌ Failed to get compare: %v", err)
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

		// Get the base content from our PR's base branch (usually main)
		fmt.Printf("🔍 Getting base content for %s from %s/%s@%s\n", path, repoOwner, repoName, baseBranch)
		baseContent, err := getFileContent(ctx, client, repoOwner, repoName, path, baseBranch)
		if err != nil {
			// File might be new in upstream
			baseContent = ""
		}

		// Get the current content in our PR branch
		fmt.Printf("🔍 Getting PR content for %s from %s/%s@%s\n", path, repoOwner, repoName, prHeadSHA)
		prContent, err := getFileContent(ctx, client, repoOwner, repoName, path, prHeadSHA)
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

		// Check if the PR content matches what it should be after applying upstream changes
		isApplied, diffSummary := verifyPatchApplied(baseContent, prContent, upstreamPatch, f.GetAdditions(), f.GetDeletions())

		status := "missing"
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
		log.Fatalf("❌ Failed to post comment to PR: %v", err)
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

func getPreviousTag(ctx context.Context, client *github.Client, owner, repo, currentTag string) (string, error) {
	releases, _, err := client.Repositories.ListReleases(ctx, owner, repo, &github.ListOptions{PerPage: 100})
	if err != nil {
		return "", err
	}
	var tags []string
	for _, r := range releases {
		tags = append(tags, r.GetTagName())
	}
	sort.Slice(tags, func(i, j int) bool {
		return tags[i] < tags[j]
	})
	for i, tag := range tags {
		if tag == currentTag && i > 0 {
			return tags[i-1], nil
		}
	}
	return "", fmt.Errorf("could not determine previous tag")
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
	content, _ := file.GetContent()

	return content, nil
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
	report.WriteString(fmt.Sprintf("- **Files with matching changes:** %d/%d\n", changesMatched, filesInPR))

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

func verifyPatchApplied(baseContent, prContent, upstreamPatch string, additions, deletions int) (bool, string) {
	// Parse the patch to extract the specific changes
	patchLines := strings.Split(upstreamPatch, "\n")

	addedLines := []string{}
	removedLines := []string{}

	for _, line := range patchLines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			addedLines = append(addedLines, strings.TrimPrefix(line, "+"))
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removedLines = append(removedLines, strings.TrimPrefix(line, "-"))
		}
	}

	// Check if all added lines are present in PR content
	missingAdditions := []string{}
	for _, addedLine := range addedLines {
		if !strings.Contains(prContent, addedLine) {
			missingAdditions = append(missingAdditions, addedLine)
		}
	}

	// Check if all removed lines are absent from PR content
	unexpectedLines := []string{}
	for _, removedLine := range removedLines {
		if strings.Contains(prContent, removedLine) {
			unexpectedLines = append(unexpectedLines, removedLine)
		}
	}

	// Generate summary
	if len(missingAdditions) == 0 && len(unexpectedLines) == 0 {
		return true, fmt.Sprintf("✅ All changes applied correctly (+%d -%d)", additions, deletions)
	}

	summary := fmt.Sprintf("❌ Cherry-pick incomplete (+%d -%d)", additions, deletions)
	if len(missingAdditions) > 0 {
		summary += fmt.Sprintf(" | Missing %d additions", len(missingAdditions))
	}
	if len(unexpectedLines) > 0 {
		summary += fmt.Sprintf(" | %d lines should be removed", len(unexpectedLines))
	}

	return false, summary
}
