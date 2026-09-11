// Package update implements the small, self-contained release updater used by
// the web UI. It only downloads the project's published Linux binary; it does
// not run a shell command, access Docker, or touch configuration and secrets.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultAPIURL   = "https://ghfast.top/https://api.github.com/repos/galiandan/WPS_2_WebDAV/releases/latest"
	defaultAssetURL = "https://ghfast.top/https://github.com/galiandan/WPS_2_WebDAV/releases/download"
	checkTTL        = 10 * time.Minute
	maxReleaseBody  = 2 << 20
	maxBinarySize   = 512 << 20
)

var ErrInProgress = errors.New("update is already in progress")

// Status is safe to return to the browser. It contains no URLs with
// credentials and no local filesystem paths.
type Status struct {
	State           string `json:"state"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	Message         string `json:"message,omitempty"`
}

type releasePayload struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

type release struct {
	Version   string
	Tag       string
	HTMLURL   string
	AssetName string
	AssetSize int64
}

// Updater keeps the latest check in memory and serializes the one destructive
// operation. The process is replaced only after the HTTP response has been
// sent and the new file has passed an independent --version check.
type Updater struct {
	currentVersion string
	apiURL         string
	assetBaseURL   string
	client         *http.Client

	mu         sync.Mutex
	status     Status
	latest     release
	checkedAt  time.Time
	updateBusy bool
}

func New(currentVersion string) *Updater {
	apiURL := os.Getenv("WPS_ADAPTER_UPDATE_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	assetBaseURL := os.Getenv("WPS_ADAPTER_UPDATE_BASE_URL")
	if assetBaseURL == "" {
		assetBaseURL = defaultAssetURL
	}
	return &Updater{
		currentVersion: currentVersion,
		apiURL:         apiURL,
		assetBaseURL:   strings.TrimRight(assetBaseURL, "/"),
		client:         &http.Client{Timeout: 25 * time.Second},
		status: Status{
			State:          "idle",
			CurrentVersion: currentVersion,
		},
	}
}

// Status returns the last known state without making a network request.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// Check asks the latest-release endpoint at most once per checkTTL. A failed
// check is returned as a normal status so a temporary GitHub mirror outage
// cannot make the file manager appear broken.
func (u *Updater) Check(ctx context.Context) (Status, error) {
	u.mu.Lock()
	if u.updateBusy {
		status := u.status
		u.mu.Unlock()
		return status, nil
	}
	if !u.checkedAt.IsZero() && time.Since(u.checkedAt) < checkTTL {
		status := u.status
		u.mu.Unlock()
		return status, nil
	}
	u.status.State = "checking"
	u.status.Message = "正在检查更新"
	u.mu.Unlock()

	latest, err := u.fetchLatest(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checkedAt = time.Now()
	if err != nil {
		u.status.State = "error"
		u.status.Message = "暂时无法检查更新"
		return u.status, err
	}
	u.latest = latest
	u.status.LatestVersion = latest.Version
	u.status.ReleaseURL = latest.HTMLURL
	u.status.UpdateAvailable = newerThan(latest.Version, u.currentVersion)
	if u.status.UpdateAvailable {
		u.status.State = "available"
		u.status.Message = "发现新版本"
	} else {
		u.status.State = "idle"
		u.status.Message = "当前已是最新版本"
	}
	return u.status, nil
}

// Start begins a background update. The caller can return an HTTP 202
// immediately; the browser polls GET /update while the binary downloads.
func (u *Updater) Start() error {
	u.mu.Lock()
	if u.updateBusy {
		u.mu.Unlock()
		return ErrInProgress
	}
	u.updateBusy = true
	u.status.State = "checking"
	u.status.Message = "正在准备更新"
	u.status.UpdateAvailable = false
	u.mu.Unlock()
	go u.run()
	return nil
}

func (u *Updater) run() {
	latest, err := u.fetchLatest(context.Background())
	if err != nil {
		u.finishError("更新检查失败", err)
		return
	}

	u.mu.Lock()
	u.latest = latest
	u.checkedAt = time.Now()
	u.status.LatestVersion = latest.Version
	u.status.ReleaseURL = latest.HTMLURL
	if !newerThan(latest.Version, u.currentVersion) {
		u.status.State = "idle"
		u.status.Message = "当前已是最新版本"
		u.status.UpdateAvailable = false
		u.updateBusy = false
		u.mu.Unlock()
		return
	}
	u.status.State = "downloading"
	u.status.Message = "正在下载新版本"
	u.status.UpdateAvailable = true
	u.mu.Unlock()

	target, err := u.install(latest)
	if err != nil {
		u.finishError("新版本安装失败，当前版本未改变", err)
		return
	}
	u.mu.Lock()
	u.status.State = "restarting"
	u.status.Message = "更新完成，正在重启服务"
	u.mu.Unlock()

	// Give the 202 response a chance to reach the browser before replacing
	// the process image. The browser reloads when the connection closes.
	time.Sleep(350 * time.Millisecond)
	if err := restart(target); err != nil {
		u.finishError("服务重启失败，请手动重启服务", err)
	}
}

func (u *Updater) finishError(message string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.State = "error"
	u.status.Message = message
	u.status.UpdateAvailable = newerThan(u.status.LatestVersion, u.currentVersion)
	u.updateBusy = false
	_ = err // detailed errors stay server-side and never reach the browser
}

func (u *Updater) fetchLatest(ctx context.Context) (release, error) {
	parsed, err := url.Parse(u.apiURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return release{}, errors.New("update API URL must be an HTTPS URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.apiURL, nil)
	if err != nil {
		return release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "wps-adapter-update")
	response, err := u.client.Do(request)
	if err != nil {
		return release{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("update API returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseBody+1))
	if err != nil || int64(len(body)) > maxReleaseBody {
		return release{}, errors.New("update metadata is too large")
	}
	var payload releasePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return release{}, err
	}
	version, ok := normalizeVersion(payload.TagName)
	if !ok {
		return release{}, errors.New("latest release has an invalid version")
	}
	assetName, assetSize, ok := chooseAsset(payload.Assets)
	if !ok {
		return release{}, errors.New("latest release has no binary for this architecture")
	}
	return release{Version: version, Tag: payload.TagName, HTMLURL: payload.HTMLURL, AssetName: assetName, AssetSize: assetSize}, nil
}

func chooseAsset(assets []struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}) (string, int64, bool) {
	preferred := []string{"wps-adapter-linux-" + runtime.GOARCH}
	if runtime.GOARCH == "arm" {
		preferred = append(preferred, "wps-adapter-linux-armv7", "wps-adapter-linux-armv6")
	}
	for _, wanted := range preferred {
		for _, asset := range assets {
			if asset.Name == wanted {
				return asset.Name, asset.Size, true
			}
		}
	}
	return "", 0, false
}

func (u *Updater) install(latest release) (string, error) {
	target, err := updateTarget()
	if err != nil {
		return "", err
	}
	if latest.AssetSize > maxBinarySize {
		return "", errors.New("release binary is too large")
	}
	assetURL := u.assetBaseURL + "/" + url.PathEscape(latest.Tag) + "/" + url.PathEscape(latest.AssetName)
	request, err := http.NewRequest(http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "wps-adapter-update")
	response, err := u.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxBinarySize {
		return "", errors.New("release binary is too large")
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".wps-adapter-update-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		temporary.Close()
		return "", err
	}
	written, copyErr := io.CopyN(temporary, response.Body, maxBinarySize+1)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		temporary.Close()
		return "", copyErr
	}
	if written > maxBinarySize {
		temporary.Close()
		return "", errors.New("release binary is too large")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := verifyBinary(temporaryPath, latest.Version); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", err
	}
	return target, nil
}

func updateTarget() (string, error) {
	configured := strings.TrimSpace(os.Getenv("WPS_ADAPTER_UPDATE_BINARY"))
	path := configured
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("update target must be an absolute path")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("update target must be a regular file")
	}
	if info.Mode()&0o111 == 0 {
		return "", errors.New("update target is not executable")
	}
	return clean, nil
}

func verifyBinary(path, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return errors.New("downloaded binary failed its version check")
	}
	first := strings.Fields(string(output))
	if len(first) == 0 || first[0] != version {
		return errors.New("downloaded binary version does not match the release")
	}
	return nil
}

func normalizeVersion(value string) (string, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" {
			return "", false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return "", false
			}
		}
	}
	return value, true
}

func newerThan(candidate, current string) bool {
	candidate, candidateOK := normalizeVersion(candidate)
	current, currentOK := normalizeVersion(current)
	if !candidateOK || !currentOK {
		return false
	}
	left := strings.Split(candidate, ".")
	right := strings.Split(current, ".")
	for index := range left {
		l, _ := strconv.Atoi(left[index])
		r, _ := strconv.Atoi(right[index])
		if l != r {
			return l > r
		}
	}
	return false
}

func restart(path string) error {
	return syscall.Exec(path, os.Args, os.Environ())
}
