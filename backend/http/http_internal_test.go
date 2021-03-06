// +build go1.8

package http

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/config"
	"github.com/ncw/rclone/fs/config/configmap"
	"github.com/ncw/rclone/fstest"
	"github.com/ncw/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	remoteName = "TestHTTP"
	testPath   = "test"
	filesPath  = filepath.Join(testPath, "files")
)

// prepareServer the test server and return a function to tidy it up afterwards
func prepareServer(t *testing.T) (configmap.Simple, func()) {
	// file server for test/files
	fileServer := http.FileServer(http.Dir(filesPath))

	// Make the test server
	ts := httptest.NewServer(fileServer)

	// Configure the remote
	config.LoadConfig()
	// fs.Config.LogLevel = fs.LogLevelDebug
	// fs.Config.DumpHeaders = true
	// fs.Config.DumpBodies = true
	// config.FileSet(remoteName, "type", "http")
	// config.FileSet(remoteName, "url", ts.URL)

	m := configmap.Simple{
		"type": "http",
		"url":  ts.URL,
	}

	// return a function to tidy up
	return m, ts.Close
}

// prepare the test server and return a function to tidy it up afterwards
func prepare(t *testing.T) (fs.Fs, func()) {
	m, tidy := prepareServer(t)

	// Instantiate it
	f, err := NewFs(remoteName, "", m)
	require.NoError(t, err)

	return f, tidy
}

func testListRoot(t *testing.T, f fs.Fs) {
	entries, err := f.List("")
	require.NoError(t, err)

	sort.Sort(entries)

	require.Equal(t, 4, len(entries))

	e := entries[0]
	assert.Equal(t, "four", e.Remote())
	assert.Equal(t, int64(-1), e.Size())
	_, ok := e.(fs.Directory)
	assert.True(t, ok)

	e = entries[1]
	assert.Equal(t, "one%.txt", e.Remote())
	assert.Equal(t, int64(6), e.Size())
	_, ok = e.(*Object)
	assert.True(t, ok)

	e = entries[2]
	assert.Equal(t, "three", e.Remote())
	assert.Equal(t, int64(-1), e.Size())
	_, ok = e.(fs.Directory)
	assert.True(t, ok)

	e = entries[3]
	assert.Equal(t, "two.html", e.Remote())
	assert.Equal(t, int64(7), e.Size())
	_, ok = e.(*Object)
	assert.True(t, ok)
}

func TestListRoot(t *testing.T) {
	f, tidy := prepare(t)
	defer tidy()
	testListRoot(t, f)
}

func TestListSubDir(t *testing.T) {
	f, tidy := prepare(t)
	defer tidy()

	entries, err := f.List("three")
	require.NoError(t, err)

	sort.Sort(entries)

	assert.Equal(t, 1, len(entries))

	e := entries[0]
	assert.Equal(t, "three/underthree.txt", e.Remote())
	assert.Equal(t, int64(9), e.Size())
	_, ok := e.(*Object)
	assert.True(t, ok)
}

func TestNewObject(t *testing.T) {
	f, tidy := prepare(t)
	defer tidy()

	o, err := f.NewObject("four/under four.txt")
	require.NoError(t, err)

	assert.Equal(t, "four/under four.txt", o.Remote())
	assert.Equal(t, int64(9), o.Size())
	_, ok := o.(*Object)
	assert.True(t, ok)

	// Test the time is correct on the object

	tObj := o.ModTime()

	fi, err := os.Stat(filepath.Join(filesPath, "four", "under four.txt"))
	require.NoError(t, err)
	tFile := fi.ModTime()

	dt, ok := fstest.CheckTimeEqualWithPrecision(tObj, tFile, time.Second)
	assert.True(t, ok, fmt.Sprintf("%s: Modification time difference too big |%s| > %s (%s vs %s) (precision %s)", o.Remote(), dt, time.Second, tObj, tFile, time.Second))
}

func TestOpen(t *testing.T) {
	f, tidy := prepare(t)
	defer tidy()

	o, err := f.NewObject("four/under four.txt")
	require.NoError(t, err)

	// Test normal read
	fd, err := o.Open()
	require.NoError(t, err)
	data, err := ioutil.ReadAll(fd)
	require.NoError(t, err)
	require.NoError(t, fd.Close())
	assert.Equal(t, "beetroot\n", string(data))

	// Test with range request
	fd, err = o.Open(&fs.RangeOption{Start: 1, End: 5})
	require.NoError(t, err)
	data, err = ioutil.ReadAll(fd)
	require.NoError(t, err)
	require.NoError(t, fd.Close())
	assert.Equal(t, "eetro", string(data))
}

func TestMimeType(t *testing.T) {
	f, tidy := prepare(t)
	defer tidy()

	o, err := f.NewObject("four/under four.txt")
	require.NoError(t, err)

	do, ok := o.(fs.MimeTyper)
	require.True(t, ok)
	assert.Equal(t, "text/plain; charset=utf-8", do.MimeType())
}

func TestIsAFileRoot(t *testing.T) {
	m, tidy := prepareServer(t)
	defer tidy()

	f, err := NewFs(remoteName, "one%.txt", m)
	assert.Equal(t, err, fs.ErrorIsFile)

	testListRoot(t, f)
}

func TestIsAFileSubDir(t *testing.T) {
	m, tidy := prepareServer(t)
	defer tidy()

	f, err := NewFs(remoteName, "three/underthree.txt", m)
	assert.Equal(t, err, fs.ErrorIsFile)

	entries, err := f.List("")
	require.NoError(t, err)

	sort.Sort(entries)

	assert.Equal(t, 1, len(entries))

	e := entries[0]
	assert.Equal(t, "underthree.txt", e.Remote())
	assert.Equal(t, int64(9), e.Size())
	_, ok := e.(*Object)
	assert.True(t, ok)
}

func TestParseName(t *testing.T) {
	for i, test := range []struct {
		base    string
		val     string
		wantErr error
		want    string
	}{
		{"http://example.com/", "potato", nil, "potato"},
		{"http://example.com/dir/", "potato", nil, "potato"},
		{"http://example.com/dir/", "potato?download=true", errFoundQuestionMark, ""},
		{"http://example.com/dir/", "../dir/potato", nil, "potato"},
		{"http://example.com/dir/", "..", errNotUnderRoot, ""},
		{"http://example.com/dir/", "http://example.com/", errNotUnderRoot, ""},
		{"http://example.com/dir/", "http://example.com/dir/", errNameIsEmpty, ""},
		{"http://example.com/dir/", "http://example.com/dir/potato", nil, "potato"},
		{"http://example.com/dir/", "https://example.com/dir/potato", errSchemeMismatch, ""},
		{"http://example.com/dir/", "http://notexample.com/dir/potato", errHostMismatch, ""},
		{"http://example.com/dir/", "/dir/", errNameIsEmpty, ""},
		{"http://example.com/dir/", "/dir/potato", nil, "potato"},
		{"http://example.com/dir/", "subdir/potato", errNameContainsSlash, ""},
		{"http://example.com/dir/", "With percent %25.txt", nil, "With percent %.txt"},
		{"http://example.com/dir/", "With colon :", errURLJoinFailed, ""},
		{"http://example.com/dir/", rest.URLPathEscape("With colon :"), nil, "With colon :"},
		{"http://example.com/Dungeons%20%26%20Dragons/", "/Dungeons%20&%20Dragons/D%26D%20Basic%20%28Holmes%2C%20B%2C%20X%2C%20BECMI%29/", nil, "D&D Basic (Holmes, B, X, BECMI)/"},
	} {
		u, err := url.Parse(test.base)
		require.NoError(t, err)
		got, gotErr := parseName(u, test.val)
		what := fmt.Sprintf("test %d base=%q, val=%q", i, test.base, test.val)
		assert.Equal(t, test.wantErr, gotErr, what)
		assert.Equal(t, test.want, got, what)
	}
}

// Load HTML from the file given and parse it, checking it against the entries passed in
func parseHTML(t *testing.T, name string, base string, want []string) {
	in, err := os.Open(filepath.Join(testPath, "index_files", name))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, in.Close())
	}()
	if base == "" {
		base = "http://example.com/"
	}
	u, err := url.Parse(base)
	require.NoError(t, err)
	entries, err := parse(u, in)
	require.NoError(t, err)
	assert.Equal(t, want, entries)
}

func TestParseEmpty(t *testing.T) {
	parseHTML(t, "empty.html", "", []string(nil))
}

func TestParseApache(t *testing.T) {
	parseHTML(t, "apache.html", "http://example.com/nick/pub/", []string{
		"SWIG-embed.tar.gz",
		"avi2dvd.pl",
		"cambert.exe",
		"cambert.gz",
		"fedora_demo.gz",
		"gchq-challenge/",
		"mandelterm/",
		"pgp-key.txt",
		"pymath/",
		"rclone",
		"readdir.exe",
		"rush_hour_solver_cut_down.py",
		"snake-puzzle/",
		"stressdisk/",
		"timer-test",
		"words-to-regexp.pl",
		"Now 100% better.mp3",
		"Now better.mp3",
	})
}

func TestParseMemstore(t *testing.T) {
	parseHTML(t, "memstore.html", "", []string{
		"test/",
		"v1.35/",
		"v1.36-01-g503cd84/",
		"rclone-beta-latest-freebsd-386.zip",
		"rclone-beta-latest-freebsd-amd64.zip",
		"rclone-beta-latest-windows-amd64.zip",
	})
}

func TestParseNginx(t *testing.T) {
	parseHTML(t, "nginx.html", "", []string{
		"deltas/",
		"objects/",
		"refs/",
		"state/",
		"config",
		"summary",
	})
}

func TestParseCaddy(t *testing.T) {
	parseHTML(t, "caddy.html", "", []string{
		"mimetype.zip",
		"rclone-delete-empty-dirs.py",
		"rclone-show-empty-dirs.py",
		"stat-windows-386.zip",
		"v1.36-155-gcf29ee8b-team-drive??/",
		"v1.36-156-gca76b3fb-team-drive??/",
		"v1.36-156-ge1f0e0f5-team-drive??/",
		"v1.36-22-g06ea13a-ssh-agent??/",
	})
}
