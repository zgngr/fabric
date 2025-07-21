package changelog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielmiessler/fabric/cmd/generate_changelog/internal/git"
)

// ProcessIncomingPR processes a single PR for changelog entry creation
func (g *Generator) ProcessIncomingPR(prNumber int) error {
	if err := g.validatePRState(prNumber); err != nil {
		return fmt.Errorf("PR validation failed: %w", err)
	}

	if err := g.validateGitStatus(); err != nil {
		return fmt.Errorf("git status validation failed: %w", err)
	}

	// Now fetch the full PR with commits for content generation
	pr, err := g.ghClient.GetPRWithCommits(prNumber)
	if err != nil {
		return fmt.Errorf("failed to fetch PR %d: %w", prNumber, err)
	}

	content := g.formatPR(pr)

	if g.cfg.EnableAISummary {
		aiContent, err := SummarizeVersionContent(content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: AI summarization failed: %v\n", err)
		} else if !checkForAIError(aiContent) {
			content = strings.TrimSpace(aiContent)
		}
	}

	if err := g.ensureIncomingDir(); err != nil {
		return fmt.Errorf("failed to create incoming directory: %w", err)
	}

	filename := filepath.Join(g.cfg.IncomingDir, fmt.Sprintf("%d.txt", prNumber))

	// Ensure content ends with a single newline
	content = strings.TrimSpace(content) + "\n"

	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write incoming file: %w", err)
	}

	if err := g.commitAndPushIncoming(prNumber, filename); err != nil {
		return fmt.Errorf("failed to commit and push: %w", err)
	}

	fmt.Printf("Successfully created incoming changelog entry: %s\n", filename)
	return nil
}

// CreateNewChangelogEntry aggregates all incoming PR files for release and includes direct commits
func (g *Generator) CreateNewChangelogEntry(version string) error {
	files, err := filepath.Glob(filepath.Join(g.cfg.IncomingDir, "*.txt"))
	if err != nil {
		return fmt.Errorf("failed to scan incoming directory: %w", err)
	}

	var content strings.Builder
	var processingErrors []string

	// First, aggregate all incoming PR files
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			processingErrors = append(processingErrors, fmt.Sprintf("failed to read %s: %v", file, err))
			continue // Continue to attempt processing other files
		}
		content.WriteString(string(data))
		content.WriteString("\n")
	}

	if len(processingErrors) > 0 {
		return fmt.Errorf("encountered errors while processing incoming files: %s", strings.Join(processingErrors, "; "))
	}

	// Extract PR numbers and their commit SHAs from processed files to avoid including their commits as "direct"
	processedPRs := make(map[int]bool)
	processedCommitSHAs := make(map[string]bool)

	for _, file := range files {
		// Extract PR number from filename (e.g., "1640.txt" -> 1640)
		filename := filepath.Base(file)
		if prNumStr := strings.TrimSuffix(filename, ".txt"); prNumStr != filename {
			if prNum, err := strconv.Atoi(prNumStr); err == nil {
				processedPRs[prNum] = true

				// Fetch the PR to get its commit SHAs
				if pr, err := g.ghClient.GetPRWithCommits(prNum); err == nil {
					for _, commit := range pr.Commits {
						processedCommitSHAs[commit.SHA] = true
					}
				}
			}
		}
	}

	// Now add direct commits since the last release, excluding commits from processed PRs
	directCommitsContent, err := g.getDirectCommitsSinceLastRelease(processedPRs, processedCommitSHAs)
	if err != nil {
		return fmt.Errorf("failed to get direct commits since last release: %w", err)
	}
	content.WriteString(directCommitsContent)

	// Check if we have any content at all
	if content.Len() == 0 {
		if len(files) == 0 {
			fmt.Fprintf(os.Stderr, "No incoming PR files found in %s and no direct commits since last release\n", g.cfg.IncomingDir)
		} else {
			fmt.Fprintf(os.Stderr, "No content found in incoming files and no direct commits since last release\n")
		}
		return nil
	}

	entry := fmt.Sprintf("## %s (%s)\n\n%s",
		version, time.Now().Format("2006-01-02"), strings.TrimLeft(content.String(), "\n"))

	if err := g.insertVersionAtTop(entry); err != nil {
		return fmt.Errorf("failed to update CHANGELOG.md: %w", err)
	}

	if g.cache != nil {
		// Create a proper new version entry for the database
		newVersionEntry := &git.Version{
			Name:      version,
			Date:      time.Now(),
			CommitSHA: "",      // Will be set when the release commit is made
			PRNumbers: []int{}, // Could be extracted from incoming files if needed
			AISummary: content.String(),
		}

		if err := g.cache.SaveVersion(newVersionEntry); err != nil {
			return fmt.Errorf("failed to save new version entry to database: %w", err)
		}
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to remove %s: %v\n", file, err)
		}
	}

	if err := g.stageChangesForRelease(); err != nil {
		return fmt.Errorf("critical: failed to stage changes for release: %w", err)
	}

	fmt.Printf("Successfully processed %d incoming PR files for version %s\n", len(files), version)
	return nil
}

// getDirectCommitsSinceLastRelease gets all direct commits (not part of PRs) since the last release
func (g *Generator) getDirectCommitsSinceLastRelease(processedPRs map[int]bool, processedCommitSHAs map[string]bool) (string, error) {
	// Get the latest tag to determine what commits are unreleased
	latestTag, err := g.gitWalker.GetLatestTag()
	if err != nil {
		return "", fmt.Errorf("failed to get latest tag: %w", err)
	}

	// Get all commits since the latest tag
	unreleasedVersion, err := g.gitWalker.WalkCommitsSinceTag(latestTag)
	if err != nil {
		return "", fmt.Errorf("failed to walk commits since tag %s: %w", latestTag, err)
	}

	if unreleasedVersion == nil || len(unreleasedVersion.Commits) == 0 {
		return "", nil // No unreleased commits
	}

	// Filter out commits that are part of PRs (we already have those from incoming files)
	// and format the direct commits
	var directCommits []*git.Commit
	for _, commit := range unreleasedVersion.Commits {
		// Skip version bump commits
		if commit.IsVersion {
			continue
		}

		// Skip commits that belong to PRs we've already processed from incoming files (by PR number)
		if commit.PRNumber > 0 && processedPRs[commit.PRNumber] {
			continue
		}

		// Skip commits whose SHA is already included in processed PRs (this catches commits
		// that might not have been detected as part of a PR but are actually in the PR)
		if processedCommitSHAs[commit.SHA] {
			continue
		}

		// Only include commits that are NOT part of any PR (direct commits)
		if commit.PRNumber == 0 {
			directCommits = append(directCommits, commit)
		}
	}

	if len(directCommits) == 0 {
		return "", nil // No direct commits
	}

	// Format the direct commits similar to how it's done in generateRawVersionContent
	var sb strings.Builder
	sb.WriteString("### Direct commits\n\n")

	// Sort direct commits by date (newest first) for consistent ordering
	sort.Slice(directCommits, func(i, j int) bool {
		return directCommits[i].Date.After(directCommits[j].Date)
	})

	for _, commit := range directCommits {
		message := g.formatCommitMessage(strings.TrimSpace(commit.Message))
		if message != "" && !g.isDuplicateMessage(message, directCommits) {
			sb.WriteString(fmt.Sprintf("- %s\n", message))
		}
	}

	return sb.String(), nil
}

// validatePRState validates that a PR is in the correct state for processing
func (g *Generator) validatePRState(prNumber int) error {
	// Use lightweight validation call that doesn't fetch commits
	details, err := g.ghClient.GetPRValidationDetails(prNumber)
	if err != nil {
		return fmt.Errorf("failed to fetch PR %d: %w", prNumber, err)
	}

	if details.State != "open" {
		return fmt.Errorf("PR %d is not open (current state: %s)", prNumber, details.State)
	}

	if !details.Mergeable {
		return fmt.Errorf("PR %d is not mergeable - please resolve conflicts first", prNumber)
	}

	return nil
}

// validateGitStatus ensures the working directory is clean
func (g *Generator) validateGitStatus() error {
	isClean, err := g.gitWalker.IsWorkingDirectoryClean()
	if err != nil {
		return fmt.Errorf("failed to check git status: %w", err)
	}

	if !isClean {
		// Get detailed status for better error message
		statusDetails, statusErr := g.gitWalker.GetStatusDetails()
		if statusErr == nil && statusDetails != "" {
			return fmt.Errorf("working directory is not clean - please commit or stash changes before proceeding:\n%s", statusDetails)
		}
		return fmt.Errorf("working directory is not clean - please commit or stash changes before proceeding")
	}

	return nil
}

// ensureIncomingDir creates the incoming directory if it doesn't exist
func (g *Generator) ensureIncomingDir() error {
	if err := os.MkdirAll(g.cfg.IncomingDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", g.cfg.IncomingDir, err)
	}
	return nil
}

// commitAndPushIncoming commits and optionally pushes the incoming changelog file
func (g *Generator) commitAndPushIncoming(prNumber int, filename string) error {
	relativeFilename, err := filepath.Rel(g.cfg.RepoPath, filename)
	if err != nil {
		relativeFilename = filename
	}

	// Add file to git index
	if err := g.gitWalker.AddFile(relativeFilename); err != nil {
		return fmt.Errorf("failed to add file %s: %w", relativeFilename, err)
	}

	// Commit changes
	commitMessage := fmt.Sprintf("chore: incoming %d changelog entry", prNumber)
	_, err = g.gitWalker.CommitChanges(commitMessage)
	if err != nil {
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	// Push to remote if enabled
	if g.cfg.Push {
		if err := g.gitWalker.PushToRemote(); err != nil {
			return fmt.Errorf("failed to push to remote: %w", err)
		}
	} else {
		fmt.Println("Commit created successfully. Please review and push manually.")
	}

	return nil
}

// detectVersion detects the current version from version.nix or git tags
func (g *Generator) detectVersion() (string, error) {
	versionNixPath := filepath.Join(g.cfg.RepoPath, "version.nix")
	if _, err := os.Stat(versionNixPath); err == nil {
		data, err := os.ReadFile(versionNixPath)
		if err != nil {
			return "", fmt.Errorf("failed to read version.nix: %w", err)
		}

		versionRegex := regexp.MustCompile(`"([^"]+)"`)
		matches := versionRegex.FindStringSubmatch(string(data))
		if len(matches) > 1 {
			return matches[1], nil
		}
	}

	latestTag, err := g.gitWalker.GetLatestTag()
	if err != nil {
		return "", fmt.Errorf("failed to get latest tag: %w", err)
	}

	if latestTag == "" {
		return "v1.0.0", nil
	}

	return latestTag, nil
}

// insertVersionAtTop inserts a new version entry at the top of CHANGELOG.md
func (g *Generator) insertVersionAtTop(entry string) error {
	changelogPath := filepath.Join(g.cfg.RepoPath, "CHANGELOG.md")
	header := "# Changelog"
	headerRegex := regexp.MustCompile(`(?m)^# Changelog\s*`)

	existingContent, err := os.ReadFile(changelogPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read existing CHANGELOG.md: %w", err)
		}
		// File doesn't exist, create it.
		newContent := fmt.Sprintf("%s\n\n%s\n", header, entry)
		return os.WriteFile(changelogPath, []byte(newContent), 0644)
	}

	contentStr := string(existingContent)
	var newContent string

	if loc := headerRegex.FindStringIndex(contentStr); loc != nil {
		// Found the header, insert after it.
		insertionPoint := loc[1]
		// Skip any existing newlines after the header to avoid double spacing
		for insertionPoint < len(contentStr) && (contentStr[insertionPoint] == '\n' || contentStr[insertionPoint] == '\r') {
			insertionPoint++
		}
		// Insert with proper spacing: single newline after header, then entry, then newline before existing content
		newContent = contentStr[:loc[1]] + "\n\n" + entry + "\n\n" + contentStr[insertionPoint:]
	} else {
		// Header not found, prepend everything.
		newContent = fmt.Sprintf("%s\n\n%s\n\n%s", header, entry, contentStr)
	}

	return os.WriteFile(changelogPath, []byte(newContent), 0644)
}

// stageChangesForRelease stages the modified files for the release commit
func (g *Generator) stageChangesForRelease() error {
	changelogPath := filepath.Join(g.cfg.RepoPath, "CHANGELOG.md")
	relativeChangelog, err := filepath.Rel(g.cfg.RepoPath, changelogPath)
	if err != nil {
		relativeChangelog = "CHANGELOG.md"
	}

	relativeCacheFile, err := filepath.Rel(g.cfg.RepoPath, g.cfg.CacheFile)
	if err != nil {
		relativeCacheFile = g.cfg.CacheFile
	}

	// Add CHANGELOG.md to git index
	if err := g.gitWalker.AddFile(relativeChangelog); err != nil {
		return fmt.Errorf("failed to add %s: %w", relativeChangelog, err)
	}

	// Add cache file to git index
	if err := g.gitWalker.AddFile(relativeCacheFile); err != nil {
		return fmt.Errorf("failed to add %s: %w", relativeCacheFile, err)
	}

	// Remove incoming directory if it exists
	if g.cfg.IncomingDir != "" {
		relativeIncomingDir, err := filepath.Rel(g.cfg.RepoPath, g.cfg.IncomingDir)
		if err != nil {
			relativeIncomingDir = g.cfg.IncomingDir
		}

		if err := g.gitWalker.RemoveFile(relativeIncomingDir); err != nil {
			return fmt.Errorf("failed to remove incoming directory %s: %w", relativeIncomingDir, err)
		}
	}

	return nil
}
