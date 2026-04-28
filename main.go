package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	regressionLabelPattern = regexp.MustCompile(`^regression in (\d+(?:\.\d+)+)$`)
	versionPattern         = regexp.MustCompile(`^\d+(?:\.\d+)+$`)
	githubIssueURLPattern  = regexp.MustCompile(`/issues/(\d+)$`)
)

// config bundles all command-line parameters accepted by the tool.
type config struct {
	// repoRoot is the path to the Git repository that contains .git/git-bug.
	repoRoot string
	// outDir is the directory in which all report files are written.
	outDir string
	// owner is the GitHub repository owner used for milestone lookups.
	owner string
	// repo is the GitHub repository name used for milestone lookups.
	repo string
}

// gitBugIssue matches the subset of `git bug bug --format json` used by this tool.
type gitBugIssue struct {
	// HumanID is git-bug's short human-readable identifier for the issue.
	HumanID string `json:"human_id"`
	// Status is the current issue status as reported by git-bug, typically "open" or "closed".
	Status string `json:"status"`
	// Labels contains the issue's current label set.
	Labels []string `json:"labels"`
	// Title is the issue title.
	Title string `json:"title"`
	// Metadata stores bridge-specific metadata such as the original GitHub issue URL.
	Metadata map[string]string `json:"metadata"`
}

// regressionIssue is the normalized internal representation used for aggregation.
type regressionIssue struct {
	// IssueNumber is the original GitHub issue number.
	IssueNumber int
	// HumanID is git-bug's short identifier for the same issue.
	HumanID string
	// Title is the issue title.
	Title string
	// URL is the original GitHub issue URL.
	URL string
	// IntroducedVersion is the version used for aggregation; when several regression labels
	// are present, this is the earliest version among them.
	IntroducedVersion string
	// RegressionLabels lists all distinct regression versions found on the issue.
	RegressionLabels []string
	// Status is the current issue status.
	Status string
	// ClosingMilestone is the GitHub milestone title for closed issues when available.
	ClosingMilestone string
}

// regressionRecord is the JSON-ready form of a regression issue.
type regressionRecord struct {
	// IssueNumber is the original GitHub issue number.
	IssueNumber int `json:"issue_number"`
	// HumanID is git-bug's short identifier for the issue.
	HumanID string `json:"human_id,omitempty"`
	// Title is the issue title.
	Title string `json:"title"`
	// Status is the current issue status.
	Status string `json:"status"`
	// URL is the original GitHub issue URL.
	URL string `json:"url"`
	// RegressionLabels lists all regression versions present on the issue.
	RegressionLabels []string `json:"regression_labels,omitempty"`
	// ClosingMilestone is the closing milestone when available and relevant.
	ClosingMilestone string `json:"closing_milestone,omitempty"`
}

// regressionsByVersionFile is written to regressions-by-version.json.
type regressionsByVersionFile struct {
	// Versions is the global version order used by the file.
	Versions []string `json:"versions"`
	// Regressions maps an introduced version to the regression issues attributed to it.
	Regressions map[string][]regressionRecord `json:"regressions"`
	// Unclassified lists closed regressions that could not be assigned to a version milestone.
	Unclassified []regressionRecord `json:"unclassified_closed_regressions,omitempty"`
}

// milestoneCountsFile is written to the milestone aggregate JSON files.
type milestoneCountsFile struct {
	// Milestones is the ordered milestone axis used by the counts matrix.
	Milestones []string `json:"milestones"`
	// Versions is the ordered introduced-version axis used by the counts matrix.
	Versions []string `json:"versions"`
	// Counts maps each milestone to a map from introduced version to count.
	Counts map[string]map[string]int `json:"counts"`
}

// graphQLRequest is the POST payload sent to GitHub's GraphQL endpoint.
type graphQLRequest struct {
	// Query is the GraphQL query text.
	Query string `json:"query"`
}

// graphQLIssue matches the subset of GitHub GraphQL issue data used for milestone lookup.
type graphQLIssue struct {
	// Number is the GitHub issue number.
	Number int `json:"number"`
	// State is the GitHub issue state.
	State string `json:"state"`
	// Milestone is the current milestone attached to the GitHub issue, when any.
	Milestone *struct {
		// Title is the milestone title.
		Title string `json:"title"`
	} `json:"milestone"`
}

// graphQLRepository captures aliased issue lookups under the repository node.
type graphQLRepository struct {
	// Fields maps GraphQL aliases such as i1234 to their decoded issue payloads.
	Fields map[string]graphQLIssue
}

// UnmarshalJSON decodes a repository object with dynamic alias fields.
//
// The input data must be a JSON object whose values are either issue objects or null.
// It returns the decoded alias-to-issue map.
func (r *graphQLRepository) UnmarshalJSON(data []byte) error {
	type rawRepository map[string]json.RawMessage
	var raw rawRepository
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Fields = make(map[string]graphQLIssue, len(raw))
	for key, value := range raw {
		if bytes.Equal(value, []byte("null")) {
			continue
		}
		var issue graphQLIssue
		if err := json.Unmarshal(value, &issue); err != nil {
			return err
		}
		r.Fields[key] = issue
	}
	return nil
}

type graphQLResponse struct {
	// Data holds the successful GraphQL response payload.
	Data struct {
		// Repository contains the aliased issue lookups.
		Repository graphQLRepository `json:"repository"`
	} `json:"data"`
	// Errors contains GraphQL-level errors returned by GitHub.
	Errors []struct {
		// Message is GitHub's human-readable error message.
		Message string `json:"message"`
	} `json:"errors"`
}

// main parses flags, runs the tool, and reports fatal errors to stderr.
func main() {
	cfg := config{}
	flag.StringVar(&cfg.repoRoot, "repo-root", ".", "path to the git repository root")
	flag.StringVar(&cfg.outDir, "out-dir", ".", "directory for generated files")
	flag.StringVar(&cfg.owner, "owner", "agda", "GitHub repository owner")
	flag.StringVar(&cfg.repo, "repo", "agda", "GitHub repository name")
	flag.Parse()

	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run executes the full extraction and report-generation pipeline.
//
// The context controls subprocesses and HTTP requests. cfg.repoRoot must point at a Git
// repository with accessible git-bug data, and cfg.outDir must be writable. The function
// returns after writing all report files or the first encountered error.
func run(ctx context.Context, cfg config) error {
	repoRoot, err := filepath.Abs(cfg.repoRoot)
	if err != nil {
		return err
	}
	outDir, err := filepath.Abs(cfg.outDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	issues, err := loadRegressionIssues(ctx, repoRoot)
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		return errors.New("no regression issues found")
	}

	closedIssueNumbers := make([]int, 0, len(issues))
	for _, issue := range issues {
		if issue.Status == "closed" {
			closedIssueNumbers = append(closedIssueNumbers, issue.IssueNumber)
		}
	}

	milestones := map[int]string{}
	if len(closedIssueNumbers) > 0 {
		// Closing milestones are not available in local git-bug JSON, so enrich closed
		// issues with GitHub data before aggregating.
		token, err := githubToken(ctx)
		if err != nil {
			return err
		}
		milestones, err = fetchClosingMilestones(ctx, cfg.owner, cfg.repo, token, closedIssueNumbers)
		if err != nil {
			return err
		}
	}

	for i := range issues {
		if issues[i].Status == "closed" {
			issues[i].ClosingMilestone = milestones[issues[i].IssueNumber]
		}
	}

	versionOrder, milestoneOrder, unclassifiedClosed := collectVersions(issues)
	if len(milestoneOrder) == 0 {
		return errors.New("no version milestones found")
	}

	byVersion := buildRegressionsByVersion(issues, versionOrder, unclassifiedClosed)
	closedCounts := buildClosedCounts(issues, versionOrder, milestoneOrder)
	openCounts := buildOpenCounts(issues, versionOrder, milestoneOrder)
	table := markdownTable(versionOrder, milestoneOrder, openCounts)
	svg := renderSVG(versionOrder, milestoneOrder, openCounts)

	// Persist all machine-readable and human-readable outputs with a shared version order.
	if err := writeJSON(filepath.Join(outDir, "regressions-by-version.json"), byVersion); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outDir, "regressions-closed-by-milestone.json"), closedCounts); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outDir, "regressions-open-by-milestone.json"), openCounts); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "regressions-open-by-version.md"), []byte(table), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "regressions-open-by-version.svg"), []byte(svg), 0o644); err != nil {
		return err
	}

	fmt.Printf("Wrote %s\n", filepath.Join(outDir, "regressions-by-version.json"))
	fmt.Printf("Wrote %s\n", filepath.Join(outDir, "regressions-closed-by-milestone.json"))
	fmt.Printf("Wrote %s\n", filepath.Join(outDir, "regressions-open-by-milestone.json"))
	fmt.Printf("Wrote %s\n", filepath.Join(outDir, "regressions-open-by-version.md"))
	fmt.Printf("Wrote %s\n", filepath.Join(outDir, "regressions-open-by-version.svg"))
	return nil
}

// loadRegressionIssues reads all git-bug issues from repoRoot and keeps only regression issues.
//
// The context is used for the `git bug` subprocess. repoRoot must contain a repository in
// which `git bug bug --format json` succeeds. The function returns normalized regression
// issues sorted by introduced version and GitHub issue number.
func loadRegressionIssues(ctx context.Context, repoRoot string) ([]regressionIssue, error) {
	cmd := exec.CommandContext(ctx, "git", "bug", "bug", "--format", "json")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run `git bug bug --format json`: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	var issues []gitBugIssue
	if err := json.Unmarshal(output, &issues); err != nil {
		return nil, err
	}

	result := make([]regressionIssue, 0, len(issues))
	for _, issue := range issues {
		// Keep every regression label for traceability, but aggregate under the earliest one.
		regressions := regressionVersions(issue.Labels)
		if len(regressions) == 0 {
			continue
		}
		url := issue.Metadata["github-url"]
		number, err := issueNumberFromURL(url)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub issue number for %q: %w", issue.Title, err)
		}
		result = append(result, regressionIssue{
			IssueNumber:       number,
			HumanID:           issue.HumanID,
			Title:             issue.Title,
			URL:               url,
			IntroducedVersion: regressions[0],
			RegressionLabels:  regressions,
			Status:            issue.Status,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].IntroducedVersion != result[j].IntroducedVersion {
			return compareVersion(result[i].IntroducedVersion, result[j].IntroducedVersion) < 0
		}
		return result[i].IssueNumber < result[j].IssueNumber
	})

	return result, nil
}

// regressionVersions extracts all distinct `regression in VER` versions from labels.
//
// labels may contain arbitrary non-regression labels. The function returns the matching
// versions in ascending semantic version order.
func regressionVersions(labels []string) []string {
	found := map[string]struct{}{}
	for _, label := range labels {
		match := regressionLabelPattern.FindStringSubmatch(label)
		if match == nil {
			continue
		}
		found[match[1]] = struct{}{}
	}
	return sortedVersions(found)
}

// issueNumberFromURL extracts a GitHub issue number from a GitHub issue URL.
//
// url must end in `/issues/<number>`. The function returns the parsed issue number.
func issueNumberFromURL(url string) (int, error) {
	match := githubIssueURLPattern.FindStringSubmatch(url)
	if match == nil {
		return 0, fmt.Errorf("not a GitHub issue URL: %q", url)
	}
	return strconv.Atoi(match[1])
}

// githubToken resolves a token for GitHub API access.
//
// The context is used for an optional `gh auth token` subprocess. Either GITHUB_TOKEN,
// GH_TOKEN, or a working `gh auth token` setup must be available. The function returns
// the resolved token string.
func githubToken(ctx context.Context) (string, error) {
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return token, nil
		}
	}
	if _, err := exec.LookPath("gh"); err == nil {
		cmd := exec.CommandContext(ctx, "gh", "auth", "token")
		output, err := cmd.Output()
		if err == nil {
			if token := strings.TrimSpace(string(output)); token != "" {
				return token, nil
			}
		}
	}
	return "", errors.New("missing GitHub token: set GITHUB_TOKEN or GH_TOKEN, or authenticate with `gh auth login`")
}

// fetchClosingMilestones queries GitHub for milestone titles of the given closed issues.
//
// ctx controls HTTP requests. owner, repo, and token must identify a readable GitHub
// repository. issueNumbers should contain GitHub issue numbers; duplicates are tolerated.
// The function returns a map from issue number to milestone title for closed issues that
// have a milestone set.
func fetchClosingMilestones(ctx context.Context, owner, repo, token string, issueNumbers []int) (map[int]string, error) {
	sortedNumbers := append([]int(nil), issueNumbers...)
	sort.Ints(sortedNumbers)

	client := &http.Client{Timeout: 30 * time.Second}
	result := make(map[int]string, len(sortedNumbers))

	for start := 0; start < len(sortedNumbers); start += 50 {
		// Batch queries to keep each GraphQL request compact while still avoiding one request
		// per issue.
		end := start + 50
		if end > len(sortedNumbers) {
			end = len(sortedNumbers)
		}
		query := milestoneQuery(owner, repo, sortedNumbers[start:end])
		payload, err := json.Marshal(graphQLRequest{Query: query})
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub GraphQL returned %s", resp.Status)
		}

		var decoded graphQLResponse
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return nil, err
		}
		if len(decoded.Errors) > 0 {
			return nil, fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
		}

		for alias, issue := range decoded.Data.Repository.Fields {
			number, err := strconv.Atoi(strings.TrimPrefix(alias, "i"))
			if err != nil {
				return nil, err
			}
			if issue.State != "CLOSED" {
				continue
			}
			if issue.Milestone == nil {
				continue
			}
			result[number] = strings.TrimSpace(issue.Milestone.Title)
		}
	}

	return result, nil
}

// milestoneQuery builds a GraphQL query that fetches milestone data for issueNumbers.
//
// owner and repo must name the target GitHub repository. issueNumbers must contain GitHub
// issue numbers. The function returns a complete GraphQL query string with aliased issue
// lookups.
func milestoneQuery(owner, repo string, issueNumbers []int) string {
	var builder strings.Builder
	builder.WriteString("query { repository(owner: ")
	builder.WriteString(strconv.Quote(owner))
	builder.WriteString(", name: ")
	builder.WriteString(strconv.Quote(repo))
	builder.WriteString(") { ")
	for _, issueNumber := range issueNumbers {
		builder.WriteString("i")
		builder.WriteString(strconv.Itoa(issueNumber))
		builder.WriteString(": issue(number: ")
		builder.WriteString(strconv.Itoa(issueNumber))
		builder.WriteString(") { number state milestone { title } } ")
	}
	builder.WriteString("} }")
	return builder.String()
}

// collectVersions derives the global version and milestone order from the loaded issues.
//
// issues must already contain IntroducedVersion and any available ClosingMilestone. The
// function returns the ordered version axis, the ordered milestone axis, and the list of
// closed regressions whose closing milestone was missing or not a version string.
func collectVersions(issues []regressionIssue) ([]string, []string, []regressionRecord) {
	versionSet := map[string]struct{}{}
	milestoneSet := map[string]struct{}{}
	var unclassifiedClosed []regressionRecord

	for _, issue := range issues {
		versionSet[issue.IntroducedVersion] = struct{}{}
		if issue.ClosingMilestone == "" {
			if issue.Status == "closed" {
				unclassifiedClosed = append(unclassifiedClosed, makeRegressionRecord(issue))
			}
			continue
		}
		if !versionPattern.MatchString(issue.ClosingMilestone) {
			unclassifiedClosed = append(unclassifiedClosed, makeRegressionRecord(issue))
			continue
		}
		milestoneSet[issue.ClosingMilestone] = struct{}{}
		versionSet[issue.ClosingMilestone] = struct{}{}
	}

	versionOrder := sortedVersions(versionSet)
	milestoneOrder := sortedVersions(versionSet)
	if len(milestoneSet) > 0 {
		// The milestone axis must also include versions that only appear as closing milestones.
		milestoneOrder = sortedVersions(union(versionSet, milestoneSet))
	}

	sort.Slice(unclassifiedClosed, func(i, j int) bool {
		return unclassifiedClosed[i].IssueNumber < unclassifiedClosed[j].IssueNumber
	})

	return versionOrder, milestoneOrder, unclassifiedClosed
}

// buildRegressionsByVersion groups issues by introduced version for JSON output.
//
// issues must already be normalized, and versionOrder should contain every introduced
// version that should appear in the output. The function returns the full
// regressions-by-version JSON payload.
func buildRegressionsByVersion(issues []regressionIssue, versionOrder []string, unclassifiedClosed []regressionRecord) regressionsByVersionFile {
	regressions := make(map[string][]regressionRecord, len(versionOrder))
	for _, version := range versionOrder {
		regressions[version] = nil
	}
	for _, issue := range issues {
		record := makeRegressionRecord(issue)
		regressions[issue.IntroducedVersion] = append(regressions[issue.IntroducedVersion], record)
	}
	for version := range regressions {
		sort.Slice(regressions[version], func(i, j int) bool {
			return regressions[version][i].IssueNumber < regressions[version][j].IssueNumber
		})
	}
	return regressionsByVersionFile{
		Versions:     versionOrder,
		Regressions:  regressions,
		Unclassified: unclassifiedClosed,
	}
}

// buildClosedCounts counts how many regressions each milestone closed per introduced version.
//
// issues must contain ClosingMilestone for closed issues when that milestone is usable.
// milestoneOrder and versionOrder define the output axes. The function returns the JSON
// payload for regressions-closed-by-milestone.json.
func buildClosedCounts(issues []regressionIssue, versionOrder, milestoneOrder []string) milestoneCountsFile {
	counts := make(map[string]map[string]int, len(milestoneOrder))
	for _, milestone := range milestoneOrder {
		counts[milestone] = make(map[string]int, len(versionOrder))
	}
	for _, issue := range issues {
		if issue.Status != "closed" || !versionPattern.MatchString(issue.ClosingMilestone) {
			continue
		}
		counts[issue.ClosingMilestone][issue.IntroducedVersion]++
	}
	return milestoneCountsFile{
		Milestones: milestoneOrder,
		Versions:   versionOrder,
		Counts:     counts,
	}
}

// buildOpenCounts counts how many regressions remain open at each milestone.
//
// issues must be normalized and milestoneOrder must be sorted ascending. Closed issues
// without a usable version milestone are excluded because they cannot be placed on the
// milestone timeline. The function returns the JSON payload for
// regressions-open-by-milestone.json.
func buildOpenCounts(issues []regressionIssue, versionOrder, milestoneOrder []string) milestoneCountsFile {
	counts := make(map[string]map[string]int, len(milestoneOrder))
	for _, milestone := range milestoneOrder {
		row := make(map[string]int)
		for _, version := range versionOrder {
			if compareVersion(version, milestone) > 0 {
				break
			}
			row[version] = 0
		}
		counts[milestone] = row
	}
	for _, milestone := range milestoneOrder {
		row := counts[milestone]
		for _, issue := range issues {
			// Skip closed issues whose milestone is not a usable version: they are surfaced in
			// the per-issue JSON, but they do not belong on the milestone timeline.
			if issue.Status == "closed" && !versionPattern.MatchString(issue.ClosingMilestone) {
				continue
			}
			if compareVersion(issue.IntroducedVersion, milestone) > 0 {
				continue
			}
			if issue.Status == "closed" && versionPattern.MatchString(issue.ClosingMilestone) && compareVersion(issue.ClosingMilestone, milestone) <= 0 {
				continue
			}
			row[issue.IntroducedVersion]++
		}
	}
	return milestoneCountsFile{
		Milestones: milestoneOrder,
		Versions:   versionOrder,
		Counts:     counts,
	}
}

// makeRegressionRecord converts an internal regressionIssue to its JSON form.
//
// issue must already be normalized. The function returns a JSON-ready record that preserves
// traceability fields such as the GitHub URL and all regression labels.
func makeRegressionRecord(issue regressionIssue) regressionRecord {
	return regressionRecord{
		IssueNumber:      issue.IssueNumber,
		HumanID:          issue.HumanID,
		Title:            issue.Title,
		Status:           issue.Status,
		URL:              issue.URL,
		RegressionLabels: issue.RegressionLabels,
		ClosingMilestone: issue.ClosingMilestone,
	}
}

// sortedVersions returns the keys of set sorted by semantic version order.
//
// All keys should be version strings accepted by compareVersion. The function returns a
// newly allocated slice.
func sortedVersions(set map[string]struct{}) []string {
	versions := make([]string, 0, len(set))
	for version := range set {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareVersion(versions[i], versions[j]) < 0
	})
	return versions
}

// union returns the set-theoretic union of a and b.
//
// The input maps are treated as sets. The function returns a newly allocated union set.
func union(a, b map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		result[key] = struct{}{}
	}
	for key := range b {
		result[key] = struct{}{}
	}
	return result
}

// compareVersion compares two dotted numeric version strings.
//
// Both inputs should be numeric version strings like `2.6.4.1`. Missing trailing
// components are treated as zero. The function returns -1, 0, or 1.
func compareVersion(left, right string) int {
	leftParts := parseVersion(left)
	rightParts := parseVersion(right)
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		leftValue := 0
		if i < len(leftParts) {
			leftValue = leftParts[i]
		}
		rightValue := 0
		if i < len(rightParts) {
			rightValue = rightParts[i]
		}
		switch {
		case leftValue < rightValue:
			return -1
		case leftValue > rightValue:
			return 1
		}
	}
	return 0
}

// parseVersion splits a dotted numeric version string into integers.
//
// version should be of the form `N(.N)*`. If parsing fails, the function returns the
// sentinel slice []int{0}.
func parseVersion(version string) []int {
	parts := strings.Split(version, ".")
	result := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return []int{0}
		}
		result = append(result, value)
	}
	return result
}

// markdownTable renders the open-count matrix as a markdown table.
//
// versionOrder and milestoneOrder must match the axes used in openCounts. The function
// returns the complete markdown document content.
func markdownTable(versionOrder, milestoneOrder []string, openCounts milestoneCountsFile) string {
	headers := append([]string{""}, versionOrder...)
	headers = append(headers, "Total")

	rows := make([][]string, 0, len(milestoneOrder)+2)
	rows = append(rows, headers)

	separator := make([]string, len(headers))
	for i := range separator {
		separator[i] = "---"
	}
	rows = append(rows, separator)

	for _, milestone := range milestoneOrder {
		row := []string{milestone}
		total := 0
		for _, version := range versionOrder {
			switch {
			case compareVersion(version, milestone) > 0:
				row = append(row, "")
			default:
				value := openCounts.Counts[milestone][version]
				total += value
				row = append(row, strconv.Itoa(value))
			}
		}
		row = append(row, strconv.Itoa(total))
		rows = append(rows, row)
	}

	widths := make([]int, len(headers))
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var builder strings.Builder
	builder.WriteString("Open regressions per version\n\n")
	// Pad cells so the raw markdown remains readable in plain text as well.
	for rowIndex, row := range rows {
		builder.WriteString("|")
		for i, cell := range row {
			builder.WriteString(" ")
			builder.WriteString(padCell(cell, widths[i], rowIndex > 1 && i > 0))
			builder.WriteString(" |")
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// padCell left- or right-aligns a markdown table cell to width characters.
//
// width must be at least len(value). When numeric is true and value is non-empty, the
// function right-aligns the cell; otherwise it left-aligns it.
func padCell(value string, width int, numeric bool) string {
	if numeric && value != "" {
		return strings.Repeat(" ", width-len(value)) + value
	}
	return value + strings.Repeat(" ", width-len(value))
}

// renderSVG renders the open-count matrix as a stacked SVG bar chart.
//
// versionOrder and milestoneOrder must match the axes used in openCounts. The function
// returns a self-contained SVG document string.
func renderSVG(versionOrder, milestoneOrder []string, openCounts milestoneCountsFile) string {
	totalByMilestone := make(map[string]int, len(milestoneOrder))
	maxTotal := 0
	for _, milestone := range milestoneOrder {
		total := 0
		for _, version := range versionOrder {
			total += openCounts.Counts[milestone][version]
		}
		totalByMilestone[milestone] = total
		if total > maxTotal {
			maxTotal = total
		}
	}
	if maxTotal == 0 {
		maxTotal = 1
	}

	const (
		topMargin    = 40
		leftMargin   = 70
		bottomMargin = 140
		plotHeight   = 420
	)

	legendColumns := 1
	if len(versionOrder) > 24 {
		legendColumns = int(math.Ceil(float64(len(versionOrder)) / 24.0))
	}
	legendColumnWidth := 130
	legendWidth := legendColumns*legendColumnWidth + 20
	plotWidth := maxInt(840, len(milestoneOrder)*56)
	width := leftMargin + plotWidth + legendWidth
	height := topMargin + plotHeight + bottomMargin
	plotBottom := topMargin + plotHeight
	plotRight := leftMargin + plotWidth

	slotWidth := float64(plotWidth) / float64(maxInt(1, len(milestoneOrder)))
	barWidth := slotWidth * 0.72
	scale := float64(plotHeight) / float64(maxTotal)
	tickStep := niceStep(maxTotal)
	palette := colorPalette(len(versionOrder))
	colorByVersion := make(map[string]string, len(versionOrder))
	for i, version := range versionOrder {
		colorByVersion[version] = palette[i]
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`, width, height, width, height))
	builder.WriteString("\n<style>")
	builder.WriteString("text{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;fill:#222} .axis{stroke:#444;stroke-width:1} .grid{stroke:#ddd;stroke-width:1} .bar{stroke:#fff;stroke-width:1} .tick{font-size:11px} .label{font-size:12px} .title{font-size:18px;font-weight:600}")
	builder.WriteString("</style>\n")
	builder.WriteString(fmt.Sprintf(`<text class="title" x="%d" y="26">Open regressions by introduced version</text>`, leftMargin))

	for tick := 0; tick <= maxTotal; tick += tickStep {
		y := plotBottom - int(math.Round(float64(tick)*scale))
		builder.WriteString(fmt.Sprintf(`<line class="grid" x1="%d" y1="%d" x2="%d" y2="%d"/>`, leftMargin, y, plotRight, y))
		builder.WriteString(fmt.Sprintf(`<text class="tick" x="%d" y="%d" text-anchor="end" dominant-baseline="middle">%d</text>`, leftMargin-8, y, tick))
		builder.WriteString("\n")
	}

	builder.WriteString(fmt.Sprintf(`<line class="axis" x1="%d" y1="%d" x2="%d" y2="%d"/>`, leftMargin, plotBottom, plotRight, plotBottom))
	builder.WriteString(fmt.Sprintf(`<line class="axis" x1="%d" y1="%d" x2="%d" y2="%d"/>`, leftMargin, topMargin, leftMargin, plotBottom))
	builder.WriteString("\n")

	// Draw each bar from bottom to top in version order so each segment keeps a stable color.
	for index, milestone := range milestoneOrder {
		centerX := float64(leftMargin) + slotWidth*(float64(index)+0.5)
		x := centerX - barWidth/2
		currentY := float64(plotBottom)
		for _, version := range versionOrder {
			if compareVersion(version, milestone) > 0 {
				break
			}
			value := openCounts.Counts[milestone][version]
			if value == 0 {
				continue
			}
			height := float64(value) * scale
			currentY -= height
			builder.WriteString(fmt.Sprintf(`<rect class="bar" x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/>`,
				x, currentY, barWidth, height, colorByVersion[version]))
			builder.WriteString("\n")
		}
		builder.WriteString(fmt.Sprintf(`<text class="tick" transform="translate(%.2f,%d) rotate(45)" text-anchor="start">%s</text>`,
			centerX-barWidth/2, plotBottom+18, milestone))
		builder.WriteString("\n")
	}

	builder.WriteString(fmt.Sprintf(`<text class="label" x="%d" y="%d" text-anchor="middle" transform="rotate(-90 %d %d)">Open regressions</text>`,
		22, topMargin+plotHeight/2, 22, topMargin+plotHeight/2))

	legendX := plotRight + 28
	legendTop := topMargin + 10
	for i, version := range versionOrder {
		column := i / 24
		row := i % 24
		x := legendX + column*legendColumnWidth
		y := legendTop + row*18
		builder.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="12" height="12" fill="%s" stroke="#fff"/>`, x, y-10, colorByVersion[version]))
		builder.WriteString(fmt.Sprintf(`<text class="label" x="%d" y="%d">%s</text>`, x+18, y, version))
		builder.WriteString("\n")
	}

	builder.WriteString("</svg>\n")
	return builder.String()
}

// niceStep chooses a readable y-axis tick step for values up to maxValue.
//
// maxValue should be positive. The function returns a step size from the sequence
// 1/2/5 * 10^k.
func niceStep(maxValue int) int {
	if maxValue <= 5 {
		return 1
	}
	targetTicks := 6.0
	raw := float64(maxValue) / targetTicks
	magnitude := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, candidate := range []float64{1, 2, 5, 10} {
		step := candidate * magnitude
		if raw <= step {
			return int(step)
		}
	}
	return int(10 * magnitude)
}

// colorPalette generates n visually distinct colors for the stacked chart.
//
// n may be zero. The function returns n hexadecimal RGB colors.
func colorPalette(n int) []string {
	colors := make([]string, n)
	for i := 0; i < n; i++ {
		h := math.Mod((float64(i) * 137.508), 360)
		colors[i] = hslToHex(h, 0.58, 0.56)
	}
	return colors
}

// hslToHex converts an HSL color to a CSS hexadecimal RGB string.
//
// h is interpreted in degrees, while s and l are expected in the range [0,1]. The
// function returns a string of the form `#rrggbb`.
func hslToHex(h, s, l float64) string {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2

	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}

	return fmt.Sprintf("#%02x%02x%02x",
		int(math.Round((r+m)*255)),
		int(math.Round((g+m)*255)),
		int(math.Round((b+m)*255)),
	)
}

// writeJSON writes value as pretty-printed JSON to path.
//
// path must be writable by the current process. The function creates or truncates the file
// and returns any write or encoding error.
func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// maxInt returns the larger of left and right.
//
// The function accepts any two integers and returns their maximum.
func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
