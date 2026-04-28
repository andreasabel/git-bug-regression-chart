# regression-chart

`regression-chart` extracts regression information from the local `git-bug` bridge data in `.git/git-bug`, augments closed issues with their GitHub milestone, and writes machine-readable summaries plus human-readable reports.

## How it works

1. It runs `git bug bug --format json` in the target repository and selects issues labeled `regression in VER`.
2. It reads the bridged GitHub issue URL for each selected issue and extracts the GitHub issue number.
3. For closed issues, it queries the GitHub GraphQL API for the closing milestone.
4. It computes:
   - regressions grouped by introduced version
   - how many regressions each milestone closes, grouped by introduced version
   - how many regressions are still open at each milestone, grouped by introduced version
5. It renders the aggregated data as JSON, a markdown table, and an SVG stacked bar chart.

## Running the tool

From the repository root:

```bash
cd src/release-tools/regression-chart
go run . -repo-root /Users/abela/agda -out-dir /tmp/regression-chart
```

Or, when already in the repository root:

```bash
go run ./src/release-tools/regression-chart -repo-root . -out-dir ./tmp/regression-chart
```

## Parameters

The tool accepts the following flags:

| Flag         | Default | Description                                                                                  |
|--------------|---------|----------------------------------------------------------------------------------------------|
| `-repo-root` | `.`     | Path to the Git repository root that contains `.git/git-bug`. The tool runs `git bug` there. |
| `-out-dir`   | `.`     | Directory in which all generated files are written. The directory is created if needed.      |
| `-owner`     | `agda`  | GitHub owner used for milestone lookups.                                                     |
| `-repo`      | `agda`  | GitHub repository name used for milestone lookups.                                           |

## Authentication

Yes. GitHub authentication is needed for milestone lookup on closed issues.

The tool looks for authentication in this order:

1. `GITHUB_TOKEN`
2. `GH_TOKEN`
3. `gh auth token`

So either set a token environment variable or make sure `gh auth login` has already been run.

The local `git-bug` data itself does not need extra authentication, but the repository must have `git bug` installed and usable.

## Output

The tool writes five files into `-out-dir`:

| File                                   | Description                                                                                                                                                                                                                                        |
|----------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `regressions-by-version.json`          | For each introduced version, the list of regression issues. Closed issues include their closing milestone when it is a version milestone. If a closed issue has no usable version milestone, it is listed under `unclassified_closed_regressions`. |
| `regressions-closed-by-milestone.json` | For each milestone, a map from introduced version to the number of regressions closed in that milestone.                                                                                                                                           |
| `regressions-open-by-milestone.json`   | For each milestone, a map from introduced version to the number of regressions still open at that milestone. The milestone itself is included as the last possible introduced version in each row.                                                 |
| `regressions-open-by-version.md`       | A markdown table of the open-regression matrix.                                                                                                                                                                                                    |
| `regressions-open-by-version.svg`      | A stacked bar chart of open regressions per milestone, colored by introduced version.                                                                                                                                                              |

## Notes about the data

- Some issues in the bridged data currently carry more than one `regression in VER` label. The tool records all such labels in JSON and uses the earliest listed version as the introduced version for counting.
- Some closed issues may lack a usable version milestone on GitHub. Those issues are reported in `regressions-by-version.json` but excluded from milestone-based aggregates.
