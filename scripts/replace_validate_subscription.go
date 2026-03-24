package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const replacement = `async function validateSubscription() {
  const repoPrivate = github.context?.payload?.repository?.private
  const upstream = 'SwiftyLab/setup-swift'
  const action = process.env.GITHUB_ACTION_REPOSITORY
  const docsUrl =
    'https://docs.stepsecurity.io/actions/stepsecurity-maintained-actions'

  info('')
  info('\u001b[1;36mStepSecurity Maintained Action\u001b[0m')
  info(` + "`" + `Secure drop-in replacement for ${upstream}` + "`" + `)
  if (repoPrivate === false)
    info('\u001b[32m\u2713 Free for public repositories\u001b[0m')
  info(` + "`" + `\u001b[36mLearn more:\u001b[0m ${docsUrl}` + "`" + `)
  info('')

  if (repoPrivate === false) return

  const serverUrl = process.env.GITHUB_SERVER_URL || 'https://github.com'
  const body: Record<string, string> = {action: action || ''}
  if (serverUrl !== 'https://github.com') body.ghes_server = serverUrl
  try {
    await axios.post(
      ` + "`" + `https://agent.api.stepsecurity.io/v1/github/${process.env.GITHUB_REPOSITORY}/actions/maintained-actions-subscription` + "`" + `,
      body,
      {timeout: 3000}
    )
  } catch (error) {
    if (isAxiosError(error) && error.response?.status === 403) {
      err(
        ` + "`" + `\u001b[1;31mThis action requires a StepSecurity subscription for private repositories.\u001b[0m` + "`" + `
      )
      err(
        ` + "`" + `\u001b[31mLearn how to enable a subscription: ${docsUrl}\u001b[0m` + "`" + `
      )
      process.exit(1)
    }
    info('Timeout or API not reachable. Continuing to next step.')
  }
}`

var signatureRE = regexp.MustCompile(`async\s+function\s+validateSubscription\s*\([^)]*\)\s*(?::\s*Promise<[^>]+>)?\s*\{`)

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get working directory: %v\n", err)
		os.Exit(1)
	}

	var updatedFile string

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		name := d.Name()

		if d.IsDir() {
			switch name {
			case ".git", "node_modules", "dist", "coverage", "vendor":
				return filepath.SkipDir
			}
			return nil
		}

		if !isCandidateFile(path) {
			return nil
		}

		changed, err := replaceInFile(path)
		if err != nil {
			return err
		}
		if changed {
			updatedFile = path
			return fs.SkipAll
		}
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "error while processing files: %v\n", err)
		os.Exit(1)
	}

	if updatedFile == "" {
		fmt.Fprintln(os.Stderr, "could not find async function validateSubscription() in any .ts or .js file outside dist/")
		os.Exit(1)
	}

	fmt.Printf("updated: %s\n", updatedFile)
	fmt.Println("replacement completed successfully")
}

func isCandidateFile(path string) bool {
	if strings.HasSuffix(path, ".d.ts") {
		return false
	}
	return strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".js")
}

func replaceInFile(path string) (bool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	start, end, found := findFunctionRange(contents)
	if !found {
		return false, nil
	}

	var out bytes.Buffer
	out.Write(contents[:start])
	out.WriteString(replacement)
	out.Write(contents[end:])

	if bytes.Equal(out.Bytes(), contents) {
		return false, nil
	}

	if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
		return false, err
	}

	return true, nil
}

func findFunctionRange(content []byte) (int, int, bool) {
	loc := signatureRE.FindIndex(content)
	if loc == nil {
		return 0, 0, false
	}

	start := loc[0]
	openBrace := bytes.IndexByte(content[loc[0]:loc[1]], '{')
	if openBrace == -1 {
		return 0, 0, false
	}
	openBrace += loc[0]

	depth := 0
	inSingle := false
	inDouble := false
	inTemplate := false
	inLineComment := false
	inBlockComment := false

	for i := openBrace; i < len(content); i++ {
		ch := content[i]
		var next byte
		if i+1 < len(content) {
			next = content[i+1]
		}

		if inLineComment {
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			if ch == '*' && next == '/' {
				inBlockComment = false
				i++
			}
			continue
		}

		if inSingle {
			if ch == '\\' {
				i++
				continue
			}
			if ch == '\'' {
				inSingle = false
			}
			continue
		}

		if inDouble {
			if ch == '\\' {
				i++
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}

		if inTemplate {
			if ch == '\\' {
				i++
				continue
			}
			if ch == '`' {
				inTemplate = false
			}
			continue
		}

		if ch == '/' && next == '/' {
			inLineComment = true
			i++
			continue
		}

		if ch == '/' && next == '*' {
			inBlockComment = true
			i++
			continue
		}

		if ch == '\'' {
			inSingle = true
			continue
		}

		if ch == '"' {
			inDouble = true
			continue
		}

		if ch == '`' {
			inTemplate = true
			continue
		}

		if ch == '{' {
			depth++
			continue
		}

		if ch == '}' {
			depth--
			if depth == 0 {
				return start, i + 1, true
			}
		}
	}

	return 0, 0, false
}