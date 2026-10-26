package taskfile

import (
	"context"
	"slices"
	"strings"
	"time"

	giturls "github.com/chainguard-dev/git-urls"

	"github.com/go-task/task/v3/internal/fsext"
)

type Node interface {
	Read() ([]byte, error)
	Parent() Node
	Location() string
	Dir() string
	Checksum() string
	Verify(checksum string) bool
	ResolveEntrypoint(entrypoint string) (string, error)
	ResolveDir(dir string) (string, error)
}

// Source identifies where a taskfile node reads its content from. It is used
// by the run result summary to report the origin of the Taskfile.
type Source string

const (
	// SourceFile is a taskfile read from the local filesystem.
	SourceFile Source = "file"
	// SourceHTTP is a taskfile downloaded over HTTP(S).
	SourceHTTP Source = "http"
	// SourceGit is a taskfile read from a Git repository.
	SourceGit Source = "git"
	// SourceStdin is a taskfile read from the standard input.
	SourceStdin Source = "stdin"
	// SourceCache is a remote taskfile served from the local remote cache.
	SourceCache Source = "cache"
)

// SourceOf reports the origin of the given node. Unknown node types (and
// local file nodes) are reported as [SourceFile].
func SourceOf(node Node) Source {
	switch node.(type) {
	case *HTTPNode:
		return SourceHTTP
	case *GitNode:
		return SourceGit
	case *StdinNode:
		return SourceStdin
	default:
		return SourceFile
	}
}

type RemoteNode interface {
	Node
	ReadContext(ctx context.Context) ([]byte, error)
	CacheKey() string
}

func NewRootNode(
	entrypoint string,
	dir string,
	insecure bool,
	timeout time.Duration,
	opts ...NodeOption,
) (Node, error) {
	dir = fsext.DefaultDir(entrypoint, dir)
	// If the entrypoint is "-", we read from stdin
	if entrypoint == "-" {
		return NewStdinNode(dir)
	}
	return NewNode(entrypoint, dir, insecure, opts...)
}

func NewNode(
	entrypoint string,
	dir string,
	insecure bool,
	opts ...NodeOption,
) (Node, error) {
	var node Node
	var err error

	scheme, err := getScheme(entrypoint)
	if err != nil {
		return nil, err
	}

	switch scheme {
	case "git":
		node, err = NewGitNode(entrypoint, dir, insecure, opts...)
	case "http", "https":
		node, err = NewHTTPNode(entrypoint, dir, insecure, opts...)
	default:
		node, err = NewFileNode(entrypoint, dir, opts...)
	}

	return node, err
}

func IsRemoteEntrypoint(entrypoint string) bool {
	scheme, _ := getScheme(entrypoint)
	switch scheme {
	case "git", "http", "https":
		return true
	default:
		return false
	}
}

func getScheme(uri string) (string, error) {
	u, err := giturls.Parse(uri)
	if u == nil {
		return "", err
	}

	isDotGit := strings.HasSuffix(strings.Split(u.Path, "//")[0], ".git")
	isUnderscoreGit := strings.Contains(strings.Split(u.Path, "//")[0], "/_git/")
	schemeIsGitCompatible := slices.Contains([]string{"git", "ssh", "https", "http"}, u.Scheme)
	if (isDotGit || isUnderscoreGit) && schemeIsGitCompatible {
		return "git", nil
	}

	if before, _, ok := strings.Cut(uri, "://"); ok {
		return before, nil
	}

	return "", nil
}
