package main

import (
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kballard/go-shellquote"
	"github.com/mattn/go-isatty"
	"github.com/pkg/browser"
)

func isFlagSet(fs *flag.FlagSet, name string) bool {
	var found bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func init() {
	usage := `
Examples:

  Upload an LSIF dump with explicit repo, commit, and upload files:

    	$ src lsif upload -repo=FOO -commit=BAR -file=dump.lsif

  Upload an LSIF dump for a subproject:

    	$ src lsif upload -root=cmd/

  Upload an LSIF dump when lsifEnforceAuth is enabled:

    	$ src lsif upload -github-token=BAZ

  Upload an LSIF dump when the LSIF indexer does not not declare a tool name.

    	$ src lsif upload -indexerName=lsif-elixir
`

	flagSet := flag.NewFlagSet("upload", flag.ExitOnError)
	usageFunc := func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of 'src lsif %s':\n", flagSet.Name())
		flagSet.PrintDefaults()
		fmt.Println(usage)
	}
	var (
		repoFlag        = flagSet.String("repo", "", `The name of the repository (e.g. github.com/gorilla/mux). By default, derived from the origin remote.`)
		commitFlag      = flagSet.String("commit", "", `The 40-character hash of the commit. Defaults to the currently checked-out commit.`)
		fileFlag        = flagSet.String("file", "./dump.lsif", `The path to the LSIF dump file.`)
		githubTokenFlag = flagSet.String("github-token", "", `A GitHub access token with 'public_repo' scope that Sourcegraph uses to verify you have access to the repository.`)
		rootFlag        = flagSet.String("root", "", `The path in the repository that matches the LSIF projectRoot (e.g. cmd/project1). Defaults to the directory where the dump file is located.`)
		indexerNameFlag = flagSet.String("indexerName", "", `The name of the indexer that generated the dump. This will override the 'toolInfo.name' field in the metadata vertex of the LSIF dump file. This must be supplied if the indexer does not set this field (in which case the upload will fail with an explicit message).`)
		openFlag        = flagSet.Bool("open", false, `Open the LSIF upload page in your browser.`)
		apiFlags        = newAPIFlags(flagSet)
	)

	handler := func(args []string) error {
		flagSet.Parse(args)

		if repoFlag == nil || *repoFlag == "" {
			remoteURL, err := exec.Command("git", "remote", "get-url", "origin").Output()
			if err != nil {
				fmt.Printf("Failed to invoke git: %v\n", err)
				fmt.Println("Unable to detect repository from environment.")
				fmt.Println("Either cd into a git repository or set -repo explicitly.")
				os.Exit(1)
			}
			*repoFlag, err = parseRemoteURL(strings.TrimSpace(string(remoteURL)))
			if err != nil {
				fmt.Println(err)
				fmt.Println("Set -repo explicitly.")
				os.Exit(1)
			}
		}
		fmt.Println("Repository: " + *repoFlag)

		if commitFlag == nil || *commitFlag == "" {
			commit, err := exec.Command("git", "rev-parse", "HEAD").Output()
			if err != nil {
				fmt.Printf("Failed to invoke git: %v\n", err)
				fmt.Println("Unable to detect commit from environment.")
				fmt.Println("Either cd into a git repository or set -commit explicitly.")
				os.Exit(1)
			}
			*commitFlag = strings.TrimSpace(string(commit))
		}
		fmt.Println("Commit: " + *commitFlag)

		if _, err := os.Stat(*fileFlag); os.IsNotExist(err) {
			fmt.Println("File does not exist: " + *fileFlag)
			fmt.Println("Either cd to the directory where it was generated or set -file explicitly.")
			os.Exit(1)
		}
		fmt.Println("File: " + *fileFlag)

		if !isFlagSet(flagSet, "root") {
			checkError := func(err error) {
				if err != nil {
					fmt.Println(err)
					fmt.Println("Unable to detect root of LSIF dump from environment.")
					fmt.Println("Either cd into a git repository or set -root explicitly.")
					os.Exit(1)
				}
			}

			topLevel, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
			checkError(err)

			absFile, err := filepath.Abs(*fileFlag)
			checkError(err)

			rel, err := filepath.Rel(strings.TrimSpace(string(topLevel)), absFile)
			checkError(err)

			relDir := filepath.Dir(rel)
			rootFlag = &relDir
		}

		*rootFlag = filepath.Clean(*rootFlag)
		if strings.HasPrefix(*rootFlag, "..") {
			fmt.Println("-root is outside the repository: " + *rootFlag)
			os.Exit(1)
		}
		if *rootFlag == "." || *rootFlag == "/" {
			*rootFlag = ""
		}
		fmt.Println("Root: " + *rootFlag)

		// First, build the URL which is used to both make the request
		// and to emit a cURL command. This is a little different than
		// the rest of the commands as it does not use a GraphQL endpoint,
		// using the path and query string instead of the body.

		qs := url.Values{}
		qs.Add("repository", *repoFlag)
		qs.Add("commit", *commitFlag)
		if *githubTokenFlag != "" {
			qs.Add("github_token", *githubTokenFlag)
		}
		if *rootFlag != "" {
			qs.Add("root", *rootFlag)
		}
		if *indexerNameFlag != "" {
			qs.Add("indexerName", *indexerNameFlag)
		}

		url, err := url.Parse(cfg.Endpoint + "/.api/lsif/upload")
		if err != nil {
			return err
		}
		url.RawQuery = qs.Encode()

		// Emit a cURL command. This is also a bit different than the rest
		// of the commands as it uploads a file rather than just sending a
		// JSON-encoded body.
		//
		// Because we compress the body before sending it to the API below,
		// we need to pipe the output of gzip into the cURL command.

		if *apiFlags.getCurl {
			curl := fmt.Sprintf("gzip -c %s | curl \\\n", shellquote.Join(*fileFlag))
			curl += fmt.Sprintf("   -X POST \\\n")
			curl += fmt.Sprintf("   %s \\\n", shellquote.Join("-H", "Content-Type: application/x-ndjson+lsif"))
			if cfg.AccessToken != "" {
				curl += fmt.Sprintf("   %s \\\n", shellquote.Join("-H", "Authorization: token "+cfg.AccessToken))
			}
			curl += fmt.Sprintf("   %s \\\n", shellquote.Join(url.String()))
			curl += fmt.Sprintf("   %s", shellquote.Join("--data-binary", "@-"))

			fmt.Println(curl)
			return nil
		}

		f, err := os.Open(*fileFlag)
		if err != nil {
			return err
		}
		defer f.Close()

		// compress the file
		pr, ch := gzipReader(f)

		// Create the HTTP request.
		req, err := http.NewRequest("POST", url.String(), pr)
		if err != nil {
			return err
		}

		req.Header.Set("Content-Type", "application/x-ndjson+lsif")
		if cfg.AccessToken != "" {
			req.Header.Set("Authorization", "token "+cfg.AccessToken)
		}

		// Perform the request.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		// See if we had a reader error
		if err := <-ch; err != nil {
			return err
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		// Our request may have failed before the reaching the upload endpoint, so
		// confirm the status code. You can test this easily with e.g. an invalid
		// endpoint like -endpoint=https://google.com
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusUnauthorized && strings.Contains(strings.ToLower(string(body)), "must provide github_token") {
				return fmt.Errorf("error: you must provide -github-token=TOKEN, where TOKEN is a GitHub personal access token with 'repo' or 'public_repo' scope")
			}

			if resp.StatusCode == http.StatusUnauthorized && isatty.IsTerminal(os.Stdout.Fd()) {
				fmt.Println("You may need to specify or update your GitHub access token to use this endpoint.")
				fmt.Println("See https://github.com/sourcegraph/src-cli#authentication")
				fmt.Println("")
			}
			return fmt.Errorf("error: %s\n\n%s", resp.Status, body)
		}

		payload := struct {
			ID string `json:"id"`
		}{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return err
		}

		uploadID := string(base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf(`LSIFUpload:"%s"`, payload.ID))))
		uploadURL := fmt.Sprintf("%s/%s/-/settings/code-intelligence/lsif-uploads/%s", cfg.Endpoint, *repoFlag, uploadID)

		fmt.Println("")
		fmt.Printf("LSIF dump successfully uploaded for processing.\n")
		fmt.Printf("View processing status at %s.\n", uploadURL)

		if *openFlag {
			if err := browser.OpenURL(uploadURL); err != nil {
				return err
			}
		}

		return nil
	}

	// Register the command.
	lsifCommands = append(lsifCommands, &command{
		flagSet:   flagSet,
		handler:   handler,
		usageFunc: usageFunc,
	})
}

func gzipReader(r io.Reader) (io.Reader, <-chan error) {
	ch := make(chan error)
	br := bufio.NewReader(r)
	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)

	go func() {
		defer close(ch)
		defer pw.Close() // must be closed 2nd
		defer gw.Close() // must be closed 1st

		if _, err := br.WriteTo(gw); err != nil {
			ch <- err
		}
	}()

	return pr, ch
}

// parseRemoteURL takes remote URLs such as:
//
// git@github.com:gorilla/mux.git
// https://github.com/gorilla/mux.git
//
// and returns:
//
// github.com/gorilla/mux
func parseRemoteURL(urlString string) (string, error) {
	if strings.HasPrefix(urlString, "git@") {
		parts := strings.Split(urlString, ":")
		if len(parts) != 2 {
			return "", fmt.Errorf("unrecognized remote URL: %s", urlString)
		}
		return strings.TrimPrefix(parts[0], "git@") + "/" + strings.TrimPrefix(strings.TrimSuffix(parts[1], ".git"), "/"), nil
	}

	remoteURL, err := url.Parse(urlString)
	if err != nil {
		return "", fmt.Errorf("unrecognized remote URL: %s", urlString)
	}
	return remoteURL.Hostname() + strings.TrimSuffix(remoteURL.Path, ".git"), nil
}
