package main

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/chroma/formatters/html"
	"github.com/alecthomas/chroma/lexers"
	"github.com/alecthomas/chroma/styles"
	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting"

	"embed"

	"github.com/song940/gitgo/githttp"
)

//go:embed templates
var templatefiles embed.FS

//go:embed static
var staticfiles embed.FS

const PAGE_SIZE int = 100

type RepositoryWithName struct {
	Name       string
	Repository *git.Repository
	// Meta       RepoConfig
}

type RepositoryByName []RepositoryWithName

func (r RepositoryByName) Len() int      { return len(r) }
func (r RepositoryByName) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r RepositoryByName) Less(i, j int) bool {
	res := strings.Compare(r[i].Name, r[j].Name)
	return res < 0
}

type ReferenceByName []*plumbing.Reference

func (r ReferenceByName) Len() int      { return len(r) }
func (r ReferenceByName) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r ReferenceByName) Less(i, j int) bool {
	res := strings.Compare(r[i].Name().String(), r[j].Name().String())
	return res < 0
}

type Commit struct {
	Commit    *object.Commit
	Subject   string
	ShortHash string
}

func (c *Commit) FormattedDate() string {
	return c.Commit.Author.When.Format("2006-01-02")
	// return c.Commit.Author.When.Format(time.RFC822)
}

func ReferenceCollector(it storer.ReferenceIter) ([]*plumbing.Reference, error) {
	var refs []*plumbing.Reference

	for {
		b, err := it.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return refs, err
		}

		refs = append(refs, b)
	}
	sort.Sort(ReferenceByName(refs))
	return refs, nil
}

func ListBranches(r *git.Repository) ([]*plumbing.Reference, error) {
	it, err := r.Branches()
	if err != nil {
		return []*plumbing.Reference{}, err
	}
	return ReferenceCollector(it)
}

func ListTags(r *git.Repository) ([]*plumbing.Reference, error) {
	it, err := r.Tags()
	if err != nil {
		return []*plumbing.Reference{}, err
	}
	return ReferenceCollector(it)
}

func GetReadmeFromCommit(commit *object.Commit) (*object.File, error) {
	options := []string{
		"README.md",
		"README",
		"README.markdown",
		"readme.md",
		"readme.markdown",
		"readme",
	}

	for _, opt := range options {
		f, err := commit.File(opt)

		if err == nil {
			return f, nil
		}
	}
	return nil, errors.New("no valid readme")
}

func FormatMarkdown(input string) string {
	var buf bytes.Buffer
	markdown := goldmark.New(
		goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(
					html.WithClasses(true),
				),
			),
		),
	)
	if err := markdown.Convert([]byte(input), &buf); err != nil {
		return input
	}
	return buf.String()
}

func RenderSyntaxHighlighting(file *object.File) (string, error) {
	contents, err := file.Contents()
	if err != nil {
		return "", err
	}
	lexer := lexers.Match(file.Name)
	if lexer == nil {
		// If the lexer is nil, we weren't able to find one based on the file
		// extension.  We can render it as plain text.
		return fmt.Sprintf("<pre>%s</pre>", contents), nil
	}

	style := styles.Get("autumn")

	if style == nil {
		style = styles.Fallback
	}

	formatter := html.New(
		html.WithClasses(true),
		html.WithLineNumbers(true),
		html.LineNumbersInTable(true),
		html.LinkableLineNumbers(true, "L"),
	)

	iterator, err := lexer.Tokenise(nil, contents)

	buf := bytes.NewBuffer(nil)
	err = formatter.Format(buf, style, iterator)

	if err != nil {
		return fmt.Sprintf("<pre>%s</pre>", contents), nil
	}

	return buf.String(), nil
}

func Http404(ctx *gin.Context) {
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	ctx.HTML(http.StatusNotFound, "404.html", makeTemplateContext(smithyConfig, gin.H{}))
}

func Http500(ctx *gin.Context) {
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	ctx.HTML(http.StatusInternalServerError, "500.html",
		makeTemplateContext(smithyConfig, gin.H{}))
}

func makeTemplateContext(config SmithyConfig, extra gin.H) gin.H {
	results := gin.H{
		"Site": gin.H{
			"Title":       config.Title,
			"Description": config.Description,
			"Host":        config.Host,
		},
	}
	for k, v := range extra {
		results[k] = v
	}
	return results
}

func IndexView(ctx *gin.Context, urlParts []string) {
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repos := smithyConfig.GetRepositories()

	ctx.HTML(http.StatusOK, "index.html", makeTemplateContext(smithyConfig, gin.H{
		"Repos": repos,
	}))
}

func findMainBranch(ctx *gin.Context, repo *git.Repository) (string, *plumbing.Hash, error) {
	for _, candidate := range []string{"main", "master"} {
		revision, err := repo.ResolveRevision(plumbing.Revision(candidate))
		if err == nil {
			return candidate, revision, nil
		}
		ctx.Error(err)
	}
	return "", nil, fmt.Errorf("failed to find a 'main' or 'master' branch")
}

func RepoIndexView(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)

	if !exists {
		Http404(ctx)
		return
	}

	branches, err := ListBranches(repo.Repository)

	if err != nil {
		Http500(ctx)
		return
	}

	tags, err := ListTags(repo.Repository)
	if err != nil {
		Http500(ctx)
		return
	}

	var formattedReadme string

	main, revision, err := findMainBranch(ctx, repo.Repository)

	log.Println("findMainBranch", main, revision)

	if err != nil {
		Http500(ctx)
		return
	}

	commitObj, err := repo.Repository.CommitObject(*revision)

	if err != nil {
		Http500(ctx)
		return
	}

	readme, err := GetReadmeFromCommit(commitObj)

	if err != nil {
		formattedReadme = ""
	} else {
		readmeContents, err := readme.Contents()

		if err != nil {
			formattedReadme = ""
		} else {
			formattedReadme = FormatMarkdown(readmeContents)
		}
	}

	ctx.HTML(http.StatusOK, "repo.html", makeTemplateContext(smithyConfig, gin.H{
		"RepoName": repoName,
		"Branches": branches,
		"Tags":     tags,
		"Readme":   template.HTML(formattedReadme),
		"Repo":     repo,
	}))
}

func RepoGitView(ctx *gin.Context, urlParts []string) {
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	git := githttp.New(smithyConfig.Git.Root)
	git.ServeHTTP(ctx.Writer, ctx.Request)
}

func RefsView(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)

	if !exists {
		Http404(ctx)
		return
	}

	branches, err := ListBranches(repo.Repository)

	if err != nil {
		branches = []*plumbing.Reference{}
	}

	tags, err := ListTags(repo.Repository)
	if err != nil {
		tags = []*plumbing.Reference{}
	}

	ctx.HTML(http.StatusOK, "refs.html", makeTemplateContext(smithyConfig, gin.H{
		"RepoName": repoName,
		"Branches": branches,
		"Tags":     tags,
	}))
}

func TreeView(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)

	if !exists {
		Http404(ctx)
		return
	}

	var err error
	var refNameString string

	if len(urlParts) > 1 {
		refNameString = urlParts[1]
	} else {
		refNameString, _, err = findMainBranch(ctx, repo.Repository)
		if err != nil {
			ctx.Error(err)
			Http404(ctx)
			return
		}
	}

	revision, err := repo.Repository.ResolveRevision(plumbing.Revision(refNameString))

	if err != nil {
		Http404(ctx)
		return
	}

	treePath := ""

	if len(urlParts) > 2 {
		treePath = urlParts[2]
	}

	parentPath := filepath.Dir(treePath)
	commitObj, err := repo.Repository.CommitObject(*revision)

	if err != nil {
		Http404(ctx)
		return
	}

	tree, err := commitObj.Tree()

	if err != nil {
		Http404(ctx)
		return
	}

	// We're looking at the root of the project.  Show a list of files.
	if treePath == "" {
		ctx.HTML(http.StatusOK, "tree.html", makeTemplateContext(smithyConfig, gin.H{
			"RepoName": repoName,
			"RefName":  refNameString,
			"Files":    tree.Entries,
			"Path":     treePath,
		}))
		return
	}

	out, err := tree.FindEntry(treePath)
	if err != nil {
		Http404(ctx)
		return
	}

	// We found a subtree.
	if !out.Mode.IsFile() {
		subTree, err := tree.Tree(treePath)
		if err != nil {
			Http404(ctx)
			return
		}
		ctx.HTML(http.StatusOK, "tree.html", makeTemplateContext(smithyConfig, gin.H{
			"RepoName":   repoName,
			"ParentPath": parentPath,
			"RefName":    refNameString,
			"SubTree":    out.Name,
			"Path":       treePath,
			"Files":      subTree.Entries,
		}))
		return
	}

	// Now do a regular file
	file, err := tree.File(treePath)
	if err != nil {
		Http404(ctx)
		return
	}
	contents, err := file.Contents()
	syntaxHighlighted, _ := RenderSyntaxHighlighting(file)
	if err != nil {
		Http404(ctx)
		return
	}
	ctx.HTML(http.StatusOK, "blob.html", makeTemplateContext(smithyConfig, gin.H{
		"RepoName":            repoName,
		"RefName":             refNameString,
		"File":                out,
		"ParentPath":          parentPath,
		"Path":                treePath,
		"Contents":            contents,
		"ContentsHighlighted": template.HTML(syntaxHighlighted),
	}))
}

func LogView(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)
	if !exists {
		Http404(ctx)
		return
	}

	refNameString := urlParts[1]
	revision, err := repo.Repository.ResolveRevision(plumbing.Revision(refNameString))
	if err != nil {
		Http404(ctx)
		return
	}

	var commits []Commit
	cIter, err := repo.Repository.Log(&git.LogOptions{From: *revision, Order: git.LogOrderCommitterTime})
	if err != nil {
		Http500(ctx)
		return
	}

	for i := 1; i <= PAGE_SIZE; i++ {
		commit, err := cIter.Next()

		if err == io.EOF {
			break
		}

		lines := strings.Split(commit.Message, "\n")

		c := Commit{
			Commit:    commit,
			Subject:   lines[0],
			ShortHash: commit.Hash.String()[:8],
		}
		commits = append(commits, c)
	}

	ctx.HTML(http.StatusOK, "log.html", makeTemplateContext(smithyConfig, gin.H{
		"RepoName": repoName,
		"RefName":  refNameString,
		"Commits":  commits,
	}))
}

func LogViewDefault(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)
	if !exists {
		Http404(ctx)
		return
	}

	mainBranchName, _, err := findMainBranch(ctx, repo.Repository)
	if err != nil {
		ctx.Error(err)
		Http404(ctx)
		return
	}

	ctx.Redirect(http.StatusPermanentRedirect, ctx.Request.RequestURI+"/"+mainBranchName)
}

func GetChanges(commit *object.Commit) (object.Changes, error) {
	var changes object.Changes
	var parentTree *object.Tree

	parent, err := commit.Parent(0)
	if err == nil {
		parentTree, err = parent.Tree()
		if err != nil {
			return changes, err
		}
	}

	currentTree, err := commit.Tree()
	if err != nil {
		return changes, err
	}

	return object.DiffTree(parentTree, currentTree)

}

// FormatChanges spits out something similar to `git diff`
func FormatChanges(changes object.Changes) (string, error) {
	var s []string
	for _, change := range changes {
		patch, err := change.Patch()
		if err != nil {
			return "", err
		}
		s = append(s, PatchHTML(*patch))
	}

	return strings.Join(s, "\n\n\n\n"), nil
}

func PatchView(ctx *gin.Context, urlParts []string) {
	const commitFormatDate = "Mon, 2 Jan 2006 15:04:05 -0700"
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)
	if !exists {
		Http404(ctx)
		return
	}

	var patch string
	commitID := urlParts[1]
	if commitID == "" {
		Http404(ctx)
		return
	}

	commitHash := plumbing.NewHash(commitID)
	commitObj, err := repo.Repository.CommitObject(commitHash)
	if err != nil {
		Http404(ctx)
		return
	}

	// TODO: If this is the first commit, we can't build the diff (#281)
	// Therefore, we have two options: either build the diff manually or
	// patch go-git
	if commitObj.NumParents() == 0 {
		Http500(ctx)
		return
	} else {
		parentCommit, err := commitObj.Parent(0)

		if err != nil {
			Http500(ctx)
			return
		}

		patchObj, err := parentCommit.Patch(commitObj)
		if err != nil {
			Http500(ctx)
			return
		}
		patch = patchObj.String()
	}

	commitHashStr := fmt.Sprintf("From %s Mon Sep 17 00:00:00 2001", commitObj.Hash)
	from := fmt.Sprintf("From: %s <%s>", commitObj.Author.Name, commitObj.Author.Email)
	date := fmt.Sprintf("Date: %s", commitObj.Author.When.Format(commitFormatDate))
	subject := fmt.Sprintf("Subject: [PATCH] %s", commitObj.Message)

	stats, err := commitObj.Stats()
	if err != nil {
		Http500(ctx)
		return
	}

	ctx.String(http.StatusOK, "%s\n%s\n%s\n%s\n---\n%s\n%s",
		commitHashStr, from, date, subject, stats.String(), patch)
}

func CommitView(ctx *gin.Context, urlParts []string) {
	repoName := urlParts[0]
	smithyConfig := ctx.MustGet("config").(SmithyConfig)
	repo, exists := smithyConfig.FindRepo(repoName)
	if !exists {
		Http404(ctx)
		return
	}

	commitID := urlParts[1]
	if commitID == "" {
		Http404(ctx)
		return
	}
	commitHash := plumbing.NewHash(commitID)
	commitObj, err := repo.Repository.CommitObject(commitHash)
	if err != nil {
		Http404(ctx)
		return
	}

	changes, err := GetChanges(commitObj)
	if err != nil {
		Http404(ctx)
		return
	}

	formattedChanges, err := FormatChanges(changes)
	if err != nil {
		Http404(ctx)
		return
	}

	ctx.HTML(http.StatusOK, "commit.html", makeTemplateContext(smithyConfig, gin.H{
		"RepoName": repoName,
		"Commit":   commitObj,
		"Changes":  template.HTML(formattedChanges),
	}))
}

// Make the config available to every request
func AddConfigMiddleware(cfg SmithyConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("config", cfg)
	}
}

// PatchHTML returns an HTML representation of a patch
func PatchHTML(p object.Patch) string {
	buf := bytes.NewBuffer(nil)
	ue := NewUnifiedEncoder(buf, DefaultContextLines)
	err := ue.Encode(p)
	if err != nil {
		fmt.Println("PatchHTML error")
	}
	return buf.String()
}

type Route struct {
	Pattern *regexp.Regexp
	View    func(*gin.Context, []string)
}

func CompileRoutes() []Route {
	// Label is either a repo, a ref
	// A filepath is a list of labels
	label := `[a-zA-Z0-9\-~\.]+`

	indexUrl := regexp.MustCompile(`^/$`)
	repoGitUrl := regexp.MustCompile(`^/git/(?P<repo>` + label + `)`)
	repoIndexUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)$`)
	refsUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/refs$`)
	logDefaultUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/log$`)
	logUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/log/(?P<ref>` + label + `)$`)
	commitUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/commit/(?P<commit>[a-z0-9]+)$`)
	patchUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/commit/(?P<commit>[a-z0-9]+).patch`)

	treeRootUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/tree$`)
	treeRootRefUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/tree/(?P<ref>` + label + `)$`)
	treeRootRefPathUrl := regexp.MustCompile(`^/(?P<repo>` + label + `)/tree/(?P<ref>` + label + `)/(?P<path>.*)$`)

	return []Route{
		{Pattern: indexUrl, View: IndexView},
		{Pattern: repoIndexUrl, View: RepoIndexView},
		{Pattern: repoGitUrl, View: RepoGitView},
		{Pattern: refsUrl, View: RefsView},
		{Pattern: logDefaultUrl, View: LogViewDefault},
		{Pattern: logUrl, View: LogView},
		{Pattern: commitUrl, View: CommitView},
		{Pattern: patchUrl, View: PatchView},
		{Pattern: treeRootUrl, View: TreeView},
		{Pattern: treeRootRefUrl, View: TreeView},
		{Pattern: treeRootRefPathUrl, View: TreeView},
	}
}

func Dispatch(ctx *gin.Context, routes []Route, fileSystemHandler http.Handler) {
	urlPath := ctx.Request.URL.String()
	if strings.HasPrefix(urlPath, "/static/") {
		fileSystemHandler.ServeHTTP(ctx.Writer, ctx.Request)
		return
	}

	for _, route := range routes {
		if !route.Pattern.MatchString(urlPath) {
			continue
		}

		urlParts := []string{}
		for i, match := range route.Pattern.FindStringSubmatch(urlPath) {
			if i != 0 {
				urlParts = append(urlParts, match)
			}
		}

		route.View(ctx, urlParts)
		return

	}

	Http404(ctx)

}

func loadTemplates(smithyConfig SmithyConfig) (*template.Template, error) {

	funcs := template.FuncMap{
		// "css": func() string {
		// 	return cssPath
		// },
	}
	t := template.New("").Funcs(funcs)
	files, err := templatefiles.ReadDir("templates")
	if err != nil {
		return t, err
	}

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".html") {
			continue
		}
		f, err := templatefiles.Open("templates/" + file.Name())
		if err != nil {
			return t, err
		}
		contents, err := ioutil.ReadAll(f)
		if err != nil {
			return t, err
		}

		_, err = t.New(file.Name()).Parse(string(contents))
		if err != nil {
			return t, err
		}

	}
	return t, nil
}
