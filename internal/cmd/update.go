package cmd

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/coder-cli/internal/config"
	"cdr.dev/coder-cli/internal/version"
	"cdr.dev/coder-cli/pkg/clog"

	"github.com/Masterminds/semver/v3"
	"github.com/manifoldco/promptui"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

const (
	goosWindows       = "windows"
	goosLinux         = "linux"
	goosDarwin        = "darwin"
	apiPrivateVersion = "/api/private/version"
)

// updater updates coder-cli.
type updater struct {
	confirmF       func(string) (string, error)
	execF          func(context.Context, string, ...string) ([]byte, error)
	executablePath string
	fs             afero.Fs
	httpClient     getter
	osF            func() string
	versionF       func() string
}

func updateCmd() *cobra.Command {
	var (
		force      bool
		coderURL   string
		versionArg string
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update coder binary",
		Long:  "Update coder to the version matching a given coder instance.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			httpClient := &http.Client{
				Timeout: 10 * time.Second,
			}

			currExe, err := os.Executable()
			if err != nil {
				return clog.Fatal("init: get current executable", clog.Causef(err.Error()))
			}

			updater := &updater{
				confirmF:       defaultConfirm,
				execF:          defaultExec,
				executablePath: currExe,
				httpClient:     httpClient,
				fs:             afero.NewOsFs(),
				osF:            func() string { return runtime.GOOS },
				versionF:       func() string { return version.Version },
			}
			return updater.Run(ctx, force, coderURL, versionArg)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "do not prompt for confirmation")
	cmd.Flags().StringVar(&coderURL, "coder", "", "query this coder instance for the matching version")
	cmd.Flags().StringVar(&versionArg, "version", "", "explicitly specify which version to fetch and install")

	return cmd
}

type getter interface {
	Get(url string) (*http.Response, error)
}

func (u *updater) Run(ctx context.Context, force bool, coderURLArg string, versionArg string) error {
	// Check under following directories and warn if coder binary is under them:
	//   * C:\Windows\
	//   * homebrew prefix
	//   * coder assets root (/var/tmp/coder)
	var pathBlockList = []string{
		`C:\Windows\`,
		`/var/tmp/coder`,
	}
	brewPrefixCmd, err := u.execF(ctx, "brew", "--prefix")
	if err == nil { // ignore errors if homebrew not installed
		pathBlockList = append(pathBlockList, strings.TrimSpace(string(brewPrefixCmd)))
	}

	for _, prefix := range pathBlockList {
		if HasFilePathPrefix(u.executablePath, prefix) {
			return clog.Fatal(
				"cowardly refusing to update coder binary",
				clog.BlankLine,
				clog.Causef("executable path %q is under blocklisted prefix %q", u.executablePath, prefix))
		}
	}

	currentBinaryStat, err := u.fs.Stat(u.executablePath)
	if err != nil {
		return clog.Fatal("preflight: cannot stat current binary", clog.Causef(err.Error()))
	}

	if currentBinaryStat.Mode().Perm()&0222 == 0 {
		return clog.Fatal("preflight: missing write permission on current binary")
	}

	clog.LogInfo(fmt.Sprintf("Current version of coder-cli is %q", version.Version))

	desiredVersion, err := getDesiredVersion(u.httpClient, coderURLArg, versionArg)
	if err != nil {
		return clog.Fatal("failed to determine desired version of coder", clog.Causef(err.Error()))
	}

	currentVersion, err := semver.NewVersion(u.versionF())
	if err != nil {
		clog.LogWarn("failed to determine current version of coder-cli", clog.Causef(err.Error()))
	} else if compareVersions(currentVersion, desiredVersion) == 0 {
		clog.LogInfo("Up to date!")
		return nil
	}

	if !force {
		prerelease := ""
		if desiredVersion.Prerelease() != "" {
			prerelease = "-" + desiredVersion.Prerelease()
		}
		hotfix := ""
		if hotfixVersion(desiredVersion) != "" {
			hotfix = hotfixVersion(desiredVersion)
		}
		label := fmt.Sprintf("Do you want to download version %d.%d.%d%s%s instead",
			desiredVersion.Major(),
			desiredVersion.Minor(),
			desiredVersion.Patch(),
			prerelease,
			hotfix,
		)
		if _, err := u.confirmF(label); err != nil {
			return clog.Fatal("user cancelled operation", clog.Tipf(`use "--force" to update without confirmation`))
		}
	}

	downloadURL, err := queryGithubAssetURL(u.httpClient, desiredVersion, u.osF())
	if err != nil {
		return clog.Fatal("failed to query github assets url", clog.Causef(err.Error()))
	}

	var downloadBuf bytes.Buffer
	memWriter := bufio.NewWriter(&downloadBuf)

	clog.LogInfo("fetching coder-cli from GitHub releases", downloadURL)
	resp, err := u.httpClient.Get(downloadURL)
	if err != nil {
		return clog.Fatal(fmt.Sprintf("failed to fetch URL %s", downloadURL), clog.Causef(err.Error()))
	}

	if resp.StatusCode != http.StatusOK {
		return clog.Fatal("failed to fetch release", clog.Causef("URL %s returned status code %d", downloadURL, resp.StatusCode))
	}

	if _, err := io.Copy(memWriter, resp.Body); err != nil {
		return clog.Fatal(fmt.Sprintf("failed to download %s", downloadURL), clog.Causef(err.Error()))
	}

	_ = resp.Body.Close()

	if err := memWriter.Flush(); err != nil {
		return clog.Fatal(fmt.Sprintf("failed to save %s", downloadURL), clog.Causef(err.Error()))
	}

	// TODO: validate the checksum of the downloaded file. GitHub does not currently provide this information
	// and we do not generate them yet.
	var updatedBinaryName string
	if u.osF() == goosWindows {
		updatedBinaryName = "coder.exe"
	} else {
		updatedBinaryName = "coder"
	}
	updatedBinary, err := extractFromArchive(updatedBinaryName, downloadBuf.Bytes())
	if err != nil {
		return clog.Fatal("failed to extract coder binary from archive", clog.Causef(err.Error()))
	}

	// We assume the binary is named coder and write it to coder.new
	updatedCoderBinaryPath := u.executablePath + ".new"
	updatedBin, err := u.fs.OpenFile(updatedCoderBinaryPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, currentBinaryStat.Mode().Perm())
	if err != nil {
		return clog.Fatal("failed to create file for updated coder binary", clog.Causef(err.Error()))
	}

	fsWriter := bufio.NewWriter(updatedBin)
	if _, err := io.Copy(fsWriter, bytes.NewReader(updatedBinary)); err != nil {
		return clog.Fatal("failed to write updated coder binary to disk", clog.Causef(err.Error()))
	}

	if err := fsWriter.Flush(); err != nil {
		return clog.Fatal("failed to persist updated coder binary to disk", clog.Causef(err.Error()))
	}

	_ = updatedBin.Close()

	if err := u.doUpdate(ctx, updatedCoderBinaryPath); err != nil {
		return clog.Fatal("failed to update coder binary", clog.Causef(err.Error()))
	}

	clog.LogSuccess("Updated coder CLI")
	return nil
}

func (u *updater) doUpdate(ctx context.Context, updatedCoderBinaryPath string) error {
	var err error
	// TODO(cian): on Windows, we must do two things differently:
	// 1) Calling the updated binary fails due to the xterminal.MakeOutputRaw call in main; skipping this check on Windows.
	// 2) We must rename the currently running binary before renaming the new binary
	if u.osF() == goosWindows {
		err = u.fs.Rename(u.executablePath, updatedCoderBinaryPath+".old")
		if err != nil {
			return xerrors.Errorf("windows: rename current coder binary: %w", err)
		}
		err = u.fs.Rename(updatedCoderBinaryPath, u.executablePath)
		if err != nil {
			return xerrors.Errorf("windows: rename updated coder binary: %w", err)
		}
		return nil
	}

	// validate that we can execute the new binary before overwriting
	updatedVersionOutput, err := u.execF(ctx, updatedCoderBinaryPath, "--version")
	if err != nil {
		return xerrors.Errorf("check version of updated coder binary: %w", err)
	}
	clog.LogInfo(fmt.Sprintf("updated binary reports %q", bytes.TrimSpace(updatedVersionOutput)))

	if err = u.fs.Rename(updatedCoderBinaryPath, u.executablePath); err != nil {
		return xerrors.Errorf("update coder binary in-place: %w", err)
	}

	return nil
}

func getDesiredVersion(httpClient getter, coderURLArg string, versionArg string) (*semver.Version, error) {
	var coderURL *url.URL
	var desiredVersion *semver.Version
	var err error

	if coderURLArg != "" && versionArg != "" {
		clog.LogWarn(fmt.Sprintf("ignoring the version reported by %q", coderURLArg), clog.Causef("--version flag was specified explicitly"))
	}

	if versionArg != "" {
		desiredVersion, err = semver.NewVersion(versionArg)
		if err != nil {
			return &semver.Version{}, xerrors.Errorf("parse desired version arg: %w", err)
		}
		return desiredVersion, nil
	}

	if coderURLArg == "" {
		coderURL, err = getCoderConfigURL()
		if err != nil {
			return &semver.Version{}, xerrors.Errorf("get coder url: %w", err)
		}
	} else {
		coderURL, err = url.Parse(coderURLArg)
		if err != nil {
			return &semver.Version{}, xerrors.Errorf("parse coder url arg: %w", err)
		}
	}

	desiredVersion, err = getAPIVersionUnauthed(httpClient, *coderURL)
	if err != nil {
		return &semver.Version{}, xerrors.Errorf("query coder version: %w", err)
	}

	clog.LogInfo(fmt.Sprintf("Coder instance at %q reports version %q", coderURL.String(), desiredVersion.String()))

	return desiredVersion, nil
}

func defaultConfirm(label string) (string, error) {
	p := promptui.Prompt{IsConfirm: true, Label: label}
	return p.Run()
}

func queryGithubAssetURL(httpClient getter, version *semver.Version, ostype string) (string, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d", version.Major())
	fmt.Fprint(&b, ".")
	fmt.Fprintf(&b, "%d", version.Minor())
	fmt.Fprint(&b, ".")
	fmt.Fprintf(&b, "%d", version.Patch())
	if version.Prerelease() != "" {
		fmt.Fprint(&b, "-")
		fmt.Fprint(&b, version.Prerelease())
	}
	fmt.Fprintf(&b, "%s", hotfixVersion(version)) // this will be empty if no hotfix

	urlString := fmt.Sprintf("https://api.github.com/repos/cdr/coder-cli/releases/tags/v%s", b.String())
	clog.LogInfo("query github releases", fmt.Sprintf("url: %q", urlString))

	type asset struct {
		BrowserDownloadURL string `json:"browser_download_url"`
		Name               string `json:"name"`
	}
	type release struct {
		Assets []asset `json:"assets"`
	}
	var r release

	resp, err := httpClient.Get(urlString)
	if err != nil {
		return "", xerrors.Errorf("query github release url %s: %w", urlString, err)
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return "", xerrors.Errorf("unmarshal github releases api response: %w", err)
	}

	var assetURLStr string
	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, "coder-cli-"+ostype) {
			assetURLStr = a.BrowserDownloadURL
		}
	}

	if assetURLStr == "" {
		return "", xerrors.Errorf("could not find release for ostype %s", ostype)
	}

	return assetURLStr, nil
}

func extractFromArchive(path string, archive []byte) ([]byte, error) {
	contentType := http.DetectContentType(archive)
	switch contentType {
	case "application/zip":
		return extractFromZip(path, archive)
	case "application/x-gzip":
		return extractFromTgz(path, archive)
	default:
		return nil, xerrors.Errorf("unknown archive type: %s", contentType)
	}
}

func extractFromZip(path string, archive []byte) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, xerrors.Errorf("failed to open zip archive")
	}

	var zf *zip.File
	for _, f := range zipReader.File {
		if f.Name == path {
			zf = f
			break
		}
	}
	if zf == nil {
		return nil, xerrors.Errorf("could not find path %q in zip archive", path)
	}

	rc, err := zf.Open()
	if err != nil {
		return nil, xerrors.Errorf("failed to extract path %q from archive", path)
	}
	defer rc.Close()

	var b bytes.Buffer
	bw := bufio.NewWriter(&b)
	if _, err := io.Copy(bw, rc); err != nil {
		return nil, xerrors.Errorf("failed to copy path %q to from archive", path)
	}
	return b.Bytes(), nil
}

func extractFromTgz(path string, archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, xerrors.Errorf("failed to gunzip archive")
	}

	tr := tar.NewReader(zr)

	var b bytes.Buffer
	bw := bufio.NewWriter(&b)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, xerrors.Errorf("failed to read tar archive: %w", err)
		}
		fi := hdr.FileInfo()
		if fi.Name() == path && fi.Mode().IsRegular() {
			_, err = io.Copy(bw, tr)
			if err != nil {
				return nil, xerrors.Errorf("failed to read file %q from archive", fi.Name())
			}
			break
		}
	}

	return b.Bytes(), nil
}

// getCoderConfigURL reads the currently configured coder URL, returning an empty string if not configured.
func getCoderConfigURL() (*url.URL, error) {
	urlString, err := config.URL.Read()
	if err != nil {
		return nil, err
	}
	configuredURL, err := url.Parse(strings.TrimSpace(urlString))
	if err != nil {
		return nil, err
	}
	return configuredURL, nil
}

// XXX: coder.Client requires an API key, but we may not be logged into the coder instance for which we
// want to determine the version. We don't need an API key to hit /api/private/version though.
func getAPIVersionUnauthed(client getter, baseURL url.URL) (*semver.Version, error) {
	baseURL.Path = path.Join(baseURL.Path, "/api/private/version")
	resp, err := client.Get(baseURL.String())
	if err != nil {
		return nil, xerrors.Errorf("get %s: %w", baseURL.String(), err)
	}
	defer resp.Body.Close()

	ver := struct {
		Version string `json:"version"`
	}{}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, xerrors.Errorf("read response body: %w", err)
	}

	if err := json.Unmarshal(body, &ver); err != nil {
		return nil, xerrors.Errorf("parse version response: %w", err)
	}

	version, err := semver.NewVersion(ver.Version)
	if err != nil {
		return nil, xerrors.Errorf("parsing coder version: %w", err)
	}

	return version, nil
}

// HasFilePathPrefix reports whether the filesystem path s
// begins with the elements in prefix.
// Lifted from github.com/golang/go/blob/master/src/cmd/internal/str/path.go.
func HasFilePathPrefix(s, prefix string) bool {
	sv := strings.ToUpper(filepath.VolumeName(s))
	pv := strings.ToUpper(filepath.VolumeName(prefix))
	s = s[len(sv):]
	prefix = prefix[len(pv):]
	switch {
	default:
		return false
	case sv != pv:
		return false
	case len(s) == len(prefix):
		return s == prefix
	case prefix == "":
		return true
	case len(s) > len(prefix):
		if prefix[len(prefix)-1] == filepath.Separator {
			return strings.HasPrefix(s, prefix)
		}
		return s[len(prefix)] == filepath.Separator && s[:len(prefix)] == prefix
	}
}

// defaultExec wraps exec.CommandContext.
func defaultExec(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, cmd, args...).CombinedOutput()
}

// hotfixExpr matches the build metadata used for identifying CLI hotfixes.
var hotfixExpr = regexp.MustCompile(`(?i)^.*?cli\.(\d+).*?$`)

// hotfixVersion returns the hotfix build metadata tag if it is present in v
// and an empty string otherwise.
func hotfixVersion(v *semver.Version) string {
	match := hotfixExpr.FindStringSubmatch(v.Metadata())
	if len(match) < 2 {
		return ""
	}

	return fmt.Sprintf("+cli.%s", match[1])
}

// compareVersions performs a NON-SEMVER-COMPLIANT comparison of two versions.
// If the two versions differ as per SemVer, then that result is returned.
// Otherwise, the build metadata of the two versions are compared based on
// the `cli.N` hotfix metadata.
//
// Examples:
//   compareVersions(semver.MustParse("v1.0.0"), semver.MustParse("v1.0.0"))
//   0
//   compareVersions(semver.MustParse("v1.0.0"), semver.MustParse("v1.0.1"))
//   1
//   compareVersions(semver.MustParse("v1.0.1"), semver.MustParse("v1.0.0"))
//   -1
//   compareVersions(semver.MustParse("v1.0.0+cli.0"), semver.MustParse("v1.0.0"))
//   1
//   compareVersions(semver.MustParse("v1.0.0+cli.0"), semver.MustParse("v1.0.0+cli.0"))
//   0
//   compareVersions(semver.MustParse("v1.0.0"), semver.MustParse("v1.0.0+cli.0"))
//   -1
//   compareVersions(semver.MustParse("v1.0.0+cli.1"), semver.MustParse("v1.0.0+cli.0"))
//   1
//   compareVersions(semver.MustParse("v1.0.0+cli.0"), semver.MustParse("v1.0.0+cli.1"))
//   -1
//
func compareVersions(a, b *semver.Version) int {
	semverComparison := a.Compare(b)
	if semverComparison != 0 {
		return semverComparison
	}

	matchA := hotfixExpr.FindStringSubmatch(a.Metadata())
	matchB := hotfixExpr.FindStringSubmatch(b.Metadata())

	hotfixA := -1
	hotfixB := -1

	// extract hotfix versions from the metadata of a and b
	if len(matchA) > 1 {
		if n, err := strconv.Atoi(matchA[1]); err == nil {
			hotfixA = n
		}
	}
	if len(matchB) > 1 {
		if n, err := strconv.Atoi(matchB[1]); err == nil {
			hotfixB = n
		}
	}

	// compare hotfix versions
	if hotfixA < hotfixB {
		return -1
	}
	if hotfixA > hotfixB {
		return 1
	}
	// both versions are the same if their semver and hotfix
	// metadata are the same.
	return 0
}
